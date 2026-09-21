package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelationshipPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationship-haru.json")
	s, err := NewRelationshipStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveTurn("小春", "我明天还要交稿，别忘了", "我才没有在担心你", "neutral", "joy", "goal_push", 0.6, 0.5, 0.8, 0.9); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("relationship file not written: %v", err)
	}
	re, err := NewRelationshipStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rel := re.Snapshot()
	if rel.Stage == "" || rel.Bond <= 0.08 {
		t.Fatalf("relationship should have grown: %+v", rel)
	}
	if len(rel.OpenLoops) == 0 && len(rel.SharedEvents) == 0 {
		t.Fatalf("expected remembered loops/events: %+v", rel)
	}
	if !strings.Contains(rel.LastEvent, "把事往前推") {
		t.Fatalf("unexpected last event: %+v", rel)
	}
}
