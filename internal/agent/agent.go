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
	"github.com/jingx8885/lov-evo/internal/studio"
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
	rel          *memory.RelationshipStore
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

	rel, rerr := memory.NewRelationshipStore(relationshipPath(opt.RunsDir, p.Name))
	if rerr != nil {
		opt.log("relationship memory disabled: %v", rerr)
	} else {
		opt.rel = rel
	}

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
			hub.Publish(avatar.DriveWithRelationship("continue", &judge.Judgment{
				Emotion: "neutral", SelfEmotion: "neutral", Engagement: 0.7,
			}, memory.Affect{Valence: 0.55, Arousal: 0.4, Emotion: "neutral"}, relCue(opt.rel)))
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
			eyes := opt.eyes
			opt.avatarHub.SetEye(func(source, dataURL string) {
				if err := eyes.Push(source, dataURL); err != nil {
					opt.log("[eye] push %s: %v", source, err)
				}
			})
			opt.avatarHub.SetEyes(eyes.Enabled, eyes.SetEnabled)
		}
	}

	cmds := make(chan string, 16)
	go readStdin(ctx, cmds)
	opt.log("commands: /say <text> (speak) /steer <text> (reinstruct) " +
		"/goal <text> (plan) /look <file> (her source) /camera /screen /eyes on|off /sense /desk <goal> " +
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
	slot := &capabilitySlot{}
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
				gate.closeUtterance()
				go processTurn(ctx, opt, p, jevClient, llmClient, mem, pl, sess, gate, slot, ev.Text, true)
			case livevoice.EventTranscript:
				if ev.Speaker == "user" {
					opt.log("[user~] %s", clip(ev.Text, 120))
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

// jevMinInterval is the floor between /v1/systemone calls. Live VAD splits
// one stretch of speech into a turn every couple of seconds; judging each
// of those turns is what made Jev fire continuously.
const jevMinInterval = 10 * time.Second

// earlyMinRunes keeps a one-character ASR fragment from spending a Jev call.
const earlyMinRunes = 8

// jevGate dedupes turn judgments. Partial transcripts do not call Jev;
// turn.done does, and not again until jevMinInterval has passed.
type jevGate struct {
	mu          sync.Mutex
	timer       *time.Timer
	pending     string
	inflight    map[string]bool
	done        map[string]time.Time
	last        *judge.Judgment
	speculative string
	epoch       uint64
	lastCall    time.Time
	minInterval time.Duration
}

func newJevGate() *jevGate {
	return &jevGate{
		inflight:    map[string]bool{},
		done:        map[string]time.Time{},
		minInterval: jevMinInterval,
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
	g.stopTimerLocked()
}

func (g *jevGate) stopTimerLocked() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// wantEarly reports whether this partial transcript may spend the one
// speculative Jev call for the current utterance.
func (g *jevGate) wantEarly(text string) bool {
	n := strings.Join(strings.Fields(text), " ")
	if len([]rune(n)) < earlyMinRunes {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.speculative == ""
}

// closeUtterance ends the speculative window. A late early result must not
// overwrite the turn.done judgment.
func (g *jevGate) closeUtterance() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopTimerLocked()
	g.speculative = ""
	g.epoch++
}

func (g *jevGate) claim(text string) bool {
	ok, _ := g.claimEpoch(text, false)
	return ok
}

func (g *jevGate) claimEpoch(text string, speculative bool) (bool, uint64) {
	n := strings.Join(strings.Fields(text), " ")
	if n == "" {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if speculative {
		if g.speculative != "" || len([]rune(n)) < earlyMinRunes {
			return false, 0
		}
	}
	if g.minInterval > 0 && !g.lastCall.IsZero() && time.Since(g.lastCall) < g.minInterval {
		return false, 0
	}
	if g.inflight[n] {
		return false, 0
	}
	if t, ok := g.done[n]; ok && time.Since(t) < 15*time.Second {
		return false, 0
	}
	now := time.Now()
	for prev, t := range g.done {
		if now.Sub(t) > 15*time.Second {
			delete(g.done, prev)
		}
	}
	g.inflight[n] = true
	g.lastCall = time.Now()
	if speculative {
		g.speculative = n
	}
	return true, g.epoch
}

func (g *jevGate) cooling() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.minInterval > 0 && !g.lastCall.IsZero() && time.Since(g.lastCall) < g.minInterval
}

func (g *jevGate) current(epoch uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.epoch == epoch
}

func (g *jevGate) finish(text string, ok bool) {
	n := strings.Join(strings.Fields(text), " ")
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inflight, n)
	if ok {
		g.done[n] = time.Now()
		return
	}
	g.lastCall = time.Time{}
	if g.speculative == n {
		g.speculative = ""
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

// capabilitySlot is the open branch plus the one hands-run it may have in flight.
// The branch stays after a burst ends, until the entry Jev says it is done.
type capabilitySlot struct {
	mu        sync.Mutex
	computer  bool
	kind      string
	goal      string
	note      string
	runCancel context.CancelFunc
	gen       int
}

func (s *capabilitySlot) snapshot() judge.Branch {
	if s == nil {
		return judge.Branch{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return judge.Branch{Kind: s.kind, Goal: s.goal, Note: s.note}
}

func (s *capabilitySlot) current() (kind, goal, note string) {
	if s == nil {
		return "", "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind, s.goal, s.note
}

func (s *capabilitySlot) open(kind, goal string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kind = kind
	s.goal = strings.TrimSpace(goal)
	s.note = ""
}

func (s *capabilitySlot) follow(text string) {
	if s == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind == "" || strings.Contains(s.goal, text) {
		return
	}
	if s.goal == "" {
		s.goal = text
		return
	}
	s.goal += "\n" + text
}

func (s *capabilitySlot) setNote(note string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.note = strings.TrimSpace(note)
	s.mu.Unlock()
}

func (s *capabilitySlot) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.runCancel
	s.runCancel = nil
	s.gen++
	s.kind = ""
	s.goal = ""
	s.note = ""
	s.computer = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *capabilitySlot) bind(parent context.Context) context.Context {
	if s == nil {
		return parent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCancel != nil {
		s.runCancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.runCancel = cancel
	return ctx
}

func (s *capabilitySlot) busy() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.computer
}

func (s *capabilitySlot) tryComputer() (bool, int) {
	if s == nil {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.computer {
		return false, 0
	}
	s.gen++
	s.computer = true
	return true, s.gen
}

func (s *capabilitySlot) endComputer(gen int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return
	}
	s.computer = false
}

// processTurn runs the one turn Jev, steering, then the capability that Jev picked.
// Partial transcripts do not call Jev. turn.done does, at most once per jevMinInterval.
// Tool side effects run only after a successful final judgment.
func processTurn(ctx context.Context, opt Options, p *persona.Persona,
	jc *jev.Client, lc *llm.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, gate *jevGate, slot *capabilitySlot, userText string, final bool) {
	// Greeting / model self-talk has no user text. Judging those as
	// "low engagement" steers re_engage and commentary-nudges, which
	// makes the duplex model say the same line again.
	if strings.TrimSpace(userText) == "" {
		return
	}
	judged := false
	claimed, epoch := gate.claimEpoch(userText, !final)
	if claimed {
		var obs judge.Observe
		if opt.sense != nil {
			live := opt.sense.Live()
			obs.Camera = live.Camera
			obs.Screen = live.Screen
			obs.Log = opt.sense.LogTail(6)
		}
		jd, err := judge.JudgeTurn(ctx, jc, p, mem, userText, pl.Current(), obs, slot.snapshot())
		if err != nil {
			gate.finish(userText, false)
			opt.log("[judge] skipped: %v", err)
		} else {
			gate.finish(userText, true)
			if !gate.current(epoch) {
				return
			}
			safety := jd.SafetyP >= p.Judge.SafetyThresh
			mem.UpdateAffect(jd.Valence, jd.Arousal, jd.Emotion, safety)
			affect := mem.Affect()
			tag := ""
			if !final {
				tag = " early"
			}
			opt.log("[judge]%s emotion=%s intent=%s self=%s jev_mode=%s attend=%s act=%s valence=%.2f arousal=%.2f engage=%.2f safety=%.2f fit=%.2f need_llm=%.2f conf=%.2f",
				tag, jd.Emotion, orDash(jd.Intent), orDash(jd.SelfEmotion), orDash(jd.Mode), orDash(jd.Attend), orDash(jd.Act),
				jd.Valence, jd.Arousal, jd.Engagement, jd.SafetyP, jd.PersonaFitP, jd.NeedLLMP, jd.Confidence)

			mode := judge.DecideMode(jd, affect, p.Judge.SafetyThresh, pl.Current())
			if final && opt.rel != nil {
				if _, err := opt.rel.ObserveTurn(p.Name, userText, mem.LatestAssistantText(), jd.Emotion, jd.SelfEmotion, mode,
					jd.Valence, jd.Arousal, jd.Engagement, jd.PersonaFitP); err != nil {
					opt.log("[relationship] save failed: %v", err)
				}
			}
			if opt.avatarHub != nil {
				frame := avatar.DriveWithRelationship(mode, jd, affect, relCue(opt.rel))
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
					"mode": mode, "emotion": jd.Emotion, "act": jd.Act,
					"valence": jd.Valence, "arousal": jd.Arousal,
					"engage": jd.Engagement, "early": !final,
				},
			})
			note := steering.BuildWithScene(p, mode, jd, affect, pl.Current(), sceneCue(opt.rel))
			ask := sense.ParseAsk(userText)
			if ask.Kind != "" {
				opt.sense.Set(func(l *sense.Live) { l.LastAsk = ask.Kind })
			}
			felt := ""
			if opt.sense != nil {
				felt = opt.sense.Felt(p, ask, jd.Attend)
			}
			if final {
				note += branchSteer(slot.snapshot(), jd, mode)
			}
			// The gateway rejects one append over 500 tokens. The scene
			// note and the observation (log, camera, screen) go separately
			// so neither one crowds the other out.
			if err := sess.Steer(livevoice.FitHead(note)); err != nil {
				opt.log("[steer] failed: %v", err)
			} else {
				opt.log("[steer] mode=%s", mode)
				opt.sense.Emit(sense.Event{Kind: sense.KindSteer, Summary: mode})
			}
			if felt != "" {
				if err := sess.Steer(livevoice.FitTail(felt)); err != nil {
					opt.log("[steer] observe failed: %v", err)
				}
			}
			gate.remember(jd)
			judged = true
		}
	} else if final && gate.cooling() {
		opt.log("[judge] skip (within %s)", gate.minInterval)
		// A log question still has to land. Cooling must not drop the
		// journal, or she answers as if she cannot see her own log.
		if ask := sense.ParseAsk(userText); opt.sense != nil && (ask.Kind == sense.AskLog || ask.Kind == sense.AskCamera || ask.Kind == sense.AskScreen) {
			attend := ""
			switch ask.Kind {
			case sense.AskCamera:
				attend = "camera"
			case sense.AskScreen:
				attend = "screen"
			}
			if felt := opt.sense.Felt(p, ask, attend); felt != "" {
				if err := sess.Steer(livevoice.FitTail(felt)); err != nil {
					opt.log("[steer] observe failed: %v", err)
				}
			}
		}
	}
	if !final {
		return
	}
	jd := gate.lastJudgment()
	mode := ""
	if jd != nil {
		mode = judge.DecideMode(jd, mem.Affect(), p.Judge.SafetyThresh, pl.Current())
	}
	dispatchCapability(ctx, opt, p, jc, lc, mem, pl, sess, slot, jd, mode, userText, judged)
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

func branchSteer(br judge.Branch, jd *judge.Judgment, mode string) string {
	if jd != nil && br.Open() && jd.BranchDone(judge.DefaultBranchDone) {
		return " The task you were on is finished. You may say so in one short line. Do not invent extra results."
	}
	if br.Open() {
		note := " You are still on " + br.Kind + ". Goal: " + clip(br.Goal, 120) + "."
		if br.Note != "" {
			note += " Progress: " + clip(br.Note, 80) + "."
		}
		return note + " Keep talking with them about that. It is not finished, so do not switch away or claim it is done."
	}
	if jd == nil {
		return ""
	}
	if jd.ComputerUseAllowed(mode) {
		return " She is about to use this computer for what they just asked. You may say so in one short line. Do not claim it is already done."
	}
	if jd.CodexAllowed(mode) {
		return " She is about to change her own source for what they just asked. You may say she has started. Do not claim it is already done."
	}
	if jd.MediaAllowed(mode) {
		return " She is about to make a " + jd.Act + " for what they just asked. You may say she has started. Do not claim it is ready."
	}
	if jd.PerceptAllowed(mode) {
		return " She is about to " + jd.Act + " a file that already exists. You may say she is looking or listening. Do not describe it until the note arrives."
	}
	return ""
}

// dispatchCapability keeps one open branch until the entry Jev says it is done.
// A cooled turn does not open, close, or start work.
func dispatchCapability(ctx context.Context, opt Options, p *persona.Persona,
	jc *jev.Client, lc *llm.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, slot *capabilitySlot, jd *judge.Judgment, mode, userText string, judged bool) {
	if jd == nil || pl == nil {
		return
	}
	thresh := p.Judge.NeedLLMThresh
	open := slot.snapshot()
	act, done := jd.Stay(open.Kind, thresh, judge.DefaultBranchDone)
	if !judged {
		if open.Kind == "" || open.Kind == judge.ActPlan {
			if act == judge.ActPlan || jd.Act == "" {
				pl.Consider(ctx, mem, jd.NeedLLMP)
			}
		}
		return
	}
	if done {
		opt.log("[branch] done %s", orDash(open.Kind))
		slot.close()
		voiceNudge(opt, sess, "The task you were on is finished. Tell them in one or two in-character sentences. Do not read logs or steps.")
	}
	if act == judge.ActNone || act == "" {
		if !done && jd.Act == "" {
			pl.Consider(ctx, mem, jd.NeedLLMP)
		}
		return
	}
	continuing := !done && open.Kind == act
	hands := act == judge.ActComputerUse || act == judge.ActCodex
	making := judge.IsStudio(act)
	sensing := judge.IsPercept(act)
	if (hands || making || sensing) && mode == "safety" {
		opt.log("[branch] held %s", act)
		return
	}
	if making && !continuing && !jd.MediaAllowed(mode) {
		opt.log("[act] %s held mode=%s", act, orDash(mode))
		return
	}
	if sensing && !continuing && !jd.PerceptAllowed(mode) {
		opt.log("[act] %s held mode=%s", act, orDash(mode))
		return
	}
	if hands && !continuing {
		if act == judge.ActComputerUse && !jd.ComputerUseAllowed(mode) {
			opt.log("[act] computer_use held mode=%s", orDash(mode))
			return
		}
		if act == judge.ActCodex && !jd.CodexAllowed(mode) {
			opt.log("[act] codex held mode=%s", orDash(mode))
			return
		}
	}
	if continuing {
		slot.follow(userText)
		opt.log("[branch] stay %s", act)
	} else {
		if slot.busy() {
			slot.close()
		}
		slot.open(act, userText)
		opt.log("[branch] open %s", act)
	}
	switch act {
	case judge.ActComputerUse:
		ensureHands(ctx, opt, jc, lc, mem, sess, slot, act, "")
	case judge.ActCodex:
		ensureHands(ctx, opt, jc, lc, mem, sess, slot, act, "codex")
	case judge.ActImage, judge.ActVideo, judge.ActSpeech, judge.ActSong:
		ensureStudio(ctx, opt, lc, sess, slot, act)
	case judge.ActPicture, judge.ActWatch, judge.ActListen:
		ensurePercept(ctx, opt, lc, sess, slot, act)
	case judge.ActReflect:
		if !continuing {
			startReflect(opt, p, sess, jd, mode, userText)
		}
	case judge.ActCamera:
		startSight(ctx, opt, p, sess, eye.SourceCamera, continuing)
	case judge.ActScreen:
		startSight(ctx, opt, p, sess, eye.SourceScreen, continuing)
	case judge.ActLook:
		if !continuing {
			startLook(opt, p, sess, userText)
		}
	case judge.ActPlan:
		if thresh <= 0 {
			thresh = judge.DefaultNeedLLM
		}
		score := jd.NeedLLMP
		if score < thresh {
			score = thresh
		}
		pl.Consider(ctx, mem, score)
	}
}

func ensureHands(ctx context.Context, opt Options, jc *jev.Client, lc *llm.Client, mem *memory.Memory,
	sess *livevoice.Session, slot *capabilitySlot, kind, driver string) {
	if slot.busy() {
		opt.log("[branch] %s still working", kind)
		return
	}
	ok, gen := slot.tryComputer()
	if !ok {
		opt.log("[branch] %s busy", kind)
		return
	}
	_, goal, _ := slot.current()
	if strings.TrimSpace(goal) == "" {
		slot.endComputer(gen)
		return
	}
	fresh := slot.snapshot().Note == ""
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindDesk, Summary: kind + " start"})
	}
	if fresh {
		if strings.EqualFold(driver, "codex") {
			voiceNudge(opt, sess, "You just started changing your own source. Tell them in one short in-character line that you have begun. Do not say it is finished.")
		} else {
			voiceNudge(opt, sess, "You just started using this computer for their request. Tell them in one short in-character line that you are doing it. Do not say it is finished. Do not describe tools.")
		}
	}
	go func() {
		defer slot.endComputer(gen)
		runHands(ctx, opt, jc, lc, mem, sess, slot, kind, driver)
	}()
}

func runHands(ctx context.Context, opt Options, jc *jev.Client, lc *llm.Client, mem *memory.Memory,
	sess *livevoice.Session, slot *capabilitySlot, kind, driver string) {
	ctx = slot.bind(ctx)
	codex := strings.EqualFold(driver, "codex")
	model := opt.PlannerModel
	if model == "" {
		model = "gpt-5.6-luna"
	}
	cwd := ""
	if opt.sense != nil {
		cwd = opt.sense.Root()
	}
	const maxBursts = 6
	for burst := 1; burst <= maxBursts; burst++ {
		if ctx.Err() != nil {
			return
		}
		cur, goal, _ := slot.current()
		if cur != kind || strings.TrimSpace(goal) == "" {
			return
		}
		runGoal := goal
		if codex {
			runGoal = "Edit this repository, which is your own source. Stay inside the request. Request: " + goal
		}
		opt.log("[branch] %s burst %d %s", kind, burst, clip(goal, 80))
		maxSteps := 4
		deskDriver := ""
		var goalFn func() string
		if codex {
			maxSteps = 1
			deskDriver = "codex"
		} else {
			goalFn = func() string { _, g, _ := slot.current(); return g }
		}
		rep, err := desk.Run(ctx, desk.Options{
			Goal:       runGoal,
			GoalFn:     goalFn,
			Cwd:        cwd,
			Driver:     deskDriver,
			CodexModel: model,
			MaxSteps:   maxSteps,
			Jev:        jc,
			LLM:        lc,
			APIKey:     opt.APIKey,
			LogFn:      func(s string) { opt.log("[desk] %s", s) },
		})
		if ctx.Err() != nil {
			return
		}
		status := "failed"
		if rep != nil {
			status = rep.Status
			opt.log("[desk] status=%s steps=%d", rep.Status, len(rep.Steps))
		}
		if err != nil {
			opt.log("[desk] failed: %v", err)
			status = "failed"
		}
		slot.setNote(kind + " " + status)
		if opt.sense != nil {
			opt.sense.Emit(sense.Event{Kind: sense.KindDesk, Summary: kind + " " + status})
		}
		recent := []string(nil)
		if mem != nil {
			recent = mem.Recent(6)
		}
		p, jerr := judge.JudgeBranchDone(ctx, jc, slot.snapshot(), recent)
		if jerr != nil {
			opt.log("[branch] done-check skipped: %v", jerr)
		} else if p >= judge.DefaultBranchDone {
			opt.log("[branch] done %s (p=%.2f)", kind, p)
			slot.close()
			voiceNudge(opt, sess, "The task you were on is finished. Tell them in one or two in-character sentences. Do not read logs or steps.")
			return
		}
		if status == desk.OpDone || status == desk.OpBlocked || status == "failed" || status == "canceled" {
			opt.log("[branch] %s parked (%s)", kind, status)
			return
		}
	}
	opt.log("[branch] %s parked (burst cap)", kind)
	voiceNudge(opt, sess, "You are still on the task, but this stretch of work paused. Say that briefly, in character. Do not claim it is finished.")
}

func ensureStudio(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, slot *capabilitySlot, kind string) {
	if slot.busy() {
		opt.log("[branch] %s still making", kind)
		return
	}
	ok, gen := slot.tryComputer()
	if !ok {
		opt.log("[branch] %s busy", kind)
		return
	}
	_, goal, _ := slot.current()
	if strings.TrimSpace(goal) == "" {
		slot.endComputer(gen)
		return
	}
	fresh := slot.snapshot().Note == ""
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindMake, Summary: kind + " start"})
	}
	if fresh {
		voiceNudge(opt, sess, "You just started making a "+kind+". Tell them in one short in-character line that it has begun. Do not say it is ready. Do not describe a finished result.")
	}
	go runStudio(ctx, opt, lc, sess, slot, kind, gen)
}

func runStudio(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, slot *capabilitySlot, kind string, gen int) {
	defer slot.endComputer(gen)
	ctx = slot.bind(ctx)
	_, goal, _ := slot.current()
	var completer studio.Completer
	if lc != nil {
		completer = lc
	}
	prompt := studio.Compose(ctx, kind, goal, completer)
	dir := opt.RunsDir
	if dir == "" {
		dir = "runs"
	}
	client := studio.New(opt.BaseURL, opt.APIKey, filepath.Join(dir, "media"))
	var mid bool
	client.OnProgress = func(p studio.Progress) {
		note := fmt.Sprintf("%s %s %.0f%%", p.Kind, p.Status, p.Ratio*100)
		slot.setNote(note)
		if opt.sense != nil {
			opt.sense.Set(func(l *sense.Live) {
				l.MakeKind = p.Kind
				l.MakeStatus = p.Status
				l.MakeProgress = p.Ratio
			})
			opt.sense.Emit(sense.Event{Kind: sense.KindMake, Summary: note})
		}
		if !mid && p.Ratio >= 0.5 && p.Ratio < 1 {
			mid = true
			voiceNudge(opt, sess, "The "+kind+" is about halfway. You can feel that. Mention it only if they ask, in character. Do not say it is ready.")
		}
	}
	res, err := client.Run(ctx, kind, prompt)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		opt.log("[make] %s failed: %v", kind, err)
		slot.setNote(kind + " failed")
		if opt.sense != nil {
			opt.sense.Set(func(l *sense.Live) {
				l.MakeKind = kind
				l.MakeStatus = "failed"
				l.MakeProgress = 0
			})
			opt.sense.Emit(sense.Event{Kind: sense.KindMake, Summary: kind + " failed"})
		}
		voiceNudge(opt, sess, "That did not finish. Say so simply, in character. Do not invent a file.")
		slot.close()
		return
	}
	file := ""
	if len(res.Files) > 0 {
		file = res.Files[0]
	}
	slot.setNote(kind + " ready " + file)
	if opt.sense != nil {
		opt.sense.Set(func(l *sense.Live) {
			l.MakeKind = kind
			l.MakeStatus = "ready"
			l.MakeProgress = 1
			l.MakeFile = file
		})
		opt.sense.Emit(sense.Event{Kind: sense.KindMake, Summary: kind + " ready " + file})
	}
	opt.log("[make] %s ready %s", kind, file)
	voiceNudge(opt, sess, "The "+kind+" is ready, saved as "+file+". Tell them in one or two in-character sentences. Do not invent details that were not asked for.")
	slot.close()
}

func ensurePercept(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, slot *capabilitySlot, kind string) {
	if slot.busy() {
		opt.log("[branch] %s still perceiving", kind)
		return
	}
	ok, gen := slot.tryComputer()
	if !ok {
		opt.log("[branch] %s busy", kind)
		return
	}
	_, goal, _ := slot.current()
	if strings.TrimSpace(goal) == "" {
		slot.endComputer(gen)
		return
	}
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindSee, Summary: kind + " start"})
	}
	voiceNudge(opt, sess, "You are about to "+kind+" something that already exists. Say you are looking or listening, in one short line. Do not describe it yet.")
	go runPercept(ctx, opt, lc, sess, slot, kind, gen)
}

func runPercept(ctx context.Context, opt Options, lc *llm.Client, sess *livevoice.Session, slot *capabilitySlot, kind string, gen int) {
	defer slot.endComputer(gen)
	ctx = slot.bind(ctx)
	_, goal, _ := slot.current()
	dir := opt.RunsDir
	if dir == "" {
		dir = "runs"
	}
	dir = filepath.Join(dir, "media")
	path, err := studio.Resolve(dir, kind, goal)
	if err != nil {
		opt.log("[percept] %s: %v", kind, err)
		voiceNudge(opt, sess, "There is nothing of that kind to perceive yet. Say so simply. Do not invent a picture, a video, or a song.")
		slot.close()
		return
	}
	client := studio.New(opt.BaseURL, opt.APIKey, dir)
	var see studio.Vision
	if lc != nil {
		see = lc
	}
	got, err := client.Perceive(ctx, kind, path, see)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		opt.log("[percept] %s failed: %v", kind, err)
		voiceNudge(opt, sess, "You could not take that in. Say so simply. Do not invent what was in it.")
		slot.close()
		return
	}
	if opt.sense != nil {
		opt.sense.Set(func(l *sense.Live) {
			l.PerceptKind = got.Kind
			l.PerceptFile = got.File
			l.Percept = got.Text
		})
		opt.sense.Emit(sense.Event{Kind: sense.KindSee, Summary: kind + " " + clip(got.Text, 80)})
	}
	slot.setNote(kind + " " + got.File)
	opt.log("[percept] %s %s", kind, got.File)
	voiceSteer(opt, sess, "You "+kind+" "+got.File+". What you actually got: "+clip(got.Text, 400)+". Speak from this only. Do not add pixels, motion, or lyrics that are not written here.")
	voiceNudge(opt, sess, "You have taken it in. Describe it in one or two in-character sentences, only from the note.")
	slot.close()
}

func startReflect(opt Options, p *persona.Persona, sess *livevoice.Session, jd *judge.Judgment, mode, userText string) {
	if opt.sense == nil || p == nil || !p.Sense.Enabled {
		opt.log("[act] reflect unavailable")
		return
	}
	self, tracking := "", ""
	if jd != nil {
		self = jd.SelfEmotion
		tracking = jd.Emotion
	}
	note := fmt.Sprintf("This turn you were steered as %s. Your feeling: %s. You were tracking them as %s. Notice yourself from this. ",
		orDash(mode), orDash(self), orDash(tracking))
	ask := sense.ParseAsk(userText)
	if ask.Kind != sense.AskBody && ask.Kind != sense.AskExistence {
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskBody}, "log"); felt != "" {
			note += felt
		}
	}
	opt.sense.Emit(sense.Event{Kind: sense.KindLook, Summary: "reflect"})
	voiceSteer(opt, sess, note)
	voiceNudge(opt, sess, "They asked you to notice yourself. Answer from that sensation, in character, short. Do not recite a manual.")
}

func startSight(ctx context.Context, opt Options, p *persona.Persona, sess *livevoice.Session, source string, again bool) {
	if opt.eyes == nil {
		opt.log("[act] %s unavailable", source)
		voiceNudge(opt, sess, sightMiss(source))
		return
	}
	go func() {
		g := opt.eyes.Glance(ctx, source)
		opt.log("[act] %s %s", source, clip(g.Caption, 80))
		if opt.sense != nil {
			if felt := opt.sense.Felt(p, sense.Ask{Kind: sightAsk(source)}, source); felt != "" {
				if err := sess.Steer(livevoice.FitTail(felt)); err != nil {
					opt.log("[act] %s steer failed: %v", source, err)
				}
			}
		}
		if again {
			return
		}
		voiceNudge(opt, sess, sightSpoke(source))
	}()
}

func sightAsk(source string) string {
	if source == eye.SourceScreen {
		return sense.AskScreen
	}
	return sense.AskCamera
}

func sightMiss(source string) string {
	if source == eye.SourceScreen {
		return "You cannot look at the screen right now. Say so in character, in one short line. Do not invent what is on it. Do not describe the camera."
	}
	return "You cannot look through the camera right now. Say so in character, in one short line. Do not invent a scene. Do not describe the screen."
}

func sightSpoke(source string) string {
	if source == eye.SourceScreen {
		return "You just looked at the screen. Say what is there in one or two in-character sentences, only from the screen note. It is window titles, not a screenshot. Do not describe the camera. Do not invent."
	}
	return "You just looked through the camera. Say what you see in one or two in-character sentences, only from the camera note. Do not describe the screen. Do not invent."
}

func glanceCommand(ctx context.Context, opt Options, p *persona.Persona, sess *livevoice.Session, source string) {
	if opt.eyes == nil {
		opt.log("%s down (pass -vision and sense.eyes: true)", source)
		return
	}
	g := opt.eyes.Glance(ctx, source)
	opt.log("[%s] %s", source, clip(g.Caption, 80))
	if opt.sense == nil {
		return
	}
	opt.sense.Emit(sense.Event{Kind: sense.KindCommand, Summary: "/" + source})
	if felt := opt.sense.Felt(p, sense.Ask{Kind: sightAsk(source)}, source); felt != "" {
		if err := sess.Steer(livevoice.FitTail(felt)); err != nil {
			opt.log("[%s] steer failed: %v", source, err)
		}
	}
}

func startLook(opt Options, p *persona.Persona, sess *livevoice.Session, userText string) {
	if opt.sense == nil {
		opt.log("[act] look unavailable")
		return
	}
	ask := sense.ParseAsk(userText)
	if ask.Kind != sense.AskFile && ask.Kind != sense.AskCode {
		ask = sense.Ask{Kind: sense.AskCode}
		opt.log("[act] look")
		if felt := opt.sense.Felt(p, ask, ""); felt != "" {
			voiceSteer(opt, sess, felt)
		}
	} else {
		opt.log("[act] look already in the turn note")
	}
	opt.sense.Emit(sense.Event{Kind: sense.KindLook, Summary: "look"})
	voiceNudge(opt, sess, "They asked how you are made. Answer from the note about your own code, in character, short. Do not recite source unless they asked to hear a line.")
}

func voiceSteer(opt Options, sess *livevoice.Session, text string) {
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	if err := sess.Steer(text); err != nil {
		opt.log("[act] steer failed: %v", err)
	}
}

func voiceNudge(opt Options, sess *livevoice.Session, text string) {
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	if err := sess.Nudge(text); err != nil {
		opt.log("[act] nudge failed: %v", err)
	}
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
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskBody}, ""); felt != "" {
			if err := sess.Steer(felt); err != nil {
				opt.log("[sense] steer failed: %v", err)
			}
		}
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskLog}, "log"); felt != "" {
			if err := sess.Steer(felt); err != nil {
				opt.log("[sense] log steer failed: %v", err)
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
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskFile, File: view.Path}, ""); felt != "" {
			if err := sess.Steer(felt); err != nil {
				opt.log("[look] steer failed: %v", err)
			}
		}
	case line == "/camera", line == "/see camera":
		glanceCommand(ctx, opt, p, sess, eye.SourceCamera)
	case line == "/screen", line == "/see screen":
		glanceCommand(ctx, opt, p, sess, eye.SourceScreen)
	case line == "/eyes" || strings.HasPrefix(line, "/eyes "):
		if opt.eyes == nil {
			opt.log("eyes down (pass -vision=both and sense.eyes: true)")
			break
		}
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/eyes"))
		switch arg {
		case "", "toggle":
			opt.eyes.SetEnabled(!opt.eyes.Enabled())
		case "status":
			if opt.eyes.Enabled() {
				opt.log("[eyes] on")
			} else {
				opt.log("[eyes] off")
			}
		case "on", "1", "true":
			opt.eyes.SetEnabled(true)
		case "off", "0", "false":
			opt.eyes.SetEnabled(false)
		default:
			opt.log("usage: /eyes on|off")
		}
	case line == "/see":
		opt.log("camera and screen are separate: /camera or /screen")
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
		opt.log("unknown command (try /say /steer /goal /look /camera /screen /eyes /sense /desk /codex /status /quit)")
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
	if o.rel != nil {
		payload["relationship"] = o.rel.Snapshot()
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

func relationshipPath(dir, personaName string) string {
	if strings.TrimSpace(dir) == "" {
		dir = "runs"
	}
	return filepath.Join(dir, "relationship-"+slugify(personaName)+".json")
}

func relCue(store *memory.RelationshipStore) memory.RelationshipCue {
	if store == nil {
		return memory.RelationshipCue{}
	}
	return store.Cue()
}

func sceneCue(store *memory.RelationshipStore) steering.SceneCue {
	if store == nil {
		return steering.SceneCue{}
	}
	c := store.Cue()
	return steering.SceneCue{
		Stage:        c.Stage,
		Summary:      c.Summary,
		OpenLoops:    append([]string(nil), c.OpenLoops...),
		SharedEvents: append([]string(nil), c.SharedEvents...),
	}
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		if r == ' ' {
			b.WriteByte('-')
			continue
		}
		if r > 127 {
			b.WriteString(fmt.Sprintf("-%x", r))
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "persona"
	}
	return out
}

func (o Options) log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if o.LogFn != nil {
		o.LogFn(line)
	} else {
		fmt.Printf("%s\n", line)
	}
	if o.sense != nil {
		o.sense.Note(line)
	}
	if o.avatarHub != nil && viewerLog(line) {
		o.avatarHub.Log(line)
	}
}

// viewerLog is the operator trace on the Live2D page. It keeps the
// decision path (judge, steering, branch, plan) and drops frame-rate noise.
func viewerLog(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.Contains(line, "uplink:") {
		return false
	}
	if rest, ok := strings.CutPrefix(line, "[eye] "); ok {
		if strings.HasPrefix(rest, "camera: ") || strings.HasPrefix(rest, "screen: ") {
			return false
		}
	}
	return true
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
