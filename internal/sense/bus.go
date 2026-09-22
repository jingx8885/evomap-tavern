package sense

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jingx8885/lov-evo/internal/memory"
)

const (
	defaultCap  = 256
	defaultNote = 64
)

// Options configure the sense bus.
type Options struct {
	Root    string
	RunsDir string
	Cap     int
}

// Live is the current felt state of her running body.
type Live struct {
	StartedAt    time.Time     `json:"started_at"`
	Voice        string        `json:"voice"` // up | down | connecting
	Persona      string        `json:"persona,omitempty"`
	VoiceName    string        `json:"voice_name,omitempty"`
	Mode         string        `json:"mode,omitempty"`
	Expression   string        `json:"expression,omitempty"`
	Emotion      string        `json:"emotion,omitempty"`
	Affect       memory.Affect `json:"affect"`
	Plan         string        `json:"plan,omitempty"`
	Mouth        float64       `json:"mouth,omitempty"`
	Turns        int           `json:"turns"`
	LastUser     string        `json:"last_user,omitempty"`
	LastAsk      string        `json:"last_ask,omitempty"`
	Camera       string        `json:"camera,omitempty"`
	Screen       string        `json:"screen,omitempty"`
	Shot         string        `json:"shot,omitempty"`
	LastFault    string        `json:"last_fault,omitempty"`
	LastFaultAt  time.Time     `json:"last_fault_at,omitempty"`
	MakeKind     string        `json:"make_kind,omitempty"`
	MakeStatus   string        `json:"make_status,omitempty"`
	MakeProgress float64       `json:"make_progress,omitempty"`
	MakeFile     string        `json:"make_file,omitempty"`
	StageOpen    bool          `json:"stage_open,omitempty"`
	StageLayout  string        `json:"stage_layout,omitempty"`
	StageGlance  string        `json:"stage_glance,omitempty"`
	PerceptKind  string        `json:"percept_kind,omitempty"`
	PerceptFile  string        `json:"percept_file,omitempty"`
	Percept      string        `json:"percept,omitempty"`
	SelfNote     string        `json:"self_note,omitempty"`
}

// Snapshot is the full self-picture: who, now, body map, recent events.
type Snapshot struct {
	Who    string         `json:"who"`
	Root   string         `json:"root"`
	Live   Live           `json:"live"`
	Body   []Organ        `json:"body,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
	Recent []Event        `json:"recent,omitempty"`
	Log    []string       `json:"log,omitempty"`
}

// Bus is a goroutine-safe ring of events plus the live felt state.
type Bus struct {
	mu      sync.Mutex
	root    string
	cap     int
	events  []Event
	live    Live
	counts  map[string]int
	jsonl   *os.File
	notes   []string
	noteCap int
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
		root:    root,
		cap:     capn,
		live:    Live{StartedAt: time.Now(), Voice: "connecting"},
		counts:  map[string]int{},
		noteCap: defaultNote,
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

// Note appends one operator log line she can later feel.
// Only the first line is kept, so a multi-line dump cannot flood the journal.
func (b *Bus) Note(line string) {
	if b == nil {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	line = clipRunes(line, 180)
	if line == "" || !journalKeep(line) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes = append(b.notes, line)
	if len(b.notes) > b.noteCap {
		b.notes = b.notes[len(b.notes)-b.noteCap:]
	}
	if isFault(line) {
		b.live.LastFault = line
		b.live.LastFaultAt = time.Now()
	}
}

// LogTail returns the last n log lines, oldest first.
func (b *Bus) LogTail(n int) []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	notes := b.notes
	if n > 0 && len(notes) > n {
		notes = notes[len(notes)-n:]
	}
	out := make([]string, len(notes))
	copy(out, notes)
	return out
}

// journalKeep drops routine chatter. Camera and screen captions live on
// Live, not in this journal. Faults stay.
func journalKeep(line string) bool {
	if strings.HasPrefix(line, "[eye] ") {
		rest := strings.TrimPrefix(line, "[eye] ")
		return isFault(rest) || strings.Contains(rest, "vlm:")
	}
	switch {
	case strings.HasPrefix(line, "[user~]"),
		strings.HasPrefix(line, "[user] "),
		strings.HasPrefix(line, "[assistant] "),
		strings.HasPrefix(line, "[steer] mode="),
		strings.HasPrefix(line, "[status]"),
		strings.HasPrefix(line, "[sense]"),
		strings.HasPrefix(line, "commands:"),
		strings.HasPrefix(line, "session started"):
		return false
	}
	if strings.Contains(line, "uplink:") {
		return false
	}
	if strings.HasPrefix(line, "[judge]") && !isFault(line) && !strings.Contains(line, "skipped") {
		return false
	}
	if strings.HasPrefix(line, "[nudge] ") && !isFault(line) {
		return false
	}
	return true
}

func isFault(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "error") ||
		strings.Contains(l, "failed") ||
		strings.Contains(l, "warning") ||
		strings.Contains(line, "失败") ||
		strings.Contains(line, "报错")
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
		Log:    b.LogTail(20),
	}
	b.mu.Lock()
	s.Counts = make(map[string]int, len(b.counts))
	for k, v := range b.counts {
		s.Counts[k] = v
	}
	b.mu.Unlock()
	return s
}
