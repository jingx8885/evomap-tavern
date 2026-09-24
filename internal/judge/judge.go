// Package judge turns each conversation turn into typed Jev judgments.
// One /v1/systemone call asks the few questions that are not the same fact:
// how they feel, how stirred up they are, how she feels, what they are
// doing, whether to keep it, and which single tool to start. Valence,
// engagement, steering mode, and which eye to show are filled in by code.
// Go applies the safety latch and drops unknown choices.
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
	"withdraw":     "Pulling away: a very short reply, trailing off, not saying goodbye.",
	"other":        "None of the above.",
}

// DefaultNeedLLM is the noul cutoff for launching the planner LLM.
const DefaultNeedLLM = 0.55

// DefaultKeep is the noul cutoff for spending a slow call to compress
// this turn into durable memory. Below it, only the local gist seeds run.
const DefaultKeep = 0.55

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
	ActShot        = "shot"
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
	KeepP        float64               `json:"keep,omitempty"`
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
	Camera     string
	Screen     string
	Log        []string
	Window     WindowView
	Remembered []string
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
// codex writes a program in a scratch dir, or edits her own repo when asked
// to change herself. computer_use is the desk loop for the machine.
var ActLabels = map[string]string{
	ActNone:        "Just talk. No tool, no desktop action, no extra look.",
	ActPlan:        "A slower written plan or decision is needed. Not a desktop action.",
	ActReflect:     "They asked her to notice herself: who she is, whether she can feel her voice, face, or mood, or a fault in her own log. Not a request to edit code, write a program, use the computer, or look at a screenshot of her appearance. A later step on this branch may read one file or remember one line.",
	ActLook:        "They asked how she is built, what her own code does, or to feel a specific file in her body. This branch keeps reading her source. Not a request to write or generate a new program, game, or script; that is codex. If they then ask her to change herself, a later step may edit one allowlisted file. It does not reload the running process.",
	ActCamera:      "They asked her to look through the camera now: at them, the room, or who is there. Not the computer screen, not a saved picture, and not a screenshot of her own face. A follow-up about that same view is not a new capability.",
	ActScreen:      "They asked her to look at the computer screen now: which window or what is visible on the desktop. Not the room camera, not a saved picture, and not a screenshot of her own face. A follow-up about that same view is not a new capability.",
	ActShot:        "They asked her to look at her own appearance now: what she looks like, her face, her clothes, her expression. One screenshot of herself on the stage. Not the room camera, not desktop window titles, not her source, and not a feeling-only check.",
	ActCodex:       "They want code written now: a new program, game, script, or tool (贪吃蛇, 小游戏, 写个程序, 用 Codex 写), or a change to her own source. A new program is written by Codex in its own fresh folder and does not read her source; a change to herself edits one allowlisted file. Not a general desktop action, and not merely talking about code.",
	ActComputerUse: "They want something done on this machine now that is not writing code: open an app, use a window, type, or act on the desktop. Writing a new program or game is codex, not this. Not mere talk about computers.",
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
	ActCamera: true, ActScreen: true, ActShot: true, ActCodex: true, ActComputerUse: true,
	ActImage: true, ActVideo: true, ActSpeech: true, ActSong: true,
	ActPicture: true, ActWatch: true, ActListen: true,
	ActDivine: true,
}

// questions builds the one-shot Jev question set for a turn.
func questions(p *persona.Persona, withPersonaFit bool, planNote string, br Branch, win WindowView) map[string]jev.Question {
	_ = planNote
	qs := map[string]jev.Question{
		"emotion": {
			Type: "choice",
			Instructions: "Which single emotion best describes the user in " +
				"section latest? This is their feeling, not the persona's.",
			Criteria: criteriaCopy(EmotionLabels),
		},
		"arousal": {
			Type: "score",
			Instructions: "How stirred up is the user in section latest? " +
				"Calm and flat at the low end, agitated or excited at the high end.",
			Levels: scoreLevels(),
		},
		"self_emotion": {
			Type: "choice",
			Instructions: "Which single emotion would the persona in state.persona " +
				"feel in response, given their style? " +
				"This is her feeling, not a copy of the user's emotion.",
			Criteria: criteriaCopy(EmotionLabels),
		},
		"intent": {
			Type: "choice",
			Instructions: "What is the user doing in section latest? " +
				"Judge the conversational move, not the emotion. " +
				"Playful complaining or tsundere sparring is banter, not comfort_seek. " +
				"A very short reply that trails off, without saying goodbye, is withdraw.",
			Criteria: criteriaCopy(IntentLabels),
		},
		"keep": {
			Type: "noul",
			Instructions: "Should this turn be written into durable memory? " +
				"Yes if section latest reveals a fact or preference about the " +
				"person, a promise, an unfinished task, a joke that might stick, " +
				"or a shared moment that changes the relationship, or if it " +
				"revises or finishes something in state.remembered. " +
				"No for greetings, backchannels (嗯/哦/好/喂), and lines that " +
				"only repeat what state.remembered already says.",
		},
		"act": {
			Type: "choice",
			Instructions: "Which single capability should the runtime start after this turn? " +
				"This is the only entry for tools. Pick exactly one. " +
				"Pick none for ordinary chat, greetings, and backchannels. " +
				"Pick plan when a slower written decision is needed (a plan, a stuck " +
				"conversation, something the live voice should not invent alone). " +
				"Pick reflect when they ask her to notice herself, her mood, or a fault in her log. " +
				"Pick look when they ask how she is built or what her own code does, not when they want a new program written. " +
				"Pick camera only when they asked her to look through the camera now. " +
				"Pick screen only when they asked her to look at the computer screen now. " +
				"Pick shot only when they asked her to look at her own appearance now. " +
				"Camera, screen, and shot are different capabilities. Do not pick one to answer the other. " +
				"Pick codex when they want code written now: a new program, game, script, or tool " +
				"(for example 用 Codex 写个贪吃蛇), or a change to her own source. Mentioning Codex with a thing to build is codex. " +
				"Pick computer_use only when they are asking her to act on this machine " +
				"now in a way that is not writing code. " +
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
			" (the screen, the camera, a screenshot of herself, a fortune, a picture, the computer, or anything else in this list)." +
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
		qs["page"] = pageQuestion(win)
	}
	return qs
}

// pageQuestion is one choice for her own page: which module, what to do,
// and which job, packed as module:op or module:op:target.
func pageQuestion(win WindowView) jev.Question {
	crit := map[string]any{
		"none": "Leave her pages alone.",
	}
	for _, m := range win.Modules {
		for op, label := range win.Ops {
			if op == "" || op == "none" || op == "feature" || op == "cancel" {
				continue
			}
			crit[m.ID+":"+op] = m.Title + ": " + label
		}
		if _, ok := win.Ops["feature"]; ok {
			for id, label := range win.Targets {
				if id == "" || id == "none" {
					continue
				}
				crit[m.ID+":feature:"+id] = m.Title + " feature " + label
			}
		}
		if _, ok := win.Ops["cancel"]; ok {
			for id, label := range win.Targets {
				if id == "" || id == "none" {
					continue
				}
				crit[m.ID+":cancel:"+id] = m.Title + " cancel " + label
			}
		}
	}
	return jev.Question{
		Type: "choice",
		Instructions: "What should happen to her own page? " +
			"Read state.window. These are her pages, not the computer's other windows. " +
			"Pick none to leave them. A value is module:op, or module:feature:job, or module:cancel:job. " +
			"Do not invent a module, an op, or a job id.",
		Criteria: crit,
	}
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
	if a, ok := ans["keep"]; ok {
		j.KeepP = noulVal(a)
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
	fillDerived(&j, ans)
	j.Confidence = confidenceOf(ans)
	return j
}

// fillDerived supplies the facts that used to be their own questions.
// An explicit answer still wins.
func fillDerived(j *Judgment, ans map[string]jev.Answer) {
	if j == nil {
		return
	}
	if _, ok := ans["valence"]; !ok {
		j.Valence = valenceFromEmotion(j.Emotion)
	}
	if _, ok := ans["engagement"]; !ok {
		j.Engagement = engagementFromIntent(j.Intent)
	}
	if _, ok := ans["need_llm"]; !ok && j.Act == ActPlan {
		j.NeedLLMP = 1
	}
	if _, ok := ans["attend"]; !ok {
		switch j.Act {
		case ActCamera:
			j.Attend = "camera"
		case ActScreen:
			j.Attend = "screen"
		}
	}
}

func valenceFromEmotion(emotion string) float64 {
	switch emotion {
	case "joy":
		return 0.85
	case "surprise":
		return 0.65
	case "fear":
		return 0.35
	case "disgust":
		return 0.32
	case "sadness":
		return 0.28
	case "anger":
		return 0.22
	default:
		return 0.55
	}
}

func engagementFromIntent(intent string) float64 {
	switch intent {
	case "withdraw":
		return 0.15
	case "goodbye":
		return 0.5
	default:
		return 0.75
	}
}

// BindLook shows the camera or the screen while that look is the open
// branch, including a follow-up whose act is none.
func (j *Judgment) BindLook(branchKind string) {
	if j == nil || (j.Attend != "" && j.Attend != "none") {
		return
	}
	kind := j.Act
	if kind == "" || kind == ActNone {
		kind = branchKind
	}
	switch kind {
	case ActCamera:
		j.Attend = "camera"
	case ActScreen:
		j.Attend = "screen"
	}
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
	if a, ok := j.Raw["page"]; ok {
		mod, op, target, ok := parsePage(a.Choice)
		if ok && (mod == "none" || v.allowModule(mod)) && (op == "" || op == "none" || v.allowOp(op)) {
			if target == "" || v.allowTarget(target) {
				j.Window = mod
				j.WinOp = op
				j.WinTarget = target
			}
		}
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

func parsePage(choice string) (module, op, target string, ok bool) {
	choice = strings.TrimSpace(choice)
	if choice == "" || choice == "none" {
		return "none", "none", "", true
	}
	parts := strings.Split(choice, ":")
	switch len(parts) {
	case 2:
		return parts[0], parts[1], "", parts[0] != "" && parts[1] != ""
	case 3:
		return parts[0], parts[1], parts[2], parts[0] != "" && parts[1] != "" && parts[2] != ""
	default:
		return "", "", "", false
	}
}

func branchDoneQuestion() jev.Question {
	return jev.Question{
		Type: "noul",
		Instructions: "Is the open branch in state.branch finished? " +
			"Yes only if its goal is already satisfied, or the user clearly cancelled " +
			"(stop, never mind, 算了, 停下, 不用了). " +
			"Looking through the camera, looking at the screen, and a screenshot of herself are different jobs. " +
			"If they ask for another one, this branch is finished. " +
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

// CodexAllowed is the outer latch for Codex: a scratch program or her own repo.
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

// WantKeep reports whether this turn should spend a slow call to write memory.
func (j *Judgment) WantKeep(thresh float64) bool {
	if j == nil {
		return false
	}
	if thresh <= 0 {
		thresh = DefaultKeep
	}
	return j.KeepP >= thresh
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
			"act is the only tool entry. plan is that act. " +
			"If branch is present, stay on it until branch_done is yes. " +
			"page is her own page module, not a desktop window. " +
			"state.window.glance says what that page looks like; do not rewrite it. " +
			"remembered is what she already keeps; keep is yes only when this turn changes that.",
	}
	if len(obs.Remembered) > 0 {
		state["remembered"] = obs.Remembered
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
