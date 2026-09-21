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
	Emotion      bool    `yaml:"emotion"`
	PersonaFit   bool    `yaml:"persona_fit"`
	Safety       bool    `yaml:"safety"`
	SafetyThresh float64 `yaml:"safety_threshold"`
}

// PlannerConfig controls the asynchronous long-term planner.
type PlannerConfig struct {
	Enabled      bool `yaml:"enabled"`
	IntervalTurn int  `yaml:"interval_turn"`
}

// Persona is a pluggable persona.
type Persona struct {
	Name       string            `yaml:"name"`
	Style      string            `yaml:"style"`
	Background string            `yaml:"background"`
	Taboos     []string          `yaml:"taboos"`
	Voice      string            `yaml:"voice"`
	Greeting   string            `yaml:"greeting"`
	Goals      []string          `yaml:"goals"`
	Judge      JudgeConfig       `yaml:"judge"`
	Planner    PlannerConfig     `yaml:"planner"`
	Extra      map[string]string `yaml:"extra"`
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
	b.WriteString("Respond conversationally in the user's language; keep replies short enough for voice.")
	return b.String()
}
