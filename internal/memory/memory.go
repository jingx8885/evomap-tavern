// Package memory is the short-term memory of the conversation:
// recent turns plus an exponential-moving-average affect state that
// Jev judgments continuously update.
package memory

import (
	"fmt"
	"strings"
	"sync"
)

const emaAlpha = 0.4

// Turn is one utterance or reply.
type Turn struct {
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	Emotion string  `json:"emotion,omitempty"`
	Valence float64 `json:"valence,omitempty"`
	Arousal float64 `json:"arousal,omitempty"`
}

// Affect is the running emotional state.
type Affect struct {
	Valence   float64 `json:"valence"`
	Arousal   float64 `json:"arousal"`
	Emotion   string  `json:"emotion"`
	SafetyHit bool    `json:"safety_hit"`
}

// Memory is goroutine-safe short-term memory.
type Memory struct {
	mu       sync.Mutex
	turns    []Turn
	maxTurns int
	affect   Affect
}

// New creates memory keeping the last maxTurns turns.
func New(maxTurns int) *Memory {
	if maxTurns <= 0 {
		maxTurns = 8
	}
	return &Memory{maxTurns: maxTurns}
}

// Add appends a turn.
func (m *Memory) Add(t Turn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = append(m.turns, t)
	if len(m.turns) > m.maxTurns {
		m.turns = m.turns[len(m.turns)-m.maxTurns:]
	}
}

// UpdateAffect folds a judgment into the EMA state.
func (m *Memory) UpdateAffect(valence, arousal float64, emotion string, safety bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.affect.Emotion == "" && !m.affect.SafetyHit {
		m.affect.Valence = valence
		m.affect.Arousal = arousal
	} else {
		m.affect.Valence = emaAlpha*valence + (1-emaAlpha)*m.affect.Valence
		m.affect.Arousal = emaAlpha*arousal + (1-emaAlpha)*m.affect.Arousal
	}
	if emotion != "" {
		m.affect.Emotion = emotion
	}
	if safety {
		m.affect.SafetyHit = true
	}
}

// Affect returns the current running affect snapshot.
func (m *Memory) Affect() Affect {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.affect
}

// Turns returns a copy of recent turns.
func (m *Memory) Turns() []Turn {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Turn, len(m.turns))
	copy(out, m.turns)
	return out
}

// LatestAssistantText returns the newest assistant reply, if any.
func (m *Memory) LatestAssistantText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.turns) - 1; i >= 0; i-- {
		if m.turns[i].Speaker == "assistant" {
			return m.turns[i].Text
		}
	}
	return ""
}

// LatestUserText returns the newest user utterance, if any.
func (m *Memory) LatestUserText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.turns) - 1; i >= 0; i-- {
		if m.turns[i].Speaker == "user" {
			return m.turns[i].Text
		}
	}
	return ""
}

// Recent renders the last n turns as compact one-line strings.
func (m *Memory) Recent(n int) []string {
	turns := m.Turns()
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	out := make([]string, len(turns))
	for i, t := range turns {
		out[i] = fmt.Sprintf("%s: %s", t.Speaker, t.Text)
	}
	return out
}

// ValenceTrend compares current EMA valence to the previous judgment's
// valence (positive = mood improving).
func (m *Memory) ValenceTrend() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.turns) - 1; i >= 0; i-- {
		if m.turns[i].Speaker == "user" && m.turns[i].Valence != 0 {
			return m.affect.Valence - m.turns[i].Valence
		}
	}
	return 0
}

// String renders affect for logs.
func (a Affect) String() string {
	return fmt.Sprintf("valence=%.2f arousal=%.2f emotion=%s safety=%v",
		a.Valence, a.Arousal, or(a.Emotion, "-"), a.SafetyHit)
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
