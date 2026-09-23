package memory

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	lineLive   = "live"
	lineFading = "fading"
	lineClosed = "closed"

	// turnFade is how much an untouched line thins each judged turn.
	// A fresh 0.62 line takes a long chat to fall out of view, and a
	// mention puts the weight back. Days since Touched thin the cue
	// further without deleting the line.
	turnFade = 0.012

	// recallMin is the overlap a line needs with the utterance to be
	// recalled on that turn.
	recallMin = 0.2
)

// Line is one durable memory. Text is a gist of what changed, not a
// clipped transcript. Weight rises when the subject comes back, falls
// when it doesn't, and a resolution drops the line.
type Line struct {
	Text    string    `json:"text"`
	Weight  float64   `json:"weight"`
	Touched time.Time `json:"touched,omitempty"`
	Status  string    `json:"status,omitempty"`
}

// Lines accepts the older on-disk shape (a JSON array of strings) and
// the current array of objects.
type Lines []Line

func (l *Lines) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*l = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(Lines, 0, len(raw))
	for _, item := range raw {
		item = bytes.TrimSpace(item)
		if len(item) == 0 {
			continue
		}
		if item[0] == '"' {
			var s string
			if err := json.Unmarshal(item, &s); err != nil {
				return err
			}
			out = append(out, migrateLine(Line{Text: s, Weight: 0.55, Status: lineLive}))
			continue
		}
		var n Line
		if err := json.Unmarshal(item, &n); err != nil {
			return err
		}
		out = append(out, migrateLine(n))
	}
	*l = out
	return nil
}

func migrateLine(ln Line) Line {
	for _, p := range []string{"小春: ", "明日香: ", "她: "} {
		if !strings.HasPrefix(ln.Text, p) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln.Text, p))
		if !loopCueRe.MatchString(rest) && !promiseCueRe.MatchString(rest) && !jokeCueRe.MatchString(rest) {
			ln.Text = ""
			ln.Status = lineClosed
			return ln
		}
		ln.Text = gistOr(rest)
		return ln
	}
	if strings.HasPrefix(ln.Text, "user: ") {
		ln.Text = gistOr(strings.TrimSpace(strings.TrimPrefix(ln.Text, "user: ")))
		if ln.Weight <= 0 || ln.Weight == 0.55 {
			ln.Weight = 0.4
		}
	}
	return ln
}

func gistOr(s string) string {
	if g := gist(s); g != "" {
		return g
	}
	return clipRunes(strings.TrimSpace(s), 36)
}

func normalizeLines(list Lines, capN int) Lines {
	seen := map[string]int{}
	out := make(Lines, 0, len(list))
	for _, ln := range list {
		ln = migrateLine(ln)
		ln.Text = strings.Join(strings.Fields(strings.TrimSpace(ln.Text)), " ")
		if ln.Text == "" || ln.Status == lineClosed {
			continue
		}
		if ln.Weight <= 0 {
			ln.Weight = 0.55
		}
		ln.Weight = clamp01(ln.Weight)
		if ln.Weight < 0.08 {
			continue
		}
		if ln.Status == "" {
			ln.Status = lineLive
		}
		if i, ok := seen[ln.Text]; ok {
			if ln.Weight > out[i].Weight {
				out[i].Weight = ln.Weight
			}
			if ln.Touched.After(out[i].Touched) {
				out[i].Touched = ln.Touched
			}
			if ln.Status == lineLive {
				out[i].Status = lineLive
			}
			continue
		}
		seen[ln.Text] = len(out)
		out = append(out, ln)
	}
	for capN > 0 && len(out) > capN {
		worst := 0
		for i := 1; i < len(out); i++ {
			if out[i].Weight < out[worst].Weight {
				worst = i
			}
		}
		out = append(out[:worst], out[worst+1:]...)
	}
	return out
}

func copyLines(list Lines) Lines {
	return append(Lines(nil), list...)
}

var (
	loopCueRe    = regexp.MustCompile(`(还没|别忘|忘了|待办|下次|回头|晚点|截止|ddl|todo|记得)`)
	promiseCueRe = regexp.MustCompile(`(答应|保证)`)
	jokeCueRe    = regexp.MustCompile(`(笑死|哈哈|梗|外号|绰号|笨蛋)`)
	doneRe       = regexp.MustCompile(`(做完|交了|交完|不用了|不用记|算了|搞定|解决了|忘了吧|别记了|已经交|写完了)`)
)

// settle thins every line, strengthens ones this utterance is about,
// and drops a loop or promise the user just finished.
func settle(r *Relationship, userText string, now time.Time) {
	r.OpenLoops = settleList(r.OpenLoops, userText, true, now)
	r.Promises = settleList(r.Promises, userText, true, now)
	r.SharedEvents = settleList(r.SharedEvents, userText, false, now)
	r.InsideJokes = settleList(r.InsideJokes, userText, false, now)
}

func settleList(list Lines, userText string, allowClose bool, now time.Time) Lines {
	out := make(Lines, 0, len(list))
	for _, ln := range list {
		if ln.Status == lineClosed || strings.TrimSpace(ln.Text) == "" {
			continue
		}
		ln.Weight = clamp01(ln.Weight - turnFade)
		if allowClose && resolves(userText) && about(userText, ln.Text) {
			continue
		}
		if strings.TrimSpace(userText) != "" && about(userText, ln.Text) {
			ln.Weight = clamp01(ln.Weight + 0.16)
			ln.Touched = now
			ln.Status = lineLive
		} else if ln.Weight < 0.28 {
			ln.Status = lineFading
		} else if ln.Status != lineFading {
			ln.Status = lineLive
		}
		if ln.Weight < 0.08 {
			continue
		}
		out = append(out, ln)
	}
	return out
}

func resolves(text string) bool {
	return doneRe.MatchString(text)
}

func about(query, text string) bool {
	if strings.TrimSpace(query) == "" || strings.TrimSpace(text) == "" {
		return false
	}
	return contentOverlap(query, text) >= 2 || bigramDice(query, text) >= 0.34
}

// seed writes a local gist when the utterance itself is a promise,
// an open loop, a joke, or a moment that changed the mood. Ordinary
// chat waits for the slow fold, so the ledger does not fill with quotes.
func seed(r *Relationship, userText, mode string, now time.Time) {
	g := gist(userText)
	if g == "" {
		return
	}
	switch {
	case promiseCueRe.MatchString(userText):
		r.Promises = rememberLine(r.Promises, g, now, 8)
	case loopCueRe.MatchString(userText):
		r.OpenLoops = rememberLine(r.OpenLoops, g, now, 8)
	case jokeCueRe.MatchString(userText):
		r.InsideJokes = rememberLine(r.InsideJokes, g, now, 6)
	}
	switch mode {
	case "celebrate":
		r.SharedEvents = rememberLine(r.SharedEvents, "一起高兴："+g, now, 8)
	case "comfort":
		r.SharedEvents = rememberLine(r.SharedEvents, "一起扛过："+g, now, 8)
	case "de_escalate":
		r.SharedEvents = rememberLine(r.SharedEvents, "有点别扭："+g, now, 8)
	}
}

func rememberLine(list Lines, text string, now time.Time, capN int) Lines {
	text = clipRunes(strings.TrimSpace(text), 40)
	if text == "" {
		return list
	}
	for i, ln := range list {
		if ln.Text == text || bigramDice(ln.Text, text) >= 0.62 {
			ln.Weight = clamp01(ln.Weight + 0.12)
			ln.Touched = now
			ln.Status = lineLive
			if len([]rune(text)) < len([]rune(ln.Text)) {
				ln.Text = text
			}
			list[i] = ln
			return list
		}
	}
	list = append(list, Line{Text: text, Weight: 0.62, Touched: now, Status: lineLive})
	return normalizeLines(list, capN)
}

func gist(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || backchannel(text) {
		return ""
	}
	parts := splitClauses(text)
	var keep []string
	for _, p := range parts {
		p = stripLead(strings.TrimSpace(p))
		if p == "" || cueOnly(p) {
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return ""
	}
	return clipRunes(strings.Join(keep, "，"), 36)
}

func topicOf(text string) string {
	g := gist(text)
	if g == "" {
		return ""
	}
	return clipRunes(g, 24)
}

func splitClauses(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '，', '。', '！', '？', '、', '；', ',', '.', '!', '?', ';', '\n':
			return true
		}
		return false
	})
}

func stripLead(s string) string {
	leads := []string{"就是", "那个", "然后", "我觉得", "我想", "哈哈哈", "哈哈", "笑死", "我还", "我要", "我"}
	for {
		trimmed := false
		for _, lead := range leads {
			if strings.HasPrefix(s, lead) && len([]rune(s)) > len([]rune(lead))+1 {
				s = strings.TrimSpace(strings.TrimPrefix(s, lead))
				trimmed = true
				break
			}
		}
		if !trimmed {
			return s
		}
	}
}

func cueOnly(s string) bool {
	switch strings.TrimSpace(s) {
	case "别忘了", "记得", "忘了", "哈哈", "哈哈哈", "笑死", "好", "好的", "嗯", "嗯嗯", "哦", "啊", "吧", "呢", "真是的", "草", "在", "喂", "嗨", "你好":
		return true
	default:
		return false
	}
}

func backchannel(s string) bool {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 4 {
		return false
	}
	return cueOnly(s)
}

func contentOverlap(a, b string) int {
	ga, gb := contentRunes(a), contentRunes(b)
	n := 0
	for r := range ga {
		if gb[r] {
			n++
		}
	}
	return n
}

func contentRunes(s string) map[rune]bool {
	skip := map[rune]bool{
		'了': true, '的': true, '我': true, '你': true, '是': true, '在': true,
		'也': true, '就': true, '还': true, '要': true, '不': true, '吗': true,
		'吧': true, '呢': true, '啊': true, '这': true, '那': true, '个': true,
	}
	m := map[rune]bool{}
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) || skip[r] || r < 0x80 {
			continue
		}
		m[r] = true
	}
	return m
}

func bigramDice(a, b string) float64 {
	ga, gb := grams(a), grams(b)
	if len(ga) == 0 || len(gb) == 0 {
		return 0
	}
	na, nb, inter := 0, 0, 0
	for _, c := range ga {
		na += c
	}
	for k, c := range gb {
		nb += c
		if ga[k] > 0 {
			n := ga[k]
			if c < n {
				n = c
			}
			inter += n
		}
	}
	if na+nb == 0 {
		return 0
	}
	return 2 * float64(inter) / float64(na+nb)
}

func grams(s string) map[string]int {
	var rs []rune
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			continue
		}
		rs = append(rs, r)
	}
	m := map[string]int{}
	if len(rs) == 0 {
		return m
	}
	if len(rs) == 1 {
		m[string(rs)] = 1
		return m
	}
	for i := 0; i < len(rs)-1; i++ {
		m[string(rs[i:i+2])]++
	}
	return m
}

func effectiveWeight(ln Line, now time.Time) float64 {
	w := ln.Weight
	if ln.Touched.IsZero() || now.IsZero() {
		return w
	}
	days := now.Sub(ln.Touched).Hours() / 24
	if days > 3 {
		w -= 0.015 * (days - 3)
	}
	if w < 0 {
		return 0
	}
	return w
}

func pick(list Lines, query string, now time.Time, n int) []string {
	type scored struct {
		text string
		s    float64
		i    int
	}
	query = strings.TrimSpace(query)
	var xs []scored
	for i, ln := range list {
		if ln.Status == lineClosed || strings.TrimSpace(ln.Text) == "" {
			continue
		}
		ov := 0.0
		if query != "" {
			ov = bigramDice(query, ln.Text)
			if contentOverlap(query, ln.Text) >= 2 && ov < 1 {
				ov += 0.45
				if ov > 1 {
					ov = 1
				}
			}
		}
		if ln.Status == lineFading && query != "" && ov < 0.4 && contentOverlap(query, ln.Text) < 2 {
			continue
		}
		// A strong but unrelated line would ride along every turn and she
		// would keep bringing up the same thing.
		if query != "" && ov < recallMin {
			continue
		}
		if ln.Status == lineFading && query == "" {
			continue
		}
		w := effectiveWeight(ln, now)
		rel := 0.45
		if query != "" {
			rel = 0.15 + 0.85*ov
		}
		sc := w * rel
		if sc < 0.08 {
			continue
		}
		xs = append(xs, scored{ln.Text, sc, i})
	}
	sort.SliceStable(xs, func(i, j int) bool {
		if xs[i].s == xs[j].s {
			return xs[i].i > xs[j].i
		}
		return xs[i].s > xs[j].s
	})
	if n > 0 && len(xs) > n {
		xs = xs[:n]
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = x.text
	}
	return out
}

func quotesUser(text, user string) bool {
	user = strings.TrimSpace(user)
	text = strings.TrimSpace(text)
	if text == "" || user == "" {
		return false
	}
	ct, cu := compact(text), compact(user)
	if ct == cu {
		return true
	}
	ut, uu := len([]rune(ct)), len([]rune(cu))
	if uu > 0 && strings.Contains(cu, ct) && float64(ut)/float64(uu) >= 0.75 {
		return true
	}
	return bigramDice(text, user) >= 0.8 && ut >= 8
}

func compact(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
