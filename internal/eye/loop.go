package eye

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Eyes holds the latest camera frame and the last captions.
// It does not sample on a clock. Glance is the only look.
type Eyes struct {
	opt Options

	mu     sync.Mutex
	latest map[string]Frame
	seen   map[string]Glimpse
}

// New builds a pair that waits for Glance.
func New(opt Options) *Eyes {
	return &Eyes{
		opt:    opt,
		latest: map[string]Frame{},
		seen:   map[string]Glimpse{},
	}
}

// Start builds the pair. There is no background sampler.
func Start(ctx context.Context, opt Options) *Eyes {
	_ = ctx
	e := New(opt)
	e.log("eyes up camera=%v screen=computer-use shot=self on demand", opt.Camera)
	return e
}

// Push accepts a JPEG from the Live2D viewer.
func (e *Eyes) Push(source, dataURL string) error {
	if e == nil {
		return fmt.Errorf("eyes down")
	}
	source = normalizeSource(source)
	if source != SourceCamera && source != SourceShot {
		return fmt.Errorf("only camera frames and a self screenshot are accepted; screen is computer-use")
	}
	raw, err := DecodeDataURL(dataURL)
	if err != nil {
		return err
	}
	return e.pushJPEG(source, raw)
}

func (e *Eyes) pushJPEG(source string, raw []byte) error {
	f, err := NormalizeFrame(source, raw, maxEdgeFor(source))
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.latest[source] = f
	e.mu.Unlock()
	return nil
}

// Snapshot is the latest captions, safe to copy onto the sense bus.
func (e *Eyes) Snapshot() Sight {
	if e == nil {
		return Sight{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return Sight{
		Camera: e.seen[SourceCamera],
		Screen: e.seen[SourceScreen],
		Shot:   e.seen[SourceShot],
	}
}

// Glance looks at one source once and leaves the others alone.
// Camera captions one room JPEG. Shot captions one screenshot of her face.
// Screen takes one computer-use window snapshot.
func (e *Eyes) Glance(ctx context.Context, source string) Glimpse {
	return e.GlanceAsk(ctx, source, "")
}

// GlanceAsk looks again for this question. An empty question is the
// generic caption. A scene question is answered from a fresh frame,
// not from the previous caption.
func (e *Eyes) GlanceAsk(ctx context.Context, source, question string) Glimpse {
	return e.look(ctx, source, question, AsksScene(question))
}

// LookCloser answers a follow-up on an open look. The screen takes a
// picture too: its window titles were already the first answer.
func (e *Eyes) LookCloser(ctx context.Context, source, question string) Glimpse {
	return e.look(ctx, source, question, strings.TrimSpace(question) != "")
}

func (e *Eyes) look(ctx context.Context, source, question string, picture bool) Glimpse {
	if e == nil {
		return Glimpse{}
	}
	switch source {
	case SourceCamera:
		return e.glanceCamera(ctx, question)
	case SourceShot:
		return e.glanceShot(ctx, question)
	case SourceScreen:
		e.refreshScreen(ctx, question, picture)
		return e.Snapshot().Screen
	default:
		return Glimpse{}
	}
}

func (e *Eyes) glanceCamera(ctx context.Context, question string) Glimpse {
	if e.opt.Grab != nil {
		mark := time.Now()
		if e.opt.Grab(ctx) {
			e.waitFrame(ctx, SourceCamera, mark, 1200*time.Millisecond)
		}
	}
	e.mu.Lock()
	cam := e.latest[SourceCamera]
	last := e.seen[SourceCamera].Caption
	e.mu.Unlock()
	if len(cam.JPEG) == 0 {
		e.log("camera: no frame")
		return Glimpse{Source: SourceCamera}
	}
	gcall := e.gate(ctx, SourceCamera, 1, last, len(cam.JPEG), cam.Width, cam.Height)
	if gcall.Private {
		e.mu.Lock()
		g := e.seen[SourceCamera]
		g.Source = SourceCamera
		g.Private = true
		g.Caption = "看起来是私人画面，不细看。"
		g.Noted = false
		g.Ready = true
		g.At = time.Now()
		e.seen[SourceCamera] = g
		e.mu.Unlock()
		e.log("camera private, skipped vlm")
		e.emit(e.Snapshot())
		return e.Snapshot().Camera
	}
	e.describe(ctx, cam, true, question)
	e.emit(e.Snapshot())
	return e.Snapshot().Camera
}

func (e *Eyes) glanceShot(ctx context.Context, question string) Glimpse {
	if e.opt.GrabShot != nil {
		mark := time.Now()
		if e.opt.GrabShot(ctx) {
			e.waitFrame(ctx, SourceShot, mark, 1200*time.Millisecond)
		}
	}
	e.mu.Lock()
	frame := e.latest[SourceShot]
	e.mu.Unlock()
	if len(frame.JPEG) == 0 {
		e.log("shot: no frame")
		return Glimpse{Source: SourceShot}
	}
	e.describe(ctx, frame, true, question)
	e.emit(e.Snapshot())
	return e.Snapshot().Shot
}

func (e *Eyes) waitFrame(ctx context.Context, source string, after time.Time, d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		e.mu.Lock()
		at := e.latest[source].At
		e.mu.Unlock()
		if !at.IsZero() && !at.Before(after) {
			return
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return
		}
		timer := time.NewTimer(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (e *Eyes) refreshScreen(ctx context.Context, question string, picture bool) {
	view, err := e.observe(ctx)
	if err != nil {
		e.log("screen: %v", err)
		return
	}
	g := Glimpse{
		Source:  SourceScreen,
		Caption: clipCaption(view.Caption, 180),
		At:      time.Now(),
		Ready:   true,
		Private: view.Private,
	}
	if g.Caption == "" {
		g.Caption = "桌面窗口快照还是空的"
	}
	if !view.Private && picture {
		pic, picErr := e.picture(ctx, question)
		switch {
		case picErr != nil || pic == "":
			g.Caption += "。这一眼没有拿到画面，回答不了画面里的问题。"
			e.log("screen picture: %v", picErr)
		default:
			g.Caption += "。画面：" + pic
			g.Noted = true
		}
	}
	e.mu.Lock()
	e.seen[SourceScreen] = g
	e.mu.Unlock()
	e.log("screen: %s", g.Caption)
	e.emit(e.Snapshot())
}

func (e *Eyes) picture(ctx context.Context, question string) (string, error) {
	if e.opt.Capture == nil || e.opt.LLM == nil {
		return "", fmt.Errorf("no screen picture")
	}
	raw, err := e.opt.Capture(ctx)
	if err != nil {
		return "", err
	}
	frame, err := NormalizeFrame(SourceScreen, raw, maxScreenEdge)
	if err != nil {
		return "", err
	}
	sys, user := picturePrompt(question)
	text, err := e.opt.LLM.ChatVision(ctx, sys, user, frame.JPEG)
	if err != nil {
		return "", err
	}
	text = clipCaption(text, 180)
	if text == "" {
		return "", fmt.Errorf("empty picture answer")
	}
	return text, nil
}

func (e *Eyes) observe(ctx context.Context) (ScreenView, error) {
	if e.opt.Observe != nil {
		return e.opt.Observe(ctx)
	}
	return ScreenView{}, fmt.Errorf("no computer-use observer")
}

func (e *Eyes) describe(ctx context.Context, f Frame, mention bool, question string) {
	if e.opt.LLM == nil || len(f.JPEG) == 0 {
		return
	}
	sys, user := describePrompt(f.Source, question)
	text, err := e.opt.LLM.ChatVision(ctx, sys, user, f.JPEG)
	if err != nil {
		e.log("%s vlm: %v", f.Source, err)
		return
	}
	text = clipCaption(text, 180)
	if text == "" {
		return
	}
	e.mu.Lock()
	g := e.seen[f.Source]
	g.Source = f.Source
	g.Caption = text
	g.At = time.Now()
	g.Ready = true
	g.Private = false
	g.Noted = mention
	e.seen[f.Source] = g
	e.mu.Unlock()
	e.log("%s: %s", f.Source, text)
}

func (e *Eyes) emit(s Sight) {
	if e.opt.OnSight == nil {
		return
	}
	if s.Camera.Noted || s.Screen.Noted || s.Shot.Noted {
		s.Mention = true
	}
	e.opt.OnSight(s)
}

func (e *Eyes) log(format string, args ...any) {
	if e.opt.LogFn == nil {
		return
	}
	e.opt.LogFn(fmt.Sprintf(format, args...))
}

func normalizeSource(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SourceCamera, "cam", "webcam":
		return SourceCamera
	case SourceShot, "screenshot", "self":
		return SourceShot
	default:
		return ""
	}
}
