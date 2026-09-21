package avatar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestFindDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "web", "live2d")
	if err := os.MkdirAll(filepath.Join(dir, "models", "Haru"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modelRel), []byte(`{"Version":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("got %s", got)
	}
	if _, err := FindDir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("expected error")
	}
}

func TestHubDriveAndWS(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o644)
	h := NewHub()
	srv := httptest.NewServer(h.Handler(dir))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var hello Frame
	if err := conn.ReadJSON(&hello); err != nil {
		t.Fatal(err)
	}
	if hello.Type != "drive" {
		t.Fatalf("hello %+v", hello)
	}

	body := `{"mode":"celebrate","emotion":"joy","valence":0.9,"arousal":0.8,"engagement":0.9}`
	resp, err := http.Post(srv.URL+"/drive", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var posted Frame
	if err := json.NewDecoder(resp.Body).Decode(&posted); err != nil {
		t.Fatal(err)
	}
	if posted.Expression != ExpBright && posted.Expression != ExpPlay {
		t.Fatalf("posted %+v", posted)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var pushed Frame
	if err := conn.ReadJSON(&pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Mode != "celebrate" {
		t.Fatalf("ws %+v", pushed)
	}

	h.SetSense(func() any { return map[string]any{"who": "小春", "voice": "up"} })
	senseResp, err := http.Get(srv.URL + "/api/sense")
	if err != nil {
		t.Fatal(err)
	}
	defer senseResp.Body.Close()
	var snap map[string]any
	if err := json.NewDecoder(senseResp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap["who"] != "小春" {
		t.Fatalf("sense %+v", snap)
	}

	h.Mouth(0.73)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			if time.Now().After(deadline) {
				t.Fatal("no lipsync frame")
			}
			continue
		}
		if msg["type"] == "lipsync" {
			mouth, _ := msg["mouth"].(float64)
			if mouth >= 0.7 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no lipsync frame")
		}
	}

	held := 0
	holdDeadline := time.Now().Add(220 * time.Millisecond)
	for time.Now().Before(holdDeadline) {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		if msg["type"] == "lipsync" {
			mouth, _ := msg["mouth"].(float64)
			if mouth >= 0.7 {
				held++
			}
		}
	}
	if held < 2 {
		t.Fatalf("held lipsync frames %d, want keepalive while mouth is open", held)
	}

	var gotSrc string
	h.SetEye(func(source, dataURL string) { gotSrc = source })
	eyeBody := `{"source":"camera","data":"data:image/jpeg;base64,/9j/4AAQ"}`
	eyeResp, err := http.Post(srv.URL+"/api/eye", "application/json", strings.NewReader(eyeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer eyeResp.Body.Close()
	if eyeResp.StatusCode != 200 {
		t.Fatalf("eye status %d", eyeResp.StatusCode)
	}
	if gotSrc != "camera" {
		t.Fatalf("eye source %q", gotSrc)
	}
}

func TestHubMouthDecays(t *testing.T) {
	h := NewHub()
	h.Mouth(0.9)
	if h.MouthValue() < 0.8 {
		t.Fatalf("fresh mouth %v", h.MouthValue())
	}
	h.mouthAt.Store(time.Now().Add(-500 * time.Millisecond).UnixNano())
	if h.MouthValue() != 0 {
		t.Fatalf("stale mouth should close, got %v", h.MouthValue())
	}
}
