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

func TestMemoryEditPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationship-haru.json")
	s, err := NewRelationshipStore(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.Apply(MemoryOp{Op: "add", Kind: KindPromise, Text: "明天一起交稿"})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].Text != "明天一起交稿" || !view.Available {
		t.Fatalf("add view: %+v", view)
	}
	if !strings.Contains(view.Summary, "答应过") {
		t.Fatalf("promise should reach the scene summary: %s", view.Summary)
	}
	id := view.Items[0].ID
	view, err = s.Apply(MemoryOp{Op: "update", ID: id, Text: "后天一起交稿"})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].Text != "后天一起交稿" || view.Items[0].ID == id {
		t.Fatalf("update view: %+v", view)
	}

	re, err := NewRelationshipStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := re.View()
	if len(got.Items) != 1 || got.Items[0].Text != "后天一起交稿" {
		t.Fatalf("reload: %+v", got)
	}
	if _, err := re.Apply(MemoryOp{Op: "delete", ID: got.Items[0].ID}); err != nil {
		t.Fatal(err)
	}
	if len(re.Snapshot().Promises) != 0 || len(re.View().Items) != 0 {
		t.Fatalf("delete left %+v", re.Snapshot())
	}

	if _, err := s.Apply(MemoryOp{Op: "add", Kind: "nope", Text: "x"}); err != ErrMemoryKind {
		t.Fatalf("kind err %v", err)
	}
	if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindJoke, Text: "   "}); err != ErrMemoryEmpty {
		t.Fatalf("empty err %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindJoke, Text: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindJoke, Text: "overflow"}); err != ErrMemoryFull {
		t.Fatalf("full err %v", err)
	}
}
