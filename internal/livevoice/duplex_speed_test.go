package livevoice

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/audio"
	"github.com/jingx8885/lov-evo/internal/config"
)

// TestDuplexSpeed times one live gpt-live call.
// It stays off the default suite: set TAVERN_DUPLEX_SPEED=1 to run it.
func TestDuplexSpeed(t *testing.T) {
	if os.Getenv("TAVERN_DUPLEX_SPEED") == "" {
		t.Skip("set TAVERN_DUPLEX_SPEED=1 to time a live duplex call")
	}
	key := duplexKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	sess, err := ConnectScripted(ctx, config.ResolveBaseURL(""), key,
		"Timing probe. Speak only an injected line.", "cove")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.player.Close()

	ice := since(sess.born, sess.iceAt)
	call := since(sess.iceAt, sess.callAt)
	ws := since(sess.callAt, sess.wsAt)
	connected := since(sess.born, sess.wsAt)

	if err := sess.WaitStarted(ctx); err != nil {
		t.Fatal(err)
	}
	started := time.Since(sess.born)

	echo, echoOK := waitKind(sess, sess.born, 8*time.Second, func(ev Event) bool {
		return ev.Kind == EventInputEcho
	})

	before := len(sess.PCM())
	speakAt := time.Now()
	if err := sess.Speak("在。"); err != nil {
		t.Fatal(err)
	}
	voice, text, voiceOK, textOK := waitReply(sess, before, speakAt, 20*time.Second)

	t.Logf("duplex speed: ice=%s call=%s (dns=%s tcp=%s tls=%s ttfb=%s body=%s) ws=%s connect=%s started=%s echo=%s first_audio=%s first_text=%s",
		ice, call,
		since(sess.httpAt, sess.dnsAt), since(sess.httpAt, sess.dialAt), since(sess.httpAt, sess.tlsAt),
		since(sess.httpAt, sess.ttfbAt), since(sess.ttfbAt, sess.bodyAt),
		ws, connected, started.Round(time.Millisecond),
		mark(echo, echoOK), mark(voice, voiceOK), mark(text, textOK))
	if !voiceOK {
		t.Fatal("no downlink audio within 20s of speakable")
	}
}

func since(a, b time.Time) time.Duration {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	return b.Sub(a).Round(time.Millisecond)
}

func mark(d time.Duration, ok bool) string {
	if !ok {
		return "timeout"
	}
	return d.Round(time.Millisecond).String()
}

func waitKind(s *Session, from time.Time, d time.Duration, match func(Event) bool) (time.Duration, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case ev := <-s.Events():
			if match(ev) {
				return time.Since(from), true
			}
		case <-timer.C:
			return 0, false
		}
	}
}

func waitReply(s *Session, before int, from time.Time, d time.Duration) (voice, text time.Duration, voiceOK, textOK bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !voiceOK || !textOK {
		select {
		case ev := <-s.Events():
			if !textOK && ev.Kind == EventTranscript && ev.Speaker != "user" && strings.TrimSpace(ev.Text) != "" {
				text = time.Since(from)
				textOK = true
			}
		case <-tick.C:
			if voiceOK {
				continue
			}
			pcm := s.PCM()
			if len(pcm) > before && audio.ChunkHasVoice(pcm[before:]) {
				voice = time.Since(from)
				voiceOK = true
			}
		case <-timer.C:
			return voice, text, voiceOK, textOK
		}
	}
	return voice, text, voiceOK, textOK
}

func duplexKey(t *testing.T) string {
	t.Helper()
	if k, err := config.ResolveAPIKey(); err == nil && k != "" {
		return k
	}
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home = os.Getenv("HOME")
	}
	f, err := os.Open(filepath.Join(home, ".config", "akasha", "credentials.env"))
	if err != nil {
		t.Fatal("no API key for the duplex timing call")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "LOVBROWSER_API_KEY=") || strings.HasPrefix(line, "OPENAI_API_KEY=") {
			_, v, ok := strings.Cut(line, "=")
			v = strings.Trim(strings.TrimSpace(v), "\"'")
			if ok && v != "" {
				return v
			}
		}
	}
	t.Fatal("no API key for the duplex timing call")
	return ""
}
