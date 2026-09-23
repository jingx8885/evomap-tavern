package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexJobWritesInItsOwnDir(t *testing.T) {
	root := t.TempDir()
	var gotCwd, gotPrompt string
	fake := func(_ context.Context, cwd, prompt string) (string, error) {
		gotCwd, gotPrompt = cwd, prompt
		return "done\nwrote main.py", os.WriteFile(filepath.Join(cwd, "main.py"), []byte("print(1)\n"), 0o644)
	}
	text, err := runCodexJob(context.Background(), root, "写一个猜数字小游戏", fake, func(string, float64) {})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join(root, "codex"))
	if filepath.Dir(gotCwd) != want {
		t.Fatalf("cwd = %s, want under %s", gotCwd, want)
	}
	if !strings.Contains(gotPrompt, "写一个猜数字小游戏") || !strings.Contains(gotPrompt, "Do not touch files outside") {
		t.Fatalf("prompt = %q", gotPrompt)
	}
	if !strings.Contains(text, "main.py") || !strings.Contains(text, gotCwd) {
		t.Fatalf("summary = %q", text)
	}
}

func TestFailedLineNamesTheRealCause(t *testing.T) {
	got := failedLine(`studio http 503: {"error":{"code":"model_not_found","message":"No available channel for model grok-imagine-image"}}`)
	if !strings.Contains(got, "no channel for that model") || !strings.Contains(got, "Do not blame the page") {
		t.Fatalf("line = %q", got)
	}
	if got := failedLine("boom"); !strings.Contains(got, "returned an error") {
		t.Fatalf("generic line = %q", got)
	}
}

func TestCodexJobFailureIsAnError(t *testing.T) {
	fake := func(context.Context, string, string) (string, error) {
		return "boom", errors.New("codex exec: exit 1")
	}
	if _, err := runCodexJob(context.Background(), t.TempDir(), "x", fake, func(string, float64) {}); err == nil {
		t.Fatal("failed codex run reported success")
	}
	if _, err := runCodexJob(context.Background(), t.TempDir(), "x", nil, func(string, float64) {}); err == nil {
		t.Fatal("missing codex reported success")
	}
}
