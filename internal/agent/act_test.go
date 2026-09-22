package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
	"github.com/jingx8885/lov-evo/internal/planner"
	"github.com/jingx8885/lov-evo/internal/sense"
)

func TestDispatchReflectLookAndHeldHands(t *testing.T) {
	bus, err := sense.Open(sense.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	p := &persona.Persona{
		Name:  "小春",
		Sense: persona.SenseConfig{Enabled: true},
		Judge: persona.JudgeConfig{NeedLLMThresh: 0.55},
	}
	pl := planner.New(p, nil, nil, "")
	mem := memory.New(4)
	opt := Options{sense: bus}
	slot := &capabilitySlot{}

	reflect := &judge.Judgment{Act: judge.ActReflect, Emotion: "neutral", SelfEmotion: "joy", UserText: "你现在什么感觉"}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, reflect, "continue", reflect.UserText, true)
	if !sawSummary(bus, "reflect") {
		t.Fatal("reflect did not land on the sense bus")
	}

	lookEarly := &judge.Judgment{Act: judge.ActLook, UserText: "你内部是怎么活的"}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, lookEarly, "continue", lookEarly.UserText, true)
	if sawSummary(bus, "look") {
		t.Fatal("an open reflect branch must stay until branch_done")
	}

	done := 0.9
	look := &judge.Judgment{
		Act: lookEarly.Act, BranchDoneP: done, UserText: lookEarly.UserText,
		Raw: map[string]jev.Answer{"branch_done": {Noul: &done}},
	}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, look, "continue", look.UserText, true)
	if !sawSummary(bus, "look") {
		t.Fatal("look did not land on the sense bus")
	}
	felt := bus.Felt(p, sense.Ask{Kind: sense.AskCode}, "")
	if !strings.Contains(felt, "one judgment picks mood") {
		t.Fatalf("code look missing logic cue: %q", felt)
	}

	held := &judge.Judgment{
		Act: judge.ActCodex, BranchDoneP: done, UserText: "改一下你自己",
		Raw: map[string]jev.Answer{"branch_done": {Noul: &done}},
	}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, held, "safety", held.UserText, true)
	if sawSummary(bus, "codex start") {
		t.Fatal("safety must not start codex")
	}
	use := &judge.Judgment{
		Act: judge.ActComputerUse, BranchDoneP: done, UserText: "打开记事本",
		Raw: map[string]jev.Answer{"branch_done": {Noul: &done}},
	}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, use, "safety", use.UserText, true)
	if sawSummary(bus, "computer_use start") {
		t.Fatal("safety must not start computer use")
	}
	song := &judge.Judgment{
		Act: judge.ActSong, BranchDoneP: done, UserText: "写一首关于花的歌",
		Raw: map[string]jev.Answer{"branch_done": {Noul: &done}},
	}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, song, "safety", song.UserText, true)
	if sawSummary(bus, "song start") {
		t.Fatal("safety must not start a song")
	}
	listen := &judge.Judgment{
		Act: judge.ActListen, BranchDoneP: done, UserText: "听听刚做的歌",
		Raw: map[string]jev.Answer{"branch_done": {Noul: &done}},
	}
	dispatchCapability(context.Background(), opt, p, nil, nil, mem, pl, nil, slot, listen, "safety", listen.UserText, true)
	if sawSummary(bus, "listen start") {
		t.Fatal("safety must not start listening")
	}
}

func sawSummary(bus *sense.Bus, summary string) bool {
	for _, ev := range bus.Snapshot("").Recent {
		if ev.Summary == summary {
			return true
		}
	}
	return false
}
