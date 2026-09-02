package tui

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utahta/herdr-hop/internal/config"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/scan"
)

func loadedHop(t *testing.T, root string, fc *fakeClient, git *fakeCloner, repos ...string) HopModel {
	t.Helper()
	// git.remotes may be keyed by the repo path relative to root; keys are
	// rewritten to normalized absolute paths once the directories exist.
	rel := git.remotes
	git.remotes = map[string]string{}
	for _, r := range repos {
		p := filepath.Join(root, r)
		if err := mkRepo(p); err != nil {
			t.Fatal(err)
		}
		u, ok := rel[r]
		if !ok {
			u = "https://" + r + ".git" // default: origin matches the ROOT layout
		}
		git.remotes[scanNormalize(p)] = u
	}
	cfg := config.Config{Root: root, SearchPaths: []string{root}, Depth: 3, DefaultHost: "github.com", CloneProtocol: "https"}
	m := NewHop(cfg, fc, git, log.New(io.Discard, "", 0), false)
	cands, err := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	if err != nil {
		t.Fatal(err)
	}
	hop.ApplyRepoIDs(cands, hop.ResolveRepoIDs(context.Background(), git, cands))
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(resolvedMsg{gen: 1})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return mm.(HopModel)
}

func scanNormalize(p string) string { return scan.Normalize(p) }

func typeKeys(m HopModel, s string) HopModel {
	for _, r := range s {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(HopModel)
	}
	return m
}

// drain feeds cmd results back until doneMsg or no further cmd.
func drain(m HopModel, cmd tea.Cmd) HopModel {
	for i := 0; cmd != nil && i < 50; i++ {
		msg := cmd()
		mm, next := m.Update(msg)
		m = mm.(HopModel)
		if _, ok := msg.(doneMsg); ok {
			return m
		}
		cmd = next
	}
	return m
}

func TestCloneRowAppearsOnlyForMissingTargets(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{}, "github.com/utahta/herdr-hop")

	// plain word: no clone row
	m = typeKeys(m, "herdr")
	if m.cloneRow != nil {
		t.Error("plain query must not produce a clone row")
	}
	// existing owner/repo (case-insensitive): no clone row
	m.input.SetValue("")
	m = typeKeys(m, "Utahta/Herdr-Hop")
	if m.cloneRow != nil {
		t.Error("existing repository must not produce a clone row")
	}
	// missing owner/repo: clone row at the end, fuzzy matches still shown
	m.input.SetValue("")
	m = typeKeys(m, "utahta/herdr-new")
	if m.cloneRow == nil || m.cloneRow.Label != "github.com/utahta/herdr-new" || m.cloneRow.Branch != "https://github.com/utahta/herdr-new.git" {
		t.Fatalf("clone row: %+v", m.cloneRow)
	}
	v := m.View()
	if !strings.Contains(v, "clone") || !strings.Contains(v, "github.com/utahta/herdr-new") {
		t.Errorf("view:\n%s", v)
	}
	// URL input
	m.input.SetValue("")
	m = typeKeys(m, "git@github.com:o/r.git")
	if m.cloneRow == nil || m.cloneRow.Label != "github.com/o/r" {
		t.Errorf("url clone row: %+v", m.cloneRow)
	}
	// Credentials never reach the display (other than the echoed input line).
	m.input.SetValue("")
	m = typeKeys(m, "https://u:tok@github.com/o/r")
	if m.cloneRow == nil || strings.Contains(viewWithoutInput(m), "tok") {
		t.Errorf("credentials leaked into view:\n%s", m.View())
	}
}

func TestCloneRowIdentityByRemoteNotLabel(t *testing.T) {
	root := t.TempDir()
	// Checked out at an unrelated path with a label that does not look like owner/repo,
	// but its origin says it IS github.com/acme/api.
	git := &fakeCloner{remotes: map[string]string{"work/api": "git@github.com:acme/api.git"}}
	m := loadedHop(t, root, &fakeClient{}, git, "work/api")
	m = typeKeys(m, "acme/api")
	if m.cloneRow != nil {
		t.Errorf("repository identified by remote must suppress the clone row: %+v", m.cloneRow)
	}
	// Same display name, different owner -> still offered.
	m.input.SetValue("")
	m = typeKeys(m, "other/api")
	if m.cloneRow == nil {
		t.Error("different owner must get a clone row")
	}
	// Existing destination path (no remote known) also suppresses.
	git.remotes = map[string]string{}
	m = loadedHop(t, root, &fakeClient{}, git, "github.com/x/y")
	for i := range m.cands {
		m.cands[i].RepoID = ""
	}
	m = typeKeys(m, "x/y")
	if m.cloneRow != nil {
		t.Errorf("destination path match must suppress the clone row: %+v", m.cloneRow)
	}
}

func TestFullURLKeepsExistingRepoVisible(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{remotes: map[string]string{"work/api": "git@github.com:acme/api.git"}}
	m := loadedHop(t, root, &fakeClient{}, git, "work/api", "github.com/x/y")
	m = typeKeys(m, "https://github.com/acme/api")
	if m.cloneRow != nil {
		t.Errorf("existing repo must suppress the clone row: %+v", m.cloneRow)
	}
	if len(m.view) != 1 {
		t.Fatalf("expected exactly the existing repo, got view=%v", m.view)
	}
	if c, ok := m.selected(); !ok || c.Kind != hop.KindRepo || !strings.HasSuffix(c.Path, "work/api") {
		t.Errorf("selected: %+v", c)
	}
	// Enter opens it (no clone).
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("enter should open the existing repo")
	}
}

func TestCloneProgressRedactedInTUI(t *testing.T) {
	root := t.TempDir()
	// A Cloner that (unlike gitx) does not mask its own output.
	git := &fakeCloner{lines: []string{"fatal: Authentication failed for 'https://u:tok@github.com/o/r'"}, err: errors.New("exit 128")}
	m := loadedHop(t, root, &fakeClient{}, git)
	m = typeKeys(m, "https://u:tok@github.com/o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	mm, cmd = m.Update(cmd()) // progress line
	m = mm.(HopModel)
	if strings.Contains(viewWithoutInput(m), "tok") {
		t.Errorf("progress leaked credentials:\n%s", m.View())
	}
	for _, p := range m.clone.progress {
		if strings.Contains(p, "tok") {
			t.Errorf("queued progress contains credentials: %q", p)
		}
	}
	m = drain(m, cmd)
	if strings.Contains(viewWithoutInput(m), "tok") {
		t.Errorf("error view leaked credentials:\n%s", m.View())
	}
}

func TestUnparsableURLIsRejectedNotEchoed(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{})
	m = typeKeys(m, "https://user:secret@example.com/%zz/o/r")
	if m.cloneRow != nil {
		t.Errorf("invalid URL must not produce a clone row: %+v", m.cloneRow)
	}
	if strings.Contains(viewWithoutInput(m), "secret") {
		t.Errorf("view leaked secret:\n%s", m.View())
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || m.clone.running {
		t.Error("enter must not start a clone for an invalid URL")
	}
}

func TestCloneSSHDiagnosticMaskedInTUI(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{lines: []string{"tok3n@example.com: Permission denied (publickey)."}, err: errors.New("tok3n@example.com: Permission denied")}
	m := loadedHop(t, root, &fakeClient{}, git)
	m = typeKeys(m, "tok3n@example.com:owner/repo.git")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(mm.(HopModel), cmd)
	if v := viewWithoutInput(m); strings.Contains(v, "tok3n") || !strings.Contains(v, "example.com: Permission denied") {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestEscAfterCloneFinishedDoesNotOpenWorkspace(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	git := &fakeCloner{block: make(chan struct{})}
	m := loadedHop(t, root, fc, git)
	m = typeKeys(m, "o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	// git finishes successfully...
	close(git.block)
	successMsg := cmd() // cloneDoneMsg{err: nil} is now queued
	// ...but the user presses Esc before the TUI processes that message.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	mm, next := m.Update(successMsg)
	m = mm.(HopModel)
	if next != nil || m.pending || m.quit || len(fc.calls) != 0 {
		t.Errorf("workspace must not be opened after Esc: pending=%v quit=%v calls=%v", m.pending, m.quit, fc.calls)
	}
	if m.clone.running || !strings.Contains(m.View(), "clone canceled") {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestStaleCloneMessagesIgnored(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{block: make(chan struct{})})
	m = typeKeys(m, "o/r")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	old := m.clone.op
	mm, _ = m.Update(cloneProgressMsg{op: old - 1, line: "stale"})
	m = mm.(HopModel)
	if len(m.clone.progress) != 0 {
		t.Errorf("stale progress must be ignored: %v", m.clone.progress)
	}
	mm, _ = m.Update(cloneDoneMsg{op: old - 1, dest: "/x"})
	m = mm.(HopModel)
	if !m.clone.running || m.pending {
		t.Error("stale done must not affect the current operation")
	}
}

func TestCloneOutputSanitizedInTUI(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{lines: []string{"\x1b[2J\x1b]52;c;c2VjcmV0\x07\x1b[31mfatal:\x1b[0m bad"}, err: errors.New("\x1b]0;x\x07boom")}
	m := loadedHop(t, root, &fakeClient{}, git)
	m = typeKeys(m, "o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(mm.(HopModel), cmd)
	v := m.View()
	for _, bad := range []string{"\x07", "52;c", "]0;x"} {
		if strings.Contains(v, bad) {
			t.Errorf("control sequence survived: %q", v)
		}
	}
	// The style library adds its own ESC colour codes, so check the stored lines, not the view, for ESC.
	for _, p := range m.clone.progress {
		if strings.Contains(p, "\x1b") {
			t.Errorf("ESC survived in progress: %q", p)
		}
	}
	if strings.Contains(m.errMsg, "\x1b") || !strings.Contains(m.errMsg, "boom") {
		t.Errorf("errMsg: %q", m.errMsg)
	}
	if !strings.Contains(v, "fatal: bad") {
		t.Errorf("text lost:\n%s", v)
	}
}

func TestPickerLabelsAreSanitizedForDisplay(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{})
	m.cands = []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/x/a", Label: "github.com/o/\x1b]52;c;c2VjcmV0\x07a", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/x/b", Label: "b", Branch: "fe\u202eat", RepoRoot: "/x/a", OpenState: hop.OpenClosed},
	}
	m.labels = []string{m.cands[0].Label, m.cands[1].Label}
	m.refilter()
	v := m.View()
	for _, bad := range []string{"\x07", "52;c", "\u202e"} {
		if strings.Contains(v, bad) {
			t.Errorf("%q reached the view: %q", bad, v)
		}
	}
	if !strings.Contains(v, "github.com/o/a") || !strings.Contains(v, "└─ feat") {
		t.Errorf("text lost:\n%s", v)
	}
	mm, _ := m.Update(doneMsg{err: errors.New("herdr: \x1b[2Jbad \x1b]0;t\x07path")})
	if v := mm.(HopModel).View(); strings.Contains(v, "\x07") || !strings.Contains(v, "error: herdr: bad path") {
		t.Errorf("error view:\n%s", v)
	}
}

func TestCloningStatusURLIsSanitized(t *testing.T) {
	root := t.TempDir()
	var logBuf strings.Builder
	git := &fakeCloner{block: make(chan struct{})}
	m := loadedHop(t, root, &fakeClient{}, git)
	m.log = log.New(&logBuf, "", 0)
	m = typeKeys(m, "https://example.com/\u202e/x/o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd == nil || !m.clone.running {
		t.Fatal("clone should start")
	}
	// url.String() percent-encodes the format character, so the status line
	// and the log show %E2%80%AE rather than a raw U+202E; both are safe.
	if v := m.View(); strings.Contains(v, "\u202e") || !strings.Contains(v, "cloning https://example.com/") {
		t.Errorf("status view:\n%s", v)
	}
	if strings.Contains(logBuf.String(), "\u202e") {
		t.Errorf("log leaked format character: %q", logBuf.String())
	}
	close(git.block)
	drain(m, cmd) // let the clone goroutine finish before the temp dir is removed
}

func TestCloneFailureRedactsURL(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{err: errors.New("fatal: unable to access 'https://u:tok@github.com/o/r': 403")}
	m := loadedHop(t, root, &fakeClient{}, git)
	m = typeKeys(m, "https://u:tok@github.com/o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(mm.(HopModel), cmd)
	if git.url != "https://u:tok@github.com/o/r" {
		t.Errorf("git must receive the raw URL, got %q", git.url)
	}
	if strings.Contains(viewWithoutInput(m), "tok") {
		t.Errorf("credentials leaked into error view:\n%s", m.View())
	}
}

// viewWithoutInput drops the input line (the first line), which echoes what
// the user typed.
func viewWithoutInput(m HopModel) string {
	lines := strings.Split(m.View(), "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

func TestCloneRowEnterClonesAndOpens(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	git := &fakeCloner{lines: []string{"Receiving objects: 100%"}}
	m := loadedHop(t, root, fc, git, "github.com/utahta/other")
	m = typeKeys(m, "utahta/new")
	// Only the clone row can match "utahta/new"? "other" fuzzy-matches too; move to the last row.
	for m.cursor < m.rowCount()-1 {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mm.(HopModel)
	}
	if c, _ := m.selected(); c.Kind != hop.KindClone {
		t.Fatalf("expected clone row selected, got %+v", c)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if !m.clone.running || cmd == nil {
		t.Fatal("clone should start")
	}
	// Enter and typing are ignored while cloning; tab-less UI has no mode switch.
	if _, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); c != nil {
		t.Error("enter must be ignored while cloning")
	}
	m = drain(m, cmd)
	want := filepath.Join(root, "github.com", "utahta", "new")
	if git.url != "https://github.com/utahta/new.git" || git.dest != want {
		t.Errorf("git: %+v", git)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "create "+want+" label=utahta/new" {
		t.Errorf("herdr calls: %v", fc.calls)
	}
	if !m.quit {
		t.Error("should quit after workspace create")
	}
}

func TestCloneRowNoMatchesIsSelectedByDefault(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{}, "github.com/a/b")
	m = typeKeys(m, "zzz/qqq")
	if m.rowCount() != 1 || m.cloneRow == nil {
		t.Fatalf("view=%v rows=%d", m.view, m.rowCount())
	}
	if c, ok := m.selected(); !ok || c.Kind != hop.KindClone {
		t.Errorf("clone row must be the selected row: %+v", c)
	}
}

func TestCloneRowValidationAndFailure(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "github.com", "o", "exists")
	os.MkdirAll(existing, 0o755) // exists on disk but not scanned (no .git)

	// no root
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{})
	m.cfg.Root = ""
	m = typeKeys(m, "o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || !strings.Contains(m.View(), "HERDR_HOP_ROOT is not set") {
		t.Errorf("no root:\n%s", m.View())
	}

	// dest exists
	m = loadedHop(t, root, &fakeClient{}, &fakeCloner{})
	m = typeKeys(m, "o/exists")
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || !strings.Contains(m.View(), "already exists") {
		t.Errorf("exists:\n%s", m.View())
	}

	// clone failure stays open, no workspace created
	fc := &fakeClient{}
	m = loadedHop(t, root, fc, &fakeCloner{err: errors.New("fatal: repository not found")})
	m = typeKeys(m, "o/r")
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(mm.(HopModel), cmd)
	if m.quit || len(fc.calls) != 0 || !strings.Contains(m.View(), "repository not found") {
		t.Errorf("quit=%v calls=%v view:\n%s", m.quit, fc.calls, m.View())
	}
}

func TestCloneRowCancel(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{lines: []string{"Receiving objects: 42%"}, block: make(chan struct{})}
	m := loadedHop(t, root, &fakeClient{}, git)
	m = typeKeys(m, "o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	mm, cmd = m.Update(cmd()) // progress
	m = mm.(HopModel)
	if !strings.Contains(m.View(), "Receiving objects: 42%") || !strings.Contains(m.View(), "esc to cancel") {
		t.Errorf("view:\n%s", m.View())
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	mm, _ = m.Update(cmd()) // cloneDoneMsg with ctx error
	m = mm.(HopModel)
	if m.clone.running || m.quit || !strings.Contains(m.View(), "error:") {
		t.Errorf("cancel should end with an error shown:\n%s", m.View())
	}
}
