package livevoice

import (
	"errors"
	"strings"
	"testing"
	"time"
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
