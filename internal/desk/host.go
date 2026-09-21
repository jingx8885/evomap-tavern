package desk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/config"
)

// DefaultHost talks to the local OS/CLIs.
type DefaultHost struct {
	CodexModel   string
	APIKey       string
	CodexTimeout time.Duration
}

func (h DefaultHost) Snapshot(cwd string) (Snapshot, error) {
	snap := Snapshot{Cwd: resolveCwd(cwd), Tools: DetectTools()}
	wins, fg, err := listWindows()
	if err != nil {
		return snap, err
	}
	snap.Windows = wins
	snap.ForegroundHWND = fg
	for _, w := range wins {
		if w.HWND == fg && w.Title != "" {
			snap.ForegroundTitle = w.Title
			break
		}
	}
	return snap, nil
}

func (h DefaultHost) Activate(win Window) error {
	if win.HWND == 0 && strings.TrimSpace(win.Title) == "" {
		return fmt.Errorf("no window to activate")
	}
	return activateWindow(win)
}

func (h DefaultHost) Hotkey(name string) error {
	return sendHotkey(name)
}

func (h DefaultHost) Paste(text string) error {
	return pasteText(text)
}

func (h DefaultHost) Launch(app, cwd string) error {
	cwd = resolveCwd(cwd)
	switch strings.ToLower(strings.TrimSpace(app)) {
	case "cursor":
		return h.OpenCursor(cwd)
	case "explorer":
		return startDetached("explorer.exe", cwd)
	case "notepad":
		return startDetached("notepad.exe")
	case "powershell":
		return startDetached("powershell.exe")
	case "terminal":
		if err := startDetached("wt.exe", "-d", cwd); err == nil {
			return nil
		}
		return startDetached("powershell.exe")
	case "chrome":
		if err := startDetached("chrome.exe"); err == nil {
			return nil
		}
		return startDetached("msedge.exe")
	default:
		return fmt.Errorf("app %q is not allowlisted", app)
	}
}

func (h DefaultHost) OpenCursor(cwd string) error {
	cwd = resolveCwd(cwd)
	bin := DetectTools().Cursor
	if bin == "" {
		return fmt.Errorf("cursor CLI not found on PATH")
	}
	return startDetached(bin, "-r", cwd)
}

func (h DefaultHost) RunCodex(ctx context.Context, cwd, prompt string) (string, error) {
	bin := DetectTools().Codex
	if bin == "" {
		return "", fmt.Errorf("codex CLI not found on PATH")
	}
	timeout := h.CodexTimeout
	if timeout <= 0 {
		timeout = 8 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, CodexExecArgs(resolveCwd(cwd), h.model(), prompt)...)
	cmd.Env = os.Environ()
	if k := strings.TrimSpace(h.APIKey); k != "" {
		cmd.Env = append(cmd.Env, "OPENAI_API_KEY="+k)
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return clip(text, 4000), fmt.Errorf("codex exec: %w", err)
	}
	return clip(text, 4000), nil
}

func (h DefaultHost) model() string {
	if m := strings.TrimSpace(h.CodexModel); m != "" {
		return m
	}
	return config.DefaultPlannerModel
}

// CodexExecArgs is the non-interactive Codex invocation: same new-api
// provider as this repo, model overridden to gpt-5.6-luna by default.
func CodexExecArgs(cwd, model, prompt string) []string {
	if strings.TrimSpace(model) == "" {
		model = config.DefaultPlannerModel
	}
	return []string{
		"exec",
		"-C", cwd,
		"-s", "workspace-write",
		"--skip-git-repo-check",
		"--approve-for-me",
		"-m", model,
		prompt,
	}
}

func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
