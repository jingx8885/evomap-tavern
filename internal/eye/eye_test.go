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

func TestStartDoesNotSample(t *testing.T) {
	var vlm atomic.Int32
	var obs atomic.Int32
	e := Start(context.Background(), Options{
		Camera: true,
		Screen: true,
		Jev:    fakeEval{noteworthy: 0.9, private: 0.1, mention: 0.9},
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			vlm.Add(1)
			return "摄像头里有个人坐着。", nil
		}},
		Observe: func(context.Context) (ScreenView, error) {
			obs.Add(1)
			return ScreenView{Caption: "前台 Cursor · lov-evo（coding）", Signature: "sig1"}, nil
		},
	})
	if err := e.pushJPEG(SourceCamera, SolidJPEG(48, 48, color.RGBA{R: 200, G: 40, B: 40, A: 255})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if vlm.Load() != 0 || obs.Load() != 0 {
		t.Fatalf("start sampled on its own vlm=%d obs=%d", vlm.Load(), obs.Load())
	}
	if g := e.Glance(context.Background(), SourceScreen); !strings.Contains(g.Caption, "Cursor") {
		t.Fatalf("screen %q", g.Caption)
	}
	if g := e.Glance(context.Background(), SourceCamera); !strings.Contains(g.Caption, "摄像头") {
		t.Fatalf("camera %q", g.Caption)
	}
	if vlm.Load() != 1 || obs.Load() != 1 {
		t.Fatalf("one look each vlm=%d obs=%d", vlm.Load(), obs.Load())
	}
}

func TestPushRejectsScreenJPEG(t *testing.T) {
	e := New(Options{})
	err := e.Push(SourceScreen, "data:image/jpeg;base64,aaaa")
	if err == nil {
		t.Fatal("screen JPEG should be rejected")
	}
}

func TestGateSkipsPrivate(t *testing.T) {
	var vlm atomic.Int32
	e := New(Options{
		Camera: true,
		Jev:    fakeEval{noteworthy: 0.9, private: 0.95, mention: 0.9},
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			vlm.Add(1)
			return "should not run", nil
		}},
	})
	if err := e.pushJPEG(SourceCamera, SolidJPEG(24, 24, color.White)); err != nil {
		t.Fatal(err)
	}
	g := e.Glance(context.Background(), SourceCamera)
	if vlm.Load() != 0 {
		t.Fatal("private frame should not call vlm")
	}
	if !strings.Contains(g.Caption, "私人") {
		t.Fatalf("caption %q", g.Caption)
	}
}

func TestGlanceWaitsForOneGrab(t *testing.T) {
	e := New(Options{
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			return "新的一帧。", nil
		}},
	})
	e.opt.Grab = func(context.Context) bool {
		if err := e.pushJPEG(SourceCamera, SolidJPEG(16, 16, color.Gray{Y: 40})); err != nil {
			t.Error(err)
		}
		return true
	}
	g := e.Glance(context.Background(), SourceCamera)
	if !strings.Contains(g.Caption, "新的一帧") {
		t.Fatalf("caption %q", g.Caption)
	}
}

func TestGlanceIsOneSource(t *testing.T) {
	var vlm atomic.Int32
	var obs atomic.Int32
	e := New(Options{
		Screen: true,
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			vlm.Add(1)
			return "看见你了。", nil
		}},
		Observe: func(context.Context) (ScreenView, error) {
			obs.Add(1)
			return ScreenView{Caption: "前台 notepad", Signature: "np"}, nil
		},
	})
	if err := e.pushJPEG(SourceCamera, SolidJPEG(16, 16, color.Gray{Y: 20})); err != nil {
		t.Fatal(err)
	}
	cam := e.Glance(context.Background(), SourceCamera)
	if cam.Caption == "" {
		t.Fatal("camera glance empty")
	}
	if obs.Load() != 0 {
		t.Fatal("camera glance must not observe the screen")
	}
	if vlm.Load() != 1 {
		t.Fatalf("vlm %d", vlm.Load())
	}
	if e.Snapshot().Screen.Caption != "" {
		t.Fatal("camera glance wrote a screen caption")
	}
	scr := e.Glance(context.Background(), SourceScreen)
	if !strings.Contains(scr.Caption, "notepad") {
		t.Fatalf("screen %q", scr.Caption)
	}
	if obs.Load() != 1 || vlm.Load() != 1 {
		t.Fatalf("obs=%d vlm=%d", obs.Load(), vlm.Load())
	}
	if e.Snapshot().Camera.Caption == "" {
		t.Fatal("screen glance cleared the camera")
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
