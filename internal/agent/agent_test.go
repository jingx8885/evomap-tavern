package agent

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingx8885/lov-evo/internal/eye"
	"github.com/jingx8885/lov-evo/internal/livevoice"
)

func TestSightFollowUpNamesTheQuestion(t *testing.T) {
	msg := sightSpoke(eye.SourceCamera, true, true, "有几个人")
	if !strings.Contains(msg, "有几个人") || !strings.Contains(msg, "follow-up") {
		t.Fatalf("follow-up %q", msg)
	}
	plain := sightSpoke(eye.SourceCamera, true, false, "你好")
	if strings.Contains(plain, "follow-up") || strings.Contains(plain, "你好") {
		t.Fatalf("plain look %q", plain)
	}
}

func TestJudgeWorthSkipsNoiseAndFragments(t *testing.T) {
	if judgeWorth("先加") || judgeWorth("就是") || judgeWorth("[mouth noise") || judgeWorth("[tongue click]") {
		t.Fatal("noise and short fragments must not spend a Jev call")
	}
	if !judgeWorth("就是这个 prompt 我们要配到那个代码上") {
		t.Fatal("a real utterance should be judged")
	}
}

func TestClipCountsRunes(t *testing.T) {
	s := strings.Repeat("邮", 40)
	got := clip(s, 8)
	if got != "邮邮邮邮邮邮邮邮..." {
		t.Fatalf("got %q", got)
	}
	if clip("你好", 8) != "你好" {
		t.Fatal("short text should pass through")
	}
}

func TestViewerLogKeepsDecisionPath(t *testing.T) {
	keep := []string{
		"[judge] emotion=joy act=none",
		"[steer] mode=continue",
		"[user] 你好",
		"[user~] 你",
		"[assistant] 在。",
		"[act] plan held mode=continue",
		"[branch] open computer_use",
		"[planner] need_llm=0.80 -> refining (async)",
		"[eye] camera vlm: timeout",
		"[eye] push camera: broken",
	}
	drop := []string{
		"[livevoice] uplink: frames=10 max_rms=0.2",
		"[eye] camera: 对面有个人",
		"[eye] screen: 记事本",
		"   ",
	}
	for _, line := range keep {
		if !viewerLog(line) {
			t.Fatalf("dropped decision line %q", line)
		}
	}
	for _, line := range drop {
		if viewerLog(line) {
			t.Fatalf("noise leaked %q", line)
		}
	}
}

func TestCapabilitySlotOneComputerUse(t *testing.T) {
	var s capabilitySlot
	ok, gen := s.tryComputer()
	if !ok {
		t.Fatal("first computer use should start")
	}
	if ok, _ := s.tryComputer(); ok {
		t.Fatal("a second computer use must wait")
	}
	s.endComputer(gen)
	if ok, _ := s.tryComputer(); !ok {
		t.Fatal("after the run ends, computer use can start again")
	}
}

func TestBranchStaysUntilClosed(t *testing.T) {
	var s capabilitySlot
	s.open("computer_use", "打开记事本")
	s.follow("把字号调大")
	s.follow("打开记事本")
	kind, goal, _ := s.current()
	if kind != "computer_use" || goal != "打开记事本\n把字号调大" {
		t.Fatalf("branch should keep the follow-up, got %s %q", kind, goal)
	}
	ok, gen := s.tryComputer()
	if !ok {
		t.Fatal("work should start")
	}
	s.close()
	if kind, _, _ := s.current(); kind != "" {
		t.Fatal("close should drop the branch")
	}
	s.mu.Lock()
	s.computer = true
	s.mu.Unlock()
	s.endComputer(gen)
	if !s.busy() {
		t.Fatal("an old run must not clear a newer busy flag")
	}
}

func TestJevGateDedupesExactUtterance(t *testing.T) {
	g := newJevGate()
	g.minInterval = 0
	if !g.claim("声音好很多了") {
		t.Fatal("first claim")
	}
	if g.claim("声音好很多了") {
		t.Fatal("inflight should block")
	}
	g.finish("声音好很多了", true)
	if g.claim("声音好很多了") {
		t.Fatal("recent done should block")
	}
	if !g.claim("换一个笑话") {
		t.Fatal("new utterance should claim")
	}
	g.finish("换一个笑话", true)
}

func TestJevGateScheduleDebounces(t *testing.T) {
	g := newJevGate()
	var n atomic.Int32
	g.schedule(40*time.Millisecond, "能听", func() { n.Add(1) })
	g.schedule(40*time.Millisecond, "能听到我说话吗", func() { n.Add(1) })
	time.Sleep(80 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("fired %d times, want 1", n.Load())
	}
	g.cancelTimer()
	g.schedule(80*time.Millisecond, "嗯", func() { n.Add(1) })
	g.cancelTimer()
	time.Sleep(120 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("cancelled timer still fired, got %d", n.Load())
	}
}

func TestJevGateOneEarlyCallPerUtterance(t *testing.T) {
	g := newJevGate()
	g.minInterval = 0
	if g.wantEarly("嗯") {
		t.Fatal("short fragment should wait for turn.done")
	}
	if ok, _ := g.claimEpoch("嗯", true); ok {
		t.Fatal("short speculative claim")
	}
	if !g.wantEarly("我想问一下美国公司怎么开") {
		t.Fatal("first long partial should be eligible")
	}
	ok, epoch := g.claimEpoch("我想问一下美国公司怎么开", true)
	if !ok {
		t.Fatal("first early claim")
	}
	g.finish("我想问一下美国公司怎么开", true)
	if g.wantEarly("我想问一下美国公司怎么开，审核要什么条件") {
		t.Fatal("second partial of the same utterance should not call Jev")
	}
	if ok, _ := g.claimEpoch("我想问一下美国公司怎么开，审核要什么条件", true); ok {
		t.Fatal("second speculative claim")
	}

	g.closeUtterance()
	if g.current(epoch) {
		t.Fatal("turn.done should retire the early result")
	}
	if !g.claim("我想问一下美国公司怎么开，审核要什么条件") {
		t.Fatal("finished utterance should still be judged once")
	}
	g.finish("我想问一下美国公司怎么开，审核要什么条件", true)
	if g.claim("我想问一下美国公司怎么开，审核要什么条件") {
		t.Fatal("final text should not be judged twice")
	}
}

func TestJevGateCooldown(t *testing.T) {
	g := newJevGate()
	g.minInterval = 40 * time.Millisecond
	if !g.claim("第一句已经说完了") {
		t.Fatal("first turn")
	}
	g.finish("第一句已经说完了", true)
	if g.claim("第二句紧接着来了") {
		t.Fatal("cooldown should block the next turn")
	}
	time.Sleep(50 * time.Millisecond)
	if !g.claim("隔了一会儿再说") {
		t.Fatal("claim after cooldown")
	}
}

func TestJevGateCooldownLeft(t *testing.T) {
	g := newJevGate()
	g.minInterval = 60 * time.Millisecond
	if g.cooldownLeft() != 0 {
		t.Fatal("no call yet, nothing to wait for")
	}
	g.claim("第一句已经说完了")
	g.finish("第一句已经说完了", true)
	left := g.cooldownLeft()
	if left <= 0 || left > g.minInterval {
		t.Fatalf("left %s", left)
	}
	time.Sleep(70 * time.Millisecond)
	if g.cooldownLeft() != 0 || g.cooling() {
		t.Fatal("cooldown should be over")
	}
}

func TestJevGateFailedFinishClearsCooldown(t *testing.T) {
	g := newJevGate()
	g.minInterval = 200 * time.Millisecond
	if !g.claim("第一句没判成") {
		t.Fatal("first claim")
	}
	g.finish("第一句没判成", false)
	if g.cooling() || !g.claim("马上再说一句") {
		t.Fatal("a failed judge should not start the cooldown")
	}
}

func TestJevGateCooldownRetriesTheLatestTurn(t *testing.T) {
	g := newJevGate()
	g.minInterval = 40 * time.Millisecond
	if !g.claim("第一句已经说完了") {
		t.Fatal("first turn")
	}
	g.finish("第一句已经说完了", true)
	if g.claim("第二句紧接着来了") {
		t.Fatal("cooldown should block")
	}
	wait := g.cooldownLeft()
	if wait <= 0 {
		t.Fatal("expected a wait")
	}
	got := make(chan string, 2)
	g.schedule(wait+20*time.Millisecond, "第二句紧接着来了", func() {
		if g.claim("第二句紧接着来了") {
			got <- "old"
		}
	})
	g.schedule(wait+20*time.Millisecond, "更新的一句", func() {
		if g.claim("更新的一句") {
			got <- "new"
			g.finish("更新的一句", true)
		}
	})
	select {
	case v := <-got:
		if v != "new" {
			t.Fatalf("retry ran %s", v)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cooled turn was not retried")
	}
	select {
	case v := <-got:
		t.Fatalf("older retry also ran: %s", v)
	case <-time.After(60 * time.Millisecond):
	}
	if g.claim("更新的一句") {
		t.Fatal("the retried turn should count as judged")
	}
}

func TestVoiceHoldPrefersTheLiveSession(t *testing.T) {
	old := &livevoice.Session{}
	var h *voiceHold
	if h.live(old) != old {
		t.Fatal("nil hold keeps the fallback")
	}
	h = &voiceHold{}
	if h.live(old) != old {
		t.Fatal("unbound hold keeps the fallback")
	}
	cur := &livevoice.Session{}
	h.bind(cur)
	if h.live(old) != cur {
		t.Fatal("work that outlived a reconnect must reach the new call")
	}
}
