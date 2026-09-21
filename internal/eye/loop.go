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
	e.log("eyes up camera=%v screen=%v every=%s", opt.Camera, opt.Screen, opt.Interval)
	return e
}

// Push accepts a JPEG from the Live2D viewer (camera or picked window).
func (e *Eyes) Push(source, dataURL string) error {
	if e == nil {
		return fmt.Errorf("eyes down")
	}
	source = normalizeSource(source)
	if source == "" {
		return fmt.Errorf("unknown eye source")
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

// LookNow forces a caption of whatever frames are currently held.
func (e *Eyes) LookNow(ctx context.Context) Sight {
	if e == nil {
		return Sight{}
	}
	if e.opt.Screen {
		if raw, err := e.grabScreen(); err == nil {
			_ = e.pushJPEG(SourceScreen, raw)
		}
	}
	e.mu.Lock()
	cam := e.latest[SourceCamera]
	scr := e.latest[SourceScreen]
	e.mu.Unlock()

	var wg sync.WaitGroup
	if len(cam.JPEG) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.describe(ctx, cam, true)
		}()
	}
	if len(scr.JPEG) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.describe(ctx, scr, true)
		}()
	}
	wg.Wait()
	s := e.Snapshot()
	e.emit(s)
	return s
}

func (e *Eyes) sampleScreen(ctx context.Context) {
	fail := 0
	tick := time.NewTicker(e.opt.Interval)
	defer tick.Stop()
	if raw, err := e.grabScreen(); err != nil {
		e.log("screen: %v", err)
		fail++
	} else if err := e.pushJPEG(SourceScreen, raw); err != nil {
		e.log("screen encode: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			raw, err := e.grabScreen()
			if err != nil {
				fail++
				if fail == 1 || fail%12 == 0 {
					e.log("screen: %v", err)
				}
				continue
			}
			fail = 0
			if err := e.pushJPEG(SourceScreen, raw); err != nil {
				e.log("screen encode: %v", err)
			}
		}
	}
}

func (e *Eyes) grabScreen() ([]byte, error) {
	if e.opt.Grab != nil {
		return e.opt.Grab()
	}
	return captureScreenJPEG(maxScreenEdge)
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
	sys, user := describePrompt(f.Source)
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

// ProbeScreen captures one desktop JPEG and returns its size. Used by doctor.
func ProbeScreen() (int, error) {
	raw, err := captureScreenJPEG(maxScreenEdge)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
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
	case SourceScreen, "scr", "desktop", "display":
		return SourceScreen
	default:
		return ""
	}
}
