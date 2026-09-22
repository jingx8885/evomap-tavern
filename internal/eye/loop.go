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
	e.log("eyes up camera=%v screen=computer-use on demand", opt.Camera)
	return e
}

// Push accepts a JPEG from the Live2D viewer.
func (e *Eyes) Push(source, dataURL string) error {
	if e == nil {
		return fmt.Errorf("eyes down")
	}
	source = normalizeSource(source)
	if source != SourceCamera {
		return fmt.Errorf("only camera frames are accepted; screen is computer-use")
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
	}
}

// Glance looks at one source once and leaves the other alone.
// Camera captions one JPEG. Screen takes one computer-use window snapshot.
func (e *Eyes) Glance(ctx context.Context, source string) Glimpse {
	if e == nil {
		return Glimpse{}
	}
	switch source {
	case SourceCamera:
		return e.glanceCamera(ctx)
	case SourceScreen:
		e.refreshScreen(ctx)
		return e.Snapshot().Screen
	default:
		return Glimpse{}
	}
}

func (e *Eyes) glanceCamera(ctx context.Context) Glimpse {
	if e.opt.Grab != nil {
		mark := time.Now()
		if e.opt.Grab(ctx) {
			e.waitCamera(ctx, mark, 1200*time.Millisecond)
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
	e.describe(ctx, cam, true)
	e.emit(e.Snapshot())
	return e.Snapshot().Camera
}

func (e *Eyes) waitCamera(ctx context.Context, after time.Time, d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		e.mu.Lock()
		at := e.latest[SourceCamera].At
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

func (e *Eyes) refreshScreen(ctx context.Context) {
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
	e.mu.Lock()
	e.seen[SourceScreen] = g
	e.mu.Unlock()
	e.log("screen: %s", g.Caption)
	e.emit(e.Snapshot())
}

func (e *Eyes) observe(ctx context.Context) (ScreenView, error) {
	if e.opt.Observe != nil {
		return e.opt.Observe(ctx)
	}
	return ScreenView{}, fmt.Errorf("no computer-use observer")
}

func (e *Eyes) describe(ctx context.Context, f Frame, mention bool) {
	if e.opt.LLM == nil || len(f.JPEG) == 0 {
		return
	}
	sys, user := describePrompt()
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
	if s.Camera.Noted || s.Screen.Noted {
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
	default:
		return ""
	}
}
