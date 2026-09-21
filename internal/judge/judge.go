// Package judge turns each conversation turn into typed Jev judgments:
// user affect, intent, the persona's own feeling, steering mode, and fit.
// Everything is asked in ONE /v1/systemone request. Go only applies the
// safety latch and falls back if Jev omits a mode.
package judge

import (
	"context"
	"fmt"
	"strings"

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

// IntentLabels is what the user is doing this turn, not how they feel.
var IntentLabels = map[string]string{
	"chat":         "Ordinary conversation or small talk.",
	"comfort_seek": "Asking to be comforted, or venting distress.",
	"banter":       "Playful teasing, arguing, or sparring. Not a real fight.",
	"share_good":   "Sharing good news or a win.",
	"share_bad":    "Sharing something hard, without necessarily asking for help.",
	"request":      "Wants information, a plan, or a concrete favor.",
	"goodbye":      "Leaving or wrapping up.",
	"other":        "None of the above.",
}

// DefaultModeCriteria describe steering modes for Jev when the persona
// YAML has no reactions. Always: pick THIS persona's way, not a therapist.
var DefaultModeCriteria = map[string]string{
	"safety":      "Distress or self-harm. Slow down, stay careful, still in persona.",
	"comfort":     "User is sad, scared, or hurting. Care the way THIS persona would — not a generic therapist.",
	"de_escalate": "User is angry or frustrated. Lower energy, don't fight, stay in persona.",
	"celebrate":   "User is genuinely happy or sharing a win. Share it in persona, not fake pep.",
	"re_engage":   "User is pulling away but not saying goodbye. One specific follow-up, no greeting.",
	"goal_push":   "The conversation can take a light nudge toward the long-term goal without ignoring mood.",
	"continue":    "Keep going from what they just said. Default when nothing else fits.",
}

// DefaultNeedLLM is the noul cutoff for launching the planner LLM.
const DefaultNeedLLM = 0.55

// DefaultFitThresh is the persona_fit noul below which steering snaps back.
const DefaultFitThresh = 0.45

var knownModes = map[string]bool{
	"safety": true, "comfort": true, "de_escalate": true,
	"celebrate": true, "re_engage": true, "goal_push": true, "continue": true,
}

// Judgment is what one turn produced.
type Judgment struct {
	UserText     string                `json:"user_text"`
	Valence      float64               `json:"valence"`
	Arousal      float64               `json:"arousal"`
	Emotion      string                `json:"emotion"`
	EmotionProbs map[string]float64    `json:"emotion_probs,omitempty"`
	Engagement   float64               `json:"engagement"`
	Intent       string                `json:"intent,omitempty"`
	SelfEmotion  string                `json:"self_emotion,omitempty"`
	Mode         string                `json:"mode,omitempty"`
	SafetyP      float64               `json:"safety_p"`
	PersonaFitP  float64               `json:"persona_fit_p,omitempty"`
	NeedLLMP     float64               `json:"need_llm"`
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

func criteriaCopy(src map[string]string) map[string]any {
	m := map[string]any{}
	for k, v := range src {
		m[k] = v
	}
	return m
}

func modeCriteria(p *persona.Persona, planNote string) map[string]any {
	m := map[string]any{}
	for mode, def := range DefaultModeCriteria {
		if mode == "goal_push" && strings.TrimSpace(planNote) == "" {
			continue
		}
		if r := p.Reaction(mode); r != "" {
			m[mode] = r + " (" + def + ")"
			continue
		}
		m[mode] = def
	}
	return m
}

// questions builds the one-shot Jev question set for a turn.
func questions(p *persona.Persona, withPersonaFit bool, planNote string) map[string]jev.Question {
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
			Criteria: criteriaCopy(EmotionLabels),
		},
		"intent": {
			Type: "choice",
			Instructions: "What is the user doing in section latest? " +
				"Judge the conversational move, not the emotion. " +
				"Playful complaining or tsundere sparring is banter, not comfort_seek.",
			Criteria: criteriaCopy(IntentLabels),
		},
		"self_emotion": {
			Type: "choice",
			Instructions: "Which single emotion would the persona in state.persona " +
				"feel in response to section latest, given their style and reactions? " +
				"This is the assistant's own feeling, not the user's.",
			Criteria: criteriaCopy(EmotionLabels),
		},
		"engagement": {
			Type: "score",
			Instructions: "How engaged is the user in section latest - " +
				"are they actively participating or pulling away (short " +
				"replies, topic drops, goodbye signals)?",
			Levels: scoreLevels(),
		},
		"mode": {
			Type: "choice",
			Instructions: "Which steering mode should this persona use this turn? " +
				"Pick from the persona's own way of handling the user's latest " +
				"utterance — not a generic therapist, customer-service, or pep-talk " +
				"script. Use state.persona.reactions when present. " +
				"Banter stays continue, not comfort or de_escalate. " +
				"Goodbye stays continue, not re_engage. " +
				"Pick goal_push only if state.plan is non-empty and a nudge fits. " +
				"If the last assistant reply drifted off persona, still pick the " +
				"mode for the USER's need; the runtime will snap the voice back.",
			Criteria: modeCriteria(p, planNote),
		},
		"need_llm": {
			Type: "noul",
			Instructions: "Should a slower LLM planner run after this turn? " +
				"Yes if the user asked for a plan, a decision, a joke/story " +
				"that needs invention, a stuck conversation that needs a new " +
				"direction, or anything the live voice model cannot do well " +
				"alone. No for greetings, backchannels (嗯/哦/好/喂), small " +
				"talk, or a reply the voice model can continue immediately.",
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
				"persona style, taboos, reactions, and catchphrases-used-sparingly " +
				"in state.persona? No if it sounded like a therapist, receptionist, " +
				"or a different character.",
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
	if a, ok := ans["intent"]; ok {
		j.Intent = a.Choice
	}
	if a, ok := ans["self_emotion"]; ok {
		j.SelfEmotion = a.Choice
	}
	if a, ok := ans["mode"]; ok && knownModes[a.Choice] {
		j.Mode = a.Choice
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
	if a, ok := ans["need_llm"]; ok {
		j.NeedLLMP = noulVal(a)
	}
	j.Confidence = confidenceOf(ans)
	return j
}

// WantLLM reports whether this turn should spend an LLM planner call.
func (j *Judgment) WantLLM(thresh float64) bool {
	if j == nil {
		return false
	}
	if thresh <= 0 {
		thresh = DefaultNeedLLM
	}
	return j.NeedLLMP >= thresh
}

// OffPersona reports whether the last assistant reply drifted off style.
// Unasked persona_fit (no noul, zero value) does not count as a miss.
func (j *Judgment) OffPersona(thresh float64) bool {
	if j == nil {
		return false
	}
	if thresh <= 0 {
		thresh = DefaultFitThresh
	}
	scored := false
	if j.Raw != nil {
		if a, ok := j.Raw["persona_fit"]; ok && a.Noul != nil {
			scored = true
		}
	}
	if !scored && j.PersonaFitP > 0 {
		scored = true
	}
	if !scored {
		return false
	}
	return j.PersonaFitP < thresh
}

// JudgeTurn evaluates one turn. userText may be empty when the upstream
// gateway exposes no user transcript; then Jev falls back to inferring
// user state from the latest exchange in state.
func JudgeTurn(ctx context.Context, client *jev.Client, p *persona.Persona,
	mem *memory.Memory, userText, planNote string) (*Judgment, error) {
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
		"persona": map[string]any{
			"name":         p.Name,
			"style":        p.Style,
			"catchphrases": strings.Join(p.Catchphrases, "; "),
			"examples":     p.Examples,
			"reactions":    p.Reactions,
			"taboos":       p.Taboos,
		},
		"recent": mem.Recent(6),
		"latest": latest,
		"plan":   planNote,
		"note": "latest.user is the utterance to judge when present; " +
			"otherwise infer the user's likely state from the latest exchange.",
	}
	res, err := client.Evaluate(ctx, state, questions(p, latest["assistant"] != "", planNote))
	if err != nil {
		return nil, err
	}
	jd := Parse(userText, res.Answers)
	return &jd, nil
}

// DecideMode maps a judgment to a steering mode.
// Safety is a hard code latch. Mode itself is Jev's choice when present;
// the emotion heuristic is only a fallback for incomplete answers.
func DecideMode(j *Judgment, a memory.Affect, safetyThresh float64, planNote string) string {
	if j.SafetyP >= safetyThresh {
		return "safety"
	}
	if knownModes[j.Mode] {
		if j.Mode == "goal_push" && strings.TrimSpace(planNote) == "" {
			return "continue"
		}
		return j.Mode
	}
	return fallbackMode(j, a, planNote)
}

func fallbackMode(j *Judgment, a memory.Affect, planNote string) string {
	_ = a
	playful := j.Intent == "banter"
	switch j.Intent {
	case "goodbye":
		return "continue"
	}
	switch j.Emotion {
	case "sadness", "fear":
		if j.Valence <= 0.48 && !playful {
			return "comfort"
		}
	case "anger", "disgust":
		if (j.Valence < 0.55 || j.Arousal >= 0.32) && !playful {
			return "de_escalate"
		}
	case "joy":
		if j.Valence >= 0.45 {
			return "celebrate"
		}
	}
	if j.Intent == "comfort_seek" && j.Valence <= 0.55 {
		return "comfort"
	}
	if j.Intent == "share_good" && j.Valence >= 0.45 {
		return "celebrate"
	}
	if j.Engagement < 0.3 {
		return "re_engage"
	}
	if planNote != "" && j.Engagement > 0.6 {
		return "goal_push"
	}
	return "continue"
}
