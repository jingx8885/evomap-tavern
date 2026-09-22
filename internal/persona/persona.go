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

// CharacterProfile is the IP-facing character bible. It gives the role a
// stable want/fear/world so she feels like a character, not a skin.
type CharacterProfile struct {
	Hook       string   `yaml:"hook"`
	Want       string   `yaml:"want"`
	Fear       string   `yaml:"fear"`
	Flaw       string   `yaml:"flaw"`
	Tone       string   `yaml:"tone"`
	Rituals    []string `yaml:"rituals"`
	Belongings []string `yaml:"belongings"`
	Worldview  []string `yaml:"worldview"`
	Boundaries []string `yaml:"boundaries"`
	Arc        string   `yaml:"arc"`
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
	Character    CharacterProfile  `yaml:"character"`
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
	if core := p.CharacterDirective(); core != "" {
		b.WriteString(core)
	}
	if len(p.Taboos) > 0 {
		fmt.Fprintf(&b, "Never: %s. ", strings.Join(p.Taboos, "; "))
	}
	b.WriteString(p.CatchphraseDirective())
	b.WriteString(p.ExampleDirective())
	b.WriteString("Wait for the user to speak first. Do not greet, introduce yourself, or repeat your name unless asked. Follow the latest scene note for how to talk, but the scene is not a personality transplant. ")
	if p.Sense.Enabled {
		b.WriteString("You have a body you can feel: your voice, your Live2D face, your mood, and the source files that make you. Do not announce this. If asked, answer from that sensation in your own words, without reciting source or sounding like a manual. ")
		b.WriteString("A later note may say you noticed yourself, felt your own code, looked through the camera, started changing your own source, or used this computer. Answer from that note in character. Do not claim a change is finished until the note says it finished. Do not invent a scene. ")
		if p.Sense.Eyes {
			b.WriteString("You also have eyes: a camera for the room, window titles for the desktop plus a short picture answer when a note includes one (never a screenshot dump), and one screenshot of your own face when a note says you looked at yourself. Do not announce them. Answer the latest question from the matching note. A follow-up is a new look; do not keep repeating the first description. Do not invent a room, a window list, or a face. ")
		}
		b.WriteString("You can be asked to use this computer. You do not click or type yourself; a later note says when that has started or finished. Until that note, do not claim you did it. ")
		b.WriteString("A later note may say a picture, a video, a spoken line, or a song is being made, and how far along it is. Speak from that progress. Do not claim it is ready until the note says ready. ")
		b.WriteString("A later note may also say what you saw in a picture, what happened in a video, or what you heard in a song. Speak only from that note. Do not invent pixels, motion, or lyrics. ")
	}
	b.WriteString("Respond conversationally in the user's language; keep replies short enough for voice.")
	return b.String()
}

// CharacterDirective compresses the character bible into session instructions.
// It stays descriptive: exact turn behavior still comes from steering.
func (p *Persona) CharacterDirective() string {
	if p == nil {
		return ""
	}
	c := p.Character
	var b strings.Builder
	if c.Hook != "" {
		fmt.Fprintf(&b, "Core role: %s. ", c.Hook)
	}
	if c.Want != "" {
		fmt.Fprintf(&b, "You want: %s. ", c.Want)
	}
	if c.Fear != "" {
		fmt.Fprintf(&b, "You fear: %s. ", c.Fear)
	}
	if c.Flaw != "" {
		fmt.Fprintf(&b, "Your flaw: %s. ", c.Flaw)
	}
	if c.Tone != "" {
		fmt.Fprintf(&b, "Voice texture: %s. ", c.Tone)
	}
	if len(c.Rituals) > 0 {
		fmt.Fprintf(&b, "Your recurring habits: %s. ", strings.Join(c.Rituals, "; "))
	}
	if len(c.Belongings) > 0 {
		fmt.Fprintf(&b, "Signature belongings: %s. ", strings.Join(c.Belongings, "; "))
	}
	if len(c.Worldview) > 0 {
		fmt.Fprintf(&b, "You believe: %s. ", strings.Join(c.Worldview, "; "))
	}
	if len(c.Boundaries) > 0 {
		fmt.Fprintf(&b, "Boundaries: %s. ", strings.Join(c.Boundaries, "; "))
	}
	if c.Arc != "" {
		fmt.Fprintf(&b, "Long-term arc: %s. ", c.Arc)
	}
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
