// Package oracle is the six-line (京房纳甲) caster.
// Coins, the plate, and the calendar are code. The reading is a separate LLM.
//
// Hexagram names, 彖, 象, and the gloss on each line come from
// github.com/godcong/yi (MIT). Month, day, and 旬空 come from
// github.com/6tail/lunar-go. 世应 uses the classical three-line gap;
// yi's table disagrees on 本宫, 游魂, and 归魂.
package oracle

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"strings"
	"time"

	"github.com/6tail/lunar-go/calendar"
	"github.com/godcong/yi"
)

// Pillars is the calendar the plate hangs on. Month is the solar-term branch.
type Pillars struct {
	MonthZhi string
	DayGan   string
	DayZhi   string
	Void     string
}

// Line is one yao, bottom (1) to top (6).
type Line struct {
	Pos      int
	Value    int
	Stem     string
	Branch   string
	Element  string
	Relation string
	Spirit   string
	Role     string
	Marks    string
	Changed  string
	Gloss    string
}

// Plate is one cast. Prompt is what the reader model sees.
type Plate struct {
	Primary  string
	Changed  string
	Palace   string
	Element  string
	Position string
	Pillars  Pillars
	Lines    [6]Line
	Hidden   []string
	Judgment string
	Image    string
	prompt   string
}

// Glance is the one line the branch keeps, and the voice may name.
func (p Plate) Glance() string {
	name := p.Primary
	if p.Changed != "" {
		name += "之" + p.Changed
	}
	return fmt.Sprintf("%s · %s %s · %s月%s%s日",
		name, p.Palace, p.Position, p.Pillars.MonthZhi, p.Pillars.DayGan, p.Pillars.DayZhi)
}

// Prompt is the plate text. It does not include an omen.
func (p Plate) Prompt() string { return p.prompt }

// Cast tosses three coins six times, bottom line first.
// seed 0 draws a fresh seed; any other seed repeats.
func Cast(when time.Time, seed int64) (Plate, error) {
	if when.IsZero() {
		when = time.Now()
	}
	return Build(toss(seed), pillarsOf(when))
}

// Build assembles a plate from classical line values 6, 7, 8, 9.
func Build(lines [6]int, when Pillars) (Plate, error) {
	for _, v := range lines {
		if v < 6 || v > 9 {
			return Plate{}, fmt.Errorf("line value %d is not 6, 7, 8, or 9", v)
		}
	}
	shang, xia := trigrams(lines)
	g, err := yi.GetGuaByIndex(baguaName[shang] + baguaName[xia])
	if err != nil || g == nil {
		return Plate{}, fmt.Errorf("no hexagram for %s/%s", baguaName[shang], baguaName[xia])
	}
	gong := yi.GetGuaGong(g)
	if gong < 0 || int(gong) >= len(baguaName) {
		return Plate{}, fmt.Errorf("no palace for %s", g.Ming)
	}
	pos := int(yi.GetGuaPosition(g))
	if pos < 0 || pos >= len(shiYing) {
		return Plate{}, fmt.Errorf("no generation for %s", g.Ming)
	}
	palaceWX := yi.GetWuXingByBagua(gong).String()
	changedLines := lines
	var moving []int
	for i, v := range lines {
		if v == 6 || v == 9 {
			moving = append(moving, i)
			changedLines[i] = changedValue(v)
		}
	}
	var changed *yi.Gua
	if len(moving) > 0 {
		cs, cx := trigrams(changedLines)
		changed, _ = yi.GetGuaByIndex(baguaName[cs] + baguaName[cx])
	}

	var plate Plate
	plate.Primary = shortName(g)
	plate.Palace = baguaName[gong] + "宫"
	plate.Element = palaceWX
	plate.Position = yi.GetGuaPosition(g).String()
	plate.Pillars = when
	if changed != nil && changed.Index != g.Index {
		plate.Changed = shortName(changed)
	}
	plate.Judgment = clipRunes(g.TuanText, 180)
	plate.Image = clipRunes(g.XiangText, 120)

	shi, ying := shiYing[pos][0], shiYing[pos][1]
	monthEl := zhiElement[when.MonthZhi]
	for i := 0; i < 6; i++ {
		stem, branch := najiaAt(shang, xia, i)
		el := zhiElement[branch]
		line := Line{
			Pos:      i + 1,
			Value:    lines[i],
			Stem:     stem,
			Branch:   branch,
			Element:  el,
			Relation: relation(palaceWX, el),
			Spirit:   spiritAt(when.DayGan, i),
		}
		if i == shi {
			line.Role = "世"
		} else if i == ying {
			line.Role = "应"
		}
		var marks []string
		if lines[i] == 6 || lines[i] == 9 {
			marks = append(marks, "动")
		}
		if s := strength(monthEl, el); s != "" {
			marks = append(marks, s)
		}
		if branch != "" && branch == when.MonthZhi {
			marks = append(marks, "临月")
		}
		if branch != "" && branch == when.DayZhi {
			marks = append(marks, "临日")
		}
		if clash(branch, when.MonthZhi) {
			marks = append(marks, "月破")
		}
		if clash(branch, when.DayZhi) && branch != when.DayZhi {
			marks = append(marks, "日冲")
		}
		if branch != "" && strings.Contains(when.Void, branch) {
			marks = append(marks, "旬空")
		}
		line.Marks = strings.Join(marks, " ")
		if (lines[i] == 6 || lines[i] == 9) && changed != nil {
			cs, cx := trigrams(changedLines)
			cStem, cBranch := najiaAt(cs, cx, i)
			cEl := zhiElement[cBranch]
			cRel := relation(palaceWX, cEl)
			line.Changed = fmt.Sprintf("化%s%s%s %s%s", cStem, cBranch, cEl, cRel, changeMark(branch, el, cBranch, cEl))
			if g.Yaos[i] != nil {
				line.Gloss = clipRunes(g.Yaos[i].Ci, 80)
			}
		}
		plate.Lines[i] = line
	}
	plate.Hidden = hidden(gong, palaceWX, plate.Lines)
	plate.prompt = formatPlate(g, changed, plate)
	return plate, nil
}

func formatPlate(g, changed *yi.Gua, p Plate) string {
	var b strings.Builder
	b.WriteString("铜钱六爻，京房纳甲。初爻先摇，下面从上爻写到初爻。\n")
	fmt.Fprintf(&b, "月建 %s（%s）  日辰 %s%s  旬空 %s\n",
		p.Pillars.MonthZhi, zhiElement[p.Pillars.MonthZhi],
		p.Pillars.DayGan, p.Pillars.DayZhi, or(p.Pillars.Void, "无"))
	fmt.Fprintf(&b, "本卦 %s %s  %s %s  宫五行 %s\n", g.Ming, g.GuaSymbol, p.Palace, p.Position, p.Element)
	if p.Judgment != "" {
		fmt.Fprintf(&b, "彖 %s\n", p.Judgment)
	}
	if p.Image != "" {
		fmt.Fprintf(&b, "象 %s\n", p.Image)
	}
	if changed != nil && p.Changed != "" {
		fmt.Fprintf(&b, "变卦 %s %s\n", changed.Ming, changed.GuaSymbol)
	} else {
		b.WriteString("变卦 无（静卦，以世爻与日辰为主）\n")
	}
	if len(p.Hidden) == 0 {
		b.WriteString("伏神 无\n")
	} else {
		fmt.Fprintf(&b, "伏神 %s\n", strings.Join(p.Hidden, "；"))
	}
	labels := []string{"初爻", "二爻", "三爻", "四爻", "五爻", "上爻"}
	for i := 5; i >= 0; i-- {
		ln := p.Lines[i]
		fmt.Fprintf(&b, "%s %s %s%s%s %s %s",
			labels[i], valueName(ln.Value), ln.Relation, ln.Stem, ln.Branch, ln.Element, ln.Spirit)
		if ln.Role != "" {
			fmt.Fprintf(&b, " %s", ln.Role)
		}
		if ln.Marks != "" {
			fmt.Fprintf(&b, " %s", ln.Marks)
		}
		if ln.Changed != "" {
			fmt.Fprintf(&b, " %s", ln.Changed)
		}
		b.WriteByte('\n')
		if ln.Gloss != "" {
			fmt.Fprintf(&b, "  备考 %s\n", ln.Gloss)
		}
	}
	return b.String()
}

func hidden(gong yi.Bagua, palaceWX string, lines [6]Line) []string {
	have := map[string]bool{}
	for _, ln := range lines {
		if ln.Relation != "" {
			have[ln.Relation] = true
		}
	}
	pure, err := yi.GetGuaByIndex(baguaName[gong] + baguaName[gong])
	if err != nil || pure == nil {
		return nil
	}
	labels := []string{"初爻", "二爻", "三爻", "四爻", "五爻", "上爻"}
	var out []string
	for _, rel := range []string{"父母", "兄弟", "妻财", "子孙", "官鬼"} {
		if have[rel] {
			continue
		}
		for i := 0; i < 6; i++ {
			stem, branch := najiaAt(int(gong), int(gong), i)
			if relation(palaceWX, zhiElement[branch]) != rel {
				continue
			}
			out = append(out, fmt.Sprintf("%s %s%s%s 伏在本宫%s", rel, stem, branch, zhiElement[branch], labels[i]))
			break
		}
	}
	return out
}

func pillarsOf(t time.Time) Pillars {
	solar := calendar.NewSolar(t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	lunar := solar.GetLunar()
	ec := lunar.GetEightChar()
	return Pillars{
		MonthZhi: ec.GetMonthZhi(),
		DayGan:   ec.GetDayGan(),
		DayZhi:   ec.GetDayZhi(),
		Void:     lunar.GetDayXunKongExact(),
	}
}

func toss(seed int64) [6]int {
	if seed == 0 {
		var buf [8]byte
		if _, err := crand.Read(buf[:]); err == nil {
			seed = int64(binary.LittleEndian.Uint64(buf[:]))
		} else {
			seed = time.Now().UnixNano()
		}
	}
	rng := mrand.New(mrand.NewSource(seed))
	var lines [6]int
	for i := 0; i < 6; i++ {
		sum := 0
		for c := 0; c < 3; c++ {
			if rng.Intn(2) == 0 {
				sum += 2
			} else {
				sum += 3
			}
		}
		lines[i] = sum
	}
	return lines
}

func trigrams(lines [6]int) (shang, xia int) {
	xia = trigramNum(lines[0], lines[1], lines[2])
	shang = trigramNum(lines[3], lines[4], lines[5])
	return shang, xia
}

// trigramNum packs bottom, mid, top. Yin is 1. Bit 0 is the top line, matching yi.
func trigramNum(bottom, mid, top int) int {
	n := 0
	if yin(top) {
		n |= 1
	}
	if yin(mid) {
		n |= 2
	}
	if yin(bottom) {
		n |= 4
	}
	return n
}

func yin(v int) bool { return v == 6 || v == 8 }

func changedValue(v int) int {
	switch v {
	case 6:
		return 7
	case 9:
		return 8
	default:
		return v
	}
}

func najiaAt(shang, xia, idx int) (stem, branch string) {
	name := baguaName[xia]
	slot := idx
	side := najiaTable[name].inner
	if idx >= 3 {
		name = baguaName[shang]
		slot = idx - 3
		side = najiaTable[name].outer
	}
	return side[slot][0], side[slot][1]
}

func spiritAt(gan string, idx int) string {
	start, ok := spiritStart[gan]
	if !ok {
		return ""
	}
	return spirits[(start+idx)%6]
}

func relation(palace, line string) string {
	switch {
	case palace == "" || line == "":
		return ""
	case palace == line:
		return "兄弟"
	case generates(line, palace):
		return "父母"
	case generates(palace, line):
		return "子孙"
	case overcomes(palace, line):
		return "妻财"
	case overcomes(line, palace):
		return "官鬼"
	default:
		return ""
	}
}

func generates(a, b string) bool {
	return map[string]string{"木": "火", "火": "土", "土": "金", "金": "水", "水": "木"}[a] == b
}

func overcomes(a, b string) bool {
	return map[string]string{"木": "土", "土": "水", "水": "火", "火": "金", "金": "木"}[a] == b
}

func strength(monthEl, lineEl string) string {
	switch {
	case monthEl == "" || lineEl == "":
		return ""
	case monthEl == lineEl:
		return "旺"
	case generates(monthEl, lineEl):
		return "相"
	case generates(lineEl, monthEl):
		return "休"
	case overcomes(monthEl, lineEl):
		return "囚"
	case overcomes(lineEl, monthEl):
		return "死"
	default:
		return ""
	}
}

func clash(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	return map[string]string{
		"子": "午", "午": "子", "丑": "未", "未": "丑",
		"寅": "申", "申": "寅", "卯": "酉", "酉": "卯",
		"辰": "戌", "戌": "辰", "巳": "亥", "亥": "巳",
	}[a] == b
}

func changeMark(from, fromEl, to, toEl string) string {
	if from == "" || to == "" {
		return ""
	}
	switch {
	case from == to:
		return " 伏吟"
	case clash(from, to):
		return " 反吟"
	case forward(from, to):
		return " 化进"
	case forward(to, from):
		return " 化退"
	case generates(toEl, fromEl):
		return " 回头生"
	case overcomes(toEl, fromEl):
		return " 回头克"
	default:
		return ""
	}
}

func forward(from, to string) bool {
	return map[string]string{"寅": "卯", "巳": "午", "申": "酉", "亥": "子"}[from] == to
}

func valueName(v int) string {
	switch v {
	case 6:
		return "老阴"
	case 7:
		return "少阳"
	case 8:
		return "少阴"
	case 9:
		return "老阳"
	default:
		return ""
	}
}

func shortName(g *yi.Gua) string {
	if g == nil {
		return ""
	}
	if g.GuaName != "" {
		return g.GuaName
	}
	return g.Ming
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

var baguaName = []string{"乾", "兑", "离", "震", "巽", "坎", "艮", "坤"}

// shiYing is 世, 应 as indexes 0=初爻. 本宫世上应三, 游魂世四应初, 归魂世三应上.
var shiYing = [8][2]int{
	{5, 2},
	{0, 3},
	{1, 4},
	{2, 5},
	{3, 0},
	{4, 1},
	{3, 0},
	{2, 5},
}

var spirits = []string{"青龙", "朱雀", "勾陈", "腾蛇", "白虎", "玄武"}

var spiritStart = map[string]int{
	"甲": 0, "乙": 0, "丙": 1, "丁": 1, "戊": 2,
	"己": 3, "庚": 4, "辛": 4, "壬": 5, "癸": 5,
}

var zhiElement = map[string]string{
	"子": "水", "亥": "水", "寅": "木", "卯": "木",
	"巳": "火", "午": "火", "申": "金", "酉": "金",
	"辰": "土", "戌": "土", "丑": "土", "未": "土",
}

type najiaSides struct {
	inner [3][2]string
	outer [3][2]string
}

// najiaTable is 京房纳甲, three lines bottom to top, inner then outer.
var najiaTable = map[string]najiaSides{
	"乾": {inner: [3][2]string{{"甲", "子"}, {"甲", "寅"}, {"甲", "辰"}}, outer: [3][2]string{{"壬", "午"}, {"壬", "申"}, {"壬", "戌"}}},
	"坎": {inner: [3][2]string{{"戊", "寅"}, {"戊", "辰"}, {"戊", "午"}}, outer: [3][2]string{{"戊", "申"}, {"戊", "戌"}, {"戊", "子"}}},
	"艮": {inner: [3][2]string{{"丙", "辰"}, {"丙", "午"}, {"丙", "申"}}, outer: [3][2]string{{"丙", "戌"}, {"丙", "子"}, {"丙", "寅"}}},
	"震": {inner: [3][2]string{{"庚", "子"}, {"庚", "寅"}, {"庚", "辰"}}, outer: [3][2]string{{"庚", "午"}, {"庚", "申"}, {"庚", "戌"}}},
	"巽": {inner: [3][2]string{{"辛", "丑"}, {"辛", "亥"}, {"辛", "酉"}}, outer: [3][2]string{{"辛", "未"}, {"辛", "巳"}, {"辛", "卯"}}},
	"离": {inner: [3][2]string{{"己", "卯"}, {"己", "丑"}, {"己", "亥"}}, outer: [3][2]string{{"己", "酉"}, {"己", "未"}, {"己", "巳"}}},
	"坤": {inner: [3][2]string{{"乙", "未"}, {"乙", "巳"}, {"乙", "卯"}}, outer: [3][2]string{{"癸", "丑"}, {"癸", "亥"}, {"癸", "酉"}}},
	"兑": {inner: [3][2]string{{"丁", "巳"}, {"丁", "卯"}, {"丁", "丑"}}, outer: [3][2]string{{"丁", "亥"}, {"丁", "酉"}, {"丁", "未"}}},
}
