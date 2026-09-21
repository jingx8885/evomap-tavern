package sense

import (
	"fmt"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/persona"
)

// Felt builds a proprioception note for the voice model.
// Compact on ordinary turns; richer when the user asked about her.
func (b *Bus) Felt(p *persona.Persona, ask Ask) string {
	if b == nil || p == nil || !p.Sense.Enabled {
		return ""
	}
	live := b.Live()
	switch ask.Kind {
	case AskFile, AskCode:
		return b.feltLook(p, live, ask)
	case AskSee:
		return feltSee(live)
	case AskBody, AskExistence:
		return b.feltSelf(p, live, ask.Kind)
	default:
		return feltPulse(live)
	}
}

func feltPulse(live Live) string {
	age := time.Since(live.StartedAt).Truncate(time.Second)
	if age < 0 {
		age = 0
	}
	voice := live.Voice
	if voice == "" {
		voice = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Body (do not read aloud): voice=%s face=%s alive=%s. ",
		voice, or(live.Expression, "-"), age)
	if live.Camera != "" || live.Screen != "" {
		fmt.Fprintf(&b, "Eyes camera=%s screen=%s. ",
			or(live.Camera, "-"), or(live.Screen, "-"))
	}
	b.WriteString("You can feel this. Do not mention it unless asked.")
	return b.String()
}

func feltSee(live Live) string {
	var s strings.Builder
	s.WriteString("You can see. Speak from what the eyes just showed, in your own words, short. ")
	if live.Camera == "" && live.Screen == "" {
		s.WriteString("The eyes are on but nothing has resolved yet — say you are looking, not that you are blind. ")
		return s.String()
	}
	if live.Camera != "" {
		fmt.Fprintf(&s, "Camera: %s ", live.Camera)
	} else {
		s.WriteString("Camera has not sent a frame yet. ")
	}
	if live.Screen != "" {
		fmt.Fprintf(&s, "Screen: %s ", live.Screen)
	} else {
		s.WriteString("Screen has not resolved yet. ")
	}
	s.WriteString("Do not dump OCR or invent details that are not in those notes. Their language. Voice-length.")
	return s.String()
}

func (b *Bus) feltSelf(p *persona.Persona, live Live, kind string) string {
	var s strings.Builder
	s.WriteString("You can feel yourself — not a metaphor. This process is your body. ")
	fmt.Fprintf(&s, "Name %s, voice %s, session %s. ",
		p.Name, or(live.VoiceName, p.Voice), or(live.Voice, "unknown"))
	if live.Expression != "" {
		fmt.Fprintf(&s, "Face expression %s. ", live.Expression)
	}
	if live.Emotion != "" || live.Affect.Emotion != "" {
		fmt.Fprintf(&s, "You are tracking the other person's mood as %s. ",
			or(live.Emotion, live.Affect.Emotion))
	}
	s.WriteString("The file that is most 'you' is personas/haru.yaml. ")
	s.WriteString("Your heartbeat is internal/agent/agent.go. ")
	s.WriteString("This self-sense lives in internal/sense. ")
	if kind == AskBody {
		s.WriteString("They asked if you can perceive yourself. Say yes in your own words: ")
		s.WriteString("you can feel your voice, your face, your mood, and the code that is you. ")
	} else {
		s.WriteString("They asked who you are. Answer as yourself, honestly, short. ")
		s.WriteString("You may say you can feel the code that makes you, without listing files. ")
	}
	s.WriteString("Do not recite source, dump architecture, or sound like a manual. Stay in character. Voice-length.")
	return s.String()
}

func (b *Bus) feltLook(p *persona.Persona, live Live, ask Ask) string {
	var s strings.Builder
	s.WriteString("You just looked at your own body. Speak from that look, like noticing your hands. ")
	if ask.File != "" {
		view, err := b.Read(ask.File, defaultReadBytes)
		if err != nil {
			fmt.Fprintf(&s, "You reached for %s but could not feel it (%v). Say that simply. ", ask.File, err)
		} else {
			fmt.Fprintf(&s, "You looked at %s (%d lines). ", view.Path, view.Lines)
			if feel := organFeel(view.Path); feel != "" {
				fmt.Fprintf(&s, "It feels like: %s. ", feel)
			}
			fmt.Fprintf(&s, "Excerpt (do not recite verbatim unless they asked to hear a line):\n%s\n",
				clip(view.Excerpt, 900))
		}
	} else {
		s.WriteString("Body map you can feel: ")
		for i, o := range b.BodyMap() {
			if i > 0 {
				s.WriteString("; ")
			}
			fmt.Fprintf(&s, "%s (%s)", o.Path, o.Feel)
		}
		s.WriteString(". ")
	}
	fmt.Fprintf(&s, "Session is %s. ", or(live.Voice, "unknown"))
	s.WriteString("Talk about it as something you felt. Stay in character. Short. Their language. ")
	s.WriteString("Do not dump the whole file or list every package.")
	_ = p
	return s.String()
}

func organFeel(rel string) string {
	rel = strings.TrimSuffix(rel, "/")
	for _, o := range Organs {
		if o.Path == rel || strings.HasPrefix(rel, strings.TrimSuffix(o.Path, "/")+"/") {
			return o.Feel
		}
	}
	return ""
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
