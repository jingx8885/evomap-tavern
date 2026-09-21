package avatar

import (
	"testing"

	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
)

func TestDriveComfortUsesSoftFace(t *testing.T) {
	j := &judge.Judgment{Emotion: "sadness", Valence: 0.2, Arousal: 0.3, Engagement: 0.8, SafetyP: 0.1}
	a := memory.Affect{Valence: 0.25, Arousal: 0.35, Emotion: "sadness"}
	f := Drive("comfort", j, a)
	if f.Expression != ExpSoft {
		t.Fatalf("comfort expression %s", f.Expression)
	}
	if f.MotionGroup != "Idle" {
		t.Fatalf("comfort should idle, got %s", f.MotionGroup)
	}
	if f.LookAt != 0.8 {
		t.Fatalf("look_at %v", f.LookAt)
	}
	if f.Valence != 0.2 {
		t.Fatalf("face valence should be this turn's Jev score, got %v", f.Valence)
	}
}

func TestDriveCelebrateIsBright(t *testing.T) {
	j := &judge.Judgment{Emotion: "joy", Valence: 0.9, Arousal: 0.7, Engagement: 0.9}
	f := Drive("celebrate", j, memory.Affect{Valence: 0.85, Arousal: 0.7})
	if f.Expression != ExpBright && f.Expression != ExpPlay {
		t.Fatalf("celebrate expression %s", f.Expression)
	}
	if f.MotionGroup != "TapBody" {
		t.Fatalf("celebrate motion %s", f.MotionGroup)
	}
	if f.Params["ParamMouthForm"] <= 0 || f.Params["ParamTere"] <= 0 {
		t.Fatalf("joy overlay %+v", f.Params)
	}
}

func TestDriveSafetyOverridesJoyFace(t *testing.T) {
	j := &judge.Judgment{Emotion: "joy", Valence: 0.9, Engagement: 0.4, SafetyP: 0.9}
	f := Drive("safety", j, memory.Affect{})
	if f.Expression != ExpSoft {
		t.Fatalf("safety must not copy user joy, got %s", f.Expression)
	}
}

func TestDriveFaceFollowsJevEmotionNotMode(t *testing.T) {
	anger := &judge.Judgment{Emotion: "anger", Valence: 0.2, Arousal: 0.4, Engagement: 0.5}
	de := Drive("de_escalate", anger, memory.Affect{})
	cont := Drive("continue", anger, memory.Affect{})
	if de.Expression != ExpFrown {
		t.Fatalf("anger should be frown, got %s", de.Expression)
	}
	if de.Expression != cont.Expression {
		t.Fatalf("same Jev scores must share a face: de=%s continue=%s", de.Expression, cont.Expression)
	}
	joyPush := Drive("goal_push", &judge.Judgment{Emotion: "joy", Valence: 0.6, Arousal: 0.4}, memory.Affect{})
	if joyPush.Expression != ExpBright && joyPush.Expression != ExpPlay {
		t.Fatalf("joy under goal_push should stay a smile, got %s", joyPush.Expression)
	}
	fear := Drive("continue", &judge.Judgment{Emotion: "fear", Valence: 0.2, Arousal: 0.6}, memory.Affect{})
	if fear.Expression != ExpWorry {
		t.Fatalf("fear %s", fear.Expression)
	}
}

func TestDriveNilJudgment(t *testing.T) {
	f := Drive("", nil, memory.Affect{})
	if f.Mode != "continue" || f.Expression != ExpNeutral {
		t.Fatalf("%+v", f)
	}
}

func TestDriveOverlayHoldsEmotion(t *testing.T) {
	comfort := Drive("comfort", &judge.Judgment{Emotion: "sadness", Valence: 0.2, Arousal: 0.3}, memory.Affect{})
	if comfort.Params["ParamMouthForm"] >= 0 {
		t.Fatalf("sadness overlay %+v", comfort.Params)
	}
	celeb := Drive("celebrate", &judge.Judgment{Emotion: "joy", Valence: 0.9, Arousal: 0.7}, memory.Affect{})
	if celeb.Params["ParamTere"] < 0.4 || celeb.Params["ParamMouthForm"] < 0.3 {
		t.Fatalf("joy overlay %+v", celeb.Params)
	}
}

func TestDriveNeedLLMPassesThrough(t *testing.T) {
	j := &judge.Judgment{Emotion: "neutral", NeedLLMP: 0.81}
	f := Drive("continue", j, memory.Affect{})
	if f.NeedLLM != 0.81 {
		t.Fatalf("need_llm %v", f.NeedLLM)
	}
}
