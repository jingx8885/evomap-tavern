package livevoice

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/audio"
)

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

func TestPlayGateHangoverKeepsIntraSpeechGaps(t *testing.T) {
	var g playGate
	speech := audio.TonePCM(440, 0.02)
	silence := make([]byte, len(speech))
	if g.Allow(silence) {
		t.Fatal("silence before speech should stay gated off")
	}
	if !g.Allow(speech) {
		t.Fatal("speech should open the gate")
	}
	if !g.Allow(silence) {
		t.Fatal("immediate silence after speech should still play")
	}
	time.Sleep(playHangover + 40*time.Millisecond)
	if g.Allow(silence) {
		t.Fatal("comfort noise after hangover should stay off")
	}
}

func TestPlayGateOpensOnLeadingSpeech(t *testing.T) {
	var g playGate
	pcm := make([]byte, (480+240)*2)
	for i := 0; i < 240; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(22000)))
	}
	if audio.MouthOpen(pcm) != 0 {
		t.Fatal("setup: trailing window is quiet")
	}
	if !g.Allow(pcm) {
		t.Fatal("chunk that starts with speech must play")
	}
}
