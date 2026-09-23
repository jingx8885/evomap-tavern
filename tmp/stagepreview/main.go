package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/window"
)

func main() {
	dir, _ := os.MkdirTemp("", "stagepreview")
	f, _ := os.Create(filepath.Join(dir, "rose.png"))
	img := image.NewRGBA(image.Rect(0, 0, 768, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 768; x++ {
			img.Set(x, y, color.RGBA{uint8(x / 3), uint8(y / 3), 140, 255})
		}
	}
	png.Encode(f, img)
	f.Close()
	st := window.NewStage(window.StageOptions{
		Runner: func(ctx context.Context, kind, prompt string, report func(string, float64)) (string, string, error) {
			switch {
			case strings.Contains(prompt, "fail"):
				return "", "", errors.New("upstream 502: image model busy")
			case strings.Contains(prompt, "slow"):
				for i := 1; i <= 1000; i++ {
					select {
					case <-ctx.Done():
						return "", "", ctx.Err()
					case <-time.After(300 * time.Millisecond):
					}
					report(fmt.Sprintf("drawing step %d", i), float64(i%100)/100)
				}
			case kind == window.KindImage:
				return "rose.png", "", nil
			}
			return "", "今晚先把灯调暗一点，然后放一首慢歌。\n第二行笔记，看看换行。", nil
		},
	})
	reg := window.New(window.Options{MediaDir: dir})
	reg.Register(st)
	url, err := reg.Listen(context.Background(), os.Getenv("ADDR"))
	if err != nil {
		panic(err)
	}
	fmt.Println(url)
	st.Enqueue(window.KindLLM, "evening plan note")
	st.Enqueue(window.KindImage, "a red rose on a wooden desk, warm light, film grain, very long prompt text to see how wrapping works in the card column")
	st.Enqueue(window.KindVideo, "fail this clip")
	st.Enqueue(window.KindImage, "slow painting of the sea")
	st.Enqueue(window.KindSong, "a song queued behind")
	reg.Apply("stage", window.OpOpen, "")
	time.Sleep(500 * time.Millisecond)
	reg.Apply("stage", window.OpFeature, "j2")
	select {}
}
