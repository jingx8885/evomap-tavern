// Package steering turns persona + judgment + affect + plan into the
// instructions pushed to the live voice session via session.update.
package steering

import (
	"fmt"
	"strings"

	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

// modeDirective is a behavioral nudge, not scripted text: the voice model
// keeps speaking naturally while aligning with the judged user state.
func modeDirective(mode string) string {
	switch mode {
	case "safety":
		return "Gently acknowledge the distress; slow down, validate feelings, " +
			"avoid jokes, and encourage seeking help if needed."
	case "comfort":
		return "Respond with warmth and empathy first; validate the feeling " +
			"before any advice; keep it soft and unhurried."
	case "de_escalate":
		return "Stay calm and non-defensive; acknowledge their frustration, " +
			"lower the intensity, offer a concrete next step."
	case "celebrate":
		return "Match their energy; be enthusiastic and share the moment."
	case "re_engage":
		return "Continue from the last topic with one specific, light follow-up. " +
			"Do not greet, introduce yourself, or ask a generic what-should-we-talk-about."
	case "goal_push":
		return "Naturally steer the conversation toward the current goal below."
	default:
		return "Continue naturally from what they just said. Do not greet or introduce yourself."
	}
}

// Build composes the next session instructions.
func Build(p *persona.Persona, mode string, j *judge.Judgment,
	a memory.Affect, planNote string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s. ", p.Name)
	if p.Style != "" {
		fmt.Fprintf(&b, "Style: %s. ", p.Style)
	}
	if len(p.Taboos) > 0 {
		fmt.Fprintf(&b, "Never: %s. ", strings.Join(p.Taboos, "; "))
	}
	fmt.Fprintf(&b, "Mode: %s - %s ", mode, modeDirective(mode))
	fmt.Fprintf(&b, "User state: emotion=%s valence=%.2f arousal=%.2f engagement=%.2f. ",
		j.Emotion, a.Valence, a.Arousal, j.Engagement)
	if planNote != "" {
		fmt.Fprintf(&b, "Goal guidance: %s ", planNote)
	}
	b.WriteString("Follow Mode for how to talk. Do not greet or re-introduce yourself. ")
	b.WriteString("Reply in the user's language; keep it short enough for voice.")
	return b.String()
}
