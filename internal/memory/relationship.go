// relationship.go is the persistent relationship ledger behind the voice.
// It records the few things that make the character feel continuous:
// shared events, promises, open loops, inside jokes, and a coarse bond.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	relationshipStageCold     = "陌生"
	relationshipStageFamiliar = "熟悉"
	relationshipStageTrusted  = "信任"
	relationshipStageBonded   = "亲密"
)

// Relationship is a compact, durable summary of "who are we to each other now".
type Relationship struct {
	Stage        string    `json:"stage"`
	Bond         float64   `json:"bond"`
	Trust        float64   `json:"trust"`
	Warmth       float64   `json:"warmth"`
	Tension      float64   `json:"tension"`
	OpenLoops    Lines     `json:"open_loops,omitempty"`
	SharedEvents Lines     `json:"shared_events,omitempty"`
	Promises     Lines     `json:"promises,omitempty"`
	InsideJokes  Lines     `json:"inside_jokes,omitempty"`
	LastEvent    string    `json:"last_event,omitempty"`
	LastTopic    string    `json:"last_topic,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RelationshipCue is the one-line scene state sent to the voice layer.
type RelationshipCue struct {
	Stage        string   `json:"stage"`
	Summary      string   `json:"summary"`
	OpenLoops    []string `json:"open_loops,omitempty"`
	SharedEvents []string `json:"shared_events,omitempty"`
}

// RelationshipStore keeps relationship memory on disk so she survives reboots.
type RelationshipStore struct {
	mu      sync.Mutex
	path    string
	rel     Relationship
	folding bool
}

// NewRelationshipStore opens (or creates) a durable relationship file.
func NewRelationshipStore(path string) (*RelationshipStore, error) {
	s := &RelationshipStore{path: path}
	if strings.TrimSpace(path) == "" {
		s.rel = defaultRelationship()
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.rel = defaultRelationship()
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.rel); err != nil {
		return nil, fmt.Errorf("parse relationship %s: %w", path, err)
	}
	s.rel = normalizeRelationship(s.rel)
	return s, nil
}

func defaultRelationship() Relationship {
	return Relationship{
		Stage:  relationshipStageCold,
		Bond:   0.08,
		Trust:  0.08,
		Warmth: 0.1,
	}
}

func normalizeRelationship(r Relationship) Relationship {
	if strings.TrimSpace(r.Stage) == "" {
		r.Stage = relationshipStageCold
	}
	if r.Bond <= 0 {
		r.Bond = 0.08
	}
	if r.Trust <= 0 {
		r.Trust = 0.08
	}
	if r.Warmth <= 0 {
		r.Warmth = 0.1
	}
	r.Bond = clamp01(r.Bond)
	r.Trust = clamp01(r.Trust)
	r.Warmth = clamp01(r.Warmth)
	r.Tension = clamp01(r.Tension)
	r.OpenLoops = normalizeLines(r.OpenLoops, 8)
	r.SharedEvents = normalizeLines(r.SharedEvents, 8)
	r.Promises = normalizeLines(r.Promises, 8)
	r.InsideJokes = normalizeLines(r.InsideJokes, 6)
	return r
}

// Snapshot returns the current durable relationship state.
func (s *RelationshipStore) Snapshot() Relationship {
	if s == nil {
		return defaultRelationship()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRelationship(s.rel)
}

// Cue returns the strongest live lines, for the face and for logs.
func (s *RelationshipStore) Cue() RelationshipCue {
	return s.Recall("")
}

// Recall picks the few lines that belong to this utterance. Overlap is
// character bigrams plus shared content words. The ledger is a few dozen
// gists, so a vector index would retrieve the same raw text more slowly.
func (s *RelationshipStore) Recall(query string) RelationshipCue {
	r := s.Snapshot()
	now := time.Now()
	return RelationshipCue{
		Stage:        r.Stage,
		Summary:      summarizeRelationship(r, query, now),
		OpenLoops:    pick(r.OpenLoops, query, now, 2),
		SharedEvents: pick(r.SharedEvents, query, now, 2),
	}
}

// ForJudge is a short list of live gists for the keep question.
func (s *RelationshipStore) ForJudge() []string {
	if s == nil {
		return nil
	}
	r := s.Snapshot()
	now := time.Now()
	type item struct {
		text string
		w    float64
	}
	var all []item
	for _, list := range []Lines{r.OpenLoops, r.Promises, r.SharedEvents, r.InsideJokes} {
		for _, ln := range list {
			if ln.Status == lineClosed || strings.TrimSpace(ln.Text) == "" {
				continue
			}
			all = append(all, item{ln.Text, effectiveWeight(ln, now)})
		}
	}
	for i := 1; i < len(all); i++ {
		j := i
		for j > 0 && all[j].w > all[j-1].w {
			all[j], all[j-1] = all[j-1], all[j]
			j--
		}
	}
	if len(all) > 8 {
		all = all[:8]
	}
	out := make([]string, len(all))
	for i, it := range all {
		out[i] = it.text
	}
	return out
}

// ObserveTurn folds a judged turn into the persistent relationship.
func (s *RelationshipStore) ObserveTurn(personaName, userText, assistantText string, userEmotion, selfEmotion, mode string,
	userValence, userArousal, engagement, personaFit float64) (Relationship, error) {
	if s == nil {
		return defaultRelationship(), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r := normalizeRelationship(s.rel)
	now := time.Now()
	r.UpdatedAt = now

	// Relationship grows slowly; big jumps feel fake and robotic.
	delta := 0.0
	switch mode {
	case "celebrate":
		delta += 0.03
	case "comfort":
		delta += 0.02
	case "goal_push":
		delta += 0.012
	case "re_engage":
		delta += 0.008
	}
	if userValence > 0.6 {
		delta += 0.01
	}
	if engagement > 0.65 {
		delta += 0.01
	}
	if personaFit > 0.6 {
		delta += 0.008
	}
	if userValence < 0.35 && mode != "safety" {
		r.Tension = clamp01(r.Tension + 0.02)
	}
	if mode == "safety" {
		r.Tension = clamp01(r.Tension + 0.04)
	}

	r.Bond = clamp01(r.Bond + delta)
	r.Trust = clamp01(r.Trust + delta*0.8)
	r.Warmth = clamp01(r.Warmth + delta*0.6)
	if userValence < 0.3 {
		r.Warmth = clamp01(r.Warmth - 0.01)
	}

	if mode == "de_escalate" || mode == "safety" {
		r.Tension = clamp01(r.Tension + 0.025)
	} else {
		r.Tension = clamp01(r.Tension - 0.015)
	}

	// Numbers move every turn. Text is a gist, and only when the
	// utterance is actually a promise, a loop, a joke, or a mood shift.
	// Safety does not write a quote of the distress into the ledger.
	settle(&r, userText, now)
	if mode != "safety" {
		seed(&r, userText, mode, now)
	}
	if topic := topicOf(userText); topic != "" {
		r.LastTopic = topic
	}

	r.Stage = stageForBond(r.Bond, r.Trust, r.Warmth)
	r.LastEvent = lastNonEmpty(modeLabel(mode), r.LastEvent)
	s.rel = normalizeRelationship(r)
	return cloneRelationship(s.rel), s.saveLocked()
}

func (s *RelationshipStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	tmp := s.path + ".tmp"
	raw, err := json.MarshalIndent(s.rel, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func cloneRelationship(r Relationship) Relationship {
	out := r
	out.OpenLoops = copyLines(r.OpenLoops)
	out.SharedEvents = copyLines(r.SharedEvents)
	out.Promises = copyLines(r.Promises)
	out.InsideJokes = copyLines(r.InsideJokes)
	return out
}

func summarizeRelationship(r Relationship, query string, now time.Time) string {
	var parts []string
	if xs := pick(r.OpenLoops, query, now, 2); len(xs) > 0 {
		parts = append(parts, "还记着: "+strings.Join(xs, " / "))
	}
	if xs := pick(r.SharedEvents, query, now, 2); len(xs) > 0 {
		parts = append(parts, "一起经历过: "+strings.Join(xs, " / "))
	}
	if xs := pick(r.InsideJokes, query, now, 1); len(xs) > 0 {
		parts = append(parts, "内部梗: "+strings.Join(xs, " / "))
	}
	if xs := pick(r.Promises, query, now, 1); len(xs) > 0 {
		parts = append(parts, "答应过: "+strings.Join(xs, " / "))
	}
	if t := strings.TrimSpace(r.LastTopic); t != "" {
		parts = append(parts, "上次说到: "+clipRunes(t, 24))
	}
	if r.Tension >= 0.45 {
		parts = append(parts, "你们之间还有一点没消的别扭")
	}
	if len(parts) == 0 {
		return "现在还在彼此试探，像刚认识的同桌"
	}
	return strings.Join(parts, "；")
}

func modeLabel(mode string) string {
	switch mode {
	case "safety":
		return "小心护住"
	case "comfort":
		return "嘴硬安慰"
	case "de_escalate":
		return "收住火气"
	case "celebrate":
		return "一起高兴"
	case "re_engage":
		return "把话接回来"
	case "goal_push":
		return "把事往前推"
	default:
		return "继续聊"
	}
}

func stageForBond(bond, trust, warmth float64) string {
	switch {
	case bond >= 0.72 && trust >= 0.68:
		return relationshipStageBonded
	case bond >= 0.45 && trust >= 0.42:
		return relationshipStageTrusted
	case bond >= 0.22 || warmth >= 0.28:
		return relationshipStageFamiliar
	default:
		return relationshipStageCold
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func lastNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
