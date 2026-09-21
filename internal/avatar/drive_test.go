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
	if f.Intensity > 0.5 {
		t.Fatalf("comfort should be low intensity, got %v", f.Intensity)
	}
}

func TestDriveCelebrateIsBright(t *testing.T) {
	j := &judge.Judgment{Emotion: "joy", Valence: 0.9, Arousal: 0.7, Engagement: 0.9}
	f := Drive("celebrate", j, memory.Affect{Valence: 0.85, Arousal: 0.7})
	if f.Expression != ExpBright {
		t.Fatalf("celebrate expression %s", f.Expression)
	}
	if f.MotionGroup != "TapBody" {
		t.Fatalf("celebrate motion %s", f.MotionGroup)
	}
	if f.Intensity != 1 {
		t.Fatalf("intensity %v", f.Intensity)
	}
}

func TestDriveSafetyOverridesJoyFace(t *testing.T) {
	j := &judge.Judgment{Emotion: "joy", Engagement: 0.4, SafetyP: 0.9}
	f := Drive("safety", j, memory.Affect{})
	if f.Expression != ExpSoft {
		t.Fatalf("safety must not copy user joy, got %s", f.Expression)
	}
}

func TestDriveContinueFallsBackToEmotion(t *testing.T) {
	j := &judge.Judgment{Emotion: "anger", Arousal: 0.4, Engagement: 0.6}
	f := Drive("continue", j, memory.Affect{Arousal: 0.4})
	if f.Expression != ExpStern {
		t.Fatalf("got %s", f.Expression)
	}
}

func TestDriveNilJudgment(t *testing.T) {
	f := Drive("", nil, memory.Affect{})
	if f.Mode != "continue" || f.Expression != ExpNeutral {
		t.Fatalf("%+v", f)
	}
}

func TestDriveOverlayHoldsEmotion(t *testing.T) {
	comfort := Drive("comfort", &judge.Judgment{Emotion: "sadness", Arousal: 0.3}, memory.Affect{})
	if comfort.Params["ParamMouthForm"] >= 0 {
		t.Fatalf("comfort overlay %+v", comfort.Params)
	}
	celeb := Drive("celebrate", &judge.Judgment{Emotion: "joy", Arousal: 0.7}, memory.Affect{})
	if celeb.Params["ParamTere"] < 0.4 || celeb.Params["ParamMouthForm"] < 0.3 {
		t.Fatalf("celebrate overlay %+v", celeb.Params)
	}
}
