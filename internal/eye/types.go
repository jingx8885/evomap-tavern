// Package eye is her outer sight: camera and screen run in parallel,
// Jev gates whether a change is worth a vision-model caption, and the
// caption lands on the sense bus as something she can feel.
package eye

import (
	"context"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/jev"
)

const (
	SourceCamera = "camera"
	SourceScreen = "screen"
)

// Evaluator is the Jev surface the gate needs.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.EvalResult, error)
}

// Visioner captions one JPEG.
type Visioner interface {
	ChatVision(ctx context.Context, system, user string, jpeg []byte) (string, error)
}

// Frame is one compressed glance. JPEG is small enough to keep around.
type Frame struct {
	Source string
	JPEG   []byte
	Width  int
	Height int
	Thumb  []byte
	At     time.Time
}

// Glimpse is what she currently knows about one source.
type Glimpse struct {
	Source    string    `json:"source"`
	Caption   string    `json:"caption,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Delta     float64   `json:"delta,omitempty"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	Bytes     int       `json:"bytes,omitempty"`
	Ready     bool      `json:"ready,omitempty"`
	Private   bool      `json:"private,omitempty"`
	Noted     bool      `json:"noted,omitempty"`
}

// Sight is both eyes at once.
type Sight struct {
	Camera  Glimpse `json:"camera"`
	Screen  Glimpse `json:"screen"`
	Mention bool    `json:"mention,omitempty"`
}

// Options configure the parallel eyes.
type Options struct {
	Camera   bool
	Screen   bool
	Interval time.Duration
	Cooldown time.Duration
	Grab     func() (anyJPEG []byte, err error) // tests inject; nil = native screen
	Jev      Evaluator
	LLM      Visioner
	LogFn    func(string)
	OnSight  func(Sight)
}

// ParseSources reads a flag like "both", "off", "camera", "screen", "camera,screen".
func ParseSources(s string) (camera, screen bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "both", "on", "true", "all":
		return true, true
	case "off", "none", "false", "0":
		return false, false
	}
	for _, p := range strings.Split(s, ",") {
		switch strings.TrimSpace(p) {
		case "camera", "cam":
			camera = true
		case "screen", "scr", "desktop":
			screen = true
		}
	}
	return camera, screen
}
