// Package window registers her pages as modules.
//
// The shell is the same idea as glazier's registry
// (https://github.com/eg9y/glazier): a window id maps to a component,
// and the shell only opens, focuses, hides, and closes. The module owns
// its model. Jev picks the id and a closed op; Go applies it.
// What the page looks like is a sentence built from that same model,
// the way typesafe-computer-use classifies structured state and lets
// code write the caption (https://github.com/awlevin/typesafe-computer-use).
// Jev does not see pixels and does not write the page.
package window

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	OpNone  = "none"
	OpOpen  = "open"
	OpFocus = "focus"
	OpHide  = "hide"
	OpClose = "close"
)

// Module is one page. Ops are the closed set beyond the shell
// (open / focus / hide / close). Targets are ids Jev may point at.
type Module interface {
	ID() string
	Title() string
	Ops() map[string]string
	Targets() []Target
	Apply(op, target string) error
	Glance(open bool) string
	View() any
	Close()
}

// Target is one closed choice inside a module, usually a job id.
type Target struct {
	ID    string
	Label string
}

type entry struct {
	mod  Module
	open bool
}

// Registry is the desktop: registered modules, one focused page.
type Registry struct {
	mu       sync.Mutex
	order    []string
	mods     map[string]*entry
	focused  string
	mediaDir string
	playDir  string
	url      string
	openFn   func(string) error
	logFn    func(string)
	srv      *http.Server
	page     []byte
}

// Options for the desktop shell.
type Options struct {
	MediaDir string
	// PlayDir holds Codex scratch folders; /play serves pages from it.
	PlayDir string
	Open    func(string) error
	LogFn   func(string)
	Page    []byte
}

// New builds an empty desktop. Register modules, then Listen.
func New(opt Options) *Registry {
	page := opt.Page
	if len(page) == 0 {
		page = mustPage()
	}
	return &Registry{
		mods:     map[string]*entry{},
		mediaDir: opt.MediaDir,
		playDir:  opt.PlayDir,
		openFn:   opt.Open,
		logFn:    opt.LogFn,
		page:     page,
	}
}

// Register adds a module. The first one is focused. Ids must be unique.
func (r *Registry) Register(m Module) {
	if r == nil || m == nil || m.ID() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.mods[m.ID()]; ok {
		return
	}
	r.mods[m.ID()] = &entry{mod: m}
	r.order = append(r.order, m.ID())
	if r.focused == "" {
		r.focused = m.ID()
	}
}

// Listen serves the page on addr (empty means 127.0.0.1:0).
func (r *Registry) Listen(ctx context.Context, addr string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("window registry required")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: r.Handler()}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	url := "http://" + printableAddr(ln.Addr().String())
	r.mu.Lock()
	r.srv = srv
	r.url = url
	r.mu.Unlock()
	if r.logFn != nil {
		r.logFn("stage window: " + url)
	}
	return url, nil
}

// URL is the page address, empty until Listen succeeds.
func (r *Registry) URL() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.url
}

// Close shuts every module. The HTTP server stops with the listen context.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	mods := make([]Module, 0, len(r.order))
	for _, id := range r.order {
		if e := r.mods[id]; e != nil {
			mods = append(mods, e.mod)
		}
	}
	r.mu.Unlock()
	for _, m := range mods {
		m.Close()
	}
}

// Apply runs one closed op. Unknown ops and unknown windows do nothing
// and return an error. Shell ops open, focus, hide, or close the page.
// Any other op is delegated to the module that declared it.
func (r *Registry) Apply(id, op, target string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("window registry required")
	}
	op = strings.TrimSpace(op)
	if op == "" || op == OpNone {
		return r.Glance(), nil
	}
	switch op {
	case OpOpen, OpFocus, OpHide, OpClose:
		if err := r.shell(id, op); err != nil {
			return r.Glance(), err
		}
	default:
		if err := r.delegate(id, op, target); err != nil {
			return r.Glance(), err
		}
	}
	return r.Glance(), nil
}

func (r *Registry) shell(id, op string) error {
	r.mu.Lock()
	id = r.resolveIDLocked(id)
	e := r.mods[id]
	if e == nil {
		r.mu.Unlock()
		return fmt.Errorf("unknown window %q", id)
	}
	launch := false
	switch op {
	case OpOpen, OpFocus:
		e.open = true
		r.focused = id
		launch = r.url != ""
	case OpHide, OpClose:
		e.open = false
	}
	url := r.url
	openFn := r.openFn
	r.mu.Unlock()
	if launch && openFn != nil {
		if err := openFn(url); err != nil && r.logFn != nil {
			r.logFn("stage window open: " + err.Error())
		}
	}
	return nil
}

func (r *Registry) delegate(id, op, target string) error {
	r.mu.Lock()
	if id == "" || id == OpNone {
		id = r.ownerLocked(op)
	}
	id = r.resolveIDLocked(id)
	e := r.mods[id]
	r.mu.Unlock()
	if e == nil {
		return fmt.Errorf("unknown window %q", id)
	}
	if _, ok := e.mod.Ops()[op]; !ok {
		return fmt.Errorf("unknown op %q", op)
	}
	return e.mod.Apply(op, target)
}

func (r *Registry) resolveIDLocked(id string) string {
	id = strings.TrimSpace(id)
	if id != "" && id != OpNone {
		if _, ok := r.mods[id]; ok {
			return id
		}
	}
	if r.focused != "" {
		return r.focused
	}
	if len(r.order) == 1 {
		return r.order[0]
	}
	return id
}

func (r *Registry) ownerLocked(op string) string {
	for _, id := range r.order {
		e := r.mods[id]
		if e == nil {
			continue
		}
		if _, ok := e.mod.Ops()[op]; ok {
			return id
		}
	}
	return r.focused
}

// Spec is the closed set a turn can ask Jev about.
type Spec struct {
	Focused string
	Glance  string
	Modules []ModInfo
	Ops     map[string]string
	Targets []Target
}

// ModInfo is one registered page, as observation.
type ModInfo struct {
	ID    string
	Title string
	Open  bool
}

// Spec snapshots modules, shell ops, and each module's own ops.
func (r *Registry) Spec() Spec {
	if r == nil {
		return Spec{}
	}
	r.mu.Lock()
	out := Spec{
		Focused: r.focused,
		Ops: map[string]string{
			OpNone:  "Leave the page alone.",
			OpOpen:  "Show that module's page.",
			OpFocus: "Bring that page forward.",
			OpHide:  "Cover the page. The queue stays underneath.",
			OpClose: "Cover the page, same as hide.",
		},
	}
	for _, id := range r.order {
		e := r.mods[id]
		if e == nil {
			continue
		}
		out.Modules = append(out.Modules, ModInfo{ID: id, Title: e.mod.Title(), Open: e.open})
		for op, label := range e.mod.Ops() {
			out.Ops[op] = label
		}
		if id == r.focused || (r.focused == "" && len(r.order) == 1) {
			out.Targets = append(out.Targets, e.mod.Targets()...)
		}
	}
	r.mu.Unlock()
	out.Glance = r.Glance()
	return out
}

// Glance is what the focused page looks like right now.
func (r *Registry) Glance() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	id := r.focused
	e := r.mods[id]
	open := false
	if e != nil {
		open = e.open
	}
	r.mu.Unlock()
	if e == nil {
		return "No page module is registered."
	}
	return e.mod.Glance(open)
}

// Open reports whether id's page is uncovered.
func (r *Registry) Open(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.mods[id]
	return e != nil && e.open
}

// Handler serves the desktop page, its JSON, and media files.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.servePage)
	mux.HandleFunc("/api/desktop", r.serveDesktop)
	mux.HandleFunc("/api/feature", r.serveFeature)
	mux.HandleFunc("/media/", r.serveMedia)
	mux.HandleFunc("/play/", r.servePlay)
	return mux
}

// servePlay serves a page Codex wrote, with its scripts and assets.
// The sandbox header gives it an opaque origin, so even opened in its own
// tab it cannot call /api on this server.
func (r *Registry) servePlay(w http.ResponseWriter, req *http.Request) {
	path, ok := r.playPath(strings.TrimPrefix(req.URL.Path, "/play/"))
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-pointer-lock")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, req)
		return
	}
	// ServeFile would redirect .../index.html to the directory.
	http.ServeContent(w, req, st.Name(), st.ModTime(), f)
}

func (r *Registry) playPath(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `\:`) {
		return "", false
	}
	r.mu.Lock()
	dir := r.playDir
	r.mu.Unlock()
	if dir == "" {
		return "", false
	}
	full := filepath.Join(dir, filepath.FromSlash(name))
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return "", false
	}
	return full, true
}

// serveFeature is a click on a card: that job goes to the right frame.
// Only this page may ask; another origin is refused.
func (r *Registry) serveFeature(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if o := req.Header.Get("Origin"); o != "" && o != "http://"+req.Host {
		http.Error(w, "cross-origin", http.StatusForbidden)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if _, err := r.Apply("", OpFeature, body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Registry) servePage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(r.page)
}

func (r *Registry) serveDesktop(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	id := r.focused
	e := r.mods[id]
	open := false
	var title string
	var view any
	if e != nil {
		open = e.open
		title = e.mod.Title()
		view = e.mod.View()
	}
	r.mu.Unlock()
	body := map[string]any{
		"focused": id,
		"open":    open,
		"title":   title,
		"glance":  r.Glance(),
		"view":    view,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (r *Registry) serveMedia(w http.ResponseWriter, req *http.Request) {
	name := strings.TrimPrefix(req.URL.Path, "/media/")
	path, ok := r.mediaPath(name)
	if !ok {
		http.NotFound(w, req)
		return
	}
	http.ServeFile(w, req, path)
}

func (r *Registry) mediaPath(name string) (string, bool) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || strings.Contains(name, "..") {
		return "", false
	}
	r.mu.Lock()
	dir := r.mediaDir
	r.mu.Unlock()
	if dir == "" {
		return "", false
	}
	full := filepath.Join(dir, name)
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return "", false
	}
	rel, err := filepath.Rel(dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return full, true
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

// OpenBrowser launches the page. Tests replace Registry.openFn instead.
func OpenBrowser(url string) error {
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

// SetMediaDir is the only directory /media may read.
func (r *Registry) SetMediaDir(dir string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mediaDir = dir
	r.mu.Unlock()
}
