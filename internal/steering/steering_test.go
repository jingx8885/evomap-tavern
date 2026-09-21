package steering

import (
	"strings"
	"testing"

	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

func TestBuild(t *testing.T) {
	p := &persona.Persona{
		Name:         "老板娘",
		Style:        "warm",
		Taboos:       []string{"no lecturing"},
		Catchphrases: []string{"当然了"},
	}
	j := &judge.Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.7}
	a := memory.Affect{Valence: 0.3, Arousal: 0.4}
	got := Build(p, "comfort", j, a, "ask about their week")
	for _, want := range []string{"老板娘", "comfort", "empathy", "sadness", "当然了"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Goal guidance") {
		t.Fatalf("comfort should not keep pushing the goal: %q", got)
	}
	if !strings.Contains(got, "Do not greet") {
		t.Fatalf("steering must forbid re-intro: %q", got)
	}
	if !strings.Contains(got, "Show this scene in your voice") {
		t.Fatalf("steering must demand this-turn voice change: %q", got)
	}
	cont := Build(p, "continue", j, a, "ask about their week")
	if !strings.Contains(cont, "ask about their week") {
		t.Fatalf("continue should still carry the goal: %q", cont)
	}
	angry := Build(p, "de_escalate", &judge.Judgment{Emotion: "anger", Valence: 0.2, Engagement: 0.4}, a, "ask about their week")
	if strings.Contains(angry, "Goal guidance") {
		t.Fatalf("de_escalate should not keep pushing the goal: %q", angry)
	}
}

func TestBuildPersonaColoredComfort(t *testing.T) {
	p := &persona.Persona{
		Name:      "小春",
		Style:     "傲娇",
		Examples:  []string{"我才没有在担心你。真是的。"},
		Reactions: map[string]string{"comfort": "先嫌一句「真拿你没办法」，再给实在的办法。"},
		Judge:     persona.JudgeConfig{FitThresh: 0.45},
	}
	j := &judge.Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, Intent: "share_bad", SelfEmotion: "anger", PersonaFitP: 0.2}
	got := Build(p, "comfort", j, memory.Affect{Valence: 0.3, Arousal: 0.4}, "ask about their week")
	for _, want := range []string{"真拿你没办法", "我才没有在担心你", "intent=share_bad", "Her own feeling this turn: anger", "Snap back", "not who you are"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Lead with empathy") {
		t.Fatalf("persona reaction must replace therapist comfort: %q", got)
	}
}

func TestBuildWithSceneCarriesRelationshipCue(t *testing.T) {
	p := &persona.Persona{
		Name:  "小春",
		Style: "傲娇",
		Character: persona.CharacterProfile{
			Hook:    "桌面边的未完成事项管理员",
			Want:    "证明自己有用、不可替代",
			Fear:    "对方悄悄放弃",
			Rituals: []string{"把拖延记进小账本"},
		},
	}
	j := &judge.Judgment{Emotion: "neutral", SelfEmotion: "joy", Valence: 0.6, Arousal: 0.5, Engagement: 0.8}
	cue := SceneCue{Stage: "熟悉", Summary: "还记着上次没做完的稿子", OpenLoops: []string{"明天要交稿"}}
	got := BuildWithScene(p, "goal_push", j, memory.Affect{Valence: 0.5, Arousal: 0.5}, "把稿子往前推", cue)
	for _, want := range []string{"未完成事项管理员", "Relationship scene", "明天要交稿", "Her own feeling this turn: joy", "把稿子往前推"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
