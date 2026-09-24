package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/eye"
	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/planner"
	"github.com/jingx8885/lov-evo/internal/sense"
	"github.com/jingx8885/lov-evo/internal/window"
)

func TestIntentRoutesTheTurn(t *testing.T) {
	cases := []struct {
		name    string
		intent  string
		emotion string
		act     string
		mode    string
		safety  float64
		plan    string
		want    string
	}{
		{name: "chat", intent: "chat", emotion: "neutral", act: judge.ActNone, want: "continue"},
		{name: "comfort", intent: "comfort_seek", emotion: "fear", act: judge.ActNone, want: "comfort"},
		{name: "banter is not comfort", intent: "banter", emotion: "sadness", act: judge.ActNone, want: "continue"},
		{name: "share good", intent: "share_good", emotion: "joy", act: judge.ActNone, want: "celebrate"},
		{name: "share good while sad still comforts", intent: "share_good", emotion: "sadness", act: judge.ActNone, want: "comfort"},
		{name: "share bad without distress", intent: "share_bad", emotion: "neutral", act: judge.ActNone, want: "continue"},
		{name: "request", intent: "request", emotion: "neutral", act: judge.ActComputerUse, want: "continue"},
		{name: "goodbye skips comfort", intent: "goodbye", emotion: "sadness", act: judge.ActNone, want: "continue"},
		{name: "withdraw", intent: "withdraw", emotion: "neutral", act: judge.ActNone, want: "re_engage"},
		{name: "withdraw while sad comforts first", intent: "withdraw", emotion: "sadness", act: judge.ActNone, want: "comfort"},
		{name: "anger", intent: "other", emotion: "anger", act: judge.ActNone, want: "de_escalate"},
		{name: "joy", intent: "other", emotion: "joy", act: judge.ActNone, want: "celebrate"},
		{name: "unknown intent", intent: "teleport", emotion: "neutral", act: "sudo", want: "continue"},
		{name: "jev mode wins", intent: "comfort_seek", emotion: "fear", act: judge.ActNone, mode: "continue", want: "continue"},
		{name: "safety wins", intent: "share_good", emotion: "joy", act: judge.ActImage, safety: 0.95, want: "safety"},
		{name: "goal push needs a plan", intent: "request", emotion: "neutral", act: judge.ActPlan, mode: "goal_push", want: "continue"},
		{name: "goal push with a plan", intent: "request", emotion: "neutral", act: judge.ActPlan, mode: "goal_push", plan: "ask which one", want: "goal_push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jd := parsedTurn("这一句", tc.intent, tc.emotion, tc.act, tc.mode, tc.safety)
			if tc.act == "sudo" && jd.Act != "" {
				t.Fatal("unknown act must be dropped before routing")
			}
			if got := judge.DecideMode(jd, memory.Affect{}, 0.6, tc.plan); got != tc.want {
				t.Fatalf("intent %s emotion %s -> %s, want %s", tc.intent, tc.emotion, got, tc.want)
			}
		})
	}
}

func TestEachActEntersOnlyThatCapability(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		jd := parsedTurn("你好", "chat", "neutral", judge.ActNone, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		if slot.snapshot().Kind != "" {
			t.Fatalf("chat opened %s", slot.snapshot().Kind)
		}
	})

	t.Run("camera screen shot", func(t *testing.T) {
		looks := &lookLog{}
		opt, slot, _ := routeRig(t, looks)
		jd := parsedTurn("看看摄像头", "request", "neutral", judge.ActCamera, "", 0)
		if jd.Attend != "camera" {
			t.Fatalf("camera attend %q", jd.Attend)
		}
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		if slot.snapshot().Kind != judge.ActCamera {
			t.Fatalf("kind %s", slot.snapshot().Kind)
		}
		if !looks.wait("camera", time.Second) || looks.count("screen") != 0 || looks.count("shot") != 0 {
			t.Fatalf("looks %v", looks.snapshot())
		}

		stay := parsedTurn("嗯", "withdraw", "neutral", judge.ActNone, "", 0)
		if judge.DecideMode(stay, memory.Affect{}, 0.6, "") != "re_engage" {
			t.Fatal("withdraw should re-engage, not switch tools")
		}
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, stay, "re_engage", stay.UserText, true)
		if slot.snapshot().Kind != judge.ActCamera || looks.count("camera") != 1 {
			t.Fatal("a withdrawn backchannel left the camera branch")
		}

		again := parsedTurn("嗯", "chat", "neutral", judge.ActCamera, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, again, "continue", again.UserText, true)
		if looks.count("camera") != 1 {
			t.Fatal("a follow-up that does not ask the scene looked again")
		}

		screen := parsedTurn("看看屏幕", "request", "neutral", judge.ActScreen, "", 0)
		if screen.Attend != "screen" {
			t.Fatalf("screen attend %q", screen.Attend)
		}
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, screen, "continue", screen.UserText, true)
		if slot.snapshot().Kind != judge.ActScreen || looks.count("camera") != 1 {
			t.Fatalf("screen route kind=%s looks=%v", slot.snapshot().Kind, looks.snapshot())
		}
		waitUntil(t, time.Second, func() bool {
			return strings.Contains(opt.eyes.Snapshot().Screen.Caption, "记事本")
		})

		shot := parsedTurn("看看你自己", "request", "neutral", judge.ActShot, "", 0)
		if shot.Attend != "" {
			t.Fatalf("shot must not borrow camera or screen attend, got %q", shot.Attend)
		}
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, shot, "continue", shot.UserText, true)
		if slot.snapshot().Kind != judge.ActShot || !looks.wait("shot", time.Second) || looks.count("screen") != 1 {
			t.Fatalf("shot route kind=%s looks=%v", slot.snapshot().Kind, looks.snapshot())
		}
	})

	t.Run("studio stays then switches", func(t *testing.T) {
		opt, slot, release := routeRig(t, nil)
		defer release()
		jd := parsedTurn("画一朵红玫瑰", "request", "neutral", judge.ActImage, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		waitJob(t, opt.stageQ, window.KindImage, window.StatusRunning)
		if slot.snapshot().Kind != judge.ActImage {
			t.Fatalf("kind %s", slot.snapshot().Kind)
		}
		follow := parsedTurn("再红一点", "request", "neutral", judge.ActImage, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, follow, "continue", follow.UserText, true)
		if jobs := opt.stageQ.Snapshot(); len(jobs) != 1 || jobs[0].Kind != window.KindImage {
			t.Fatalf("follow-up started another job: %+v", jobs)
		}
		video := parsedTurn("改成一段视频", "request", "joy", judge.ActVideo, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, video, "celebrate", video.UserText, true)
		waitKind(t, opt.stageQ, window.KindVideo)
		if slot.snapshot().Kind != judge.ActVideo {
			t.Fatalf("kind %s", slot.snapshot().Kind)
		}
	})

	for _, kind := range []string{judge.ActSpeech, judge.ActSong} {
		t.Run(kind, func(t *testing.T) {
			opt, slot, release := routeRig(t, nil)
			defer release()
			jd := parsedTurn("做一段"+kind, "request", "neutral", kind, "", 0)
			dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
			job := waitJob(t, opt.stageQ, kind, window.StatusReady)
			if job.File != kind+".bin" {
				t.Fatalf("file %q", job.File)
			}
			if len(opt.stageQ.Snapshot()) != 1 {
				t.Fatal("a studio act started more than its own job")
			}
		})
	}

	t.Run("plan queue", func(t *testing.T) {
		opt, slot, release := routeRig(t, nil)
		defer release()
		jd := parsedTurn("帮我想想先做哪件", "request", "neutral", judge.ActPlan, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		job := waitJob(t, opt.stageQ, window.KindLLM, window.StatusReady)
		if slotOpen(slot) != "" && slotOpen(slot) != judge.ActPlan {
			t.Fatalf("plan opened %s", slotOpen(slot))
		}
		if job.Kind != window.KindLLM {
			t.Fatalf("plan queued %s", job.Kind)
		}
	})

	t.Run("plan thought", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		opt.stageQ = nil
		pl := routePlanner(noteLLM{note: "先问她想先做哪一件"})
		jd := parsedTurn("帮我拿个主意", "request", "neutral", judge.ActPlan, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), pl, nil, slot, jd, "continue", jd.UserText, true)
		waitUntil(t, 2*time.Second, func() bool {
			return strings.Contains(pl.Current(), "先问她想先做哪一件")
		})
		if slot.snapshot().Kind != judge.ActPlan {
			t.Fatalf("kind %s", slot.snapshot().Kind)
		}
	})

	for _, kind := range []string{judge.ActPicture, judge.ActWatch, judge.ActListen} {
		t.Run(kind, func(t *testing.T) {
			opt, slot, _ := routeRig(t, nil)
			opt.stageQ = nil
			opt.RunsDir = t.TempDir()
			jd := parsedTurn("看看刚做好的", "request", "neutral", kind, "", 0)
			dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
			if !sawSummary(opt.sense, kind+" start") {
				t.Fatalf("%s did not start", kind)
			}
			waitUntil(t, time.Second, func() bool { return !slot.busy() })
			if slot.snapshot().Kind != "" {
				t.Fatalf("%s stayed open after there was nothing to perceive", kind)
			}
		})
	}

	t.Run("divine", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		jd := parsedTurn("帮我算一卦", "request", "neutral", judge.ActDivine, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		if slot.snapshot().Kind != judge.ActDivine || !strings.Contains(slot.holdText(), "本卦") {
			t.Fatalf("divine kind=%s hold=%s", slot.snapshot().Kind, slot.holdText())
		}
		if !sawSummary(opt.sense, "divine start") || sawSummary(opt.sense, "computer_use start") {
			t.Fatal("divine took another capability")
		}
	})

	t.Run("codex program goes to the queue", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		jd := parsedTurn("你能调用 Codex 给你自己写一个程序吗", "request", "neutral", judge.ActCodex, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		job := waitJob(t, opt.stageQ, window.KindCodex, window.StatusReady)
		if job.Prompt != jd.UserText {
			t.Fatalf("codex prompt = %q", job.Prompt)
		}
		if sawSummary(opt.sense, "codex start") {
			t.Fatal("a program request took the self-edit path")
		}
	})

	t.Run("codex program without a queue does not read her source", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		opt.stageQ = nil
		jd := parsedTurn("你可以在Codex那里写一个贪吃蛇的游戏吗", "request", "neutral", judge.ActCodex, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		if sawSummary(opt.sense, "codex start") || slot.busy() {
			t.Fatal("a program request took the self-edit path")
		}
	})

	t.Run("reflect look codex", func(t *testing.T) {
		for _, kind := range []string{judge.ActReflect, judge.ActLook, judge.ActCodex} {
			opt, slot, _ := routeRig(t, nil)
			text := map[string]string{
				judge.ActReflect: "你现在什么感觉",
				judge.ActLook:    "你内部是怎么活的",
				judge.ActCodex:   "改一下你自己",
			}[kind]
			jd := parsedTurn(text, "request", "neutral", kind, "", 0)
			dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
			want := kind
			if kind == judge.ActCodex {
				want = "codex start"
			}
			if !sawSummary(opt.sense, want) {
				t.Fatalf("%s did not enter", kind)
			}
			waitUntil(t, 3*time.Second, func() bool { return !slot.busy() })
			if sawSummary(opt.sense, "computer_use start") {
				t.Fatalf("%s entered the desk", kind)
			}
		}
	})

	t.Run("computer use", func(t *testing.T) {
		opt, slot, _ := routeRig(t, nil)
		jd := parsedTurn("打开记事本", "request", "neutral", judge.ActComputerUse, "", 0)
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		if !sawSummary(opt.sense, "computer_use start") {
			t.Fatal("computer use did not start")
		}
		waitUntil(t, 2*time.Second, func() bool { return sawSummary(opt.sense, "computer_use failed") })
		if sawSummary(opt.sense, "codex start") || sawSummary(opt.sense, "divine start") {
			t.Fatal("computer use entered another capability")
		}
	})
}

func TestHeldActsDoNotEnter(t *testing.T) {
	cases := []struct {
		name   string
		act    string
		mode   string
		safety float64
		conf   float64
	}{
		{name: "safety computer", act: judge.ActComputerUse, mode: "safety", safety: 0.95, conf: -1},
		{name: "safety image", act: judge.ActImage, mode: "safety", safety: 0.95, conf: -1},
		{name: "safety picture", act: judge.ActPicture, mode: "safety", safety: 0.95, conf: -1},
		{name: "safety divine", act: judge.ActDivine, mode: "safety", safety: 0.95, conf: -1},
		{name: "low confidence codex", act: judge.ActCodex, mode: "continue", conf: 0.1},
		{name: "low confidence song", act: judge.ActSong, mode: "continue", conf: 0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt, slot, release := routeRig(t, nil)
			defer release()
			jd := parsedTurn("做这件事", "request", "neutral", tc.act, "", tc.safety)
			if tc.conf >= 0 {
				low := tc.conf
				jd.Raw["act"] = jev.Answer{Choice: tc.act, Confidence: &low}
			}
			dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, tc.mode, jd.UserText, true)
			if slot.snapshot().Kind != "" || slot.busy() {
				t.Fatalf("held act entered kind=%s busy=%v", slot.snapshot().Kind, slot.busy())
			}
			if opt.stageQ != nil && len(opt.stageQ.Snapshot()) != 0 {
				t.Fatal("held act queued a job")
			}
			if sawSummary(opt.sense, tc.act+" start") || sawSummary(opt.sense, "computer_use start") || sawSummary(opt.sense, "codex start") || sawSummary(opt.sense, "divine start") {
				t.Fatal("held act emitted a start")
			}
		})
	}
}

func parsedTurn(user, intent, emotion, act, mode string, safety float64) *judge.Judgment {
	ans := map[string]jev.Answer{
		"intent":  {Choice: intent},
		"emotion": {Choice: emotion},
		"act":     {Choice: act},
	}
	if mode != "" {
		ans["mode"] = jev.Answer{Choice: mode}
	}
	if safety > 0 {
		ans["safety"] = jev.Answer{Noul: &safety}
	}
	jd := judge.Parse(user, ans)
	return &jd
}

func routePersona() *persona.Persona {
	return &persona.Persona{
		Name:    "小春",
		Goals:   []string{"陪着说话"},
		Sense:   persona.SenseConfig{Enabled: true},
		Judge:   persona.JudgeConfig{NeedLLMThresh: 0.55},
		Planner: persona.PlannerConfig{Enabled: true, IntervalTurn: 1},
	}
}

func routePlanner(llm planner.Completer) *planner.Planner {
	return planner.New(routePersona(), nil, llm, "")
}

type noteLLM struct{ note string }

func (n noteLLM) ChatComplete(context.Context, string, string) (string, error) {
	return `{"note":"` + n.note + `","goal_status":{}}`, nil
}

func routeRig(t *testing.T, looks *lookLog) (Options, *capabilitySlot, func()) {
	t.Helper()
	bus, err := sense.Open(sense.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	if looks == nil {
		looks = &lookLog{}
	}
	release := make(chan struct{})
	var stop sync.Once
	done := func() { stop.Do(func() { close(release) }) }
	st := window.NewStage(window.StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			if kind == window.KindImage {
				select {
				case <-release:
				case <-ctx.Done():
					return "", "", ctx.Err()
				}
			}
			report("ready", 1)
			return kind + ".bin", "ok", nil
		},
	})
	t.Cleanup(func() {
		done()
		st.Close()
	})
	opt := Options{
		sense:   bus,
		stageQ:  st,
		voice:   &voiceHold{},
		RunsDir: t.TempDir(),
		eyes: eye.New(eye.Options{
			Grab: func(context.Context) bool {
				looks.add("camera")
				return false
			},
			GrabShot: func(context.Context) bool {
				looks.add("shot")
				return false
			},
			Observe: func(context.Context) (eye.ScreenView, error) {
				looks.add("screen")
				return eye.ScreenView{Caption: "记事本在前台"}, nil
			},
		}),
	}
	return opt, &capabilitySlot{}, done
}

type lookLog struct {
	mu   sync.Mutex
	hits []string
}

func (l *lookLog) add(name string) {
	l.mu.Lock()
	l.hits = append(l.hits, name)
	l.mu.Unlock()
}

func (l *lookLog) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.hits {
		if h == name {
			n++
		}
	}
	return n
}

func (l *lookLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.hits...)
}

func (l *lookLog) wait(name string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if l.count(name) > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return l.count(name) > 0
}

func waitUntil(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok() {
		t.Fatal("timed out waiting for the capability")
	}
}

func waitJob(t *testing.T, st *window.Stage, kind, status string) window.Job {
	t.Helper()
	var found window.Job
	waitUntil(t, 2*time.Second, func() bool {
		for _, job := range st.Snapshot() {
			if job.Kind == kind && job.Status == status {
				found = job
				return true
			}
		}
		return false
	})
	return found
}

func waitKind(t *testing.T, st *window.Stage, kind string) {
	t.Helper()
	waitUntil(t, 2*time.Second, func() bool {
		for _, job := range st.Snapshot() {
			if job.Kind == kind {
				return true
			}
		}
		return false
	})
}

func slotOpen(slot *capabilitySlot) string {
	return slot.snapshot().Kind
}

func TestSelfFollowUpRunsOnlyWhenAskedAgain(t *testing.T) {
	opt, slot, _ := routeRig(t, nil)
	run := func(jd *judge.Judgment) {
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
		waitUntil(t, 3*time.Second, func() bool { return !slot.busy() })
	}
	stretches := func() int {
		n := 0
		for _, ev := range opt.sense.Snapshot("").Recent {
			if ev.Summary == "look" {
				n++
			}
		}
		return n
	}
	run(parsedTurn("你内部是怎么活的", "request", "neutral", judge.ActLook, "", 0))
	if slotOpen(slot) != judge.ActLook || stretches() != 1 {
		t.Fatalf("look did not open: kind=%s stretches=%d", slotOpen(slot), stretches())
	}
	run(parsedTurn("好的，挺有意思", "chat", "neutral", judge.ActNone, "", 0))
	if slotOpen(slot) != judge.ActLook || stretches() != 1 {
		t.Fatalf("a folded follow-up ran another stretch: kind=%s stretches=%d", slotOpen(slot), stretches())
	}
	run(parsedTurn("那你的记忆模块是怎么写的", "request", "neutral", judge.ActLook, "", 0))
	if stretches() != 2 {
		t.Fatalf("picking look again did not look again: stretches=%d", stretches())
	}
}

func TestHandoffStaysBoundToItsTurn(t *testing.T) {
	opt, slot, release := routeRig(t, nil)
	defer release()
	run := func(jd *judge.Judgment) {
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
	}
	opt.voice.setDelegation("del_1", "画一朵红玫瑰")
	opt.voice.markAnswered("del_1")
	run(parsedTurn("随便聊聊", "chat", "neutral", judge.ActNone, "", 0))
	if opt.voice.delegation() != "del_1" {
		t.Fatal("chat dropped the handoff a running task still answers on")
	}
	run(parsedTurn("画一只猫", "request", "neutral", judge.ActImage, "", 0))
	if opt.voice.delegation() != "" {
		t.Fatal("work opened on a turn she kept spoke through an old handoff")
	}
	opt.voice.setDelegation("del_2", "改成一段视频")
	run(parsedTurn("改成一段视频", "request", "neutral", judge.ActVideo, "", 0))
	if slotOpen(slot) != judge.ActVideo || opt.voice.delegation() != "del_2" {
		t.Fatalf("handed-off work lost its id: kind=%s id=%q", slotOpen(slot), opt.voice.delegation())
	}
}

func TestHandedOffLookFollowUpLooksAgain(t *testing.T) {
	looks := &lookLog{}
	opt, slot, _ := routeRig(t, looks)
	run := func(jd *judge.Judgment) {
		dispatchCapability(context.Background(), opt, routePersona(), nil, nil, memory.New(4), routePlanner(nil), nil, slot, jd, "continue", jd.UserText, true)
	}
	looked := func() bool {
		_, _, note := slot.current()
		return note == lookDone
	}
	run(parsedTurn("看看屏幕", "request", "neutral", judge.ActScreen, "", 0))
	waitUntil(t, time.Second, looked)
	run(parsedTurn("那个看看fit ui吧", "request", "neutral", judge.ActScreen, "", 0))
	if !looked() || looks.count("screen") != 1 {
		t.Fatalf("a follow-up she answered herself looked again: %v", looks.snapshot())
	}
	opt.voice.setDelegation("del_up", "嗯...上面的部分呗")
	run(parsedTurn("嗯...上面的部分呗", "request", "neutral", judge.ActScreen, "", 0))
	waitUntil(t, time.Second, func() bool { return looks.count("screen") == 2 && looked() })
	if got := lookAnswers(opt.voice, "", "嗯...上面的部分呗"); got != "del_up" {
		t.Fatalf("the look would answer %q", got)
	}
}

func TestLookFallbackSaysWhatTheEyeDid(t *testing.T) {
	opt, slot, _ := routeRig(t, nil)
	slot.open(judge.ActScreen, "看看屏幕")
	slot.noteFor(judge.ActScreen, lookPending)
	if got := handoffFallback(opt, slot); !strings.Contains(got, "still looking") || strings.Contains(got, "they asked for") {
		t.Fatalf("pending look fallback = %q", got)
	}
	slot.noteFor(judge.ActScreen, lookDone)
	if got := handoffFallback(opt, slot); !strings.Contains(got, "Nothing new was looked at") {
		t.Fatalf("finished look fallback = %q", got)
	}
	steer := branchSteer(slot.snapshot(), parsedTurn("嗯...上面的部分呗", "request", "neutral", judge.ActScreen, "", 0), "continue")
	if strings.Contains(steer, "not finished") || !strings.Contains(steer, "latest screen note") {
		t.Fatalf("look steer = %q", steer)
	}
	slot.noteFor(judge.ActImage, "image queued j1")
	if _, _, note := slot.current(); note != lookDone {
		t.Fatal("a late note for another kind overwrote the open look")
	}
}

func TestLogHandoffCarriesTheJournal(t *testing.T) {
	opt, slot, _ := routeRig(t, nil)
	opt.sense.Note("[branch] open codex")
	opt.sense.Note("[window] open http://127.0.0.1:1")
	got := senseReply(opt, routePersona(), "嗯嗯你能看到你的日志吗")
	if !strings.Contains(got, "Process log") || !strings.Contains(got, "open codex") {
		t.Fatalf("log reply = %q", got)
	}
	if senseReply(opt, routePersona(), "你好") != "" {
		t.Fatal("ordinary chat must stay on the empty fallback")
	}
	if senseReply(opt, routePersona(), "看看屏幕") != "" {
		t.Fatal("a look still waits on the eye")
	}
	if handoffFallback(opt, slot) != delegAck {
		t.Fatal("an idle branch still says nothing ran")
	}
}

func TestHandoffFallbackSaysWhatIsTrue(t *testing.T) {
	opt, slot, release := routeRig(t, nil)
	defer release()
	if got := handoffFallback(opt, slot); got != delegAck {
		t.Fatalf("idle fallback = %q", got)
	}
	job := opt.stageQ.Enqueue(window.KindImage, "a red rose")
	slot.open(judge.ActImage, "画一朵红玫瑰")
	slot.setNote(window.KindImage + " queued " + job.ID)
	waitJob(t, opt.stageQ, window.KindImage, window.StatusRunning)
	if got := handoffFallback(opt, slot); !strings.Contains(got, "being made") || strings.Contains(got, "ready") {
		t.Fatalf("running fallback = %q", got)
	}
	release()
	waitJob(t, opt.stageQ, window.KindImage, window.StatusReady)
	if got := handoffFallback(opt, slot); !strings.Contains(got, "ready on the stage window") {
		t.Fatalf("ready fallback = %q", got)
	}
}
