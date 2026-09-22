// Package sense is 小春's nervous system: an event log operators can
// watch, and proprioception so she can feel her own running body —
// voice, face, mood, and the source files that make her.
package sense

import "time"

// Event kinds. Stable strings; they show up in JSONL and /api/sense.
const (
	KindVoice   = "voice"
	KindTurn    = "turn"
	KindJudge   = "judge"
	KindSteer   = "steer"
	KindPlan    = "plan"
	KindDesk    = "desk"
	KindMake    = "make"
	KindAvatar  = "avatar"
	KindLook    = "look"
	KindSee     = "see"
	KindCommand = "command"
	KindError   = "error"
	KindWarning = "warning"
)

// Event is one moment she (or an operator) can look back at.
type Event struct {
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Summary string         `json:"summary,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}
