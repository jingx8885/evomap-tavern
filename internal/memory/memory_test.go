package memory

import "testing"

func TestAddAndTrim(t *testing.T) {
	m := New(3)
	for i := 0; i < 5; i++ {
		m.Add(Turn{Speaker: "user", Text: string(rune('a' + i))})
	}
	if got := len(m.Turns()); got != 3 {
		t.Fatalf("want 3 turns, got %d", got)
	}
	if m.Turns()[2].Text != "e" {
		t.Fatalf("wrong tail: %+v", m.Turns())
	}
}

func TestUpdateAffectEMA(t *testing.T) {
	m := New(8)
	m.UpdateAffect(0.8, 0.5, "joy", false)
	a := m.Affect()
	if a.Valence != 0.8 || a.Emotion != "joy" {
		t.Fatalf("first update should set state: %+v", a)
	}
	m.UpdateAffect(0.2, 0.1, "sadness", false)
	a = m.Affect()
	want := 0.4*0.2 + 0.6*0.8
	if abs(a.Valence-want) > 1e-6 {
		t.Fatalf("EMA valence want %.3f got %.3f", want, a.Valence)
	}
	if a.Emotion != "sadness" {
		t.Fatalf("emotion should update: %s", a.Emotion)
	}
}

func TestSafetySticks(t *testing.T) {
	m := New(8)
	m.UpdateAffect(0.1, 0.9, "fear", true)
	m.UpdateAffect(0.9, 0.1, "joy", false)
	if !m.Affect().SafetyHit {
		t.Fatal("safety flag must latch")
	}
}

func TestLatestAssistant(t *testing.T) {
	m := New(8)
	m.Add(Turn{Speaker: "user", Text: "hi"})
	m.Add(Turn{Speaker: "assistant", Text: "hello"})
	m.Add(Turn{Speaker: "user", Text: "again"})
	if got := m.LatestAssistantText(); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := m.LatestUserText(); got != "again" {
		t.Fatalf("got %q", got)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
