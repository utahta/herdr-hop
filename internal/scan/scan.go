// Package scan finds git repositories (including linked worktrees) below search paths.
package scan

import (
	"os"
	"path/filepath"
	"sort"
)

// Repo is a scanned checkout.
type Repo struct {
	// Path is normalized (absolute, cleaned, symlinks resolved where possible).
	Path string
	// GitIsFile is true when ".git" is a file, i.e. this is (very likely) a
	// linked worktree rather than a main checkout.
	GitIsFile bool
	// HasWorktrees is true when ".git/worktrees" is non-empty, i.e. this main
	// checkout has (or at least once had) linked worktrees. git keeps all
	// linked-worktree metadata there, so a main checkout without it cannot
	// have any. Always false when GitIsFile is true.
	HasWorktrees bool
}

// Target is one directory to scan and how deep to look below it.
type Target struct {
	Path  string
	Depth int
}

// Repos walks each target up to its depth and returns directories containing
// a ".git" entry (directory or file). Results are deduplicated and sorted by path.
func Repos(targets []Target) []Repo {
	seen := map[string]bool{}
	var out []Repo
	for _, t := range targets {
		walk(t.Path, 0, t.Depth, func(r Repo) {
			r.Path = Normalize(r.Path)
			if !seen[r.Path] {
				seen[r.Path] = true
				out = append(out, r)
			}
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func walk(dir string, level, maxDepth int, found func(Repo)) {
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		found(repoAt(dir, !fi.IsDir()))
		return // do not descend into repositories
	}
	if level >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if e.Name()[0] == '.' {
			// Hidden directories are not traversed, but a hidden directory
			// that is itself a repository (e.g. owner/.github) is a candidate.
			if _, err := os.Lstat(filepath.Join(child, ".git")); err == nil {
				found(repoAt(child, isFile(filepath.Join(child, ".git"))))
			}
			continue
		}
		walk(child, level+1, maxDepth, found)
	}
}

func repoAt(dir string, gitIsFile bool) Repo {
	r := Repo{Path: dir, GitIsFile: gitIsFile}
	if !gitIsFile {
		entries, err := os.ReadDir(filepath.Join(dir, ".git", "worktrees"))
		r.HasWorktrees = err == nil && len(entries) > 0
	}
	return r
}

func isFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && !fi.IsDir()
}

// Normalize returns an absolute, cleaned path with symlinks resolved when possible.
func Normalize(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Clean(p)
}
