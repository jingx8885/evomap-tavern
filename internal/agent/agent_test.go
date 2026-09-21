package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestJevGateDedupesExactUtterance(t *testing.T) {
	g := newJevGate()
	if !g.claim("声音好很多了") {
		t.Fatal("first claim")
	}
	if g.claim("声音好很多了") {
		t.Fatal("inflight should block")
	}
	g.finish("声音好很多了", true)
	if g.claim("声音好很多了") {
		t.Fatal("recent done should block")
	}
	if !g.claim("换一个笑话") {
		t.Fatal("new utterance should claim")
	}
	g.finish("换一个笑话", true)
}

func TestJevGateScheduleDebounces(t *testing.T) {
	g := newJevGate()
	var n atomic.Int32
	g.schedule(40*time.Millisecond, "能听", func() { n.Add(1) })
	g.schedule(40*time.Millisecond, "能听到我说话吗", func() { n.Add(1) })
	time.Sleep(80 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("fired %d times, want 1", n.Load())
	}
	g.cancelTimer()
	g.schedule(80*time.Millisecond, "嗯", func() { n.Add(1) })
	g.cancelTimer()
	time.Sleep(120 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("cancelled timer still fired, got %d", n.Load())
	}
}
