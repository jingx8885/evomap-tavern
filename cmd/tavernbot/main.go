// tavernbot is the CLI entrypoint: run | speak | probe | judge | plan | doctor.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jingx8885/lov-evo/internal/agent"
	"github.com/jingx8885/lov-evo/internal/audio"
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
  doctor  local environment check (no network)
`)
}

func commonFlags(fs *flag.FlagSet) (baseURL, personaPath, key string, verbose *bool) {
	baseURL = *fs.String("base-url", "", "new-api base URL (default NEW_API_BASE_URL or "+config.DefaultBaseURL+")")
	personaPath = *fs.String("persona", "personas/tavern_keeper.yaml", "persona YAML")
	key = *fs.String("key", "", "API key (default resolves env/credentials)")
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
	greeting := fs.Bool("greeting", true, "speak persona greeting on start")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := agent.Run(ctx, agent.Options{
		BaseURL:      config.ResolveBaseURL(baseURL),
		APIKey:       mustKey(key),
		PersonaPath:  personaPath,
		JevModel:     *jevModel,
		PlannerModel: *plannerModel,
		RunsDir:      *runsDir,
		Greeting:     *greeting,
		Verbose:      *verbose,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdSpeak(args []string) int {
	fs := flag.NewFlagSet("speak", flag.ExitOnError)
	baseURL, _, key, _ := commonFlags(fs)
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
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(baseURL), mustKey(key),
		"Speak the user's text exactly as written. Do not add, remove, explain, or answer anything.",
		*voice)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sess.Close()
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
				goto done
			}
		case <-time.After(500 * time.Millisecond):
		}
		n := int64(len(sess.PCM()))
		if n != lastBytes {
			lastBytes = n
			quietSince = time.Now()
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
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(baseURL), mustKey(key),
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
	fs.Parse(args)
	if strings.TrimSpace(*text) == "" {
		fmt.Fprintln(os.Stderr, "--text required")
		return 2
	}
	p := mustPersona(personaPath)
	jc := jev.NewClient(config.ResolveBaseURL(baseURL), mustKey(key), "")
	mem := memory.New(8)
	jd, err := judge.JudgeTurn(context.Background(), jc, p, mem, *text)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, _ := json.MarshalIndent(jd, "", "  ")
	fmt.Println(string(raw))
	return 0
}

func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	baseURL, personaPath, key, _ := commonFlags(fs)
	plannerModel := fs.String("planner-model", config.DefaultPlannerModel, "planner LLM")
	fs.Parse(args)
	p := mustPersona(personaPath)
	base := config.ResolveBaseURL(baseURL)
	k := mustKey(key)
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
		fmt.Println("player: ok")
	} else {
		fmt.Println("player: none (afplay/aplay not found; WAV only)")
	}
	if _, _, err := audio.OpenMic(audio.PCMUUplinkRate); err != nil {
		fmt.Println("mic:", err)
	} else {
		fmt.Println("mic: device build")
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
	sess, err := livevoice.Connect(ctx, config.ResolveBaseURL(baseURL), mustKey(key),
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
