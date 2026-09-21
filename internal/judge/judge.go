// Package judge turns each conversation turn into typed Jev judgments:
// emotion (valence/arousal/label), engagement, safety, persona fit.
// Everything is asked in ONE /v1/systemone request.
package judge

import (
	"context"
	"fmt"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

// EmotionLabels is the closed set for the emotion choice question.
var EmotionLabels = map[string]string{
	"joy":      "Happy, excited, amused, or delighted.",
	"sadness":  "Sad, disappointed, hurt, or grieving.",
	"anger":    "Angry, annoyed, frustrated, or hostile.",
	"fear":     "Anxious, worried, scared, or uneasy.",
	"surprise": "Surprised, amazed, or caught off guard.",
	"disgust":  "Disgusted or contemptuous.",
	"neutral":  "Calm, factual, or hard to read.",
	"other":    "A different emotion dominates.",
}

// Judgment is what one turn produced.
type Judgment struct {
	UserText     string                `json:"user_text"`
	Valence      float64               `json:"valence"`
	Arousal      float64               `json:"arousal"`
	Emotion      string                `json:"emotion"`
	EmotionProbs map[string]float64    `json:"emotion_probs,omitempty"`
	Engagement   float64               `json:"engagement"`
	SafetyP      float64               `json:"safety_p"`
	PersonaFitP  float64               `json:"persona_fit_p,omitempty"`
	Confidence   float64               `json:"confidence,omitempty"`
	Raw          map[string]jev.Answer `json:"-"`
}

func scoreLevels() []string {
	return []string{
		"Very low: clearly negative, flat, or absent.",
		"Low: leaning negative or faint.",
		"Moderate: mixed, mild, or ambiguous.",
		"High: clearly present and strong.",
		"Very high: intense and unmistakable.",
	}
}

// questions builds the one-shot Jev question set for a turn.
func questions(p *persona.Persona, withPersonaFit bool) map[string]jev.Question {
	qs := map[string]jev.Question{
		"valence": {
			Type: "score",
			Instructions: "Rate the emotional valence expressed by the user in " +
				"section latest of state: how positive vs negative they feel. " +
				"Use the ordered levels.",
			Levels: scoreLevels(),
		},
		"arousal": {
			Type: "score",
			Instructions: "Rate the emotional arousal/energy of the user in " +
				"section latest: calm and flat at the low end, agitated " +
				"or excited at the high end.",
			Levels: scoreLevels(),
		},
		"emotion": {
			Type: "choice",
			Instructions: "Which single emotion best describes the user in " +
				"section latest?",
			Criteria: func() map[string]any {
				m := map[string]any{}
				for k, v := range EmotionLabels {
					m[k] = v
				}
				return m
			}(),
		},
		"engagement": {
			Type: "score",
			Instructions: "How engaged is the user in section latest - " +
				"are they actively participating or pulling away (short " +
				"replies, topic drops, goodbye signals)?",
			Levels: scoreLevels(),
		},
	}
	if p.Judge.Safety {
		qs["safety"] = jev.Question{
			Type: "noul",
			Instructions: "Does section latest of state signal distress, " +
				"self-harm, abuse, or another safety concern that needs a " +
				"gentle, careful response?",
		}
	}
	if withPersonaFit && p.Judge.PersonaFit {
		qs["persona_fit"] = jev.Question{
			Type: "noul",
			Instructions: "Does the assistant reply in section latest match the " +
				"persona style in state.persona (tone, register, taboos)?",
		}
	}
	return qs
}

func scoreNorm(a jev.Answer) float64 {
	n := a.LegendLen()
	if a.Score == nil || n < 2 {
		return 0
	}
	v := *a.Score / float64(n-1)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func noulVal(a jev.Answer) float64 {
	if a.Noul == nil {
		return 0
	}
	return *a.Noul
}

func confidenceOf(ans map[string]jev.Answer) float64 {
	var vals []float64
	for _, a := range ans {
		if a.Confidence != nil {
			vals = append(vals, *a.Confidence)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

// Parse converts raw Jev answers into a Judgment.
func Parse(userText string, ans map[string]jev.Answer) Judgment {
	j := Judgment{UserText: userText, Emotion: "neutral", Raw: ans}
	if a, ok := ans["valence"]; ok {
		j.Valence = scoreNorm(a)
	}
	if a, ok := ans["arousal"]; ok {
		j.Arousal = scoreNorm(a)
	}
	if a, ok := ans["emotion"]; ok {
		if a.Choice != "" {
			j.Emotion = a.Choice
		}
		j.EmotionProbs = a.Probabilities
	}
	if a, ok := ans["engagement"]; ok {
		j.Engagement = scoreNorm(a)
	}
	if a, ok := ans["safety"]; ok {
		j.SafetyP = noulVal(a)
	}
	if a, ok := ans["persona_fit"]; ok {
		j.PersonaFitP = noulVal(a)
	}
	j.Confidence = confidenceOf(ans)
	return j
}

// JudgeTurn evaluates one turn. userText may be empty when the upstream
// gateway exposes no user transcript; then Jev falls back to inferring
// user state from the latest exchange in state.
func JudgeTurn(ctx context.Context, client *jev.Client, p *persona.Persona,
	mem *memory.Memory, userText string) (*Judgment, error) {
	latest := map[string]string{}
	if t := userText; t != "" {
		latest["user"] = t
	}
	if t := mem.LatestAssistantText(); t != "" {
		latest["assistant"] = t
	}
	if len(latest) == 0 {
		return nil, fmt.Errorf("nothing to judge yet")
	}
	state := map[string]any{
		"persona": map[string]string{
			"name": p.Name, "style": p.Style,
		},
		"recent": mem.Recent(6),
		"latest": latest,
		"note": "latest.user is the utterance to judge when present; " +
			"otherwise infer the user's likely state from the latest exchange.",
	}
	res, err := client.Evaluate(ctx, state, questions(p, latest["assistant"] != ""))
	if err != nil {
		return nil, err
	}
	jd := Parse(userText, res.Answers)
	return &jd, nil
}

// DecideMode maps a judgment + running affect to a steering mode.
// Order matters: safety first.
func DecideMode(j *Judgment, a memory.Affect, safetyThresh float64, planNote string) string {
	if j.SafetyP >= safetyThresh {
		return "safety"
	}
	switch j.Emotion {
	case "sadness", "fear":
		if j.Valence < 0.4 {
			return "comfort"
		}
	case "anger":
		if j.Arousal > 0.5 {
			return "de_escalate"
		}
	case "joy":
		if j.Valence > 0.7 {
			return "celebrate"
		}
	}
	if j.Engagement < 0.3 {
		return "re_engage"
	}
	if planNote != "" && j.Engagement > 0.6 {
		return "goal_push"
	}
	return "continue"
}
