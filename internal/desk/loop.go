package desk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/config"
)

// Run asks Jev for one operation per step, then executes it on the host.
// Driver "codex" skips Jev and runs Codex as gpt-5.6-luna (or CodexModel).
func Run(ctx context.Context, opt Options) (*Report, error) {
	goal := strings.TrimSpace(opt.Goal)
	if goal == "" {
		return nil, fmt.Errorf("goal required")
	}
	if strings.EqualFold(strings.TrimSpace(opt.Driver), "codex") {
		return runCodexDirect(ctx, opt)
	}
	if opt.Jev == nil {
		return nil, fmt.Errorf("jev client required")
	}
	host := hostOf(opt)
	max := opt.MaxSteps
	if max <= 0 {
		max = 8
	}
	wait := opt.Wait
	if wait <= 0 {
		wait = 700 * time.Millisecond
	}
	cwd := resolveCwd(opt.Cwd)
	rep := &Report{Goal: goal, Cwd: cwd, Status: OpBlocked}

	var history []Step
	waits := 0
	same := 0
	var lastOp string
	for i := 0; i < max; i++ {
		if err := ctx.Err(); err != nil {
			rep.Status = "canceled"
			return rep, err
		}
		snap, err := host.Snapshot(cwd)
		if err != nil {
			opt.log("snapshot: %v", err)
			snap = Snapshot{Cwd: cwd, Tools: DetectTools()}
		}
		d, err := decide(ctx, opt.Jev, goal, opt.Prefer, snap, history)
		if err != nil {
			return rep, err
		}
		step := Step{N: i + 1, Op: d.Op, SafeP: d.SafeP, DoneP: d.DoneP, At: time.Now()}
		t0 := time.Now()

		if d.DoneP >= doneThreshold {
			step.Op = OpDone
			step.Detail = "goal_achieved noul above threshold"
			step.ElapsedMS = time.Since(t0).Milliseconds()
			history = append(history, step)
			rep.Steps = history
			rep.Status = OpDone
			opt.log("[%d] done (p=%.2f)", step.N, d.DoneP)
			return rep, nil
		}
		if d.Op == OpDone {
			step.Detail = "jev chose done"
			step.ElapsedMS = time.Since(t0).Milliseconds()
			history = append(history, step)
			rep.Steps = history
			rep.Status = OpDone
			opt.log("[%d] done", step.N)
			return rep, nil
		}
		if d.Op == OpBlocked || d.Confidence < minConfidence {
			step.Op = OpBlocked
			step.Detail = fmt.Sprintf("blocked conf=%.2f", d.Confidence)
			step.ElapsedMS = time.Since(t0).Milliseconds()
			history = append(history, step)
			rep.Steps = history
			rep.Status = OpBlocked
			opt.log("[%d] blocked conf=%.2f", step.N, d.Confidence)
			return rep, nil
		}
		if d.SafeP < minSafeP {
			step.Op = OpBlocked
			step.Skipped = true
			step.Detail = fmt.Sprintf("unsafe safe_p=%.2f", d.SafeP)
			step.ElapsedMS = time.Since(t0).Milliseconds()
			history = append(history, step)
			rep.Steps = history
			rep.Status = OpBlocked
			opt.log("[%d] skipped unsafe p=%.2f", step.N, d.SafeP)
			return rep, nil
		}
		if d.Op == lastOp {
			same++
			if same >= 3 {
				step.Op = OpBlocked
				step.Detail = "repeated the same operation"
				step.ElapsedMS = time.Since(t0).Milliseconds()
				history = append(history, step)
				rep.Steps = history
				rep.Status = OpBlocked
				return rep, nil
			}
		} else {
			same = 0
			lastOp = d.Op
		}

		text, out, detail, execErr := apply(ctx, opt, host, snap, d, wait)
		step.Text = text
		step.Output = out
		step.Detail = detail
		if execErr != nil {
			step.Err = execErr.Error()
			opt.log("[%d] %s failed: %v", step.N, d.Op, execErr)
		} else {
			opt.log("[%d] %s %s", step.N, d.Op, clip(detail, 160))
		}
		if opt.DryRun {
			step.Skipped = true
			if step.Detail == "" {
				step.Detail = "dry-run"
			}
		}
		step.ElapsedMS = time.Since(t0).Milliseconds()
		history = append(history, step)
		rep.Steps = history

		if d.Op == OpWait {
			waits++
			if waits >= 3 {
				rep.Status = OpBlocked
				return rep, nil
			}
		} else {
			waits = 0
		}
	}
	rep.Status = "max_steps"
	return rep, nil
}

func apply(ctx context.Context, opt Options, host Host, snap Snapshot, d Decision, wait time.Duration) (text, out, detail string, err error) {
	if opt.DryRun {
		return "", "", dryDetail(d, opt), nil
	}
	switch d.Op {
	case OpWait:
		select {
		case <-ctx.Done():
			return "", "", "wait", ctx.Err()
		case <-time.After(wait):
			return "", "", "wait", nil
		}
	case OpFocusWindow:
		win, ok := snap.windowByID(d.WindowID)
		if !ok {
			return "", "", "", fmt.Errorf("unknown window %q", d.WindowID)
		}
		return "", "", win.Process + " · " + clip(win.Title, 60), host.Activate(win)
	case OpLaunchApp:
		app := d.App
		if app == "" {
			return "", "", "", fmt.Errorf("launch_app missing app_target")
		}
		return "", "", app, host.Launch(app, snap.Cwd)
	case OpHotkey:
		if d.Hotkey == "" {
			return "", "", "", fmt.Errorf("hotkey missing hotkey_target")
		}
		return "", "", d.Hotkey, host.Hotkey(d.Hotkey)
	case OpTypeText:
		text, err = generate(ctx, opt.LLM, typeSystem, typeUser(opt.Goal, snap, opt.Prefer))
		if err != nil {
			return "", "", "", err
		}
		if text == "" {
			return "", "", "", fmt.Errorf("llm returned empty text")
		}
		return text, "", clip(text, 80), host.Paste(text)
	case OpCursorDev:
		text, err = generate(ctx, opt.LLM, coderSystem, coderUser(opt.Goal, snap, "cursor"))
		if err != nil {
			return "", "", "", err
		}
		if text == "" {
			text = opt.Goal
		}
		if err := host.OpenCursor(snap.Cwd); err != nil {
			return text, "", "open cursor", err
		}
		time.Sleep(1100 * time.Millisecond)
		live, _ := host.Snapshot(snap.Cwd)
		if win, ok := live.firstProcess("cursor"); ok {
			_ = host.Activate(win)
			time.Sleep(250 * time.Millisecond)
		}
		if err := host.Hotkey("ctrl_l"); err != nil {
			return text, "", "opened cursor; chat hotkey failed", err
		}
		time.Sleep(350 * time.Millisecond)
		if err := host.Paste(text); err != nil {
			return text, "", "opened cursor chat; paste failed", err
		}
		_ = host.Hotkey("enter")
		return text, "", "cursor chat", nil
	case OpCodexDev:
		text = opt.Goal
		out, err = host.RunCodex(ctx, snap.Cwd, text)
		return text, out, "codex exec -m " + codexModel(opt), err
	default:
		return "", "", "", fmt.Errorf("unsupported op %q", d.Op)
	}
}

func generate(ctx context.Context, llm Completer, system, user string) (string, error) {
	if llm == nil {
		return "", fmt.Errorf("llm client required for this operation")
	}
	raw, err := llm.ChatComplete(ctx, system, user)
	if err != nil {
		return "", err
	}
	return parseTextJSON(raw), nil
}

func dryDetail(d Decision, opt Options) string {
	switch d.Op {
	case OpFocusWindow:
		return "would focus " + d.WindowID
	case OpLaunchApp:
		return "would launch " + d.App
	case OpHotkey:
		return "would send " + d.Hotkey
	case OpCursorDev:
		return "would open Cursor and paste a prompt"
	case OpCodexDev:
		return "would run codex exec -m " + codexModel(opt)
	case OpTypeText:
		return "would paste generated text"
	default:
		return d.Op
	}
}

func hostOf(opt Options) Host {
	if opt.Host != nil {
		return opt.Host
	}
	return DefaultHost{CodexModel: codexModel(opt), APIKey: opt.APIKey}
}

func codexModel(opt Options) string {
	if m := strings.TrimSpace(opt.CodexModel); m != "" {
		return m
	}
	return config.DefaultPlannerModel
}

func runCodexDirect(ctx context.Context, opt Options) (*Report, error) {
	goal := strings.TrimSpace(opt.Goal)
	cwd := resolveCwd(opt.Cwd)
	model := codexModel(opt)
	host := hostOf(opt)
	rep := &Report{Goal: goal, Cwd: cwd, Status: OpCodexDev}
	step := Step{
		N: 1, Op: OpCodexDev, Text: goal,
		Detail: "codex exec -m " + model,
		At:     time.Now(),
	}
	t0 := time.Now()
	opt.log("codex model=%s cwd=%s", model, cwd)
	if opt.DryRun {
		step.Skipped = true
		step.Detail = "would run codex exec -m " + model
		step.ElapsedMS = time.Since(t0).Milliseconds()
		rep.Steps = []Step{step}
		rep.Status = OpDone
		return rep, nil
	}
	out, err := host.RunCodex(ctx, cwd, goal)
	step.Output = out
	if err != nil {
		step.Err = err.Error()
		rep.Status = OpBlocked
		opt.log("codex failed: %v", err)
	} else {
		rep.Status = OpDone
		opt.log("codex done")
	}
	step.ElapsedMS = time.Since(t0).Milliseconds()
	rep.Steps = []Step{step}
	return rep, err
}

func (o Options) log(format string, args ...any) {
	if o.LogFn == nil {
		return
	}
	o.LogFn(fmt.Sprintf(format, args...))
}

const coderSystem = `Return a JSON object with exactly one key, text: the exact prompt to send to a local coding agent.
The agent will edit the git repo at cwd. Include the user's goal and that it should continue implementation, not explain.
No markdown fences, no commentary. If a required value is missing return {"text": null}.`

const typeSystem = `Return a JSON object with exactly one key, text: the exact string to paste into the focused window.
Infer it from the user's goal. No commentary. Never invent passwords or personal data.
If a required value is missing return {"text": null}.`

func coderUser(goal string, snap Snapshot, driver string) string {
	return fmt.Sprintf("driver=%s\ncwd=%s\ngoal=%s\nforeground=%s",
		driver, snap.Cwd, goal, clip(snap.ForegroundTitle, 80))
}

func typeUser(goal string, snap Snapshot, prefer string) string {
	return fmt.Sprintf("prefer=%s\ncwd=%s\ngoal=%s\nforeground=%s",
		prefer, snap.Cwd, goal, clip(snap.ForegroundTitle, 80))
}
