package avatar

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jingx8885/lov-evo/internal/judge"
	"github.com/jingx8885/lov-evo/internal/memory"
)

const modelRel = "models/Haru/Haru.model3.json"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Hub broadcasts drive frames to Live2D viewers.
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]*client
	last    Frame
	mouth   atomic.Uint64
	mouthAt atomic.Int64
	senseFn atomic.Value // func() any
	eyeFn   atomic.Value // func(source string, jpegDataURL string)
}

type client struct {
	frames chan Frame
	pcm    chan []byte
}

// NewHub creates an empty hub with a default idle frame.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]*client),
		last:    Drive("continue", &judge.Judgment{Emotion: "neutral", Engagement: 0.6}, memory.Affect{}),
	}
}

// Last returns the most recently published frame.
func (h *Hub) Last() Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// Publish sends a frame to every connected viewer.
func (h *Hub) Publish(f Frame) {
	if f.Type == "" {
		f.Type = "drive"
	}
	h.mu.Lock()
	h.last = f
	var chans []chan Frame
	for _, c := range h.clients {
		chans = append(chans, c.frames)
	}
	h.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- f:
		default:
		}
	}
}

const mouthHold = 320 * time.Millisecond

// Mouth publishes a 0..1 lip-sync value. The WS writer coalesces at ~20Hz.
func (h *Hub) Mouth(v float64) {
	h.mouth.Store(math.Float64bits(clamp01(v)))
	h.mouthAt.Store(time.Now().UnixNano())
}

// MouthValue is the latest lip-sync openness. It decays to 0 if no
// PCM has arrived recently so the mouth does not freeze half-open.
func (h *Hub) MouthValue() float64 {
	t := h.mouthAt.Load()
	if t != 0 && time.Since(time.Unix(0, t)) > mouthHold {
		return 0
	}
	return math.Float64frombits(h.mouth.Load())
}

// SetSense registers GET /api/sense payload (her current self-snapshot).
func (h *Hub) SetSense(fn func() any) {
	if h == nil {
		return
	}
	h.senseFn.Store(fn)
}

// SetEye registers a handler for camera/screen JPEGs from the viewer.
func (h *Hub) SetEye(fn func(source, dataURL string)) {
	if h == nil {
		return
	}
	h.eyeFn.Store(fn)
}

// PCM pushes a downlink s16le 24kHz chunk to every viewer for WebAudio playback.
func (h *Hub) PCM(p []byte) {
	if len(p) == 0 {
		return
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	h.mu.Lock()
	var chans []chan []byte
	for _, c := range h.clients {
		chans = append(chans, c.pcm)
	}
	h.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- cp:
		default:
		}
	}
}

// Handler serves the viewer, model files, WebSocket, and POST /drive.
func (h *Hub) Handler(dir string) http.Handler {
	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir(dir))
	mux.Handle("/", fs)
	mux.HandleFunc("/ws", h.serveWS)
	mux.HandleFunc("/api/last", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Last())
	})
	mux.HandleFunc("/api/sense", h.serveSense)
	mux.HandleFunc("/api/eye", h.serveEye)
	mux.HandleFunc("/drive", h.serveDrive)
	return mux
}

func (h *Hub) serveSense(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fn, _ := h.senseFn.Load().(func() any)
	if fn == nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false})
		return
	}
	json.NewEncoder(w).Encode(fn())
}

func (h *Hub) serveEye(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		Source string `json:"source"`
		Data   string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.dispatchEye(in.Source, in.Data) {
		http.Error(w, "eye handler missing", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (h *Hub) dispatchEye(source, dataURL string) bool {
	fn, _ := h.eyeFn.Load().(func(string, string))
	if fn == nil || strings.TrimSpace(dataURL) == "" {
		return fn != nil && strings.TrimSpace(dataURL) != ""
	}
	fn(source, dataURL)
	return true
}

func (h *Hub) serveDrive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in Frame
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	j := &judge.Judgment{
		UserText:    in.UserText,
		Valence:     in.Valence,
		Arousal:     in.Arousal,
		Emotion:     in.Emotion,
		SelfEmotion: in.SelfEmotion,
		Engagement:  in.Engagement,
		SafetyP:     in.SafetyP,
		NeedLLMP:    in.NeedLLM,
	}
	if j.Emotion == "" {
		j.Emotion = "neutral"
	}
	f := Drive(in.Mode, j, memory.Affect{Valence: in.Valence, Arousal: in.Arousal, Emotion: j.Emotion})
	if in.RelationshipStage != "" {
		f.RelationshipStage = in.RelationshipStage
		f.Bond = in.Bond
	}
	if in.Expression != "" {
		f.Expression = in.Expression
	}
	if in.MotionGroup != "" {
		f.MotionGroup = in.MotionGroup
		f.MotionIndex = in.MotionIndex
	}
	h.Publish(f)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f)
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	cli := &client{
		frames: make(chan Frame, 8),
		pcm:    make(chan []byte, 64),
	}
	h.mu.Lock()
	h.clients[conn] = cli
	last := h.last
	h.mu.Unlock()
	go h.readEye(conn)
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, conn)
			h.mu.Unlock()
			conn.Close()
		}()
		_ = conn.WriteJSON(last)
		ping := time.NewTicker(20 * time.Second)
		mouthTick := time.NewTicker(50 * time.Millisecond)
		defer ping.Stop()
		defer mouthTick.Stop()
		lastMouth := -1.0
		for {
			select {
			case f, ok := <-cli.frames:
				if !ok {
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(f); err != nil {
					return
				}
			case chunk := <-cli.pcm:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					return
				}
			case <-mouthTick.C:
				v := h.MouthValue()
				// Keep sending while the mouth is open: the viewer times out
				// if a held syllable looks unchanged for ~400ms.
				if math.Abs(v-lastMouth) < 0.015 && v < 0.01 && lastMouth >= 0 {
					continue
				}
				lastMouth = v
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(map[string]any{"type": "lipsync", "mouth": v}); err != nil {
					return
				}
			case <-ping.C:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()
}

func (h *Hub) readEye(conn *websocket.Conn) {
	conn.SetReadLimit(1 << 20)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Data   string `json:"data"`
		}
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		if in.Type != "eye" {
			continue
		}
		h.dispatchEye(in.Source, in.Data)
	}
}

// Options for Listen.
type ListenOptions struct {
	Addr      string
	Dir       string
	Open      bool
	LogFn     func(string)
	IdleFrame *Frame
}

// Listen starts HTTP+WS on addr. Cancelling ctx shuts the server down.
func Listen(ctx context.Context, opt ListenOptions) (*Hub, string, error) {
	dir, err := FindDir(opt.Dir)
	if err != nil {
		return nil, "", err
	}
	addr := opt.Addr
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	h := NewHub()
	if opt.IdleFrame != nil {
		h.Publish(*opt.IdleFrame)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	srv := &http.Server{Handler: h.Handler(dir)}
	go func() {
		_ = srv.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	url := "http://" + printableAddr(ln.Addr().String())
	if opt.LogFn != nil {
		opt.LogFn(fmt.Sprintf("live2d viewer: %s  (ws %s/ws)", url, url))
	}
	if opt.Open {
		_ = openBrowser(url)
	}
	return h, url, nil
}

func printableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// FindDir locates web/live2d containing the Haru model.
func FindDir(explicit string) (string, error) {
	if explicit != "" {
		d := filepath.Clean(explicit)
		if _, err := os.Stat(filepath.Join(d, modelRel)); err != nil {
			return "", fmt.Errorf("Live2D Haru model not found in %s (%s)", d, modelRel)
		}
		return d, nil
	}
	var cands []string
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "web", "live2d"))
		dir := wd
		for i := 0; i < 6; i++ {
			cands = append(cands, filepath.Join(dir, "web", "live2d"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "web", "live2d"))
	}
	seen := map[string]bool{}
	for _, d := range cands {
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, err := os.Stat(filepath.Join(d, modelRel)); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("Live2D Haru model not found (looked for %s under web/live2d)", modelRel)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
