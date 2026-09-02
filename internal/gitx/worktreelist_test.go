package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeListPorcelain(t *testing.T) {
	dir := initRepo(t)
	g := New()

	// Only the main checkout.
	l, err := g.WorktreeList(dir)
	if err != nil || len(l.Worktrees) != 1 || l.RepoRoot != mustReal(t, dir) || l.Worktrees[0].IsLinked {
		t.Fatalf("main only: %+v err=%v", l, err)
	}

	// git prints symlink-resolved worktree paths (macOS TempDir lives under
	// /var -> /private/var), so resolve the base before building paths.
	wts := mustReal(t, t.TempDir())
	feat := filepath.Join(wts, "feat")
	detached := filepath.Join(wts, "detached")
	// A worktree whose path contains a newline: the reason -z (and therefore
	// Git 2.36+) is required. Without it a newline-delimited parser would
	// still pass this test suite.
	newline := filepath.Join(wts, "with\nnewline")
	gone := filepath.Join(wts, "gone")
	for _, args := range [][]string{
		{"worktree", "add", "-q", "-b", "feat", feat},
		{"worktree", "add", "-q", "--detach", detached},
		{"worktree", "add", "-q", "-b", "nl", newline},
		{"worktree", "add", "-q", "-b", "gone", gone},
	} {
		if _, err := g.Run(dir, args...); err != nil {
			if strings.Contains(err.Error(), newline) || strings.Contains(err.Error(), "newline") {
				t.Skipf("filesystem rejects newline paths: %v", err)
			}
			t.Fatal(err)
		}
	}
	// Deleting the checkout makes it prunable.
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	l, err = g.WorktreeList(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.RepoRoot != mustReal(t, dir) {
		t.Errorf("RepoRoot: %q", l.RepoRoot)
	}
	byPath := map[string]WorktreeEntry{}
	for _, w := range l.Worktrees {
		byPath[w.Path] = w
	}
	main := byPath[mustReal(t, dir)]
	if main.IsLinked || main.Branch != "main" {
		t.Errorf("main: %+v", main)
	}
	if w := byPath[feat]; !w.IsLinked || w.Branch != "feat" || w.IsPrunable {
		t.Errorf("feat: %+v", w)
	}
	if w := byPath[detached]; !w.IsLinked || w.Branch != "" {
		t.Errorf("detached must have no branch: %+v", w)
	}
	if w := byPath[newline]; !w.IsLinked || w.Branch != "nl" {
		t.Errorf("newline path: %+v (paths seen: %v)", w, l.Worktrees)
	}
	if w := byPath[gone]; !w.IsLinked || !w.IsPrunable {
		t.Errorf("gone must be prunable: %+v", w)
	}

	// Listing from a linked worktree returns the main checkout and siblings.
	l, err = g.WorktreeList(feat)
	if err != nil || l.RepoRoot != mustReal(t, dir) || len(l.Worktrees) != 5 {
		t.Errorf("from linked: %+v err=%v", l, err)
	}
}

func TestWorktreeListBrokenGitdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nonexistent/repo/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New().WorktreeList(dir); err == nil {
		t.Fatal("expected an error for a broken gitdir reference")
	}
}

// mustReal resolves symlinks the way git prints worktree paths.
func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
