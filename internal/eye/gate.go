package eye

import (
	"context"
	"strings"

	"github.com/jingx8885/lov-evo/internal/jev"
)

const (
	defaultNoteworthy = 0.55
	defaultPrivate    = 0.7
	defaultMention    = 0.78
	hashFallback      = 0.08
)

func gateQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		"noteworthy": {
			Type: "noul",
			Instructions: "Did the visual scene change in a way a companion should notice? " +
				"Use hash_delta, source, and last_caption in state. Tiny lighting flicker is not noteworthy. " +
				"A person appearing, a new window, or a clearly different scene is noteworthy.",
		},
		"private": {
			Type: "noul",
			Instructions: "Is this likely private (password field, banking, private chat, identity document)? " +
				"If last_caption suggests secrets, high. Ordinary coding or a face on camera is low.",
		},
		"mention": {
			Type: "noul",
			Instructions: "Should she mention this unprompted on the next voice turn? Almost always low. " +
				"High only if something startling is on camera and it is not private.",
		},
	}
}

type gateCall struct {
	Noteworthy bool
	Private    bool
	Mention    bool
	Raw        map[string]jev.Answer
}

func (e *Eyes) gate(ctx context.Context, src string, delta float64, lastCaption string, bytes, w, h int) gateCall {
	out := gateCall{}
	if e.opt.Jev == nil {
		out.Noteworthy = delta >= hashFallback
		return out
	}
	state := map[string]any{
		"source":       src,
		"hash_delta":   delta,
		"bytes":        bytes,
		"width":        w,
		"height":       h,
		"last_caption": lastCaption,
		"note":         "Jev does not see pixels. Judge from the structured fields only.",
	}
	res, err := e.opt.Jev.Evaluate(ctx, state, gateQuestions())
	if err != nil {
		e.log("jev gate: %v (falling back to hash)", err)
		out.Noteworthy = delta >= hashFallback
		return out
	}
	out.Raw = res.Answers
	out.Noteworthy = noulOf(res.Answers["noteworthy"]) >= defaultNoteworthy
	out.Private = noulOf(res.Answers["private"]) >= defaultPrivate
	out.Mention = noulOf(res.Answers["mention"]) >= defaultMention && !out.Private
	if out.Private {
		out.Noteworthy = false
	}
	return out
}

func noulOf(a jev.Answer) float64 {
	if a.Noul == nil {
		return 0
	}
	return *a.Noul
}

func describePrompt(source string) (system, user string) {
	if source == SourceShot {
		system = "You caption one screenshot of a Live2D character, for that character. " +
			"One or two short Chinese sentences about her appearance only: hair, expression, clothes, and pose. " +
			"Do not describe a room, a desktop, window titles, or a person behind a camera. " +
			"Do not invent details that are not in the image."
		user = "This screenshot is you, on your own stage. What do you look like right now?"
		return system, user
	}
	system = "You caption a single JPEG from a room camera for a voice companion. " +
		"One or two short Chinese sentences. No lists, no speculation about identity. " +
		"Say whether a person is visible, rough expression, and lighting. " +
		"If private (passwords, banking on a phone), say only that it looks private. " +
		"This is never a computer screenshot, and never her own Live2D face."
	user = "This is the camera pointed at the room. What do you see?"
	return system, user
}

func clipCaption(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
