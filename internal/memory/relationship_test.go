package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if len(rel.OpenLoops) == 0 {
		t.Fatalf("expected a gist, got %+v", rel)
	}
	got := rel.OpenLoops[0].Text
	if !strings.Contains(got, "交稿") || strings.Contains(got, "别忘了") {
		t.Fatalf("memory should be the stake, not the cue or the quote: %q", got)
	}
	for _, ev := range rel.SharedEvents {
		if strings.HasPrefix(ev.Text, "user:") || strings.Contains(ev.Text, "我才没有在担心你") {
			t.Fatalf("raw transcript stored: %q", ev.Text)
		}
	}
	if !strings.Contains(rel.LastEvent, "把事往前推") {
		t.Fatalf("unexpected last event: %+v", rel)
	}
}

func TestMemoryClosesAndFades(t *testing.T) {
	s, err := NewRelationshipStore(filepath.Join(t.TempDir(), "relationship.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveTurn("小春", "我明天还要交稿，别忘了", "", "neutral", "joy", "goal_push", 0.6, 0.5, 0.8, 0.9); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveTurn("小春", "稿子交了", "", "joy", "joy", "celebrate", 0.8, 0.5, 0.7, 0.8); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().OpenLoops) != 0 {
		t.Fatalf("finished loop should leave: %+v", s.Snapshot().OpenLoops)
	}
	cue := s.Recall("稿子")
	joined := strings.Join(append(append([]string{}, cue.SharedEvents...), cue.Summary), " ")
	if !strings.Contains(joined, "高兴") && !strings.Contains(joined, "交") {
		t.Fatalf("the finished moment should still be recallable: %+v", cue)
	}
}

func TestRecallPrefersTheSubject(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindShared, Text: "周末去爬山"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(MemoryOp{Op: "add", Kind: KindOpenLoop, Text: "明天要交稿"}); err != nil {
		t.Fatal(err)
	}
	cue := s.Recall("稿子什么时候交")
	if len(cue.OpenLoops) == 0 || !strings.Contains(cue.OpenLoops[0], "交稿") {
		t.Fatalf("recall should surface the manuscript, got %+v", cue)
	}
	if strings.Contains(strings.Join(cue.OpenLoops, " "), "爬山") {
		t.Fatalf("unrelated line crowded the cue: %+v", cue)
	}
}

func TestClosenessKeepsMovingNearTheTop(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	warm := Observed{User: "今天升职了", Mode: "celebrate", Intent: "share_good", Valence: 0.9, Engagement: 0.9, PersonaFit: 0.9}
	for i := 0; i < 200; i++ {
		if _, err := s.Observe("小春", warm); err != nil {
			t.Fatal(err)
		}
	}
	top := s.Snapshot()
	if top.Bond >= 1 || top.Trust >= 1 || top.Stage != relationshipStageBonded {
		t.Fatalf("closeness pinned or never arrived: %+v", top)
	}
	sour := Observed{User: "你烦不烦", Mode: "de_escalate", Intent: "other", Valence: 0.1, Engagement: 0.5}
	for i := 0; i < 3; i++ {
		if _, err := s.Observe("小春", sour); err != nil {
			t.Fatal(err)
		}
	}
	after := s.Snapshot()
	if after.Warmth >= top.Warmth-0.05 {
		t.Fatalf("warmth did not follow a sour stretch: %.2f -> %.2f", top.Warmth, after.Warmth)
	}
	if after.Stage != relationshipStageBonded {
		t.Fatalf("one sour stretch flipped the stage: %+v", after)
	}
}

func TestTimeApartCools(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	s.rel = normalizeRelationship(Relationship{
		Stage: relationshipStageBonded, Bond: 1, Trust: 1, Warmth: 1, Tension: 0.5,
		UpdatedAt: time.Now().Add(-30 * 24 * time.Hour),
	})
	rel, err := s.Observe("小春", Observed{User: "好久不见", Mode: "continue", Intent: "chat", Valence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Bond >= 0.9 || rel.Bond < bondFloor || rel.Warmth >= 0.8 || rel.Tension >= 0.1 {
		t.Fatalf("a month apart should cool, not erase: %+v", rel)
	}
	fresh, _ := NewRelationshipStore("")
	if got, _ := fresh.Observe("小春", Observed{User: "你好", Mode: "continue", Valence: 0.5}); got.Bond < 0.08 {
		t.Fatalf("a new bond fell below its start: %+v", got)
	}
}

func TestTurnRecallLeavesUnrelatedLinesOut(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []MemoryOp{
		{Op: "add", Kind: KindShared, Text: "周末去爬山"},
		{Op: "add", Kind: KindOpenLoop, Text: "明天要交稿"},
	} {
		if _, err := s.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Observe("小春", Observed{User: "我最近在学吉他", Mode: "continue", Intent: "chat", Valence: 0.6}); err != nil {
		t.Fatal(err)
	}
	cue := s.Recall("我今天好累")
	if len(cue.OpenLoops)+len(cue.SharedEvents) != 0 || cue.Summary != "" {
		t.Fatalf("unrelated memory rode along: %+v", cue)
	}
	if cue := s.Recall("我最近在学吉他"); strings.Contains(cue.Summary, "上次说到") {
		t.Fatalf("the line just said came back as a memory: %q", cue.Summary)
	}
	all := s.Recall("")
	if len(all.OpenLoops) == 0 || len(all.SharedEvents) == 0 || !strings.Contains(all.Summary, "吉他") {
		t.Fatalf("the open view lost its lines: %+v", all)
	}
}

func TestTaskTurnIsNotRemembered(t *testing.T) {
	s, err := NewRelationshipStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Observe("小春", Observed{User: "我在养一只橘猫", Mode: "continue", Intent: "share_good", Valence: 0.7}); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []Observed{
		{User: "帮我画一只猫，别忘了加帽子", Mode: "continue", Intent: "request", Task: true},
		{User: "现在呢，画画了吗", Mode: "continue", Intent: "request"},
	} {
		if _, err := s.Observe("小春", turn); err != nil {
			t.Fatal(err)
		}
	}
	rel := s.Snapshot()
	if len(rel.OpenLoops) != 0 {
		t.Fatalf("a task became an open loop: %+v", rel.OpenLoops)
	}
	if !strings.Contains(rel.LastTopic, "橘猫") {
		t.Fatalf("a request replaced the topic: %q", rel.LastTopic)
	}
}

func TestLegacyRawLinesCompress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relationship.json")
	raw := []byte(`{"stage":"熟悉","bond":0.4,"trust":0.4,"warmth":0.4,"open_loops":["user: 我明天还要交稿，别忘了"],"shared_events":["小春: 我才没有在担心你"]}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewRelationshipStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rel := s.Snapshot()
	if len(rel.OpenLoops) != 1 || strings.HasPrefix(rel.OpenLoops[0].Text, "user:") || !strings.Contains(rel.OpenLoops[0].Text, "交稿") {
		t.Fatalf("legacy quote should compress: %+v", rel.OpenLoops)
	}
	for _, ev := range rel.SharedEvents {
		if strings.Contains(ev.Text, "我才没有在担心你") {
			t.Fatalf("her own line should not stay as a quote: %q", ev.Text)
		}
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
