package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const memoryTextMax = 80

const (
	KindOpenLoop = "open_loop"
	KindPromise  = "promise"
	KindShared   = "shared_event"
	KindJoke     = "inside_joke"
	KindTopic    = "topic"
)

// MemoryItem is one line she can be shown, edited, or forgotten.
type MemoryItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// MemoryView is the operator-facing snapshot of her durable memory.
type MemoryView struct {
	Available bool         `json:"available"`
	Stage     string       `json:"stage,omitempty"`
	Bond      float64      `json:"bond,omitempty"`
	Trust     float64      `json:"trust,omitempty"`
	Warmth    float64      `json:"warmth,omitempty"`
	Tension   float64      `json:"tension,omitempty"`
	Summary   string       `json:"summary,omitempty"`
	LastEvent string       `json:"last_event,omitempty"`
	UpdatedAt time.Time    `json:"updated_at,omitempty"`
	Items     []MemoryItem `json:"items"`
}

// MemoryOp edits one memory line. Op is add, update, or delete.
type MemoryOp struct {
	Op   string `json:"op"`
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
	Text string `json:"text,omitempty"`
}

var (
	ErrMemoryEmpty     = errors.New("记忆不能是空的")
	ErrMemoryLong      = errors.New("一条记忆最多 80 个字")
	ErrMemoryKind      = errors.New("不认识这类记忆")
	ErrMemoryMissing   = errors.New("找不到这条记忆")
	ErrMemoryFull      = errors.New("这一类记满了，先删一条")
	ErrMemoryDuplicate = errors.New("这条她已经记得")
	ErrMemoryOp        = errors.New("不认识这个操作")
)

// View returns her current durable memories.
func (s *RelationshipStore) View() MemoryView {
	if s == nil {
		return MemoryView{Items: []MemoryItem{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return viewOf(s.rel)
}

// Apply edits one memory line and writes it back to disk.
func (s *RelationshipStore) Apply(op MemoryOp) (MemoryView, error) {
	if s == nil {
		return MemoryView{}, errors.New("记忆还没接上")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rel
	if err := applyMemory(&r, op); err != nil {
		return MemoryView{}, err
	}
	r.UpdatedAt = time.Now()
	s.rel = normalizeRelationship(r)
	if err := s.saveLocked(); err != nil {
		return MemoryView{}, err
	}
	return viewOf(s.rel), nil
}

func viewOf(r Relationship) MemoryView {
	r = normalizeRelationship(r)
	items := make([]MemoryItem, 0)
	if t := strings.TrimSpace(r.LastTopic); t != "" {
		items = append(items, MemoryItem{ID: KindTopic, Kind: KindTopic, Text: t})
	}
	for _, kind := range []string{KindOpenLoop, KindPromise, KindShared, KindJoke} {
		for _, text := range memoryLines(&r, kind) {
			items = append(items, MemoryItem{ID: memoryID(kind, text), Kind: kind, Text: text})
		}
	}
	return MemoryView{
		Available: true,
		Stage:     r.Stage,
		Bond:      r.Bond,
		Trust:     r.Trust,
		Warmth:    r.Warmth,
		Tension:   r.Tension,
		Summary:   summarizeRelationship(r),
		LastEvent: r.LastEvent,
		UpdatedAt: r.UpdatedAt,
		Items:     items,
	}
}

func applyMemory(r *Relationship, op MemoryOp) error {
	switch strings.TrimSpace(op.Op) {
	case "add":
		return addMemory(r, op.Kind, op.Text)
	case "update":
		return updateMemory(r, op.ID, op.Text)
	case "delete":
		return deleteMemory(r, op.ID)
	default:
		return ErrMemoryOp
	}
}

func addMemory(r *Relationship, kind, text string) error {
	text, err := cleanMemoryText(text)
	if err != nil {
		return err
	}
	if kind == KindTopic {
		r.LastTopic = text
		return nil
	}
	list := memoryLines(r, kind)
	if list == nil && !knownMemoryKind(kind) {
		return ErrMemoryKind
	}
	if containsText(list, text) {
		return ErrMemoryDuplicate
	}
	if len(list) >= memoryCap(kind) {
		return ErrMemoryFull
	}
	return setMemoryLines(r, kind, append(append([]string{}, list...), text))
}

func updateMemory(r *Relationship, id, text string) error {
	text, err := cleanMemoryText(text)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == KindTopic {
		if strings.TrimSpace(r.LastTopic) == "" {
			return ErrMemoryMissing
		}
		r.LastTopic = text
		return nil
	}
	kind, idx, ok := findMemory(r, id)
	if !ok {
		return ErrMemoryMissing
	}
	list := append([]string{}, memoryLines(r, kind)...)
	if list[idx] == text {
		return nil
	}
	if containsText(list, text) {
		return ErrMemoryDuplicate
	}
	list[idx] = text
	return setMemoryLines(r, kind, list)
}

func deleteMemory(r *Relationship, id string) error {
	id = strings.TrimSpace(id)
	if id == KindTopic {
		if strings.TrimSpace(r.LastTopic) == "" {
			return ErrMemoryMissing
		}
		r.LastTopic = ""
		return nil
	}
	kind, idx, ok := findMemory(r, id)
	if !ok {
		return ErrMemoryMissing
	}
	list := append([]string{}, memoryLines(r, kind)...)
	list = append(list[:idx], list[idx+1:]...)
	return setMemoryLines(r, kind, list)
}

func findMemory(r *Relationship, id string) (string, int, bool) {
	for _, kind := range []string{KindOpenLoop, KindPromise, KindShared, KindJoke} {
		for i, text := range memoryLines(r, kind) {
			if memoryID(kind, text) == id {
				return kind, i, true
			}
		}
	}
	return "", 0, false
}

func memoryLines(r *Relationship, kind string) []string {
	switch kind {
	case KindOpenLoop:
		return r.OpenLoops
	case KindPromise:
		return r.Promises
	case KindShared:
		return r.SharedEvents
	case KindJoke:
		return r.InsideJokes
	default:
		return nil
	}
}

func setMemoryLines(r *Relationship, kind string, list []string) error {
	switch kind {
	case KindOpenLoop:
		r.OpenLoops = list
	case KindPromise:
		r.Promises = list
	case KindShared:
		r.SharedEvents = list
	case KindJoke:
		r.InsideJokes = list
	default:
		return ErrMemoryKind
	}
	return nil
}

func knownMemoryKind(kind string) bool {
	switch kind {
	case KindOpenLoop, KindPromise, KindShared, KindJoke, KindTopic:
		return true
	default:
		return false
	}
}

func memoryCap(kind string) int {
	switch kind {
	case KindOpenLoop, KindPromise, KindShared:
		return 8
	case KindJoke:
		return 6
	case KindTopic:
		return 1
	default:
		return 0
	}
}

func memoryID(kind, text string) string {
	if kind == KindTopic {
		return KindTopic
	}
	sum := sha256.Sum256([]byte(kind + "\n" + text))
	return kind + ":" + hex.EncodeToString(sum[:8])
}

func cleanMemoryText(s string) (string, error) {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "", ErrMemoryEmpty
	}
	if len([]rune(s)) > memoryTextMax {
		return "", ErrMemoryLong
	}
	return s, nil
}

func containsText(list []string, text string) bool {
	for _, item := range list {
		if item == text {
			return true
		}
	}
	return false
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
