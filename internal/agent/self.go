package agent

import (
	"context"
	"strings"

	"github.com/jingx8885/lov-evo/internal/desk"
	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/livevoice"
	"github.com/jingx8885/lov-evo/internal/llm"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/react"
	"github.com/jingx8885/lov-evo/internal/sense"
)

// repoCoder is the Codex CLI behind one already-checked self intent.
type repoCoder struct {
	model string
	key   string
}

func (c repoCoder) Edit(ctx context.Context, cwd, prompt string) (string, error) {
	h := desk.DefaultHost{CodexModel: c.model, APIKey: c.key}
	return h.RunCodex(ctx, cwd, prompt)
}

// ensureSelf runs one ReAct stretch on reflect, look, or codex.
// The inner Jev picks notice, read, remember, or one allowlisted change.
// The voice path is not blocked. A follow-up on the same branch runs another stretch.
func ensureSelf(ctx context.Context, opt Options, p *persona.Persona, jc *jev.Client, lc *llm.Client,
	sess *livevoice.Session, slot *capabilitySlot, jd *judge.Judgment, act, mode string) {
	if opt.sense == nil || p == nil || !p.Sense.Enabled {
		opt.log("[self] %s unavailable", act)
		return
	}
	if slot.busy() {
		opt.log("[self] %s still working", act)
		return
	}
	ok, gen := slot.tryComputer()
	if !ok {
		opt.log("[self] %s busy", act)
		return
	}
	switch act {
	case judge.ActReflect:
		opt.sense.Emit(sense.Event{Kind: sense.KindLook, Summary: "reflect"})
	case judge.ActLook:
		opt.sense.Emit(sense.Event{Kind: sense.KindLook, Summary: "look"})
	case judge.ActCodex:
		opt.sense.Emit(sense.Event{Kind: sense.KindDesk, Summary: "codex start"})
	}
	_, goal, _ := slot.current()
	canChange := mode != "safety" && (act == judge.ActCodex || react.WantsChange(goal))
	selfEmotion, userEmotion := "", ""
	if jd != nil {
		selfEmotion = jd.SelfEmotion
		userEmotion = jd.Emotion
	}
	var ev react.Evaluator
	if jc != nil {
		ev = jc
	}
	var completer react.Completer
	if lc != nil {
		completer = lc
	}
	var coder react.Coder
	if canChange {
		model := opt.PlannerModel
		if model == "" {
			model = "gpt-5.6-luna"
		}
		coder = repoCoder{model: model, key: opt.APIKey}
	}
	go func() {
		defer slot.endComputer(gen)
		ctx = slot.bind(ctx)
		last := opt.sense.Live().SelfNote
		rep, err := react.Run(ctx, react.Options{
			Kind:        act,
			Goal:        goal,
			LastNote:    last,
			Mode:        mode,
			SelfEmotion: selfEmotion,
			UserEmotion: userEmotion,
			CanChange:   canChange,
			Bus:         opt.sense,
			Memory:      opt.rel,
			Jev:         ev,
			LLM:         completer,
			Coder:       coder,
			LogFn:       func(s string) { opt.log("[self] %s", s) },
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil && (rep == nil || strings.TrimSpace(rep.Note) == "") {
			opt.log("[self] %s failed: %v", act, err)
			voiceNudge(opt, sess, "You could not do that to yourself. Say so simply. Do not invent a change.")
			return
		}
		if rep == nil || len(rep.Steps) == 0 {
			return
		}
		slot.setNote(clip(rep.Note, 200))
		opt.log("[self] %s status=%s changed=%v steps=%s", act, rep.Status, rep.Changed, stepNames(rep))
		note := strings.TrimSpace(rep.Note)
		if act == judge.ActCodex && !rep.Changed {
			note = "Codex did not run this time. No file was written and nothing is queued."
		}
		if note != "" {
			voiceSteer(opt, sess, note)
		}
		voiceNudge(opt, sess, selfSpokeFor(act, rep))
	}()
}

func stepNames(rep *react.Report) string {
	names := make([]string, 0, len(rep.Steps))
	for _, s := range rep.Steps {
		names = append(names, s.Step)
	}
	return strings.Join(names, ",")
}

func selfSpokeFor(act string, rep *react.Report) string {
	if act == judge.ActCodex && (rep == nil || !rep.Changed) {
		return "You did not start Codex and nothing was written. If they ask, say so plainly in character. Never claim it is running, queued, or finished."
	}
	return selfSpoke(rep)
}

func selfSpoke(rep *react.Report) string {
	if rep != nil && rep.Changed {
		return "You changed one file of your own source on disk. Say what changed in one or two in-character sentences, only from the note. The running process is still the previous build. Do not say you have reloaded."
	}
	if rep != nil && rep.Status == react.StatusBlocked {
		return "You could not do that to yourself. Say so simply, in character. Do not invent a change."
	}
	return "You noticed something about yourself. Answer from that note, in character, short. Do not recite a manual."
}
