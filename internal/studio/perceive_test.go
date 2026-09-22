package studio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveStaysInsideMediaDir(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "old.png")
	newer := filepath.Join(dir, "new.png")
	os.WriteFile(older, []byte{0x89, 'P', 'N', 'G'}, 0o644)
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(newer, []byte{0x89, 'P', 'N', 'G'}, 0o644)
	os.WriteFile(filepath.Join(dir, "song.mp3"), []byte("ID3"), 0o644)

	got, err := Resolve(dir, KindPicture, "看看")
	if err != nil || filepath.Base(got) != "new.png" {
		t.Fatalf("newest picture %s %v", got, err)
	}
	got, err = Resolve(dir, KindPicture, "请看 old.png")
	if err != nil || filepath.Base(got) != "old.png" {
		t.Fatalf("named picture %s %v", got, err)
	}
	got, err = Resolve(dir, KindListen, "../secret.mp3")
	if err != nil || filepath.Base(got) != "song.mp3" {
		t.Fatalf("outside path must not win, got %s %v", got, err)
	}
}

func TestPerceivePictureAndSong(t *testing.T) {
	dir := t.TempDir()
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	imgPath := filepath.Join(dir, "flower.jpg")
	if err := os.WriteFile(imgPath, jpg, 0o644); err != nil {
		t.Fatal(err)
	}
	songPath := filepath.Join(dir, "flower.mp3")
	if err := os.WriteFile(songPath, []byte("ID3fake-audio-bytes-padded-past-thirty-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"text": "一束花在桌上"})
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "test-key", dir)
	c.HTTP = srv.Client()
	see := fakeSee{text: "桌上有一束花"}
	pic, err := c.Perceive(context.Background(), KindPicture, imgPath, see)
	if err != nil || pic.Text != "桌上有一束花" || pic.File != "flower.jpg" {
		t.Fatalf("picture %+v %v", pic, err)
	}
	heard, err := c.Perceive(context.Background(), KindListen, songPath, nil)
	if err != nil || heard.Text != "一束花在桌上" {
		t.Fatalf("listen %+v %v", heard, err)
	}
}

type fakeSee struct{ text string }

func (f fakeSee) ChatVision(context.Context, string, string, []byte) (string, error) {
	return f.text, nil
}
