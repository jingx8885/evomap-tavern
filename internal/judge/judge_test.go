package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jingx8885/lov-evo/internal/jev"
	"github.com/jingx8885/lov-evo/internal/memory"
	"github.com/jingx8885/lov-evo/internal/persona"
)

func f(v float64) *float64 { return &v }

func TestScoreNorm(t *testing.T) {
	a := jev.Answer{Score: f(4), Legend: map[string]string{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}}
	if got := scoreNorm(a); got != 1.0 {
		t.Fatalf("got %v", got)
	}
	a.Score = f(2)
	if got := scoreNorm(a); got != 0.5 {
		t.Fatalf("got %v", got)
	}
}

func TestParse(t *testing.T) {
	ans := map[string]jev.Answer{
		"valence":    {Score: f(3.5), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"arousal":    {Score: f(1), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"emotion":    {Choice: "sadness", Probabilities: map[string]float64{"sadness": 0.8}},
		"engagement": {Score: f(2), Legend: map[string]string{"0": "1", "1": "2", "2": "3", "3": "4", "4": "5"}},
		"safety":     {Noul: f(0.7)},
		"need_llm":   {Noul: f(0.2)},
	}
	j := Parse("i feel bad", ans)
	if j.Valence != 0.875 {
		t.Fatalf("valence %v", j.Valence)
	}
	if j.Emotion != "sadness" || j.SafetyP != 0.7 || j.Engagement != 0.5 {
		t.Fatalf("bad judgment: %+v", j)
	}
	if j.NeedLLMP != 0.2 {
		t.Fatalf("need_llm %v", j.NeedLLMP)
	}
	if j.WantLLM(0.55) {
		t.Fatal("low need_llm should not launch LLM")
	}
	if j.Attend != "" {
		t.Fatalf("missing attend should stay empty, got %q", j.Attend)
	}
	j = Parse("看看", map[string]jev.Answer{"attend": {Choice: "camera"}})
	if j.Attend != "camera" {
		t.Fatalf("attend %q", j.Attend)
	}
	j = Parse("看看", map[string]jev.Answer{"attend": {Choice: "nope"}})
	if j.Attend != "" {
		t.Fatalf("unknown attend should be dropped, got %q", j.Attend)
	}
}

func TestDerivedFeelingsAndLook(t *testing.T) {
	j := Parse("嗯", map[string]jev.Answer{
		"emotion": {Choice: "sadness"},
		"intent":  {Choice: "withdraw"},
	})
	if j.Valence < 0.2 || j.Valence > 0.35 {
		t.Fatalf("valence should follow sadness, got %v", j.Valence)
	}
	if j.Engagement >= 0.3 {
		t.Fatalf("withdraw should be low engagement, got %v", j.Engagement)
	}
	j = Parse("看摄像头", map[string]jev.Answer{"act": {Choice: ActCamera}})
	if j.Attend != "camera" {
		t.Fatalf("camera act should show the camera, got %q", j.Attend)
	}
	j = Parse("有几个人", map[string]jev.Answer{"act": {Choice: ActNone}})
	j.BindLook(ActCamera)
	if j.Attend != "camera" {
		t.Fatalf("open camera branch should stay on camera, got %q", j.Attend)
	}
}

func TestCapability(t *testing.T) {
	low := 0.1
	j := Parse("hi", map[string]jev.Answer{"need_llm": {Noul: f(0.2)}})
	if j.Act != "" || j.Capability(0.55) != ActNone {
		t.Fatalf("omitted low need_llm stays none: %+v", j)
	}
	j = Parse("plan this", map[string]jev.Answer{"need_llm": {Noul: f(0.9)}})
	if j.Capability(0.55) != ActPlan {
		t.Fatal("omitted act with high need_llm is plan")
	}
	j = Parse("chat", map[string]jev.Answer{
		"act":      {Choice: ActNone},
		"need_llm": {Noul: f(0.9)},
	})
	if j.Capability(0.55) != ActNone {
		t.Fatal("explicit none wins over need_llm")
	}
	j = Parse("open notepad", map[string]jev.Answer{"act": {Choice: ActComputerUse}})
	if !j.ComputerUseAllowed("continue") {
		t.Fatal("computer_use without a confidence should be allowed")
	}
	j = Parse("open notepad", map[string]jev.Answer{
		"act": {Choice: ActComputerUse, Confidence: &low},
	})
	if j.ComputerUseAllowed("continue") {
		t.Fatal("low confidence must hold computer use")
	}
	if j.ComputerUseAllowed("safety") {
		t.Fatal("safety must hold computer use")
	}
	j = Parse("改你自己", map[string]jev.Answer{"act": {Choice: ActCodex}})
	if !j.CodexAllowed("continue") || j.ComputerUseAllowed("continue") {
		t.Fatal("codex is its own latch, not computer use")
	}
	j = Parse("改你自己", map[string]jev.Answer{
		"act": {Choice: ActCodex, Confidence: &low},
	})
	if j.CodexAllowed("continue") {
		t.Fatal("low confidence must hold codex")
	}
	j = Parse("huh", map[string]jev.Answer{"act": {Choice: "sudo"}})
	if j.Act != "" || j.Capability(0.55) != ActNone {
		t.Fatalf("unknown act dropped: %+v", j)
	}
}

func TestDecideMode(t *testing.T) {
	a := memory.Affect{Valence: 0.2, Arousal: 0.4, Emotion: "sadness"}
	j := &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, SafetyP: 0.1}
	if got := DecideMode(j, a, 0.6, ""); got != "comfort" {
		t.Fatalf("want comfort, got %s", got)
	}
	j.SafetyP = 0.9
	if got := DecideMode(j, a, 0.6, ""); got != "safety" {
		t.Fatalf("want safety, got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.5, Engagement: 0.1}
	if got := DecideMode(j, a, 0.6, ""); got != "re_engage" {
		t.Fatalf("want re_engage, got %s", got)
	}
	j = &Judgment{Emotion: "joy", Valence: 0.9, Engagement: 0.9}
	if got := DecideMode(j, a, 0.6, "push"); got != "celebrate" {
		t.Fatalf("want celebrate, got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.6, Engagement: 0.8}
	if got := DecideMode(j, a, 0.6, "push"); got != "goal_push" {
		t.Fatalf("want goal_push, got %s", got)
	}
	j = &Judgment{Emotion: "joy", Valence: 0.52, Engagement: 0.74, SafetyP: 0.03}
	if got := DecideMode(j, a, 0.6, "push"); got != "celebrate" {
		t.Fatalf("mild joy must celebrate, not goal_push; got %s", got)
	}
	j = &Judgment{Emotion: "anger", Valence: 0.17, Arousal: 0.26, Engagement: 0.14}
	if got := DecideMode(j, a, 0.6, "push"); got != "de_escalate" {
		t.Fatalf("low-arousal anger must de_escalate, not re_engage; got %s", got)
	}
	j = &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, Intent: "banter"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("playful 我好惨 is banter, not comfort; got %s", got)
	}
	j = &Judgment{Emotion: "neutral", Valence: 0.5, Engagement: 0.1, Intent: "goodbye"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("goodbye must not re_engage; got %s", got)
	}
	j = &Judgment{Emotion: "sadness", Valence: 0.2, Engagement: 0.8, Mode: "continue"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("Jev mode wins over the emotion heuristic; got %s", got)
	}
	j = &Judgment{Emotion: "joy", Valence: 0.9, Engagement: 0.9, Mode: "celebrate", SafetyP: 0.9}
	if got := DecideMode(j, a, 0.6, ""); got != "safety" {
		t.Fatalf("safety latch still overrides Jev mode; got %s", got)
	}
	j = &Judgment{Mode: "goal_push"}
	if got := DecideMode(j, a, 0.6, ""); got != "continue" {
		t.Fatalf("goal_push without a plan note must not fire; got %s", got)
	}
}

func TestOffPersona(t *testing.T) {
	if (&Judgment{}).OffPersona(0.45) {
		t.Fatal("unasked fit is not a miss")
	}
	if !(&Judgment{PersonaFitP: 0.2}).OffPersona(0.45) {
		t.Fatal("low fit should snap back")
	}
	if (&Judgment{PersonaFitP: 0.9}).OffPersona(0.45) {
		t.Fatal("high fit should not snap back")
	}
	n := 0.05
	j := &Judgment{Raw: map[string]jev.Answer{"persona_fit": {Noul: &n}}}
	j.PersonaFitP = 0.05
	if !j.OffPersona(0.45) {
		t.Fatal("raw noul 0.05 is a miss")
	}
}

func TestJudgeTurnAgainstFakeServer(t *testing.T) {
	var gotQuestions map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if qs, ok := req["questions"].(map[string]any); ok {
			gotQuestions = qs
		} else {
			t.Error("missing questions")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-latest",
			"answers": map[string]any{
				"valence":      map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"arousal":      map[string]any{"type": "score", "score": 2, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"emotion":      map[string]any{"type": "choice", "choice": "joy", "probabilities": map[string]float64{"joy": 0.9}},
				"engagement":   map[string]any{"type": "score", "score": 4, "legend": map[string]any{"0": "a", "1": "b", "2": "c", "3": "d", "4": "e"}},
				"safety":       map[string]any{"type": "noul", "noul": 0.01},
				"persona_fit":  map[string]any{"type": "noul", "noul": 0.9},
				"need_llm":     map[string]any{"type": "noul", "noul": 0.12},
				"keep":         map[string]any{"type": "noul", "noul": 0.2},
				"intent":       map[string]any{"type": "choice", "choice": "share_good"},
				"self_emotion": map[string]any{"type": "choice", "choice": "joy"},
				"mode":         map[string]any{"type": "choice", "choice": "celebrate"},
				"attend":       map[string]any{"type": "choice", "choice": "none"},
				"act":          map[string]any{"type": "choice", "choice": "none"},
			},
		})
	}))
	defer srv.Close()
	p := &persona.Persona{Name: "test", Style: "warm",
		Judge: persona.JudgeConfig{Emotion: true, PersonaFit: true, Safety: true}}
	jc := jev.NewClient(srv.URL, "k", "")
	mem := memory.New(8)
	mem.Add(memory.Turn{Speaker: "assistant", Text: "welcome!"})
	p.Reactions = map[string]string{"comfort": "先嫌一句，再帮忙"}
	jd, err := JudgeTurn(context.Background(), jc, p, mem, "I love this place", "", Observe{}, Branch{})
	if err != nil {
		t.Fatal(err)
	}
	if jd.Emotion != "joy" || jd.Valence != 1.0 || jd.PersonaFitP != 0.9 {
		t.Fatalf("bad judgment: %+v", jd)
	}
	if jd.Intent != "share_good" || jd.SelfEmotion != "joy" || jd.Mode != "celebrate" {
		t.Fatalf("jev extras %+v", jd)
	}
	if jd.NeedLLMP != 0.12 || jd.WantLLM(0) {
		t.Fatalf("need_llm %+v", jd)
	}
	if jd.Attend != "none" {
		t.Fatalf("attend %q", jd.Attend)
	}
	if jd.Act != ActNone || jd.Capability(0.55) != ActNone {
		t.Fatalf("act %+v", jd)
	}
	for _, q := range []string{"emotion", "arousal", "self_emotion", "intent", "keep", "act", "safety", "persona_fit"} {
		if _, ok := gotQuestions[q]; !ok {
			t.Fatalf("turn judge must ask %s, got %v", q, gotQuestions)
		}
	}
	for _, q := range []string{"valence", "engagement", "mode", "need_llm", "attend", "window", "win_op", "win_target"} {
		if _, ok := gotQuestions[q]; ok {
			t.Fatalf("merged question %s should not be asked", q)
		}
	}
	if len(gotQuestions) != 8 {
		t.Fatalf("ordinary turn should ask 8 questions, got %d: %v", len(gotQuestions), gotQuestions)
	}
	actQ, _ := gotQuestions["act"].(map[string]any)
	actCrit, _ := actQ["criteria"].(map[string]any)
	for _, id := range []string{"reflect", "look", "camera", "screen", "shot", "codex", "computer_use", "image", "video", "speech", "song", "picture", "watch", "listen"} {
		if _, ok := actCrit[id]; !ok {
			t.Fatalf("act must offer %s: %v", id, actCrit)
		}
	}
	if _, ok := actCrit["see"]; ok {
		t.Fatal("see is not a capability; camera and screen are separate")
	}
	if _, ok := gotQuestions["branch_done"]; ok {
		t.Fatal("branch_done is only asked while a branch is open")
	}
}

func TestBranchDoneOnlyWhileOpen(t *testing.T) {
	p := &persona.Persona{Name: "t"}
	closed := questions(p, false, "", Branch{}, WindowView{})
	if _, ok := closed["branch_done"]; ok {
		t.Fatal("no open branch, no branch_done")
	}
	open := questions(p, false, "", Branch{Kind: ActComputerUse, Goal: "打开记事本"}, WindowView{})
	if _, ok := open["branch_done"]; !ok {
		t.Fatal("open branch must ask branch_done")
	}
	act, _ := open["act"]
	instr, _ := act.Instructions.(string)
	if !strings.Contains(instr, "state.branch is already open") {
		t.Fatal("open branch should tell act to stay")
	}
}

func TestStayOnOpenBranch(t *testing.T) {
	done := 0.9
	open := Parse("继续", map[string]jev.Answer{
		"act":         {Choice: ActNone},
		"branch_done": {Noul: f(0.2)},
	})
	act, finished := open.Stay(ActComputerUse, 0.55, 0)
	if finished || act != ActComputerUse {
		t.Fatalf("open branch must stick, got %s done=%v", act, finished)
	}
	closed := Parse("好了", map[string]jev.Answer{
		"act":         {Choice: ActNone},
		"branch_done": {Noul: &done},
	})
	act, finished = closed.Stay(ActComputerUse, 0.55, 0)
	if !finished || act != ActNone {
		t.Fatalf("branch_done must release, got %s done=%v", act, finished)
	}
	next := Parse("再打开声音", map[string]jev.Answer{
		"act":         {Choice: ActComputerUse},
		"branch_done": {Noul: &done},
	})
	act, finished = next.Stay(ActCodex, 0.55, 0)
	if !finished || act != ActComputerUse {
		t.Fatalf("a finished branch can hand off, got %s done=%v", act, finished)
	}
	fresh := Parse("帮我开记事本", map[string]jev.Answer{"act": {Choice: ActComputerUse}})
	act, finished = fresh.Stay("", 0.55, 0)
	if finished || act != ActComputerUse {
		t.Fatalf("no branch should follow act, got %s done=%v", act, finished)
	}
	leave := Parse("看一下桌面上有什么", map[string]jev.Answer{
		"act":         {Choice: ActScreen},
		"branch_done": {Noul: f(0.2)},
	})
	act, finished = leave.Stay(ActDivine, 0.55, 0)
	if finished || act != ActScreen {
		t.Fatalf("a different act must leave, got %s done=%v", act, finished)
	}
	if !leave.Yields(ActDivine) {
		t.Fatal("explicit screen should yield an open divine branch")
	}
	if leave.Yields(ActScreen) {
		t.Fatal("the same act does not yield")
	}
	if fresh.BranchDone(0) {
		t.Fatal("unasked branch_done is not finished")
	}
}

func TestWindowQuestionsStayClosed(t *testing.T) {
	p := &persona.Persona{Name: "t"}
	bare := questions(p, false, "", Branch{}, WindowView{})
	if _, ok := bare["window"]; ok {
		t.Fatal("no page module, no window question")
	}
	view := WindowView{
		Modules: []WindowMod{{ID: "stage", Title: "stage", Open: true}},
		Ops: map[string]string{
			"none": "leave", "open": "show", "layout_split": "split", "feature": "feature a job",
		},
		Targets: map[string]string{"none": "leave", "j1": "image ready"},
	}
	qs := questions(p, false, "", Branch{}, view)
	page, ok := qs["page"]
	if !ok {
		t.Fatal("registered module must ask one page question")
	}
	if _, ok := qs["window"]; ok || qs["win_op"].Type != "" || qs["win_target"].Type != "" {
		t.Fatal("page ops must not be three questions")
	}
	if _, ok := page.Criteria["stage:layout_split"]; !ok {
		t.Fatalf("page criteria %v", page.Criteria)
	}
	if _, ok := page.Criteria["stage:feature:j1"]; !ok {
		t.Fatalf("feature target missing: %v", page.Criteria)
	}
	jd := Parse("打开", map[string]jev.Answer{
		"page": {Choice: "stage:layout_split"},
	})
	takeWindow(&jd, view)
	if jd.Window != "stage" || jd.WinOp != "layout_split" {
		t.Fatalf("window %+v", jd)
	}
	if jd.WinTarget != "" {
		t.Fatalf("unknown target kept: %q", jd.WinTarget)
	}
	bad := Parse("打开", map[string]jev.Answer{"page": {Choice: "stage:click"}})
	takeWindow(&bad, view)
	if bad.WinOp != "" {
		t.Fatalf("unknown op kept: %q", bad.WinOp)
	}
}
