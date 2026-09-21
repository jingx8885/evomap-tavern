package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	os.WriteFile(f, []byte("name: x\nstyle: warm\ngoals: [g1]\n"), 0o644)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Voice != "cove" || p.Judge.SafetyThresh != 0.6 || p.Planner.IntervalTurn != 5 {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if p.BaseInstructions() == "" {
		t.Fatal("instructions empty")
	}
	if !strings.Contains(p.BaseInstructions(), "Wait for the user") {
		t.Fatal("must wait for the user instead of self-introducing")
	}
}

func TestCatchphrasesInInstructions(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	os.WriteFile(f, []byte("name: 小春\nstyle: 傲娇\ncatchphrases: [当然了, 笨蛋]\n"), 0o644)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	got := p.BaseInstructions()
	for _, want := range []string{"当然了", "笨蛋", "at most one"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestLoadShippedPersonas(t *testing.T) {
	cases := []struct {
		path string
		name string
		hook string
	}{
		{"../../personas/haru.yaml", "小春", "当然了"},
		{"../../personas/asuka.yaml", "明日香", "あんたバカ？"},
	}
	for _, tc := range cases {
		p, err := Load(tc.path)
		if err != nil {
			t.Fatalf("load %s: %v", tc.path, err)
		}
		if p.Name != tc.name {
			t.Fatalf("%s name=%q want %q", tc.path, p.Name, tc.name)
		}
		if len(p.Catchphrases) == 0 {
			t.Fatalf("%s missing catchphrases", tc.path)
		}
		if !strings.Contains(p.BaseInstructions(), tc.hook) {
			t.Fatalf("%s instructions missing %q", tc.path, tc.hook)
		}
	}
	haru, err := Load("../../personas/haru.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !haru.Sense.Enabled {
		t.Fatal("haru should be able to feel herself")
	}
	if haru.Reaction("comfort") == "" || len(haru.Examples) == 0 {
		t.Fatal("haru needs reactions and examples so comfort stays tsundere")
	}
	if !strings.Contains(haru.BaseInstructions(), "我才没有在担心你") {
		t.Fatalf("examples should land in base instructions: %q", haru.BaseInstructions())
	}
	if !strings.Contains(haru.BaseInstructions(), "未完成事项管理员") ||
		!strings.Contains(haru.BaseInstructions(), "把对方的事真的放在心上") {
		t.Fatalf("haru needs her character bible in instructions: %q", haru.BaseInstructions())
	}
	asuka, err := Load("../../personas/asuka.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asuka.Reaction("comfort"), "硬塞") {
		t.Fatalf("asuka comfort should stay fierce: %q", asuka.Reaction("comfort"))
	}
}

func TestSenseEnabledInInstructions(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	os.WriteFile(f, []byte("name: 小春\nsense:\n  enabled: true\n"), 0o644)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Sense.Enabled {
		t.Fatal("sense not enabled")
	}
	got := p.BaseInstructions()
	if !strings.Contains(got, "body you can feel") {
		t.Fatalf("missing proprioception: %q", got)
	}
	plain := filepath.Join(dir, "off.yaml")
	os.WriteFile(plain, []byte("name: x\n"), 0o644)
	q, err := Load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.BaseInstructions(), "body you can feel") {
		t.Fatal("disabled sense should stay quiet")
	}
}
