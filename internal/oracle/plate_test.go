package oracle

import (
	"strings"
	"testing"
	"time"
)

func TestLinMovingSecond(t *testing.T) {
	// 地泽临, second line old yang, becomes 地雷复.
	// 未月辛卯日, 旬空午未. 五爻妻财亥水, 世在二, 应在五, 辛日起白虎.
	p, err := Build([6]int{7, 9, 8, 8, 8, 8}, Pillars{
		MonthZhi: "未", DayGan: "辛", DayZhi: "卯", Void: "午未",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Primary != "临" || p.Changed != "复" {
		t.Fatalf("names %s of %s", p.Primary, p.Changed)
	}
	if p.Palace != "坤宫" || p.Position != "二世" || p.Element != "土" {
		t.Fatalf("palace %s %s %s", p.Palace, p.Position, p.Element)
	}
	fifth := p.Lines[4]
	if fifth.Relation != "妻财" || fifth.Branch != "亥" || fifth.Element != "水" || fifth.Role != "应" {
		t.Fatalf("fifth %+v", fifth)
	}
	if !strings.Contains(fifth.Marks, "囚") {
		t.Fatalf("water in 未 month should be 囚: %s", fifth.Marks)
	}
	second := p.Lines[1]
	if second.Role != "世" || second.Relation != "官鬼" || second.Branch != "卯" {
		t.Fatalf("second %+v", second)
	}
	if !strings.Contains(second.Marks, "动") || !strings.Contains(second.Marks, "临日") {
		t.Fatalf("second marks %s", second.Marks)
	}
	if !strings.Contains(second.Changed, "庚寅") || !strings.Contains(second.Changed, "化退") {
		t.Fatalf("changed %s", second.Changed)
	}
	if p.Lines[0].Spirit != "白虎" || p.Lines[4].Spirit != "勾陈" {
		t.Fatalf("spirits %s %s", p.Lines[0].Spirit, p.Lines[4].Spirit)
	}
	if strings.Contains(p.Prompt(), "伏神") && !strings.Contains(p.Prompt(), "伏神 无") {
		t.Fatalf("临 should not hide a relation:\n%s", p.Prompt())
	}
	if !strings.Contains(p.Glance(), "临之复") {
		t.Fatal(p.Glance())
	}
}

func TestGouHidesWealth(t *testing.T) {
	// 天风姤, 乾宫一世. 妻财伏在本宫二爻甲寅木.
	p, err := Build([6]int{8, 7, 7, 7, 7, 7}, Pillars{
		MonthZhi: "子", DayGan: "甲", DayZhi: "子", Void: "戌亥",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Primary != "姤" || p.Palace != "乾宫" || p.Position != "一世" {
		t.Fatalf("got %s %s %s", p.Primary, p.Palace, p.Position)
	}
	if p.Lines[0].Role != "世" {
		t.Fatalf("一世世在初, got %+v", p.Lines[0])
	}
	joined := strings.Join(p.Hidden, ";")
	if !strings.Contains(joined, "妻财") || !strings.Contains(joined, "甲寅") || !strings.Contains(joined, "二爻") {
		t.Fatalf("hidden %s", joined)
	}
}

func TestQianShiOnTop(t *testing.T) {
	p, err := Build([6]int{7, 7, 7, 7, 7, 7}, Pillars{MonthZhi: "子", DayGan: "甲", DayZhi: "子"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Primary != "乾" || p.Position != "本宫" {
		t.Fatalf("%s %s", p.Primary, p.Position)
	}
	if p.Lines[5].Role != "世" || p.Lines[2].Role != "应" {
		t.Fatalf("本宫世上应三, shi=%s ying line3=%s", p.Lines[5].Role, p.Lines[2].Role)
	}
}

func TestCastSeedRepeats(t *testing.T) {
	when := time.Date(2024, 1, 1, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	a, err := Cast(when, 42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Cast(when, 42)
	if err != nil {
		t.Fatal(err)
	}
	if a.Prompt() != b.Prompt() {
		t.Fatal("same seed must repeat the plate")
	}
	if a.Pillars.DayGan != "甲" || a.Pillars.DayZhi != "子" || a.Pillars.MonthZhi != "子" || a.Pillars.Void != "戌亥" {
		t.Fatalf("pillars %+v", a.Pillars)
	}
}

func TestWantsRecast(t *testing.T) {
	if WantsRecast("那财运呢") {
		t.Fatal("a follow-up keeps the cast")
	}
	if !WantsRecast("再摇一卦") {
		t.Fatal("再摇 should recast")
	}
}
