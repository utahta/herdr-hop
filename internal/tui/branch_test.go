package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/worktree"
)

// branchScreenFor puts the picker on the branch screen for the first repo
// and delivers the refs. Returns the model and the repo path.
func branchScreenFor(t *testing.T, fc *fakeClient, git *fakeCloner, worktreeMode bool) (HopModel, string) {
	t.Helper()
	root := t.TempDir()
	m := loadedHop(t, root, fc, git, "github.com/o/r")
	m.worktreeMode = worktreeMode
	repo := m.cands[0].Path
	key := tea.KeyCtrlT
	if worktreeMode {
		key = tea.KeyEnter
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: key})
	m = mm.(HopModel)
	if m.wt == nil || cmd == nil {
		t.Fatal("expected branch screen with a refs load")
	}
	// Batch(blink, loadRefs): run the batch's cmds and deliver the refs msg.
	m = deliverBatch(m, cmd)
	return m, repo
}

func deliverBatch(m HopModel, cmd tea.Cmd) HopModel {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			if inner := c(); inner != nil {
				if _, blink := inner.(tea.BatchMsg); blink {
					continue
				}
				switch inner.(type) {
				case refsLoadedMsg, fetchDoneMsg, worktreeDoneMsg:
					mm, _ := m.Update(inner)
					m = mm.(HopModel)
				}
			}
		}
		return m
	}
	mm, _ := m.Update(msg)
	return mm.(HopModel)
}

// run executes cmd and feeds every resulting message back into the model,
// following chains and batches, until nothing is left (bounded).
func run(t *testing.T, m HopModel, cmd tea.Cmd) HopModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for i := 0; len(queue) > 0 && i < 50; i++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if !isModelMsg(msg) {
			continue // e.g. textinput blink ticks: not part of the flows under test
		}
		mm, next := m.Update(msg)
		m = mm.(HopModel)
		if next != nil {
			queue = append(queue, next)
		}
	}
	return m
}

// isModelMsg reports whether msg is one of this package's own messages.
func isModelMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case loadedMsg, doneMsg, cloneProgressMsg, cloneDoneMsg,
		refsLoadedMsg, fetchDoneMsg, worktreeDoneMsg, prHeadsMsg, prInfoMsg, prCheckedMsg, prFetchedMsg, prDoneMsg,
		resolvedMsg, wtStateMsg:
		return true
	}
	return false
}

func TestBranchScreenListsLocalThenRemote(t *testing.T) {
	git := &fakeCloner{refs: []string{"refs/remotes/origin/HEAD\t\t\t", "refs/remotes/origin/feat\t\t\t", "refs/heads/main\t\t\t", "refs/heads/feat\t\t\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	if got := m.wt.names; strings.Join(got, ",") != "main,feat,origin/feat" {
		t.Errorf("names = %v", got)
	}
	v := m.View()
	// Remote branches read by their dimmed remote-name prefix, not a tag.
	if !strings.Contains(v, "worktree> ") || !strings.Contains(v, "origin/feat") || strings.Contains(v, " remote") || !strings.Contains(v, "auto name") {
		t.Errorf("view:\n%s", v)
	}
}

func TestBranchLocalCheckout(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t", "refs/heads/feat\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "feat")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 1 || fc.calls[0] != "wtcreate "+repo+" feat base=" || len(git.upstreams) != 0 || !m.quit {
		t.Errorf("calls=%v upstreams=%v quit=%v", fc.calls, git.upstreams, m.quit)
	}
}

func TestBranchCreateRelabelsParentWorkspace(t *testing.T) {
	p := "w9"
	fc := &fakeClient{parent: &p, labels: map[string]string{"w9": "r"}} // herdr's default: directory name
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt.cursor = 1 // main (row 0 is the new-branch row)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if strings.Join(fc.calls, ",") != "wtcreate "+repo+" main base=,rename w9 o/r" || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
}

// selectRemote moves the cursor to the (first) remote row and presses Enter,
// which must enter the local-name state rather than create anything.
func selectRemote(t *testing.T, m HopModel) HopModel {
	t.Helper()
	for i := range m.wt.view {
		if m.wt.branches[m.wt.view[i]].IsRemote() {
			m.wt.cursor = i + 1 // row 0 is the new-branch row
			break
		}
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || m.wt.remote == nil {
		t.Fatalf("selecting a remote must switch to the name state, got cmd=%v remote=%v", cmd != nil, m.wt.remote)
	}
	return m
}

func TestBranchRemoteTracked(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t", "refs/remotes/origin/feat\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	if len(fc.calls) != 0 || m.wt.input.Value() != "feat" {
		t.Fatalf("no creation yet, name prefilled with short name: calls=%v input=%q", fc.calls, m.wt.input.Value())
	}
	if !strings.Contains(m.View(), "--base refs/remotes/origin/feat") {
		t.Errorf("view:\n%s", m.View())
	}
	// Enter with the prefilled name -> tracking branch.
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 1 || fc.calls[0] != "wtcreate "+repo+" feat base=refs/remotes/origin/feat" {
		t.Errorf("calls=%v", fc.calls)
	}
	if len(git.upstreams) != 1 || git.upstreams[0] != "feat->refs/remotes/origin/feat" || !m.quit {
		t.Errorf("upstreams=%v quit=%v", git.upstreams, m.quit)
	}
}

func TestCreateAsksGitBeforeCreating(t *testing.T) {
	// The list knows only "feature"; git (case-insensitive FS) says
	// "Feature" exists too. Creating "Feature" must be refused.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/feature\t\t\t", "refs/remotes/origin/Feature\t\t\t"}, existing: map[string]bool{"feature": true, "Feature": true}}
	m, _ := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "origin/Feature")
	m = selectRemote(t, m)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || !strings.Contains(m.errMsg, "existing local branch") || len(git.upstreams) != 0 {
		t.Errorf("calls=%v err=%q upstreams=%v", fc.calls, m.errMsg, git.upstreams)
	}
	if len(git.existsAsked) != 1 || git.existsAsked[0] != "Feature" {
		t.Errorf("git must be asked about %q: %v", "Feature", git.existsAsked)
	}
	// Checking out an existing local branch does not ask (nothing is created).
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}
	m, _ = branchScreenFor(t, fc, git, false)
	m.wt.cursor = 1 // main (row 0 is the new-branch row)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.existsAsked) != 0 || len(fc.calls) != 1 {
		t.Errorf("checkout must not ask: asked=%v calls=%v", git.existsAsked, fc.calls)
	}
	// New names from HEAD and auto names are checked as well.
	for _, typed := range []string{"topic", ""} {
		fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}, existing: map[string]bool{"topic": true}}
		m, _ = branchScreenFor(t, fc, git, false)
		m.wt = typeBranch(m.wt, typed)
		m.wt.cursor = 0 // the new-branch row (top)
		mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = run(t, mm.(HopModel), cmd)
		if len(git.existsAsked) != 1 {
			t.Errorf("%q: git must be asked once, got %v", typed, git.existsAsked)
		}
		if typed == "topic" && len(fc.calls) != 0 {
			t.Errorf("existing name must not be created: %v", fc.calls)
		}
		if typed == "" && len(fc.calls) != 1 {
			t.Errorf("auto name must be created after the check: %v", fc.calls)
		}
	}
}

func TestBranchRemoteWithDifferentName(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t", "refs/remotes/origin/feat\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	// Edit the name to "foo": base must stay origin/feat, no upstream.
	m.wt.input.SetValue("foo")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 1 || fc.calls[0] != "wtcreate "+repo+" foo base=refs/remotes/origin/feat" || len(git.upstreams) != 0 {
		t.Errorf("calls=%v upstreams=%v", fc.calls, git.upstreams)
	}
}

func TestBranchNamesAreNotTrimmed(t *testing.T) {
	// A remote whose short name ends in U+00A0 must be tracked under that
	// exact name, not a trimmed one.
	fc := &fakeClient{}
	short := "feat\u00a0"
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t", "refs/remotes/origin/" + short + "\t\ts\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	want := "wtcreate " + repo + " " + short + " base=refs/remotes/origin/" + short
	if len(fc.calls) != 1 || fc.calls[0] != want || len(git.upstreams) != 1 {
		t.Errorf("calls=%v upstreams=%v", fc.calls, git.upstreams)
	}
	// Blank input is an invalid name, not an auto name.
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}
	m, _ = branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "  ")
	m.wt.cursor = 0 // the new-branch row (top)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || !strings.Contains(m.errMsg, "invalid branch name") {
		t.Errorf("blank name: calls=%v err=%q", fc.calls, m.errMsg)
	}
}

func TestBranchRemoteEscReturnsToList(t *testing.T) {
	m, _ := branchScreenFor(t, &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t", "refs/remotes/origin/feat\t\t\t"}}, false)
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt == nil || m.wt.remote != nil || m.wt.input.Value() != "origin/feat" || m.wt.rowCount() == 0 {
		t.Errorf("esc must return to the filtered list: wt=%+v", m.wt)
	}
}

func TestBranchRemoteWithLocalConflict(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/feat\t\t\t", "refs/remotes/origin/feat\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	if !strings.Contains(m.View(), "already exists") {
		t.Errorf("view:\n%s", m.View())
	}
	// Keeping the same name is an error (herdr would check out local feat).
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || m.errMsg == "" || len(fc.calls) != 0 {
		t.Errorf("same name must be rejected: err=%q calls=%v", m.errMsg, fc.calls)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // dismiss
	m = mm.(HopModel)
	// Empty name is rejected too.
	m.wt.input.SetValue("")
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || m.errMsg == "" {
		t.Errorf("empty name must be rejected: err=%q", m.errMsg)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = mm.(HopModel)
	// A different name is created from origin/feat without upstream.
	m.wt.input.SetValue("feat2")
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 1 || fc.calls[0] != "wtcreate "+repo+" feat2 base=refs/remotes/origin/feat" || len(git.upstreams) != 0 {
		t.Errorf("calls=%v upstreams=%v", fc.calls, git.upstreams)
	}
}

func TestBranchInUseIsHiddenAndGuarded(t *testing.T) {
	// This screen only creates worktrees: a branch that already lives in one
	// is not listed (the picker opens those). Typing its name anyway is
	// refused with a hint that says where it is and where to go.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t/src/repo", "refs/heads/free\t\tf\t"}}
	m, _ := branchScreenFor(t, fc, git, false)
	if got := strings.Join(m.wt.names, ","); got != "free" {
		t.Fatalf("in-use branch must not be listed: %v", got)
	}
	if v := m.View(); strings.Contains(v, "main") || strings.Contains(v, "/src/repo") {
		t.Fatalf("view:\n%s", v)
	}
	m.wt = typeBranch(m.wt, "main") // no match: the cursor sits on the new row
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || len(fc.calls) != 0 ||
		!strings.Contains(m.errMsg, "/src/repo") || !strings.Contains(m.errMsg, "picker") {
		t.Errorf("creating an in-use name must point at its worktree: cmd=%v calls=%v err=%q", cmd != nil, fc.calls, m.errMsg)
	}
}

func TestBranchNamesAreSanitizedForDisplay(t *testing.T) {
	bidi := "fea\u202eture"
	git := &fakeCloner{refs: []string{
		"refs/heads/" + bidi + "\t\ts1\t",
		"refs/heads/\x1b]52;c;c2VjcmV0\x07inuse\t\ts2\t/src/repo",
		"refs/remotes/origin/\x1b[31mred\x1b[0m\t\ts3\t",
	}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	v := m.View()
	for _, bad := range []string{"\u202e", "\x07", "52;c", "]52"} {
		if strings.Contains(v, bad) {
			t.Errorf("%q reached the view: %q", bad, v)
		}
	}
	for _, want := range []string{"feature", "origin/red"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in view:\n%s", want, v)
		}
	}
	if strings.Contains(v, "inuse") || strings.Contains(v, "/src/repo") {
		t.Errorf("in-use branch must not be listed:\n%s", v)
	}
	// Model values are untouched: they are what git/herdr will receive.
	if m.wt.branches[0].Name != bidi || !strings.Contains(m.wt.branches[1].Name, "\x1b") {
		t.Error("internal ref names must not be altered")
	}
	// Selecting a remote shows its ref in the status/command line, sanitized.
	m.wt = typeBranch(m.wt, "origin/")
	m = selectRemote(t, m)
	if v := m.View(); strings.Contains(v, "\x1b[31m") || !strings.Contains(v, "--base refs/remotes/origin/red") {
		t.Errorf("remote state view:\n%s", v)
	}
}

func TestPrefilledRemoteNameIsSanitizedInInput(t *testing.T) {
	short := "rel\u202eease\u200b"
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t", "refs/remotes/origin/" + short + "\t\ts\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.wt = typeBranch(m.wt, "origin/")
	m = selectRemote(t, m)
	if m.wt.input.Value() != short {
		t.Fatalf("model must keep the real name, got %q", m.wt.input.Value())
	}
	v := m.View()
	if strings.Contains(v, "\u202e") || strings.Contains(v, "\u200b") {
		t.Errorf("format characters reached the view: %q", v)
	}
	if !strings.Contains(stripANSI(v), "> release") {
		t.Errorf("sanitized input not shown:\n%s", stripANSI(v))
	}
	// The value still drives the plan unchanged.
	if got := m.wt.newName(); got != short {
		t.Errorf("newName = %q", got)
	}
}

func TestBranchInUsePathIsSanitizedForDisplay(t *testing.T) {
	// The worktree path surfaces only in the collision error; its display
	// copy is sanitized while the internal value stays intact.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm1\t/src/\x1b]52;c;c2VjcmV0\x07\x1b[31mevil\x1b[0m/repo"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.wt = typeBranch(m.wt, "main")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	v := m.View()
	if strings.Contains(v, "\x07") || strings.Contains(v, "52;c") || strings.Contains(v, "]52") {
		t.Errorf("control sequence from worktree path reached the view: %q", v)
	}
	if !strings.Contains(v, "/src/evil/repo") {
		t.Errorf("path text lost:\n%s", v)
	}
	// The real path is kept in the model.
	if m.wt.inUse["main"] == "/src/evil/repo" {
		t.Error("internal path must not be altered")
	}
}

func TestBranchErrorKeysActImmediately(t *testing.T) {
	git := &fakeCloner{refsErr: errors.New("not a git repo")}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = deliverBatch(mm.(HopModel), cmd)
	if m.errMsg == "" {
		t.Fatal("precondition: error shown")
	}
	// One ctrl-r retries (returns the load command) and clears the error.
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	if cmd == nil || m.errMsg != "" || !m.wt.loading {
		t.Errorf("ctrl-r must retry in one press: cmd=%v err=%q loading=%v", cmd != nil, m.errMsg, m.wt.loading)
	}
	m = run(t, m, cmd) // fails again
	if m.wt.refsReady {
		t.Fatal("refsReady must stay false")
	}
	// One esc goes back to the picker.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt != nil || m.errMsg != "" {
		t.Errorf("esc must return to the picker in one press: wt=%v err=%q", m.wt != nil, m.errMsg)
	}
}

func TestBranchRefsFailureBlocksCreation(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refsErr: errors.New("not a git repo")}
	root := t.TempDir()
	m := loadedHop(t, root, fc, git, "github.com/o/r")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = deliverBatch(mm.(HopModel), cmd)
	if m.wt.refsReady {
		t.Fatal("refsReady must be false after a failed load")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // dismiss error
	m = mm.(HopModel)
	if !strings.Contains(m.View(), "failed to load branches") || m.wt.rowCount() != 0 {
		t.Errorf("view:\n%s", m.View())
	}
	for _, k := range []tea.KeyType{tea.KeyEnter, tea.KeyCtrlF} {
		if _, c := m.Update(tea.KeyMsg{Type: k}); c != nil || len(fc.calls) != 0 {
			t.Errorf("key %v must be inert until refs load", k)
		}
	}
	// Ctrl-R retries; once refs load, collisions are detected.
	git.refsErr = nil
	git.refs = []string{"refs/heads/feat\t\tf\t", "refs/remotes/origin/feat\t\tf\t"}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = run(t, mm.(HopModel), cmd)
	if !m.wt.refsReady || len(m.wt.branches) != 2 {
		t.Fatalf("retry failed: ready=%v branches=%v", m.wt.refsReady, m.wt.branches)
	}
	m.wt = typeBranch(m.wt, "origin/feat")
	m = selectRemote(t, m)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // same name as existing local
	m = mm.(HopModel)
	if cmd != nil || m.errMsg == "" || len(fc.calls) != 0 {
		t.Errorf("collision must be detected after reload: err=%q calls=%v", m.errMsg, fc.calls)
	}
}

func TestBranchNewNameAndAuto(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}
	m, repo := branchScreenFor(t, fc, git, false)
	// typed name that matches nothing: the "new" row is the only row after filtering
	m.wt = typeBranch(m.wt, "topic")
	if m.wt.rowCount() != 1 {
		t.Fatalf("rows=%d", m.wt.rowCount())
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 1 || fc.calls[0] != "wtcreate "+repo+" topic base=" {
		t.Errorf("calls=%v", fc.calls)
	}
	// taken name -> error
	fc.calls = nil
	m, _ = branchScreenFor(t, fc, git, false)
	m.wt = typeBranch(m.wt, "main")
	m.wt.cursor = 0 // the new-branch row (top); typed "main" equals an existing branch so newName is ""
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	// newName()=="" -> auto name, never "main"
	if len(fc.calls) != 1 || !strings.Contains(fc.calls[0], " wt/") {
		t.Errorf("calls=%v", fc.calls)
	}
}

func TestBranchFetchAndReload(t *testing.T) {
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}, fetchAdds: []string{"refs/remotes/origin/new\t\tn\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = run(t, mm.(HopModel), cmd)
	if git.fetched != 1 || strings.Join(m.wt.names, ",") != "main,origin/new" {
		t.Errorf("fetched=%d names=%v", git.fetched, m.wt.names)
	}
}

func TestBranchErrorsStayOnScreen(t *testing.T) {
	fc := &fakeClient{createErr: errors.New("herdr worktree: boom")}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}
	m, _ := branchScreenFor(t, fc, git, false)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // local main
	m = run(t, mm.(HopModel), cmd)
	if m.quit || !strings.Contains(m.View(), "boom") || m.wt == nil {
		t.Errorf("quit=%v view:\n%s", m.quit, m.View())
	}
	// refs failure
	m2, _ := loadedHop(t, t.TempDir(), &fakeClient{}, &fakeCloner{refsErr: errors.New("not a git repo")}, "github.com/o/r"), ""
	mm, cmd = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m2 = deliverBatch(mm.(HopModel), cmd)
	if !strings.Contains(m2.View(), "not a git repo") {
		t.Errorf("view:\n%s", m2.View())
	}
}

func TestBranchEscReturnsToPicker(t *testing.T) {
	m, _ := branchScreenFor(t, &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}, false)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt != nil || !strings.HasPrefix(stripANSI(m.View()), "hop") {
		t.Errorf("expected picker, view:\n%s", m.View())
	}
}

func TestWorktreeModeEnterOpensBranchScreen(t *testing.T) {
	fc := &fakeClient{}
	m, _ := branchScreenFor(t, fc, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}, true)
	if m.wt == nil || len(fc.calls) != 0 {
		t.Errorf("worktree mode: Enter must not open a workspace: calls=%v", fc.calls)
	}
}

func TestWorktreeModeOffersNoCloneRow(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{}
	m := loadedHop(t, root, &fakeClient{}, git, "github.com/o/r")
	m.worktreeMode = true
	m = typeKeys(m, "zzz/qqq")
	if m.cloneRow != nil || m.rowCount() != 0 {
		t.Errorf("worktree mode must not offer a clone row: row=%+v rows=%d", m.cloneRow, m.rowCount())
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || git.url != "" || m.clone.running {
		t.Error("enter must not clone in worktree mode")
	}
	// Defence in depth: even a synthetic clone row must not be executed.
	m.cloneRow = &hop.Candidate{Kind: hop.KindClone, Label: "github.com/zzz/qqq"}
	m.cursor = 0
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || mm.(HopModel).clone.running || git.url != "" {
		t.Error("enter on a clone row must be ignored in worktree mode")
	}
}

func TestWorktreeModeFindsExistingRepoByURL(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{remotes: map[string]string{"work/api": "git@github.com:acme/api.git"}}
	m := loadedHop(t, root, &fakeClient{}, git, "work/api")
	m.worktreeMode = true
	m = typeKeys(m, "https://github.com/acme/api")
	if len(m.view) != 1 || m.cloneRow != nil {
		t.Errorf("identity match must work in worktree mode: view=%v clone=%v", m.view, m.cloneRow)
	}
}

func TestCtrlTOnWorktreeRowUsesRepoRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "github.com/o/r")
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\t\t"}}, "github.com/o/r")
	m.cands = append(m.cands, hop.Candidate{Kind: hop.KindWorktree, Path: "/home/u/.herdr/worktrees/r/x", Label: "/home/u/.herdr/worktrees/r/x", RepoRoot: repo, RepoLabel: "github.com/o/r"})
	m.labels = append(m.labels, "/home/u/.herdr/worktrees/r/x")
	m.refilter()
	m.cursor = len(m.view) - 1
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	if m.wt == nil || m.wt.repo != repo || m.wt.label != "github.com/o/r" {
		t.Errorf("wt=%+v", m.wt)
	}
	// workspace rows are not eligible
	m = loadedHop(t, root, &fakeClient{}, &fakeCloner{}, "github.com/o/r")
	m.cands = []hop.Candidate{{Kind: hop.KindWorkspace, Label: "ws", OpenWorkspaceID: "w1", OpenState: hop.OpenOpen}}
	m.labels = []string{"ws"}
	m.refilter()
	if mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT}); mm.(HopModel).wt != nil || cmd != nil {
		t.Error("ctrl-t on a workspace row must do nothing")
	}
}

func typeBranch(b *branchState, s string) *branchState {
	b.input.SetValue(s)
	if b.remote == nil {
		b.refilter()
	}
	return b
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestNewNameMatchesRealBranchNamesNotSearchText(t *testing.T) {
	// With PR annotations loaded, names hold "feature #12"; typing the
	// existing name "feature" must still mean "filter", never "create".
	git := &fakeCloner{refs: []string{"refs/heads/feature\t\tf\t", "refs/heads/main\t\tm\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	mm, _ := m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{remote: "origin", heads: []worktree.PRHead{{Remote: "origin", Number: 12, SHA: "f"}}}}})
	m = mm.(HopModel)
	if !strings.Contains(strings.Join(m.wt.names, ","), "feature #12") {
		t.Fatalf("precondition: annotated names, got %v", m.wt.names)
	}
	m.wt = typeBranch(m.wt, "feature")
	if got := m.wt.newName(); got != "" {
		t.Errorf("existing name must be a filter, got newName=%q", got)
	}
	// The "new" row therefore shows the auto name, not "feature".
	m.wt.cursor = 0 // the new-branch row (top)
	if v := m.View(); !strings.Contains(v, "auto name") {
		t.Errorf("view:\n%s", v)
	}
}

func TestFetchFailureStillRefreshesAnnotations(t *testing.T) {
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, remotes: map[string]string{}, fetchErr: errors.New("upstream: unreachable")}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	git.remotes[m.cands[0].Path] = "https://github.com/o/r"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = run(t, mm.(HopModel), cmd)
	git.mu.Lock()
	git.prRefs = map[string][]gitx.PRRef{"origin": {{Remote: "origin", Number: 5, SHA: "m"}}}
	git.mu.Unlock()
	before := len(git.lsCalls)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = run(t, mm.(HopModel), cmd)
	if m.errMsg == "" {
		t.Fatal("fetch failure must still be shown")
	}
	if len(git.lsCalls) <= before {
		t.Fatal("annotations must be refreshed despite the fetch failure")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // dismiss error
	m = mm.(HopModel)
	if !strings.Contains(strings.Join(m.wt.names, ","), "main #5") {
		t.Errorf("annotation missing after failed fetch: %v", m.wt.names)
	}
}

func TestBranchCursorFollowsLateMatches(t *testing.T) {
	// "#12" matches nothing until the PR heads arrive, so the cursor parks
	// on the new row; once the annotation lands, the cursor must move to the
	// match — Enter must not create a branch named "#12".
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tsha-main\t", "refs/heads/feat\t\tsha-feat\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.wt = typeBranch(m.wt, "#12")
	if m.wt.cursor != 0 || len(m.wt.view) != 0 {
		t.Fatalf("no match yet: cursor=%d view=%v", m.wt.cursor, m.wt.view)
	}
	m.wt.applyPRResults([]string{"origin"}, []remotePRResult{{remote: "origin", heads: []worktree.PRHead{{Remote: "origin", Number: 12, SHA: "sha-feat"}}}})
	if br, ok := m.wt.selected(); !ok || br == nil || br.Name != "feat" {
		t.Fatalf("cursor must follow the late match: %+v", br)
	}

	// A new row chosen deliberately (matches existed) is kept across rebuilds.
	m.wt = typeBranch(m.wt, "fea")
	m.wt.cursor = 0
	m.wt.rebuild()
	if br, ok := m.wt.selected(); !ok || br != nil {
		t.Fatalf("a deliberately chosen new row must stay selected: %+v", br)
	}
}

func TestBranchRemoteNameWithSlashSplitsAtRemote(t *testing.T) {
	// Remote names may contain "/": the dimmed prefix is the remote's own
	// name, not the text up to the first slash.
	// ParseRefs attributes refs to the configured remotes, so the fake must
	// report the slashed remote name.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\t\t", "refs/remotes/foo/bar/feature\t\t\t"}, remoteNames: []string{"foo/bar"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	for i := range m.wt.view {
		br := m.wt.branches[m.wt.view[i]]
		if !br.IsRemote() {
			continue
		}
		if br.Remote != "foo/bar" {
			t.Fatalf("ref parsing attributes the remote as %q, want foo/bar", br.Remote)
		}
		segs := m.branchRowSegs(i + 1)
		if len(segs) < 2 || segs[0].text != "foo/bar/" || segs[1].text != "feature" {
			t.Fatalf("segments: %+v", segs)
		}
		return
	}
	t.Fatal("no remote branch listed")
}

func TestBranchPRLabelsFormAlignedColumn(t *testing.T) {
	// PR labels sit in their own column, so rows with names of different
	// length line them up instead of trailing each name at its own offset.
	git := &fakeCloner{refs: []string{"refs/heads/a\t\tsha-a\t", "refs/heads/a-much-longer-name\t\tsha-b\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.width, m.height = 80, 20
	m.wt.applyPRResults([]string{"origin"}, []remotePRResult{{remote: "origin", heads: []worktree.PRHead{
		{Remote: "origin", Number: 3, SHA: "sha-a"},
		{Remote: "origin", Number: 12, SHA: "sha-b"},
	}}})
	cols := map[string]int{}
	for _, line := range strings.Split(stripANSI(m.View()), "\n") {
		for _, label := range []string{"#3", "#12"} {
			if i := strings.Index(line, label); i >= 0 {
				cols[label] = i
			}
		}
	}
	if len(cols) != 2 || cols["#3"] != cols["#12"] {
		t.Fatalf("PR labels must start in the same column, got %v\n%s", cols, stripANSI(m.View()))
	}
	// A "#12" query lights up the label in the PR column: the match
	// positions index SearchText ("name #12") and are shifted past the name.
	m.wt = typeBranch(m.wt, "#12")
	if len(m.wt.view) != 1 {
		t.Fatalf("view=%v", m.wt.view)
	}
	segs := m.branchPRColumn(1)
	if len(segs) != 1 || segs[0].text != "#12" || segs[0].style.GetBold() != styleMatch.GetBold() || segs[0].style.GetForeground() != styleMatch.GetForeground() {
		t.Errorf("PR label must be a single highlighted run, got %+v", segs)
	}
	if name := m.branchRowSegs(1); segsWidth(name) != len("a-much-longer-name") {
		t.Errorf("name cell must not carry the label: %+v", name)
	}
}

// ghRepo is the forge address of github.com/o/r, the test repository.
var ghRepo = clone.ForgeRepo{Scheme: "https", Host: "github.com", Owner: "o", Name: "r"}

// fakeForge answers PR details per repository.
type fakeForge struct {
	mu    sync.Mutex
	infos map[clone.ForgeRepo]map[int]worktree.PRInfo // repo -> number -> info
	err   error
	asked []string // "<repo>:n,n"
}

func (f *fakeForge) PullRequests(_ context.Context, repo clone.ForgeRepo, numbers []int) (map[int]worktree.PRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := make([]string, len(numbers))
	for i, n := range numbers {
		parts[i] = fmt.Sprint(n)
	}
	f.asked = append(f.asked, repo.String()+":"+strings.Join(parts, ","))
	if f.err != nil {
		return nil, f.err
	}
	out := map[int]worktree.PRInfo{}
	for n, info := range f.infos[repo] {
		if slices.Contains(numbers, n) {
			out[n] = info
		}
	}
	return out, nil
}

// branchScreenWithPRs opens the branch screen with two PR-annotated branches
// and delivers the forge's answer, returning the model ready for display.
func branchScreenWithPRs(t *testing.T, f *fakeForge) HopModel {
	t.Helper()
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tsha-main\t", "refs/heads/feat\t\tsha-feat\t", "refs/heads/fix\t\tsha-fix\t"}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.width, m.height = 100, 20
	if f != nil {
		m = m.WithForge(f)
	}
	mm, cmd := m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{
		remote: "origin", repo: ghRepo,
		heads: []worktree.PRHead{{Remote: "origin", Number: 12, SHA: "sha-feat"}, {Remote: "origin", Number: 7, SHA: "sha-fix"}},
	}}})
	m = mm.(HopModel)
	if f == nil {
		if cmd != nil {
			t.Fatal("without a forge no details fetch may start")
		}
		return m
	}
	if cmd == nil {
		t.Fatal("PR heads must start the details fetch")
	}
	mm, _ = m.Update(cmd())
	return mm.(HopModel)
}

func TestBranchPRTitlesShownAndAskedOncePerRepository(t *testing.T) {
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {
		12: {Title: "Add PR column", State: worktree.PROpen},
		7:  {Title: "Fix the thing", State: worktree.PRMerged},
	}}}
	m := branchScreenWithPRs(t, f)
	if got := f.asked; len(got) != 1 || got[0] != "https://github.com/o/r:7,12" {
		t.Fatalf("one query per repository with the listed PR numbers, got %v", got)
	}
	v := stripANSI(m.View())
	for _, want := range []string{"#12  Add PR column", "#7   merged Fix the thing"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	if strings.Contains(v, "open ") {
		t.Errorf("a plainly open PR carries no state word:\n%s", v)
	}
	// Emphasis goes to what is still alive: the open PR's number is green
	// and its title in the normal tone; the merged PR's number and title
	// are dimmed like its state word.
	rowOf := func(name string) int {
		for i, idx := range m.wt.view {
			if m.wt.branches[idx].Name == name {
				return i + 1
			}
		}
		t.Fatalf("no row for %s", name)
		return -1
	}
	if segs := m.branchPRColumn(rowOf("feat")); segs[0].style.GetFaint() || segs[0].style.GetForeground() != styleOpen.GetForeground() {
		t.Errorf("open PR number must be green: %+v", segs)
	}
	if segs := m.branchTitleColumn(rowOf("feat")); segs[0].style.GetFaint() || segs[0].quiet {
		t.Errorf("open PR title must read in the normal tone: %+v", segs)
	}
	if segs := m.branchPRColumn(rowOf("fix")); !segs[0].style.GetFaint() || !segs[0].quiet {
		t.Errorf("merged PR number must be dimmed, and stay so under the cursor (quiet): %+v", segs)
	}
	if segs := m.branchPRColumn(rowOf("feat")); segs[0].quiet {
		t.Errorf("an alive PR's number takes the cursor emphasis: %+v", segs)
	}
	if segs := m.branchTitleColumn(rowOf("fix")); len(segs) < 2 || !segs[0].style.GetFaint() || !segs[1].style.GetFaint() {
		t.Errorf("merged PR state and title must be dimmed: %+v", segs)
	}
}

func TestBranchTitleWordsSearchRanksBelowNameHits(t *testing.T) {
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {
		12: {Title: "Rework the main loop", State: worktree.PROpen},
		7:  {Title: "Fix the thing", State: worktree.PROpen},
	}}}
	m := branchScreenWithPRs(t, f)
	// "main" hits the branch name main (fuzzy) and PR #12's title (word):
	// the name hit comes first, the title hit follows, the rest is out.
	m.wt = typeBranch(m.wt, "main")
	var names []string
	for _, i := range m.wt.view {
		names = append(names, m.wt.branches[i].Name)
	}
	if strings.Join(names, ",") != "main,feat" {
		t.Fatalf("view=%v", names)
	}
	if tm := m.wt.titleMatches[m.wt.view[1]]; tm.pr != 0 || len(tm.pos) != 4 || tm.pos[0] != 11 {
		t.Errorf("title positions of 'main' in %q: %+v", "Rework the main loop", tm)
	}
	// Every word must occur: "rework loop" matches, "rework nope" does not.
	m.wt = typeBranch(m.wt, "rework loop")
	if len(m.wt.view) != 1 || m.wt.branches[m.wt.view[0]].Name != "feat" || m.wt.cursor != 1 {
		t.Errorf("multi-word title search: view=%v cursor=%d", m.wt.view, m.wt.cursor)
	}
	m.wt = typeBranch(m.wt, "rework nope")
	if len(m.wt.view) != 0 {
		t.Errorf("all words must match: view=%v", m.wt.view)
	}
	// A title is never fuzzy-matched: "rtml" (r-t-m-l scattered in the
	// title) must not surface the branch.
	m.wt = typeBranch(m.wt, "rtml")
	if len(m.wt.view) != 0 {
		t.Errorf("titles are word-matched, not fuzzy: view=%v", m.wt.view)
	}
}

func TestBranchPRInfoIgnoresSupersededGenerationAndKeepsOldDetails(t *testing.T) {
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {12: {Title: "Old title", State: worktree.PROpen}}}}
	m := branchScreenWithPRs(t, f)
	stale := prInfoMsg{op: m.wt.op, gen: m.wt.prGen - 1, request: m.wt.prInfoReq, results: []prInfoResult{{repo: ghRepo, infos: map[int]worktree.PRInfo{12: {Title: "Stale", State: worktree.PRClosed}}}}}
	mm, _ := m.Update(stale)
	m = mm.(HopModel)
	if !strings.Contains(stripANSI(m.View()), "Old title") || strings.Contains(stripANSI(m.View()), "Stale") {
		t.Errorf("a superseded generation must be dropped:\n%s", stripANSI(m.View()))
	}
	// A failing round keeps the details on display.
	mm, _ = m.Update(prInfoMsg{op: m.wt.op, gen: m.wt.prGen, request: m.wt.prInfoReq, results: []prInfoResult{{repo: ghRepo, err: errors.New("boom")}}})
	m = mm.(HopModel)
	if !strings.Contains(stripANSI(m.View()), "Old title") {
		t.Errorf("a failed fetch must keep the previous details:\n%s", stripANSI(m.View()))
	}
}

func TestBranchWithoutForgeShowsNumbersOnly(t *testing.T) {
	m := branchScreenWithPRs(t, nil)
	v := stripANSI(m.View())
	if !strings.Contains(v, "#12") || strings.Contains(v, "merged") {
		t.Errorf("numbers without details:\n%s", v)
	}
	// Colour is reserved for a confirmed-alive PR: an unknown state (no
	// forge, or details still loading) leaves the number dimmed.
	for i, idx := range m.wt.view {
		if m.wt.branches[idx].Name == "feat" {
			if segs := m.branchPRColumn(i + 1); len(segs) == 0 || !segs[0].style.GetFaint() || !segs[0].quiet {
				t.Errorf("unknown-state PR number must be dimmed, under the cursor too: %+v", segs)
			}
		}
	}
}

func TestBranchPRDetailsRequestedWhenHeadsArriveBeforeRefs(t *testing.T) {
	// The refs and the PR heads load concurrently. When the heads land
	// first there is no branch list to pick PR numbers from; the refs must
	// then start the details request — and it must not be made twice.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tsha-main\t", "refs/heads/feat\t\tsha-feat\t"}}
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {12: {Title: "Late title", State: worktree.PROpen}}}}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r").WithForge(f)
	mm, batch := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	heads := prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{
		remote: "origin", repo: ghRepo, heads: []worktree.PRHead{{Remote: "origin", Number: 12, SHA: "sha-feat"}},
	}}}
	mm, cmd := m.Update(heads)
	m = mm.(HopModel)
	if cmd != nil {
		t.Fatal("no branches yet: nothing to ask about")
	}
	// Deliver the refs from the batch by hand, keeping the returned cmd.
	var infoCmd tea.Cmd
	for _, c := range batch().(tea.BatchMsg) {
		if c == nil {
			continue
		}
		if msg, ok := c().(refsLoadedMsg); ok {
			mm, infoCmd = m.Update(msg)
			m = mm.(HopModel)
		}
	}
	if infoCmd == nil {
		t.Fatal("the refs arriving after the heads must start the details request")
	}
	mm, _ = m.Update(infoCmd())
	m = mm.(HopModel)
	if !strings.Contains(stripANSI(m.View()), "Late title") {
		t.Errorf("details missing:\n%s", stripANSI(m.View()))
	}
	// The same heads arriving again (or the refs) ask nothing new.
	if _, cmd := m.Update(heads); cmd != nil {
		t.Error("an identical request within the generation must not be repeated")
	}
	if got := f.asked; len(got) != 1 {
		t.Errorf("asked %v", got)
	}
}

func TestBranchPRInfoPartialFailureMergesIntoCache(t *testing.T) {
	// The forge returns the answers it got before a failure alongside the
	// error; those refresh the cache, the rest keeps its previous details.
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {
		12: {Title: "Twelve", State: worktree.PROpen},
		7:  {Title: "Seven", State: worktree.PROpen},
	}}}
	m := branchScreenWithPRs(t, f)
	partial := prInfoMsg{op: m.wt.op, gen: m.wt.prGen, request: m.wt.prInfoReq, results: []prInfoResult{{
		repo:  ghRepo,
		infos: map[int]worktree.PRInfo{7: {Title: "Seven", State: worktree.PRMerged}},
		err:   errors.New("network"),
	}}}
	mm, _ := m.Update(partial)
	m = mm.(HopModel)
	v := stripANSI(m.View())
	if !strings.Contains(v, "#12  Twelve") || !strings.Contains(v, "merged Seven") {
		t.Errorf("partial answers must merge, not replace:\n%s", v)
	}
}

func TestBranchSeveralPRsOnOneCommitKeepTheirOwnDetails(t *testing.T) {
	// One commit, two PRs: each label is coloured by its own state, the
	// details cell shows the PR that is still alive, and either title is
	// searchable.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tsha-main\t", "refs/heads/feat\t\tsha-feat\t"}}
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {
		12: {Title: "Old attempt", State: worktree.PRClosed},
		30: {Title: "Second attempt", State: worktree.PROpen},
	}}}
	m, _ := branchScreenFor(t, &fakeClient{}, git, false)
	m.width, m.height = 100, 20
	m = m.WithForge(f)
	mm, cmd := m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{
		remote: "origin", repo: ghRepo,
		heads: []worktree.PRHead{{Remote: "origin", Number: 12, SHA: "sha-feat"}, {Remote: "origin", Number: 30, SHA: "sha-feat"}},
	}}})
	m = mm.(HopModel)
	mm, _ = m.Update(cmd())
	m = mm.(HopModel)
	row := -1
	for i, idx := range m.wt.view {
		if m.wt.branches[idx].Name == "feat" {
			row = i + 1
		}
	}
	labels := m.branchPRColumn(row)
	var texts []string
	for _, s := range labels {
		texts = append(texts, s.text)
	}
	if strings.Join(texts, "") != "#12 #30" {
		t.Fatalf("labels: %q", texts)
	}
	if !labels[0].style.GetFaint() || labels[2].style.GetFaint() {
		t.Errorf("closed #12 must be dim and open #30 green: %+v", labels)
	}
	if v := stripANSI(m.View()); !strings.Contains(v, "#12 #30  Second attempt") || strings.Contains(v, "Old attempt") {
		t.Errorf("the alive PR's title is shown:\n%s", v)
	}
	// Searching the other PR's title finds the branch and shows that title.
	m.wt = typeBranch(m.wt, "old attempt")
	if len(m.wt.view) != 1 || m.wt.branches[m.wt.view[0]].Name != "feat" {
		t.Fatalf("view=%v", m.wt.view)
	}
	if v := stripANSI(m.View()); !strings.Contains(v, "closed Old attempt") {
		t.Errorf("the matched PR's title is shown:\n%s", v)
	}
}

func TestBranchLateAnswerOfReplacedPRInfoFetchIsDropped(t *testing.T) {
	// Within one PR-head generation two fetches may start: the refs arrive
	// first and ask with the previous heads (A), then the new heads arrive
	// and ask again with a different set (B). Cancelling A does not keep it
	// from answering late; its answer must be dropped, B's kept.
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghRepo: {
		12: {Title: "Twelve", State: worktree.PROpen},
		7:  {Title: "Seven", State: worktree.PROpen},
		99: {Title: "Ninety-nine", State: worktree.PROpen},
	}}}
	m := branchScreenWithPRs(t, f)
	// Refresh: a new generation; the refs land first (same branches), which
	// asks with the heads still on record (A).
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = mm.(HopModel)
	done, ok := cmd().(fetchDoneMsg)
	if !ok {
		t.Fatal("ctrl-f must run the fetch")
	}
	mm, cmd = m.Update(done) // -> Batch(loadRefs, loadPRHeads)
	m = mm.(HopModel)
	var refsMsg tea.Msg
	for _, c := range cmd().(tea.BatchMsg) {
		if c == nil {
			continue
		}
		if msg, ok := c().(refsLoadedMsg); ok {
			refsMsg = msg
		}
	}
	mm, cmdA := m.Update(refsMsg)
	m = mm.(HopModel)
	if cmdA == nil {
		t.Fatal("refs after a refresh must ask for details (A)")
	}
	// The new heads: #12 is gone, #99 appeared -> a different question (B).
	mm, cmdB := m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{
		remote: "origin", repo: ghRepo,
		heads: []worktree.PRHead{{Remote: "origin", Number: 99, SHA: "sha-feat"}, {Remote: "origin", Number: 7, SHA: "sha-fix"}},
	}}})
	m = mm.(HopModel)
	if cmdB == nil {
		t.Fatal("changed heads must ask again (B)")
	}
	// B answers first, then A's late answer arrives.
	msgB, msgA := cmdB(), cmdA()
	mm, _ = m.Update(msgB)
	m = mm.(HopModel)
	mm, _ = m.Update(msgA)
	m = mm.(HopModel)
	v := stripANSI(m.View())
	if !strings.Contains(v, "#99  Ninety-nine") || !strings.Contains(v, "#7   Seven") {
		t.Errorf("B's details must be on display:\n%s", v)
	}
	// A's answer, if applied, would have replaced the repository's details
	// with {12, 7}: #99 would have lost its title.
	if strings.Contains(v, "Twelve") {
		t.Errorf("A's late answer must be dropped:\n%s", v)
	}
}

func TestBranchForgeIsAddressedWithThePort(t *testing.T) {
	// An enterprise host on a non-default HTTPS port is a different API
	// endpoint: the forge must be told the scheme and port of the remote,
	// which the comparison identity (ghe.example/o/r) folds away.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, remotes: map[string]string{}}
	git.prRefs = map[string][]gitx.PRRef{"origin": {{Remote: "origin", Number: 5, SHA: "m"}}}
	ghe := clone.ForgeRepo{Scheme: "https", Host: "ghe.example", Port: "8443", Owner: "o", Name: "r"}
	f := &fakeForge{infos: map[clone.ForgeRepo]map[int]worktree.PRInfo{ghe: {5: {Title: "Ported", State: worktree.PROpen}}}}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r").WithForge(f)
	git.remotes[m.cands[0].Path] = "https://ghe.example:8443/o/r.git"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = run(t, mm.(HopModel), cmd)
	if got := f.asked; len(got) != 1 || got[0] != "https://ghe.example:8443/o/r:5" {
		t.Fatalf("asked %v", got)
	}
	m.width, m.height = 100, 20
	if v := stripANSI(m.View()); !strings.Contains(v, "#5  Ported") {
		t.Errorf("details missing:\n%s", v)
	}
}

func TestPRHeadsListRemotesOnceAndMaskTheirURLs(t *testing.T) {
	// The PR-heads fetch learns the remotes from one `git remote -v`
	// (RemoteFetchURLs) and hands their URLs to ls-remote for masking,
	// instead of re-reading them per remote.
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, remotes: map[string]string{}, fetchURLs: map[string]string{}}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	repo := m.cands[0].Path
	git.remotes[repo] = "https://user:secret@github.com/o/r.git"
	git.remoteNames = []string{"origin", "upstream"}
	git.fetchURLs[repo+"\x00upstream"] = "https://github.com/up/r.git"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = run(t, mm.(HopModel), cmd)
	if got := strings.Join(git.lsCalls, ","); got != "origin,upstream" && got != "upstream,origin" {
		t.Fatalf("ls-remote per recognised remote, got %q", got)
	}
	for _, mask := range git.lsMasks {
		if len(mask) != 2 || !slices.Contains(mask, "https://user:secret@github.com/o/r.git") || !slices.Contains(mask, "https://github.com/up/r.git") {
			t.Errorf("every ls-remote gets all remote URLs to mask, got %v", mask)
		}
	}
}
