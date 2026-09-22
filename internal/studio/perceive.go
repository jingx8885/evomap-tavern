package studio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	KindPicture = "picture"
	KindWatch   = "watch"
	KindListen  = "listen"

	transcribeModel = "fish-transcribe-1"
)

// Percept is what she got from a file: a caption or a transcript, not the bytes.
type Percept struct {
	Kind string
	File string
	Text string
}

// Vision captions one JPEG. The live model already used for camera frames
// implements this; Jev does not.
type Vision interface {
	ChatVision(ctx context.Context, system, user string, jpeg []byte) (string, error)
}

// Perceive looks at a picture, samples a video, or transcribes a song.
// The file must already be a local path. Jev only chose the kind.
func (c *Client) Perceive(ctx context.Context, kind, path string, see Vision) (Percept, error) {
	if c == nil {
		return Percept{}, fmt.Errorf("studio client required")
	}
	path = filepath.Clean(path)
	name := filepath.Base(path)
	switch kind {
	case KindPicture:
		if see == nil {
			return Percept{}, fmt.Errorf("vision required")
		}
		jpg, err := fileJPEG(path)
		if err != nil {
			return Percept{}, err
		}
		text, err := see.ChatVision(ctx, picturePrompt, "What is in this picture?", jpg)
		if err != nil {
			return Percept{}, err
		}
		return Percept{Kind: kind, File: name, Text: clipPublic(text)}, nil
	case KindWatch:
		if see == nil {
			return Percept{}, fmt.Errorf("vision required")
		}
		frames, err := videoFrames(ctx, path)
		if err != nil {
			return Percept{}, err
		}
		var parts []string
		for i, frame := range frames {
			text, err := see.ChatVision(ctx, watchPrompt, fmt.Sprintf("Frame %d of a short clip. What is happening?", i+1), frame)
			if err != nil {
				return Percept{}, err
			}
			if t := strings.TrimSpace(text); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return Percept{}, fmt.Errorf("video had no visible frames")
		}
		return Percept{Kind: kind, File: name, Text: clipPublic(strings.Join(parts, " "))}, nil
	case KindListen:
		text, err := c.Transcribe(ctx, path)
		if err != nil {
			return Percept{}, err
		}
		return Percept{Kind: kind, File: name, Text: clipPublic(text)}, nil
	default:
		return Percept{}, fmt.Errorf("unknown percept kind %q", kind)
	}
}

const (
	picturePrompt = "You caption one still image for a voice companion. One or two short sentences. No lists. Do not invent text that is not visible."
	watchPrompt   = "You caption one frame from a short video for a voice companion. One short sentence about what is happening. No lists."
)

// Resolve picks a file inside dir. hint may name a basename; otherwise the
// newest file of the right kind is used. Paths outside dir are refused.
func Resolve(dir, kind, hint string) (string, error) {
	dir = filepath.Clean(dir)
	if name := namedFile(hint); name != "" {
		full := filepath.Join(dir, name)
		if inside(dir, full) {
			if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() && kindExt(kind, full) {
				return full, nil
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestTime time.Time
	for _, ent := range entries {
		if ent.IsDir() || !kindExt(kind, ent.Name()) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best = ent.Name()
			bestTime = info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s to perceive", kind)
	}
	return filepath.Join(dir, best), nil
}

func namedFile(hint string) string {
	hint = strings.ReplaceAll(hint, "\\", "/")
	for _, part := range strings.FieldsFunc(hint, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '"' || r == '\'' || r == ',' || r == '，' || r == '。'
	}) {
		base := filepath.Base(part)
		if base == "." || base == ".." || strings.Contains(base, "/") {
			continue
		}
		if kindExt(KindPicture, base) || kindExt(KindWatch, base) || kindExt(KindListen, base) {
			return base
		}
	}
	return ""
}

func kindExt(kind, name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch kind {
	case KindPicture:
		return ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif"
	case KindWatch:
		return ext == ".mp4"
	case KindListen:
		return ext == ".mp3" || ext == ".wav" || ext == ".m4a"
	default:
		return false
	}
}

func inside(dir, full string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(full))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileJPEG(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 20<<20 {
		return nil, fmt.Errorf("image too large")
	}
	if bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}) {
		return raw, nil
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("image decode: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func videoFrames(ctx context.Context, path string) ([][]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found, cannot watch video")
	}
	var frames [][]byte
	for _, ss := range []string{"0", "1", "2"} {
		jpg, err := grabFrame(ctx, path, ss)
		if err != nil || len(jpg) < 32 {
			continue
		}
		frames = append(frames, jpg)
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("could not read a frame from the video")
	}
	return frames, nil
}

func grabFrame(ctx context.Context, path, ss string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-ss", ss,
		"-i", path,
		"-frames:v", "1",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Transcribe sends one audio file to new-api Fish speech-to-text.
func (c *Client) Transcribe(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, io.LimitReader(f, 32<<20)); err != nil {
		return "", err
	}
	if err := w.WriteField("model", transcribeModel); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.v1("/audio/transcriptions"), &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", userAgent)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	raw, err := c.do(req, 90*time.Second)
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("transcript: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", fmt.Errorf("empty transcript")
	}
	return strings.TrimSpace(out.Text), nil
}
