package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Completer is the slow reader. It does not speak to the user.
type Completer interface {
	ChatComplete(ctx context.Context, system, user string) (string, error)
}

// Reading is a steering note for the voice, not a script.
type Reading struct {
	Note  string `json:"note"`
	Omen  string `json:"omen"`
	Focus string `json:"focus"`
}

// WantsRecast reports a follow-up that asks for a new toss.
// The open cast stays until one of these is said.
func WantsRecast(text string) bool {
	for _, w := range []string{"再摇", "重摇", "再算", "重新算", "另起", "再占", "再起卦", "再起一卦", "换一卦"} {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// Read asks the LLM to judge the plate that Go already cast.
func Read(ctx context.Context, llm Completer, name, style, plate, question string) (Reading, error) {
	if llm == nil {
		return Reading{}, fmt.Errorf("no reader")
	}
	plate = strings.TrimSpace(plate)
	question = strings.TrimSpace(question)
	if plate == "" || question == "" {
		return Reading{}, fmt.Errorf("plate and question required")
	}
	system := "你是人格语音机器人里单独的六爻断卦人，不对用户开口。" +
		"卦盘是程序用铜钱摇好的，不许改爻、不许换卦、不许改世应、六亲、六神、月建、日辰。" +
		"按京房纳甲取用神：求财看妻财，求官求名看官鬼，占父母长辈文书看父母，占子女福德医药看子孙，占兄弟同辈竞争看兄弟。" +
		"动爻、世应、旬空、月破、化进化退、回头生克都在盘上，用它们断，不要另起一套。" +
		"行末的备考是卦书白话，不能推翻纳甲。" +
		"输出给声音模型的行为提示：她该传达的意思，不是逐字台词，一两句中文。" +
		"不要断言生死、疾病结局或犯罪，不要教人伤害自己或别人。" +
		"若所问是自伤、自杀或伤害他人，omen 必须是 refuse，note 让她陪着，不要断吉凶。" +
		"只输出 JSON：{\"note\":\"提示\",\"omen\":\"吉|凶|平|未明|refuse\",\"focus\":\"用神在哪一爻\"}"
	user := fmt.Sprintf("人设：%s（%s）\n所问（后一行是这一次要答的）：\n%s\n\n卦盘：\n%s",
		or(name, "她"), or(style, "按她自己的口气"), question, plate)
	text, err := llm.ChatComplete(ctx, system, user)
	if err != nil {
		return Reading{}, err
	}
	var out Reading
	if err := json.Unmarshal([]byte(extractJSON(text)), &out); err != nil {
		return Reading{}, fmt.Errorf("reader json: %w", err)
	}
	out.Note = strings.TrimSpace(out.Note)
	out.Focus = strings.TrimSpace(out.Focus)
	out.Omen = normalizeOmen(out.Omen)
	if out.Note == "" {
		return Reading{}, fmt.Errorf("reader note empty")
	}
	return out, nil
}

func normalizeOmen(s string) string {
	s = strings.TrimSpace(s)
	switch s {
	case "吉", "凶", "平", "未明", "refuse":
		return s
	default:
		return "未明"
	}
}

func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}
