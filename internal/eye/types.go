// Package eye is her outer sight. Camera, her own face, and the desktop
// are separate looks. Camera frames and one screenshot of herself go
// through a VLM. The screen is not a screenshot: it is computer-use
// observation (window titles classified by Jev).
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
	SourceShot   = "shot"
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
	Source  string    `json:"source"`
	Caption string    `json:"caption,omitempty"`
	At      time.Time `json:"at,omitempty"`
	Delta   float64   `json:"delta,omitempty"`
	Width   int       `json:"width,omitempty"`
	Height  int       `json:"height,omitempty"`
	Bytes   int       `json:"bytes,omitempty"`
	Ready   bool      `json:"ready,omitempty"`
	Private bool      `json:"private,omitempty"`
	Noted   bool      `json:"noted,omitempty"`
}

// Sight is the looks she can take. Shot is a screenshot of her own face.
type Sight struct {
	Camera  Glimpse `json:"camera"`
	Screen  Glimpse `json:"screen"`
	Shot    Glimpse `json:"shot"`
	Mention bool    `json:"mention,omitempty"`
}

// Options configure on-demand eyes. Nothing here starts a clock.
type Options struct {
	Camera   bool
	Screen   bool
	Grab     func(ctx context.Context) bool                // ask the viewer for one camera JPEG; false means do not wait
	GrabShot func(ctx context.Context) bool                // ask the viewer for one screenshot of her own face
	Observe  func(ctx context.Context) (ScreenView, error) // computer-use glance; tests inject
	Jev      Evaluator
	LLM      Visioner
	LogFn    func(string)
	OnSight  func(Sight)
}

// ScreenView is what computer-use observation reports. No JPEG.
type ScreenView struct {
	Caption   string
	Signature string
	Private   bool
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
