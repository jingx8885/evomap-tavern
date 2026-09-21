package desk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DetectTools finds allowlisted CLIs on PATH.
func DetectTools() Tools {
	return Tools{
		Cursor: lookPath("cursor", "cursor.cmd"),
		Codex:  lookPath("codex", "codex.cmd"),
	}
}

func lookPath(names ...string) string {
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return ""
}

func resolveCwd(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		if w, err := os.Getwd(); err == nil {
			return w
		}
		return "."
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		return abs
	}
	return cwd
}

func (t Tools) hasCursor() bool { return t.Cursor != "" }
func (t Tools) hasCodex() bool  { return t.Codex != "" }

func (s Snapshot) windowByID(id string) (Window, bool) {
	for _, w := range s.Windows {
		if w.ID == id {
			return w, true
		}
	}
	return Window{}, false
}

func (s Snapshot) firstProcess(name string) (Window, bool) {
	name = strings.ToLower(name)
	for _, w := range s.Windows {
		if strings.Contains(strings.ToLower(w.Process), name) ||
			strings.Contains(strings.ToLower(w.Title), name) {
			return w, true
		}
	}
	return Window{}, false
}
