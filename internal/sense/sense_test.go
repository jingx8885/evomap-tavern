package sense

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/persona"
)

func TestParseAsk(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		file string
	}{
		{"今天天气不错", AskNone, ""},
		{"你是谁", AskExistence, ""},
		{"你能感觉到自己吗", AskBody, ""},
		{"你能看见自己的代码吗", AskCode, ""},
		{"看看 internal/agent/agent.go", AskFile, "internal/agent/agent.go"},
		{"how do you work", AskCode, ""},
		{"who are you", AskExistence, ""},
		{"你能看见我吗", AskCamera, ""},
		{"屏幕上有什么", AskScreen, ""},
		{"can you see me", AskCamera, ""},
		{"看看你的日志", AskLog, ""},
		{"你能看到自己的日志吗", AskLog, ""},
		{"这个窗口长什么样", AskWindow, ""},
	}
	for _, c := range cases {
		got := ParseAsk(c.in)
		if got.Kind != c.kind || got.File != c.file {
			t.Fatalf("%q: got %+v want kind=%s file=%s", c.in, got, c.kind, c.file)
		}
	}
}

func TestAllowedPaths(t *testing.T) {
	if !allowed("internal/agent/agent.go") {
		t.Fatal("agent.go should be her body")
	}
	if !allowed("personas/haru.yaml") {
		t.Fatal("persona should be her body")
	}
	if allowed(".env") || allowed("internal/secret.key") || allowed("web/live2d/models/Haru/a.png") {
		t.Fatal("secrets and binary models are not her thoughts")
	}
	if allowed("../etc/passwd") || allowed("internal/../.env") {
		t.Fatal("escape should fail")
	}
}

func TestBusEmitAndSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFakeRoot(t, root)
	b, err := Open(Options{Root: root, Cap: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Emit(Event{Kind: KindTurn, Summary: "hi"})
	b.Emit(Event{Kind: KindJudge, Summary: "joy"})
	b.Emit(Event{Kind: KindSteer, Summary: "continue"})
	b.Emit(Event{Kind: KindError, Summary: "drop"})
	if n := len(b.Recent(10)); n != 3 {
		t.Fatalf("ring %d want 3", n)
	}
	b.Set(func(l *Live) {
		l.Voice = "up"
		l.Persona = "小春"
		l.Mode = "continue"
	})
	s := b.Snapshot("小春")
	if s.Who != "小春" || s.Live.Voice != "up" || s.Counts[KindError] != 1 {
		t.Fatalf("snapshot %+v", s)
	}
	if s.Root != root {
		t.Fatalf("root %s", s.Root)
	}
}

func TestReadBodyAndFelt(t *testing.T) {
	root := t.TempDir()
	writeFakeRoot(t, root)
	agentDir := filepath.Join(root, "internal", "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package agent\n\nfunc Run() {}\n"
	if err := os.WriteFile(filepath.Join(agentDir, "agent.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "personas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "personas", "haru.yaml"), []byte("name: 小春\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Read(".env", 0); err == nil {
		t.Fatal("must refuse .env")
	}
	view, err := b.Read("internal/agent/agent.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Excerpt, "package agent") {
		t.Fatalf("excerpt %q", view.Excerpt)
	}

	off := &persona.Persona{Name: "小春"}
	if b.Felt(off, Ask{Kind: AskBody}, "") != "" {
		t.Fatal("sense disabled should not feel")
	}
	p := &persona.Persona{Name: "小春", Voice: "maple", Sense: persona.SenseConfig{Enabled: true}}
	b.Set(func(l *Live) {
		l.Voice = "up"
		l.VoiceName = "maple"
		l.Expression = "F01"
		l.StartedAt = time.Now().Add(-2 * time.Second)
	})
	pulse := b.Felt(p, Ask{}, "")
	if !strings.Contains(pulse, "Body") || !strings.Contains(pulse, "Do not mention") {
		t.Fatalf("pulse %q", pulse)
	}
	self := b.Felt(p, Ask{Kind: AskBody}, "")
	if !strings.Contains(self, "You can feel yourself") {
		t.Fatalf("self %q", self)
	}
	if strings.Contains(self, "package agent") {
		t.Fatal("body ask should not dump source")
	}
	look := b.Felt(p, Ask{Kind: AskFile, File: "internal/agent/agent.go"}, "")
	if !strings.Contains(look, "package agent") {
		t.Fatalf("look missing excerpt: %q", look)
	}
	b.Set(func(l *Live) { l.Camera = "对面有个人"; l.Screen = "在写代码" })
	camAsk := b.Felt(p, Ask{Kind: AskCamera}, "")
	if !strings.Contains(camAsk, "对面有个人") || strings.Contains(camAsk, "在写代码") {
		t.Fatalf("camera ask %q", camAsk)
	}
	scrAsk := b.Felt(p, Ask{Kind: AskScreen}, "")
	if !strings.Contains(scrAsk, "在写代码") || strings.Contains(scrAsk, "对面有个人") {
		t.Fatalf("screen ask %q", scrAsk)
	}
	pulse = b.Felt(p, Ask{}, "none")
	if strings.Contains(pulse, "对面有个人") || strings.Contains(pulse, "在写代码") {
		t.Fatalf("ordinary turn must not include eyes until Jev asks: %q", pulse)
	}
	shown := b.Felt(p, Ask{}, "camera")
	if !strings.Contains(shown, "对面有个人") || strings.Contains(shown, "在写代码") {
		t.Fatalf("camera attend should be camera only: %q", shown)
	}
}

func TestJournalDropsRoutineLines(t *testing.T) {
	root := t.TempDir()
	writeFakeRoot(t, root)
	b, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Note("[livevoice] uplink: frames=100 max_rms=0.2")
	b.Note("[user~] 小春")
	b.Note("[steer] mode=continue")
	b.Note("[eye] camera: 对面有个人")
	b.Note("[judge] emotion=joy intent=chat")
	b.Note("[steer] failed: boom")
	b.Note("[eye] camera vlm: timeout")
	b.Note("[judge] skipped: context canceled")
	got := strings.Join(b.LogTail(10), "\n")
	for _, keep := range []string{"[steer] failed: boom", "[eye] camera vlm: timeout", "[judge] skipped:"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("missing %q in %q", keep, got)
		}
	}
	for _, drop := range []string{"uplink:", "[user~]", "mode=continue", "对面有个人", "emotion=joy"} {
		if strings.Contains(got, drop) {
			t.Fatalf("routine line leaked %q in %q", drop, got)
		}
	}
}

func TestFindRootRejectsOtherModule(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/other\n"), 0o644)
	if _, err := FindRoot(root); err == nil {
		t.Fatal("expected reject")
	}
}

func writeFakeRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+modulePath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
