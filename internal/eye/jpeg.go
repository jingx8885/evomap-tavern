package eye

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"strings"
	"time"
)

const (
	defaultQuality = 55
	thumbSize      = 16
	maxCameraEdge  = 480
	maxScreenEdge  = 720
)

// DecodeDataURL accepts raw JPEG bytes or a data:image/jpeg;base64,... URL.
func DecodeDataURL(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty image")
	}
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(strings.ToLower(s), "data:") {
		s = s[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("image base64: %w", err)
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("image too small")
	}
	return raw, nil
}

// NormalizeFrame resizes and re-encodes so WS and VLM stay cheap.
func NormalizeFrame(source string, raw []byte, maxEdge int) (Frame, error) {
	if maxEdge <= 0 {
		maxEdge = maxEdgeFor(source)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return Frame{}, fmt.Errorf("jpeg: %w", err)
	}
	out, w, h, err := EncodeJPEG(img, maxEdge, defaultQuality)
	if err != nil {
		return Frame{}, err
	}
	resized, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		resized = img
	}
	return Frame{
		Source: source,
		JPEG:   out,
		Width:  w,
		Height: h,
		Thumb:  Thumb(resized, thumbSize),
		At:     time.Now(),
	}, nil
}

func maxEdgeFor(source string) int {
	if source == SourceScreen {
		return maxScreenEdge
	}
	return maxCameraEdge
}

// EncodeJPEG writes a JPEG no wider/taller than maxEdge.
func EncodeJPEG(img image.Image, maxEdge, quality int) ([]byte, int, int, error) {
	if img == nil {
		return nil, 0, 0, fmt.Errorf("nil image")
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("empty image")
	}
	if maxEdge > 0 && (w > maxEdge || h > maxEdge) {
		scale := float64(maxEdge) / float64(w)
		if h > w {
			scale = float64(maxEdge) / float64(h)
		}
		nw := int(float64(w)*scale + 0.5)
		nh := int(float64(h)*scale + 0.5)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		img = resizeNearest(img, nw, nh)
		w, h = nw, nh
	}
	if quality <= 0 {
		quality = defaultQuality
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

func resizeNearest(src image.Image, nw, nh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for y := 0; y < nh; y++ {
		sy := sb.Min.Y + y*sh/nh
		for x := 0; x < nw; x++ {
			sx := sb.Min.X + x*sw/nw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// Thumb is an n×n grayscale patch used for cheap change detection.
func Thumb(img image.Image, n int) []byte {
	if img == nil || n <= 0 {
		return nil
	}
	small := resizeNearest(img, n, n)
	out := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			r, g, b, _ := small.At(x, y).RGBA()
			out[y*n+x] = byte((r*299 + g*587 + b*114) / 1000 >> 8)
		}
	}
	return out
}

// ThumbDelta is mean absolute difference in 0..1.
func ThumbDelta(a, b []byte) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 1
	}
	var sum int
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return float64(sum) / (float64(len(a)) * 255)
}

// SolidJPEG is a test helper: a w×h block of one color.
func SolidJPEG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	raw, _, _, err := EncodeJPEG(img, 0, 80)
	if err != nil {
		return nil
	}
	return raw
}
