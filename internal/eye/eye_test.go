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
	"unicode/utf8"

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

func TestGlanceShotIsHerFace(t *testing.T) {
	var prompt string
	e := New(Options{
		LLM: fakeVision{fn: func(_ context.Context, sys, user string, jpeg []byte) (string, error) {
			if len(jpeg) == 0 {
				t.Error("shot jpeg empty")
			}
			prompt = sys + " " + user
			return "短发，表情平静。", nil
		}},
	})
	e.opt.GrabShot = func(context.Context) bool {
		if err := e.pushJPEG(SourceShot, SolidJPEG(32, 48, color.RGBA{R: 80, G: 40, B: 20, A: 255})); err != nil {
			t.Error(err)
		}
		return true
	}
	g := e.Glance(context.Background(), SourceShot)
	if !strings.Contains(g.Caption, "短发") {
		t.Fatalf("shot %q", g.Caption)
	}
	if !strings.Contains(prompt, "screenshot") || !strings.Contains(prompt, "appearance") {
		t.Fatalf("prompt %q", prompt)
	}
	if e.Snapshot().Camera.Caption != "" || e.Snapshot().Screen.Caption != "" {
		t.Fatal("shot glance wrote another eye")
	}
	cam := e.Glance(context.Background(), SourceCamera)
	if cam.Caption != "" {
		t.Fatalf("camera should stay empty without a room frame, got %q", cam.Caption)
	}
	if !strings.Contains(e.Snapshot().Shot.Caption, "短发") {
		t.Fatal("camera glance cleared the screenshot")
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

func TestAsksScene(t *testing.T) {
	if !AsksScene("有几个人") || !AsksScene("摄像头里现在呢") {
		t.Fatal("a question about the view should look again")
	}
	if AsksScene("哈哈你好搞笑") || AsksScene("看看屏幕") || AsksScene("看看摄像头") || AsksScene("") {
		t.Fatal("chat and a plain look are not a picture question")
	}
	for _, q := range []string{"可以擦看文字吗", "Cursor 的页面,然后你看上面有什么字", "上面写的什么"} {
		if !AsksScene(q) {
			t.Fatalf("asking what it says needs the picture: %q", q)
		}
	}
}

func TestScreenFollowUpTakesPicture(t *testing.T) {
	var caps atomic.Int32
	var asked string
	e := New(Options{
		Observe: func(context.Context) (ScreenView, error) {
			return ScreenView{Caption: "前台 Cursor"}, nil
		},
		Capture: func(context.Context) ([]byte, error) {
			caps.Add(1)
			return SolidJPEG(32, 24, color.White), nil
		},
		LLM: fakeVision{fn: func(_ context.Context, _, user string, _ []byte) (string, error) {
			asked = user
			return "上面写着 Memory and task module issues。", nil
		}},
	})
	plain := e.GlanceAsk(context.Background(), SourceScreen, "嗯...上面的部分呗")
	if caps.Load() != 0 || strings.Contains(plain.Caption, "画面") {
		t.Fatalf("a first look without a picture question took one: %q", plain.Caption)
	}
	closer := e.LookCloser(context.Background(), SourceScreen, "你看上面有什么字；嗯...上面的部分呗")
	if caps.Load() != 1 || !strings.Contains(closer.Caption, "Memory and task") || !strings.Contains(asked, "上面的部分") {
		t.Fatalf("follow-up look %q caps=%d asked=%q", closer.Caption, caps.Load(), asked)
	}
	e.LookCloser(context.Background(), SourceScreen, "")
	if caps.Load() != 1 {
		t.Fatal("a follow-up with no question took a picture")
	}
}

func TestClipCaptionKeepsWholeRunes(t *testing.T) {
	got := clipCaption(strings.Repeat("字", 100), 181)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip split a rune: %q", got)
	}
}

func TestCameraFollowUpReplacesCaption(t *testing.T) {
	var prompts []string
	e := New(Options{
		LLM: fakeVision{fn: func(_ context.Context, _, user string, _ []byte) (string, error) {
			prompts = append(prompts, user)
			if strings.Contains(user, "有几个人") {
				return "两个人。", nil
			}
			return "屋里亮着灯。", nil
		}},
	})
	if err := e.pushJPEG(SourceCamera, SolidJPEG(16, 16, color.Gray{Y: 30})); err != nil {
		t.Fatal(err)
	}
	first := e.GlanceAsk(context.Background(), SourceCamera, "看看摄像头")
	if !strings.Contains(first.Caption, "屋里亮着灯") {
		t.Fatalf("first %q", first.Caption)
	}
	second := e.GlanceAsk(context.Background(), SourceCamera, "有几个人")
	if !strings.Contains(second.Caption, "两个人") || strings.Contains(second.Caption, "屋里亮着灯") {
		t.Fatalf("follow-up kept the first caption: %q", second.Caption)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "有几个人") {
		t.Fatalf("prompts %q", prompts)
	}
}

func TestScreenQuestionUsesPicture(t *testing.T) {
	var caps atomic.Int32
	e := New(Options{
		Observe: func(context.Context) (ScreenView, error) {
			return ScreenView{Caption: "前台 Chrome", Signature: "ch"}, nil
		},
		Capture: func(context.Context) ([]byte, error) {
			caps.Add(1)
			return SolidJPEG(32, 24, color.RGBA{R: 20, G: 40, B: 80, A: 255}), nil
		},
		LLM: fakeVision{fn: func(_ context.Context, system, user string, jpeg []byte) (string, error) {
			if len(jpeg) == 0 || !strings.Contains(system, "screenshot") || !strings.Contains(user, "有几个人") {
				t.Fatalf("picture prompt sys=%q user=%q bytes=%d", system, user, len(jpeg))
			}
			return "画面里有两个人。", nil
		}},
	})
	plain := e.Glance(context.Background(), SourceScreen)
	if caps.Load() != 0 || !strings.Contains(plain.Caption, "Chrome") || strings.Contains(plain.Caption, "两个人") {
		t.Fatalf("plain look should stay titles: %q caps=%d", plain.Caption, caps.Load())
	}
	asked := e.GlanceAsk(context.Background(), SourceScreen, "有几个人")
	if caps.Load() != 1 || !strings.Contains(asked.Caption, "Chrome") || !strings.Contains(asked.Caption, "两个人") {
		t.Fatalf("question look %q caps=%d", asked.Caption, caps.Load())
	}
}

func TestScreenPrivateSkipsPicture(t *testing.T) {
	var caps atomic.Int32
	e := New(Options{
		Observe: func(context.Context) (ScreenView, error) {
			return ScreenView{Caption: "看起来是私人窗口，不细看。", Private: true}, nil
		},
		Capture: func(context.Context) ([]byte, error) {
			caps.Add(1)
			return SolidJPEG(8, 8, color.White), nil
		},
		LLM: fakeVision{fn: func(context.Context, string, string, []byte) (string, error) {
			t.Fatal("private screen must not call the vision model")
			return "", nil
		}},
	})
	g := e.GlanceAsk(context.Background(), SourceScreen, "有几个人")
	if caps.Load() != 0 || !strings.Contains(g.Caption, "私人") || strings.Contains(g.Caption, "画面") {
		t.Fatalf("caption %q caps=%d", g.Caption, caps.Load())
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
