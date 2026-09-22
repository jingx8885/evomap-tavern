package window

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	FontSans  = "sans"
	FontSerif = "serif"
	FontMono  = "mono"

	RadiusTight = "tight"
	RadiusSoft  = "soft"
	RadiusRound = "round"

	FramePlain = "plain"
	FrameLine  = "line"
	FrameGlow  = "glow"

	ControlLabel = "label"
	ControlNote  = "note"
	ControlChip  = "chip"
	ControlRule  = "rule"
	ControlMeter = "meter"

	maxControls = 8
)

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Theme is the checked decoration of the stage page.
// The slow model may propose one; only these fields are applied.
type Theme struct {
	Background string `json:"background"`
	Desk       string `json:"desk"`
	Ink        string `json:"ink"`
	Muted      string `json:"muted"`
	Accent     string `json:"accent"`
	Card       string `json:"card"`
	Line       string `json:"line"`
	Font       string `json:"font"`
	Radius     string `json:"radius"`
	Frame      string `json:"frame"`
}

// Control is one added widget. Kind is a closed set; text is plain.
type Control struct {
	ID    string  `json:"id"`
	Kind  string  `json:"kind"`
	Text  string  `json:"text,omitempty"`
	Value float64 `json:"value,omitempty"`
}

// DefaultTheme matches the page before anyone restyles it.
func DefaultTheme() Theme {
	return Theme{
		Background: "#100e0c",
		Desk:       "#1b1714",
		Ink:        "#f3ece4",
		Muted:      "#c8b8a4",
		Accent:     "#e0b15a",
		Card:       "#221c18",
		Line:       "#3a332c",
		Font:       FontSans,
		Radius:     RadiusSoft,
		Frame:      FrameLine,
	}
}

// ParseTheme reads one JSON object from a model draft and checks every field.
func ParseTheme(raw string) (Theme, error) {
	var t Theme
	if err := json.Unmarshal([]byte(extractJSON(raw)), &t); err != nil {
		return Theme{}, fmt.Errorf("theme json: %w", err)
	}
	t.Font = strings.ToLower(strings.TrimSpace(t.Font))
	t.Radius = strings.ToLower(strings.TrimSpace(t.Radius))
	t.Frame = strings.ToLower(strings.TrimSpace(t.Frame))
	for _, c := range []string{t.Background, t.Desk, t.Ink, t.Muted, t.Accent, t.Card, t.Line} {
		if !hexColor.MatchString(c) {
			return Theme{}, fmt.Errorf("theme color %q", c)
		}
	}
	switch t.Font {
	case FontSans, FontSerif, FontMono:
	default:
		return Theme{}, fmt.Errorf("theme font %q", t.Font)
	}
	switch t.Radius {
	case RadiusTight, RadiusSoft, RadiusRound:
	default:
		return Theme{}, fmt.Errorf("theme radius %q", t.Radius)
	}
	switch t.Frame {
	case FramePlain, FrameLine, FrameGlow:
	default:
		return Theme{}, fmt.Errorf("theme frame %q", t.Frame)
	}
	return t, nil
}

// ParseControls reads a JSON object {"controls":[...]} and checks each widget.
func ParseControls(raw string) ([]Control, error) {
	var body struct {
		Controls []Control `json:"controls"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &body); err != nil {
		return nil, fmt.Errorf("controls json: %w", err)
	}
	if len(body.Controls) == 0 {
		return nil, fmt.Errorf("no controls")
	}
	if len(body.Controls) > maxControls {
		return nil, fmt.Errorf("too many controls")
	}
	out := make([]Control, 0, len(body.Controls))
	seen := map[string]bool{}
	for i, c := range body.Controls {
		kind := strings.ToLower(strings.TrimSpace(c.Kind))
		switch kind {
		case ControlLabel, ControlNote, ControlChip, ControlRule, ControlMeter:
		default:
			return nil, fmt.Errorf("control kind %q", c.Kind)
		}
		text := plainText(c.Text, 80)
		if kind != ControlRule && text == "" {
			return nil, fmt.Errorf("control %d needs text", i)
		}
		if kind == ControlMeter {
			if c.Value < 0 || c.Value > 1 {
				return nil, fmt.Errorf("meter value")
			}
		} else {
			c.Value = 0
		}
		id := strings.ToLower(strings.TrimSpace(c.ID))
		if !controlID(id) {
			id = fmt.Sprintf("c%d", i+1)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate control %s", id)
		}
		seen[id] = true
		out = append(out, Control{ID: id, Kind: kind, Text: text, Value: c.Value})
	}
	return out, nil
}

// SetTheme replaces the decoration. An invalid theme leaves the page as it was.
func (s *Stage) SetTheme(t Theme) error {
	checked, err := ParseTheme(mustJSON(t))
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("stage required")
	}
	s.mu.Lock()
	s.theme = checked
	s.mu.Unlock()
	s.ping()
	return nil
}

// Theme returns a copy of the current decoration.
func (s *Stage) Theme() Theme {
	if s == nil {
		return DefaultTheme()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.theme
}

// AddControls appends checked widgets, replacing any with the same id.
func (s *Stage) AddControls(list []Control) error {
	if s == nil {
		return fmt.Errorf("stage required")
	}
	if len(list) == 0 {
		return fmt.Errorf("no controls")
	}
	raw, err := json.Marshal(map[string]any{"controls": list})
	if err != nil {
		return err
	}
	checked, err := ParseControls(string(raw))
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, c := range checked {
		replaced := false
		for i := range s.controls {
			if s.controls[i].ID == c.ID {
				s.controls[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			s.controls = append(s.controls, c)
		}
	}
	if len(s.controls) > maxControls {
		s.controls = append([]Control(nil), s.controls[len(s.controls)-maxControls:]...)
	}
	s.mu.Unlock()
	s.ping()
	return nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func plainText(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '<' || r == '>' || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, s)
	return clip(s, n)
}

func controlID(id string) bool {
	if id == "" || len(id) > 16 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' || r == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
