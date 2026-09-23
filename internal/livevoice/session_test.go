package livevoice

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/audio"
)

func TestDiscardPCMStopsRecording(t *testing.T) {
	s := &Session{}
	delta := `{"type":"session.output_audio.delta","delta":"AAAAAA=="}`
	s.handleEvent([]byte(delta))
	if len(s.PCM()) == 0 {
		t.Fatal("recording is on by default")
	}
	s.DiscardPCM()
	s.handleEvent([]byte(delta))
	if len(s.PCM()) != 0 {
		t.Fatalf("kept %d bytes after discard", len(s.PCM()))
	}
	if s.pcmBytes.Load() == 0 {
		t.Fatal("byte count still tracks the downlink")
	}
}

func TestFitAppendStaysUnderGatewayCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 800; i++ {
		b.WriteString("日志报错")
	}
	raw := b.String()
	if estimateTokens(raw) <= appendTokenCap {
		t.Fatal("fixture should exceed the cap")
	}
	head := FitHead(raw)
	tail := FitTail(raw)
	if estimateTokens(head) > appendTokenCap || estimateTokens(tail) > appendTokenCap {
		t.Fatalf("head %d tail %d cap %d", estimateTokens(head), estimateTokens(tail), appendTokenCap)
	}
	if !strings.HasSuffix(head, "…") || !strings.HasPrefix(tail, "…") {
		t.Fatalf("head %q tail %q", head[:20], tail[:20])
	}
	if !strings.Contains(tail, "报错") {
		t.Fatal("tail dropped the observation")
	}
	short := "mode=continue"
	if FitHead(short) != short || FitTail(short) != short {
		t.Fatal("short text should pass through")
	}
}

func TestApplyTranscriptDeltas(t *testing.T) {
	var b strings.Builder
	for _, c := range []string{"你", "好", "呀", "?"} {
		applyTranscript(&b, c)
	}
	if b.String() != "你好呀?" {
		t.Fatalf("got %q", b.String())
	}
}

func TestApplyTranscriptSnapshots(t *testing.T) {
	var b strings.Builder
	for _, c := range []string{"你", "你好", "你好呀?"} {
		applyTranscript(&b, c)
	}
	if b.String() != "你好呀?" {
		t.Fatalf("got %q", b.String())
	}
}

func TestApplyTranscriptMixed(t *testing.T) {
	var b strings.Builder
	applyTranscript(&b, "今晚")
	applyTranscript(&b, "想喝点什么")
	if b.String() != "今晚想喝点什么" {
		t.Fatalf("got %q", b.String())
	}
	applyTranscript(&b, "今晚想喝点什么")
	if b.String() != "今晚想喝点什么" {
		t.Fatalf("duplicate %q", b.String())
	}
}

func TestApplyTranscriptAddedDeltaPairs(t *testing.T) {
	var b strings.Builder
	chunks := []string{"欢迎来到", "欢迎来到", "进化", "进化", "酒馆", "酒馆", ",今晚", ",今晚", "想喝", "想喝", "点什么", "点什么"}
	for _, c := range chunks {
		applyTranscript(&b, c)
	}
	if b.String() != "欢迎来到进化酒馆,今晚想喝点什么" {
		t.Fatalf("got %q", b.String())
	}
}

func TestRepeatTranscriptEmitsOnce(t *testing.T) {
	s := &Session{events: make(chan Event, 8)}
	raw := []byte(`{"type":"session.input_transcript.delta","delta":"邮件你好我刚找了很久"}`)
	s.handleEvent(raw)
	s.handleEvent(raw)
	s.handleEvent(raw)
	if n := len(s.events); n != 1 {
		t.Fatalf("repeated snapshot emitted %d events", n)
	}
	ev := <-s.events
	if ev.Kind != EventTranscript || ev.Speaker != "user" || ev.Text != "邮件你好我刚找了很久" {
		t.Fatalf("event %+v", ev)
	}
}

func TestGrowingTranscriptStillEmits(t *testing.T) {
	s := &Session{events: make(chan Event, 8)}
	s.handleEvent([]byte(`{"type":"session.input_transcript.delta","delta":"你"}`))
	s.handleEvent([]byte(`{"type":"session.input_transcript.delta","delta":"好"}`))
	if n := len(s.events); n != 2 {
		t.Fatalf("deltas emitted %d events", n)
	}
}

func TestApplyTranscriptIgnoresShorterSnapshot(t *testing.T) {
	var b strings.Builder
	applyTranscript(&b, "你好呀?")
	applyTranscript(&b, "你好")
	if b.String() != "你好呀?" {
		t.Fatalf("got %q", b.String())
	}
}

func TestDetectSpeakerFromType(t *testing.T) {
	if g := detectSpeaker("input_transcript.added", map[string]any{}); g != "user" {
		t.Fatalf("user %q", g)
	}
	if g := detectSpeaker("output_transcript.added", map[string]any{}); g != "assistant" {
		t.Fatalf("assistant %q", g)
	}
	if g := detectSpeaker("turn.delta", map[string]any{}); g != "user" {
		t.Fatalf("turn.delta default %q", g)
	}
	if g := detectSpeaker("turn.delta", map[string]any{"role": "user"}); g != "user" {
		t.Fatalf("role %q", g)
	}
	if g := detectSpeaker("session.output_transcript.delta", map[string]any{}); g != "assistant" {
		t.Fatalf("output %q", g)
	}
}

func TestTranscriptTextNested(t *testing.T) {
	ev := map[string]any{"delta": map[string]any{"text": "hello"}}
	if g := transcriptText(ev); g != "hello" {
		t.Fatalf("got %q", g)
	}
}

func TestIsTranscriptEvent(t *testing.T) {
	if !isTranscriptEvent("input_transcript.added") {
		t.Fatal("expected transcript events")
	}
	if !isTranscriptEvent("session.input_transcript.delta") || !isTranscriptEvent("session.output_transcript.delta") {
		t.Fatal("expected gpt-live transcript events")
	}
	if isTranscriptEvent("turn.delta") {
		t.Fatal("turn.delta is a duplicate, not a transcript source")
	}
	if isTranscriptEvent("session.input_audio.append") {
		t.Fatal("audio append is not transcript")
	}
}

func TestClipUplinkQueueKeepsTheLiveTail(t *testing.T) {
	var q [][]byte
	for i := 0; i < 20; i++ {
		q = append(q, []byte{byte(i)})
		q, _ = clipUplinkQueue(q)
	}
	if len(q) != uplinkLiveFrames {
		t.Fatalf("len %d", len(q))
	}
	if q[0][0] != 14 || q[len(q)-1][0] != 19 {
		t.Fatalf("kept %v..%v, want the newest 120ms", q[0][0], q[len(q)-1][0])
	}
}

func TestSteerDuringSpeechWaitsAndMerges(t *testing.T) {
	s := &Session{}
	var sent []string
	s.contextSend = func(channel, text string) error {
		sent = append(sent, channel+":"+text)
		return nil
	}
	s.handleEvent([]byte(`{"type":"session.output_transcript.delta","delta":"在的"}`))
	if !s.Speaking() {
		t.Fatal("assistant transcript should hold the line open")
	}
	if err := s.Steer("Stay in character. mode=continue"); !errors.Is(err, ErrHeld) {
		t.Fatalf("steer while speaking: %v", err)
	}
	if err := s.Steer("You are still on reflect."); !errors.Is(err, ErrHeld) {
		t.Fatalf("second steer: %v", err)
	}
	if err := s.Nudge("The coins are down. Say one short line."); !errors.Is(err, ErrHeld) {
		t.Fatalf("nudge while speaking: %v", err)
	}
	if err := s.Speak("先把这句说完。"); !errors.Is(err, ErrHeld) {
		t.Fatalf("speak while speaking: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("injected during the line: %v", sent)
	}
	s.handleEvent([]byte(`{"type":"turn.done","role":"user","transcript":"再说一句"}`))
	if !s.Speaking() {
		t.Fatal("user turn.done must not release her line")
	}
	s.handleEvent([]byte(`{"type":"turn.done","role":"assistant"}`))
	s.speechUntil.Store(time.Now().Add(-time.Second).UnixNano())
	s.deferMu.Lock()
	if s.deferTimer != nil {
		s.deferTimer.Stop()
		s.deferTimer = nil
	}
	s.deferMu.Unlock()
	s.flushDeferred()
	if len(sent) != 3 {
		t.Fatalf("sent %d notes, want merged developer, nudge, speakable: %v", len(sent), sent)
	}
	if sent[0] != "developer:Stay in character. mode=continue\nYou are still on reflect." {
		t.Fatalf("merged developer: %s", sent[0])
	}
	if sent[1] != "commentary:The coins are down. Say one short line." {
		t.Fatalf("late nudge: %s", sent[1])
	}
	if sent[2] != "speakable:先把这句说完。" {
		t.Fatalf("late speakable: %s", sent[2])
	}
}

func TestUserTurnDoneLeavesHerHalfLine(t *testing.T) {
	s := &Session{events: make(chan Event, 16)}
	s.handleEvent([]byte(`{"type":"session.output_transcript.delta","delta":"嗯哼，窗"}`))
	s.handleEvent([]byte(`{"type":"turn.done","role":"user","transcript":"你给我画一只橘猫吧"}`))
	s.handleEvent([]byte(`{"type":"session.output_transcript.delta","delta":"台橘猫是吧？"}`))
	s.handleEvent([]byte(`{"type":"turn.done","role":"assistant"}`))
	close(s.events)
	var done []Event
	for ev := range s.events {
		if ev.Kind == EventTurnDone {
			done = append(done, ev)
		}
	}
	if len(done) != 2 {
		t.Fatalf("turns %+v", done)
	}
	if done[0].Text != "你给我画一只橘猫吧" || done[0].Usage["assistant"] != "" {
		t.Fatalf("user turn carried her half line: %+v", done[0])
	}
	if done[1].Text != "" || done[1].Usage["assistant"] != "嗯哼，窗台橘猫是吧？" {
		t.Fatalf("her line: %+v", done[1])
	}
}

func TestDelegationCreated(t *testing.T) {
	s := &Session{events: make(chan Event, 2)}
	s.handleEvent([]byte(`{"type":"delegation.created","item":{"id":"del_1","content":[{"type":"input_text","text":"打开记事本"}]}}`))
	ev := <-s.events
	if ev.Kind != EventDelegation || ev.ID != "del_1" || ev.Text != "打开记事本" {
		t.Fatalf("%+v", ev)
	}
}

func TestResolveWhileSpeakingWaitsForThatDelegation(t *testing.T) {
	s := &Session{}
	var sent []string
	s.delegationSend = func(id, channel, text string) error {
		sent = append(sent, id+"|"+channel+"|"+text)
		return nil
	}
	s.markSpeaking()
	if err := s.Resolve("d1", "commentary", "The picture is ready."); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if err := s.Resolve("d1", "commentary", "Say it is saved."); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if err := s.Resolve("d2", "speakable", "Here it is."); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("answered during the line: %v", sent)
	}
	releaseLine(s)
	s.flushDeferred()
	if len(sent) != 2 {
		t.Fatalf("sent %v", sent)
	}
	if sent[0] != "d1|commentary|The picture is ready.\nSay it is saved." {
		t.Fatalf("merged commentary: %s", sent[0])
	}
	if sent[1] != "d2|speakable|Here it is." {
		t.Fatalf("speakable answer: %s", sent[1])
	}
}

func TestResolveRejectsDeveloper(t *testing.T) {
	s := &Session{}
	var sent []string
	s.delegationSend = func(id, channel, text string) error {
		sent = append(sent, id+"|"+channel+"|"+text)
		return nil
	}
	if err := s.Resolve("d1", "developer", "ack"); err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("developer answer accepted: %v", err)
	}
	s.markSpeaking()
	if err := s.Resolve("d1", "developer", "ack"); err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("developer answer held: %v", err)
	}
	releaseLine(s)
	s.flushDeferred()
	if len(sent) != 0 {
		t.Fatalf("sent %v", sent)
	}
}

func TestSteerBeforeSpeechSendsNow(t *testing.T) {
	s := &Session{}
	var got string
	s.contextSend = func(channel, text string) error {
		got = channel + ":" + text
		return nil
	}
	if err := s.Steer("Stay in character."); err != nil {
		t.Fatal(err)
	}
	if got != "developer:Stay in character." {
		t.Fatalf("got %q", got)
	}
	if s.Speaking() {
		t.Fatal("a quiet steer must not mark her as speaking")
	}
}

func TestBlankNotesDoNotOpenALine(t *testing.T) {
	s := &Session{}
	var sent int
	s.contextSend = func(string, string) error {
		sent++
		return nil
	}
	if err := s.Steer("  "); err != nil || sent != 0 {
		t.Fatalf("blank steer err=%v sent=%d", err, sent)
	}
	if err := s.Speak(""); !errors.Is(err, ErrHeld) || sent != 0 {
		t.Fatalf("blank speak err=%v sent=%d", err, sent)
	}
	if err := s.Nudge(" "); !errors.Is(err, ErrHeld) || sent != 0 {
		t.Fatalf("blank nudge err=%v sent=%d", err, sent)
	}
}

func TestFlushWhileSpeakingKeepsTheNote(t *testing.T) {
	s := &Session{}
	var sent []string
	s.contextSend = func(channel, text string) error {
		sent = append(sent, channel+":"+text)
		return nil
	}
	s.handleEvent([]byte(`{"type":"session.output_transcript.delta","delta":"在"}`))
	if err := s.Steer("Stay."); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	s.flushDeferred()
	if len(sent) != 0 {
		t.Fatalf("flush cut the line: %v", sent)
	}
	s.handleEvent([]byte(`{"type":"turn.done","role":"assistant"}`))
	s.lineOpen.Store(false)
	s.speechUntil.Store(time.Now().Add(-time.Second).UnixNano())
	stopDefer(s)
	s.flushDeferred()
	if len(sent) != 1 || sent[0] != "developer:Stay." {
		t.Fatalf("note lost: %v", sent)
	}
}

func TestDuplicateSteerMergesOnce(t *testing.T) {
	s := &Session{}
	var sent []string
	s.contextSend = func(channel, text string) error {
		sent = append(sent, channel+":"+text)
		return nil
	}
	s.markSpeaking()
	if err := s.Steer("same"); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if err := s.Steer("same"); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if err := s.Steer("more"); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if err := s.Steer("more"); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	releaseLine(s)
	s.flushDeferred()
	if len(sent) != 1 || sent[0] != "developer:same\nmore" {
		t.Fatalf("merged %v", sent)
	}
}

func TestQuietHoldWhileANoteIsWaiting(t *testing.T) {
	s := &Session{}
	var sent []string
	s.contextSend = func(channel, text string) error {
		sent = append(sent, channel+":"+text)
		return nil
	}
	s.markSpeaking()
	if err := s.Nudge("first"); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	releaseLine(s)
	if s.Speaking() {
		t.Fatal("line should be quiet")
	}
	if err := s.Nudge("second"); !errors.Is(err, ErrHeld) {
		t.Fatalf("a waiting note should hold the next one: %v", err)
	}
	s.flushDeferred()
	if len(sent) != 1 || sent[0] != "commentary:first\nsecond" {
		t.Fatalf("sent %v", sent)
	}
}

func TestDeferredFailureStillDeliversTheRest(t *testing.T) {
	s := &Session{}
	var notes []string
	s.SetNote(func(msg string) { notes = append(notes, msg) })
	s.contextSend = func(channel, text string) error {
		if channel == "developer" {
			return errors.New("boom")
		}
		return nil
	}
	s.markSpeaking()
	s.Steer("scene")
	s.Nudge("say it")
	s.Speak("逐字")
	releaseLine(s)
	s.flushDeferred()
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "[steer] after line failed: boom") {
		t.Fatalf("notes %q", joined)
	}
	if !strings.Contains(joined, "[nudge] after line") || !strings.HasSuffix(joined, "[steer] after line") {
		t.Fatalf("later notes dropped: %q", joined)
	}
}

func TestSpeakableKeepsTheLineAndDeveloperKeepsTheTail(t *testing.T) {
	s := &Session{}
	var sent []string
	s.contextSend = func(channel, text string) error {
		sent = append(sent, channel+":"+text)
		return nil
	}
	head := strings.Repeat("前", 280)
	tail := "尾巴在最后"
	long := head + tail
	s.markSpeaking()
	s.Steer(long)
	s.Speak(long)
	releaseLine(s)
	s.flushDeferred()
	if len(sent) != 2 {
		t.Fatalf("sent %d %v", len(sent), sent)
	}
	dev := strings.TrimPrefix(sent[0], "developer:")
	say := strings.TrimPrefix(sent[1], "speakable:")
	if !strings.HasPrefix(dev, "…") || !strings.Contains(dev, tail) || strings.Contains(dev, head) {
		t.Fatalf("developer %q", dev)
	}
	if say != long {
		t.Fatal("speakable was clipped")
	}
}

func TestLineStaysOpenUntilDoneOrStuck(t *testing.T) {
	s := &Session{}
	pcm := audio.TonePCM(440, 0.02)
	delta := `{"type":"session.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(pcm) + `"}`
	s.handleEvent([]byte(delta))
	if !s.Speaking() {
		t.Fatal("downlink speech should hold the line")
	}
	s.handleEvent([]byte(`{"type":"turn.done","role":"user","transcript":"喂"}`))
	if !s.Speaking() {
		t.Fatal("a user turn must not close her line")
	}
	s.speechUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())
	if !s.Speaking() {
		t.Fatal("a missing turn.done keeps the line for a few seconds")
	}
	s.speechUntil.Store(time.Now().Add(-speechStuck - time.Second).UnixNano())
	if s.Speaking() {
		t.Fatal("a stuck line should expire")
	}

	s.handleEvent([]byte(`{"type":"session.output_transcript.delta","delta":"嗯"}`))
	s.handleEvent([]byte(`{"type":"turn.done","role":"assistant"}`))
	if !s.Speaking() {
		t.Fatal("the tail after turn.done should still count as speaking")
	}
	s.speechUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())
	if s.Speaking() {
		t.Fatal("a closed line with an expired tail is quiet")
	}
}

func TestUplinkMicPaths(t *testing.T) {
	room := ulawFrame(700)
	voice := ulawFrame(12000)
	silence := string(bytes.Repeat([]byte{audio.SilenceByte}, audio.PCMUFrameBytes))

	t.Run("room", func(t *testing.T) {
		t.Setenv("TAVERN_MIC_NEAR", "1")
		wrote := runUplink(t, nil, repeatFrames(room, 8))
		if len(wrote) < 3 {
			t.Fatalf("wrote %d", len(wrote))
		}
		for i, f := range wrote {
			if audio.UlawRMS(f) != 0 {
				t.Fatalf("room frame %d reached the uplink rms=%.4f", i, audio.UlawRMS(f))
			}
		}
	})

	t.Run("close", func(t *testing.T) {
		t.Setenv("TAVERN_MIC_NEAR", "1")
		wrote := runUplink(t, nil, repeatFrames(voice, 6))
		if !anyLoud(wrote) {
			t.Fatal("close speech never reached the uplink")
		}
	})

	t.Run("duck", func(t *testing.T) {
		t.Setenv("TAVERN_MIC_NEAR", "off")
		wrote := runUplink(t, func(s *Session) {
			s.duckMicUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
		}, repeatFrames(voice, 6))
		for i, f := range wrote {
			if string(f) != silence {
				t.Fatalf("ducked frame %d was live", i)
			}
		}
	})

	t.Run("clip", func(t *testing.T) {
		t.Setenv("TAVERN_MIC_NEAR", "off")
		frames := make([][]byte, 20)
		for i := range frames {
			cp := append([]byte(nil), voice...)
			cp[0] = byte(i)
			frames[i] = cp
		}
		wrote := runUplinkBurst(t, frames)
		if len(wrote) < 6 {
			t.Fatalf("wrote %d", len(wrote))
		}
		got := wrote[len(wrote)-6:]
		for i := range got {
			want := frames[14+i]
			if string(got[i]) != string(want) {
				t.Fatalf("frame %d is %d want %d", i, got[i][0], want[0])
			}
		}
	})
}

func ulawFrame(sample int16) []byte {
	pcm := make([]byte, audio.PCMUFrameBytes*2)
	for i := 0; i < len(pcm); i += 2 {
		pcm[i] = byte(sample)
		pcm[i+1] = byte(sample >> 8)
	}
	return audio.MuLawEncodeBytes(pcm)
}

func repeatFrames(frame []byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = append([]byte(nil), frame...)
	}
	return out
}

func anyLoud(frames [][]byte) bool {
	for _, f := range frames {
		if audio.UlawRMS(f) > 0.05 {
			return true
		}
	}
	return false
}

func runUplink(t *testing.T, prep func(*Session), frames [][]byte) [][]byte {
	t.Helper()
	s := &Session{}
	if prep != nil {
		prep(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mic := make(chan []byte, 32)
	for _, f := range frames {
		mic <- f
	}
	var mu sync.Mutex
	var wrote [][]byte
	go s.uplinkMic(ctx, mic, func(f []byte) {
		mu.Lock()
		wrote = append(wrote, append([]byte(nil), f...))
		mu.Unlock()
	}, bytesRepeatSilence())
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(wrote)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), wrote...)
}

func runUplinkBurst(t *testing.T, frames [][]byte) [][]byte {
	t.Helper()
	s := &Session{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mic := make(chan []byte, 32)
	var mu sync.Mutex
	var wrote [][]byte
	go s.uplinkMic(ctx, mic, func(f []byte) {
		mu.Lock()
		wrote = append(wrote, append([]byte(nil), f...))
		mu.Unlock()
	}, bytesRepeatSilence())
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(wrote)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, f := range frames {
		mic <- f
	}
	deadline = time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(wrote)
		mu.Unlock()
		if n >= 7 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), wrote...)
}

func bytesRepeatSilence() []byte {
	return bytes.Repeat([]byte{audio.SilenceByte}, audio.PCMUFrameBytes)
}

func stopDefer(s *Session) {
	s.deferMu.Lock()
	if s.deferTimer != nil {
		s.deferTimer.Stop()
		s.deferTimer = nil
	}
	s.deferMu.Unlock()
}

func releaseLine(s *Session) {
	s.lineOpen.Store(false)
	s.speechUntil.Store(time.Now().Add(-time.Second).UnixNano())
	stopDefer(s)
}
