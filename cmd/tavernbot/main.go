// tavernbot is the CLI entrypoint: run | speak | probe | judge | plan | doctor.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jingx8885/lov-evo/internal/agent"
	"github.com/jingx8885/lov-evo/internal/audio"
	"github.com/jingx8885/lov-evo/internal/avatar"
	"github.com/jingx8885/lov-evo/internal/config"
	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/livevoice"
	"github.com/jingx8885/lov-evo/internal/llm"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/planner"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "run":
		os.Exit(cmdRun(args))
	case "speak":
		os.Exit(cmdSpeak(args))
	case "probe":
		os.Exit(cmdProbe(args))
	case "judge":
		os.Exit(cmdJudge(args))
	case "plan":
		os.Exit(cmdPlan(args))
	case "doctor":
		os.Exit(cmdDoctor(args))
	case "ctxprobe":
		os.Exit(cmdCtxProbe(args))
	case "live2d":
		os.Exit(cmdLive2D(args))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `tavernbot - persona voice bot with Jev judgment and async LLM planning

Commands:
  run     start the duplex voice loop (WebRTC uplink + WS events)
  speak   one-shot TTS through the speakable channel, writes a WAV
  probe   connectivity check: call create, session.started, RTP echo
  judge   judge one text with Jev (no voice)
  plan    force one async planner refresh (prints the resulting note)
  doctor    local environment check (no network)
  ctxprobe  experiment with session.context.append channels
  live2d    serve the Haru viewer (Jev frames + lip sync over WebSocket)
`)
}

func commonFlags(fs *flag.FlagSet) (baseURL, personaPath, key *string, verbose *bool) {
	baseURL = fs.String("base-url", "", "new-api base URL (default NEW_API_BASE_URL or "+config.DefaultBaseURL+")")
	personaPath = fs.String("persona", "personas/haru.yaml", "persona YAML")
	key = fs.String("key", "", "API key (default resolves env/credentials)")
	verbose = fs.Bool("v", false, "verbose")
	return
}

func mustKey(explicit string) string {
	k, kerr := config.ResolveAPIKey()
	if explicit == "" {
		_ = kerr
		if k == "" {
			fmt.Fprintln(os.Stderr, "no API key; set LOVBROWSER_API_KEY or OPENAI_API_KEY")
			os.Exit(1)
		}
		return k
	}
	return explicit
}

func mustPersona(path string) *persona.Persona {
	p, err := persona.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return p
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	baseURL, personaPath, key, verbose := commonFlags(fs)
	jevModel := fs.String("jev-model", config.DefaultJevModel, "System One model")
	plannerModel := fs.String("planner-model", config.DefaultPlannerModel, "planner LLM")
	runsDir := fs.String("runs-dir", "runs", "session log directory")
	greeting := fs.Bool("greeting", false, "speak a scripted persona greeting on start (off: wait for the user, Jev steers)")
	live2dAddr := fs.String("live2d", "127.0.0.1:8787", "Live2D viewer addr; off to disable")
	live2dDir := fs.String("live2d-dir", "", "web/live2d directory")
	noBrowser := fs.Bool("no-browser", false, "do not open the Live2D viewer")
	say := fs.String("say", "", "speak this text once after the session starts")
	quitAfter := fs.Duration("quit-after", 0, "exit after this duration (0 = until Ctrl+C / /quit)")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *quitAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *quitAfter)
		defer cancel()
	}
	err := agent.Run(ctx, agent.Options{
		BaseURL:      config.ResolveBaseURL(*baseURL),
		APIKey:       mustKey(*key),
		PersonaPath:  *personaPath,
		JevModel:     *jevModel,
		PlannerModel: *plannerModel,
		RunsDir:      *runsDir,
		Greeting:     *greeting,
		Verbose:      *verbose,
		Live2DAddr:   *live2dAddr,
		Live2DDir:    *live2dDir,
		OpenViewer:   !*noBrowser,
		Say:          *say,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdSpeak(args []string) int {
	fs := flag.NewFlagSet("speak", flag.ExitOnError)
	baseURL, _, key, verbose := commonFlags(fs)
	text := fs.String("text", "", "text to speak")
	textFile := fs.String("text-file", "", "file with text")
	voice := fs.String("voice", "cove", "voice")
	out := fs.String("output", "", "WAV output path")
	timeout := fs.Duration("timeout", 90*time.Second, "max wait")
	fs.Parse(args)

	t := *text
	if t == "" && *textFile != "" {
		raw, err := os.ReadFile(*textFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		t = string(raw)
	}
	if strings.TrimSpace(t) == "" {
		fmt.Fprintln(os.Stderr, "--text or --text-file required")
		return 2
	}
	if *out == "" {
		*out = fmt.Sprintf("speak-%d.wav", time.Now().Unix())
	}
	ctx := context.Background()
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(*baseURL), mustKey(*key),
		"Speak the user's text exactly as written. Do not add, remove, explain, or answer anything.",
		*voice)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sess.Close()
	sess.Verbose = *verbose
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := sess.WaitStarted(wctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cancel()
	if err := sess.Speak(t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// wait for turn.done, else fall back to a silence-based cutoff
	deadline := time.Now().Add(*timeout)
	lastBytes := int64(0)
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		select {
		case ev := <-sess.Events():
			if ev.Kind == livevoice.EventTurnDone {
				assistant, _ := ev.Usage["assistant"].(string)
				fmt.Printf("turn.done user=%q assistant=%q\n", ev.Text, assistant)
				goto done
			}
		case <-time.After(500 * time.Millisecond):
		}
		n := int64(len(sess.PCM()))
		if n != lastBytes {
			tail := sess.PCM()
			if len(tail) > 1920 {
				tail = tail[len(tail)-1920:]
			}
			lastBytes = n
			if audio.MouthOpen(tail) > 0 {
				quietSince = time.Now()
			}
		} else if time.Since(quietSince) > 4*time.Second && lastBytes > 0 {
			goto done
		}
	}
done:
	pcm := sess.PCM()
	if len(pcm) == 0 {
		fmt.Fprintln(os.Stderr, "no audio received")
		return 1
	}
	if err := audio.WriteWAV(*out, pcm, audio.DownlinkRate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc, _ := json.Marshal(map[string]any{
		"output":    *out,
		"pcm_bytes": len(pcm),
		"seconds":   float64(len(pcm)) / (audio.DownlinkRate * 2),
	})
	fmt.Println(string(enc))
	return 0
}

func cmdProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	baseURL, _, key, _ := commonFlags(fs)
	fs.Parse(args)
	ctx := context.Background()
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(*baseURL), mustKey(*key),
		"You are a connectivity probe.", "cove")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sess.Close()
	counts := map[string]int{}
	deadline := time.Now().Add(20 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		select {
		case ev := <-sess.Events():
			counts[ev.Kind]++
			if ev.Kind == livevoice.EventStarted {
				started = true
			}
			if ev.Kind == livevoice.EventClosed {
				goto report
			}
		case <-time.After(300 * time.Millisecond):
		}
		if started && counts[livevoice.EventInputEcho] > 5 {
			goto report
		}
	}
report:
	fmt.Printf("started=%v input_echo=%d pcm_bytes=%d counts=%v\n",
		started, counts[livevoice.EventInputEcho], len(sess.PCM()), counts)
	if !started {
		return 1
	}
	return 0
}

func cmdJudge(args []string) int {
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	baseURL, personaPath, key, _ := commonFlags(fs)
	text := fs.String("text", "", "user text to judge")
	live2dURL := fs.String("live2d", "", "POST drive frame to viewer origin, e.g. http://127.0.0.1:8787")
	fs.Parse(args)
	if strings.TrimSpace(*text) == "" {
		fmt.Fprintln(os.Stderr, "--text required")
		return 2
	}
	p := mustPersona(*personaPath)
	jc := jev.NewClient(config.ResolveBaseURL(*baseURL), mustKey(*key), "")
	mem := memory.New(8)
	jd, err := judge.JudgeTurn(context.Background(), jc, p, mem, *text)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, _ := json.MarshalIndent(jd, "", "  ")
	fmt.Println(string(raw))
	if strings.TrimSpace(*live2dURL) != "" {
		mem.UpdateAffect(jd.Valence, jd.Arousal, jd.Emotion, jd.SafetyP >= p.Judge.SafetyThresh)
		mode := judge.DecideMode(jd, mem.Affect(), p.Judge.SafetyThresh, "")
		frame := avatar.Drive(mode, jd, mem.Affect())
		body, _ := json.Marshal(frame)
		url := strings.TrimRight(*live2dURL, "/") + "/drive"
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintln(os.Stderr, "live2d push:", err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			fmt.Fprintf(os.Stderr, "live2d push: HTTP %d\n", resp.StatusCode)
			return 1
		}
		fmt.Printf("live2d pushed mode=%s expression=%s\n", frame.Mode, frame.Expression)
	}
	return 0
}

func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	baseURL, personaPath, key, _ := commonFlags(fs)
	plannerModel := fs.String("planner-model", config.DefaultPlannerModel, "planner LLM")
	fs.Parse(args)
	p := mustPersona(*personaPath)
	base := config.ResolveBaseURL(*baseURL)
	k := mustKey(*key)
	pl := planner.New(p,
		jev.NewClient(base, k, ""),
		llm.NewClient(base, k, *plannerModel), *plannerModel)
	pl.LogFn = func(s string) { fmt.Println("[planner]", s) }
	mem := memory.New(8)
	mem.Add(memory.Turn{Speaker: "user", Text: "Let's plan something together."})
	pl.Tick(context.Background(), mem, true)
	deadline := time.Now().Add(45 * time.Second)
	for pl.Refining() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("note:", pl.Current())
	return 0
}

func cmdDoctor(args []string) int {
	baseURL := config.ResolveBaseURL("")
	_, keyErr := config.ResolveAPIKey()
	fmt.Println("base_url:", baseURL)
	if keyErr != nil {
		fmt.Println("api_key: missing (" + keyErr.Error() + ")")
	} else {
		fmt.Println("api_key: resolved")
	}
	if p := audio.NewPlayer(); p != nil {
		fmt.Println("player:", p.Kind())
		p.WritePCM(audio.TonePCM(880, 0.35))
		time.Sleep(500 * time.Millisecond)
		p.Close()
	} else {
		fmt.Println("player: none")
	}
	if ch, stop, err := audio.OpenMic(audio.PCMUUplinkRate); err != nil {
		fmt.Println("mic:", err)
	} else {
		var max float64
		n := 0
		deadline := time.Now().Add(700 * time.Millisecond)
	micLoop:
		for time.Now().Before(deadline) {
			select {
			case f, ok := <-ch:
				if !ok {
					break micLoop
				}
				n++
				if r := audio.UlawRMS(f); r > max {
					max = r
				}
			case <-time.After(50 * time.Millisecond):
			}
		}
		if stop != nil {
			stop()
		}
		fmt.Printf("mic: ready format=%s frames=%d max_rms=%.4f\n", audio.MicFormat(), n, max)
		if n == 0 {
			fmt.Println("mic: warning: no frames in 700ms")
		} else if max < 0.002 {
			fmt.Println("mic: warning: digital silence; capture format may be wrong")
		}
	}
	if dir, err := avatar.FindDir(""); err != nil {
		fmt.Println("live2d:", err)
	} else {
		fmt.Println("live2d:", dir)
	}
	return 0
}

// cmdCtxProbe experiments with session.context.append channels:
// which ones are accepted silently, which trigger a spoken reply.
func cmdCtxProbe(args []string) int {
	fs := flag.NewFlagSet("ctxprobe", flag.ExitOnError)
	baseURL, _, key, _ := commonFlags(fs)
	channel := fs.String("channel", "context", "context channel name")
	text := fs.String("text", "STEERING_NOTE: the user seems tired.", "context text")
	respond := fs.Bool("respond", false, "also send response.create")
	fs.Parse(args)
	ctx := context.Background()
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(*baseURL), mustKey(*key),
		"You are a probe.", "cove")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sess.Close()
	sess.Verbose = true
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := sess.WaitStarted(wctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cancel()
	if err := sess.AppendContext(*channel, *text); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("sent context.append channel=" + *channel)
	if *respond {
		_ = sess.Respond()
		fmt.Println("sent response.create")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-sess.Events():
			fmt.Printf("event kind=%s speaker=%s text=%.80s err=%v\n", ev.Kind, ev.Speaker, ev.Text, ev.Err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	return 0
}

func cmdLive2D(args []string) int {
	fs := flag.NewFlagSet("live2d", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	dir := fs.String("dir", "", "web/live2d directory")
	demo := fs.Bool("demo", false, "cycle steering modes so Haru visibly reacts")
	lipsync := fs.Bool("lipsync", false, "synthetic mouth motion (no voice)")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hub, url, err := avatar.Listen(ctx, avatar.ListenOptions{
		Addr:  *addr,
		Dir:   *dir,
		Open:  !*noBrowser,
		LogFn: func(s string) { fmt.Println(s) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *demo {
		fmt.Println("demo: cycling Jev modes every 4s")
		go cycleLive2DDemo(ctx, hub)
	}
	if *lipsync {
		fmt.Println("lipsync: synthetic mouth envelope")
		go cycleMouthDemo(ctx, hub)
	}
	fmt.Println("open", url, " — Ctrl+C to stop")
	<-ctx.Done()
	return 0
}

func cycleLive2DDemo(ctx context.Context, hub *avatar.Hub) {
	samples := []struct {
		mode string
		j    judge.Judgment
	}{
		{"continue", judge.Judgment{Emotion: "neutral", Valence: 0.5, Arousal: 0.4, Engagement: 0.6}},
		{"comfort", judge.Judgment{Emotion: "sadness", Valence: 0.2, Arousal: 0.3, Engagement: 0.8}},
		{"de_escalate", judge.Judgment{Emotion: "anger", Valence: 0.3, Arousal: 0.7, Engagement: 0.7}},
		{"celebrate", judge.Judgment{Emotion: "joy", Valence: 0.9, Arousal: 0.8, Engagement: 0.9}},
		{"re_engage", judge.Judgment{Emotion: "neutral", Valence: 0.5, Arousal: 0.4, Engagement: 0.15}},
		{"goal_push", judge.Judgment{Emotion: "neutral", Valence: 0.65, Arousal: 0.55, Engagement: 0.8}},
		{"safety", judge.Judgment{Emotion: "fear", Valence: 0.15, Arousal: 0.6, Engagement: 0.5, SafetyP: 0.9}},
	}
	i := 0
	tick := time.NewTicker(4 * time.Second)
	defer tick.Stop()
	for {
		s := samples[i%len(samples)]
		a := memory.Affect{Valence: s.j.Valence, Arousal: s.j.Arousal, Emotion: s.j.Emotion}
		hub.Publish(avatar.Drive(s.mode, &s.j, a))
		i++
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func cycleMouthDemo(ctx context.Context, hub *avatar.Hub) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	t0 := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			hub.Mouth(audio.MouthEnvelope(time.Since(t0).Seconds()))
		}
	}
}
