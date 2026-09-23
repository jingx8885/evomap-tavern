package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/livevoice"
	"github.com/jingx8885/lov-evo/internal/llm"
	"github.com/jingx8885/lov-evo/internal/planner"
	"github.com/jingx8885/lov-evo/internal/sense"
	"github.com/jingx8885/lov-evo/internal/studio"
	"github.com/jingx8885/lov-evo/internal/window"
)

type voiceHold struct {
	mu   sync.Mutex
	sess *livevoice.Session
}

func (h *voiceHold) bind(s *livevoice.Session) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.sess = s
	h.mu.Unlock()
}

// live returns the bound session, or fallback when none is bound.
// Async work outlives a reconnect and must reach the new call.
func (h *voiceHold) live(fallback *livevoice.Session) *livevoice.Session {
	if h == nil {
		return fallback
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess != nil {
		return h.sess
	}
	return fallback
}

func (h *voiceHold) nudge(opt Options, text string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	s := h.sess
	h.mu.Unlock()
	if s == nil {
		return
	}
	voiceNudge(opt, s, text)
}

func mediaDir(opt Options) string {
	dir := opt.RunsDir
	if dir == "" {
		dir = "runs"
	}
	return filepath.Join(dir, "media")
}

func windowView(s window.Spec) judge.WindowView {
	v := judge.WindowView{
		Focused: s.Focused,
		Glance:  s.Glance,
		Ops:     s.Ops,
	}
	for _, m := range s.Modules {
		v.Modules = append(v.Modules, judge.WindowMod{ID: m.ID, Title: m.Title, Open: m.Open})
	}
	if len(s.Targets) > 0 {
		v.Targets = map[string]string{}
		for _, t := range s.Targets {
			v.Targets[t.ID] = t.Label
		}
	}
	return v
}

func rememberStage(opt Options) {
	if opt.deskWin == nil || opt.sense == nil {
		return
	}
	glance := opt.deskWin.Glance()
	open := opt.deskWin.Open("stage")
	layout := ""
	var latest window.Job
	if opt.stageQ != nil {
		layout = opt.stageQ.Layout()
		if jobs := opt.stageQ.Snapshot(); len(jobs) > 0 {
			latest = jobs[len(jobs)-1]
		}
	}
	opt.sense.Set(func(l *sense.Live) {
		l.StageGlance = glance
		l.StageOpen = open
		l.StageLayout = layout
		if latest.Kind != "" {
			l.MakeKind = latest.Kind
			l.MakeStatus = latest.Status
			l.MakeProgress = latest.Ratio
			l.MakeFile = latest.File
		}
	})
}

func stageRunner(base, key, dir string, lc *llm.Client) window.Runner {
	return func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
		if kind == window.KindLLM {
			if lc == nil {
				return "", "", fmt.Errorf("llm client required")
			}
			report("thinking", 0.4)
			raw, err := lc.ChatComplete(ctx,
				"You write one private note for a voice companion's board. "+
					"Output STRICT JSON only: {\"text\":\"...\"}. "+
					"Two or three sentences. Not a line to speak aloud.",
				prompt)
			if err != nil {
				return "", "", err
			}
			report("ready", 1)
			return "", parseBoardNote(raw), nil
		}
		client := studio.New(base, key, dir)
		client.OnProgress = func(p studio.Progress) { report(p.Status, p.Ratio) }
		res, err := client.Run(ctx, kind, prompt)
		if err != nil {
			return "", "", err
		}
		file := ""
		if len(res.Files) > 0 {
			file = res.Files[0]
		}
		return file, "", nil
	}
}

func startQueued(ctx context.Context, opt Options, pl *planner.Planner, lc *llm.Client,
	sess *livevoice.Session, slot *capabilitySlot, kind string, continuing bool) {
	if opt.stageQ == nil || continuing {
		return
	}
	jobKind := kind
	if kind == judge.ActPlan {
		jobKind = window.KindLLM
	}
	_, _, note := slot.current()
	if strings.HasPrefix(note, jobKind+" queued") {
		return
	}
	slot.setNote(jobKind + " queued")
	go runQueued(ctx, opt, pl, lc, sess, slot, kind, jobKind)
}

func runQueued(ctx context.Context, opt Options, pl *planner.Planner, lc *llm.Client,
	sess *livevoice.Session, slot *capabilitySlot, branchKind, jobKind string) {
	_, goal, _ := slot.current()
	goal = strings.TrimSpace(goal)
	if goal == "" || opt.stageQ == nil {
		slot.closeIf(branchKind)
		return
	}
	prompt := goal
	if jobKind != window.KindLLM {
		var completer studio.Completer
		if lc != nil {
			completer = lc
		}
		prompt = studio.Compose(ctx, jobKind, goal, completer)
	}
	job := opt.stageQ.Enqueue(jobKind, prompt)
	slot.setNote(jobKind + " queued " + job.ID)
	openStage(opt)
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindStage, Summary: jobKind + " queued " + job.ID})
	}
	voiceNudge(opt, sess, "You just started a "+jobKind+". Tell them in one short in-character line that it has begun. Do not say it is ready. Do not describe a finished result.")
	done, err := opt.stageQ.Wait(ctx, job.ID)
	if err != nil {
		return
	}
	if done.Status == window.StatusReady && jobKind == window.KindImage && done.File != "" && lc != nil {
		done.Look = captionStageImage(opt, lc, done)
	}
	if jobKind == window.KindLLM && pl != nil && strings.TrimSpace(done.Text) != "" {
		pl.SetNote(done.Text)
		if opt.sense != nil {
			opt.sense.Set(func(l *sense.Live) { l.Plan = done.Text })
		}
	}
	rememberStage(opt)
	switch done.Status {
	case window.StatusReady:
		opt.voice.nudge(opt, readyLine(jobKind, done))
	case window.StatusFailed:
		opt.voice.nudge(opt, "That did not finish. Say so simply, in character. Do not invent a file or a picture.")
	}
	slot.closeIf(branchKind)
}

func readyLine(kind string, job window.Job) string {
	switch kind {
	case window.KindLLM:
		return "A note is on the stage window. Tell them you have it, in one or two in-character sentences. Do not read the note aloud as a list. Note: " + clip(job.Text, 240)
	case window.KindImage:
		line := "The picture is ready and on the stage window. Tell them in one or two in-character sentences."
		if job.Look != "" {
			line += " What is in the picture: " + clip(job.Look, 240) + "."
		}
		line += " Do not add details that are not written here."
		return line
	default:
		file := job.File
		if file == "" {
			file = kind
		}
		return "The " + kind + " is ready, saved as " + file + ", and it is on the stage window. Tell them in one or two in-character sentences. Do not invent details that were not asked for."
	}
}

func captionStageImage(opt Options, lc *llm.Client, job window.Job) string {
	path := filepath.Join(mediaDir(opt), job.File)
	client := studio.New(opt.BaseURL, opt.APIKey, mediaDir(opt))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	got, err := client.Perceive(ctx, studio.KindPicture, path, lc)
	if err != nil || opt.stageQ == nil {
		return ""
	}
	opt.stageQ.SetLook(job.ID, got.Text)
	return got.Text
}

func dressStage(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, ask, op string) {
	if opt.stageQ == nil {
		return
	}
	openStage(opt)
	voiceNudge(opt, sess, "You are changing the stage page. Say in one short in-character line that you have started. Do not describe the finished look yet.")
	var err error
	switch op {
	case window.OpDecorate:
		err = applyThemeDraft(ctx, opt, lc, ask)
	case window.OpAddControl:
		err = applyControlDraft(ctx, opt, lc, ask)
	default:
		return
	}
	if err != nil {
		opt.log("[window] %s: %v", op, err)
		opt.voice.nudge(opt, "The page did not change. Say so simply, in character. Do not invent a new color or a new control.")
		return
	}
	rememberStage(opt)
	glance := ""
	if opt.deskWin != nil {
		glance = opt.deskWin.Glance()
	}
	opt.voice.nudge(opt, "The stage page changed. Describe only what this says it looks like, in one or two in-character sentences: "+clip(glance, 320))
}

func applyThemeDraft(ctx context.Context, opt Options, lc *llm.Client, ask string) error {
	if lc == nil {
		return fmt.Errorf("llm client required")
	}
	cur, _ := json.Marshal(opt.stageQ.Theme())
	raw, err := lc.ChatComplete(ctx,
		"You restyle one private page for a voice companion. "+
			"Output STRICT JSON only with these fields: "+
			"background, desk, ink, muted, accent, card, line as #rrggbb colors; "+
			"font is sans, serif, or mono; radius is tight, soft, or round; frame is plain, line, or glow. "+
			"Keep contrast readable. Do not add other fields.",
		"Current theme: "+string(cur)+"\nWhat they asked: "+clip(ask, 400))
	if err != nil {
		return err
	}
	theme, err := window.ParseTheme(raw)
	if err != nil {
		return err
	}
	return opt.stageQ.SetTheme(theme)
}

func applyControlDraft(ctx context.Context, opt Options, lc *llm.Client, ask string) error {
	if lc == nil {
		return fmt.Errorf("llm client required")
	}
	raw, err := lc.ChatComplete(ctx,
		"You add a few controls to one private page. "+
			"Output STRICT JSON only: {\"controls\":[{\"id\":\"c1\",\"kind\":\"label\",\"text\":\"...\"}]}. "+
			"kind is label, note, chip, rule, or meter. "+
			"meter also has value from 0 to 1. rule may omit text. "+
			"At most four controls. Plain text only. No HTML.",
		clip(ask, 400))
	if err != nil {
		return err
	}
	list, err := window.ParseControls(raw)
	if err != nil {
		return err
	}
	return opt.stageQ.AddControls(list)
}

func openStage(opt Options) {
	if opt.deskWin == nil {
		return
	}
	if _, err := opt.deskWin.Apply("stage", window.OpOpen, ""); err != nil {
		opt.log("[window] open: %v", err)
		return
	}
	rememberStage(opt)
	if url := opt.deskWin.URL(); url != "" {
		opt.log("[window] open %s", url)
	}
}

// parseStageCommand splits `/stage ...`. ok is false when the line is not /stage.
func parseStageCommand(line string) (op, arg string, ok bool) {
	line = strings.TrimSpace(line)
	if line != "/stage" && !strings.HasPrefix(line, "/stage ") {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "/stage"))
	if rest == "" {
		return "open", "", true
	}
	op, arg, _ = strings.Cut(rest, " ")
	op = strings.ToLower(strings.TrimSpace(op))
	arg = strings.TrimSpace(arg)
	switch op {
	case "open", "hide", "clear", "image", "video", "speech", "song", "llm", "note", "decorate", "control", "layout":
		return op, arg, true
	default:
		return op, arg, true
	}
}

func stageCommand(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, line string) {
	op, arg, ok := parseStageCommand(line)
	if !ok {
		return
	}
	if opt.stageQ == nil || opt.deskWin == nil {
		opt.log("stage unavailable")
		return
	}
	switch op {
	case "open":
		openStage(opt)
		opt.log("[stage] %s", clip(opt.deskWin.Glance(), 240))
	case "hide":
		if _, err := opt.deskWin.Apply("stage", window.OpHide, ""); err != nil {
			opt.log("[stage] %v", err)
			return
		}
		rememberStage(opt)
		opt.log("[stage] covered")
	case "clear":
		if _, err := opt.deskWin.Apply("stage", window.OpClearControls, ""); err != nil {
			opt.log("[stage] %v", err)
			return
		}
		rememberStage(opt)
		opt.log("[stage] controls cleared")
	case "layout":
		winOp := map[string]string{
			"queue": window.OpLayoutQueue,
			"stage": window.OpLayoutStage,
			"split": window.OpLayoutSplit,
		}[strings.ToLower(arg)]
		if winOp == "" {
			opt.log("stage layout is queue, stage, or split")
			return
		}
		openStage(opt)
		if _, err := opt.deskWin.Apply("stage", winOp, ""); err != nil {
			opt.log("[stage] %v", err)
			return
		}
		rememberStage(opt)
		opt.log("[stage] layout %s", arg)
	case "image", "video", "speech", "song", "llm", "note":
		if arg == "" {
			opt.log("stage %s needs a prompt", op)
			return
		}
		kind := op
		if op == "note" {
			kind = window.KindLLM
		}
		openStage(opt)
		opt.log("[stage] queue %s", kind)
		go runStageJob(ctx, opt, lc, sess, kind, arg)
	case "decorate":
		if arg == "" {
			opt.log("stage decorate needs a description")
			return
		}
		openStage(opt)
		opt.log("[stage] decorate")
		go dressStage(ctx, opt, lc, sess, arg, window.OpDecorate)
	case "control":
		if arg == "" {
			opt.log("stage control needs a description")
			return
		}
		openStage(opt)
		opt.log("[stage] control")
		go dressStage(ctx, opt, lc, sess, arg, window.OpAddControl)
	default:
		opt.log("stage: open | hide | image <prompt> | video <prompt> | speech <prompt> | song <prompt> | llm <prompt> | decorate <look> | control <widgets> | clear | layout queue|stage|split")
	}
}

func runStageJob(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, kind, prompt string) {
	if opt.stageQ == nil {
		return
	}
	made := prompt
	if kind != window.KindLLM {
		var completer studio.Completer
		if lc != nil {
			completer = lc
		}
		made = studio.Compose(ctx, kind, prompt, completer)
	}
	job := opt.stageQ.Enqueue(kind, made)
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindStage, Summary: kind + " queued " + job.ID})
	}
	voiceNudge(opt, sess, "You just started a "+kind+". Tell them in one short in-character line that it has begun. Do not say it is ready.")
	done, err := opt.stageQ.Wait(ctx, job.ID)
	if err != nil {
		return
	}
	if done.Status == window.StatusReady && kind == window.KindImage && done.File != "" && lc != nil {
		done.Look = captionStageImage(opt, lc, done)
	}
	rememberStage(opt)
	switch done.Status {
	case window.StatusReady:
		opt.voice.nudge(opt, readyLine(kind, done))
	case window.StatusFailed:
		opt.voice.nudge(opt, "That did not finish. Say so simply, in character. Do not invent a file or a picture.")
	}
}

func parseBoardNote(raw string) string {
	raw = strings.TrimSpace(raw)
	s := raw
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil || strings.TrimSpace(out.Text) == "" {
		return clip(raw, 400)
	}
	return clip(strings.TrimSpace(out.Text), 400)
}
