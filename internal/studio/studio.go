// Package studio calls the same new-api media routes as akasha-grimoire:
// Grok image and video, Fish speech, and Suno songs. Go executes; Jev only
// picks the kind. Progress is reported so she can feel the job.
package studio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	KindImage  = "image"
	KindVideo  = "video"
	KindSpeech = "speech"
	KindSong   = "song"

	userAgent = "akasha-lov-evo-studio/1.0"

	defaultImageModel  = "grok-imagine-image"
	defaultVideoModel  = "grok-imagine-video"
	defaultSpeechModel = "fish-s2.1-pro"
	// warm-friendly-female from the grimoire Fish voice library.
	defaultSpeechVoice = "faccba1a8ac54016bcfc02761285e67f"
	defaultSongModel   = "V5_5"
)

// Progress is one felt beat of a media job. Ratio is 0–1.
type Progress struct {
	Kind   string
	Status string
	Ratio  float64
}

// Result is a finished job. Files are base names, not remote URLs.
type Result struct {
	Kind   string
	Status string
	Files  []string
}

// Client talks to one new-api gateway.
type Client struct {
	BaseURL      string
	APIKey       string
	OutDir       string
	ImageModel   string
	VideoModel   string
	SpeechModel  string
	SpeechVoice  string
	SongModel    string
	HTTP         *http.Client
	PollEvery    time.Duration
	VideoTimeout time.Duration
	SongTimeout  time.Duration
	OnProgress   func(Progress)
}

// New builds a client. Empty models use the grimoire defaults.
func New(baseURL, apiKey, outDir string) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:       apiKey,
		OutDir:       outDir,
		ImageModel:   defaultImageModel,
		VideoModel:   defaultVideoModel,
		SpeechModel:  defaultSpeechModel,
		SpeechVoice:  defaultSpeechVoice,
		SongModel:    defaultSongModel,
		PollEvery:    4 * time.Second,
		VideoTimeout: 4 * time.Minute,
		SongTimeout:  8 * time.Minute,
	}
}

// Run generates one artifact and writes it under OutDir.
func (c *Client) Run(ctx context.Context, kind, prompt string) (Result, error) {
	if c == nil {
		return Result{}, fmt.Errorf("studio client required")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Result{}, fmt.Errorf("prompt required")
	}
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		return Result{}, err
	}
	switch kind {
	case KindImage:
		return c.image(ctx, prompt)
	case KindVideo:
		return c.video(ctx, prompt)
	case KindSpeech:
		return c.speech(ctx, prompt)
	case KindSong:
		return c.song(ctx, prompt)
	default:
		return Result{}, fmt.Errorf("unknown studio kind %q", kind)
	}
}

func (c *Client) image(ctx context.Context, prompt string) (Result, error) {
	c.progress(KindImage, "submitting", 0.15)
	body := map[string]any{
		"model":           c.ImageModel,
		"prompt":          prompt,
		"n":               1,
		"response_format": "b64_json",
	}
	var resp struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, c.v1("/images/generations"), body, &resp, 90*time.Second); err != nil {
		return Result{}, err
	}
	if len(resp.Data) == 0 {
		return Result{}, fmt.Errorf("image response had no data")
	}
	c.progress(KindImage, "saving", 0.7)
	item := resp.Data[0]
	var raw []byte
	var err error
	if item.B64 != "" {
		raw, err = base64.StdEncoding.DecodeString(item.B64)
	} else if item.URL != "" {
		raw, err = c.download(ctx, item.URL)
	} else {
		err = fmt.Errorf("image response had neither b64_json nor url")
	}
	if err != nil {
		return Result{}, err
	}
	name, err := c.write(KindImage, raw)
	if err != nil {
		return Result{}, err
	}
	c.progress(KindImage, "ready", 1)
	return Result{Kind: KindImage, Status: "ready", Files: []string{name}}, nil
}

func (c *Client) speech(ctx context.Context, prompt string) (Result, error) {
	c.progress(KindSpeech, "submitting", 0.4)
	body := map[string]any{
		"model":           c.SpeechModel,
		"input":           prompt,
		"voice":           c.SpeechVoice,
		"response_format": "mp3",
	}
	raw, err := c.postBytes(ctx, c.v1("/audio/speech"), body, 60*time.Second)
	if err != nil {
		return Result{}, err
	}
	if len(raw) < 32 || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return Result{}, fmt.Errorf("speech response was not audio")
	}
	name, err := c.write(KindSpeech, raw)
	if err != nil {
		return Result{}, err
	}
	c.progress(KindSpeech, "ready", 1)
	return Result{Kind: KindSpeech, Status: "ready", Files: []string{name}}, nil
}

func (c *Client) video(ctx context.Context, prompt string) (Result, error) {
	c.progress(KindVideo, "submitting", 0.1)
	body := map[string]any{
		"model":    c.VideoModel,
		"prompt":   prompt,
		"duration": 4,
	}
	var created map[string]any
	if err := c.postJSON(ctx, c.v1("/videos/generations"), body, &created, 60*time.Second); err != nil {
		return Result{}, err
	}
	id := firstString(created, "id", "request_id", "task_id")
	if id == "" {
		return Result{}, fmt.Errorf("video submit had no task id")
	}
	ctx, cancel := context.WithTimeout(ctx, c.VideoTimeout)
	defer cancel()
	return c.pollMedia(ctx, KindVideo, c.v1("/videos/"+url.PathEscape(id)), c.v1("/videos/"+url.PathEscape(id)+"/content"))
}

func (c *Client) song(ctx context.Context, prompt string) (Result, error) {
	c.progress(KindSong, "submitting", 0.1)
	body := map[string]any{
		"gpt_description_prompt": prompt,
		"mv":                     c.SongModel,
		"make_instrumental":      instrumental(prompt),
	}
	var created map[string]any
	if err := c.postJSON(ctx, c.host("/suno/submit/MUSIC"), body, &created, 60*time.Second); err != nil {
		return Result{}, err
	}
	id := taskID(created)
	if id == "" {
		return Result{}, fmt.Errorf("song submit had no task id")
	}
	ctx, cancel := context.WithTimeout(ctx, c.SongTimeout)
	defer cancel()
	deadline := time.Now().Add(c.SongTimeout)
	every := c.PollEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		var fetched map[string]any
		if err := c.getJSON(ctx, c.host("/suno/fetch/"+url.PathEscape(id)), &fetched, 30*time.Second); err != nil {
			return Result{}, err
		}
		task := unwrapTask(fetched)
		status := strings.ToUpper(firstString(task, "status"))
		switch status {
		case "SUCCESS":
			c.progress(KindSong, "saving", 0.9)
			clips := songClips(task["data"])
			if len(clips) == 0 {
				return Result{}, fmt.Errorf("song finished without audio")
			}
			raw, err := c.download(ctx, clips[0])
			if err != nil {
				return Result{}, err
			}
			name, err := c.write(KindSong, raw)
			if err != nil {
				return Result{}, err
			}
			c.progress(KindSong, "ready", 1)
			return Result{Kind: KindSong, Status: "ready", Files: []string{name}}, nil
		case "FAILURE", "FAILED":
			reason := firstString(task, "fail_reason")
			if reason == "" {
				reason = "song failed"
			}
			return Result{}, fmt.Errorf("%s", clipPublic(reason))
		default:
			c.progress(KindSong, strings.ToLower(or(status, "queued")), songRatio(status))
		}
		if time.Now().After(deadline) {
			return Result{}, fmt.Errorf("song timed out")
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(every):
		}
	}
}

func (c *Client) pollMedia(ctx context.Context, kind, statusURL, contentURL string) (Result, error) {
	every := c.PollEvery
	if every <= 0 {
		every = 4 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		var task map[string]any
		if err := c.getJSON(ctx, statusURL, &task, 30*time.Second); err != nil {
			return Result{}, err
		}
		status := strings.ToLower(firstString(task, "status"))
		switch status {
		case "completed", "succeeded", "success":
			c.progress(kind, "saving", 0.9)
			raw, err := c.getBytes(ctx, contentURL, 90*time.Second)
			if err != nil {
				return Result{}, err
			}
			if len(raw) < 16 || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<")) {
				return Result{}, fmt.Errorf("%s content was not media", kind)
			}
			name, err := c.write(kind, raw)
			if err != nil {
				return Result{}, err
			}
			c.progress(kind, "ready", 1)
			return Result{Kind: kind, Status: "ready", Files: []string{name}}, nil
		case "failed", "failure", "canceled", "cancelled":
			return Result{}, fmt.Errorf("%s failed", kind)
		default:
			c.progress(kind, or(status, "queued"), ratioOf(task, status))
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(every):
		}
	}
}

func (c *Client) progress(kind, status string, ratio float64) {
	if c.OnProgress == nil {
		return
	}
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	c.OnProgress(Progress{Kind: kind, Status: status, Ratio: ratio})
}

func (c *Client) write(kind string, raw []byte) (string, error) {
	ext := extOf(kind, raw)
	name := kind + "-" + time.Now().Format("20060102-150405") + ext
	path := filepath.Join(c.OutDir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// Missing lists "kind(model)" for each media model the gateway does not offer.
func (c *Client) Missing(ctx context.Context) ([]string, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.v1("/models"), &resp, 20*time.Second); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, m := range resp.Data {
		have[m.ID] = true
	}
	var out []string
	for _, km := range [][2]string{
		{KindImage, c.ImageModel}, {KindVideo, c.VideoModel},
		{KindSpeech, c.SpeechModel}, {KindSong, c.SongModel},
	} {
		if km[1] != "" && !have[km[1]] {
			out = append(out, km[0]+"("+km[1]+")")
		}
	}
	return out, nil
}

func (c *Client) v1(path string) string   { return c.join(path, false) }
func (c *Client) host(path string) string { return c.join(path, true) }

func (c *Client) join(path string, stripV1 bool) string {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		base := strings.TrimRight(c.BaseURL, "/")
		if stripV1 {
			base = strings.TrimSuffix(base, "/v1")
		}
		return base + path
	}
	p := strings.TrimRight(u.Path, "/")
	if stripV1 && strings.HasSuffix(p, "/v1") {
		p = strings.TrimSuffix(p, "/v1")
	}
	if !stripV1 && p != "" && !strings.HasSuffix(p, "/v1") {
		p += "/v1"
	}
	u.Path = p + path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (c *Client) postJSON(ctx context.Context, endpoint string, body any, dest any, timeout time.Duration) error {
	raw, err := c.postBytes(ctx, endpoint, body, timeout)
	if err != nil {
		return err
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("studio response: %w", err)
	}
	return nil
}

func (c *Client) postBytes(ctx context.Context, endpoint string, body any, timeout time.Duration) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.do(req, timeout)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, dest any, timeout time.Duration) error {
	raw, err := c.getBytes(ctx, endpoint, timeout)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("studio response: %w", err)
	}
	return nil
}

func (c *Client) getBytes(ctx context.Context, endpoint string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.do(req, timeout)
}

func (c *Client) download(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("refusing non-http media url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return c.do(req, 90*time.Second)
}

func (c *Client) do(req *http.Request, timeout time.Duration) ([]byte, error) {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		req = req.Clone(ctx)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("studio http %d: %s", res.StatusCode, clipPublic(string(raw)))
	}
	return raw, nil
}

func taskID(m map[string]any) string {
	if id := firstString(m, "id", "task_id"); id != "" {
		return id
	}
	if s, ok := m["data"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func unwrapTask(m map[string]any) map[string]any {
	if data, ok := m["data"].(map[string]any); ok {
		if _, has := data["status"]; has {
			return data
		}
	}
	return m
}

func songClips(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if u := firstString(obj, "audio_url"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func ratioOf(task map[string]any, status string) float64 {
	switch v := task["progress"].(type) {
	case float64:
		if v > 1 {
			return v / 100
		}
		if v > 0 {
			return v
		}
	}
	switch status {
	case "processing", "in_progress":
		return 0.55
	case "queued", "submitted", "":
		return 0.15
	default:
		return 0.3
	}
}

func songRatio(status string) float64 {
	switch status {
	case "IN_PROGRESS":
		return 0.55
	case "QUEUED", "SUBMITTED", "":
		return 0.15
	default:
		return 0.3
	}
}

func instrumental(prompt string) bool {
	p := strings.ToLower(prompt)
	return strings.Contains(p, "instrumental") || strings.Contains(prompt, "纯音乐") || strings.Contains(p, "no vocal")
}

func extOf(kind string, raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}):
		return ".png"
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		return ".jpg"
	case bytes.HasPrefix(raw, []byte("GIF8")):
		return ".gif"
	case bytes.HasPrefix(raw, []byte("RIFF")) && bytes.Contains(raw[:min(16, len(raw))], []byte("WEBP")):
		return ".webp"
	case len(raw) >= 12 && bytes.Contains(raw[:12], []byte("ftyp")):
		return ".mp4"
	case kind == KindSpeech || kind == KindSong:
		return ".mp3"
	default:
		return ".bin"
	}
}

func clipPublic(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 180 {
		s = s[:180]
	}
	return s
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
