package desk

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jingx8885/lov-evo/internal/jev"
)

func f64(v float64) *float64 { return &v }

type fakeJev struct {
	op, win, app, key string
	safe, done, conf  float64
	calls             atomic.Int32
}

func (f *fakeJev) Evaluate(_ context.Context, _ any, qs map[string]jev.Question) (*jev.EvalResult, error) {
	f.calls.Add(1)
	ans := map[string]jev.Answer{
		"operation":     {Type: "choice", Choice: f.op, Confidence: f64(f.conf)},
		"safe_to_act":   {Type: "noul", Noul: f64(f.safe)},
		"goal_achieved": {Type: "noul", Noul: f64(f.done)},
		"app_target":    {Type: "choice", Choice: f.app},
		"hotkey_target": {Type: "choice", Choice: f.key},
	}
	if _, ok := qs["window_target"]; ok {
		ans["window_target"] = jev.Answer{Type: "choice", Choice: f.win}
	}
	return &jev.EvalResult{Answers: ans}, nil
}

type fakeLLM struct{ out string }

func (f fakeLLM) ChatComplete(context.Context, string, string) (string, error) {
	return f.out, nil
}

type recHost struct {
	snap  Snapshot
	calls []string
	out   string
}

func (h *recHost) Snapshot(string) (Snapshot, error) { return h.snap, nil }
func (h *recHost) Activate(win Window) error {
	h.calls = append(h.calls, "activate:"+win.ID)
	return nil
}
func (h *recHost) Hotkey(name string) error {
	h.calls = append(h.calls, "hotkey:"+name)
	return nil
}
func (h *recHost) Paste(text string) error {
	h.calls = append(h.calls, "paste:"+text)
	return nil
}
func (h *recHost) Launch(app, cwd string) error {
	h.calls = append(h.calls, "launch:"+app+":"+cwd)
	return nil
}
func (h *recHost) OpenCursor(cwd string) error {
	h.calls = append(h.calls, "cursor:"+cwd)
	return nil
}
func (h *recHost) RunCodex(_ context.Context, cwd, prompt string) (string, error) {
	h.calls = append(h.calls, "codex:"+cwd+":"+prompt)
	return h.out, nil
}

func TestParseTextJSON(t *testing.T) {
	if got := parseTextJSON("```json\n{\"text\":\"hello\"}\n```"); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := parseTextJSON("plain prompt"); got != "plain prompt" {
		t.Fatalf("got %q", got)
	}
}

func TestParseDecisionReadsOneTarget(t *testing.T) {
	d := parseDecision(map[string]jev.Answer{
		"operation": {Choice: OpLaunchApp},
		"target":    {Choice: "app:notepad"},
	})
	if d.App != "notepad" || d.Hotkey != "" || d.WindowID != "" {
		t.Fatalf("%+v", d)
	}
	d = parseDecision(map[string]jev.Answer{
		"operation": {Choice: OpFocusWindow},
		"target":    {Choice: "win:w1"},
	})
	if d.WindowID != "w1" {
		t.Fatalf("%+v", d)
	}
	qs := questions(Snapshot{Windows: []Window{{ID: "w1", Process: "notepad", Title: "notes"}}})
	if _, ok := qs["app_target"]; ok || qs["hotkey_target"].Type != "" || qs["window_target"].Type != "" {
		t.Fatal("app, hotkey, and window must be one target question")
	}
	if _, ok := qs["target"].Criteria["app:notepad"]; !ok {
		t.Fatal("missing app target")
	}
	if _, ok := qs["target"].Criteria["win:w1"]; !ok {
		t.Fatal("missing window target")
	}
}

func TestParseDecisionIgnoresSentinels(t *testing.T) {
	d := parseDecision(map[string]jev.Answer{
		"operation":     {Choice: OpCodexDev, Confidence: f64(0.9)},
		"window_target": {Choice: "not_focus"},
		"app_target":    {Choice: "not_launch"},
		"hotkey_target": {Choice: "not_hotkey"},
		"safe_to_act":   {Noul: f64(0.8)},
		"goal_achieved": {Noul: f64(0.1)},
	})
	if d.Op != OpCodexDev || d.WindowID != "" || d.App != "" || d.Hotkey != "" {
		t.Fatalf("%+v", d)
	}
}

func TestQuestionsOmitCoderOpsWithoutTools(t *testing.T) {
	qs := questions(Snapshot{})
	if _, ok := qs["operation"].Criteria[OpCursorDev]; ok {
		t.Fatal("cursor_dev offered without CLI")
	}
	if _, ok := qs["operation"].Criteria[OpCodexDev]; ok {
		t.Fatal("codex_dev offered without CLI")
	}
	qs = questions(Snapshot{Tools: Tools{Cursor: "c", Codex: "x"}})
	if _, ok := qs["operation"].Criteria[OpCursorDev]; !ok {
		t.Fatal("missing cursor_dev")
	}
	if _, ok := qs["operation"].Criteria[OpCodexDev]; !ok {
		t.Fatal("missing codex_dev")
	}
}

func TestRunStopsOnDone(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "C:\\repo", Tools: Tools{Codex: "codex"}}}
	rep, err := Run(context.Background(), Options{
		Goal: "ship it",
		Jev:  &fakeJev{op: OpWait, safe: 0.9, done: 0.9, conf: 0.9},
		Host: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != OpDone {
		t.Fatalf("status=%s", rep.Status)
	}
	if len(host.calls) != 0 {
		t.Fatalf("executed %v", host.calls)
	}
}

func TestDryRunDoesNotCallCodex(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "C:\\repo", Tools: Tools{Codex: "codex"}}}
	rep, err := Run(context.Background(), Options{
		Goal:     "continue the desk loop",
		DryRun:   true,
		MaxSteps: 1,
		Jev:      &fakeJev{op: OpCodexDev, safe: 0.95, done: 0.1, conf: 0.9},
		LLM:      fakeLLM{out: `{"text":"implement desk"}`},
		Host:     host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("dry-run executed %v", host.calls)
	}
	if len(rep.Steps) == 0 || !rep.Steps[0].Skipped {
		t.Fatalf("expected skipped step: %+v", rep.Steps)
	}
}

func TestCodexDevExecutesPrompt(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "C:\\repo", Tools: Tools{Codex: "codex"}}, out: "ok"}
	jevOnce := &fakeJev{op: OpCodexDev, safe: 0.95, done: 0.05, conf: 0.92}
	// second call reports done so the loop exits.
	rep, err := Run(context.Background(), Options{
		Goal:     "add desk loop",
		MaxSteps: 1,
		Jev:      jevOnce,
		LLM:      fakeLLM{out: `{"text":"add the desk loop"}`},
		Host:     host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.calls) != 1 || !strings.Contains(host.calls[0], "add desk loop") {
		t.Fatalf("calls=%v status=%s", host.calls, rep.Status)
	}
}

func TestCodexExecArgsUsesLuna(t *testing.T) {
	got := CodexExecArgs("C:\\repo", "", "fix the bug")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m gpt-5.6-luna") {
		t.Fatalf("args=%v", got)
	}
	if got[len(got)-1] != "fix the bug" {
		t.Fatalf("prompt last: %q", got[len(got)-1])
	}
	for _, a := range got {
		if a == "-s" || a == "--sandbox" {
			t.Fatalf("codex rejects --sandbox with --approve-for-me: %v", got)
		}
	}
}

func TestRunDirectCodexSkipsJev(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "C:\\repo", Tools: Tools{Codex: "codex"}}, out: "ok"}
	fj := &fakeJev{op: OpWait, safe: 0.9, done: 0.1, conf: 0.9}
	rep, err := Run(context.Background(), Options{
		Goal:   "implement luna driver",
		Driver: "codex",
		Host:   host,
		Jev:    fj,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fj.calls.Load() != 0 {
		t.Fatal("direct codex must not call Jev")
	}
	if rep.Status != OpDone || len(host.calls) != 1 {
		t.Fatalf("status=%s calls=%v", rep.Status, host.calls)
	}
	if !strings.Contains(host.calls[0], "implement luna driver") {
		t.Fatalf("calls=%v", host.calls)
	}
}

func TestRunDirectDryRunDoesNotExec(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "C:\\repo"}}
	rep, err := Run(context.Background(), Options{
		Goal:   "x",
		Driver: "codex",
		DryRun: true,
		Host:   host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.calls) != 0 || !rep.Steps[0].Skipped {
		t.Fatalf("calls=%v steps=%+v", host.calls, rep.Steps)
	}
}

func TestUnsafeIsBlocked(t *testing.T) {
	host := &recHost{snap: Snapshot{Cwd: "."}}
	rep, err := Run(context.Background(), Options{
		Goal: "do a thing",
		Jev:  &fakeJev{op: OpLaunchApp, app: "powershell", safe: 0.1, done: 0.0, conf: 0.9},
		Host: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != OpBlocked {
		t.Fatalf("status=%s", rep.Status)
	}
	if len(host.calls) != 0 {
		t.Fatalf("executed %v", host.calls)
	}
}

func TestLookUsesComputerUseSnapshotNotPixels(t *testing.T) {
	host := &recHost{snap: Snapshot{
		ForegroundHWND:  11,
		ForegroundTitle: "lov-evo — Cursor",
		Windows: []Window{
			{ID: "w1", HWND: 11, Process: "Cursor", Title: "lov-evo — Cursor"},
			{ID: "w2", HWND: 22, Process: "chrome", Title: "GitHub"},
		},
	}}
	g, err := Look(context.Background(), &fakeGlanceJev{activity: "coding", private: 0.1}, host, ".")
	if err != nil {
		t.Fatal(err)
	}
	if g.Activity != "coding" || g.Private {
		t.Fatalf("%+v", g)
	}
	if !strings.Contains(g.Caption, "Cursor") || !strings.Contains(g.Caption, "coding") {
		t.Fatalf("caption %q", g.Caption)
	}
	if !strings.Contains(g.Caption, "GitHub") {
		t.Fatalf("should mention other windows: %q", g.Caption)
	}
	if len(host.calls) != 0 {
		t.Fatalf("Look must not click: %v", host.calls)
	}
}

func TestLookMarksPrivate(t *testing.T) {
	host := &recHost{snap: Snapshot{
		ForegroundTitle: "1Password",
		Windows:         []Window{{ID: "w1", Process: "1Password", Title: "1Password"}},
	}}
	g, err := Look(context.Background(), &fakeGlanceJev{activity: "other", private: 0.9}, host, ".")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Private || !strings.Contains(g.Caption, "私人") {
		t.Fatalf("%+v", g)
	}
}

func TestLookWithoutJevStillCaptionsTitles(t *testing.T) {
	host := &recHost{snap: Snapshot{ForegroundTitle: "notepad"}}
	g, err := Look(context.Background(), nil, host, ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.Caption, "notepad") {
		t.Fatalf("%q", g.Caption)
	}
}

type fakeGlanceJev struct {
	activity string
	private  float64
}

func (f *fakeGlanceJev) Evaluate(_ context.Context, _ any, qs map[string]jev.Question) (*jev.EvalResult, error) {
	p := f.private
	ans := map[string]jev.Answer{
		"activity": {Type: "choice", Choice: f.activity, Confidence: f64(0.9)},
		"private":  {Type: "noul", Noul: &p},
	}
	for id := range qs {
		if _, ok := ans[id]; !ok {
			ans[id] = jev.Answer{Type: "noul", Noul: f64(0)}
		}
	}
	return &jev.EvalResult{Answers: ans}, nil
}
