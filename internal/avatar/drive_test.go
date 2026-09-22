package avatar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if f.MotionGroup != "Sway" {
		t.Fatalf("comfort should sway, got %s", f.MotionGroup)
	}
	if f.MotionIndex < 0 || f.MotionIndex >= len(motionCatalog["Sway"]) {
		t.Fatalf("comfort motion index %d", f.MotionIndex)
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

func TestDriveFaceFollowsSelfEmotionNotUserMirror(t *testing.T) {
	j := &judge.Judgment{Emotion: "sadness", SelfEmotion: "anger", Valence: 0.2, Arousal: 0.4, Engagement: 0.5}
	f := Drive("comfort", j, memory.Affect{})
	if f.Expression != ExpFrown {
		t.Fatalf("her own anger should override mirrored sadness, got %s", f.Expression)
	}
	if f.SelfEmotion != "anger" {
		t.Fatalf("self emotion not carried: %+v", f)
	}
}

func TestDriveWithRelationshipCarriesScene(t *testing.T) {
	cue := memory.RelationshipCue{Stage: "熟悉", Summary: "还记着上次没做完的稿子"}
	f := DriveWithRelationship("goal_push", &judge.Judgment{Emotion: "neutral", SelfEmotion: "joy", Valence: 0.6, Arousal: 0.4}, memory.Affect{}, cue)
	if f.RelationshipStage != "熟悉" || f.Bond <= 0.1 {
		t.Fatalf("relationship cue lost: %+v", f)
	}
	if f.Expression != ExpBright && f.Expression != ExpPlay {
		t.Fatalf("self joy should color the face, got %s", f.Expression)
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

func TestEveryHaruMotionIsReachable(t *testing.T) {
	root := filepath.Join("..", "..", "web", "live2d", "models", "Haru")
	raw, err := os.ReadFile(filepath.Join(root, "Haru.model3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		FileReferences struct {
			Motions map[string][]struct {
				File string `json:"File"`
			} `json:"Motions"`
		} `json:"FileReferences"`
	}
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	if len(model.FileReferences.Motions) != len(motionCatalog) {
		t.Fatalf("model groups %d catalog %d", len(model.FileReferences.Motions), len(motionCatalog))
	}
	seen := map[string]string{}
	for group, files := range motionCatalog {
		got := model.FileReferences.Motions[group]
		if len(got) != len(files) {
			t.Fatalf("%s model %d catalog %d", group, len(got), len(files))
		}
		for i, want := range files {
			if got[i].File != want {
				t.Fatalf("%s[%d] model %s catalog %s", group, i, got[i].File, want)
			}
			if prev, ok := seen[want]; ok {
				t.Fatalf("%s registered in %s and %s", want, prev, group)
			}
			seen[want] = group
			path := filepath.Join(root, filepath.FromSlash(want))
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("missing %s: %v", want, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "motions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".motion3.json") {
			continue
		}
		rel := "motions/" + ent.Name()
		if _, ok := seen[rel]; !ok {
			t.Fatalf("motion file not registered: %s", rel)
		}
		delete(seen, rel)
	}
	if len(seen) != 0 {
		t.Fatalf("catalog entries missing on disk: %v", seen)
	}

	hit := map[string]map[int]bool{}
	for group, files := range motionCatalog {
		hit[group] = map[int]bool{}
		for i := range files {
			hit[group][i] = false
		}
	}
	emotions := []string{"neutral", "joy", "surprise", "sadness", "fear", "anger", "disgust"}
	modes := []string{"continue", "comfort", "de_escalate", "celebrate", "re_engage", "goal_push", "safety"}
	for _, mode := range modes {
		for _, emo := range emotions {
			for _, arousal := range []float64{0, 0.3, 0.5, 0.65, 0.8, 1} {
				for _, valence := range []float64{0.1, 0.5, 0.9} {
					j := &judge.Judgment{Emotion: emo, SelfEmotion: emo, Arousal: arousal, Valence: valence}
					group, index := motionFor(mode, j)
					files := motionCatalog[group]
					if index < 0 || index >= len(files) {
						t.Fatalf("%s %s arousal %.2f -> %s[%d] out of range", mode, emo, arousal, group, index)
					}
					hit[group][index] = true
				}
			}
		}
	}
	for group, files := range motionCatalog {
		for i, file := range files {
			if !hit[group][i] {
				t.Fatalf("unreachable %s[%d] %s", group, i, file)
			}
		}
	}
}

func TestDriveNeedLLMPassesThrough(t *testing.T) {
	j := &judge.Judgment{Emotion: "neutral", NeedLLMP: 0.81}
	f := Drive("continue", j, memory.Affect{})
	if f.NeedLLM != 0.81 {
		t.Fatalf("need_llm %v", f.NeedLLM)
	}
}
