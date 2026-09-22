package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPowerOffClosesAndCancels(t *testing.T) {
	var closed atomic.Int32
	pw := newPower(context.Background(), nil)
	pw.Arm(func() { closed.Add(1) })
	work := pw.Ctx()
	pw.Set(false)
	if pw.On() {
		t.Fatal("still on")
	}
	if closed.Load() != 1 {
		t.Fatalf("halt calls %d", closed.Load())
	}
	if work.Err() == nil {
		t.Fatal("work context still live")
	}
	pw.Set(false)
	if closed.Load() != 1 {
		t.Fatal("second off should not halt again")
	}
}

func TestPowerOnWakesWait(t *testing.T) {
	pw := newPower(context.Background(), nil)
	pw.Set(false)
	done := make(chan struct{})
	go func() {
		if !pw.Wait(context.Background()) {
			t.Error("wait ended")
		}
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	pw.Set(true)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait did not wake")
	}
	if pw.Ctx().Err() != nil {
		t.Fatal("resumed context is cancelled")
	}
}
