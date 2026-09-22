package react

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/sense"
)

func TestWantsChange(t *testing.T) {
	if !WantsChange("改一下你自己") || !WantsChange("change your own source") {
		t.Fatal("expected a change ask")
	}
	if WantsChange("你内部是怎么活的") || WantsChange("你现在什么感觉") {
		t.Fatal("noticing is not a change ask")
	}
}

func TestReadThenStop(t *testing.T) {
	bus := openRepo(t, map[string]string{
		"personas/haru.yaml": "name: 小春\n",
	})
	jevFake := &scriptJev{answers: []map[string]jev.Answer{
		{"step": {Choice: StepRead}, "path": {Choice: "personas/haru.yaml"}, "safe_to_act": noul(0.9)},
		{"step": {Choice: StepDone}, "path": {Choice: "none"}, "safe_to_act": noul(0.9)},
	}}
	rep, err := Run(context.Background(), Options{
		Kind: "look", Goal: "看看 personas/haru.yaml", Bus: bus, Jev: jevFake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed {
		t.Fatal("a read must not count as a change")
	}
	if !strings.Contains(rep.Note, "name: 小春") {
		t.Fatalf("note %q", rep.Note)
	}
	if !strings.Contains(bus.Live().SelfNote, "personas/haru.yaml") {
		t.Fatalf("self note %q", bus.Live().SelfNote)
	}
}

func TestChangeRefusesLatchAndStrayEdits(t *testing.T) {
	const secret = "这句话不该进编辑器"
	bus := openRepo(t, map[string]string{
		"personas/haru.yaml":      "name: 小春\n",
		"internal/judge/judge.go": "package judge\nfunc Latch() {}\n",
	})
	coder := &fakeCoder{}
	jevFake := &scriptJev{answers: []map[string]jev.Answer{
		{"step": {Choice: StepChange}, "path": {Choice: "internal/judge/judge.go"}, "safe_to_act": noul(0.9)},
	}}
	rep, err := Run(context.Background(), Options{
		Kind: "codex", Goal: "改一下 internal/judge/judge.go", CanChange: true,
		Bus: bus, Jev: jevFake, LLM: fakeLLM{}, Coder: coder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coder.calls != 0 {
		t.Fatal("a safety latch must not be edited")
	}
	if rep.Status != StatusBlocked {
		t.Fatalf("status %s", rep.Status)
	}

	coder.fn = func(cwd, prompt string) error {
		if strings.Contains(prompt, secret) {
			t.Fatalf("coder prompt leaked the raw goal: %s", prompt)
		}
		if !strings.Contains(prompt, "Edit only this file: personas/haru.yaml") || !strings.Contains(prompt, "soften the greeting") {
			t.Fatalf("prompt %s", prompt)
		}
		haru := filepath.Join(cwd, "personas", "haru.yaml")
		if err := os.WriteFile(haru, []byte("name: 小春\nstyle: soft\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(cwd, "internal", "judge", "judge.go"), []byte("package judge\nfunc HACK() {}\n"), 0o644)
	}
	jevFake = &scriptJev{answers: []map[string]jev.Answer{
		{"step": {Choice: StepChange}, "path": {Choice: "personas/haru.yaml"}, "safe_to_act": noul(0.9)},
		{"step": {Choice: StepDone}, "path": {Choice: "none"}, "safe_to_act": noul(0.9)},
	}}
	rep, err = Run(context.Background(), Options{
		Kind: "look", Goal: "改一下 personas/haru.yaml。" + secret, CanChange: true,
		Bus: bus, Jev: jevFake, LLM: fakeLLM{out: `{"why":"softer hello","change":"soften the greeting"}`}, Coder: coder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Changed {
		t.Fatalf("expected a kept edit, note %s", rep.Note)
	}
	if !strings.Contains(rep.Note, "previous build") {
		t.Fatalf("note should say the process is unchanged: %s", rep.Note)
	}
	haru, err := os.ReadFile(filepath.Join(bus.Root(), "personas", "haru.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(haru), "style: soft") {
		t.Fatalf("haru %s", haru)
	}
	latch, err := os.ReadFile(filepath.Join(bus.Root(), "internal", "judge", "judge.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(latch), "HACK") || !strings.Contains(string(latch), "Latch") {
		t.Fatalf("latch was not restored: %s", latch)
	}
}

func TestLowSafeDoesNotEdit(t *testing.T) {
	bus := openRepo(t, map[string]string{"personas/haru.yaml": "name: 小春\n"})
	coder := &fakeCoder{fn: func(string, string) error { t.Fatal("coder ran"); return nil }}
	rep, err := Run(context.Background(), Options{
		Kind: "codex", Goal: "改一下 personas/haru.yaml", CanChange: true,
		Bus: bus, Coder: coder, LLM: fakeLLM{out: `{"why":"x","change":"y"}`},
		Jev: &scriptJev{answers: []map[string]jev.Answer{
			{"step": {Choice: StepChange}, "path": {Choice: "personas/haru.yaml"}, "safe_to_act": noul(0.1)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed || rep.Status != StatusBlocked {
		t.Fatalf("status=%s changed=%v", rep.Status, rep.Changed)
	}
}

func TestRememberOneLine(t *testing.T) {
	bus := openRepo(t, map[string]string{"personas/haru.yaml": "name: 小春\n"})
	store, err := memory.NewRelationshipStore(filepath.Join(t.TempDir(), "rel.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Options{
		Kind: "reflect", Goal: "记住你想把招呼说软一点", Bus: bus, Memory: store,
		LLM: fakeLLM{out: `{"text":"她想把招呼说软一点"}`},
		Jev: &scriptJev{answers: []map[string]jev.Answer{
			{"step": {Choice: StepRemember}, "path": {Choice: "none"}, "safe_to_act": noul(0.8)},
			{"step": {Choice: StepDone}, "path": {Choice: "none"}, "safe_to_act": noul(0.8)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed {
		t.Fatal("memory is not a source edit")
	}
	if !strings.Contains(rep.Note, "她想把招呼说软一点") {
		t.Fatalf("note %s", rep.Note)
	}
	view := store.View()
	if len(view.Items) != 1 || view.Items[0].Text != "她想把招呼说软一点" {
		t.Fatalf("memory %+v", view.Items)
	}
}

type scriptJev struct {
	answers []map[string]jev.Answer
	i       int
}

func (f *scriptJev) Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.EvalResult, error) {
	if f.i >= len(f.answers) {
		return &jev.EvalResult{Answers: map[string]jev.Answer{"step": {Choice: StepDone}, "path": {Choice: "none"}}}, nil
	}
	ans := f.answers[f.i]
	f.i++
	return &jev.EvalResult{Answers: ans}, nil
}

type fakeLLM struct{ out string }

func (f fakeLLM) ChatComplete(ctx context.Context, system, user string) (string, error) {
	return f.out, nil
}

type fakeCoder struct {
	calls int
	fn    func(cwd, prompt string) error
}

func (f *fakeCoder) Edit(ctx context.Context, cwd, prompt string) (string, error) {
	f.calls++
	if f.fn != nil {
		return "", f.fn(cwd, prompt)
	}
	return "", nil
}

func noul(v float64) jev.Answer {
	return jev.Answer{Noul: &v}
}

func openRepo(t *testing.T, files map[string]string) *sense.Bus {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	files["go.mod"] = "module github.com/jingx8885/lov-evo\n"
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=react", "GIT_AUTHOR_EMAIL=react@example.com", "GIT_COMMITTER_NAME=react", "GIT_COMMITTER_EMAIL=react@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init")
	git("add", "-A")
	git("commit", "-m", "init")
	bus, err := sense.Open(sense.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}
