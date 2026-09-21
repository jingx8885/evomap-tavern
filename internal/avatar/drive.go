// Package avatar maps Jev judgments onto a Live2D drive frame.
// The viewer is a thin web client; this package only decides expression,
// motion, look-at and intensity. It does not generate text.
package avatar

import (
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
	ExpWarm    = "F01"
	ExpAlert   = "F06"
)

// Frame is one Live2D pose instruction pushed over WebSocket.
type Frame struct {
	Type        string             `json:"type"`
	Mode        string             `json:"mode"`
	Emotion     string             `json:"emotion"`
	Expression  string             `json:"expression"`
	MotionGroup string             `json:"motion_group"`
	MotionIndex int                `json:"motion_index"`
	Valence     float64            `json:"valence"`
	Arousal     float64            `json:"arousal"`
	Engagement  float64            `json:"engagement"`
	SafetyP     float64            `json:"safety_p"`
	LookAt      float64            `json:"look_at"`
	Intensity   float64            `json:"intensity"`
	UserText    string             `json:"user_text,omitempty"`
	Params      map[string]float64 `json:"params,omitempty"`
}

// Drive turns a steering mode + Jev judgment + running affect into a frame.
// The avatar reacts as the persona (comfort when the user is sad), not by
// copying the user's face.
func Drive(mode string, j *judge.Judgment, a memory.Affect) Frame {
	if j == nil {
		j = &judge.Judgment{Emotion: "neutral"}
	}
	if mode == "" {
		mode = "continue"
	}
	f := Frame{
		Type:       "drive",
		Mode:       mode,
		Emotion:    j.Emotion,
		Valence:    a.Valence,
		Arousal:    a.Arousal,
		Engagement: j.Engagement,
		SafetyP:    j.SafetyP,
		UserText:   j.UserText,
	}
	if f.Valence == 0 && j.Valence != 0 {
		f.Valence = j.Valence
	}
	if f.Arousal == 0 && j.Arousal != 0 {
		f.Arousal = j.Arousal
	}
	f.Expression = expressionFor(mode, j)
	f.MotionGroup, f.MotionIndex = motionFor(mode, f.Arousal)
	f.LookAt = clamp01(j.Engagement)
	if f.LookAt == 0 {
		f.LookAt = 0.55
	}
	f.Intensity = intensityFor(mode, f.Arousal)
	f.Params = overlayFor(mode, j, f.Intensity)
	return f
}

func expressionFor(mode string, j *judge.Judgment) string {
	switch mode {
	case "safety", "comfort":
		return ExpSoft
	case "de_escalate":
		return ExpNeutral
	case "celebrate":
		if j.Arousal > 0.8 {
			return ExpPlay
		}
		return ExpBright
	case "re_engage":
		return ExpSpark
	case "goal_push":
		return ExpWarm
	}
	switch j.Emotion {
	case "joy":
		return ExpBright
	case "surprise":
		return ExpAlert
	case "sadness", "fear":
		return ExpSoft
	case "anger", "disgust":
		return ExpStern
	default:
		return ExpNeutral
	}
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

func intensityFor(mode string, arousal float64) float64 {
	switch mode {
	case "safety", "comfort", "de_escalate":
		return clamp01(0.25 + arousal*0.2)
	case "celebrate":
		return 1
	case "re_engage":
		return 0.75
	default:
		return clamp01(0.35 + arousal*0.5)
	}
}

// overlayFor is applied every frame after Idle motions, so the face
// actually holds the Jev reaction instead of snapping back to rest.
// Uses Haru params (ParamMouthForm / ParamTere / brows). Mouth openness
// is ParamMouthOpenY and is driven only by lip sync.
func overlayFor(mode string, j *judge.Judgment, intensity float64) map[string]float64 {
	if j == nil {
		j = &judge.Judgment{Emotion: "neutral"}
	}
	k := clamp01(0.4 + intensity*0.6)
	p := map[string]float64{}
	switch mode {
	case "safety", "comfort":
		p["ParamBrowLY"] = -0.55 * k
		p["ParamBrowRY"] = -0.55 * k
		p["ParamMouthForm"] = -0.7 * k
		p["ParamTear"] = 0.25 * k
	case "de_escalate":
		p["ParamBrowLForm"] = 0.45 * k
		p["ParamBrowRForm"] = 0.45 * k
		p["ParamMouthForm"] = -0.35 * k
	case "celebrate":
		p["ParamTere"] = 0.85 * k
		p["ParamMouthForm"] = 0.65 * k
		p["ParamEyeLSmile"] = 0.6 * k
		p["ParamEyeRSmile"] = 0.6 * k
	case "re_engage":
		p["ParamBrowLY"] = 0.3 * k
		p["ParamBrowRY"] = 0.3 * k
		p["ParamMouthForm"] = 0.25 * k
		p["ParamTere"] = 0.2 * k
	case "goal_push":
		p["ParamMouthForm"] = 0.4 * k
		p["ParamTere"] = 0.28 * k
		p["ParamBrowLY"] = 0.18 * k
		p["ParamBrowRY"] = 0.18 * k
	default:
		switch j.Emotion {
		case "joy":
			p["ParamTere"] = 0.5 * k
			p["ParamMouthForm"] = 0.5 * k
			p["ParamEyeLSmile"] = 0.35 * k
			p["ParamEyeRSmile"] = 0.35 * k
		case "surprise":
			p["ParamBrowLY"] = 0.6 * k
			p["ParamBrowRY"] = 0.6 * k
			p["ParamMouthForm"] = -0.2 * k
		case "sadness", "fear":
			p["ParamBrowLY"] = -0.45 * k
			p["ParamBrowRY"] = -0.45 * k
			p["ParamMouthForm"] = -0.55 * k
			p["ParamTear"] = 0.2 * k
		case "anger", "disgust":
			p["ParamMouthForm"] = -0.6 * k
			p["ParamBrowLForm"] = 0.45 * k
			p["ParamBrowRForm"] = 0.45 * k
		}
	}
	if len(p) == 0 {
		return nil
	}
	return p
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
