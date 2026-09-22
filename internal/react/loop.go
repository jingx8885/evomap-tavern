package react

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/sense"
)

const (
	StatusDone    = "done"
	StatusParked  = "parked"
	StatusBlocked = "blocked"

	defaultSteps = 3
)

// Completer writes the free-text intent. Jev does not.
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// Coder applies one already-checked intent to the repo.
type Coder interface {
	Edit(ctx context.Context, cwd, prompt string) (string, error)
}

// Options is one stretch of her self loop.
type Options struct {
	Kind        string
	Goal        string
	LastNote    string
	Mode        string
	SelfEmotion string
	UserEmotion string
	CanChange   bool
	MaxSteps    int
	Bus         *sense.Bus
	Memory      *memory.RelationshipStore
	Jev         Evaluator
	LLM         Completer
	Coder       Coder
	LogFn       func(string)
}

// Step is one observation she can feel.
type Step struct {
	Step string `json:"step"`
	Path string `json:"path,omitempty"`
	Note string `json:"note"`
}

// Report is the stretch she can be steered from.
type Report struct {
	Status  string
	Changed bool
	Note    string
	Steps   []Step
}

// Run is observe → one typed step → act → observe, up to a few steps.
// It does not speak. The caller steers the note.
func Run(ctx context.Context, opt Options) (*Report, error) {
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = defaultSteps
	}
	rep := &Report{Status: StatusDone}
	var history []Step
	changed := false
	for n := 0; n < opt.MaxSteps; n++ {
		if ctx.Err() != nil {
			rep.Status = StatusParked
			rep.Note = joinNotes(history)
			rep.Changed = changed
			return rep, ctx.Err()
		}
		paths := choices(opt.Bus, opt.Goal)
		d, err := decide(ctx, opt.Jev, opt, paths, history)
		if err != nil {
			d = fallback(opt, paths, history)
			opt.log("self decide failed, fallback %s: %v", d.Step, err)
		}
		if d.Step == StepDone || d.Step == "" {
			break
		}
		if seen(history, d.Step, d.Path) {
			break
		}
		note, stepChanged, err := act(ctx, opt, d, paths)
		history = append(history, Step{Step: d.Step, Path: d.Path, Note: note})
		if stepChanged {
			changed = true
			opt.CanChange = false
		}
		opt.LastNote = note
		rememberNote(opt.Bus, note)
		opt.log("self %s %s", d.Step, clip(note, 120))
		if err != nil {
			rep.Status = StatusBlocked
			break
		}
	}
	if len(history) == opt.MaxSteps && rep.Status == StatusDone {
		rep.Status = StatusParked
	}
	rep.Changed = changed
	rep.Steps = history
	rep.Note = joinNotes(history)
	if rep.Note == "" {
		rep.Note = "You noticed yourself. Nothing was edited."
	}
	return rep, nil
}

func act(ctx context.Context, opt Options, d Decision, paths []choice) (string, bool, error) {
	if err := guard(d, opt.CanChange, paths); err != nil {
		return "You did not " + d.Step + ": " + err.Error() + ". Do not invent a result.", false, err
	}
	switch d.Step {
	case StepNotice:
		return notice(opt), false, nil
	case StepRead:
		return readStep(opt, d.Path)
	case StepRemember:
		return remember(ctx, opt)
	case StepChange:
		return change(ctx, opt, d.Path)
	default:
		return "Nothing further.", false, nil
	}
}

func notice(opt Options) string {
	return fmt.Sprintf(
		"You noticed yourself. This turn you were steered as %s. Your feeling: %s. You were tracking them as %s. The running process is this build.",
		or(opt.Mode, "continue"), or(opt.SelfEmotion, "unspecified"), or(opt.UserEmotion, "unspecified"))
}

func readStep(opt Options, rel string) (string, bool, error) {
	if opt.Bus == nil {
		return "You could not read " + rel + ".", false, fmt.Errorf("no sense bus")
	}
	view, err := opt.Bus.Read(rel, 0)
	if err != nil {
		return fmt.Sprintf("You reached for %s but could not feel it (%v).", rel, err), false, err
	}
	text := strings.TrimSpace(view.Excerpt)
	if text == "" {
		text = "(empty)"
	}
	return fmt.Sprintf("You read %s (%d lines). Excerpt: %s", view.Path, view.Lines, clip(text, 500)), false, nil
}

func remember(ctx context.Context, opt Options) (string, bool, error) {
	if opt.Memory == nil {
		return "You have nowhere to keep a memory line.", false, fmt.Errorf("no memory")
	}
	if opt.LLM == nil {
		return "You could not write the memory line.", false, fmt.Errorf("no llm")
	}
	raw, err := opt.LLM.ChatComplete(ctx, rememberSystem, "goal:\n"+clip(opt.Goal, 400))
	if err != nil {
		return "You could not write the memory line.", false, err
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &body); err != nil || strings.TrimSpace(body.Text) == "" {
		return "The memory line was not usable.", false, fmt.Errorf("memory json")
	}
	view, err := opt.Memory.Apply(memory.MemoryOp{Op: "add", Kind: memory.KindOpenLoop, Text: body.Text})
	if err != nil {
		return "You could not keep that line (" + err.Error() + ").", false, err
	}
	_ = view
	return "You remembered one line: " + strings.TrimSpace(body.Text) + ". No source file changed.", false, nil
}

func change(ctx context.Context, opt Options, rel string) (string, bool, error) {
	root := ""
	if opt.Bus != nil {
		root = opt.Bus.Root()
	}
	if root == "" {
		return "You could not see your own source tree.", false, fmt.Errorf("no root")
	}
	if opt.LLM == nil || opt.Coder == nil {
		return "You did not edit " + rel + ". The intent or the editor was missing.", false, fmt.Errorf("change unavailable")
	}
	raw, err := opt.LLM.ChatComplete(ctx, intentSystem, "file: "+rel+"\ngoal:\n"+clip(opt.Goal, 400))
	if err != nil {
		return "You could not form an intent for " + rel + ".", false, err
	}
	var body struct {
		Why    string `json:"why"`
		Change string `json:"change"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &body); err != nil {
		return "The intent for " + rel + " was not usable.", false, fmt.Errorf("intent json")
	}
	body.Why = strings.TrimSpace(body.Why)
	body.Change = strings.TrimSpace(body.Change)
	if body.Change == "" {
		return "The intent for " + rel + " was empty.", false, fmt.Errorf("empty change")
	}
	snap, err := takeSnap(ctx, root)
	if err != nil {
		return "You did not edit " + rel + " (" + err.Error() + ").", false, err
	}
	keepBefore, keepExisted, err := readFile(root, rel)
	if err != nil {
		return "You could not feel " + rel + " before editing.", false, err
	}
	prompt := coderPrompt(rel, body.Why, body.Change)
	if _, err := opt.Coder.Edit(ctx, root, prompt); err != nil {
		_, _ = snap.restoreExcept(ctx, "")
		if keepExisted {
			full, _ := safeJoin(root, rel)
			_ = os.WriteFile(full, keepBefore, 0o644)
		}
		return "The edit of " + rel + " failed. Nothing was kept.", false, err
	}
	reverted, left := snap.restoreExcept(ctx, rel)
	if len(left) > 0 {
		full, joinErr := safeJoin(root, rel)
		if joinErr == nil {
			if keepExisted {
				_ = os.WriteFile(full, keepBefore, 0o644)
			} else {
				_ = os.Remove(full)
			}
		}
		return "The edit touched " + strings.Join(left, ", ") + " outside " + rel + ". It was put back. The running process is still the previous build.", false, fmt.Errorf("edit left the file")
	}
	note := fmt.Sprintf("You edited only %s on disk. Why: %s. Change: %s.", rel, or(body.Why, "unspecified"), clip(body.Change, 200))
	if len(reverted) > 0 {
		note += " Put back " + strings.Join(reverted, ", ") + "."
	}
	note += " The running process is still the previous build. You have not reloaded."
	return note, true, nil
}

const intentSystem = `Return a JSON object with exactly two string keys, why and change.
why is one short reason. change is the concrete edit for the file named by the user message.
Do not name any other file. No markdown, no extra keys.`

const rememberSystem = `Return a JSON object with exactly one string key, text.
text is one memory line of at most 80 characters about what she should keep.
No markdown, no extra keys.`

func coderPrompt(path, why, change string) string {
	return fmt.Sprintf(`Edit only this file: %s
Do not create, rename, or edit any other path.
Do not change safety checks, the judge, the agent loop, or the voice session.
The running process will not reload. This is a source edit on disk.

Why: %s
Change: %s
`, path, why, change)
}

func rememberNote(bus *sense.Bus, note string) {
	if bus == nil {
		return
	}
	bus.Set(func(l *sense.Live) { l.SelfNote = clip(note, 400) })
	bus.Emit(sense.Event{Kind: sense.KindLook, Summary: "react " + clip(note, 80)})
}

func joinNotes(steps []Step) string {
	if len(steps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Self loop, in order. Speak only from this. ")
	for _, s := range steps {
		fmt.Fprintf(&b, "%s: %s ", s.Step, clip(s.Note, 280))
	}
	return b.String()
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (o Options) log(format string, args ...any) {
	if o.LogFn == nil {
		return
	}
	o.LogFn(fmt.Sprintf(format, args...))
}
