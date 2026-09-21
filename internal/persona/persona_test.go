package persona

import (
	"os"
	"path/filepath"
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
}
