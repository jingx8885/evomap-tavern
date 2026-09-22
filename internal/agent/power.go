package agent

import (
	"context"
	"strings"
	"sync"
	"time"
)

// power is the run switch. Off closes the live voice session, cancels
// in-flight model work, and stays down until turned back on.
type power struct {
	mu     sync.Mutex
	on     bool
	wake   chan struct{}
	base   context.Context
	work   context.Context
	cancel context.CancelFunc
	halt   func()
	stop   func()
	log    func(string)
}

func newPower(base context.Context, log func(string)) *power {
	ctx, cancel := context.WithCancel(base)
	return &power{
		on:     true,
		wake:   make(chan struct{}, 1),
		base:   base,
		work:   ctx,
		cancel: cancel,
		log:    log,
	}
}

func (pw *power) On() bool {
	if pw == nil {
		return true
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.on
}

// Ctx is cancelled when the switch turns off. A later on gets a fresh context.
func (pw *power) Ctx() context.Context {
	if pw == nil {
		return context.Background()
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.work
}

// Arm registers the live session closer. stop cancels the open branch.
func (pw *power) Arm(halt func()) {
	if pw == nil {
		return
	}
	pw.mu.Lock()
	pw.halt = halt
	pw.mu.Unlock()
}

// SetStop registers branch cancellation without touching the session closer.
func (pw *power) SetStop(stop func()) {
	if pw == nil {
		return
	}
	pw.mu.Lock()
	pw.stop = stop
	pw.mu.Unlock()
}

// Disarm forgets the session closer after the session is already finished.
func (pw *power) Disarm() {
	if pw == nil {
		return
	}
	pw.mu.Lock()
	pw.halt = nil
	pw.stop = nil
	pw.mu.Unlock()
}

// Set turns the whole run on or off.
func (pw *power) Set(on bool) {
	if pw == nil {
		return
	}
	pw.mu.Lock()
	if pw.on == on {
		pw.mu.Unlock()
		return
	}
	pw.on = on
	halt, stop := pw.halt, pw.stop
	if on {
		pw.work, pw.cancel = context.WithCancel(pw.base)
	} else if pw.cancel != nil {
		pw.cancel()
	}
	pw.mu.Unlock()
	if !on {
		if stop != nil {
			stop()
		}
		if halt != nil {
			halt()
		}
	}
	select {
	case pw.wake <- struct{}{}:
	default:
	}
	if pw.log != nil {
		if on {
			pw.log("system on")
		} else {
			pw.log("system off")
		}
	}
}

// Wait blocks until the switch is on or ctx ends. False means ctx ended.
func (pw *power) Wait(ctx context.Context) bool {
	if pw == nil {
		return ctx.Err() == nil
	}
	for !pw.On() {
		select {
		case <-ctx.Done():
			return false
		case <-pw.wake:
		}
	}
	return ctx.Err() == nil
}

func (o Options) handlePaused(line string) bool {
	switch strings.TrimSpace(line) {
	case "/quit", "/q":
		return true
	case "/on":
		o.power.Set(true)
	case "/off":
	default:
		o.log("system off; /on to resume, /quit to exit")
	}
	return false
}

func (o Options) afterVoiceGap(ctx context.Context) bool {
	if o.power == nil {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
			return true
		}
	}
	return !o.power.Sleep(ctx, 2*time.Second)
}

func (o Options) runCtx(parent context.Context) context.Context {
	if o.power == nil {
		return parent
	}
	return o.power.Ctx()
}

// Sleep waits for d, a wake, or ctx. True means ctx ended.
func (pw *power) Sleep(ctx context.Context, d time.Duration) bool {
	if pw == nil {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(d):
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	case <-pw.wake:
		return ctx.Err() != nil
	}
}
