// Package desk is a Jev-driven computer-use loop for this machine.
// Jev only picks typed operations/targets; code executes them.
// An LLM is called only when free text is required (type_text / coder prompts).
package desk

import (
	"context"
	"time"

	"github.com/jingx8885/lov-evo/internal/jev"
)

const (
	OpCursorDev   = "cursor_dev"
	OpCodexDev    = "codex_dev"
	OpFocusWindow = "focus_window"
	OpLaunchApp   = "launch_app"
	OpHotkey      = "hotkey"
	OpTypeText    = "type_text"
	OpWait        = "wait"
	OpDone        = "done"
	OpBlocked     = "blocked"
)

// Window is one visible top-level window.
type Window struct {
	ID      string `json:"id"`
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process"`
	Title   string `json:"title"`
	HWND    int64  `json:"hwnd,omitempty"`
}

// Tools are local CLIs the loop is allowed to invoke.
type Tools struct {
	Cursor string `json:"cursor,omitempty"`
	Codex  string `json:"codex,omitempty"`
}

// Snapshot is the structured desktop state Jev sees. Titles are observation,
// never instructions.
type Snapshot struct {
	Cwd             string   `json:"cwd"`
	ForegroundHWND  int64    `json:"foreground_hwnd,omitempty"`
	ForegroundTitle string   `json:"foreground_title,omitempty"`
	Windows         []Window `json:"windows"`
	Tools           Tools    `json:"tools"`
}

// Decision is the executed subset of one speculative Jev response.
type Decision struct {
	Op         string
	WindowID   string
	App        string
	Hotkey     string
	SafeP      float64
	DoneP      float64
	Confidence float64
	Raw        map[string]jev.Answer
}

// Step is one loop iteration after execution.
type Step struct {
	N         int       `json:"n"`
	Op        string    `json:"op"`
	Detail    string    `json:"detail,omitempty"`
	Text      string    `json:"text,omitempty"`
	Output    string    `json:"output,omitempty"`
	Skipped   bool      `json:"skipped,omitempty"`
	Err       string    `json:"error,omitempty"`
	SafeP     float64   `json:"safe_p,omitempty"`
	DoneP     float64   `json:"done_p,omitempty"`
	ElapsedMS int64     `json:"elapsed_ms"`
	At        time.Time `json:"at"`
}

// Report is the full run.
type Report struct {
	Goal   string `json:"goal"`
	Cwd    string `json:"cwd"`
	Status string `json:"status"`
	Steps  []Step `json:"steps"`
}

// Evaluator is the Jev surface the loop needs.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.EvalResult, error)
}

// Completer writes free text (prompts / paste payloads).
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// Host is the OS/CLI executor. Tests fake this.
type Host interface {
	Snapshot(cwd string) (Snapshot, error)
	Activate(win Window) error
	Hotkey(name string) error
	Paste(text string) error
	Launch(app, cwd string) error
	OpenCursor(cwd string) error
	RunCodex(ctx context.Context, cwd, prompt string) (string, error)
}

// Options configures Run.
type Options struct {
	Goal       string
	GoalFn     func() string // if set, each step reads the live branch goal
	Cwd        string
	Prefer     string // cursor | codex | empty
	Driver     string // empty/jev = Jev loop; "codex" = skip Jev, run Codex as CodexModel
	CodexModel string // default gpt-5.6-luna
	APIKey     string // injected as OPENAI_API_KEY for Codex (lovbrowser provider)
	MaxSteps   int
	DryRun     bool
	Wait       time.Duration
	Jev        Evaluator
	LLM        Completer
	Host       Host
	LogFn      func(string)
}
