// Package avatar maps Jev judgments onto a Live2D drive frame.
// The viewer is a thin web client; this package only decides expression,
// motion, look-at and intensity. It does not generate text.
package avatar

import (
	"math"
	"strings"

	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
)

// Official Haru expression ids (CubismWebSamples).
// F01 smile, F02 intense, F03 sad, F04 worried, F05 closed-eye smile,
// F06 surprise, F07 blush, F08 frown.
const (
	ExpNeutral = "F01"
	ExpSoft    = "F03"
	ExpStern   = "F02"
	ExpBright  = "F05"
	ExpSpark   = "F06"
	ExpPlay    = "F07"
	ExpWarm    = "F07"
	ExpAlert   = "F06"
	ExpWorry   = "F04"
	ExpFrown   = "F08"
)

// Frame is one Live2D pose instruction pushed over WebSocket.
type Frame struct {
	Type              string             `json:"type"`
	Mode              string             `json:"mode"`
	Emotion           string             `json:"emotion"`
	SelfEmotion       string             `json:"self_emotion,omitempty"`
	Expression        string             `json:"expression"`
	MotionGroup       string             `json:"motion_group"`
	MotionIndex       int                `json:"motion_index"`
	Valence           float64            `json:"valence"`
	Arousal           float64            `json:"arousal"`
	Engagement        float64            `json:"engagement"`
	SafetyP           float64            `json:"safety_p"`
	LookAt            float64            `json:"look_at"`
	Intensity         float64            `json:"intensity"`
	NeedLLM           float64            `json:"need_llm"`
	RelationshipStage string             `json:"relationship_stage,omitempty"`
	Bond              float64            `json:"bond,omitempty"`
	UserText          string             `json:"user_text,omitempty"`
	Params            map[string]float64 `json:"params,omitempty"`
}

// Drive turns a steering mode + Jev judgment + running affect into a frame.
// The face follows her own feeling first; the user's emotion is context, not
// the thing she mirrors one-to-one.
func Drive(mode string, j *judge.Judgment, a memory.Affect) Frame {
	if j == nil {
		j = &judge.Judgment{Emotion: "neutral"}
	}
	if mode == "" {
		mode = "continue"
	}
	f := Frame{
		Type:        "drive",
		Mode:        mode,
		Emotion:     j.Emotion,
		SelfEmotion: j.SelfEmotion,
		Valence:     j.Valence,
		Arousal:     j.Arousal,
		Engagement:  j.Engagement,
		SafetyP:     j.SafetyP,
		NeedLLM:     j.NeedLLMP,
		UserText:    j.UserText,
	}
	if f.Valence == 0 && a.Valence != 0 {
		f.Valence = a.Valence
	}
	if f.Arousal == 0 && a.Arousal != 0 {
		f.Arousal = a.Arousal
	}
	f.Expression = expressionFor(mode, j)
	f.MotionGroup, f.MotionIndex = motionFor(mode, f.Arousal)
	f.LookAt = clamp01(j.Engagement)
	if f.LookAt == 0 {
		f.LookAt = 0.55
	}
	f.Intensity = intensityFromScores(j)
	f.Params = overlayFor(mode, j, f.Intensity)
	return f
}

// DriveWithRelationship is the live turn's face state plus the relationship
// scene, so the same mode can look warmer as the bond grows.
func DriveWithRelationship(mode string, j *judge.Judgment, a memory.Affect, cue memory.RelationshipCue) Frame {
	f := Drive(mode, j, a)
	f.RelationshipStage = cue.Stage
	f.Bond = bondScore(cue)
	return f
}

func expressionFor(mode string, j *judge.Judgment) string {
	if mode == "safety" {
		return ExpSoft
	}
	if j == nil {
		return ExpNeutral
	}
	self := strings.ToLower(strings.TrimSpace(j.SelfEmotion))
	if self == "" {
		self = strings.ToLower(strings.TrimSpace(j.Emotion))
	}
	switch self {
	case "joy":
		if j.Arousal > 0.68 || j.Valence >= 0.8 {
			return ExpPlay
		}
		return ExpBright
	case "surprise":
		return ExpAlert
	case "sadness":
		return ExpSoft
	case "fear":
		return ExpWorry
	case "anger":
		return ExpFrown
	case "disgust":
		return ExpStern
	}
	if j.Valence >= 0.65 {
		return ExpBright
	}
	if j.Valence > 0 && j.Valence <= 0.35 {
		return ExpSoft
	}
	return ExpNeutral
}

func motionFor(mode string, arousal float64) (string, int) {
	switch mode {
	case "celebrate":
		return "TapBody", 3 // special_01
	case "re_engage":
		return "TapBody", 0
	case "safety", "comfort", "de_escalate":
		return "Idle", 0
	case "goal_push":
		if arousal > 0.6 {
			return "TapBody", 1
		}
		return "Idle", 1
	default:
		if arousal > 0.75 {
			return "TapBody", 2
		}
		return "Idle", 0
	}
}

func intensityFromScores(j *judge.Judgment) float64 {
	if j == nil {
		return 0.45
	}
	polar := math.Abs(j.Valence-0.5) * 2
	return clamp01(0.4 + 0.35*polar + 0.35*j.Arousal)
}

// overlayFor is applied every frame after Idle motions, so the face
// actually holds the Jev scores instead of snapping back to rest.
func overlayFor(mode string, j *judge.Judgment, intensity float64) map[string]float64 {
	if j == nil {
		j = &judge.Judgment{Emotion: "neutral"}
	}
	k := clamp01(0.4 + intensity*0.6)
	p := map[string]float64{}
	if mode == "safety" {
		p["ParamBrowLY"] = -0.7 * k
		p["ParamBrowRY"] = -0.7 * k
		p["ParamMouthForm"] = -0.85 * k
		p["ParamTear"] = 0.4 * k
		return p
	}
	self := strings.ToLower(strings.TrimSpace(j.SelfEmotion))
	if self == "" {
		self = strings.ToLower(strings.TrimSpace(j.Emotion))
	}
	switch self {
	case "joy":
		p["ParamTere"] = clamp01(0.45+j.Valence*0.55) * k
		p["ParamMouthForm"] = clamp01(0.35+j.Valence*0.6) * k
		p["ParamEyeLSmile"] = clamp01(0.3+j.Arousal*0.5) * k
		p["ParamEyeRSmile"] = clamp01(0.3+j.Arousal*0.5) * k
	case "surprise":
		p["ParamBrowLY"] = 0.75 * k
		p["ParamBrowRY"] = 0.75 * k
		p["ParamMouthForm"] = -0.25 * k
	case "sadness":
		p["ParamBrowLY"] = -0.7 * k
		p["ParamBrowRY"] = -0.7 * k
		p["ParamMouthForm"] = -0.85 * k
		p["ParamTear"] = 0.4 * k
	case "fear":
		p["ParamBrowLY"] = -0.35 * k
		p["ParamBrowRY"] = -0.35 * k
		p["ParamBrowLForm"] = 0.4 * k
		p["ParamBrowRForm"] = 0.4 * k
		p["ParamMouthForm"] = -0.45 * k
	case "anger", "disgust":
		p["ParamMouthForm"] = -0.85 * k
		p["ParamBrowLForm"] = 0.7 * k
		p["ParamBrowRForm"] = 0.7 * k
	default:
		if j.Valence >= 0.6 {
			p["ParamMouthForm"] = (j.Valence - 0.5) * 1.4 * k
			p["ParamTere"] = (j.Valence - 0.5) * k
		} else if j.Valence > 0 && j.Valence <= 0.4 {
			p["ParamMouthForm"] = (j.Valence - 0.5) * 1.6 * k
			p["ParamBrowLY"] = (j.Valence - 0.5) * k
			p["ParamBrowRY"] = (j.Valence - 0.5) * k
		}
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

func bondScore(cue memory.RelationshipCue) float64 {
	switch strings.ToLower(strings.TrimSpace(cue.Stage)) {
	case "信任":
		return 0.62
	case "亲密":
		return 0.88
	case "熟悉":
		return 0.36
	default:
		return 0.12
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
