// Package planner is the asynchronous long-term planner. Each tick asks
// Jev whether the conversation is at a natural pause AND still has
// unmet goals; only then does it spend an LLM call to refine guidance.
// Refinement runs in a goroutine so it never blocks the voice loop.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

// Evaluator is the Jev surface the planner needs.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.EvalResult, error)
}

// Completer is the LLM surface the planner needs.
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// Plan is the current long-term guidance state.
type Plan struct {
	Note          string            `json:"note"`
	Source        string            `json:"source"` // persona | llm | manual
	GoalStatus    map[string]string `json:"goal_status,omitempty"`
	LastRefreshed time.Time         `json:"last_refreshed,omitempty"`
}

// Planner owns the plan and the async refine pipeline.
type Planner struct {
	Persona *persona.Persona
	Jev     Evaluator
	LLM     Completer
	Model   string
	LogFn   func(string)

	mu         sync.Mutex
	plan       Plan
	turns      int
	refining   bool
	nextGoalIx int
	nudgedAt   time.Time
}

// New builds a planner; initial guidance comes from persona goals.
func New(p *persona.Persona, j Evaluator, l Completer, model string) *Planner {
	pl := &Planner{Persona: p, Jev: j, LLM: l, Model: model}
	if len(p.Goals) > 0 {
		pl.plan = Plan{
			Note:   "Your first long-term goal: " + p.Goals[0],
			Source: "persona",
		}
	}
	return pl
}

// Current returns the current plan note (empty if none).
func (pl *Planner) Current() string {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.plan.Note
}

// State returns the plan snapshot for logs.
func (pl *Planner) State() Plan {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.plan
}

// Refining reports whether an async LLM refine is in flight.
func (pl *Planner) Refining() bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.refining
}

// SetNote overrides the plan note (e.g. the /goal command).
func (pl *Planner) SetNote(note string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.plan.Note = note
	pl.plan.Source = "manual"
}

// ConsumeNudge returns a newly refined LLM note once. Duplicate waiters
// (and duplicate turn.done events) must not commentary-nudge twice.
func (pl *Planner) ConsumeNudge(since time.Time) (string, bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.plan.Source != "llm" || !pl.plan.LastRefreshed.After(since) {
		return "", false
	}
	if !pl.nudgedAt.Before(pl.plan.LastRefreshed) {
		return "", false
	}
	pl.nudgedAt = pl.plan.LastRefreshed
	return pl.plan.Note, true
}

func gateQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"natural_pause": {
			Type: "noul",
			Instructions: "Is the conversation in state at a natural pause where " +
				"steering the long-term direction would not interrupt the user? " +
				"Consider engagement and whether a topic just wrapped up.",
		},
		"goals_remaining": {
			Type: "noul",
			Instructions: "Given state.goals and state.recent, is there still " +
				"meaningful long-term guidance the conversation has not " +
				"yet achieved?",
		},
	}
}

// Tick advances one turn. When the interval hits and the Jev gate
// passes, it launches an asynchronous LLM refine.
func (pl *Planner) Tick(ctx context.Context, mem *memory.Memory, force bool) {
	pl.turns++
	if !pl.Persona.Planner.Enabled && !force {
		return
	}
	if pl.turns%pl.Persona.Planner.IntervalTurn != 0 && !force {
		return
	}
	pl.mu.Lock()
	if pl.refining {
		pl.mu.Unlock()
		return
	}
	pl.mu.Unlock()

	state := map[string]any{
		"persona": map[string]string{"name": pl.Persona.Name, "style": pl.Persona.Style},
		"goals":   pl.Persona.Goals,
		"recent":  mem.Recent(8),
		"affect":  mem.Affect(),
		"plan":    pl.State(),
	}
	res, err := pl.Jev.Evaluate(ctx, state, gateQuestions())
	if err != nil {
		pl.logf("planner gate failed: %v", err)
		return
	}
	pause := noul(res.Answers["natural_pause"])
	remaining := noul(res.Answers["goals_remaining"])
	if !force && (pause < 0.55 || remaining < 0.4) {
		pl.logf("planner gate: pause=%.2f remaining=%.2f -> skip", pause, remaining)
		return
	}

	pl.mu.Lock()
	pl.refining = true
	pl.mu.Unlock()
	pl.logf("planner gate: pause=%.2f remaining=%.2f -> refining (async)", pause, remaining)

	go pl.refine(state)
}

// refine calls the LLM in the background and applies the result.
func (pl *Planner) refine(state map[string]any) {
	defer func() {
		pl.mu.Lock()
		pl.refining = false
		pl.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	system := "You are the long-term planner inside a persona voice bot. " +
		"Keep guidance to one short behavioral directive for the voice model " +
		"(what to steer toward, not exact words). Output STRICT JSON only."
	user := fmt.Sprintf("Persona: %s (%s)\nGoals: %s\nRecent: %s\nAffect: %+v\nCurrent plan: %s\n\n"+
		"Return JSON: {\"note\": \"<one-line steering guidance>\", \"goal_status\": {\"<goal>\": \"done|active|abandoned\"}}",
		pl.Persona.Name, pl.Persona.Style,
		strings.Join(pl.Persona.Goals, " | "),
		strings.Join(toStrings(state["recent"]), " / "),
		state["affect"], pl.Current())

	text, err := pl.LLM.ChatComplete(ctx, system, user)
	if err != nil {
		pl.logf("planner llm failed: %v", err)
		pl.fallbackRotate()
		return
	}
	var out struct {
		Note       string            `json:"note"`
		GoalStatus map[string]string `json:"goal_status"`
	}
	if err := json.Unmarshal([]byte(extractJSON(text)), &out); err != nil ||
		strings.TrimSpace(out.Note) == "" {
		pl.logf("planner llm unparsable: %.120s", text)
		pl.fallbackRotate()
		return
	}
	pl.mu.Lock()
	pl.plan.Note = out.Note
	pl.plan.Source = "llm"
	pl.plan.GoalStatus = out.GoalStatus
	pl.plan.LastRefreshed = time.Now()
	pl.mu.Unlock()
	pl.logf("planner refined: %s", out.Note)
}

// fallbackRotate advances through persona goals without an LLM.
func (pl *Planner) fallbackRotate() {
	if len(pl.Persona.Goals) == 0 {
		return
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.nextGoalIx = (pl.nextGoalIx + 1) % len(pl.Persona.Goals)
	pl.plan.Note = "Shift toward: " + pl.Persona.Goals[pl.nextGoalIx]
	pl.plan.Source = "persona"
	pl.plan.LastRefreshed = time.Now()
}

func noul(a jev.Answer) float64 {
	if a.Noul == nil {
		return 0
	}
	return *a.Noul
}

func toStrings(v any) []string {
	if ss, ok := v.([]string); ok {
		return ss
	}
	return nil
}

func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func (pl *Planner) logf(format string, args ...any) {
	if pl.LogFn != nil {
		pl.LogFn(fmt.Sprintf(format, args...))
	}
}
