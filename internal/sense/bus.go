package sense

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jingx8885/lov-evo/internal/memory"
)

const defaultCap = 256

// Options configure the sense bus.
type Options struct {
	Root    string
	RunsDir string
	Cap     int
}

// Live is the current felt state of her running body.
type Live struct {
	StartedAt  time.Time     `json:"started_at"`
	Voice      string        `json:"voice"` // up | down | connecting
	Persona    string        `json:"persona,omitempty"`
	VoiceName  string        `json:"voice_name,omitempty"`
	Mode       string        `json:"mode,omitempty"`
	Expression string        `json:"expression,omitempty"`
	Emotion    string        `json:"emotion,omitempty"`
	Affect     memory.Affect `json:"affect"`
	Plan       string        `json:"plan,omitempty"`
	Mouth      float64       `json:"mouth,omitempty"`
	Turns      int           `json:"turns"`
	LastUser   string        `json:"last_user,omitempty"`
	LastAsk    string        `json:"last_ask,omitempty"`
	Camera     string        `json:"camera,omitempty"`
	Screen     string        `json:"screen,omitempty"`
}

// Snapshot is the full self-picture: who, now, body map, recent events.
type Snapshot struct {
	Who    string         `json:"who"`
	Root   string         `json:"root"`
	Live   Live           `json:"live"`
	Body   []Organ        `json:"body,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
	Recent []Event        `json:"recent,omitempty"`
}

// Bus is a goroutine-safe ring of events plus the live felt state.
type Bus struct {
	mu     sync.Mutex
	root   string
	cap    int
	events []Event
	live   Live
	counts map[string]int
	jsonl  *os.File
}

// Open starts a bus rooted at this repo. RunsDir, if set, appends JSONL.
func Open(opt Options) (*Bus, error) {
	root, err := FindRoot(opt.Root)
	if err != nil {
		return nil, err
	}
	capn := opt.Cap
	if capn <= 0 {
		capn = defaultCap
	}
	b := &Bus{
		root:   root,
		cap:    capn,
		live:   Live{StartedAt: time.Now(), Voice: "connecting"},
		counts: map[string]int{},
	}
	if dir := opt.RunsDir; dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			p := filepath.Join(dir, "sense.jsonl")
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				b.jsonl = f
			}
		}
	}
	return b, nil
}

// Close releases the JSONL file, if any.
func (b *Bus) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.jsonl == nil {
		return nil
	}
	err := b.jsonl.Close()
	b.jsonl = nil
	return err
}

// Root is the repo root she can look at.
func (b *Bus) Root() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.root
}

// Emit records one event.
func (b *Bus) Emit(ev Event) {
	if b == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.mu.Lock()
	b.events = append(b.events, ev)
	if len(b.events) > b.cap {
		b.events = b.events[len(b.events)-b.cap:]
	}
	b.counts[ev.Kind]++
	if b.jsonl != nil {
		if raw, err := json.Marshal(ev); err == nil {
			_, _ = b.jsonl.Write(append(raw, '\n'))
		}
	}
	b.mu.Unlock()
}

// Set patches the live felt state.
func (b *Bus) Set(fn func(*Live)) {
	if b == nil || fn == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(&b.live)
}

// Live returns a copy of the current felt state.
func (b *Bus) Live() Live {
	if b == nil {
		return Live{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live
}

// Recent returns the last n events, oldest first.
func (b *Bus) Recent(n int) []Event {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ev := b.events
	if n > 0 && len(ev) > n {
		ev = ev[len(ev)-n:]
	}
	out := make([]Event, len(ev))
	copy(out, ev)
	return out
}

// Snapshot builds the operator + self picture.
func (b *Bus) Snapshot(who string) Snapshot {
	if b == nil {
		return Snapshot{}
	}
	s := Snapshot{
		Who:    who,
		Root:   b.Root(),
		Live:   b.Live(),
		Body:   b.BodyMap(),
		Recent: b.Recent(24),
	}
	b.mu.Lock()
	s.Counts = make(map[string]int, len(b.counts))
	for k, v := range b.counts {
		s.Counts[k] = v
	}
	b.mu.Unlock()
	return s
}
