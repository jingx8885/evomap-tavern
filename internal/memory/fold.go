package memory

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Completer is the slow LLM. It writes gists. It does not speak and it
// does not choose the next action.
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// FoldIn is one judged turn the slow model may compress into memory.
type FoldIn struct {
	Persona   string
	User      string
	Assistant string
	Mode      string
}

const foldSystem = `You keep durable memory for a voice companion. Return a JSON object with one key, notes, an array of at most 2 items.
Each item has op, kind, text, and match.
op is add, revise, or close.
kind is open_loop, promise, shared_event, or inside_joke.
text is one gist in the user's language, at most 40 characters. Say what changed and why it matters to the relationship. Do not quote the utterance. Do not start with a speaker name or "user:".
match is the existing line this op revises or closes, or empty for add.
add: a new fact, preference, promise, unfinished thing, or a shared moment that changed the relationship.
revise: this turn changes an existing line. text is the updated gist.
close: this turn finishes an open loop or a promise.
If nothing should be kept, return {"notes":[]}.
No markdown, no extra keys.`

// BeginFold reports whether a fold may start. One fold runs at a time
// so two turns cannot write the file together.
func (s *RelationshipStore) BeginFold() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folding {
		return false
	}
	s.folding = true
	return true
}

// EndFold releases the fold slot.
func (s *RelationshipStore) EndFold() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.folding = false
	s.mu.Unlock()
}

// Fold asks the slow model for a gist and applies it. A quote of the
// user utterance is refused. An empty result leaves the ledger as the
// local settle left it.
func (s *RelationshipStore) Fold(ctx context.Context, llm Completer, in FoldIn) ([]string, error) {
	if s == nil || llm == nil {
		return nil, nil
	}
	raw, err := llm.ChatComplete(ctx, foldSystem, foldUser(s.Snapshot(), in))
	if err != nil {
		return nil, err
	}
	var body struct {
		Notes []foldNote `json:"notes"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &body); err != nil {
		return nil, err
	}
	if len(body.Notes) > 2 {
		body.Notes = body.Notes[:2]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := applyFold(&s.rel, body.Notes, in.User, time.Now())
	if len(notes) == 0 {
		return nil, nil
	}
	s.rel.UpdatedAt = time.Now()
	s.rel = normalizeRelationship(s.rel)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return notes, nil
}

func foldUser(r Relationship, in FoldIn) string {
	var b strings.Builder
	b.WriteString("mode: " + strings.TrimSpace(in.Mode) + "\n")
	b.WriteString("user:\n" + clipRunes(strings.TrimSpace(in.User), 400) + "\n")
	if t := strings.TrimSpace(in.Assistant); t != "" {
		b.WriteString("assistant:\n" + clipRunes(t, 200) + "\n")
	}
	b.WriteString("already remembered:\n")
	n := 0
	for _, kind := range []string{KindOpenLoop, KindPromise, KindShared, KindJoke} {
		for _, ln := range linesOf(&r, kind) {
			if ln.Status == lineClosed || strings.TrimSpace(ln.Text) == "" {
				continue
			}
			b.WriteString("- " + kind + " " + ln.Text + "\n")
			n++
			if n >= 12 {
				return b.String()
			}
		}
	}
	if n == 0 {
		b.WriteString("(nothing)\n")
	}
	return b.String()
}

type foldNote struct {
	Op    string `json:"op"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Match string `json:"match"`
}

func applyFold(r *Relationship, notes []foldNote, user string, now time.Time) []string {
	var kept []string
	for _, n := range notes {
		op := strings.TrimSpace(n.Op)
		kind := strings.TrimSpace(n.Kind)
		if !knownMemoryKind(kind) || kind == KindTopic {
			continue
		}
		text := clipRunes(strings.TrimSpace(n.Text), 40)
		switch op {
		case "add":
			if text == "" || quotesUser(text, user) {
				continue
			}
			if err := setLines(r, kind, rememberLine(linesOf(r, kind), text, now, memoryCap(kind))); err != nil {
				continue
			}
			kept = append(kept, kind+":"+text)
		case "revise":
			if text == "" || quotesUser(text, user) {
				continue
			}
			if !reviseLine(r, kind, n.Match, text, now) {
				if err := setLines(r, kind, rememberLine(linesOf(r, kind), text, now, memoryCap(kind))); err != nil {
					continue
				}
			}
			kept = append(kept, "revise:"+text)
		case "close":
			if closeLine(r, kind, n.Match, n.Text) {
				kept = append(kept, "close:"+strings.TrimSpace(n.Match))
			}
		}
	}
	return kept
}

func reviseLine(r *Relationship, kind, match, text string, now time.Time) bool {
	list := linesOf(r, kind)
	idx := bestLine(list, match)
	if idx < 0 {
		idx = bestLine(list, text)
	}
	if idx < 0 {
		return false
	}
	list[idx].Text = text
	list[idx].Weight = clamp01(list[idx].Weight + 0.1)
	list[idx].Touched = now
	list[idx].Status = lineLive
	return setLines(r, kind, list) == nil
}

func closeLine(r *Relationship, kind, match, text string) bool {
	list := linesOf(r, kind)
	idx := bestLine(list, match)
	if idx < 0 {
		idx = bestLine(list, text)
	}
	if idx < 0 {
		return false
	}
	list = append(list[:idx], list[idx+1:]...)
	return setLines(r, kind, list) == nil
}

func bestLine(list Lines, query string) int {
	query = strings.TrimSpace(query)
	if query == "" {
		return -1
	}
	best, score := -1, 0.0
	for i, ln := range list {
		if ln.Text == query {
			return i
		}
		s := bigramDice(ln.Text, query)
		if contentOverlap(ln.Text, query) >= 2 && s < 0.7 {
			s = 0.7
		}
		if s >= 0.45 && s > score {
			best, score = i, s
		}
	}
	return best
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}
