package planner

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

type fakeJev struct {
	pause, remaining float64
	calls            atomic.Int32
	err              error
}

func (f *fakeJev) Evaluate(_ context.Context, _ any, qs map[string]jev.Question) (*jev.EvalResult, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &jev.EvalResult{Answers: map[string]jev.Answer{
		"natural_pause":   {Type: "noul", Noul: &f.pause},
		"goals_remaining": {Type: "noul", Noul: &f.remaining},
	}}, nil
}

type fakeLLM struct {
	out   string
	err   error
	calls atomic.Int32
}

func (f *fakeLLM) ChatComplete(_ context.Context, _, _ string) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func newPlanner(j Evaluator, l Completer) *Planner {
	return New(&persona.Persona{
		Name: "test", Goals: []string{"g1", "g2"},
		Planner: persona.PlannerConfig{Enabled: true, IntervalTurn: 1},
	}, j, l, "m")
}

func TestGateSkipsWhenNoPause(t *testing.T) {
	fj := &fakeJev{pause: 0.1, remaining: 0.9}
	fl := &fakeLLM{out: `{"note":"x"}`}
	pl := newPlanner(fj, fl)
	pl.Tick(context.Background(), memory.New(4), false)
	if fl.calls.Load() != 0 {
		t.Fatal("LLM must not run when gate fails")
	}
}

func TestRefineAppliesLLM(t *testing.T) {
	fj := &fakeJev{pause: 0.9, remaining: 0.9}
	fl := &fakeLLM{out: `{"note":"ask about the map","goal_status":{"g1":"active"}}`}
	pl := newPlanner(fj, fl)
	mem := memory.New(4)
	mem.Add(memory.Turn{Speaker: "user", Text: "hi"})
	pl.Tick(context.Background(), mem, false)
	deadline := time.Now().Add(2 * time.Second)
	for pl.State().Source != "llm" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := pl.Current(); got != "ask about the map" {
		t.Fatalf("note=%q", got)
	}
	if pl.State().GoalStatus["g1"] != "active" {
		t.Fatalf("goal status not applied")
	}
}

func TestConsumeNudgeOnce(t *testing.T) {
	pl := newPlanner(&fakeJev{}, &fakeLLM{})
	before := time.Now().Add(-time.Second)
	pl.mu.Lock()
	pl.plan.Note = "ask about the map"
	pl.plan.Source = "llm"
	pl.plan.LastRefreshed = time.Now()
	pl.mu.Unlock()
	note, ok := pl.ConsumeNudge(before)
	if !ok || note != "ask about the map" {
		t.Fatalf("first consume: note=%q ok=%v", note, ok)
	}
	if note, ok := pl.ConsumeNudge(before); ok {
		t.Fatalf("second consume must be empty, got %q", note)
	}
}

func TestFallbackOnLLMError(t *testing.T) {
	fj := &fakeJev{pause: 0.9, remaining: 0.9}
	fl := &fakeLLM{err: fmt.Errorf("boom")}
	pl := newPlanner(fj, fl)
	pl.Tick(context.Background(), memory.New(4), false)
	deadline := time.Now().Add(2 * time.Second)
	for pl.State().LastRefreshed.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pl.Current() == "" || pl.State().Source != "persona" {
		t.Fatalf("fallback not applied: %+v", pl.State())
	}
}
