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

	"github.com/jingx8885/lov-evo/internal/audio"
	"github.com/jingx8885/lov-evo/internal/avatar"
	"github.com/jingx8885/lov-evo/internal/desk"
	"github.com/jingx8885/lov-evo/internal/eye"
	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/livevoice"
	"github.com/jingx8885/lov-evo/internal/llm"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/planner"
	"github.com/jingx8885/lov-evo/internal/sense"
	"github.com/jingx8885/lov-evo/internal/steering"
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
	Live2DAddr   string
	Live2DDir    string
	OpenViewer   bool
	Say          string
	SenseRoot    string
	Vision       string
	VisionModel  string
	VisionEvery  time.Duration
	avatarHub    *avatar.Hub
	sense        *sense.Bus
	eyes         *eye.Eyes
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

	bus, serr := sense.Open(sense.Options{Root: opt.SenseRoot, RunsDir: opt.RunsDir})
	if serr != nil {
		opt.log("sense disabled: %v", serr)
	} else {
		opt.sense = bus
		defer bus.Close()
		bus.Set(func(l *sense.Live) {
			l.Persona = p.Name
			l.VoiceName = p.Voice
			l.Voice = "connecting"
		})
	}

	if addr := strings.TrimSpace(opt.Live2DAddr); addr != "" && addr != "off" {
		hub, _, err := avatar.Listen(ctx, avatar.ListenOptions{
			Addr:  addr,
			Dir:   opt.Live2DDir,
			Open:  opt.OpenViewer,
			LogFn: func(s string) { opt.log("%s", s) },
		})
		if err != nil {
			opt.log("live2d disabled: %v", err)
		} else {
			opt.avatarHub = hub
			if opt.sense != nil {
				hub.SetSense(func() any { return opt.sense.Snapshot(p.Name) })
			}
			hub.Publish(avatar.Drive("continue", &judge.Judgment{
				Emotion: "neutral", Engagement: 0.7,
			}, memory.Affect{Valence: 0.55, Arousal: 0.4, Emotion: "neutral"}))
		}
	}

	cam, scr := eye.ParseSources(opt.Vision)
	if strings.TrimSpace(opt.Vision) == "" {
		cam, scr = p.Sense.Eyes, p.Sense.Eyes
	}
	if !p.Sense.Enabled || !p.Sense.Eyes {
		cam, scr = false, false
	}
	if cam || scr {
		visModel := opt.VisionModel
		if visModel == "" {
			visModel = opt.PlannerModel
		}
		vis := llm.NewClient(opt.BaseURL, opt.APIKey, visModel)
		host := desk.DefaultHost{}
		opt.eyes = eye.Start(ctx, eye.Options{
			Camera:   cam,
			Screen:   scr,
			Interval: opt.VisionEvery,
			Jev:      jevClient,
			LLM:      vis,
			Observe: func(ctx context.Context) (eye.ScreenView, error) {
				g, err := desk.Look(ctx, jevClient, host, "")
				if err != nil {
					return eye.ScreenView{}, err
				}
				return eye.ScreenView{Caption: g.Caption, Signature: g.Signature, Private: g.Private}, nil
			},
			LogFn: func(s string) { opt.log("[eye] %s", s) },
			OnSight: func(s eye.Sight) {
				if opt.sense == nil {
					return
				}
				opt.sense.Set(func(l *sense.Live) {
					if s.Camera.Caption != "" {
						l.Camera = s.Camera.Caption
					}
					if s.Screen.Caption != "" {
						l.Screen = s.Screen.Caption
					}
				})
				sum := s.Camera.Caption
				if s.Screen.Caption != "" {
					if sum != "" {
						sum += " | "
					}
					sum += s.Screen.Caption
				}
				if strings.TrimSpace(sum) != "" {
					opt.sense.Emit(sense.Event{Kind: sense.KindSee, Summary: clip(sum, 120)})
				}
			},
		})
		if opt.avatarHub != nil {
			opt.avatarHub.SetEye(func(source, dataURL string) {
				if err := opt.eyes.Push(source, dataURL); err != nil {
					opt.log("[eye] push %s: %v", source, err)
				}
			})
		}
	}

	cmds := make(chan string, 16)
	go readStdin(ctx, cmds)
	opt.log("commands: /say <text> (speak) /steer <text> (reinstruct) " +
		"/goal <text> (plan) /look <file> (her source) /see /sense /desk <goal> " +
		"/codex <goal> (luna) /status /quit")

	first := true
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return opt.saveLog(mem, pl, lastErr)
		}
		sess, err := livevoice.Connect(ctx, opt.BaseURL, opt.APIKey,
			p.BaseInstructions(), p.Voice)
		if err != nil {
			lastErr = err
			opt.log("voice connect failed: %v; retry in 2s", err)
			select {
			case <-ctx.Done():
				return opt.saveLog(mem, pl, err)
			case <-time.After(2 * time.Second):
			}
			continue
		}
		sess.Verbose = opt.Verbose
		if opt.avatarHub != nil {
			sess.OnPCM(func(pcm []byte) {
				// Mouth only: local winmm already plays this PCM. Sending it
				// to the viewer made the browser play a second copy (echo).
				opt.avatarHub.Mouth(audio.MouthOpen(pcm))
			})
		}

		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = sess.WaitStarted(wctx)
		cancel()
		if err != nil {
			sess.Close()
			lastErr = err
			opt.log("session did not start: %v; retry in 2s", err)
			select {
			case <-ctx.Done():
				return opt.saveLog(mem, pl, err)
			case <-time.After(2 * time.Second):
			}
			continue
		}
		opt.log("session started; persona=%s voice=%s", p.Name, p.Voice)
		opt.sense.Set(func(l *sense.Live) { l.Voice = "up" })
		opt.sense.Emit(sense.Event{Kind: sense.KindVoice, Summary: "up"})

		if first {
			if opt.Greeting && p.Greeting != "" {
				if err := sess.Speak(p.Greeting); err != nil {
					opt.log("greeting failed: %v", err)
				}
			}
			if s := strings.TrimSpace(opt.Say); s != "" {
				if err := sess.Speak(s); err != nil {
					opt.log("say failed: %v", err)
				} else {
					opt.log("say: %s", s)
				}
			}
			first = false
		}

		res := pumpSession(ctx, opt, p, jevClient, llmClient, mem, pl, sess, cmds)
		sess.Close()
		opt.sense.Set(func(l *sense.Live) { l.Voice = "down" })
		opt.sense.Emit(sense.Event{Kind: sense.KindVoice, Summary: "down"})
		switch res.kind {
		case pumpQuit, pumpCancel:
			return opt.saveLog(mem, pl, res.err)
		default:
			lastErr = res.err
			opt.log("voice session dropped: %v; reconnecting in 2s", res.err)
			select {
			case <-ctx.Done():
				return opt.saveLog(mem, pl, res.err)
			case <-time.After(2 * time.Second):
			}
		}
	}
}

type pumpKind int

const (
	pumpDrop pumpKind = iota
	pumpQuit
	pumpCancel
)

type pumpResult struct {
	kind pumpKind
	err  error
}

func pumpSession(ctx context.Context, opt Options, p *persona.Persona,
	jevClient *jev.Client, llmClient *llm.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, cmds <-chan string) pumpResult {
	var lastTurnKey string
	var lastTurnAt time.Time
	gate := newJevGate()
	for {
		select {
		case <-ctx.Done():
			return pumpResult{kind: pumpCancel, err: ctx.Err()}
		case ev, ok := <-sess.Events():
			if !ok {
				return pumpResult{kind: pumpDrop, err: fmt.Errorf("events closed")}
			}
			switch ev.Kind {
			case livevoice.EventTurnDone:
				assistant, _ := ev.Usage["assistant"].(string)
				key := ev.Text + "\x00" + assistant
				if key == lastTurnKey && time.Since(lastTurnAt) < time.Second {
					continue
				}
				lastTurnKey, lastTurnAt = key, time.Now()
				if assistant != "" {
					mem.Add(memory.Turn{Speaker: "assistant", Text: assistant})
					opt.log("[assistant] %s", clip(assistant, 120))
				}
				if ev.Text != "" {
					mem.Add(memory.Turn{Speaker: "user", Text: ev.Text})
					opt.log("[user] %s", clip(ev.Text, 120))
				}
				opt.sense.Emit(sense.Event{
					Kind:    sense.KindTurn,
					Summary: clip(ev.Text, 80),
					Data:    map[string]any{"user": clip(ev.Text, 160), "assistant": clip(assistant, 160)},
				})
				opt.sense.Set(func(l *sense.Live) {
					l.Turns++
					if ev.Text != "" {
						l.LastUser = clip(ev.Text, 160)
					}
				})
				gate.cancelTimer()
				go processTurn(ctx, opt, p, jevClient, mem, pl, sess, gate, ev.Text, true)
			case livevoice.EventTranscript:
				if ev.Speaker == "user" {
					opt.log("[user~] %s", clip(ev.Text, 120))
					text := ev.Text
					gate.schedule(320*time.Millisecond, text, func() {
						go processTurn(ctx, opt, p, jevClient, mem, pl, sess, gate, text, false)
					})
				}
			case livevoice.EventWarning:
				opt.log("[voice warning] %v", ev.Err)
				opt.sense.Emit(sense.Event{Kind: sense.KindWarning, Summary: fmt.Sprint(ev.Err)})
			case livevoice.EventError:
				opt.log("[voice error] %v", ev.Err)
				opt.sense.Emit(sense.Event{Kind: sense.KindError, Summary: fmt.Sprint(ev.Err)})
			case livevoice.EventClosed:
				opt.log("session closed: %v", ev.Err)
				return pumpResult{kind: pumpDrop, err: ev.Err}
			}
		case line := <-cmds:
			if handleCommand(ctx, line, p, pl, sess, opt, jevClient, llmClient) {
				return pumpResult{kind: pumpQuit}
			}
		}
	}
}

// jevGate dedupes speculative (partial transcript) and final turn judgments.
type jevGate struct {
	mu       sync.Mutex
	timer    *time.Timer
	pending  string
	inflight map[string]bool
	done     map[string]time.Time
	last     *judge.Judgment
}

func newJevGate() *jevGate {
	return &jevGate{
		inflight: map[string]bool{},
		done:     map[string]time.Time{},
	}
}

func (g *jevGate) schedule(delay time.Duration, text string, fn func()) {
	n := strings.Join(strings.Fields(text), " ")
	if n == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending = n
	if g.timer != nil {
		g.timer.Stop()
	}
	captured := n
	g.timer = time.AfterFunc(delay, func() {
		g.mu.Lock()
		same := g.pending == captured
		g.timer = nil
		g.mu.Unlock()
		if same {
			fn()
		}
	})
}

func (g *jevGate) cancelTimer() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

func (g *jevGate) claim(text string) bool {
	n := strings.Join(strings.Fields(text), " ")
	if n == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[n] {
		return false
	}
	if t, ok := g.done[n]; ok && time.Since(t) < 15*time.Second {
		return false
	}
	now := time.Now()
	for prev, t := range g.done {
		if now.Sub(t) > 15*time.Second {
			delete(g.done, prev)
		}
	}
	g.inflight[n] = true
	return true
}

func (g *jevGate) finish(text string, ok bool) {
	n := strings.Join(strings.Fields(text), " ")
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inflight, n)
	if ok {
		g.done[n] = time.Now()
	}
}

func (g *jevGate) remember(jd *judge.Judgment) {
	g.mu.Lock()
	g.last = jd
	g.mu.Unlock()
}

func (g *jevGate) lastJudgment() *judge.Judgment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// processTurn runs Jev judgment + steering + planner tick for one turn.
// Speculative calls (final=false) run from a debounced user transcript so
// steering can land while the duplex model is still speaking.
func processTurn(ctx context.Context, opt Options, p *persona.Persona,
	jc *jev.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, gate *jevGate, userText string, final bool) {
	// Greeting / model self-talk has no user text. Judging those as
	// "low engagement" steers re_engage and commentary-nudges, which
	// makes the duplex model say the same line again.
	if strings.TrimSpace(userText) == "" {
		return
	}
	claimed := gate.claim(userText)
	if claimed {
		jd, err := judge.JudgeTurn(ctx, jc, p, mem, userText, pl.Current())
		if err != nil {
			gate.finish(userText, false)
			opt.log("[judge] skipped: %v", err)
		} else {
			gate.finish(userText, true)
			safety := jd.SafetyP >= p.Judge.SafetyThresh
			mem.UpdateAffect(jd.Valence, jd.Arousal, jd.Emotion, safety)
			affect := mem.Affect()
			tag := ""
			if !final {
				tag = " early"
			}
			opt.log("[judge]%s emotion=%s intent=%s self=%s jev_mode=%s valence=%.2f arousal=%.2f engage=%.2f safety=%.2f fit=%.2f need_llm=%.2f conf=%.2f",
				tag, jd.Emotion, orDash(jd.Intent), orDash(jd.SelfEmotion), orDash(jd.Mode),
				jd.Valence, jd.Arousal, jd.Engagement, jd.SafetyP, jd.PersonaFitP, jd.NeedLLMP, jd.Confidence)

			mode := judge.DecideMode(jd, affect, p.Judge.SafetyThresh, pl.Current())
			if opt.avatarHub != nil {
				frame := avatar.Drive(mode, jd, affect)
				opt.avatarHub.Publish(frame)
				opt.sense.Set(func(l *sense.Live) {
					l.Expression = frame.Expression
					l.Mouth = opt.avatarHub.MouthValue()
				})
			}
			opt.sense.Set(func(l *sense.Live) {
				l.Mode = mode
				l.Emotion = jd.Emotion
				l.Affect = affect
				l.Plan = pl.Current()
				l.LastUser = clip(userText, 160)
			})
			opt.sense.Emit(sense.Event{
				Kind:    sense.KindJudge,
				Summary: mode + " " + jd.Emotion,
				Data: map[string]any{
					"mode": mode, "emotion": jd.Emotion,
					"valence": jd.Valence, "arousal": jd.Arousal,
					"engage": jd.Engagement, "early": !final,
				},
			})
			note := steering.Build(p, mode, jd, affect, pl.Current())
			ask := sense.ParseAsk(userText)
			if ask.Kind != "" {
				opt.sense.Set(func(l *sense.Live) { l.LastAsk = ask.Kind })
			}
			if felt := opt.sense.Felt(p, ask); felt != "" {
				note = note + " " + felt
			}
			if err := sess.Steer(note); err != nil {
				opt.log("[steer] failed: %v", err)
			} else {
				opt.log("[steer] mode=%s", mode)
				opt.sense.Emit(sense.Event{Kind: sense.KindSteer, Summary: mode})
			}
			gate.remember(jd)
		}
	}
	if !final {
		return
	}
	if jd := gate.lastJudgment(); jd != nil {
		pl.Consider(ctx, mem, jd.NeedLLMP)
	}
	before := pl.State().LastRefreshed
	go func() {
		deadline := time.Now().Add(50 * time.Second)
		for time.Now().Before(deadline) {
			if note, ok := pl.ConsumeNudge(before); ok {
				if err := sess.Nudge(note); err != nil {
					opt.log("[nudge] failed: %v", err)
				} else {
					opt.log("[nudge] %s", note)
					opt.sense.Emit(sense.Event{Kind: sense.KindPlan, Summary: clip(note, 120)})
				}
				return
			}
			if !pl.Refining() {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
}

// handleCommand processes a slash command; returns true to quit.
func handleCommand(ctx context.Context, line string, p *persona.Persona, pl *planner.Planner,
	sess *livevoice.Session, opt Options, jc *jev.Client, lc *llm.Client) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	switch {
	case line == "/quit", line == "/q":
		return true
	case line == "/status":
		if opt.sense != nil {
			live := opt.sense.Live()
			opt.log("[status] voice=%s mode=%s face=%s turns=%d plan=%s cam=%s scr=%s",
				live.Voice, orDash(live.Mode), orDash(live.Expression), live.Turns, pl.Current(),
				orDash(live.Camera), orDash(live.Screen))
		} else {
			opt.log("[status] plan=%s affect_state logged at exit", pl.Current())
		}
	case line == "/sense":
		if opt.sense == nil {
			opt.log("sense unavailable")
			break
		}
		raw, _ := json.MarshalIndent(opt.sense.Snapshot(p.Name), "", "  ")
		opt.log("[sense]\n%s", raw)
		opt.sense.Emit(sense.Event{Kind: sense.KindCommand, Summary: "/sense"})
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskBody}); felt != "" {
			if err := sess.Steer(felt); err != nil {
				opt.log("[sense] steer failed: %v", err)
			}
		}
	case strings.HasPrefix(line, "/look "):
		rel := strings.TrimSpace(strings.TrimPrefix(line, "/look "))
		if opt.sense == nil {
			opt.log("sense unavailable")
			break
		}
		view, err := opt.sense.Read(rel, 0)
		if err != nil {
			opt.log("[look] %v", err)
			break
		}
		opt.log("[look] %s lines=%d clipped=%v", view.Path, view.Lines, view.Clipped)
		opt.sense.Emit(sense.Event{Kind: sense.KindLook, Summary: view.Path})
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskFile, File: view.Path}); felt != "" {
			if err := sess.Steer(felt); err != nil {
				opt.log("[look] steer failed: %v", err)
			}
		}
	case line == "/see":
		if opt.eyes == nil {
			opt.log("eyes down (pass -vision=both and sense.eyes: true)")
			break
		}
		s := opt.eyes.LookNow(ctx)
		opt.log("[see] camera=%s screen=%s", clip(s.Camera.Caption, 80), clip(s.Screen.Caption, 80))
		if opt.sense != nil {
			opt.sense.Emit(sense.Event{Kind: sense.KindCommand, Summary: "/see"})
			if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskSee}); felt != "" {
				if err := sess.Steer(felt); err != nil {
					opt.log("[see] steer failed: %v", err)
				}
			}
		}
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
	case strings.HasPrefix(line, "/desk "):
		goal := strings.TrimSpace(strings.TrimPrefix(line, "/desk "))
		if goal == "" {
			opt.log("desk needs a goal")
			break
		}
		opt.log("[desk] starting: %s", goal)
		go func() {
			rep, err := desk.Run(ctx, desk.Options{
				Goal:   goal,
				Prefer: "",
				Jev:    jc,
				LLM:    lc,
				APIKey: opt.APIKey,
				LogFn:  func(s string) { opt.log("[desk] %s", s) },
			})
			if err != nil {
				opt.log("[desk] failed: %v", err)
				return
			}
			opt.log("[desk] status=%s steps=%d", rep.Status, len(rep.Steps))
			opt.sense.Emit(sense.Event{Kind: sense.KindDesk, Summary: "desk " + rep.Status})
		}()
	case strings.HasPrefix(line, "/codex "):
		goal := strings.TrimSpace(strings.TrimPrefix(line, "/codex "))
		if goal == "" {
			opt.log("codex needs a goal")
			break
		}
		model := opt.PlannerModel
		if model == "" {
			model = "gpt-5.6-luna"
		}
		opt.log("[codex] starting model=%s: %s", model, goal)
		go func() {
			rep, err := desk.Run(ctx, desk.Options{
				Goal:       goal,
				Driver:     "codex",
				CodexModel: model,
				APIKey:     opt.APIKey,
				LogFn:      func(s string) { opt.log("[codex] %s", s) },
			})
			if err != nil {
				opt.log("[codex] failed: %v", err)
				return
			}
			opt.log("[codex] status=%s steps=%d", rep.Status, len(rep.Steps))
			opt.sense.Emit(sense.Event{Kind: sense.KindDesk, Summary: "codex " + rep.Status})
		}()
	default:
		opt.log("unknown command (try /say /steer /goal /look /see /sense /desk /codex /status /quit)")
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
	if o.sense != nil {
		payload["sense"] = o.sense.Snapshot("")
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

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
