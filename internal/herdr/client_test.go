package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake herdr script that echoes canned JSON per subcommand.
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSnapshotParse(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo '{"id":"x","result":{"snapshot":{"workspaces":[{"workspace_id":"w1","label":"a","number":1,"active_tab_id":"w1:t1","worktree":{"checkout_path":"/p","repo_root":"/r","is_linked_worktree":true}}],"layouts":[{"tab_id":"w1:t1","workspace_id":"w1","focused_pane_id":"w1:p1"}],"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","cwd":null,"foreground_cwd":"/f"}]}}}'`)}
	s, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Workspaces) != 1 || s.Workspaces[0].Worktree == nil || s.Workspaces[0].Worktree.CheckoutPath != "/p" {
		t.Errorf("bad workspaces: %+v", s.Workspaces)
	}
	if s.Panes[0].Cwd != nil || *s.Panes[0].ForegroundCwd != "/f" {
		t.Errorf("bad pane: %+v", s.Panes[0])
	}
}

func TestErrorEnvelopeOnStderr(t *testing.T) {
	// Real herdr: failure envelope goes to stderr, stdout is empty, exit 1.
	c := &CLI{Bin: fakeBin(t, `echo '{"error":{"code":"workspace_not_found","message":"workspace wZ not found"},"id":"x"}' >&2; exit 1`)}
	err := c.WorkspaceFocus("wZ")
	if err == nil || err.Error() != "herdr workspace: workspace wZ not found (workspace_not_found)" {
		t.Errorf("got %v", err)
	}
}

func TestErrorEnvelopeOnStdout(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo '{"error":{"code":"nope","message":"bad"},"id":"x"}'; exit 1`)}
	if _, err := c.Snapshot(); err == nil || err.Error() != "herdr api: bad (nope)" {
		t.Errorf("got %v", err)
	}
}

func TestNonJSONFailure(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo 'boom' >&2; exit 3`)}
	err := c.WorkspaceFocus("w1")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
}

func TestWorktreeListArgs(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `[ "$1 $2 $3 $4" = "worktree list --cwd /repo" ] || { echo "args: $@" >&2; exit 2; }
echo '{"id":"x","result":{"source":{"repo_root":"/repo"},"worktrees":[{"path":"/wt","branch":"b","is_linked_worktree":true,"is_prunable":false,"open_workspace_id":"w9"}]}}'`)}
	l, err := c.WorktreeList("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if l.Source.RepoRoot != "/repo" || *l.Worktrees[0].OpenWorkspaceID != "w9" {
		t.Errorf("bad: %+v", l)
	}
}

func TestWorkspaceLabelAndRename(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo "$@" > "$0.args"
case "$2" in
  get) echo '{"id":"x","result":{"workspace":{"workspace_id":"w1","label":"repo","number":1}}}' ;;
  *) echo '{"id":"x","result":{"type":"ok"}}' ;;
esac`)}
	l, err := c.WorkspaceLabel("w1")
	if err != nil || l != "repo" {
		t.Fatalf("label=%q err=%v", l, err)
	}
	if err := c.WorkspaceRename("w1", "o/repo"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.Bin + ".args")
	if got := strings.TrimSpace(string(b)); got != "workspace rename w1 o/repo" {
		t.Errorf("args: %q", got)
	}
}

func TestWorktreeCreateArgs(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo "$@" > "$0.args"; echo '{"id":"x","result":{"type":"worktree_created"}}'`)}
	if err := c.WorktreeCreate("/r", "feat", "origin/feat"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.Bin + ".args")
	if got := strings.TrimSpace(string(b)); got != "worktree create --cwd /r --branch feat --focus --base origin/feat" {
		t.Errorf("args: %q", got)
	}
	c.WorktreeCreate("/r", "feat", "")
	b, _ = os.ReadFile(c.Bin + ".args")
	if got := strings.TrimSpace(string(b)); got != "worktree create --cwd /r --branch feat --focus" {
		t.Errorf("no base: %q", got)
	}
}

func TestWorkspaceCreateArgs(t *testing.T) {
	c := &CLI{Bin: fakeBin(t, `echo "$@" > "$0.args"; echo '{"id":"x","result":{"type":"ok"}}'`)}
	if err := c.WorkspaceCreate("/p", "o/r"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.Bin + ".args")
	if got := strings.TrimSpace(string(b)); got != "workspace create --cwd /p --focus --label o/r" {
		t.Errorf("args: %q", got)
	}
	if err := c.WorkspaceCreate("/p", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(c.Bin + ".args")
	if got := strings.TrimSpace(string(b)); got != "workspace create --cwd /p --focus" {
		t.Errorf("no label: %q", got)
	}
}
