package livevoice

import (
	"strings"
	"testing"
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
	if g := detectSpeaker("turn.delta", map[string]any{"role": "user"}); g != "user" {
		t.Fatalf("role %q", g)
	}
}

func TestTranscriptTextNested(t *testing.T) {
	ev := map[string]any{"delta": map[string]any{"text": "hello"}}
	if g := transcriptText(ev); g != "hello" {
		t.Fatalf("got %q", g)
	}
}

func TestIsTranscriptEvent(t *testing.T) {
	if !isTranscriptEvent("turn.delta") || !isTranscriptEvent("input_transcript.added") {
		t.Fatal("expected transcript events")
	}
	if isTranscriptEvent("session.input_audio.append") {
		t.Fatal("audio append is not transcript")
	}
}
