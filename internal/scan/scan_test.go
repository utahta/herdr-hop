package scan

import (
	"os"
	"path/filepath"
	"testing"
)

func mk(t *testing.T, path string, gitIsFile bool) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(path, ".git")
	if gitIsFile {
		if err := os.WriteFile(g, []byte("gitdir: /x"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Mkdir(g, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRepos(t *testing.T) {
	root := Normalize(t.TempDir())
	a := filepath.Join(root, "github.com", "o", "a")
	b := filepath.Join(root, "github.com", "o", "b-wt") // linked worktree: .git file
	deep := filepath.Join(root, "1", "2", "3", "deep")
	nested := filepath.Join(a, "vendor", "inner") // inside repo: must not be found
	mk(t, a, false)
	mk(t, b, true)
	mk(t, deep, false)
	mk(t, nested, false)
	os.MkdirAll(filepath.Join(root, ".hidden", "h"), 0o755)
	mk(t, filepath.Join(root, ".hidden", "h"), false) // inside a hidden dir: not found
	dotRepo := filepath.Join(root, "github.com", "o", ".github")
	mk(t, dotRepo, false) // hidden dir that is itself a repo: found

	got := Repos([]Target{{root, 3}, {root, 3}})
	want := []Repo{{Path: dotRepo}, {Path: a}, {Path: b, GitIsFile: true}}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v want %v", got, want)
		}
	}
	if got := Repos([]Target{{root, 4}}); len(got) != 4 {
		t.Errorf("depth 4 should find deep: %v", got)
	}
}

func TestReposHasWorktrees(t *testing.T) {
	root := Normalize(t.TempDir())
	with := filepath.Join(root, "with")
	empty := filepath.Join(root, "empty")
	linked := filepath.Join(root, "linked")
	mk(t, with, false)
	mk(t, empty, false)
	mk(t, linked, true)
	if err := os.MkdirAll(filepath.Join(with, ".git", "worktrees", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// an empty worktrees dir (all worktrees removed) counts as none
	if err := os.MkdirAll(filepath.Join(empty, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Repo{}
	for _, r := range Repos([]Target{{root, 1}}) {
		byPath[r.Path] = r
	}
	if !byPath[with].HasWorktrees {
		t.Errorf("with: %+v", byPath[with])
	}
	if byPath[empty].HasWorktrees {
		t.Errorf("empty: %+v", byPath[empty])
	}
	if byPath[linked].HasWorktrees {
		t.Errorf("linked worktree must not claim metadata: %+v", byPath[linked])
	}
}

func TestReposMissingRoot(t *testing.T) {
	if got := Repos([]Target{{"/nonexistent/xyz", 3}}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}
