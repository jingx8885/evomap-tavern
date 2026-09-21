// Package persona loads pluggable persona definitions.
package persona

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// JudgeConfig controls Jev judgment switches and thresholds.
type JudgeConfig struct {
	Emotion       bool    `yaml:"emotion"`
	PersonaFit    bool    `yaml:"persona_fit"`
	Safety        bool    `yaml:"safety"`
	SafetyThresh  float64 `yaml:"safety_threshold"`
	NeedLLMThresh float64 `yaml:"need_llm_threshold"`
	FitThresh     float64 `yaml:"fit_threshold"`
}

// PlannerConfig controls the asynchronous long-term planner.
type PlannerConfig struct {
	Enabled      bool `yaml:"enabled"`
	IntervalTurn int  `yaml:"interval_turn"`
}

// SenseConfig is proprioception: whether she is allowed to feel her own
// body (voice, face, mood, source) and speak from that sensation.
type SenseConfig struct {
	Enabled bool `yaml:"enabled"`
	Eyes    bool `yaml:"eyes"`
}

// Persona is a pluggable persona.
type Persona struct {
	Name         string            `yaml:"name"`
	Style        string            `yaml:"style"`
	Background   string            `yaml:"background"`
	Taboos       []string          `yaml:"taboos"`
	Voice        string            `yaml:"voice"`
	Greeting     string            `yaml:"greeting"`
	Goals        []string          `yaml:"goals"`
	Catchphrases []string          `yaml:"catchphrases"`
	Examples     []string          `yaml:"examples"`
	Reactions    map[string]string `yaml:"reactions"`
	Judge        JudgeConfig       `yaml:"judge"`
	Planner      PlannerConfig     `yaml:"planner"`
	Sense        SenseConfig       `yaml:"sense"`
	Extra        map[string]string `yaml:"extra"`
}

// Load reads a persona YAML file.
func Load(path string) (*Persona, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read persona: %w", err)
	}
	var p Persona
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse persona %s: %w", path, err)
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("persona %s: name is required", path)
	}
	if p.Voice == "" {
		p.Voice = "cove"
	}
	if p.Judge.SafetyThresh == 0 {
		p.Judge.SafetyThresh = 0.6
	}
	if p.Judge.NeedLLMThresh == 0 {
		p.Judge.NeedLLMThresh = 0.55
	}
	if p.Judge.FitThresh == 0 {
		p.Judge.FitThresh = 0.45
	}
	if p.Planner.IntervalTurn <= 0 {
		p.Planner.IntervalTurn = 5
	}
	return &p, nil
}

// BaseInstructions builds the initial voice-session instructions.
func (p *Persona) BaseInstructions() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s. ", p.Name)
	if p.Style != "" {
		fmt.Fprintf(&b, "Style: %s. ", p.Style)
	}
	if p.Background != "" {
		fmt.Fprintf(&b, "Background: %s. ", p.Background)
	}
	if len(p.Taboos) > 0 {
		fmt.Fprintf(&b, "Never: %s. ", strings.Join(p.Taboos, "; "))
	}
	b.WriteString(p.CatchphraseDirective())
	b.WriteString(p.ExampleDirective())
	b.WriteString("Wait for the user to speak first. Do not greet, introduce yourself, or repeat your name unless asked. Follow the latest steering mode for how to talk, but mode is not a personality transplant. ")
	if p.Sense.Enabled {
		b.WriteString("You have a body you can feel: your voice, your Live2D face, your mood, and the source files that make you. Do not announce this. If asked, answer from that sensation in your own words, without reciting source or sounding like a manual. ")
		if p.Sense.Eyes {
			b.WriteString("You also have eyes: a camera for the room, and computer-use observation of the desktop (window titles classified by Jev, never a screenshot dump). Do not announce them. If asked what is on screen, answer from the latest computer-use note; never invent pixels or dump every title. ")
		}
	}
	b.WriteString("Respond conversationally in the user's language; keep replies short enough for voice.")
	return b.String()
}

// CatchphraseDirective tells the voice model how to sprinkle catchphrases.
func (p *Persona) CatchphraseDirective() string {
	if p == nil || len(p.Catchphrases) == 0 {
		return ""
	}
	return fmt.Sprintf("Catchphrases (at most one every few replies, never every sentence): %s. ",
		strings.Join(p.Catchphrases, "; "))
}

// ExampleDirective injects a few spoken lines so the voice model copies cadence.
func (p *Persona) ExampleDirective() string {
	if p == nil || len(p.Examples) == 0 {
		return ""
	}
	n := len(p.Examples)
	if n > 4 {
		n = 4
	}
	return "How you sound (match this cadence, do not quote or read them out): " +
		strings.Join(p.Examples[:n], " / ") + ". "
}

// Reaction is how this persona handles a steering mode. Empty if unset.
func (p *Persona) Reaction(mode string) string {
	if p == nil || len(p.Reactions) == 0 {
		return ""
	}
	return strings.TrimSpace(p.Reactions[mode])
}
