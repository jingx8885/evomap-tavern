package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

func f(v float64) *float64 { return &v }

func TestScoreNorm(t *testing.T) {
	a := jev.Answer{Score: f(4), Legend: map[string]string{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}}
	if got := scoreNorm(a); got != 1.0 {
		t.Fatalf("got %v", got)
	}
	a.Score = f(2)
	if got := scoreNorm(a); got != 0.5 {
		t.Fatalf("got %v", got)
	}
}

func TestParse(t *testing.T) {
	ans := map[string]jev.Answer{
		"valence":    {Score: f(3.5), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"arousal":    {Score: f(1), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"emotion":    {Choice: "sadness", Probabilities: map[string]float64{"sadness": 0.8}},
		"engagement": {Score: f(2), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"safety":     {Noul: f(0.7)},
	}
	j := Parse("i feel bad", ans)
	if j.Valence != 0.875 {
		t.Fatalf("valence %v", j.Valence)
	}
	if j.Emotion != "sadness" || j.SafetyP != 0.7 || j.Engagement != 0.5 {
		t.Fatalf("bad judgment: %+v", j)
	}
}

func TestDecideMode(t *testing.T) {
	a := memory.Affect{Valence: 0.2, Arousal: 0.4, Emotion: "sadness"}
	j := &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, SafetyP: 0.1}
	if got := DecideMode(j, a, 0.6, ""); got != "comfort" {
		t.Fatalf("want comfort, got %s", got)
	}
	j.SafetyP = 0.9
	if got := DecideMode(j, a, 0.6, ""); got != "safety" {
		t.Fatalf("want safety, got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.5, Engagement: 0.1}
	if got := DecideMode(j, a, 0.6, ""); got != "re_engage" {
		t.Fatalf("want re_engage, got %s", got)
	}
	j = &Judgment{Emotion: "joy", Valence: 0.9, Engagement: 0.9}
	if got := DecideMode(j, a, 0.6, "push"); got != "celebrate" {
		t.Fatalf("want celebrate, got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.6, Engagement: 0.8}
	if got := DecideMode(j, a, 0.6, "push"); got != "goal_push" {
		t.Fatalf("want goal_push, got %s", got)
	}
}

func TestJudgeTurnAgainstFakeServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["questions"]; !ok {
			t.Error("missing questions")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-latest",
			"answers": map[string]any{
				"valence":     map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"arousal":     map[string]any{"type": "score", "score": 2, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"emotion":     map[string]any{"type": "choice", "choice": "joy", "probabilities": map[string]float64{"joy": 0.9}},
				"engagement":  map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"safety":      map[string]any{"type": "noul", "noul": 0.01},
				"persona_fit": map[string]any{"type": "noul", "noul": 0.9},
			},
		})
	}))
	defer srv.Close()
	p := &persona.Persona{Name: "test", Style: "warm",
		Judge: persona.JudgeConfig{Emotion: true, PersonaFit: true, Safety: true}}
	jc := jev.NewClient(srv.URL, "k", "")
	mem := memory.New(8)
	mem.Add(memory.Turn{Speaker: "assistant", Text: "welcome!"})
	jd, err := JudgeTurn(context.Background(), jc, p, mem, "I love this place")
	if err != nil {
		t.Fatal(err)
	}
	if jd.Emotion != "joy" || jd.Valence != 1.0 || jd.PersonaFitP != 0.9 {
		t.Fatalf("bad judgment: %+v", jd)
	}
}
