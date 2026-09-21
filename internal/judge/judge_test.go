package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"need_llm":   {Noul: f(0.2)},
	}
	j := Parse("i feel bad", ans)
	if j.Valence != 0.875 {
		t.Fatalf("valence %v", j.Valence)
	}
	if j.Emotion != "sadness" || j.SafetyP != 0.7 || j.Engagement != 0.5 {
		t.Fatalf("bad judgment: %+v", j)
	}
	if j.NeedLLMP != 0.2 {
		t.Fatalf("need_llm %v", j.NeedLLMP)
	}
	if j.WantLLM(0.55) {
		t.Fatal("low need_llm should not launch LLM")
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
	j = &Judgment{Emotion: "joy", Valence: 0.52, Engagement: 0.74, SafetyP: 0.03}
	if got := DecideMode(j, a, 0.6, "push"); got != "celebrate" {
		t.Fatalf("mild joy must celebrate, not goal_push; got %s", got)
	}
	j = &Judgment{Emotion: "anger", Valence: 0.17, Arousal: 0.26, Engagement: 0.14}
	if got := DecideMode(j, a, 0.6, "push"); got != "de_escalate" {
		t.Fatalf("low-arousal anger must de_escalate, not re_engage; got %s", got)
	}
	j = &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, Intent: "banter"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("playful 我好惨 is banter, not comfort; got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.5, Engagement: 0.1, Intent: "goodbye"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("goodbye must not re_engage; got %s", got)
	}
	j = &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, Mode: "continue"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("Jev mode wins over the emotion heuristic; got %s", got)
	}
	j = &Judgment{Emotion: "joy", Valence: 0.9, Engagement: 0.9, Mode: "celebrate", SafetyP: 0.9}
	if got := DecideMode(j, a, 0.6, ""); got != "safety" {
		t.Fatalf("safety latch still overrides Jev mode; got %s", got)
	}
	j = &Judgment{Mode: "goal_push"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("goal_push without a plan note must not fire; got %s", got)
	}
}

func TestOffPersona(t *testing.T) {
	if (&Judgment{}).OffPersona(0.45) {
		t.Fatal("unasked fit is not a miss")
	}
	if !(&Judgment{PersonaFitP: 0.2}).OffPersona(0.45) {
		t.Fatal("low fit should snap back")
	}
	if (&Judgment{PersonaFitP: 0.9}).OffPersona(0.45) {
		t.Fatal("high fit should not snap back")
	}
	n := 0.05
	j := &Judgment{Raw: map[string]jev.Answer{"persona_fit": {Noul: &n}}}
	j.PersonaFitP = 0.05
	if !j.OffPersona(0.45) {
		t.Fatal("raw noul 0.05 is a miss")
	}
}

func TestJudgeTurnAgainstFakeServer(t *testing.T) {
	var gotQuestions map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if qs, ok := req["questions"].(map[string]any); ok {
			gotQuestions = qs
		} else {
			t.Error("missing questions")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-latest",
			"answers": map[string]any{
				"valence":      map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"arousal":      map[string]any{"type": "score", "score": 2, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"emotion":      map[string]any{"type": "choice", "choice": "joy", "probabilities": map[string]float64{"joy": 0.9}},
				"engagement":   map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"safety":       map[string]any{"type": "noul", "noul": 0.01},
				"persona_fit":  map[string]any{"type": "noul", "noul": 0.9},
				"need_llm":     map[string]any{"type": "noul", "noul": 0.12},
				"intent":       map[string]any{"type": "choice", "choice": "share_good"},
				"self_emotion": map[string]any{"type": "choice", "choice": "joy"},
				"mode":         map[string]any{"type": "choice", "choice": "celebrate"},
			},
		})
	}))
	defer srv.Close()
	p := &persona.Persona{Name: "test", Style: "warm",
		Judge: persona.JudgeConfig{Emotion: true, PersonaFit: true, Safety: true}}
	jc := jev.NewClient(srv.URL, "k", "")
	mem := memory.New(8)
	mem.Add(memory.Turn{Speaker: "assistant", Text: "welcome!"})
	p.Reactions = map[string]string{"comfort": "先嫌一句，再帮忙"}
	jd, err := JudgeTurn(context.Background(), jc, p, mem, "I love this place", "")
	if err != nil {
		t.Fatal(err)
	}
	if jd.Emotion != "joy" || jd.Valence != 1.0 || jd.PersonaFitP != 0.9 {
		t.Fatalf("bad judgment: %+v", jd)
	}
	if jd.Intent != "share_good" || jd.SelfEmotion != "joy" || jd.Mode != "celebrate" {
		t.Fatalf("jev extras %+v", jd)
	}
	if jd.NeedLLMP != 0.12 || jd.WantLLM(0) {
		t.Fatalf("need_llm %+v", jd)
	}
	for _, q := range []string{"need_llm", "intent", "self_emotion", "mode"} {
		if _, ok := gotQuestions[q]; !ok {
			t.Fatalf("turn judge must ask %s, got %v", q, gotQuestions)
		}
	}
	modeQ, _ := gotQuestions["mode"].(map[string]any)
	crit, _ := modeQ["criteria"].(map[string]any)
	if _, ok := crit["goal_push"]; ok {
		t.Fatalf("empty plan must omit goal_push: %v", crit)
	}
	if s, _ := crit["comfort"].(string); !strings.Contains(s, "先嫌一句") {
		t.Fatalf("mode criteria should carry persona reaction, got %v", crit["comfort"])
	}
}
