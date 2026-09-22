package studio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestImageSpeechVideoAndSong(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	var polls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" && !strings.HasSuffix(r.URL.Path, ".mp3") {
			t.Errorf("missing auth on %s", r.URL.Path)
		}
		switch {
		case r.URL.Path == "/v1/images/generations":
			raw := base64.StdEncoding.EncodeToString(jpeg)
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": raw}}})
		case r.URL.Path == "/v1/audio/speech":
			w.Write([]byte("ID3fake-audio-bytes-padded-past-thirty-two"))
		case r.URL.Path == "/v1/videos/generations":
			json.NewEncoder(w).Encode(map[string]any{"id": "vid-1", "status": "queued"})
		case r.URL.Path == "/v1/videos/vid-1":
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n == 1 {
				json.NewEncoder(w).Encode(map[string]any{"status": "processing", "progress": 40})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "completed"})
		case r.URL.Path == "/v1/videos/vid-1/content":
			w.Write([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0})
		case r.URL.Path == "/suno/submit/MUSIC":
			json.NewEncoder(w).Encode(map[string]any{"code": "success", "data": "song-1"})
		case r.URL.Path == "/suno/fetch/song-1":
			json.NewEncoder(w).Encode(map[string]any{
				"code": "success",
				"data": map[string]any{
					"status": "SUCCESS",
					"data":   []any{map[string]any{"audio_url": "http://" + r.Host + "/clip.mp3", "title": "花"}},
				},
			})
		case r.URL.Path == "/clip.mp3":
			w.Write([]byte("ID3song-bytes-here-padded-past-thirty-two"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	var seen []string
	c := New(srv.URL+"/v1", "test-key", dir)
	c.HTTP = srv.Client()
	c.PollEvery = 5 * time.Millisecond
	c.OnProgress = func(p Progress) { seen = append(seen, p.Kind+":"+p.Status) }

	img, err := c.Run(context.Background(), KindImage, "a bouquet")
	if err != nil || len(img.Files) != 1 || !strings.HasSuffix(img.Files[0], ".jpg") {
		t.Fatalf("image %+v %v", img, err)
	}
	if _, err := os.Stat(filepath.Join(dir, img.Files[0])); err != nil {
		t.Fatal(err)
	}

	sp, err := c.Run(context.Background(), KindSpeech, "给你一束花")
	if err != nil || !strings.HasSuffix(sp.Files[0], ".mp3") {
		t.Fatalf("speech %+v %v", sp, err)
	}

	vid, err := c.Run(context.Background(), KindVideo, "the bouquet sways")
	if err != nil || !strings.HasSuffix(vid.Files[0], ".mp4") {
		t.Fatalf("video %+v %v", vid, err)
	}
	if polls < 2 {
		t.Fatalf("video polls %d", polls)
	}

	song, err := c.Run(context.Background(), KindSong, "a warm instrumental about flowers")
	if err != nil || !strings.HasSuffix(song.Files[0], ".mp3") {
		t.Fatalf("song %+v %v", song, err)
	}
	joined := strings.Join(seen, " ")
	if !strings.Contains(joined, "video:processing") || !strings.Contains(joined, "song:ready") {
		t.Fatalf("progress %s", joined)
	}
}

func TestComposeFallsBack(t *testing.T) {
	if got := Compose(context.Background(), KindImage, "一束花", nil); got != "一束花" {
		t.Fatalf("nil llm %q", got)
	}
	llm := fakeLLM{text: "```json\n{\"prompt\":\"a small bouquet on a table\"}\n```"}
	if got := Compose(context.Background(), KindImage, "花", llm); got != "a small bouquet on a table" {
		t.Fatalf("compose %q", got)
	}
}

type fakeLLM struct{ text string }

func (f fakeLLM) ChatComplete(context.Context, string, string) (string, error) {
	return f.text, nil
}
