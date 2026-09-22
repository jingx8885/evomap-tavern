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

// SceneCue is the relationship/context slice the voice model may use this turn.
type SceneCue struct {
	Stage        string
	Summary      string
	OpenLoops    []string
	SharedEvents []string
}

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

func cueDirective(cue SceneCue) string {
	if cue.Stage == "" && cue.Summary == "" && len(cue.OpenLoops) == 0 && len(cue.SharedEvents) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relationship scene: ")
	if cue.Stage != "" {
		fmt.Fprintf(&b, "stage=%s. ", cue.Stage)
	}
	if cue.Summary != "" {
		fmt.Fprintf(&b, "%s. ", cue.Summary)
	}
	if len(cue.OpenLoops) > 0 {
		fmt.Fprintf(&b, "Open loops you can revisit naturally: %s. ", strings.Join(tailStrings(cue.OpenLoops, 2), " / "))
	}
	if len(cue.SharedEvents) > 0 {
		fmt.Fprintf(&b, "Shared events you can recall: %s. ", strings.Join(tailStrings(cue.SharedEvents, 2), " / "))
	}
	b.WriteString("Use this like memory, not a database dump. Do not read it aloud unless it fits the turn.")
	return b.String()
}

// Build composes the next session instructions.
func Build(p *persona.Persona, mode string, j *judge.Judgment,
	a memory.Affect, planNote string) string {
	return BuildWithScene(p, mode, j, a, planNote, SceneCue{})
}

// BuildWithScene is the character-facing steering note: short, current-scene,
// and centered on her own emotion rather than the user's emotion alone.
// Persona bible stays in BaseInstructions; repeating it here blows the
// developer-channel 500-token cap and the gateway drops the note.
func BuildWithScene(p *persona.Persona, mode string, j *judge.Judgment,
	a memory.Affect, planNote string, cue SceneCue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stay in character as %s. ", p.Name)
	fmt.Fprintf(&b, "Scene %s: %s ", mode, modeDirective(p, mode))
	if j != nil && j.SelfEmotion != "" {
		fmt.Fprintf(&b, "Her feeling: %s. ", j.SelfEmotion)
	}
	fmt.Fprintf(&b, "User: emotion=%s valence=%.2f arousal=%.2f engagement=%.2f",
		j.Emotion, a.Valence, a.Arousal, j.Engagement)
	if j != nil && j.Intent != "" {
		fmt.Fprintf(&b, " intent=%s", j.Intent)
	}
	b.WriteString(". ")
	if cueText := cueDirective(cue); cueText != "" {
		b.WriteString(cueText + " ")
	}
	if planNote != "" && mode != "de_escalate" && mode != "comfort" && mode != "safety" {
		fmt.Fprintf(&b, "Thread: %s ", planNote)
	}
	if j != nil && j.OffPersona(p.Judge.FitThresh) {
		b.WriteString("Last reply drifted; snap back without announcing it. ")
	}
	b.WriteString("Show this scene in your voice on this turn, not later. ")
	b.WriteString("Do not greet, re-introduce yourself, or say your name. ")
	b.WriteString("Do not read labels aloud. Reply in the user's language; keep it short enough for voice.")
	return b.String()
}

func tailStrings(list []string, n int) []string {
	if n <= 0 || len(list) <= n {
		return append([]string(nil), list...)
	}
	return append([]string(nil), list[len(list)-n:]...)
}
