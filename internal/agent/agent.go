// Package agent orchestrates the full loop:
// duplex voice -> Jev turn judgment -> steering -> async planner.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jingx8885/evomap-tavern/internal/jev"
	"github.com/jingx8885/evomap-tavern/internal/judge"
	"github.com/jingx8885/evomap-tavern/internal/livevoice"
	"github.com/jingx8885/evomap-tavern/internal/llm"
	"github.com/jingx8885/evomap-tavern/internal/memory"
	"github.com/jingx8885/evomap-tavern/internal/persona"
	"github.com/jingx8885/evomap-tavern/internal/planner"
	"github.com/jingx8885/evomap-tavern/internal/steering"
)

// Options configures a run.
type Options struct {
	BaseURL      string
	APIKey       string
	PersonaPath  string
	JevModel     string
	PlannerModel string
	RunsDir      string
	Greeting     bool
	Verbose      bool
	LogFn        func(string)
}

// Run starts a live voice session and processes turns until ctx is done.
func Run(ctx context.Context, opt Options) error {
	p, err := persona.Load(opt.PersonaPath)
	if err != nil {
		return err
	}
	jevClient := jev.NewClient(opt.BaseURL, opt.APIKey, opt.JevModel)
	llmClient := llm.NewClient(opt.BaseURL, opt.APIKey, opt.PlannerModel)
	mem := memory.New(10)
	pl := planner.New(p, jevClient, llmClient, opt.PlannerModel)
	pl.LogFn = func(s string) { opt.log("%s", "[planner] "+s) }

	sess, err := livevoice.Connect(ctx, opt.BaseURL, opt.APIKey,
		p.BaseInstructions(), p.Voice)
	if err != nil {
		return err
	}
	sess.Verbose = opt.Verbose
	defer sess.Close()

	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := sess.WaitStarted(wctx); err != nil {
		cancel()
		return fmt.Errorf("session did not start: %w", err)
	}
	cancel()
	opt.log("session started; persona=%s voice=%s", p.Name, p.Voice)

	if opt.Greeting && p.Greeting != "" {
		if err := sess.Speak(p.Greeting); err != nil {
			opt.log("greeting failed: %v", err)
		}
	}

	cmds := make(chan string, 16)
	go readStdin(ctx, cmds)
	opt.log("commands: /say <text> (speak) /steer <text> (reinstruct) " +
		"/goal <text> (plan) /status /quit")

	for {
		select {
		case <-ctx.Done():
			return opt.saveLog(mem, pl, nil)
		case ev, ok := <-sess.Events():
			if !ok {
				return opt.saveLog(mem, pl, nil)
			}
			switch ev.Kind {
			case livevoice.EventTurnDone:
				assistant, _ := ev.Usage["assistant"].(string)
				if assistant != "" {
					mem.Add(memory.Turn{Speaker: "assistant", Text: assistant})
					opt.log("[assistant] %s", clip(assistant, 120))
				}
				if ev.Text != "" {
					mem.Add(memory.Turn{Speaker: "user", Text: ev.Text})
					opt.log("[user] %s", clip(ev.Text, 120))
				}
				go processTurn(ctx, opt, p, jevClient, mem, pl, sess, ev.Text)
			case livevoice.EventTranscript:
				if ev.Speaker == "user" {
					opt.log("[user~] %s", clip(ev.Text, 120))
				}
			case livevoice.EventError:
				opt.log("[voice error] %v", ev.Err)
			case livevoice.EventClosed:
				opt.log("session closed: %v", ev.Err)
				return opt.saveLog(mem, pl, ev.Err)
			}
		case line := <-cmds:
			if handleCommand(line, p, pl, sess, opt) {
				return opt.saveLog(mem, pl, nil)
			}
		}
	}
}

// processTurn runs Jev judgment + steering + planner tick for one turn.
func processTurn(ctx context.Context, opt Options, p *persona.Persona,
	jc *jev.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, userText string) {
	jd, err := judge.JudgeTurn(ctx, jc, p, mem, userText)
	if err != nil {
		opt.log("[judge] skipped: %v", err)
		return
	}
	safety := jd.SafetyP >= p.Judge.SafetyThresh
	mem.UpdateAffect(jd.Valence, jd.Arousal, jd.Emotion, safety)
	affect := mem.Affect()
	opt.log("[judge] emotion=%s valence=%.2f arousal=%.2f engage=%.2f safety=%.2f conf=%.2f",
		jd.Emotion, jd.Valence, jd.Arousal, jd.Engagement, jd.SafetyP, jd.Confidence)

	mode := judge.DecideMode(jd, affect, p.Judge.SafetyThresh, pl.Current())
	note := steering.Build(p, mode, jd, affect, pl.Current())
	if err := sess.Steer(note); err != nil {
		opt.log("[steer] failed: %v", err)
	} else {
		opt.log("[steer] mode=%s", mode)
	}
	pl.Tick(ctx, mem, false)
}

// handleCommand processes a slash command; returns true to quit.
func handleCommand(line string, p *persona.Persona, pl *planner.Planner,
	sess *livevoice.Session, opt Options) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	switch {
	case line == "/quit", line == "/q":
		return true
	case line == "/status":
		opt.log("[status] plan=%s affect_state logged at exit", pl.Current())
	case strings.HasPrefix(line, "/say "):
		if err := sess.Speak(strings.TrimPrefix(line, "/say ")); err != nil {
			opt.log("speak failed: %v", err)
		}
	case strings.HasPrefix(line, "/steer "):
		if err := sess.Steer(strings.TrimPrefix(line, "/steer ")); err != nil {
			opt.log("steer failed: %v", err)
		}
	case strings.HasPrefix(line, "/goal "):
		pl.SetNote(strings.TrimPrefix(line, "/goal "))
		opt.log("goal set")
	default:
		opt.log("unknown command (try /say /steer /goal /status /quit)")
	}
	return false
}

// readStdin forwards stdin lines into the command channel.
func readStdin(ctx context.Context, out chan<- string) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		select {
		case out <- sc.Text():
		case <-ctx.Done():
			return
		}
	}
}

// saveLog writes the session transcript + affect + plan to runs/.
func (o Options) saveLog(mem *memory.Memory, pl *planner.Planner, cause error) error {
	dir := o.RunsDir
	if dir == "" {
		dir = "runs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(dir, time.Now().Format("20060102-150405")+".json")
	var causeStr string
	if cause != nil {
		causeStr = cause.Error()
	}
	payload := map[string]any{
		"persona": o.PersonaPath,
		"turns":   mem.Turns(),
		"affect":  mem.Affect(),
		"plan":    pl.State(),
		"ended":   time.Now().Format(time.RFC3339),
		"cause":   causeStr,
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.WriteFile(name, raw, 0o644); err != nil {
		return err
	}
	o.log("session log: %s", name)
	return nil
}

func (o Options) log(format string, args ...any) {
	if o.LogFn != nil {
		o.LogFn(fmt.Sprintf(format, args...))
		return
	}
	fmt.Printf(format+"\n", args...)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// suppress unused import warning for sync in some build paths.
var _ = sync.Mutex{}
