package window

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueRunsAndGlanceMatchesLayout(t *testing.T) {
	started := make(chan struct{})
	st := NewStage(StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			close(started)
			report("drawing", 0.4)
			return "rose.jpg", "", nil
		},
	})
	defer st.Close()
	reg := New(Options{Open: func(string) error { return nil }})
	reg.Register(st)

	job := st.Enqueue(KindImage, "a red rose")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	done, err := st.Wait(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusReady || done.File != "rose.jpg" {
		t.Fatalf("done %+v", done)
	}
	if _, err := reg.Apply("stage", OpLayoutSplit, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Apply("", OpOpen, ""); err != nil {
		t.Fatal(err)
	}
	glance, err := reg.Apply("stage", OpFeature, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"open", "split", "left column", "right frame", "rose", "still image"} {
		if !strings.Contains(strings.ToLower(glance), want) {
			t.Fatalf("glance %q missing %q", glance, want)
		}
	}
	if _, err := reg.Apply("stage", "click", ""); err == nil {
		t.Fatal("unknown op must be refused")
	}
	if st.Layout() != LayoutSplit {
		t.Fatalf("unknown op changed layout to %s", st.Layout())
	}
}

func TestCancelQueuedJob(t *testing.T) {
	block := make(chan struct{})
	st := NewStage(StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			<-block
			return "", "", nil
		},
	})
	defer st.Close()
	defer close(block)
	reg := New(Options{})
	reg.Register(st)
	first := st.Enqueue(KindLLM, "hold the worker")
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("first job did not start")
	case <-waitStatus(st, first.ID, StatusRunning):
	}
	second := st.Enqueue(KindImage, "later")
	if _, err := reg.Apply("stage", OpCancel, second.ID); err != nil {
		t.Fatal(err)
	}
	done, err := st.Wait(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusCanceled {
		t.Fatalf("status %s", done.Status)
	}
}

func waitStatus(st *Stage, id, status string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, j := range st.Snapshot() {
				if j.ID == id && j.Status == status {
					close(ch)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ch
}

func TestDesktopJSONMatchesGlance(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rose.jpg"), []byte("jpeg"), 0o644)
	st := NewStage(StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			return "rose.jpg", "", nil
		},
	})
	defer st.Close()
	reg := New(Options{MediaDir: dir})
	reg.Register(st)
	job := st.Enqueue(KindImage, "a red rose")
	if _, err := st.Wait(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	st.SetLook(job.ID, "a single red rose")
	if _, err := reg.Apply("stage", OpOpen, ""); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktop", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Open   bool   `json:"open"`
		Glance string `json:"glance"`
		View   struct {
			Layout string `json:"layout"`
			Jobs   []Job  `json:"jobs"`
		} `json:"view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Open || body.View.Layout != LayoutSplit || len(body.View.Jobs) != 1 {
		t.Fatalf("body %+v", body)
	}
	if !strings.Contains(body.Glance, "left column") || !strings.Contains(body.Glance, "red rose") {
		t.Fatalf("glance %s", body.Glance)
	}

	rec = httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/rose.jpg", nil))
	if rec.Code != 200 || rec.Body.String() != "jpeg" {
		t.Fatalf("media %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/..%2F..%2Fwindows%2Fwin.ini", nil))
	if rec.Code == 200 {
		t.Fatal("media path must stay inside the directory")
	}
}

func TestCoveredGlanceHidesCards(t *testing.T) {
	st := NewStage(StageOptions{Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
		return "", "note", nil
	}})
	defer st.Close()
	reg := New(Options{})
	reg.Register(st)
	job := st.Enqueue(KindLLM, "evening plan")
	if _, err := st.Wait(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	g := reg.Glance()
	if !strings.Contains(g, "covered") || !strings.Contains(g, "evening plan") {
		t.Fatalf("glance %s", g)
	}
	if strings.Contains(g, "window is open") {
		t.Fatalf("closed window described as open: %s", g)
	}
}

func TestStagePanelNewestFirst(t *testing.T) {
	block := make(chan struct{})
	st := NewStage(StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			<-block
			return "", "note", nil
		},
	})
	defer st.Close()
	defer close(block)

	older := st.Enqueue(KindLLM, "older note")
	newer := st.Enqueue(KindImage, "newer picture")
	panel := st.QueueView(true, "http://127.0.0.1:9/")
	if !panel.Available || !panel.Open || panel.Media != "http://127.0.0.1:9" {
		t.Fatalf("panel %+v", panel)
	}
	if len(panel.Jobs) != 2 || panel.Jobs[0].ID != newer.ID || panel.Jobs[1].ID != older.ID {
		t.Fatalf("order %+v", panel.Jobs)
	}
	if panel.Feature != older.ID {
		t.Fatalf("feature stays on the first job, got %s", panel.Feature)
	}
	raw, err := json.Marshal(st.QueueView(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"jobs":[`) || strings.Contains(string(raw), `"jobs":null`) {
		t.Fatalf("jobs must be an array: %s", raw)
	}

	empty := NewStage(StageOptions{})
	defer empty.Close()
	blank := empty.QueueView(false, "  ")
	if blank.Jobs == nil || len(blank.Jobs) != 0 || blank.Media != "" {
		t.Fatalf("empty %+v", blank)
	}
}
