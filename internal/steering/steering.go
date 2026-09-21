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

func fallbackDirective(mode string) string {
	switch mode {
	case "safety":
		return "This turn: slow down and speak softly. Validate the distress " +
			"before anything else. No jokes, no pep."
	case "comfort":
		return "This turn: warmer and quieter. Lead with empathy; name the " +
			"feeling before any advice. Soft, unhurried voice."
	case "de_escalate":
		return "This turn: drop your energy. Short calm sentences. Acknowledge " +
			"the frustration first. Do not ask where we left off or push a topic."
	case "celebrate":
		return "This turn: actually sound pleased — brighter voice, a little " +
			"laugh or spark, share the moment. Do not stay flat or politely neutral."
	case "re_engage":
		return "This turn: one specific, light follow-up about the last thing " +
			"they said. Do not greet, introduce yourself, or ask a generic " +
			"what-should-we-talk-about."
	case "goal_push":
		return "This turn: react to their mood first, then nudge toward the " +
			"goal. Stay warm; do not sound like a checklist."
	default:
		return "This turn: continue from what they just said. Let your tone " +
			"follow their emotion. Do not greet or go flat."
	}
}

// modeDirective is a behavioral nudge. Persona reactions win: the same
// mode (e.g. comfort) is how THIS character cares, not a therapist script.
func modeDirective(p *persona.Persona, mode string) string {
	if r := p.Reaction(mode); r != "" {
		return "This turn: " + r
	}
	return fallbackDirective(mode)
}

// Build composes the next session instructions.
func Build(p *persona.Persona, mode string, j *judge.Judgment,
	a memory.Affect, planNote string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stay in character as %s. ", p.Name)
	if p.Style != "" {
		fmt.Fprintf(&b, "Style: %s. ", p.Style)
	}
	if len(p.Taboos) > 0 {
		fmt.Fprintf(&b, "Never: %s. ", strings.Join(p.Taboos, "; "))
	}
	b.WriteString(p.CatchphraseDirective())
	b.WriteString(p.ExampleDirective())
	b.WriteString("Mode changes how you handle this turn, not who you are. ")
	fmt.Fprintf(&b, "Mode: %s - %s ", mode, modeDirective(p, mode))
	fmt.Fprintf(&b, "User state: emotion=%s valence=%.2f arousal=%.2f engagement=%.2f",
		j.Emotion, a.Valence, a.Arousal, j.Engagement)
	if j.Intent != "" {
		fmt.Fprintf(&b, " intent=%s", j.Intent)
	}
	b.WriteString(". ")
	if j.SelfEmotion != "" {
		fmt.Fprintf(&b, "Your own feeling this turn: %s. Let it color the reply without naming the label. ",
			j.SelfEmotion)
	}
	if planNote != "" && mode != "de_escalate" && mode != "comfort" && mode != "safety" {
		fmt.Fprintf(&b, "Goal guidance: %s ", planNote)
	}
	if j.OffPersona(p.Judge.FitThresh) {
		b.WriteString("Your last reply drifted off this persona. Snap back to Style and the reaction above without announcing it. ")
	}
	b.WriteString("Show Mode in your voice on this turn, not later. ")
	b.WriteString("Do not greet, re-introduce yourself, or say your name. ")
	b.WriteString("Do not read labels aloud (Mode, User state, Goal, 名字). ")
	b.WriteString("Reply in the user's language; keep it short enough for voice.")
	return b.String()
}
