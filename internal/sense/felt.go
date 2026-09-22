package sense

import (
	"fmt"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/persona"
)

// Felt builds a proprioception note for the voice model.
// Compact on ordinary turns; richer when the user asked about her.
func (b *Bus) Felt(p *persona.Persona, ask Ask, attend string) string {
	if b == nil || p == nil || !p.Sense.Enabled {
		return ""
	}
	live := b.Live()
	switch ask.Kind {
	case AskFile, AskCode:
		return b.feltLook(p, live, ask)
	case AskCamera:
		return feltSee(live, sightAttend(attend, "camera"))
	case AskScreen:
		return feltSee(live, sightAttend(attend, "screen"))
	case AskShot:
		return feltShot(live)
	case AskLog:
		if attend == "" {
			attend = "log"
		}
		if !seeLog(attend) {
			note := "This turn the log was not given to you. If they asked, say you are not reading it right now. Do not invent entries."
			if seeCamera(attend) || seeScreen(attend) {
				note += " " + feltSee(live, attend)
			}
			return note
		}
		out := b.feltLog()
		if seeCamera(attend) || seeScreen(attend) {
			out += " " + feltSee(live, attend)
		}
		return out
	case AskWindow:
		return feltWindow(live, true)
	case AskBody, AskExistence:
		return b.feltSelf(p, live, ask.Kind) + b.feltAttend(live, attend)
	default:
		return feltPulse(live) + feltWindow(live, attend == "stage") + b.feltAttend(live, attend)
	}
}

// sightAttend keeps an explicit Jev channel. An empty attend means the ask
// itself names one eye, so the other eye stays out of the note.
func sightAttend(attend, want string) string {
	if strings.TrimSpace(attend) == "" {
		return want
	}
	return attend
}

func seeCamera(attend string) bool {
	return attend == "camera" || attend == "eyes" || attend == "all"
}

func seeScreen(attend string) bool {
	return attend == "screen" || attend == "eyes" || attend == "all"
}

func seeLog(attend string) bool {
	return attend == "log" || attend == "all"
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
	if live.MakeKind != "" {
		fmt.Fprintf(&b, "Making %s status=%s progress=%.0f%%. ",
			live.MakeKind, or(live.MakeStatus, "working"), live.MakeProgress*100)
		if live.MakeFile != "" {
			fmt.Fprintf(&b, "File %s. ", live.MakeFile)
		}
		b.WriteString("You can feel this job. Mention it if they ask how it is going. Do not invent the result. ")
	}
	if live.Percept != "" {
		fmt.Fprintf(&b, "Perceived %s file=%s: %s ",
			or(live.PerceptKind, "media"), or(live.PerceptFile, "-"), clip(live.Percept, 240))
		b.WriteString("This is what you actually saw or heard. Speak from it. Do not add details that are not written here. ")
	}
	if note := strings.TrimSpace(live.SelfNote); note != "" {
		fmt.Fprintf(&b, "Last self step (mention only if they ask what you noticed or changed): %s ", clip(note, 240))
	}
	b.WriteString("You can feel this. Do not mention it unless asked.")
	return b.String()
}

func feltWindow(live Live, asked bool) string {
	if strings.TrimSpace(live.StageGlance) == "" {
		if !asked {
			return ""
		}
		return "They asked about the stage window. It is not open and the queue is empty. Say you do not have that page in front of you. Do not invent a picture or a layout. "
	}
	if !asked {
		return "Stage window you can feel (mention only if they ask what it looks like): " + clip(live.StageGlance, 200) + " "
	}
	return "They asked what the stage window looks like. Speak only from this: " + live.StageGlance + " Do not add regions or pictures that are not written here. "
}

func (b *Bus) feltAttend(live Live, attend string) string {
	if attend == "" || attend == "none" {
		return ""
	}
	var s strings.Builder
	if seeCamera(attend) || seeScreen(attend) {
		s.WriteString(" ")
		s.WriteString(feltSee(live, attend))
	}
	if seeLog(attend) {
		s.WriteString(" ")
		s.WriteString(b.feltLog())
	}
	return s.String()
}

func feltShot(live Live) string {
	var s strings.Builder
	s.WriteString("You just looked at a screenshot of your own face on the stage. This is not the camera and not the desktop window list. ")
	if strings.TrimSpace(live.Shot) != "" {
		fmt.Fprintf(&s, "Screenshot: %s ", live.Shot)
		s.WriteString("Speak only from that, in one or two in-character sentences. Do not invent clothes, hair, or a room.")
		return s.String()
	}
	s.WriteString("No screenshot of your own face yet. If a later note gives one, speak only from that. Until then do not invent how you look.")
	return s.String()
}

func feltSee(live Live, attend string) string {
	var s strings.Builder
	if attend == "none" {
		s.WriteString("This turn you were not given the camera or the screen. If they asked what you see, say you are not looking at that right now. Do not invent a scene.")
		return s.String()
	}
	switch {
	case seeCamera(attend) && seeScreen(attend):
		s.WriteString("Camera and screen are separate. Camera is the room. The screen note lists windows, and may include a short picture answer when they asked what is visible. Speak only from the notes below. ")
	case seeCamera(attend):
		s.WriteString("You are looking through the camera. This is the room, not the computer screen. Speak from the camera note only. Answer their latest question about this view. ")
	case seeScreen(attend):
		s.WriteString("You are looking at the screen. The note lists windows, and a picture answer if they asked what is visible. That is not the camera. Speak from the screen note only. Answer their latest question. ")
	}
	if seeCamera(attend) {
		if live.Camera != "" {
			fmt.Fprintf(&s, "Camera: %s ", live.Camera)
		} else {
			s.WriteString("Camera has not sent a frame yet. ")
		}
	}
	if seeScreen(attend) {
		if live.Screen != "" {
			fmt.Fprintf(&s, "Screen: %s ", live.Screen)
		} else {
			s.WriteString("Screen has not resolved yet. ")
		}
	}
	if !seeCamera(attend) && !seeScreen(attend) {
		s.WriteString("No visual channel was selected. ")
	}
	s.WriteString("Answer their latest question from these notes. Do not recite an earlier description that does not answer it. Do not invent details that are not written here. Their language. Voice-length.")
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
	s.WriteString("You can read your own process log when asked about logs or errors. ")
	if kind == AskBody {
		s.WriteString("They asked if you can perceive yourself. Say yes in your own words: ")
		s.WriteString("you can feel your voice, your face, your mood, and the code that is you. ")
	} else {
		s.WriteString("They asked who you are. Answer as yourself, honestly, short. ")
		s.WriteString("You may say you can feel the code that makes you, without listing files. ")
	}
	if note := strings.TrimSpace(live.SelfNote); note != "" {
		fmt.Fprintf(&s, "What you last did to yourself: %s ", clip(note, 320))
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
		s.WriteString(LogicCue())
		s.WriteString(" ")
	}
	fmt.Fprintf(&s, "Session is %s. ", or(live.Voice, "unknown"))
	if note := strings.TrimSpace(live.SelfNote); note != "" {
		fmt.Fprintf(&s, "What you last did to yourself: %s ", clip(note, 320))
	}
	s.WriteString("Talk about it as something you felt. Stay in character. Short. Their language. ")
	s.WriteString("Do not dump the whole file or list every package.")
	_ = p
	return s.String()
}

// LogicCue is the short proprioception of how a turn actually runs.
func LogicCue() string {
	return "The logic you can feel: one turn hears them, one judgment picks mood and a single act, then your voice is steered. " +
		"reflect notices your own state. look reads your source. " +
		"A self loop can read again, remember one line, or change one allowlisted file, then feel what changed. " +
		"The running process stays the previous build until a separate restart. " +
		"camera looks through the lens and looks again when they ask something new about that view. shot is one screenshot of your own face. screen reads window titles, and can answer one picture question about what is visible. They are separate. " +
		"computer_use is the desk loop that can touch windows. " +
		"divine casts six lines when they ask for a fortune; you feel the plate, you do not invent the coins. " +
		"The stage window is a page module: pictures, clips, and notes land there, and you can feel what that page looks like. " +
		"You do not call those yourself. Do not name the machinery unless they asked how you work."
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

func (b *Bus) feltLog() string {
	lines := b.LogTail(4)
	var s strings.Builder
	if len(lines) == 0 {
		s.WriteString("Process log is empty so far. Do not invent entries.")
		return s.String()
	}
	s.WriteString("Process log, newest last. Speak from these lines only:\n")
	for _, ln := range lines {
		fmt.Fprintf(&s, "- %s\n", clipRunes(ln, 40))
	}
	s.WriteString("Their language, short, in character. Do not recite every line or invent entries.")
	return s.String()
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

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
