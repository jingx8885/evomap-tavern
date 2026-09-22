package react

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxDirty = 200

// treeSnap is the dirty working tree before a change, so a step that
// touches anything except the chosen file can be put back.
type treeSnap struct {
	root  string
	dirty map[string][]byte
}

func takeSnap(ctx context.Context, root string) (treeSnap, error) {
	names, err := gitDirty(ctx, root)
	if err != nil {
		return treeSnap{}, err
	}
	if len(names) > maxDirty {
		return treeSnap{}, fmt.Errorf("working tree has %d dirty paths; refusing to edit", len(names))
	}
	s := treeSnap{root: root, dirty: map[string][]byte{}}
	for _, name := range names {
		full, err := safeJoin(root, name)
		if err != nil {
			return treeSnap{}, err
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				s.dirty[name] = nil
				continue
			}
			return treeSnap{}, err
		}
		s.dirty[name] = raw
	}
	return s, nil
}

// restoreExcept puts every path except keep back to the snapshot.
// left lists paths that still differ.
func (s treeSnap) restoreExcept(ctx context.Context, keep string) (reverted, left []string) {
	if s.root == "" {
		return nil, []string{"no snapshot"}
	}
	names, err := gitDirty(ctx, s.root)
	if err != nil {
		return nil, []string{err.Error()}
	}
	for _, name := range names {
		if name == keep {
			continue
		}
		full, err := safeJoin(s.root, name)
		if err != nil {
			left = append(left, name)
			continue
		}
		cur, readErr := os.ReadFile(full)
		missing := os.IsNotExist(readErr)
		prev, was := s.dirty[name]
		if was {
			if !missing && bytes.Equal(cur, prev) {
				continue
			}
			if prev == nil {
				_ = os.Remove(full)
			} else if err := os.WriteFile(full, prev, 0o644); err != nil {
				left = append(left, name)
				continue
			}
			reverted = append(reverted, name)
			continue
		}
		if missing {
			continue
		}
		if err := restoreClean(ctx, s.root, name); err != nil {
			left = append(left, name)
			continue
		}
		reverted = append(reverted, name)
	}
	left = append(left, stillStrange(ctx, s, keep)...)
	return reverted, left
}

func stillStrange(ctx context.Context, s treeSnap, keep string) []string {
	names, err := gitDirty(ctx, s.root)
	if err != nil {
		return []string{err.Error()}
	}
	var left []string
	for _, name := range names {
		if name == keep {
			continue
		}
		full, err := safeJoin(s.root, name)
		if err != nil {
			left = append(left, name)
			continue
		}
		cur, readErr := os.ReadFile(full)
		prev, was := s.dirty[name]
		if was {
			if readErr == nil && bytes.Equal(cur, prev) {
				continue
			}
			left = append(left, name)
			continue
		}
		if os.IsNotExist(readErr) {
			continue
		}
		left = append(left, name)
	}
	return left
}

func gitDirty(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	return parsePorcelain(out), nil
}

func parsePorcelain(out []byte) []string {
	var names []string
	for len(out) > 0 {
		i := bytes.IndexByte(out, 0)
		if i < 0 {
			break
		}
		entry := string(out[:i])
		out = out[i+1:]
		if len(entry) < 4 {
			continue
		}
		status := entry[:2]
		name := strings.TrimSpace(entry[3:])
		if status[0] == 'R' || status[0] == 'C' {
			j := bytes.IndexByte(out, 0)
			if j < 0 {
				break
			}
			name = string(out[:j])
			out = out[j+1:]
		}
		name = filepath.ToSlash(name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func restoreClean(ctx context.Context, root, rel string) error {
	full, err := safeJoin(root, rel)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "checkout", "--", rel)
	if err := cmd.Run(); err != nil {
		return os.Remove(full)
	}
	return nil
}

func safeJoin(root, rel string) (string, error) {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes the repo: %s", rel)
	}
	full := filepath.Join(root, rel)
	root = filepath.Clean(root)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the repo: %s", rel)
	}
	return full, nil
}

func readFile(root, rel string) ([]byte, bool, error) {
	full, err := safeJoin(root, rel)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}
