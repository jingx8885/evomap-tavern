package eye

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Eyes is the running pair of camera + screen samplers plus a Jev/VLM loop.
type Eyes struct {
	opt Options

	mu     sync.Mutex
	latest map[string]Frame
	seen   map[string]Glimpse
	thumbs map[string][]byte
	lastV  map[string]time.Time
	dirty  map[string]bool
	scrSig string
}

// New builds a stopped pair. Start launches the goroutines.
func New(opt Options) *Eyes {
	if opt.Interval <= 0 {
		opt.Interval = 2 * time.Second
	}
	if opt.Cooldown <= 0 {
		opt.Cooldown = 8 * time.Second
	}
	return &Eyes{
		opt:    opt,
		latest: map[string]Frame{},
		seen:   map[string]Glimpse{},
		thumbs: map[string][]byte{},
		lastV:  map[string]time.Time{},
		dirty:  map[string]bool{},
	}
}

// Start launches camera inbox consumption, native screen grab, and the gate loop.
func Start(ctx context.Context, opt Options) *Eyes {
	e := New(opt)
	if opt.Screen {
		go e.sampleScreen(ctx)
	}
	go e.loop(ctx)
	e.log("eyes up camera=%v screen=computer-use every=%s", opt.Camera, opt.Interval)
	return e
}

// Push accepts a JPEG from the Live2D viewer (camera or picked window).
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
	e.dirty[source] = true
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

// LookNow forces a camera caption and a computer-use screen glance.
func (e *Eyes) LookNow(ctx context.Context) Sight {
	if e == nil {
		return Sight{}
	}
	var wg sync.WaitGroup
	if e.opt.Screen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.refreshScreen(ctx, true)
		}()
	}
	e.mu.Lock()
	cam := e.latest[SourceCamera]
	e.mu.Unlock()
	if len(cam.JPEG) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.describe(ctx, cam, true)
		}()
	}
	wg.Wait()
	s := e.Snapshot()
	e.emit(s)
	return s
}

func (e *Eyes) sampleScreen(ctx context.Context) {
	e.refreshScreen(ctx, false)
	tick := time.NewTicker(e.opt.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.refreshScreen(ctx, false)
		}
	}
}

func (e *Eyes) refreshScreen(ctx context.Context, force bool) {
	view, err := e.observe(ctx)
	if err != nil {
		e.log("screen: %v", err)
		return
	}
	e.mu.Lock()
	same := !force && view.Signature != "" && view.Signature == e.scrSig && e.seen[SourceScreen].Caption != ""
	if !force && !same {
		lastAt := e.lastV[SourceScreen]
		if e.seen[SourceScreen].Caption != "" && !lastAt.IsZero() && time.Since(lastAt) < e.opt.Cooldown && view.Signature == e.scrSig {
			same = true
		}
	}
	if same {
		e.mu.Unlock()
		return
	}
	e.scrSig = view.Signature
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
	e.seen[SourceScreen] = g
	e.lastV[SourceScreen] = time.Now()
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

func (e *Eyes) loop(ctx context.Context) {
	tick := time.NewTicker(e.opt.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.tick(ctx)
		}
	}
}

func (e *Eyes) tick(ctx context.Context) {
	e.mu.Lock()
	var jobs []Frame
	for src, f := range e.latest {
		if !e.dirty[src] || len(f.JPEG) == 0 {
			continue
		}
		jobs = append(jobs, f)
		e.dirty[src] = false
	}
	e.mu.Unlock()
	if len(jobs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, f := range jobs {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.consider(ctx, f)
		}()
	}
	wg.Wait()
	e.emit(e.Snapshot())
}

func (e *Eyes) consider(ctx context.Context, f Frame) {
	e.mu.Lock()
	prev := e.thumbs[f.Source]
	lastCap := e.seen[f.Source].Caption
	lastAt := e.lastV[f.Source]
	e.mu.Unlock()

	delta := ThumbDelta(prev, f.Thumb)
	e.mu.Lock()
	e.thumbs[f.Source] = f.Thumb
	g := e.seen[f.Source]
	g.Source = f.Source
	g.Delta = delta
	g.Width = f.Width
	g.Height = f.Height
	g.Bytes = len(f.JPEG)
	g.Ready = true
	g.At = f.At
	e.seen[f.Source] = g
	e.mu.Unlock()

	if delta < hashFallback && lastCap != "" {
		return
	}
	if !lastAt.IsZero() && time.Since(lastAt) < e.opt.Cooldown && lastCap != "" {
		return
	}

	gcall := e.gate(ctx, f.Source, delta, lastCap, len(f.JPEG), f.Width, f.Height)
	if gcall.Private {
		e.mu.Lock()
		g := e.seen[f.Source]
		g.Private = true
		g.Caption = "看起来是私人画面，不细看。"
		g.Noted = false
		e.seen[f.Source] = g
		e.lastV[f.Source] = time.Now()
		e.mu.Unlock()
		e.log("%s private, skipped vlm", f.Source)
		return
	}
	if !gcall.Noteworthy && lastCap != "" {
		return
	}
	e.describe(ctx, f, gcall.Mention)
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
	e.lastV[f.Source] = time.Now()
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
