package steering

import (
	"strings"
	"testing"

	"github.com/jingx8885/evomap-tavern/internal/judge"
	"github.com/jingx8885/evomap-tavern/internal/memory"
	"github.com/jingx8885/evomap-tavern/internal/persona"
)

func TestBuild(t *testing.T) {
	p := &persona.Persona{Name: "老板娘", Style: "warm", Taboos: []string{"no lecturing"}}
	j := &judge.Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.7}
	a := memory.Affect{Valence: 0.3, Arousal: 0.4}
	got := Build(p, "comfort", j, a, "ask about their week")
	for _, want := range []string{"老板娘", "comfort", "empathy", "sadness", "ask about their week"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
