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
	"regexp"
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
	OpenLoops    []string  `json:"open_loops,omitempty"`
	SharedEvents []string  `json:"shared_events,omitempty"`
	Promises     []string  `json:"promises,omitempty"`
	InsideJokes  []string  `json:"inside_jokes,omitempty"`
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
	mu   sync.Mutex
	path string
	rel  Relationship
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
	r.OpenLoops = uniqueTail(r.OpenLoops, 8)
	r.SharedEvents = uniqueTail(r.SharedEvents, 8)
	r.Promises = uniqueTail(r.Promises, 8)
	r.InsideJokes = uniqueTail(r.InsideJokes, 6)
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

// Cue returns the voice-facing scene cue without dumping private notes.
func (s *RelationshipStore) Cue() RelationshipCue {
	r := s.Snapshot()
	return RelationshipCue{
		Stage:        r.Stage,
		Summary:      summarizeRelationship(r),
		OpenLoops:    append([]string(nil), r.OpenLoops...),
		SharedEvents: append([]string(nil), r.SharedEvents...),
	}
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
	r.UpdatedAt = time.Now()
	if t := strings.TrimSpace(userText); t != "" {
		r.LastTopic = t
	}

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

	// Keep a tiny, human-sized memory of what happened together.
	if t := strings.TrimSpace(userText); t != "" {
		r.SharedEvents = appendUnique(r.SharedEvents, "user: "+clipText(t, 48), 8)
	}
	if t := strings.TrimSpace(assistantText); t != "" {
		r.SharedEvents = appendUnique(r.SharedEvents, pLabel(personaName)+": "+clipText(t, 48), 8)
	}
	if mode == "celebrate" && strings.TrimSpace(userText) != "" {
		r.SharedEvents = appendUnique(r.SharedEvents, "一起庆祝: "+clipText(userText, 48), 8)
	}
	if mode == "comfort" && strings.TrimSpace(userText) != "" {
		r.SharedEvents = appendUnique(r.SharedEvents, "一起扛过: "+clipText(userText, 48), 8)
	}
	if mode == "de_escalate" && strings.TrimSpace(userText) != "" {
		r.SharedEvents = appendUnique(r.SharedEvents, "有点别扭: "+clipText(userText, 48), 8)
	}

	for _, line := range extractLoops(userText) {
		r.OpenLoops = appendUnique(r.OpenLoops, line, 8)
	}
	for _, line := range extractPromises(userText) {
		r.Promises = appendUnique(r.Promises, line, 8)
	}
	for _, line := range extractJokes(userText) {
		r.InsideJokes = appendUnique(r.InsideJokes, line, 6)
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
	out.OpenLoops = append([]string(nil), r.OpenLoops...)
	out.SharedEvents = append([]string(nil), r.SharedEvents...)
	out.Promises = append([]string(nil), r.Promises...)
	out.InsideJokes = append([]string(nil), r.InsideJokes...)
	return out
}

func summarizeRelationship(r Relationship) string {
	var parts []string
	if len(r.OpenLoops) > 0 {
		parts = append(parts, "还记着: "+strings.Join(tail(r.OpenLoops, 2), " / "))
	}
	if len(r.SharedEvents) > 0 {
		parts = append(parts, "一起经历过: "+strings.Join(tail(r.SharedEvents, 2), " / "))
	}
	if len(r.InsideJokes) > 0 {
		parts = append(parts, "内部梗: "+strings.Join(tail(r.InsideJokes, 1), " / "))
	}
	if r.Tension >= 0.45 {
		parts = append(parts, "你们之间还有一点没消的别扭")
	}
	if len(parts) == 0 {
		return "现在还在彼此试探，像刚认识的同桌"
	}
	return strings.Join(parts, "；")
}

func pLabel(name string) string {
	switch strings.TrimSpace(name) {
	case "小春":
		return "小春"
	case "明日香":
		return "明日香"
	default:
		return "她"
	}
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

var (
	loopRe = regexp.MustCompile(`(还没|没有|忘了|待办|下次|回头|之后|以后|晚点|明天|周末|截止|ddl|todo|promise|答应|记得)`)
	jokeRe = regexp.MustCompile(`(笑死|哈哈|草|梗|外号|绰号|笨蛋|真是的)`)
)

func extractLoops(text string) []string {
	t := strings.TrimSpace(text)
	if t == "" || len(t) < 2 || !loopRe.MatchString(t) {
		return nil
	}
	return []string{clipText(t, 64)}
}

func extractPromises(text string) []string {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil
	}
	if strings.Contains(t, "答应") || strings.Contains(t, "下次") || strings.Contains(t, "明天") || strings.Contains(t, "保证") {
		return []string{clipText(t, 64)}
	}
	return nil
}

func extractJokes(text string) []string {
	t := strings.TrimSpace(text)
	if t == "" || !jokeRe.MatchString(t) {
		return nil
	}
	return []string{clipText(t, 64)}
}

func appendUnique(list []string, item string, capN int) []string {
	item = strings.TrimSpace(item)
	if item == "" {
		return list
	}
	for _, x := range list {
		if x == item {
			return list
		}
	}
	list = append(list, item)
	return tail(list, capN)
}

func uniqueTail(list []string, capN int) []string {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return tail(out, capN)
}

func tail(list []string, n int) []string {
	if n <= 0 || len(list) <= n {
		return append([]string(nil), list...)
	}
	return append([]string(nil), list[len(list)-n:]...)
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

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
