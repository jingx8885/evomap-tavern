package memory

import (
	"context"
	"strings"
	"testing"
)

type scriptLLM struct {
	reply string
	err   error
	got   string
}

func (s *scriptLLM) ChatComplete(_ context.Context, _, user string) (string, error) {
	s.got = user
	return s.reply, s.err
}

func TestFoldRewritesInsteadOfQuoting(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveTurn("小春", "我明天还要交稿，别忘了", "", "neutral", "", "goal_push", 0.5, 0.4, 0.7, 0.8); err != nil {
		t.Fatal(err)
	}
	llm := &scriptLLM{reply: `{"notes":[
		{"op":"add","kind":"open_loop","text":"我明天还要交稿，别忘了","match":""},
		{"op":"revise","kind":"open_loop","text":"明天交稿，她在催","match":"明天还要交稿"}
	]}`}
	notes, err := s.Fold(context.Background(), llm, FoldIn{User: "我明天还要交稿，别忘了", Mode: "goal_push"})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "明天交稿，她在催") {
		t.Fatalf("quote should be dropped and the gist kept: %v", notes)
	}
	rel := s.Snapshot()
	if len(rel.OpenLoops) != 1 || rel.OpenLoops[0].Text != "明天交稿，她在催" {
		t.Fatalf("ledger: %+v", rel.OpenLoops)
	}
	if !strings.Contains(llm.got, "明天") {
		t.Fatalf("fold should see what she already keeps:\n%s", llm.got)
	}
}

func TestFoldClosesALoop(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindOpenLoop, Text: "声音卡顿还没修"}); err != nil {
		t.Fatal(err)
	}
	llm := &scriptLLM{reply: `{"notes":[{"op":"close","kind":"open_loop","text":"","match":"声音卡顿"}]}`}
	notes, err := s.Fold(context.Background(), llm, FoldIn{User: "卡顿修好了", Mode: "celebrate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || len(s.Snapshot().OpenLoops) != 0 {
		t.Fatalf("close notes=%v loops=%+v", notes, s.Snapshot().OpenLoops)
	}
}
