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

const logCap = 200

// LogLine is one operator-facing trace line for the viewer.
type LogLine struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Hub broadcasts drive frames to Live2D viewers.
type Hub struct {
	mu       sync.Mutex
	clients  map[*websocket.Conn]*client
	last     Frame
	logs     []LogLine
	mouth    atomic.Uint64
	mouthAt  atomic.Int64
	senseFn  atomic.Value // func() any
	queueFn  atomic.Value // func() any
	eyeFn    atomic.Value // func(source string, jpegDataURL string)
	sysGet   func() bool
	sysSet   func(bool)
	memGet   func() memory.MemoryView
	memApply func(memory.MemoryOp) (memory.MemoryView, error)
}

type client struct {
	frames  chan Frame
	pcm     chan []byte
	logs    chan LogLine
	capture chan struct{}
	shot    chan struct{}
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

// Log keeps an operator trace and pushes it to connected viewers.
// Routine noise should be filtered before this call.
func (h *Hub) Log(line string) {
	if h == nil {
		return
	}
	line = clipLog(line)
	if line == "" {
		return
	}
	entry := LogLine{At: time.Now(), Text: line}
	h.mu.Lock()
	h.logs = append(h.logs, entry)
	if len(h.logs) > logCap {
		h.logs = h.logs[len(h.logs)-logCap:]
	}
	var chans []chan LogLine
	for _, c := range h.clients {
		chans = append(chans, c.logs)
	}
	h.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- entry:
		default:
		}
	}
}

// RecentLogs returns the trace, oldest first.
func (h *Hub) RecentLogs() []LogLine {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]LogLine, len(h.logs))
	copy(out, h.logs)
	return out
}

func clipLog(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) > 280 {
		return string(r[:280]) + "…"
	}
	return s
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

// SetQueue registers GET /api/queue. The payload is the stage queue
// the companion viewer paints; nil means the queue is not wired.
func (h *Hub) SetQueue(fn func() any) {
	if h == nil || fn == nil {
		return
	}
	h.queueFn.Store(fn)
}

// SetMemory registers the durable-memory viewer. get lists what she
// remembers; apply adds, edits, or deletes one line.
func (h *Hub) SetMemory(get func() memory.MemoryView, apply func(memory.MemoryOp) (memory.MemoryView, error)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.memGet = get
	h.memApply = apply
	h.mu.Unlock()
}

// SetSystem registers the run switch. get reports whether voice and
// model work are live; set turns that whole run on or off.
func (h *Hub) SetSystem(get func() bool, set func(bool)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.sysGet = get
	h.sysSet = set
	h.mu.Unlock()
}

// RequestCapture asks every viewer for one camera JPEG. There is no timer.
func (h *Hub) RequestCapture() {
	h.askViewers(func(c *client) chan struct{} { return c.capture })
}

// RequestShot asks every viewer for one screenshot of her own face.
func (h *Hub) RequestShot() {
	h.askViewers(func(c *client) chan struct{} { return c.shot })
}

func (h *Hub) askViewers(pick func(*client) chan struct{}) {
	if h == nil || pick == nil {
		return
	}
	h.mu.Lock()
	var chans []chan struct{}
	for _, c := range h.clients {
		if ch := pick(c); ch != nil {
			chans = append(chans, ch)
		}
	}
	h.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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
	mux.HandleFunc("/api/log", h.serveLog)
	mux.HandleFunc("/api/eye", h.serveEye)
	mux.HandleFunc("/api/system", h.serveSystem)
	mux.HandleFunc("/api/memory", h.serveMemory)
	mux.HandleFunc("/api/queue", h.serveQueue)
	mux.HandleFunc("/drive", h.serveDrive)
	return mux
}

func (h *Hub) systemState(set *bool) (bool, bool) {
	if h == nil {
		return false, false
	}
	h.mu.Lock()
	get, setFn := h.sysGet, h.sysSet
	h.mu.Unlock()
	if get == nil || setFn == nil {
		return false, false
	}
	if set != nil {
		setFn(*set)
	}
	return get(), true
}

func (h *Hub) serveSystem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		on, ok := h.systemState(nil)
		json.NewEncoder(w).Encode(map[string]any{"on": on, "available": ok})
	case http.MethodPost:
		var in struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		on, ok := h.systemState(&in.On)
		if !ok {
			http.Error(w, "system down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"on": on, "available": true})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) serveMemory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	h.mu.Lock()
	get, apply := h.memGet, h.memApply
	h.mu.Unlock()
	if get == nil {
		json.NewEncoder(w).Encode(memory.MemoryView{Items: []memory.MemoryItem{}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(get())
	case http.MethodPost:
		if apply == nil {
			http.Error(w, "memory down", http.StatusServiceUnavailable)
			return
		}
		var op memory.MemoryOp
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&op); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"available": true, "error": "请求读不懂"})
			return
		}
		view, err := apply(op)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"available": true, "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(view)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) serveQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fn, _ := h.queueFn.Load().(func() any)
	if fn == nil {
		json.NewEncoder(w).Encode(map[string]any{"available": false, "jobs": []any{}})
		return
	}
	json.NewEncoder(w).Encode(fn())
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

func (h *Hub) serveLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"lines": h.RecentLogs()})
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
		frames:  make(chan Frame, 8),
		pcm:     make(chan []byte, 64),
		logs:    make(chan LogLine, 128),
		capture: make(chan struct{}, 1),
		shot:    make(chan struct{}, 1),
	}
	h.mu.Lock()
	h.clients[conn] = cli
	last := h.last
	backlog := append([]LogLine(nil), h.logs...)
	h.mu.Unlock()
	go h.readEye(conn)
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, conn)
			h.mu.Unlock()
			conn.Close()
		}()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(last); err != nil {
			return
		}
		for _, line := range backlog {
			if err := writeLog(conn, line); err != nil {
				return
			}
		}
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
			case line := <-cli.logs:
				if err := writeLog(conn, line); err != nil {
					return
				}
			case <-cli.capture:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(map[string]any{"type": "capture"}); err != nil {
					return
				}
			case <-cli.shot:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(map[string]any{"type": "shot"}); err != nil {
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

func writeLog(conn *websocket.Conn, line LogLine) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(map[string]any{
		"type": "log",
		"at":   line.At.Format(time.RFC3339Nano),
		"text": line.Text,
	})
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
