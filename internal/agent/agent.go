// Package agent orchestrates the full loop:
// duplex voice -> Jev turn judgment -> steering -> async planner.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jingx8885/lov-evo/internal/audio"
	"github.com/jingx8885/lov-evo/internal/avatar"
	"github.com/jingx8885/lov-evo/internal/desk"
	"github.com/jingx8885/lov-evo/internal/eye"
	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/livevoice"
	"github.com/jingx8885/lov-evo/internal/llm"
	"github.com/jingx8885/lov-evo/internal/logq"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/oracle"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/planner"
	"github.com/jingx8885/lov-evo/internal/react"
	"github.com/jingx8885/lov-evo/internal/sense"
	"github.com/jingx8885/lov-evo/internal/steering"
	"github.com/jingx8885/lov-evo/internal/studio"
	"github.com/jingx8885/lov-evo/internal/window"
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
	avatarHub    *avatar.Hub
	sense        *sense.Bus
	eyes         *eye.Eyes
	rel          *memory.RelationshipStore
	power        *power
	deskWin      *window.Registry
	stageQ       *window.Stage
	voice        *voiceHold
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

	opt.voice = &voiceHold{}
	media := mediaDir(opt)
	st := window.NewStage(window.StageOptions{
		Runner:  stageRunner(opt.BaseURL, opt.APIKey, media, runsDir(opt), llmClient, codexRunner(opt)),
		Changed: func() { rememberStage(opt) },
	})
	reg := window.New(window.Options{
		MediaDir: media,
		PlayDir:  filepath.Join(runsDir(opt), "codex"),
		Open:     window.OpenBrowser,
		LogFn:    func(s string) { opt.log("%s", s) },
	})
	reg.Register(st)
	opt.stageQ = st
	go func() {
		missing, err := studio.New(opt.BaseURL, opt.APIKey, media).Missing(ctx)
		if err == nil && len(missing) > 0 {
			opt.log("[studio] gateway does not offer: %s; those jobs will fail until it is enabled", strings.Join(missing, ", "))
		}
	}()
	opt.deskWin = reg
	if _, err := reg.Listen(ctx, "127.0.0.1:0"); err != nil {
		opt.log("stage window disabled: %v", err)
	}
	defer reg.Close()

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
			Camera: cam,
			Screen: scr,
			Grab: func(context.Context) bool {
				if opt.avatarHub == nil {
					return false
				}
				opt.avatarHub.RequestCapture()
				return true
			},
			GrabShot: func(context.Context) bool {
				if opt.avatarHub == nil {
					return false
				}
				opt.avatarHub.RequestShot()
				return true
			},
			Jev: jevClient,
			LLM: vis,
			Observe: func(ctx context.Context) (eye.ScreenView, error) {
				g, err := desk.Look(ctx, jevClient, host, "")
				if err != nil {
					return eye.ScreenView{}, err
				}
				return eye.ScreenView{Caption: g.Caption, Signature: g.Signature, Private: g.Private}, nil
			},
			Capture: eye.CaptureDesktop,
			LogFn:   func(s string) { opt.log("[eye] %s", s) },
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
					if s.Shot.Caption != "" {
						l.Shot = s.Shot.Caption
					}
				})
				sum := s.Camera.Caption
				if s.Screen.Caption != "" {
					if sum != "" {
						sum += " | "
					}
					sum += s.Screen.Caption
				}
				if s.Shot.Caption != "" {
					if sum != "" {
						sum += " | "
					}
					sum += s.Shot.Caption
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
		}
	}

	opt.power = newPower(ctx, func(s string) { opt.log("%s", s) })
	if opt.avatarHub != nil {
		pw := opt.power
		opt.avatarHub.SetSystem(pw.On, pw.Set)
		st := opt.stageQ
		reg := opt.deskWin
		opt.avatarHub.SetQueue(func() any {
			if st == nil {
				return window.QueueView{Jobs: []window.Job{}}
			}
			media := ""
			open := false
			if reg != nil {
				media = reg.URL()
				open = reg.Open("stage")
			}
			return st.QueueView(open, media)
		})
		if opt.rel != nil {
			rel := opt.rel
			opt.avatarHub.SetMemory(rel.View, func(op memory.MemoryOp) (memory.MemoryView, error) {
				view, err := rel.Apply(op)
				if err != nil {
					opt.log("[memory] %s: %v", op.Op, err)
					return view, err
				}
				label := strings.TrimSpace(op.Text)
				if label == "" {
					label = op.ID
				}
				opt.log("[memory] %s %s", op.Op, clip(label, 40))
				return view, nil
			})
		}
	}

	cmds := make(chan string, 16)
	go readStdin(ctx, cmds)
	opt.log("commands: /say <text> (speak) /steer <text> (reinstruct) " +
		"/goal <text> (plan) /look <file> (her source) /camera /screen /shot /sense /desk <goal> " +
		"/stage (page) /codex <goal> (luna) /divine <question> /off /on /status /quit")

	first := true
	var lastErr error
	var droppedAt time.Time
	// One branch slot for the whole run. A reconnect must not let a second
	// hands task start while the first one is still clicking.
	slot := &capabilitySlot{}
	for {
		if err := ctx.Err(); err != nil {
			return opt.saveLog(mem, pl, lastErr)
		}
		for opt.power != nil && !opt.power.On() {
			select {
			case <-ctx.Done():
				return opt.saveLog(mem, pl, ctx.Err())
			case <-opt.power.wake:
			case line := <-cmds:
				if opt.handlePaused(line) {
					return opt.saveLog(mem, pl, lastErr)
				}
			}
		}
		sess, err := livevoice.Connect(ctx, opt.BaseURL, opt.APIKey,
			p.BaseInstructions(), p.Voice)
		if err != nil {
			lastErr = err
			opt.log("voice connect failed: %v; retry in 2s", err)
			if !opt.afterVoiceGap(ctx) {
				return opt.saveLog(mem, pl, err)
			}
			continue
		}
		opt.power.Arm(sess.Close)
		sess.Verbose = opt.Verbose
		sess.DiscardPCM()
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
			opt.power.Disarm()
			sess.Close()
			lastErr = err
			if !opt.power.On() {
				continue
			}
			opt.log("session did not start: %v; retry in 2s", err)
			if !opt.afterVoiceGap(ctx) {
				return opt.saveLog(mem, pl, err)
			}
			continue
		}
		opt.log("session started; persona=%s voice=%s", p.Name, p.Voice)
		opt.sense.Set(func(l *sense.Live) { l.Voice = "up" })
		opt.sense.Emit(sense.Event{Kind: sense.KindVoice, Summary: "up"})

		if first {
			if opt.Greeting && p.Greeting != "" {
				if err := sess.Speak(p.Greeting); err != nil && !errors.Is(err, livevoice.ErrHeld) {
					opt.log("greeting failed: %v", err)
				}
			}
			if s := strings.TrimSpace(opt.Say); s != "" {
				if err := sess.Speak(s); err != nil && !errors.Is(err, livevoice.ErrHeld) {
					opt.log("say failed: %v", err)
				} else {
					opt.log("say: %s", s)
				}
			}
			first = false
		} else if time.Since(droppedAt) > catchUpWithin {
			opt.log("[steer] no catch-up: away %s", time.Since(droppedAt).Round(time.Second))
		} else if note := catchUp(mem); note != "" {
			if err := sess.Steer(note); err != nil && !errors.Is(err, livevoice.ErrHeld) {
				opt.log("[steer] catch-up failed: %v", err)
			} else {
				opt.log("[steer] catch-up after reconnect")
			}
		}

		res := pumpSession(ctx, opt, p, jevClient, llmClient, mem, pl, sess, slot, cmds)
		droppedAt = time.Now()
		sess.Close()
		opt.power.Disarm()
		opt.sense.Set(func(l *sense.Live) { l.Voice = "down" })
		opt.sense.Emit(sense.Event{Kind: sense.KindVoice, Summary: "down"})
		switch res.kind {
		case pumpQuit, pumpCancel:
			return opt.saveLog(mem, pl, res.err)
		default:
			lastErr = res.err
			if !opt.power.On() {
				continue
			}
			opt.log("voice session dropped: %v; reconnecting in 2s", res.err)
			if !opt.afterVoiceGap(ctx) {
				return opt.saveLog(mem, pl, res.err)
			}
		}
	}
}

// catchUpWithin is how soon after a drop the new call still continues it.
const catchUpWithin = 5 * time.Minute

// catchUp is the silent note a new call gets after a drop: the last few
// lines, so she picks the thread up when they speak instead of starting over.
func catchUp(mem *memory.Memory) string {
	lines := mem.Recent(6)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The call dropped and came back mid-conversation. The last lines were:\n")
	for _, ln := range lines {
		b.WriteString(clip(ln, 80) + "\n")
	}
	b.WriteString("When they speak, continue from there. Do not greet, reintroduce yourself, or read this aloud.")
	return b.String()
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
	sess *livevoice.Session, slot *capabilitySlot, cmds <-chan string) pumpResult {
	if opt.voice != nil {
		opt.voice.bind(sess)
		defer opt.voice.bind(nil)
	}
	sess.SetNote(func(m string) { opt.log("%s", m) })
	var lastTurnKey string
	var lastTurnAt time.Time
	var lastPartial string
	var burstText string
	var burstAt time.Time
	gate := newJevGate()
	// A judge timer that fires after the drop would steer a closed session.
	defer gate.cancelTimer()
	// armJudge waits out a burst of turn.done slices, then judges once.
	// Partial transcripts must not call it.
	armJudge := func(text string) {
		if !judgeWorth(text) {
			return
		}
		gate.schedule(judgeQuiet, text, func() {
			processTurn(ctx, opt, p, jevClient, llmClient, mem, pl, sess, gate, slot, text, true)
		})
	}
	if opt.power != nil {
		opt.power.SetStop(slot.close)
	}
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
				lastPartial = ""
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
				if ev.Text != "" && sameBurst(burstText, ev.Text, burstAt) {
					opt.voice.noteCover(burstText)
					opt.log("[delegation] turn already handed off")
				} else {
					if ev.Text != "" {
						burstText = strings.Join(strings.Fields(ev.Text), " ")
						burstAt = time.Now()
					}
					armJudge(ev.Text)
				}
			case livevoice.EventDelegation:
				ask := strings.TrimSpace(ev.Text)
				opt.voice.setDelegation(ev.ID, ask)
				opt.log("[delegation] %s", clip(ask, 120))
				// She said a filler and waits for an answer on this id; if no
				// work line comes, the fallback answers it.
				awaitHandoff(opt, sess, slot, ev.ID, delegWait)
				if ask == "" {
					continue
				}
				if sameBurst(burstText, ask, burstAt) {
					opt.voice.noteCover(burstText)
					opt.log("[delegation] same turn, judge already scheduled")
					continue
				}
				burstText = strings.Join(strings.Fields(ask), " ")
				burstAt = time.Now()
				opt.voice.noteCover(burstText)
				if mem.LatestUserText() != ask {
					mem.Add(memory.Turn{Speaker: "user", Text: ask})
					opt.log("[user] %s", clip(ask, 120))
				}
				armJudge(ask)
			case livevoice.EventTranscript:
				if ev.Speaker == "user" {
					shown := clip(ev.Text, 120)
					// Once the visible prefix stops changing, further
					// deltas are the same line. Logging each one stalled
					// the turn loop and the Live2D trace.
					if shown != lastPartial {
						lastPartial = shown
						opt.log("[user~] %s", shown)
					}
					// A pause in the partial used to be judged as a finished
					// turn. Developer steering then landed while they were
					// still talking, and that channel does not make her speak.
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

// judgeQuiet waits out a VAD burst. A pause shorter than this is still
// the same utterance, so it must not spend another System One call.
const judgeQuiet = 1200 * time.Millisecond

// earlyMinRunes keeps a one-character ASR fragment from spending a Jev call.
const earlyMinRunes = 8

// judgeWorth reports whether this text may spend a System One call.
// Mouth noise, backchannels, and clipped fragments are not turns. Three
// Chinese characters already carry a request ("画只猫", "几点了").
func judgeWorth(text string) bool {
	n := strings.Join(strings.Fields(text), " ")
	if n == "" {
		return false
	}
	lower := strings.ToLower(n)
	if strings.Contains(lower, "[mouth") || strings.Contains(lower, "[tongue") || strings.Contains(lower, "[click") || strings.Contains(lower, "noise]") {
		return false
	}
	han, filler, other := 0, 0, 0
	for _, r := range n {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
			if strings.ContainsRune(backchannelHan, r) {
				filler++
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			other++
		}
	}
	if han > 0 && han == filler && other == 0 {
		return false
	}
	return han >= 3 || len([]rune(n)) >= earlyMinRunes
}

// backchannelHan is what a reply made only of acknowledgement is spelled with.
const backchannelHan = "嗯哦噢喔啊呃额哈呵嘿唉诶欸哎好对是的呀吧呢嘛啦"

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
	return g.cooldownLeft() > 0
}

func (g *jevGate) cooldownLeft() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.minInterval <= 0 || g.lastCall.IsZero() {
		return 0
	}
	if left := g.minInterval - time.Since(g.lastCall); left > 0 {
		return left
	}
	return 0
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
	hold      string
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
	s.hold = ""
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

// noteFor sets the note only while kind is still the open branch, so a
// late look does not overwrite the branch that replaced it.
func (s *capabilitySlot) noteFor(kind, note string) {
	if s == nil || kind == "" {
		return
	}
	s.mu.Lock()
	if s.kind == kind {
		s.note = strings.TrimSpace(note)
	}
	s.mu.Unlock()
}

func (s *capabilitySlot) setHold(v string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.hold = v
	s.mu.Unlock()
}

func (s *capabilitySlot) holdText() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hold
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
	s.hold = ""
	s.computer = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *capabilitySlot) closeIf(kind string) {
	if s == nil || kind == "" {
		return
	}
	s.mu.Lock()
	if s.kind != kind {
		s.mu.Unlock()
		return
	}
	cancel := s.runCancel
	s.runCancel = nil
	s.gen++
	s.kind = ""
	s.goal = ""
	s.note = ""
	s.hold = ""
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
// Steering and branch notes share one developer inject. If she is still
// speaking, livevoice queues the note and merges later ones, then flushes
// after the line. Commentary and speakable wait on the same queue.
func processTurn(ctx context.Context, opt Options, p *persona.Persona,
	jc *jev.Client, lc *llm.Client, mem *memory.Memory, pl *planner.Planner,
	sess *livevoice.Session, gate *jevGate, slot *capabilitySlot, userText string, final bool) {
	if opt.power != nil {
		ctx = opt.power.Ctx()
		if !opt.power.On() || ctx.Err() != nil {
			return
		}
	}
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
		if opt.deskWin != nil {
			obs.Window = windowView(opt.deskWin.Spec())
		}
		if opt.rel != nil {
			obs.Remembered = opt.rel.ForJudge()
		}
		jd, err := judge.JudgeTurn(ctx, jc, p, mem, userText, pl.Current(), obs, slot.snapshot())
		if err != nil {
			gate.finish(userText, false)
			opt.log("[judge] skipped: %v", err)
		} else {
			gate.finish(userText, true)
			// A later VAD slice used to bump the epoch and throw this
			// result away, then the next slice paid for another call.
			// Speculative results still have to lose to turn.done.
			if !final && !gate.current(epoch) {
				return
			}
			safety := jd.SafetyP >= p.Judge.SafetyThresh
			mem.UpdateAffect(jd.Valence, jd.Arousal, jd.Emotion, safety)
			affect := mem.Affect()
			tag := ""
			if !final {
				tag = " early"
			}
			opt.log("[judge]%s emotion=%s intent=%s self=%s jev_mode=%s attend=%s act=%s window=%s op=%s valence=%.2f arousal=%.2f engage=%.2f safety=%.2f fit=%.2f need_llm=%.2f keep=%.2f conf=%.2f",
				tag, jd.Emotion, orDash(jd.Intent), orDash(jd.SelfEmotion), orDash(jd.Mode), orDash(jd.Attend), orDash(jd.Act),
				orDash(jd.Window), orDash(jd.WinOp),
				jd.Valence, jd.Arousal, jd.Engagement, jd.SafetyP, jd.PersonaFitP, jd.NeedLLMP, jd.KeepP, jd.Confidence)

			mode := judge.DecideMode(jd, affect, p.Judge.SafetyThresh, pl.Current())
			if final && opt.rel != nil {
				task := taskAct(jd.Act)
				if _, err := opt.rel.Observe(p.Name, memory.Observed{
					User: userText, Assistant: mem.LatestAssistantText(),
					UserEmotion: jd.Emotion, SelfEmotion: jd.SelfEmotion,
					Mode: mode, Intent: jd.Intent, Task: task,
					Valence: jd.Valence, Arousal: jd.Arousal, Engagement: jd.Engagement, PersonaFit: jd.PersonaFitP,
				}); err != nil {
					opt.log("[relationship] save failed: %v", err)
				} else if mode != "safety" && !task && jd.WantKeep(judge.DefaultKeep) {
					foldMemory(opt, lc, userText, mem.LatestAssistantText(), mode)
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
			note := steering.BuildWithScene(p, mode, jd, affect, pl.Current(), sceneCue(opt.rel, userText, mode))
			ask := sense.ParseAsk(userText)
			if ask.Kind != "" {
				opt.sense.Set(func(l *sense.Live) { l.LastAsk = ask.Kind })
			}
			jd.BindLook(slot.snapshot().Kind)
			if final && opt.deskWin != nil && mode != "safety" && jd.WinOp != "" && jd.WinOp != window.OpNone {
				if jd.WinOp == window.OpDecorate || jd.WinOp == window.OpAddControl {
					opt.log("[window] %s %s", orDash(jd.Window), jd.WinOp)
					go dressStage(ctx, opt, lc, sess, userText, jd.WinOp)
				} else if _, err := opt.deskWin.Apply(jd.Window, jd.WinOp, jd.WinTarget); err != nil {
					opt.log("[window] %s %s: %v", orDash(jd.Window), jd.WinOp, err)
				} else {
					opt.log("[window] %s %s", orDash(jd.Window), jd.WinOp)
					rememberStage(opt)
				}
			}
			felt := ""
			if opt.sense != nil {
				felt = opt.sense.Felt(p, ask, jd.Attend)
			}
			if final {
				note += branchSteer(slot.snapshot(), jd, mode)
			}
			// A handoff turn is answered on the delegation, not with a scene
			// essay. Safety, comfort, and de-escalation still steer.
			// The gateway rejects one append over 500 tokens. The scene
			// note and the observation (log, camera, screen) go separately
			// so neither one crowds the other out. While she is speaking
			// they queue and merge into one developer flush.
			if opt.voice.handoffTurn(userText) && !modeNeedsDirector(mode) {
				opt.log("[steer] handoff mode=%s", mode)
			} else {
				if err := sess.Steer(livevoice.FitHead(note)); errors.Is(err, livevoice.ErrHeld) {
					opt.log("[steer] queued mode=%s", mode)
					opt.sense.Emit(sense.Event{Kind: sense.KindSteer, Summary: mode})
				} else if err != nil {
					opt.log("[steer] failed: %v", err)
				} else {
					opt.log("[steer] mode=%s", mode)
					opt.sense.Emit(sense.Event{Kind: sense.KindSteer, Summary: mode})
				}
				if felt != "" {
					if err := sess.Steer(livevoice.FitTail(felt)); err != nil && !errors.Is(err, livevoice.ErrHeld) {
						opt.log("[steer] observe failed: %v", err)
					} else if errors.Is(err, livevoice.ErrHeld) {
						opt.log("[steer] observe queued")
					}
				}
			}
			gate.remember(jd)
			judged = true
		}
	} else if final && gate.cooling() {
		// A log question still has to land. Cooling must not drop the
		// journal, or she answers as if she cannot see her own log.
		if ask := sense.ParseAsk(userText); opt.sense != nil && (ask.Kind == sense.AskLog || ask.Kind == sense.AskCamera || ask.Kind == sense.AskScreen || ask.Kind == sense.AskShot || ask.Kind == sense.AskWindow) {
			attend := ""
			switch ask.Kind {
			case sense.AskCamera:
				attend = "camera"
			case sense.AskScreen:
				attend = "screen"
			}
			if felt := opt.sense.Felt(p, ask, attend); felt != "" {
				if err := sess.Steer(livevoice.FitTail(felt)); err != nil && !errors.Is(err, livevoice.ErrHeld) {
					opt.log("[steer] observe failed: %v", err)
				} else if errors.Is(err, livevoice.ErrHeld) {
					opt.log("[steer] observe queued")
				}
			}
		}
		// Dropping a cooled turn lost requests said a few seconds after
		// the last call. Judge the latest one once the interval has passed;
		// a newer turn.done replaces it, so the rate stays the same.
		if wait := gate.cooldownLeft(); wait > 0 {
			opt.log("[judge] cooling; retry in %s", wait.Round(100*time.Millisecond))
			gate.schedule(wait+50*time.Millisecond, userText, func() {
				processTurn(ctx, opt, p, jc, lc, mem, pl, sess, gate, slot, userText, true)
			})
			return
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
				quiet := "A plan note is ready. Do not bring it up on your own. Use it only if they ask what you are thinking: " + note
				if err := sess.Steer(quiet); errors.Is(err, livevoice.ErrHeld) {
					opt.log("[steer] queued")
				} else if err != nil {
					opt.log("[plan] steer failed: %v", err)
				} else {
					opt.log("[plan] %s", clip(note, 80))
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
	leaving := jd != nil && jd.Yields(br.Kind)
	if jd != nil && br.Open() && jd.BranchDone(judge.DefaultBranchDone) && !leaving {
		return " The task you were on is finished. You may say so in one short line. Do not invent extra results."
	}
	if br.Open() && !leaving && isLook(br.Kind) {
		return " You are looking at the " + br.Kind + " with them. Answer from the latest " + br.Kind + " note only; if it does not show what they ask, say so."
	}
	if br.Open() && !leaving {
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
		if react.WantsChange(jd.UserText) {
			return " She is about to change her own source for what they just asked. You may say she has started. Do not claim it is already done."
		}
		return " She is about to have Codex write that program in its own folder. You may say she has started. Do not claim it is already done."
	}
	if jd.MediaAllowed(mode) {
		return " She is about to make a " + jd.Act + " for what they just asked. You may say she has started. Do not claim it is ready."
	}
	if jd.PerceptAllowed(mode) {
		return " She is about to " + jd.Act + " a file that already exists. You may say she is looking or listening. Do not describe it until the note arrives."
	}
	return ""
}

// dispatchCapability keeps one open branch for follow-ups until branch_done.
// An explicit different act leaves that branch immediately and starts the new one.
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
	// A turn she handed over gets an answer even when no work line comes:
	// at once when nothing starts, after a short grace on a follow-up.
	// A question she can already feel (her log, her body, the stage) is
	// that answer. The empty "nothing ran" line is only for everything else.
	handoff := opt.voice.handoffTurn(userText)
	settle := func(wait time.Duration) {
		if !handoff {
			return
		}
		id := opt.voice.delegation()
		if wait == 0 {
			if line := senseReply(opt, p, userText); line != "" {
				voiceAnswer(opt, sess, id, "commentary", livevoice.FitTail(line))
				if !opt.voice.unanswered(id) {
					return
				}
			}
		}
		awaitHandoff(opt, sess, slot, id, wait)
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
		settle(0)
		return
	}
	continuing := !done && open.Kind == act
	hands := act == judge.ActComputerUse || act == judge.ActCodex
	making := judge.IsStudio(act)
	sensing := judge.IsPercept(act)
	telling := act == judge.ActDivine
	if (hands || making || sensing || telling) && mode == "safety" {
		opt.log("[branch] held %s", act)
		settle(0)
		return
	}
	if making && !continuing && !jd.MediaAllowed(mode) {
		opt.log("[act] %s held mode=%s", act, orDash(mode))
		settle(0)
		return
	}
	if sensing && !continuing && !jd.PerceptAllowed(mode) {
		opt.log("[act] %s held mode=%s", act, orDash(mode))
		settle(0)
		return
	}
	if telling && !continuing && !jd.DivineAllowed(mode) {
		opt.log("[act] divine held mode=%s", orDash(mode))
		settle(0)
		return
	}
	if hands && !continuing {
		if act == judge.ActComputerUse && !jd.ComputerUseAllowed(mode) {
			opt.log("[act] computer_use held mode=%s", orDash(mode))
			settle(0)
			return
		}
		if act == judge.ActCodex && !jd.CodexAllowed(mode) {
			opt.log("[act] codex held mode=%s", orDash(mode))
			settle(0)
			return
		}
	}
	// owned is work that answers this handoff itself when it lands.
	owned := false
	if continuing {
		slot.follow(userText)
		opt.log("[branch] stay %s", act)
		// Anything else gets a status answer after a short grace.
		defer func() {
			if !owned {
				settle(handoffGrace)
			}
		}()
	} else {
		if !handoff {
			opt.voice.retireAnswered()
		}
		if open.Kind != "" && open.Kind != act {
			opt.log("[branch] leave %s", open.Kind)
			slot.close()
		} else if slot.busy() {
			slot.close()
		}
		slot.open(act, userText)
		opt.log("[branch] open %s", act)
	}
	switch act {
	case judge.ActComputerUse:
		ensureHands(ctx, opt, jc, lc, mem, sess, slot, act, "")
	case judge.ActImage, judge.ActVideo, judge.ActSpeech, judge.ActSong:
		if opt.stageQ != nil {
			startQueued(ctx, opt, nil, lc, sess, slot, act, continuing)
		} else {
			ensureStudio(ctx, opt, lc, sess, slot, act)
		}
	case judge.ActPicture, judge.ActWatch, judge.ActListen:
		ensurePercept(ctx, opt, lc, sess, slot, act)
	case judge.ActCodex:
		// Asking her to edit herself stays on the checked self path; any
		// other program is written by Codex in a scratch dir on the queue.
		_, goal, _ := slot.current()
		if !react.WantsChange(goal) {
			if opt.stageQ != nil {
				startQueued(ctx, opt, nil, lc, sess, slot, act, continuing)
			} else {
				opt.log("[act] codex program needs the stage queue")
				slot.closeIf(act)
			}
		} else if selfAgain(jd, act, continuing) {
			ensureSelf(ctx, opt, p, jc, lc, sess, slot, jd, act, mode)
		} else {
			opt.log("[self] %s follow-up, no new stretch", act)
		}
	case judge.ActReflect, judge.ActLook:
		if selfAgain(jd, act, continuing) {
			ensureSelf(ctx, opt, p, jc, lc, sess, slot, jd, act, mode)
		} else {
			opt.log("[self] %s follow-up, no new stretch", act)
		}
	case judge.ActCamera:
		owned = lookAgain(continuing, handoff, userText) && startSight(ctx, opt, p, sess, slot, eye.SourceCamera, continuing, userText)
	case judge.ActScreen:
		owned = lookAgain(continuing, handoff, userText) && startSight(ctx, opt, p, sess, slot, eye.SourceScreen, continuing, userText)
	case judge.ActShot:
		owned = lookAgain(continuing, handoff, userText) && startSight(ctx, opt, p, sess, slot, eye.SourceShot, continuing, userText)
	case judge.ActDivine:
		ensureDivine(ctx, opt, p, lc, sess, slot, userText, continuing)
	case judge.ActPlan:
		if opt.stageQ != nil {
			startQueued(ctx, opt, pl, lc, sess, slot, judge.ActPlan, continuing)
		} else if thresh <= 0 {
			thresh = judge.DefaultNeedLLM
		}
		if opt.stageQ != nil {
			break
		}
		score := jd.NeedLLMP
		if score < thresh {
			score = thresh
		}
		pl.Consider(ctx, mem, score)
		settle(0)
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
	const maxBursts = 2
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
		maxSteps := 2
		deskDriver := ""
		var goalFn func() string
		if codex {
			maxSteps = 1
			deskDriver = "codex"
		} else {
			goalFn = func() string { _, g, _ := slot.current(); return g }
		}
		deskOpt := desk.Options{
			Goal:       runGoal,
			GoalFn:     goalFn,
			Cwd:        cwd,
			Driver:     deskDriver,
			CodexModel: model,
			MaxSteps:   maxSteps,
			APIKey:     opt.APIKey,
			LogFn:      func(s string) { opt.log("[desk] %s", s) },
		}
		// A nil pointer stored in the interface is not a client. Desk would
		// snapshot the machine and then panic inside Evaluate.
		if jc != nil {
			deskOpt.Jev = jc
		}
		if lc != nil {
			deskOpt.LLM = lc
		}
		rep, err := desk.Run(ctx, deskOpt)
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
		if status == desk.OpDone {
			opt.log("[branch] done %s", kind)
			slot.close()
			voiceNudge(opt, sess, "The computer task finished. Mention it only if they ask. Do not read logs or steps.")
			return
		}
		if status == desk.OpBlocked || status == "failed" || status == "canceled" {
			opt.log("[branch] %s parked (%s)", kind, status)
			return
		}
	}
	opt.log("[branch] %s parked (burst cap)", kind)
	voiceNudge(opt, sess, "This stretch of computer work paused. Mention it only if they ask. Do not claim it is finished.")
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

// startSight takes one look for this turn and reports whether it started.
// A turn she handed off is answered on that delegation with what she saw:
// she said a filler and is waiting for it. Any other look stays quiet
// reference, so one ask is not spoken twice.
func startSight(ctx context.Context, opt Options, p *persona.Persona, sess *livevoice.Session, slot *capabilitySlot,
	source string, again bool, userText string) bool {
	if opt.eyes == nil {
		opt.log("[act] %s unavailable", source)
		voiceNudge(opt, sess, sightMiss(source))
		return false
	}
	kind, goal, _ := slot.current()
	question := lookAsk(goal, userText)
	bound := ""
	if opt.voice.handoffTurn(userText) {
		bound = opt.voice.delegation()
	}
	slot.noteFor(kind, lookPending)
	go func() {
		look := opt.eyes.GlanceAsk
		if again {
			look = opt.eyes.LookCloser
		}
		g := look(ctx, source, question)
		if ctx.Err() != nil {
			return
		}
		opt.log("[act] %s %s", source, clip(g.Caption, 80))
		slot.noteFor(kind, lookDone)
		spoke := sightSpoke(source, g.Ready && strings.TrimSpace(g.Caption) != "", again, userText)
		felt := ""
		if opt.sense != nil {
			felt = opt.sense.Felt(p, sense.Ask{Kind: sightAsk(source)}, source)
		}
		if felt == "" && strings.TrimSpace(g.Caption) != "" {
			felt = "What the " + source + " showed: " + g.Caption
		}
		// One append only: the gateway may start a line on the first one,
		// and a second append landing mid-line cuts it off.
		if id := lookAnswers(opt.voice, bound, userText); id != "" {
			voiceAnswer(opt, sess, id, "commentary", livevoice.FitTail(strings.TrimSpace(felt+"\n"+spoke)))
			return
		}
		note := livevoice.FitTail(strings.TrimSpace(felt + "\n" + nudgeLead + spoke))
		live := opt.voice.live(sess)
		if live == nil {
			opt.log("[act] %s steer skipped", source)
		} else if err := live.Steer(note); errors.Is(err, livevoice.ErrHeld) {
			opt.log("[steer] queued")
		} else if err != nil {
			opt.log("[act] %s steer failed: %v", source, err)
		}
	}()
	return true
}

// lookAgain reports whether this turn takes a fresh look. A follow-up
// looks again when she handed it off or it asks what the view shows;
// a backchannel on an open look does not.
func lookAgain(continuing, handoff bool, userText string) bool {
	return !continuing || handoff || eye.AsksScene(userText)
}

// lookAnswers is the delegation a finished look replies on: the one bound
// when it started, or one the gateway raised for this same utterance after
// dispatch. A newer handoff for another ask gets nothing.
func lookAnswers(h *voiceHold, bound, userText string) string {
	id := bound
	if id == "" && h.handoffTurn(userText) {
		id = h.delegation()
	}
	if id == "" || h.delegation() != id {
		return ""
	}
	return id
}

// lookAsk is what a look should answer: the latest lines of this branch,
// newest last, so a short follow-up keeps what it points at.
func lookAsk(goal, userText string) string {
	cur := strings.Join(strings.Fields(userText), " ")
	var lines []string
	for _, l := range strings.Split(goal, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" && l != cur {
			lines = append(lines, l)
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > lookAskLines {
		lines = lines[len(lines)-lookAskLines:]
	}
	q := []rune(strings.Join(lines, "；"))
	if len(q) > lookAskRunes {
		q = q[len(q)-lookAskRunes:]
	}
	return string(q)
}

// isLook is a branch that only looks: the camera, the screen, or her own shot.
func isLook(kind string) bool {
	return kind == judge.ActCamera || kind == judge.ActScreen || kind == judge.ActShot
}

// lookFallback answers a handoff on an open look that got no fresh note.
func lookFallback(kind, note string) string {
	if note == lookPending {
		return "You are still looking at the " + kind + " for this; nothing is back yet. Say so in one short in-character line. Do not describe it yet."
	}
	return "Nothing new was looked at for this. Answer from your last " + kind + " note in one short in-character line. If it does not show what they ask, say you cannot tell from it. Do not claim you looked again."
}

const (
	// lookPending and lookDone are a look branch's note while the eye is out, and after.
	lookPending = "looking"
	lookDone    = "looked"
	// lookAskLines and lookAskRunes keep a look's question inside the eye prompt.
	lookAskLines = 3
	lookAskRunes = 80
)

func sightAsk(source string) string {
	switch source {
	case eye.SourceScreen:
		return sense.AskScreen
	case eye.SourceShot:
		return sense.AskShot
	default:
		return sense.AskCamera
	}
}

func sightMiss(source string) string {
	switch source {
	case eye.SourceScreen:
		return "You cannot look at the screen right now. Say so in character, in one short line. Do not invent what is on it. Do not describe the camera."
	case eye.SourceShot:
		return "You cannot see a screenshot of yourself right now. Say so in character, in one short line. Do not invent how you look. Do not describe the camera or the desktop."
	default:
		return "You cannot look through the camera right now. Say so in character, in one short line. Do not invent a scene. Do not describe the screen."
	}
}

func sightSpoke(source string, ready, again bool, question string) string {
	if source == eye.SourceShot && !ready {
		return "The screenshot of yourself did not arrive. Say so in one short in-character line. Do not invent how you look. Do not describe the camera or the desktop."
	}
	q := strings.TrimSpace(question)
	if q != "" && (again || eye.AsksScene(q)) {
		lead := "You just looked. "
		if again {
			lead = "This is a new look for their follow-up. "
		}
		return lead + "Answer only this question, in one or two in-character sentences, from the latest note: " +
			clip(q, 80) + ". Do not repeat an earlier description that does not answer it. Do not invent."
	}
	switch source {
	case eye.SourceScreen:
		return "You just looked at the screen. Say what is there in one or two in-character sentences, only from the screen note. Window titles say which app is open. A picture answer, if the note has one, is what is visible. Do not describe the camera. Do not invent."
	case eye.SourceShot:
		return "You just looked at a screenshot of yourself. Say what you look like in one or two in-character sentences, only from the screenshot note. Do not describe the camera or the desktop. Do not invent."
	default:
		return "You just looked through the camera. Say what you see in one or two in-character sentences, only from the camera note. Do not describe the screen. Do not invent."
	}
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
		live := opt.voice.live(sess)
		if live == nil {
			opt.log("[%s] steer skipped", source)
		} else if err := live.Steer(livevoice.FitTail(felt)); errors.Is(err, livevoice.ErrHeld) {
			opt.log("[steer] queued")
		} else if err != nil {
			opt.log("[%s] steer failed: %v", source, err)
		}
	}
}

func voiceSteer(opt Options, sess *livevoice.Session, text string) {
	sess = opt.voice.live(sess)
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	if err := sess.Steer(text); errors.Is(err, livevoice.ErrHeld) {
		opt.log("[steer] queued")
	} else if err != nil {
		opt.log("[act] steer failed: %v", err)
	}
}

// voiceNudge tells her about background work.
// An open delegation is answered on that id: commentary so she says the
// result, the way a handoff returns [BACKEND] text to the voice model.
// Lines that should stay quiet stay on the developer channel.
// With no delegation, the note stays silent so a later chat turn is not cut.
func voiceNudge(opt Options, sess *livevoice.Session, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if id := opt.voice.delegation(); id != "" && handoffShouldSpeak(text) {
		voiceAnswer(opt, sess, id, "commentary", text)
		return
	}
	voiceSteer(opt, sess, nudgeLead+text)
}

func voiceAnswer(opt Options, sess *livevoice.Session, id, channel, text string) {
	sess = opt.voice.live(sess)
	if sess == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(text) == "" {
		return
	}
	if err := sess.Resolve(id, channel, text); errors.Is(err, livevoice.ErrHeld) {
		opt.voice.markAnswered(id)
		opt.log("[answer] queued")
	} else if err != nil {
		opt.log("[answer] failed: %v", err)
	} else {
		opt.voice.markAnswered(id)
		opt.log("[answer] %s", channel)
	}
}

// awaitHandoff answers id after wait if nothing else has: what is open
// and how far it got, or an honest "nothing ran".
func awaitHandoff(opt Options, sess *livevoice.Session, slot *capabilitySlot, id string, wait time.Duration) {
	if id == "" {
		return
	}
	time.AfterFunc(wait, func() {
		if !opt.voice.unanswered(id) {
			return
		}
		opt.log("[answer] fallback after %s", wait)
		voiceAnswer(opt, sess, id, "commentary", handoffFallback(opt, slot))
	})
}

// senseReply is a handoff answer already in hand: the journal, her body,
// or the stage. A look still waits on the eye, so camera, screen, and shot
// stay out of this.
func senseReply(opt Options, p *persona.Persona, userText string) string {
	if opt.sense == nil || p == nil {
		return ""
	}
	ask := sense.ParseAsk(userText)
	switch ask.Kind {
	case sense.AskLog, sense.AskBody, sense.AskExistence, sense.AskCode, sense.AskFile, sense.AskWindow:
	default:
		return ""
	}
	return strings.TrimSpace(opt.sense.Felt(p, ask, ""))
}

// handoffFallback is what she can truthfully say when no work line came:
// the open branch and what the queue says about it, or that nothing ran.
func handoffFallback(opt Options, slot *capabilitySlot) string {
	kind, _, note := slot.current()
	if kind == "" {
		return delegAck
	}
	if isLook(kind) {
		return lookFallback(kind, note)
	}
	status := stageStatus(opt, note)
	if status == "" && slot.busy() {
		status = "it is still running"
	}
	if status == "" {
		status = strings.TrimSpace(note)
	}
	line := "You are still on the " + kind + " they asked for"
	if status != "" {
		line += "; right now " + clip(status, 160)
	}
	return line + ". If they asked about it, say where it stands in one short in-character line. Do not claim more than this."
}

// handoffShouldSpeak is false for progress she should keep unless asked.
func handoffShouldSpeak(text string) bool {
	low := strings.ToLower(text)
	return !strings.Contains(low, "only if they ask") && !strings.Contains(low, "mention it only")
}

// sameBurst reports that two texts are one utterance split across
// turn.done and a delegation.
func sameBurst(prev, cur string, at time.Time) bool {
	if at.IsZero() || time.Since(at) > 4*time.Second {
		return false
	}
	prev = strings.Join(strings.Fields(prev), " ")
	cur = strings.Join(strings.Fields(cur), " ")
	if prev == "" || cur == "" {
		return false
	}
	return prev == cur || strings.Contains(prev, cur) || strings.Contains(cur, prev)
}

func modeNeedsDirector(mode string) bool {
	switch mode {
	case "safety", "comfort", "de_escalate":
		return true
	default:
		return false
	}
}

const nudgeLead = "Background only. Do not start speaking about this. Use it only if their latest utterance asked: "

// delegAck answers a handoff where nothing ran. It is spoken guidance on
// commentary; delegation.context.append rejects the developer channel.
const delegAck = "Nothing ran for this. Answer them yourself, in character, from the conversation. Do not claim you checked, looked, or ran anything."

const (
	// delegWait is the longest she waits on a handoff before a fallback.
	delegWait = 12 * time.Second
	// handoffGrace lets a follow-up's own line land before the status answer.
	handoffGrace = 2 * time.Second
)

// ensureDivine keeps one six-line plate for the open branch.
// A follow-up rereads that plate. A new toss happens only when they ask.
func ensureDivine(ctx context.Context, opt Options, p *persona.Persona, lc *llm.Client,
	sess *livevoice.Session, slot *capabilitySlot, userText string, continuing bool) {
	if slot.busy() {
		opt.log("[divine] still reading")
		return
	}
	ok, gen := slot.tryComputer()
	if !ok {
		opt.log("[divine] busy")
		return
	}
	fresh := !continuing || oracle.WantsRecast(userText) || strings.TrimSpace(slot.holdText()) == ""
	if fresh {
		plate, err := oracle.Cast(time.Now(), 0)
		if err != nil {
			opt.log("[divine] cast failed: %v", err)
			slot.endComputer(gen)
			return
		}
		slot.setHold(plate.Prompt())
		slot.setNote(plate.Glance())
		if opt.sense != nil {
			opt.sense.Emit(sense.Event{Kind: sense.KindDivine, Summary: "divine start", Data: map[string]any{"glance": plate.Glance()}})
		}
		voiceNudge(opt, sess, "You just cast three coins, six times, for their question. Tell them in one short in-character line that the coins are down and you are reading them. Do not give the omen yet.")
	}
	_, goal, glance := slot.current()
	plateText := slot.holdText()
	if lc == nil {
		runOracle(ctx, opt, p, nil, sess, goal, plateText, glance)
		slot.endComputer(gen)
		return
	}
	go func() {
		defer slot.endComputer(gen)
		runCtx := slot.bind(ctx)
		runOracle(runCtx, opt, p, lc, sess, goal, plateText, glance)
	}()
}

func runOracle(ctx context.Context, opt Options, p *persona.Persona, lc *llm.Client,
	sess *livevoice.Session, question, plate, glance string) {
	if opt.sense != nil {
		opt.sense.Emit(sense.Event{Kind: sense.KindDivine, Summary: "divine read"})
	}
	if lc == nil {
		voiceNudge(opt, sess, divineNudge(oracle.Reading{}, glance, true))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	name, style := "", ""
	if p != nil {
		name, style = p.Name, p.Style
	}
	rd, err := oracle.Read(ctx, lc, name, style, plate, question)
	if err != nil {
		opt.log("[divine] read failed: %v", err)
		voiceNudge(opt, sess, divineNudge(oracle.Reading{}, glance, true))
		return
	}
	opt.log("[divine] omen=%s focus=%s", rd.Omen, clip(rd.Focus, 40))
	voiceNudge(opt, sess, divineNudge(rd, glance, false))
}

func divineNudge(rd oracle.Reading, glance string, failed bool) string {
	if rd.Omen == "refuse" {
		note := strings.TrimSpace(rd.Note)
		if note == "" {
			note = "Stay with them."
		}
		return "Do not tell a fortune. Stay with them, in character, short. " + note
	}
	if failed || strings.TrimSpace(rd.Note) == "" {
		return "The coins are down but the reading did not settle. Name the cast if you have it (" + orDash(glance) + ") and say you are still looking. Do not invent an omen."
	}
	return "A six-line cast is ready. Say it in character, two or three sentences, in their language. " +
		"Do not recite every line. Do not claim their future is fixed. " +
		"Omen tone: " + orDash(rd.Omen) + ". What to convey: " + rd.Note +
		". If they ask which hexagram, the cast is " + orDash(glance) + "."
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
	case line == "/off":
		if opt.power != nil {
			opt.power.Set(false)
		}
	case line == "/on":
		if opt.power != nil {
			opt.power.Set(true)
		}
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
			if err := sess.Steer(felt); errors.Is(err, livevoice.ErrHeld) {
				opt.log("[steer] queued")
			} else if err != nil {
				opt.log("[sense] steer failed: %v", err)
			}
		}
		if felt := opt.sense.Felt(p, sense.Ask{Kind: sense.AskLog}, "log"); felt != "" {
			if err := sess.Steer(felt); errors.Is(err, livevoice.ErrHeld) {
				opt.log("[steer] queued")
			} else if err != nil {
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
			if err := sess.Steer(felt); errors.Is(err, livevoice.ErrHeld) {
				opt.log("[steer] queued")
			} else if err != nil {
				opt.log("[look] steer failed: %v", err)
			}
		}
	// A glance waits for a frame and a vision call; the event loop must
	// keep draining turn.done meanwhile.
	case line == "/camera", line == "/see camera":
		go glanceCommand(opt.runCtx(ctx), opt, p, sess, eye.SourceCamera)
	case line == "/screen", line == "/see screen":
		go glanceCommand(opt.runCtx(ctx), opt, p, sess, eye.SourceScreen)
	case line == "/shot", line == "/see shot":
		go glanceCommand(opt.runCtx(ctx), opt, p, sess, eye.SourceShot)
	case line == "/see":
		opt.log("camera, screen, and a screenshot of herself are separate: /camera or /screen or /shot")
	case strings.HasPrefix(line, "/say "):
		if err := sess.Speak(strings.TrimPrefix(line, "/say ")); errors.Is(err, livevoice.ErrHeld) {
			opt.log("[say] queued")
		} else if err != nil {
			opt.log("speak failed: %v", err)
		}
	case strings.HasPrefix(line, "/steer "):
		if err := sess.Steer(strings.TrimPrefix(line, "/steer ")); errors.Is(err, livevoice.ErrHeld) {
			opt.log("[steer] queued")
		} else if err != nil {
			opt.log("steer failed: %v", err)
		}
	case strings.HasPrefix(line, "/goal "):
		pl.SetNote(strings.TrimPrefix(line, "/goal "))
		opt.log("goal set")
	case line == "/stage" || strings.HasPrefix(line, "/stage "):
		stageCommand(opt.runCtx(ctx), opt, lc, sess, line)
	case strings.HasPrefix(line, "/desk "):
		goal := strings.TrimSpace(strings.TrimPrefix(line, "/desk "))
		if goal == "" {
			opt.log("desk needs a goal")
			break
		}
		opt.log("[desk] starting: %s", goal)
		go func() {
			rep, err := desk.Run(opt.runCtx(ctx), desk.Options{
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
	case line == "/divine" || strings.HasPrefix(line, "/divine "):
		q := strings.TrimSpace(strings.TrimPrefix(line, "/divine"))
		if q == "" {
			opt.log("divine needs a question")
			break
		}
		plate, err := oracle.Cast(time.Now(), 0)
		if err != nil {
			opt.log("[divine] cast failed: %v", err)
			break
		}
		opt.log("[divine] %s", plate.Glance())
		if opt.sense != nil {
			opt.sense.Emit(sense.Event{Kind: sense.KindDivine, Summary: "divine start", Data: map[string]any{"glance": plate.Glance()}})
		}
		voiceNudge(opt, sess, "You just cast three coins, six times, for their question. Tell them in one short in-character line that the coins are down and you are reading them. Do not give the omen yet.")
		go runOracle(opt.runCtx(ctx), opt, p, lc, sess, q, plate.Prompt(), plate.Glance())
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
			rep, err := desk.Run(opt.runCtx(ctx), desk.Options{
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
		opt.log("unknown command (try /say /steer /goal /look /camera /screen /shot /sense /stage /desk /codex /divine /off /on /status /quit)")
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

func sceneCue(store *memory.RelationshipStore, userText, mode string) steering.SceneCue {
	if store == nil {
		return steering.SceneCue{}
	}
	// Only lines about this utterance come along, except when they pull
	// away: then an old thread is something to come back to.
	query := userText
	if mode == "re_engage" {
		query = ""
	}
	c := store.Recall(query)
	return steering.SceneCue{
		Stage:        c.Stage,
		Summary:      c.Summary,
		OpenLoops:    append([]string(nil), c.OpenLoops...),
		SharedEvents: append([]string(nil), c.SharedEvents...),
	}
}

// taskAct is an act that asks her to do something now. Its progress is
// runtime state, not relationship memory.
func taskAct(act string) bool {
	switch act {
	case "", judge.ActNone, judge.ActPlan, judge.ActDivine:
		return false
	}
	return true
}

func foldMemory(opt Options, lc *llm.Client, userText, assistantText, mode string) {
	if opt.rel == nil || lc == nil || !opt.rel.BeginFold() {
		return
	}
	go func() {
		defer opt.rel.EndFold()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		notes, err := opt.rel.Fold(ctx, lc, memory.FoldIn{
			User:      userText,
			Assistant: assistantText,
			Mode:      mode,
		})
		if err != nil {
			opt.log("[memory] fold skipped: %v", err)
			return
		}
		if len(notes) > 0 {
			opt.log("[memory] %s", strings.Join(notes, " | "))
		}
	}()
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
		logq.Println(line)
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
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
