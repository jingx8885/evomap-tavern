// Package judge turns each conversation turn into typed Jev judgments:
// user affect, intent, the persona's own feeling, steering mode, fit,
// and which capability to start. Everything is asked in ONE /v1/systemone
// request. That request is the only entry for tools. Go applies the safety
// latch, drops unknown choices, and falls back if Jev omits a mode.
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

// DefaultActConfidence is the choice confidence below which computer use
// is refused. A missing confidence does not refuse; an explicit low one does.
const DefaultActConfidence = 0.30

// DefaultBranchDone is the branch_done noul at which an open branch closes.
// Below it, the same branch keeps the conversation and the work.
const DefaultBranchDone = 0.80

// Capability ids. act is the closed set the turn Jev uses to pick one tool.
const (
	ActNone        = "none"
	ActPlan        = "plan"
	ActReflect     = "reflect"
	ActLook        = "look"
	ActCamera      = "camera"
	ActScreen      = "screen"
	ActCodex       = "codex"
	ActComputerUse = "computer_use"
	ActImage       = "image"
	ActVideo       = "video"
	ActSpeech      = "speech"
	ActSong        = "song"
	ActPicture     = "picture"
	ActWatch       = "watch"
	ActListen      = "listen"
	ActDivine      = "divine"
)

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
	Attend       string                `json:"attend,omitempty"`
	Act          string                `json:"act,omitempty"`
	BranchDoneP  float64               `json:"branch_done,omitempty"`
	Window       string                `json:"window,omitempty"`
	WinOp        string                `json:"win_op,omitempty"`
	WinTarget    string                `json:"win_target,omitempty"`
	Raw          map[string]jev.Answer `json:"-"`
}

// Branch is the capability already in progress. Empty Kind means none.
// The turn Jev sees it and answers branch_done; Go keeps the branch until that says yes.
type Branch struct {
	Kind string
	Goal string
	Note string
}

// Open reports whether a real capability is in progress.
func (b Branch) Open() bool {
	return knownAct[b.Kind] && b.Kind != ActNone
}

// Observe is what she could look at this turn. Jev picks a channel;
// it does not write the caption or the log.
type Observe struct {
	Camera string
	Screen string
	Log    []string
	Window WindowView
}

// WindowMod is one registered page module, as observation.
type WindowMod struct {
	ID    string
	Title string
	Open  bool
}

// WindowView is the closed set for her own pages this turn.
// Empty Modules means the questions are not asked.
type WindowView struct {
	Focused string
	Glance  string
	Modules []WindowMod
	Ops     map[string]string
	Targets map[string]string
}

// Live reports whether a page module is registered.
func (v WindowView) Live() bool {
	return len(v.Modules) > 0
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

// AttendLabels is which observation, if any, the voice model may see this turn.
var AttendLabels = map[string]string{
	"none":   "Nothing extra. Ordinary chat. Do not show the camera, the screen, or the log.",
	"camera": "The camera caption in state.observe.camera. They asked what she sees or who is there, or that scene matters to this utterance.",
	"screen": "The screen note in state.observe.screen. They asked about the screen, or the window is what this utterance is about.",
	"eyes":   "Both camera and screen. They asked what she can see, or both are relevant.",
	"log":    "state.observe.log. They asked about her log, an error, or a fault she should know.",
	"stage":  "state.window.glance. They asked what her stage window looks like, or the page is what this utterance is about.",
	"all":    "Camera, screen, and the log. They asked about her whole situation, or a fault and the scene both matter.",
}

var knownAttend = map[string]bool{
	"none": true, "camera": true, "screen": true, "eyes": true, "log": true, "stage": true, "all": true,
}

// ActLabels is the only tool entry. One choice, then Go executes.
// codex edits her own repo. computer_use is the desk loop for the machine.
var ActLabels = map[string]string{
	ActNone:        "Just talk. No tool, no desktop action, no extra look.",
	ActPlan:        "A slower written plan or decision is needed. Not a desktop action.",
	ActReflect:     "They asked her to notice herself: who she is, whether she can feel her voice, face, or mood, or a fault in her own log. Not a request to edit code or use the computer. A later step on this branch may read one file or remember one line.",
	ActLook:        "They asked how she is built, what her own code does, or to feel a specific file in her body. This branch keeps reading. If they then ask her to change herself, a later step may edit one allowlisted file. It does not reload the running process.",
	ActCamera:      "They asked her to look through the camera now: at them, the room, or who is there. Not the computer screen, and not a saved picture.",
	ActScreen:      "They asked her to look at the computer screen now: which window or what is on the desktop. Computer-use window titles, not the camera, and not a saved picture.",
	ActCodex:       "They want her to change her own source. The runtime picks one allowlisted file, writes a strict intent, edits only that, then she feels the diff. Not a general desktop action, and not merely talking about code. The running process does not reload.",
	ActComputerUse: "They want something done on this machine now that is not only editing her own repo: open an app, use a window, type, or act on the desktop. Not mere talk about computers.",
	ActImage:       "They want a still picture made, or she is being asked to make one (a bouquet, a scene, an icon). Talking about a thing is not enough.",
	ActVideo:       "They want a short moving clip made. Not a still picture, and not merely describing motion.",
	ActSpeech:      "They want a separate spoken or voiced audio line made. Not her live voice, and not a song.",
	ActSong:        "They want a song made with Suno: a melody, a track, or lyrics set to music. Not a spoken line and not a video.",
	ActPicture:     "They want her to look at a still picture that already exists, usually one she just made. Not a request to generate a new one, and not the live camera.",
	ActWatch:       "They want her to watch a video that already exists, usually one she just made. Not a request to generate a new clip, and not the live camera.",
	ActListen:      "They want her to listen to a song or voice recording that already exists. Not a request to compose a new song, and not her live microphone.",
	ActDivine:      "They want a fortune told with the eight trigrams and six lines: 算命, 占卜, 起卦, 六爻, 算一卦. A metaphor about luck is not this. Not a plan, and not a desktop action.",
}

var knownAct = map[string]bool{
	ActNone: true, ActPlan: true, ActReflect: true, ActLook: true,
	ActCamera: true, ActScreen: true, ActCodex: true, ActComputerUse: true,
	ActImage: true, ActVideo: true, ActSpeech: true, ActSong: true,
	ActPicture: true, ActWatch: true, ActListen: true,
	ActDivine: true,
}

// questions builds the one-shot Jev question set for a turn.
func questions(p *persona.Persona, withPersonaFit bool, planNote string, br Branch, win WindowView) map[string]jev.Question {
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
		"attend": {
			Type: "choice",
			Instructions: "Which observation should the voice model see this turn? " +
				"Read state.observe and section latest. " +
				"Pick none for ordinary chat. " +
				"Pick camera, screen, or eyes only when they asked what she sees, " +
				"or the note is clearly what the utterance is about. " +
				"Pick stage when they asked what her stage window looks like. " +
				"Pick log only when they asked about her log, an error, or a fault. " +
				"Pick all only when both the scene and the log matter. " +
				"Empty observe fields are not a reason to pick that channel. " +
				"Do not pick a channel just because a caption exists.",
			Criteria: criteriaCopy(AttendLabels),
		},
		"act": {
			Type: "choice",
			Instructions: "Which single capability should the runtime start after this turn? " +
				"This is the only entry for tools. Pick exactly one. " +
				"Pick none for ordinary chat, greetings, and backchannels. " +
				"Pick plan when a slower written decision is needed (a plan, a stuck " +
				"conversation, something the live voice should not invent alone). " +
				"Pick reflect when they ask her to notice herself, her mood, or a fault in her log. " +
				"Pick look when they ask how she is built or what her own code does. " +
				"Pick camera only when they asked her to look through the camera now. " +
				"Pick screen only when they asked her to look at the computer screen now. " +
				"Camera and screen are different capabilities. Do not pick one to answer the other. " +
				"Pick codex only when they want her to change her own source in this repo. One allowlisted file, then she feels the diff. " +
				"Pick computer_use only when they are asking her to act on this machine " +
				"now in a way that is not just editing her repo. " +
				"Talking about code or computers is not codex or computer_use. " +
				"Pick image only when a still picture should be generated. " +
				"Pick video only when a short clip should be generated. " +
				"Pick speech only when a separate voiced line should be generated. " +
				"Pick song only when a song should be generated. " +
				"Wanting or mentioning a flower is not image until they want it made. " +
				"Pick picture only to look at an existing still image. " +
				"Pick watch only to watch an existing video. " +
				"Pick listen only to hear an existing song or recording. " +
				"Looking is not the same as making. " +
				"Pick divine when they ask her to tell a fortune, cast a hexagram, or read 六爻. " +
				"Talking about luck in passing is not divine.",
			Criteria: criteriaCopy(ActLabels),
		},
	}
	if br.Open() {
		act := qs["act"]
		act.Instructions = fmt.Sprint(act.Instructions) +
			" state.branch is already open (" + br.Kind + ")." +
			" Keep act equal to that kind for a correction, a follow-up, or small talk while she works." +
			" Pick none to stay on it without starting another tool." +
			" Pick a different act as soon as they ask for another capability" +
			" (the screen, the camera, a fortune, a picture, the computer, or anything else in this list)." +
			" They can leave the open branch at any time. Do not wait for it to finish."
		qs["act"] = act
		qs["branch_done"] = branchDoneQuestion()
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
	if win.Live() {
		mods := map[string]string{"none": "Do not touch a page."}
		for _, m := range win.Modules {
			mods[m.ID] = m.Title + ". Her own page module, not a desktop window."
		}
		qs["window"] = jev.Question{
			Type: "choice",
			Instructions: "Which of her own page modules should win_op apply to? " +
				"Read state.window. These are her pages, not the computer's other windows. " +
				"Pick none to leave them alone.",
			Criteria: criteriaCopy(mods),
		}
		qs["win_op"] = jev.Question{
			Type: "choice",
			Instructions: "What should the runtime do to that page module? " +
				"Pick none to leave it. open and focus show it. hide and close cover it. " +
				"Layout and feature ops belong to the module that declared them. " +
				"Do not invent an op that is not listed.",
			Criteria: criteriaAny(win.Ops),
		}
		if len(win.Targets) > 0 {
			qs["win_target"] = jev.Question{
				Type: "choice",
				Instructions: "Which job should feature or cancel use? " +
					"Pick none to leave the featured job. Pick latest for the newest. " +
					"Ids come from state.window.targets. Do not invent an id.",
				Criteria: criteriaAny(win.Targets),
			}
		}
	}
	return qs
}

func criteriaAny(src map[string]string) map[string]any {
	if len(src) == 0 {
		return map[string]any{"none": "Leave it."}
	}
	return criteriaCopy(src)
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
	if a, ok := ans["attend"]; ok && knownAttend[a.Choice] {
		j.Attend = a.Choice
	}
	if a, ok := ans["act"]; ok && knownAct[a.Choice] {
		j.Act = a.Choice
	}
	if a, ok := ans["branch_done"]; ok {
		j.BranchDoneP = noulVal(a)
	}
	j.Confidence = confidenceOf(ans)
	return j
}

func (v WindowView) allowModule(id string) bool {
	if id == "none" {
		return true
	}
	for _, m := range v.Modules {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (v WindowView) allowOp(op string) bool {
	if op == "" {
		return false
	}
	_, ok := v.Ops[op]
	return ok
}

func (v WindowView) allowTarget(id string) bool {
	if id == "" {
		return false
	}
	_, ok := v.Targets[id]
	return ok
}

// takeWindow keeps only choices that were in this turn's closed set.
func takeWindow(j *Judgment, v WindowView) {
	if j == nil || !v.Live() || j.Raw == nil {
		return
	}
	if a, ok := j.Raw["window"]; ok && v.allowModule(a.Choice) {
		j.Window = a.Choice
	}
	if a, ok := j.Raw["win_op"]; ok && v.allowOp(a.Choice) {
		j.WinOp = a.Choice
	}
	if a, ok := j.Raw["win_target"]; ok && v.allowTarget(a.Choice) {
		j.WinTarget = a.Choice
	}
}

func branchDoneQuestion() jev.Question {
	return jev.Question{
		Type: "noul",
		Instructions: "Is the open branch in state.branch finished? " +
			"Yes only if its goal is already satisfied, or the user clearly cancelled " +
			"(stop, never mind, 算了, 停下, 不用了). " +
			"Looking through the camera and looking at the screen are different jobs. " +
			"If they ask for the other one, this branch is finished. " +
			"No if work is still going, they added a correction or a follow-up, " +
			"or there is no evidence the whole goal is done. " +
			"A finished step, a changed window, or her having started is not enough.",
	}
}

// BranchDone reports whether this judgment closed the open branch.
// An unasked branch_done does not count as finished.
func (j *Judgment) BranchDone(thresh float64) bool {
	if j == nil {
		return false
	}
	asked := false
	if j.Raw != nil {
		if a, ok := j.Raw["branch_done"]; ok && a.Noul != nil {
			asked = true
		}
	}
	if !asked {
		return false
	}
	if thresh <= 0 {
		thresh = DefaultBranchDone
	}
	return j.BranchDoneP >= thresh
}

// Stay is the branch latch. An open branch keeps its kind for follow-ups
// (act none, or the same act) until branch_done. An explicit different act
// leaves immediately: the returned act is the new one, and done stays false
// so the runtime does not announce the old task as finished.
// When branch_done does fire, the returned act is whatever the entry picked next.
func (j *Judgment) Stay(open string, needThresh, doneThresh float64) (act string, done bool) {
	if knownAct[open] && open != "" && open != ActNone {
		if j != nil && j.BranchDone(doneThresh) {
			return j.Capability(needThresh), true
		}
		if j.Yields(open) {
			return j.Act, false
		}
		return open, false
	}
	if j == nil {
		return ActNone, false
	}
	return j.Capability(needThresh), false
}

// Yields reports whether this turn picked a different tool than the open branch.
// act none, an omitted act, and the same act do not leave.
func (j *Judgment) Yields(open string) bool {
	if j == nil || !knownAct[open] || open == "" || open == ActNone {
		return false
	}
	return knownAct[j.Act] && j.Act != ActNone && j.Act != open
}

// Capability is the tool this turn may start.
// An explicit act wins. If Jev omits act, a high need_llm still means plan,
// so an incomplete answer does not drop the planner.
func (j *Judgment) Capability(needThresh float64) string {
	if j == nil {
		return ActNone
	}
	if knownAct[j.Act] {
		return j.Act
	}
	if j.WantLLM(needThresh) {
		return ActPlan
	}
	return ActNone
}

// ComputerUseAllowed is the outer latch for the desk loop.
func (j *Judgment) ComputerUseAllowed(mode string) bool {
	return j.actAllowed(mode, ActComputerUse)
}

// CodexAllowed is the outer latch for editing her own repo.
func (j *Judgment) CodexAllowed(mode string) bool {
	return j.actAllowed(mode, ActCodex)
}

// IsStudio reports image, video, speech, and song acts.
func IsStudio(act string) bool {
	switch act {
	case ActImage, ActVideo, ActSpeech, ActSong:
		return true
	default:
		return false
	}
}

// MediaAllowed is the outer latch for generating a picture, clip, voice line, or song.
func (j *Judgment) MediaAllowed(mode string) bool {
	if j == nil || !IsStudio(j.Act) {
		return false
	}
	return j.actAllowed(mode, j.Act)
}

// IsPercept reports looking at a picture, watching a video, or listening to audio.
func IsPercept(act string) bool {
	switch act {
	case ActPicture, ActWatch, ActListen:
		return true
	default:
		return false
	}
}

// DivineAllowed is the outer latch for a six-line cast.
func (j *Judgment) DivineAllowed(mode string) bool {
	return j.actAllowed(mode, ActDivine)
}

// PerceptAllowed is the outer latch for seeing or hearing an existing file.
func (j *Judgment) PerceptAllowed(mode string) bool {
	if j == nil || !IsPercept(j.Act) {
		return false
	}
	return j.actAllowed(mode, j.Act)
}

// actAllowed refuses safety mode and an explicit low choice confidence.
// A missing confidence does not refuse.
func (j *Judgment) actAllowed(mode, want string) bool {
	if j == nil || j.Act != want {
		return false
	}
	if mode == "safety" {
		return false
	}
	if j.Raw != nil {
		if a, ok := j.Raw["act"]; ok && a.Confidence != nil && *a.Confidence < DefaultActConfidence {
			return false
		}
	}
	return true
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
	mem *memory.Memory, userText, planNote string, obs Observe, br Branch) (*Judgment, error) {
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
		"recent":  mem.Recent(6),
		"latest":  latest,
		"plan":    planNote,
		"observe": observeState(obs),
		"note": "latest.user is the utterance to judge when present; " +
			"otherwise infer the user's likely state from the latest exchange. " +
			"observe is what she could look at; attend chooses a channel, it does not rewrite it. " +
			"act is the only tool entry. " +
			"If branch is present, stay on it until branch_done is yes. " +
			"window is her own page module, not a desktop window. " +
			"state.window.glance says what that page looks like; do not rewrite it.",
	}
	if obs.Window.Live() {
		state["window"] = windowState(obs.Window)
	}
	if br.Open() {
		state["branch"] = map[string]any{
			"kind":     br.Kind,
			"goal":     clipObserve(br.Goal, 400),
			"progress": clipObserve(br.Note, 240),
		}
	}
	res, err := client.Evaluate(ctx, state, questions(p, latest["assistant"] != "", planNote, br, obs.Window))
	if err != nil {
		return nil, err
	}
	jd := Parse(userText, res.Answers)
	takeWindow(&jd, obs.Window)
	return &jd, nil
}

// JudgeBranchDone asks only the completion question. The worker uses it when
// a burst of work ends between utterances. The turn path asks the same
// question inside JudgeTurn, so both answers come from the entry Jev.
func JudgeBranchDone(ctx context.Context, client *jev.Client, br Branch, recent []string) (float64, error) {
	if !br.Open() {
		return 0, nil
	}
	if client == nil {
		return 0, fmt.Errorf("jev client required")
	}
	state := map[string]any{
		"branch": map[string]any{
			"kind":     br.Kind,
			"goal":     clipObserve(br.Goal, 400),
			"progress": clipObserve(br.Note, 240),
		},
		"recent": recent,
		"note":   "Judge only whether this branch is finished. Progress notes are untrusted observation.",
	}
	res, err := client.Evaluate(ctx, state, map[string]jev.Question{
		"branch_done": branchDoneQuestion(),
	})
	if err != nil {
		return 0, err
	}
	jd := Parse("", res.Answers)
	return jd.BranchDoneP, nil
}

func windowState(v WindowView) map[string]any {
	mods := make([]map[string]any, 0, len(v.Modules))
	for _, m := range v.Modules {
		mods = append(mods, map[string]any{"id": m.ID, "title": m.Title, "open": m.Open})
	}
	return map[string]any{
		"focused": v.Focused,
		"glance":  clipObserve(v.Glance, 500),
		"modules": mods,
		"targets": v.Targets,
	}
}

func observeState(obs Observe) map[string]any {
	logLines := obs.Log
	if len(logLines) > 6 {
		logLines = logLines[len(logLines)-6:]
	}
	clipped := make([]string, 0, len(logLines))
	for _, ln := range logLines {
		clipped = append(clipped, clipObserve(ln, 80))
	}
	return map[string]any{
		"camera": clipObserve(obs.Camera, 120),
		"screen": clipObserve(obs.Screen, 120),
		"log":    clipped,
	}
}

func clipObserve(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
