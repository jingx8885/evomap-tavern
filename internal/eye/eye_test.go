package eye

import (
	"bytes"
	"context"
	"image/color"
	"image/jpeg"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/jev"
)

func TestParseSources(t *testing.T) {
	cam, scr := ParseSources("both")
	if !cam || !scr {
		t.Fatalf("both: %v %v", cam, scr)
	}
	cam, scr = ParseSources("off")
	if cam || scr {
		t.Fatalf("off")
	}
	cam, scr = ParseSources("camera")
	if !cam || scr {
		t.Fatalf("camera")
	}
	cam, scr = ParseSources("screen,camera")
	if !cam || !scr {
		t.Fatalf("comma")
	}
}

func TestThumbDeltaDetectsChange(t *testing.T) {
	black := SolidJPEG(32, 32, color.Black)
	white := SolidJPEG(32, 32, color.White)
	fb, err := NormalizeFrame(SourceCamera, black, 32)
	if err != nil {
		t.Fatal(err)
	}
	fw, err := NormalizeFrame(SourceCamera, white, 32)
	if err != nil {
		t.Fatal(err)
	}
	if d := ThumbDelta(fb.Thumb, fb.Thumb); d > 0.01 {
		t.Fatalf("same thumb delta %v", d)
	}
	if d := ThumbDelta(fb.Thumb, fw.Thumb); d < 0.4 {
		t.Fatalf("opposite colors should differ, got %v", d)
	}
}

func TestParallelCameraAndScreen(t *testing.T) {
	var vlm atomic.Int32
	var sights []Sight
	var muN int
	llm := fakeVision{fn: func(_ context.Context, _, src string, _ []byte) (string, error) {
		vlm.Add(1)
		if strings.Contains(src, "camera") {
			return "摄像头里有个人坐着。", nil
		}
		return "屏幕在写 Go 代码。", nil
	}}
	jev := fakeEval{noteworthy: 0.9, private: 0.1, mention: 0.1}
	grabN := atomic.Int32{}
	e := New(Options{
		Camera:   true,
		Screen:   true,
		Interval: 30 * time.Millisecond,
		Cooldown: time.Millisecond,
		Jev:      jev,
		LLM:      llm,
		Grab: func() ([]byte, error) {
			grabN.Add(1)
			return SolidJPEG(40, 24, color.RGBA{R: 40, G: 80, B: 120, A: 255}), nil
		},
		OnSight: func(s Sight) {
			if s.Camera.Caption != "" || s.Screen.Caption != "" {
				sights = append(sights, s)
				muN++
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.sampleScreen(ctx)
	go e.loop(ctx)

	if err := e.pushJPEG(SourceCamera, SolidJPEG(48, 48, color.RGBA{R: 200, G: 40, B: 40, A: 255})); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if s.Camera.Caption != "" && s.Screen.Caption != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := e.Snapshot()
	if s.Camera.Caption == "" || s.Screen.Caption == "" {
		t.Fatalf("expected both captions, got %+v vlm=%d grabs=%d", s, vlm.Load(), grabN.Load())
	}
	if !strings.Contains(s.Camera.Caption, "摄像头") {
		t.Fatalf("camera caption %q", s.Camera.Caption)
	}
	if !strings.Contains(s.Screen.Caption, "屏幕") {
		t.Fatalf("screen caption %q", s.Screen.Caption)
	}
	if vlm.Load() < 2 {
		t.Fatalf("vlm calls %d, want both sources", vlm.Load())
	}
	if grabN.Load() < 1 {
		t.Fatal("screen grab never ran")
	}
}

func TestGateSkipsPrivate(t *testing.T) {
	var vlm atomic.Int32
	e := New(Options{
		Camera:   true,
		Interval: time.Hour,
		Cooldown: time.Millisecond,
		Jev:      fakeEval{noteworthy: 0.9, private: 0.95, mention: 0.9},
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			vlm.Add(1)
			return "should not run", nil
		}},
	})
	f, err := NormalizeFrame(SourceCamera, SolidJPEG(24, 24, color.White), 24)
	if err != nil {
		t.Fatal(err)
	}
	e.consider(context.Background(), f)
	if vlm.Load() != 0 {
		t.Fatal("private frame should not call vlm")
	}
	if got := e.Snapshot().Camera.Caption; !strings.Contains(got, "私人") {
		t.Fatalf("caption %q", got)
	}
}

func TestLookNowCaptionsBoth(t *testing.T) {
	llm := fakeVision{fn: func(_ context.Context, _, user string, _ []byte) (string, error) {
		if strings.Contains(user, "camera") || strings.Contains(user, "摄像头") {
			return "看见你了。", nil
		}
		return "桌面。", nil
	}}
	e := New(Options{
		Screen: true,
		LLM:    llm,
		Grab: func() ([]byte, error) {
			return SolidJPEG(16, 16, color.Gray{Y: 90}), nil
		},
	})
	if err := e.pushJPEG(SourceCamera, SolidJPEG(16, 16, color.Gray{Y: 20})); err != nil {
		t.Fatal(err)
	}
	s := e.LookNow(context.Background())
	if s.Camera.Caption == "" || s.Screen.Caption == "" {
		t.Fatalf("%+v", s)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	raw := SolidJPEG(64, 48, color.RGBA{R: 10, G: 200, B: 30, A: 255})
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() < 8 {
		t.Fatal(img.Bounds())
	}
}

type fakeEval struct {
	noteworthy, private, mention float64
}

func (f fakeEval) Evaluate(_ context.Context, _ any, qs map[string]jev.Question) (*jev.EvalResult, error) {
	n, p, m := f.noteworthy, f.private, f.mention
	out := &jev.EvalResult{Answers: map[string]jev.Answer{}}
	for id := range qs {
		var v float64
		switch id {
		case "noteworthy":
			v = n
		case "private":
			v = p
		case "mention":
			v = m
		}
		vv := v
		out.Answers[id] = jev.Answer{Type: "noul", Noul: &vv}
	}
	return out, nil
}

type fakeVision struct {
	fn func(ctx context.Context, system, user string, jpeg []byte) (string, error)
}

func (f fakeVision) ChatVision(ctx context.Context, system, user string, jpeg []byte) (string, error) {
	if f.fn == nil {
		return "ok", nil
	}
	return f.fn(ctx, system, user, jpeg)
}
