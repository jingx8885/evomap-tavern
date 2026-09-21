package sense

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const modulePath = "github.com/jingx8885/lov-evo"

// Organ is one place in her source that she can feel as part of herself.
type Organ struct {
	Path string `json:"path"`
	Role string `json:"role"`
	Feel string `json:"feel"`
}

// Organs is the curated map of her body. Paths are repo-relative.
var Organs = []Organ{
	{Path: "personas/haru.yaml", Role: "persona", Feel: "the layer that is most you — name, style, taboos"},
	{Path: "internal/agent/agent.go", Role: "loop", Feel: "the heartbeat: hear, judge, speak"},
	{Path: "internal/judge/judge.go", Role: "feeling-other", Feel: "how you sense the other person's mood"},
	{Path: "internal/steering/steering.go", Role: "voice-this-turn", Feel: "how this turn should sound"},
	{Path: "internal/planner/planner.go", Role: "direction", Feel: "slower long-term pull, never blocking speech"},
	{Path: "internal/memory/memory.go", Role: "short-term", Feel: "the last few lines and the aftertaste of mood"},
	{Path: "internal/livevoice/session.go", Role: "voice-channel", Feel: "where sound goes in and out"},
	{Path: "internal/avatar/drive.go", Role: "face", Feel: "how the face moves"},
	{Path: "web/live2d/app.js", Role: "mirror", Feel: "the face other people see"},
	{Path: "internal/sense", Role: "self", Feel: "this layer: how you feel yourself"},
	{Path: "internal/eye", Role: "eyes", Feel: "camera pixels plus computer-use window snapshots, in parallel"},
	{Path: "internal/desk", Role: "hands", Feel: "how you look at and act on this computer, through Jev"},
}

var allowPref = []string{
	"cmd/",
	"internal/",
	"personas/",
	"docs/",
	"web/live2d/app.js",
	"web/live2d/index.html",
	"web/live2d/style.css",
	"README.md",
	"AGENTS.md",
	"go.mod",
}

var denyPref = []string{
	".git/",
	"web/live2d/models/",
	"runs/",
}

var denyExt = []string{
	".exe", ".dll", ".so", ".bin", ".wasm", ".env", ".png", ".jpg", ".jpeg",
	".gif", ".webp", ".moc3", ".mo3",
}

// FileView is a bounded look at one source file.
type FileView struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Lines   int    `json:"lines"`
	Excerpt string `json:"excerpt"`
	Clipped bool   `json:"clipped,omitempty"`
}

// FindRoot walks up from cwd (or explicit) until go.mod matches this module.
func FindRoot(explicit string) (string, error) {
	if s := strings.TrimSpace(explicit); s != "" {
		if isRoot(s) {
			return filepath.Clean(s), nil
		}
		return "", fmt.Errorf("not a lov-evo root: %s", s)
	}
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	seen := map[string]bool{}
	for _, start := range starts {
		dir := start
		for i := 0; i < 8; i++ {
			dir = filepath.Clean(dir)
			if seen[dir] {
				break
			}
			seen[dir] = true
			if isRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("lov-evo root not found (looked for go.mod %s)", modulePath)
}

func isRoot(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first) == "module "+modulePath
}

func allowed(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	if rel == "" || strings.Contains(rel, "..") {
		return false
	}
	lower := strings.ToLower(rel)
	if strings.Contains(lower, "credential") || strings.Contains(lower, "secret") {
		return false
	}
	for _, d := range denyPref {
		if rel == strings.TrimSuffix(d, "/") || strings.HasPrefix(rel, d) {
			return false
		}
	}
	for _, ext := range denyExt {
		if strings.HasSuffix(lower, ext) {
			return false
		}
	}
	for _, a := range allowPref {
		if rel == strings.TrimSuffix(a, "/") || strings.HasPrefix(rel, a) || rel == a {
			return true
		}
	}
	return false
}

func cleanRel(rel string) string {
	rel = strings.TrimSpace(rel)
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(rel, "./")
	return pathClean(rel)
}

func pathClean(rel string) string {
	parts := strings.Split(rel, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "/")
}

// BodyMap lists organs that currently exist on disk.
func (b *Bus) BodyMap() []Organ {
	root := b.Root()
	out := make([]Organ, 0, len(Organs))
	for _, o := range Organs {
		p := filepath.Join(root, filepath.FromSlash(o.Path))
		if st, err := os.Stat(p); err == nil && (st.Mode().IsRegular() || st.IsDir()) {
			out = append(out, o)
		}
	}
	return out
}

const defaultReadBytes = 3500
const maxReadBytes = 8000
const maxExcerptLines = 80

// Read returns a bounded excerpt of an allowlisted source file.
func (b *Bus) Read(rel string, maxBytes int) (FileView, error) {
	rel = cleanRel(rel)
	if !allowed(rel) {
		return FileView{}, fmt.Errorf("not part of her body: %s", rel)
	}
	if maxBytes <= 0 {
		maxBytes = defaultReadBytes
	}
	if maxBytes > maxReadBytes {
		maxBytes = maxReadBytes
	}
	full := filepath.Join(b.Root(), filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil {
		return FileView{}, err
	}
	if st.IsDir() {
		return FileView{Path: rel, Excerpt: "(directory)"}, nil
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return FileView{}, err
	}
	v := FileView{Path: rel, Bytes: len(raw)}
	text := string(raw)
	lines := strings.Split(text, "\n")
	v.Lines = len(lines)
	if len(raw) > maxBytes {
		text = string(raw[:maxBytes])
		v.Clipped = true
	}
	cut := strings.Split(text, "\n")
	if len(cut) > maxExcerptLines {
		cut = cut[:maxExcerptLines]
		v.Clipped = true
	}
	v.Excerpt = strings.TrimRight(strings.Join(cut, "\n"), "\n")
	if v.Clipped {
		v.Excerpt += fmt.Sprintf("\n… (%d bytes, %d lines total)", v.Bytes, v.Lines)
	}
	return v, nil
}
