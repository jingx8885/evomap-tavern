package studio

import (
	"context"
	"encoding/json"
	"strings"
)

// Completer writes the free-text prompt. Jev does not.
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// Compose asks the slow model for one concrete prompt. On failure it uses
// the utterance itself.
func Compose(ctx context.Context, kind, utterance string, llm Completer) string {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return ""
	}
	if llm == nil {
		return clipRunes(utterance, 400)
	}
	system := "You write one generation prompt for a voice companion. " +
		"Output STRICT JSON only: {\"prompt\":\"...\"}. " +
		"Kind is " + kind + ". " +
		"Image and video: one concrete scene, no real people, no copyrighted characters. " +
		"Speech: the exact short line to speak, in the user's language. " +
		"Song: a music description (mood, genre, vocals or instrumental), not a full lyric sheet unless they already gave lyrics."
	text, err := llm.ChatComplete(ctx, system, utterance)
	if err != nil {
		return clipRunes(utterance, 400)
	}
	var out struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(extractJSON(text)), &out); err != nil || strings.TrimSpace(out.Prompt) == "" {
		return clipRunes(utterance, 400)
	}
	return clipRunes(strings.TrimSpace(out.Prompt), 800)
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}
