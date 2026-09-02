package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/utahta/herdr-hop/internal/config"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/hop"
)

type fakeClient struct {
	snap      herdr.Snapshot
	calls     []string
	createErr error
	parent    *string           // source_workspace_id reported by WorktreeList
	labels    map[string]string // workspace id -> label
	renameErr error
	removeErr error
	worktrees map[string][]herdr.Worktree
}

func (f *fakeClient) Snapshot() (*herdr.Snapshot, error) { return &f.snap, nil }
func (f *fakeClient) WorktreeList(repo string) (*herdr.WorktreeList, error) {
	l := &herdr.WorktreeList{}
	l.Source.RepoRoot = repo
	l.Source.SourceWorkspaceID = f.parent
	l.Worktrees = f.worktrees[repo]
	return l, nil
}
func (f *fakeClient) WorkspaceCreate(cwd, label string) error {
	f.calls = append(f.calls, "create "+cwd+" label="+label)
	return f.createErr
}
func (f *fakeClient) WorkspaceFocus(id string) error {
	f.calls = append(f.calls, "focus "+id)
	return nil
}
func (f *fakeClient) WorktreeOpen(r, p string) error {
	f.calls = append(f.calls, "wtopen "+p)
	return nil
}
func (f *fakeClient) WorkspaceLabel(id string) (string, error) { return f.labels[id], nil }
func (f *fakeClient) WorkspaceRename(id, label string) error {
	f.calls = append(f.calls, "rename "+id+" "+label)
	return f.renameErr
}
func (f *fakeClient) WorktreeCreate(repo, branch, base string) error {
	f.calls = append(f.calls, "wtcreate "+repo+" "+branch+" base="+base)
	return f.createErr
}
func (f *fakeClient) WorktreeRemove(id string, force bool) error {
	f.calls = append(f.calls, fmt.Sprintf("wtremove %s force=%v", id, force))
	return f.removeErr
}
func (f *fakeClient) PluginPaneOpen(a, b string) error { return nil }

func TestHopModelFlow(t *testing.T) {
	root := t.TempDir()
	for _, r := range []string{"github.com/o/alpha", "github.com/o/beta"} {
		if err := mkRepo(filepath.Join(root, r)); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeClient{}
	cfg := config.Config{SearchPaths: []string{root}, Depth: 3}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)

	// Simulate load completing.
	cands, err := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	if err != nil {
		t.Fatal(err)
	}
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m = mm.(HopModel)
	v := m.View()
	if !strings.Contains(v, "o/alpha") || !strings.Contains(v, "o/beta") || !strings.Contains(v, "2/2") {
		t.Fatalf("view:\n%s", v)
	}

	// Filter to beta, press enter -> create workspace.
	for _, r := range "beta" {
		mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(HopModel)
	}
	if !strings.Contains(m.View(), "1/2") {
		t.Fatalf("filter failed:\n%s", m.View())
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected open cmd")
	}
	if msg, ok := cmd().(doneMsg); !ok || msg.err != nil {
		t.Fatalf("done: %+v", msg)
	}
	if len(fc.calls) != 1 || !strings.HasSuffix(fc.calls[0], "github.com/o/beta label=o/beta") || !strings.HasPrefix(fc.calls[0], "create ") {
		t.Errorf("calls: %v", fc.calls)
	}
}

func TestHopModelPendingBlocksDoubleOpen(t *testing.T) {
	root := t.TempDir()
	mkRepo(filepath.Join(root, "r"))
	fc := &fakeClient{}
	cfg := config.Config{SearchPaths: []string{root}, Depth: 1}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	cands, _ := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)

	mm, cmd1 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	mm, cmd2 := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // second press while pending
	m = mm.(HopModel)
	if cmd1 == nil || cmd2 != nil {
		t.Fatalf("second Enter must be ignored while pending (cmd1=%v cmd2=%v)", cmd1 != nil, cmd2 != nil)
	}
	if !strings.Contains(m.View(), "opening...") {
		t.Errorf("expected pending indicator:\n%s", m.View())
	}
	cmd1()
	if len(fc.calls) != 1 {
		t.Errorf("calls: %v", fc.calls)
	}
	mm, _ = m.Update(doneMsg{err: io.ErrUnexpectedEOF})
	m = mm.(HopModel)
	if m.pending {
		t.Error("pending must reset on doneMsg")
	}
}

func TestHopModelLoadingBlocksActions(t *testing.T) {
	root := t.TempDir()
	mkRepo(filepath.Join(root, "r"))
	fc := &fakeClient{}
	cfg := config.Config{SearchPaths: []string{root}, Depth: 1}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	cands, _ := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)

	// Start a reload: loading=true with stale candidates still in the model.
	mm, reload := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	if reload == nil || !m.loading {
		t.Fatal("first ctrl-r must start a load")
	}
	for name, key := range map[string]tea.KeyType{"enter": tea.KeyEnter, "ctrl-n": tea.KeyCtrlN, "ctrl-r": tea.KeyCtrlR} {
		mm, cmd := m.Update(tea.KeyMsg{Type: key})
		m = mm.(HopModel)
		if cmd != nil {
			t.Errorf("%s must be ignored while loading", name)
		}
	}
	if len(fc.calls) != 0 {
		t.Errorf("no herdr calls expected, got %v", fc.calls)
	}
	// Load completes -> actions work again.
	mm, _ = m.Update(reload())
	m = mm.(HopModel)
	if m.loading {
		t.Fatal("loading must reset")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Error("enter must work after load")
	}
	// While pending, ctrl-r is also ignored.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR}); cmd != nil {
		t.Error("ctrl-r must be ignored while pending")
	}
}

func TestHopModelOpenBadgeWithoutSnapshot(t *testing.T) {
	m := NewHop(config.Config{SearchPaths: []string{"/x"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	cands := []hop.Candidate{
		{Kind: hop.KindWorktree, Path: "/x/w", Label: "w", OpenState: hop.OpenOpen, OpenWorkspaceID: "w7"}, // OpenCount 0: snapshot failed
		{Kind: hop.KindRepo, Path: "/x/r", Label: "r", OpenState: hop.OpenOpen, OpenWorkspaceID: "w1", OpenCount: 3},
		{Kind: hop.KindRepo, Path: "/x/c", Label: "c", OpenState: hop.OpenClosed},
	}
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	lines := strings.Split(mm.(HopModel).View(), "\n")
	find := func(label string) string {
		for _, l := range lines {
			if strings.Contains(l, " "+label+" ") || strings.HasSuffix(l, label) {
				return l
			}
		}
		return ""
	}
	if l := find("w"); !strings.Contains(l, "●") {
		t.Errorf("worktree open via list must show the glyph: %q", l)
	}
	if l := find("r"); !strings.Contains(l, "●3") {
		t.Errorf("count glyph: %q", l)
	}
	if l := find("c"); strings.Contains(l, "●") {
		t.Errorf("closed must not show the glyph: %q", l)
	}
}

func TestCtrlRWhileErrorShownReloads(t *testing.T) {
	m := NewHop(config.Config{SearchPaths: []string{t.TempDir()}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: nil, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(doneMsg{err: io.ErrUnexpectedEOF})
	m = mm.(HopModel)
	if m.errMsg == "" {
		t.Fatal("precondition: error shown")
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	if m.errMsg != "" || !m.loading || cmd == nil {
		t.Errorf("one ctrl-r must dismiss and reload: err=%q loading=%v cmd=%v", m.errMsg, m.loading, cmd != nil)
	}
}

func TestHopModelWarningsAccumulate(t *testing.T) {
	m := NewHop(config.Config{}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: nil, warn: io.ErrUnexpectedEOF, gen: 1, occupancy: hop.Occupancy{OK: true}})
	v := mm.(HopModel).View()
	if !strings.Contains(v, "warning: unexpected EOF") || !strings.Contains(v, "HERDR_HOP_ROOT is not set") {
		t.Errorf("both warnings must be shown:\n%s", v)
	}
}

func TestHopModelErrorDisplay(t *testing.T) {
	m := NewHop(config.Config{}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: nil, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if !strings.Contains(m.View(), "HERDR_HOP_ROOT is not set") {
		t.Errorf("expected root warning:\n%s", m.View())
	}
	mm, _ = m.Update(doneMsg{err: io.ErrUnexpectedEOF})
	m = mm.(HopModel)
	if !strings.Contains(m.View(), "error: unexpected EOF") {
		t.Errorf("expected error:\n%s", m.View())
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if strings.Contains(mm.(HopModel).View(), "error:") {
		t.Error("error should be dismissed by a key")
	}
}

type fakeCloner struct {
	url, dest     string
	err           error
	lines         []string
	block         chan struct{}     // if set, Clone waits until closed or ctx is cancelled
	resolveBlocks bool              // RemoteFetchURLs waits until its ctx is cancelled
	remotes       map[string]string // repo path -> origin URL
	fetchURLs     map[string]string // repo + "\x00" + remote -> effective fetch URL (non-origin remotes)

	refs        []string
	remoteNames []string
	refspecs    []string        // nil = standard wildcard mapping
	existing    map[string]bool // BranchExists answers (git's view, may be case-insensitive)
	existsAsked []string
	existsErr   error
	refsErr     error
	refsCalls   int
	fetchAdds   []string
	fetched     int
	fetchErr    error
	upstreams   []string
	upstreamErr error

	mu          sync.Mutex
	config      map[string]string // git config key -> value
	fetchRefs   []string          // "<remoteOrURL> <refspec>"
	fetchRefErr error
	fetchBlock  chan struct{}
	prRefs      map[string][]gitx.PRRef // remote -> PR heads
	removed     []string                // WorktreeRemove calls: "<repo> <path> force=<bool>"
	removeErr   error
	lsCalls     []string
	lsMasks     [][]string // mask URLs handed to each LsRemotePRs call
	lsErr       error
	lsBlock     chan struct{}
	lsPRCalls   []string
	remoteState struct {
		st  gitx.RemoteState
		err error
	}
}

func (f *fakeCloner) WorktreeList(repo string) (gitx.WorktreeListing, error) {
	return gitx.WorktreeListing{RepoRoot: repo, Worktrees: []gitx.WorktreeEntry{{Path: repo}}}, nil
}
func (f *fakeCloner) WorktreeRemove(repo, path string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, fmt.Sprintf("%s %s force=%v", repo, path, force))
	return f.removeErr
}

func (f *fakeCloner) RemoteFetchURLs(ctx context.Context, repo string) ([]gitx.Remote, error) {
	if f.resolveBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	names, _ := f.Remotes(repo)
	var rs []gitx.Remote
	for _, n := range names {
		if u, _ := f.FetchURL(repo, n); u != "" {
			rs = append(rs, gitx.Remote{Name: n, URL: u})
		}
	}
	return rs, nil
}

// branch-screen fakes
func (f *fakeCloner) Refs(repo string) ([]string, error) {
	f.refsCalls++
	if f.refsErr != nil {
		return nil, f.refsErr
	}
	return f.refs, nil
}
func (f *fakeCloner) Remotes(repo string) ([]string, error) {
	if f.remoteNames != nil {
		return f.remoteNames, nil
	}
	if f.remotes[repo] != "" {
		return []string{"origin"}, nil
	}
	return nil, nil
}
func (f *fakeCloner) FetchRefspecs(repo, name string) ([]string, error) {
	if f.refspecs != nil {
		return f.refspecs, nil
	}
	return []string{"+refs/heads/*:refs/remotes/" + name + "/*"}, nil
}
func (f *fakeCloner) FetchURL(repo, name string) (string, error) {
	if name == "origin" {
		return f.remotes[repo], nil
	}
	return f.fetchURLs[repo+"\x00"+name], nil
}
func (f *fakeCloner) BranchExists(repo, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.existsAsked = append(f.existsAsked, name)
	return f.existing[name], f.existsErr
}
func (f *fakeCloner) Fetch(repo string) error {
	f.fetched++
	f.refs = append(f.refs, f.fetchAdds...)
	return f.fetchErr
}
func (f *fakeCloner) ConfigGet(repo, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config[key], nil
}
func (f *fakeCloner) ConfigSet(repo, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.config == nil {
		f.config = map[string]string{}
	}
	f.config[key] = value
	return nil
}
func (f *fakeCloner) FetchRef(ctx context.Context, repo, remoteOrURL, refspec string) error {
	f.mu.Lock()
	f.fetchRefs = append(f.fetchRefs, remoteOrURL+" "+refspec)
	f.mu.Unlock()
	if f.fetchBlock != nil {
		select {
		case <-f.fetchBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.fetchRefErr
}
func (f *fakeCloner) LsRemotePR(ctx context.Context, repo, src, headRef string) (gitx.RemoteState, error) {
	f.mu.Lock()
	f.lsPRCalls = append(f.lsPRCalls, src+" "+headRef)
	f.mu.Unlock()
	if f.remoteState.err != nil {
		return gitx.RemoteState{}, f.remoteState.err
	}
	return f.remoteState.st, nil
}
func (f *fakeCloner) LsRemotePRs(ctx context.Context, repo, remote string, mask []string) ([]gitx.PRRef, error) {
	f.mu.Lock()
	f.lsCalls = append(f.lsCalls, remote)
	f.lsMasks = append(f.lsMasks, mask)
	f.mu.Unlock()
	if f.lsBlock != nil {
		select {
		case <-f.lsBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.lsErr != nil {
		return nil, f.lsErr
	}
	return f.prRefs[remote], nil
}
func (f *fakeCloner) SetUpstream(repo, branch, upstream string) error {
	f.upstreams = append(f.upstreams, branch+"->"+upstream)
	return f.upstreamErr
}

func (f *fakeCloner) Clone(ctx context.Context, url, dest string, progress func(string)) error {
	f.url, f.dest = url, dest
	for _, l := range f.lines {
		progress(l)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	return os.MkdirAll(filepath.Join(dest, ".git"), 0o755)
}

func TestScrollCursorWalksWindowBeforeSliding(t *testing.T) {
	root := t.TempDir()
	for i := range 30 {
		if err := mkRepo(filepath.Join(root, fmt.Sprintf("c%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeClient{}
	cfg := config.Config{SearchPaths: []string{root}, Depth: 1}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	cands, err := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	if err != nil {
		t.Fatal(err)
	}
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10}) // listRows() = 6
	m = mm.(HopModel)

	// state returns the visible candidate labels and the cursor's label.
	labelRe := regexp.MustCompile(`c\d{2}`)
	state := func() (visible []string, cursor string) {
		for l := range strings.SplitSeq(m.View(), "\n") {
			label := labelRe.FindString(l)
			if label == "" {
				continue
			}
			visible = append(visible, label)
			if strings.HasPrefix(l, "> ") {
				cursor = label
			}
		}
		return visible, cursor
	}
	press := func(k tea.KeyType, n int) {
		for range n {
			mm, _ := m.Update(tea.KeyMsg{Type: k})
			m = mm.(HopModel)
		}
	}
	expect := func(step, wantFirst, wantLast, wantCursor string) {
		t.Helper()
		visible, cursor := state()
		if len(visible) != 6 || visible[0] != wantFirst || visible[5] != wantLast || cursor != wantCursor {
			t.Fatalf("%s: visible=%v cursor=%s (want %s..%s cursor %s)", step, visible, cursor, wantFirst, wantLast, wantCursor)
		}
	}

	expect("initial", "c00", "c05", "c00")
	// Moving down: the cursor walks to the bottom edge, then the window slides.
	press(tea.KeyDown, 7)
	expect("down x7", "c02", "c07", "c07")
	// Moving back up: the cursor walks up inside the window; the window must
	// not slide until the cursor reaches the top edge.
	press(tea.KeyUp, 1)
	expect("up x1", "c02", "c07", "c06")
	press(tea.KeyUp, 4)
	expect("up to top edge", "c02", "c07", "c02")
	press(tea.KeyUp, 1)
	expect("up past top edge slides", "c01", "c06", "c01")
	press(tea.KeyUp, 1)
	expect("at first row", "c00", "c05", "c00")
}

func TestScrollWindowStableWhenHeaderGrows(t *testing.T) {
	root := t.TempDir()
	for i := range 30 {
		if err := mkRepo(filepath.Join(root, fmt.Sprintf("c%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeClient{}
	cfg := config.Config{SearchPaths: []string{root}, Depth: 1}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	cands, err := hop.Build(fc, &fakeCloner{}, cfg.ScanTargets(), cfg.SearchPaths)
	if err != nil {
		t.Fatal(err)
	}
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10}) // listRows() = 6
	m = mm.(HopModel)

	labelRe := regexp.MustCompile(`c\d{2}`)
	state := func() (visible []string, cursor string) {
		for l := range strings.SplitSeq(m.View(), "\n") {
			label := labelRe.FindString(l)
			if label == "" {
				continue
			}
			visible = append(visible, label)
			if strings.HasPrefix(l, "> ") {
				cursor = label
			}
		}
		return visible, cursor
	}

	// Scroll to cursor 7: top = 2, window c02..c07 with 6 rows.
	for range 7 {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mm.(HopModel)
	}
	// Enter starts an open: pending adds the "opening..." line, shrinking the
	// list to 5 rows. The render slides the window to c03..c07 but the stored
	// top cannot be updated by View.
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd == nil || !m.pending {
		t.Fatal("expected a pending open")
	}
	if visible, cursor := state(); len(visible) != 5 || visible[0] != "c03" || cursor != "c07" {
		t.Fatalf("pending render: visible=%v cursor=%s", visible, cursor)
	}
	// Up must move the cursor inside the rendered window (c03..c07), not
	// scroll back to the stale stored top.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = mm.(HopModel)
	if visible, cursor := state(); len(visible) != 5 || visible[0] != "c03" || visible[4] != "c07" || cursor != "c06" {
		t.Fatalf("up with grown header: visible=%v cursor=%s (want c03..c07 cursor c06)", visible, cursor)
	}
}

func TestTreeGroupingFoldingAndPin(t *testing.T) {
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/alpha", Label: "repoAL"},
		{Kind: hop.KindRepo, Path: "/r/beta", Label: "repoBE"},
		{Kind: hop.KindWorktree, Path: "/w/b1", Label: "wtBE1", Branch: "brONE", RepoRoot: "/r/beta", RepoLabel: "repoBE", Current: true},
		{Kind: hop.KindWorktree, Path: "/w/b2", Label: "wtBE2", Branch: "brTWO", RepoRoot: "/r/beta", RepoLabel: "repoBE"},
		{Kind: hop.KindWorktree, Path: "/w/orphan", Label: "wtORP", Branch: "brORP", RepoRoot: "/outside", RepoLabel: "outside"},
		{Kind: hop.KindWorkspace, Path: "", Label: "wsSTD", OpenState: hop.OpenOpen, OpenWorkspaceID: "w9"},
	}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = mm.(HopModel)

	labels := []string{"repoAL", "repoBE", "brONE", "brTWO", "brORP", "wsSTD"}
	rows := func() (order []string, cursor string) {
		for l := range strings.SplitSeq(m.View(), "\n") {
			for _, lb := range labels {
				if strings.Contains(l, lb) {
					order = append(order, lb)
					if strings.Contains(l, "> ") {
						cursor = lb
					}
				}
			}
		}
		return order, cursor
	}
	key := func(k tea.KeyType) {
		mm, _ := m.Update(tea.KeyMsg{Type: k})
		m = mm.(HopModel)
	}
	expect := func(step string, wantCursor string, want ...string) {
		t.Helper()
		order, cursor := rows()
		if strings.Join(order, " ") != strings.Join(want, " ") || cursor != wantCursor {
			t.Fatalf("%s: order=%v cursor=%s (want %v cursor %s)\n%s", step, order, cursor, want, wantCursor, m.View())
		}
	}

	// The invoking worktree's repository group is pinned first; groups are
	// expanded; the workspace section follows the open groups, the orphan
	// worktree stays flat in the rest.
	expect("initial", "repoBE", "repoBE", "brONE", "brTWO", "wsSTD", "repoAL", "brORP")
	v := m.View()
	if !strings.Contains(v, "- 2 worktrees") || !strings.Contains(v, "└─ brONE") || strings.Contains(v, "└─ brORP") {
		t.Fatalf("markers:\n%s", v)
	}

	// Tab on the repo folds its group and keeps the cursor on it.
	key(tea.KeyTab)
	expect("fold", "repoBE", "repoBE", "wsSTD", "repoAL", "brORP")
	if !strings.Contains(m.View(), "+ 2 worktrees") {
		t.Fatalf("fold marker:\n%s", m.View())
	}
	// Tab again unfolds.
	key(tea.KeyTab)
	expect("unfold", "repoBE", "repoBE", "brONE", "brTWO", "wsSTD", "repoAL", "brORP")

	// Tab on a child folds the group and lands on its repo.
	key(tea.KeyDown)
	key(tea.KeyTab)
	expect("fold from child", "repoBE", "repoBE", "wsSTD", "repoAL", "brORP")

	// Tab on rows without a group does nothing.
	key(tea.KeyDown) // wsSTD: not a repo group
	key(tea.KeyTab)
	expect("tab on plain row", "wsSTD", "repoBE", "wsSTD", "repoAL", "brORP")

	// A query switches to the grouped match list: the folded worktrees
	// match directly (via their branches) and reappear under their repo,
	// cursor on the best worktree; fold markers are not shown and Tab is a
	// no-op.
	for _, r := range "br" {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(HopModel)
	}
	expect("filtered", "brONE", "repoBE", "brONE", "brTWO", "brORP")
	if !strings.Contains(m.View(), "└─ brONE") || strings.Contains(m.View(), "2 worktrees") {
		t.Fatalf("filtered markers:\n%s", m.View())
	}
	before := m.View()
	key(tea.KeyTab)
	if m.View() != before {
		t.Fatal("tab while filtering must not change anything")
	}

	// Clearing the query restores the tree with the fold state kept.
	key(tea.KeyBackspace)
	key(tea.KeyBackspace)
	order, _ := rows()
	if strings.Join(order, " ") != "repoBE wsSTD repoAL brORP" || !strings.Contains(m.View(), "+ 2 worktrees") {
		t.Fatalf("restored: order=%v\n%s", order, m.View())
	}
}

func TestFilterKeepsGroups(t *testing.T) {
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/alpha", Label: "acme/alpha"},
		{Kind: hop.KindRepo, Path: "/r/big", Label: "acme/bigbroker"},
		{Kind: hop.KindWorktree, Path: "/w/f", Label: "wtFLK", Branch: "fix-flaky", RepoRoot: "/r/big", RepoLabel: "acme/bigbroker"},
		{Kind: hop.KindWorktree, Path: "/w/x", Label: "wtFEA", Branch: "feature-x", RepoRoot: "/r/big", RepoLabel: "acme/bigbroker"},
		{Kind: hop.KindWorktree, Path: "/w/o", Label: "wtORP", Branch: "fix-orphan", RepoRoot: "/outside", RepoLabel: "outside/repo"},
	}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = mm.(HopModel)

	labels := []string{"acme/alpha", "acme/bigbroker", "fix-flaky", "feature-x", "fix-orphan"}
	rows := func() (order []string, cursor string) {
		for l := range strings.SplitSeq(m.View(), "\n") {
			for _, lb := range labels {
				if strings.Contains(l, lb) {
					order = append(order, lb)
					if strings.Contains(l, "> ") {
						cursor = lb
					}
				}
			}
		}
		return order, cursor
	}
	setQuery := func(q string) {
		m.input.SetValue(q)
		m.refilter()
	}

	cases := []struct {
		q, cursor string
		order     []string
	}{
		// repo name: the whole group, cursor on the repo.
		{"bigbro", "acme/bigbroker", []string{"acme/bigbroker", "fix-flaky", "feature-x"}},
		// branch name: the repo with the matching worktree, cursor on it.
		{"flaky", "fix-flaky", []string{"acme/bigbroker", "fix-flaky"}},
		// repo + branch: composite AND across fields.
		{"bigbro flaky", "fix-flaky", []string{"acme/bigbroker", "fix-flaky"}},
		// word order must not matter.
		{"flaky bigbro", "fix-flaky", []string{"acme/bigbroker", "fix-flaky"}},
		// all words match the repo label alone: a repo query with spaces —
		// whole group, cursor on the repo (not dragged onto a worktree).
		{"acme big", "acme/bigbroker", []string{"acme/bigbroker", "fix-flaky", "feature-x"}},
		// an orphan worktree is found through its RepoLabel.
		{"outside orphan", "fix-orphan", []string{"fix-orphan"}},
		{"zzz", "", nil},
	}
	for _, tc := range cases {
		setQuery(tc.q)
		order, cursor := rows()
		if strings.Join(order, "|") != strings.Join(tc.order, "|") || cursor != tc.cursor {
			t.Errorf("%q: order=%v cursor=%q (want %v cursor %q)\n%s", tc.q, order, cursor, tc.order, tc.cursor, m.View())
		}
	}
}

func TestResolvingGatesURLQueries(t *testing.T) {
	// Between loadedMsg and resolvedMsg, a query that names a repository
	// cannot be identity-matched: no clone row may be offered, no candidate
	// action may run, and the view says why.
	root := "/r"
	cands := []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/o-old/api-old", Label: "o-old/api-old"}}
	cfg := config.Config{Root: root, SearchPaths: []string{root}, Depth: 3, DefaultHost: "github.com", CloneProtocol: "https"}
	fc := &fakeClient{}
	m := NewHop(cfg, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = typeKeys(mm.(HopModel), "o/api")

	v := m.View()
	if !strings.Contains(v, "resolving remotes...") || strings.Contains(v, "clone") && strings.Contains(v, "https://") {
		t.Fatalf("resolving view:\n%s", v)
	}
	if strings.Contains(v, "no matches") {
		t.Fatalf("no matches must be suppressed while resolving:\n%s", v)
	}
	for name, key := range map[string]tea.KeyType{"enter": tea.KeyEnter, "ctrl-t": tea.KeyCtrlT, "ctrl-n": tea.KeyCtrlN} {
		mm, cmd := m.Update(tea.KeyMsg{Type: key})
		m = mm.(HopModel)
		if cmd != nil || m.pending || m.clone.running || m.wt != nil {
			t.Fatalf("%s must be ignored while resolving", name)
		}
	}
	if len(fc.calls) != 0 {
		t.Fatalf("no herdr calls expected, got %v", fc.calls)
	}

	// Identities arrive: the clone row appears and Enter works again.
	mm, _ = m.Update(resolvedMsg{gen: 1})
	m = mm.(HopModel)
	v = m.View()
	if strings.Contains(v, "resolving remotes...") || !strings.Contains(v, "clone") {
		t.Fatalf("resolved view:\n%s", v)
	}
	if m.cloneRow == nil {
		t.Fatal("clone row must be offered once resolved")
	}
}

func TestResolvedKeepsCursorOnPlainQuery(t *testing.T) {
	// resolvedMsg for a plain query must not rebuild the view: the cursor
	// the user parked must stay put.
	var cands []hop.Candidate
	for i := range 5 {
		cands = append(cands, hop.Candidate{Kind: hop.KindRepo, Path: fmt.Sprintf("/r/c%d", i), Label: fmt.Sprintf("c%d", i)})
	}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = typeKeys(mm.(HopModel), "c")
	for range 3 {
		mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mm.(HopModel)
	}
	before := m.View()
	cursorBefore := m.cursor
	mm, _ = m.Update(resolvedMsg{gen: 1})
	m = mm.(HopModel)
	if m.cursor != cursorBefore || m.View() != before {
		t.Fatalf("resolvedMsg must not move the cursor for a plain query: cursor %d -> %d", cursorBefore, m.cursor)
	}
	if m.idsPending() {
		t.Fatal("ids must be marked resolved")
	}
}

func TestReloadResetsResolvedAndDiscardsStale(t *testing.T) {
	cands := []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/a", Label: "a"}}
	cfg := config.Config{Root: "/r", SearchPaths: []string{"/r"}, DefaultHost: "github.com", CloneProtocol: "https"}
	m := NewHop(cfg, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(resolvedMsg{gen: 1})
	m = mm.(HopModel)
	if m.idsPending() {
		t.Fatal("generation 1 resolved")
	}
	// Reload: a new generation begins; the old resolved state must not leak
	// into it, and the old generation's resolvedMsg must be discarded.
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	if !m.idsPending() {
		t.Fatal("reload must reset the resolved state")
	}
	mm, _ = m.Update(cmd()) // loadedMsg gen 2
	m = mm.(HopModel)
	mm, _ = m.Update(resolvedMsg{gen: 1}) // stale
	m = mm.(HopModel)
	if !m.idsPending() {
		t.Fatal("a stale resolvedMsg must be discarded")
	}
	m = typeKeys(m, "o/api")
	if m.cloneRow != nil || !strings.Contains(m.View(), "resolving remotes...") {
		t.Fatalf("URL query right after reload must be gated:\n%s", m.View())
	}
	mm, _ = m.Update(resolvedMsg{gen: 2})
	m = mm.(HopModel)
	if m.idsPending() || m.cloneRow == nil {
		t.Fatal("generation 2 resolvedMsg must apply")
	}
}

func TestResolveCancelledOnReloadAndQuit(t *testing.T) {
	cands := []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/a", Label: "a"}}
	newBlocked := func() (HopModel, tea.Cmd) {
		m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{resolveBlocks: true}, log.New(io.Discard, "", 0), false)
		// The query names a repository, so the load starts the resolver.
		m.input.SetValue("https://github.com/o/r")
		mm, cmd := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
		if cmd == nil || mm.(HopModel).resolveStarted != 1 {
			t.Fatal("loadedMsg must start the resolver for a repository query")
		}
		return mm.(HopModel), cmd
	}
	// loadedMsg starts the background passes as a tea.Batch: run its
	// commands and report only the resolver's message, which is what the
	// blocked fake holds back until cancelled.
	run := func(cmd tea.Cmd) chan tea.Msg {
		done := make(chan tea.Msg, 4)
		go func() {
			msg := cmd()
			batch, ok := msg.(tea.BatchMsg)
			if !ok {
				done <- msg
				return
			}
			for _, c := range batch {
				if c == nil {
					continue
				}
				if r, isResolve := c().(resolvedMsg); isResolve {
					done <- r
				}
			}
		}()
		return done
	}
	wait := func(step string, done chan tea.Msg) {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not release the blocked resolver", step)
		}
	}

	m, cmd := newBlocked()
	done := run(cmd)
	select {
	case <-done:
		t.Fatal("resolver must block until cancelled")
	case <-time.After(20 * time.Millisecond):
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR}) // startLoad cancels the old resolver
	wait("reload", done)

	m, cmd = newBlocked()
	done = run(cmd)
	m.cancelAll() // quitting stops it too
	wait("cancelAll", done)

	// A successful open quits the picker: that path must cancel the resolver
	// as well (it is the most common way to leave).
	m, cmd = newBlocked()
	done = run(cmd)
	mm, quit := m.Update(doneMsg{err: nil})
	if quit == nil || !mm.(HopModel).quit {
		t.Fatal("doneMsg must quit")
	}
	wait("successful open", done)
}

func TestResolvedMsgSurvivesBranchScreen(t *testing.T) {
	// Entering the branch screen before the identities arrive must not
	// swallow the one-shot resolvedMsg.
	cands := []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/a", Label: "a"}}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	if m.wt == nil {
		t.Fatal("precondition: branch screen open")
	}
	mm, _ = m.Update(resolvedMsg{gen: 1})
	m = mm.(HopModel)
	if m.idsPending() {
		t.Fatal("resolvedMsg must be applied while the branch screen is open")
	}
}

func TestResolvingLineCountsAgainstViewport(t *testing.T) {
	// The "resolving remotes..." line eats one list row; the geometry must
	// account for it or the list overflows the screen and scrolling drifts.
	var cands []hop.Candidate
	for i := range 30 {
		cands = append(cands, hop.Candidate{Kind: hop.KindRepo, Path: fmt.Sprintf("/r/c%02d", i), Label: fmt.Sprintf("o/c%02d", i)})
	}
	cfg := config.Config{SearchPaths: []string{"/r"}, DefaultHost: "github.com", CloneProtocol: "https"}
	m := NewHop(cfg, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 10}) // 6 rows without extra header lines
	m = typeKeys(mm.(HopModel), "o/c")                                      // parses as a clone target AND fuzzy-matches
	count := func() int {
		n := 0
		for l := range strings.SplitSeq(m.View(), "\n") {
			if strings.Contains(l, "o/c") && !strings.HasPrefix(l, "hop>") {
				n++
			}
		}
		return n
	}
	if !strings.Contains(m.View(), "resolving remotes...") || count() != 5 {
		t.Fatalf("resolving must shrink the list to 5 rows, got %d:\n%s", count(), m.View())
	}
	mm, _ = m.Update(resolvedMsg{gen: 1})
	m = mm.(HopModel)
	if strings.Contains(m.View(), "resolving remotes...") || count() != 6 {
		t.Fatalf("after resolving the list must be 6 rows again, got %d:\n%s", count(), m.View())
	}
}

func TestTreeOpenReposFirst(t *testing.T) {
	// With an empty query the top of the list is "what is open right now":
	// the pinned current group, repo groups open as a workspace, then the
	// standalone workspace rows. An orphan worktree stays put even when
	// open (its state arrives after the first paint), and an open worktree
	// must not lift its closed repository.
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/a", Label: "rA", OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/b", Label: "rB", OpenState: hop.OpenOpen, OpenWorkspaceID: "w1"},
		{Kind: hop.KindRepo, Path: "/r/c", Label: "rC", OpenState: hop.OpenClosed, Current: true},
		{Kind: hop.KindRepo, Path: "/r/e", Label: "rE", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/e1", Label: "lblE", Branch: "wE", RepoRoot: "/r/e", RepoLabel: "rE", OpenState: hop.OpenOpen, OpenWorkspaceID: "w2"},
		{Kind: hop.KindWorktree, Path: "/w/o", Label: "lblO", Branch: "wO", RepoRoot: "/x", RepoLabel: "x", OpenState: hop.OpenOpen, OpenWorkspaceID: "w3"},
		{Kind: hop.KindWorkspace, Path: "", Label: "wsS", OpenState: hop.OpenOpen, OpenWorkspaceID: "w4"},
	}, config.Config{SearchPaths: []string{"/r"}})
	labels := []string{"rA", "rB", "rC", "rE", "wE", "wO", "wsS"}
	var order []string
	for l := range strings.SplitSeq(m.View(), "\n") {
		for _, lb := range labels {
			if strings.Contains(l, lb) {
				order = append(order, lb)
			}
		}
	}
	want := "rC rB wsS rA rE wE wO" // current, open repos, workspaces, rest
	if strings.Join(order, " ") != want {
		t.Fatalf("order=%v (want %s)\n%s", order, want, m.View())
	}
}

func TestFilterRecordsMatchPositions(t *testing.T) {
	// The filter records which runes matched, split into label and branch,
	// so the view can highlight them.
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/big", Label: "acme/bigbroker", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/f", Label: "wtFLK", Branch: "fix-flaky", RepoRoot: "/r/big", RepoLabel: "acme/bigbroker", OpenState: hop.OpenClosed},
	}
	m := newHopWith(t, cands, config.Config{SearchPaths: []string{"/r"}})
	set := func(q string) {
		m.input.SetValue(q)
		m.refilter()
	}

	// A branch match: positions land in branch, none in the label.
	set("flaky")
	if mi := m.matches[1]; len(mi.label) != 0 || len(mi.branch) != 5 || mi.branch[0] != 4 {
		t.Fatalf("branch match: %+v", mi)
	}
	// A repo-name match: consecutive positions in the label ("acme/" is 5
	// runes, so "bigbro" starts at 5).
	set("bigbro")
	if mi := m.matches[0]; len(mi.label) != 6 || mi.label[0] != 5 || mi.label[5] != 10 {
		t.Fatalf("label match: %+v", mi)
	}
	// A composite query distributes words across rows and fields.
	set("bigbro flaky")
	if mi := m.matches[0]; len(mi.label) != 6 {
		t.Fatalf("composite parent label: %+v", mi)
	}
	if mi := m.matches[1]; len(mi.branch) != 5 || len(mi.label) != 0 {
		t.Fatalf("composite branch: %+v", mi)
	}
}

func TestRowTruncatedToWidth(t *testing.T) {
	// An overlong row is clipped with an ellipsis: a wrapped row would break
	// the one-line-per-row geometry the viewport math relies on.
	long := "github.com/o/" + strings.Repeat("x", 100)
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/l", Label: long, OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/s", Label: "short", OpenState: hop.OpenClosed},
	}, config.Config{SearchPaths: []string{"/r"}})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = mm.(HopModel)
	for l := range strings.SplitSeq(m.View(), "\n") {
		if w := lipgloss.Width(l); w > 40 {
			t.Fatalf("line exceeds the width (%d): %q", w, l)
		}
		// The overlong label is clipped to the left column (ellipsis) so the
		// path column still fits on the line.
		if strings.Contains(l, "github.com/o/") && (!strings.Contains(l, "…") || !strings.Contains(l, "/r/l")) {
			t.Fatalf("overlong label must be clipped before the path column: %q", l)
		}
	}
}

func TestWorktreeModeJumpsFromWorktreeOutsideSearchPaths(t *testing.T) {
	// The current row is a worktree whose main checkout lies outside the
	// search paths (no repo row): its own RepoRoot still names the
	// repository to create the worktree from.
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/a", Label: "o/a", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/x", Label: "/w/x", Branch: "feat", RepoRoot: "/out/repo", RepoLabel: "/out/repo", OpenState: hop.OpenClosed, Current: true},
	}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), true)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.wt == nil || m.wt.repo != "/out/repo" {
		t.Fatalf("must open the branch screen for the worktree's repository: wt=%+v", m.wt)
	}
}

func TestCounterRuleUnderInput(t *testing.T) {
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/a", Label: "rA", OpenState: hop.OpenClosed},
	}, config.Config{SearchPaths: []string{"/r"}})
	lines := strings.Split(m.View(), "\n")
	// 0: prompt+query, 1: counter + rule, 2: key hints.
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "hop> ") {
		t.Fatalf("prompt line: %q", lines)
	}
	if !strings.Contains(lines[1], "1/1") || !strings.Contains(lines[1], "─") {
		t.Fatalf("counter/rule line: %q", lines)
	}
	if last := lines[len(lines)-1]; strings.Contains(last, "1/1") {
		t.Fatalf("the counter must no longer be the footer: %q", last)
	}
	// The hint line carries key chips and, like list rows, is clipped to the
	// width (at 100 columns the tail hints are cut).
	if !strings.Contains(lines[2], " enter ") || !strings.Contains(lines[2], " tab ") {
		t.Fatalf("hints must carry key chips: %q", lines[2])
	}
	if lipgloss.Width(lines[2]) > 100 {
		t.Fatalf("hint line must be clipped: %q", lines[2])
	}
}

func TestPathColumnAligned(t *testing.T) {
	// Every row's dim absolute path starts at the same column, aligned to
	// the widest left cell; rows without a path (path-unknown workspaces)
	// have no column.
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/aa", Label: "aa", OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/a-much-longer-name", Label: "a-much-longer-name", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorkspace, Path: "", Label: "wsX", OpenState: hop.OpenOpen, OpenWorkspaceID: "w1"},
	}, config.Config{SearchPaths: []string{"/r"}})
	col := -1
	for l := range strings.SplitSeq(m.View(), "\n") {
		i := strings.Index(l, "/r/")
		switch {
		case strings.Contains(l, "wsX"):
			if i >= 0 {
				t.Fatalf("path-less row must have no column: %q", l)
			}
		case i >= 0:
			if col == -1 {
				col = i
			} else if i != col {
				t.Fatalf("path columns misaligned (%d vs %d):\n%s", col, i, m.View())
			}
		}
	}
	if col == -1 {
		t.Fatalf("no path column found:\n%s", m.View())
	}
}

func TestSelectedRowKeepsMarkerAndText(t *testing.T) {
	// The cursor row is emphasized through text styling (bold segments, the
	// "> " marker), never through padding or content changes: the row text
	// must be byte-identical to an unselected row's, marker aside.
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/j", Label: "日本語リポジトリ", OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/k", Label: "rK", OpenState: hop.OpenClosed},
	}
	m := newHopWith(t, cands, config.Config{SearchPaths: []string{"/r"}})
	row := func() string {
		for l := range strings.SplitSeq(m.View(), "\n") {
			if strings.Contains(l, "日本語リポジトリ") {
				return l
			}
		}
		return ""
	}
	// The cursor row is padded to the width (it carries the backdrop);
	// the text itself must stay byte-identical, marker and padding aside.
	sel := strings.TrimRight(row(), " ")
	if !strings.HasPrefix(sel, "> ") {
		t.Fatalf("selected: %q", sel)
	}
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(HopModel)
	unsel := strings.TrimRight(row(), " ")
	if !strings.HasPrefix(unsel, "  ") || strings.TrimPrefix(sel, "> ") != strings.TrimPrefix(unsel, "  ") {
		t.Fatalf("row text must not change with selection:\nsel=%q\nuns=%q", sel, unsel)
	}
}

func TestWorktreeEnterQueuedUntilStates(t *testing.T) {
	// Enter on a worktree row before the authoritative herdr states arrive
	// must not act on the provisional state: it queues, and runs with the
	// authoritative workspace id once wtStateMsg lands (~150ms in practice).
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/a", Label: "a"},
		{Kind: hop.KindWorktree, Path: "/w/x", Label: "wtX", Branch: "b", RepoRoot: "/r/a", RepoLabel: "a", OpenState: hop.OpenClosed},
	}
	fc := &fakeClient{}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, fc, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}) // onto the worktree row
	m = mm.(HopModel)

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || !m.pending || m.queuedWorktree != "/w/x" {
		t.Fatalf("enter must queue: cmd=%v pending=%v queued=%q", cmd != nil, m.pending, m.queuedWorktree)
	}
	// A stale generation's states are discarded and must not run the queue.
	mm, cmd = m.Update(wtStateMsg{gen: 0})
	m = mm.(HopModel)
	if cmd != nil || m.queuedWorktree != "/w/x" {
		t.Fatal("stale wtStateMsg must be ignored")
	}
	// The authoritative states arrive: the worktree is actually open in w9
	// (e.g. as a plain workspace, which the provisional state cannot see).
	mm, cmd = m.Update(wtStateMsg{gen: 1, states: hop.WorktreeStateResult{
		Open: map[string]string{"/w/x": "w9"},
		OK:   map[string]bool{"/r/a": true},
	}})
	m = mm.(HopModel)
	if cmd == nil || m.queuedWorktree != "" {
		t.Fatal("queued open must run on wtStateMsg")
	}
	if msg, ok := cmd().(doneMsg); !ok || msg.err != nil {
		t.Fatalf("done: %+v", msg)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "focus w9" {
		t.Fatalf("must focus the authoritative workspace, got %v", fc.calls)
	}
}

func TestWorktreeUnknownBadgeAndRefusal(t *testing.T) {
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindWorktree, Path: "/w/x", Label: "wtX", Branch: "b", RepoRoot: "/r/a", RepoLabel: "a", OpenState: hop.OpenUnknown},
	}, config.Config{SearchPaths: []string{"/r"}})
	v := m.View()
	if !strings.Contains(v, "?") {
		t.Fatalf("worktree with unknown open state must show the glyph:\n%s", v)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd == nil {
		t.Fatal("enter returns the open cmd")
	}
	if msg, ok := cmd().(doneMsg); !ok || !errors.Is(msg.err, hop.ErrWorktreeStateUnknown) {
		t.Fatalf("open must refuse an unknown worktree: %+v", msg)
	}
}

// newHopWith is a picker over a fixed candidate list, sized for tests, with
// the remote identities already resolved (any RepoID set on cands stands)
// and the constructed worktree open states treated as final.
func newHopWith(t *testing.T, cands []hop.Candidate, cfg config.Config) HopModel {
	t.Helper()
	m := NewHop(cfg, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	hm := mm.(HopModel)
	hm.wtStateGen = hm.loadGen // the constructed open states are authoritative
	mm, _ = hm.Update(resolvedMsg{gen: 1})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return mm.(HopModel)
}

func TestFilterCursorStaysOnRepoWhenItMatches(t *testing.T) {
	// "api" matches the repo scattered (a-p-i) and the worktree label
	// consecutively, so the worktree scores higher — but the user named the
	// repo, and Enter must not open a worktree.
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/api", Label: "acme/a-p-i"},
		{Kind: hop.KindWorktree, Path: "/w/f", Label: "api-feature", Branch: "feat", RepoRoot: "/r/api", RepoLabel: "acme/a-p-i"},
	}, config.Config{SearchPaths: []string{"/r"}})
	m.input.SetValue("api")
	m.refilter()
	if c, ok := m.selected(); !ok || c.Kind != hop.KindRepo {
		t.Fatalf("cursor must stay on the repo: %+v\n%s", c, m.View())
	}
	if m.rowCount() != 2 { // the whole group is still shown
		t.Fatalf("rows: %d\n%s", m.rowCount(), m.View())
	}
}

func TestFilterCursorOnWorktreeDespiteNegativeScore(t *testing.T) {
	// A sparse fuzzy match is valid but can score zero or below; the cursor
	// must still land on the matched worktree, not stay on a repo that did
	// not match at all.
	const wtLabel, branch, query = "x-aaaaaaaaaa-z", "feat", "xz"
	if s, ok := fuzzyScore([]rune(query), []rune(wtLabel+" "+branch)); !ok || s > 0 {
		t.Fatalf("fixture must be a valid non-positive match, got score=%d ok=%v", s, ok)
	}
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/repo", Label: "acme/repo"},
		{Kind: hop.KindWorktree, Path: "/w/x", Label: wtLabel, Branch: branch, RepoRoot: "/r/repo", RepoLabel: "acme/repo"},
	}, config.Config{SearchPaths: []string{"/r"}})
	m.input.SetValue(query)
	m.refilter()
	if c, ok := m.selected(); !ok || c.Kind != hop.KindWorktree {
		t.Fatalf("cursor must be on the matched worktree: %+v\n%s", c, m.View())
	}
}

func TestWorktreeModeJumpsToCurrentRepo(t *testing.T) {
	// prefix+t with a recognizable current repository skips the repo picker
	// and opens its branch screen directly; esc falls back to the picker,
	// and a later reload must not jump again.
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/a", Label: "o/a", OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/b", Label: "o/b", OpenState: hop.OpenClosed, Current: true},
	}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), true)
	mm, cmd := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.wt == nil || m.wt.repo != "/r/b" || m.wt.label != "o/b" || cmd == nil {
		t.Fatalf("must open the branch screen for the current repo: wt=%+v", m.wt)
	}
	// esc (once the refs have loaded) returns to the repo picker.
	mm, _ = m.Update(refsLoadedMsg{op: m.wt.op})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt != nil {
		t.Fatal("esc must return to the picker")
	}
	// A reload (new generation) stays on the picker.
	mm, reload := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	mm, _ = m.Update(reload())
	m = mm.(HopModel)
	if m.wt != nil {
		t.Fatal("a reload must not jump back to the branch screen")
	}

	// Without a recognizable current repository the picker shows as before.
	m2 := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), true)
	mm, _ = m2.Update(loadedMsg{cands: []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/a", Label: "o/a", OpenState: hop.OpenClosed}}, gen: 1, occupancy: hop.Occupancy{OK: true}})
	if mm.(HopModel).wt != nil {
		t.Fatal("no current repo: must stay on the picker")
	}
}

func TestPinFromSubdirectoryPane(t *testing.T) {
	// The focused workspace's pane sits in a subdirectory of a checkout, so
	// its row merges with nothing: the pin must map it to the containing
	// repo, on path boundaries (/r/api must not claim /r/api-old/sub).
	cands := func(cur string) []hop.Candidate {
		return []hop.Candidate{
			{Kind: hop.KindRepo, Path: "/r/api", Label: "repoAPI"},
			{Kind: hop.KindRepo, Path: "/r/api-old", Label: "repoOLD"},
			{Kind: hop.KindWorkspace, Path: cur, Label: cur, OpenState: hop.OpenOpen, OpenWorkspaceID: "w1", Current: true},
		}
	}
	for cur, want := range map[string]string{
		"/r/api-old/sub":      "repoOLD",
		"/r/api/internal/srv": "repoAPI",
	} {
		m := newHopWith(t, cands(cur), config.Config{SearchPaths: []string{"/r"}})
		if c, ok := m.rowAt(0); !ok || c.Label != want {
			t.Errorf("cur=%s: first row %+v (want %s)\n%s", cur, c, want, m.View())
		}
	}
}

func TestCloneURLKeepsGroups(t *testing.T) {
	// Pasting a clone URL of an existing checkout must show its group like
	// a normal query would: repo first, worktree indented under it.
	m := newHopWith(t, []hop.Candidate{
		{Kind: hop.KindWorktree, Path: "/w/f", Label: "wtR", Branch: "feat", RepoRoot: "/r/r", RepoLabel: "o/r", RepoID: "github.com/o/r"},
		{Kind: hop.KindRepo, Path: "/r/r", Label: "o/r", RepoID: "github.com/o/r"},
		{Kind: hop.KindRepo, Path: "/r/other", Label: "o/other", RepoID: "github.com/o/other"},
	}, config.Config{SearchPaths: []string{"/r"}, DefaultHost: "github.com", CloneProtocol: "https"})
	m.input.SetValue("https://github.com/o/r")
	m.refilter()
	first, _ := m.rowAt(0)
	second, _ := m.rowAt(1)
	if first.Kind != hop.KindRepo || first.Path != "/r/r" || second.Path != "/w/f" || m.cursor != 0 {
		t.Fatalf("rows: %+v / %+v cursor=%d\n%s", first, second, m.cursor, m.View())
	}
	if !strings.Contains(m.View(), "└─ feat") {
		t.Fatalf("worktree must render as an indented child:\n%s", m.View())
	}
}

func mkRepo(p string) error {
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		return err
	}
	return nil
}

// hopForDelete builds a picker over one repository with an open worktree
// (w1, workspace ws1), a closed one (w2) and the current one (w3), with
// the authoritative open states in place. It returns the model and its fakes.
func hopForDelete(t *testing.T) (HopModel, *fakeClient, *fakeCloner) {
	t.Helper()
	fc, git := &fakeClient{}, &fakeCloner{}
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/api", Label: "acme/api", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/one", Label: "one", Branch: "feat-one", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenOpen, OpenWorkspaceID: "ws1", OpenCount: 1},
		{Kind: hop.KindWorktree, Path: "/w/two", Label: "two", Branch: "feat-two", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/three", Label: "three", Branch: "feat-three", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenOpen, OpenWorkspaceID: "ws3", OpenCount: 1, Current: true},
		{Kind: hop.KindWorktree, Path: "/w/four", Label: "four", Branch: "feat-four", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenOpen, OpenWorkspaceID: "ws4", OpenCount: 2},
		{Kind: hop.KindWorktree, Path: "/w/five", Label: "five", Branch: "feat-five", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenClosed},
		// The pane the picker was opened from sits in a subdirectory of
		// /w/five: a plain workspace, so a row of its own.
		{Kind: hop.KindWorkspace, Path: "/w/five/cmd/x", Label: "x", OpenState: hop.OpenOpen, OpenWorkspaceID: "ws5", OpenCount: 1, Current: true},
		{Kind: hop.KindWorktree, Path: "/w/six", Label: "six", Branch: "feat-six", RepoRoot: "/r/api", RepoLabel: "acme/api", OpenState: hop.OpenClosed},
		// Another workspace (ws6, rows as /w/six/docs) has an inactive pane
		// in another tab inside /w/six — a pane the rows never show.
		{Kind: hop.KindWorkspace, Path: "/w/other", Label: "other", OpenState: hop.OpenOpen, OpenWorkspaceID: "ws6", OpenCount: 1},
	}
	// Where the panes are, as the snapshot tells it: the open worktree's
	// own pane sits in a subdirectory of its checkout (fine), the current
	// workspace's pane in /w/five, ws6's hidden pane in /w/six.
	occ := hop.Occupancy{OK: true, Occupants: []hop.Occupant{
		{Path: "/w/one", WorkspaceID: "ws1", Managed: true}, {Path: "/w/one/internal", WorkspaceID: "ws1", Managed: true},
		{Path: "/w/three", WorkspaceID: "ws3", Current: true, Managed: true},
		{Path: "/w/four", WorkspaceID: "ws4", Managed: true}, {Path: "/w/four", WorkspaceID: "ws4b"},
		{Path: "/w/five/cmd/x", WorkspaceID: "ws5", Current: true},
		{Path: "/w/other", WorkspaceID: "ws6"}, {Path: "/w/six/docs", WorkspaceID: "ws6"},
	}}
	// What herdr answers when the deletion re-checks the state: the same
	// picture the rows were built from, until a test changes it.
	str := func(s string) *string { return &s }
	fc.snap = herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "ws1", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/one", IsLinkedWorktree: true}},
			{ID: "ws3", Focused: true, Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/three", IsLinkedWorktree: true}},
			{ID: "ws4", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/four", IsLinkedWorktree: true}},
			{ID: "ws4b", ActiveTabID: "t4b"},
			{ID: "ws5", ActiveTabID: "t5"},
			{ID: "ws6", ActiveTabID: "t6"},
		},
		Layouts: []herdr.Layout{{TabID: "t4b", WorkspaceID: "ws4b", FocusedPaneID: "p4b"}, {TabID: "t5", WorkspaceID: "ws5", FocusedPaneID: "p5"}, {TabID: "t6", WorkspaceID: "ws6", FocusedPaneID: "p6"}},
		Panes: []herdr.Pane{
			{ID: "p1", WorkspaceID: "ws1", Cwd: str("/w/one/internal")},
			{ID: "p3", WorkspaceID: "ws3", Cwd: str("/w/three")},
			{ID: "p4b", WorkspaceID: "ws4b", Cwd: str("/w/four")},
			{ID: "p5", WorkspaceID: "ws5", Cwd: str("/w/five/cmd/x")},
			{ID: "p6", WorkspaceID: "ws6", Cwd: str("/w/other")},
			{ID: "p6b", WorkspaceID: "ws6", Cwd: str("/w/six/docs")},
		},
	}
	fc.worktrees = map[string][]herdr.Worktree{"/r/api": {
		{Path: "/w/one", Branch: "feat-one", IsLinkedWorktree: true, OpenWorkspaceID: str("ws1")},
		{Path: "/w/two", Branch: "feat-two", IsLinkedWorktree: true},
		{Path: "/w/three", Branch: "feat-three", IsLinkedWorktree: true, OpenWorkspaceID: str("ws3")},
		{Path: "/w/four", Branch: "feat-four", IsLinkedWorktree: true, OpenWorkspaceID: str("ws4")},
		{Path: "/w/five", Branch: "feat-five", IsLinkedWorktree: true},
		{Path: "/w/six", Branch: "feat-six", IsLinkedWorktree: true},
	}}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, fc, git, log.New(io.Discard, "", 0), false)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: occ})
	hm := mm.(HopModel)
	hm.wtStateGen = hm.loadGen
	mm, _ = hm.Update(resolvedMsg{gen: 1})
	mm, _ = mm.(HopModel).Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return mm.(HopModel), fc, git
}

// cursorOnPath moves the cursor to the row whose candidate has path.
func cursorOnPath(t *testing.T, m *HopModel, path string) {
	t.Helper()
	for i := range m.view {
		if c, ok := m.rowAt(i); ok && c.Path == path {
			m.cursor = i
			return
		}
	}
	t.Fatalf("no row for %s", path)
}

func TestDeleteWorktreeAsksThenRemovesThroughHerdrWhenOpen(t *testing.T) {
	m, fc, git := hopForDelete(t)
	m.input.SetValue("feat-one")
	m.refilter()
	cursorOnPath(t, &m, "/w/one")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = mm.(HopModel)
	if cmd != nil || m.confirm == nil || m.pending {
		t.Fatalf("ctrl-d must only ask: confirm=%v pending=%v", m.confirm != nil, m.pending)
	}
	if v := stripANSI(m.View()); !strings.Contains(v, "delete worktree feat-one? (y/N)") || !strings.Contains(v, "the branch is kept") {
		t.Fatalf("question missing:\n%s", v)
	}
	if len(fc.calls)+len(git.removed) != 0 {
		t.Fatal("nothing may be removed before the answer")
	}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = mm.(HopModel)
	if cmd == nil || !m.pending || m.confirm != nil {
		t.Fatalf("y must start the removal: cmd=%v pending=%v", cmd != nil, m.pending)
	}
	if m.input.Prompt != "hop> " || m.input.Value() != "" {
		t.Errorf("a yes brings the prompt back and clears the query (it named the deleted worktree): %q %q", m.input.Prompt, m.input.Value())
	}
	msg := cmd()
	if got := fc.calls; len(got) != 1 || got[0] != "wtremove ws1 force=false" {
		t.Fatalf("open worktree goes through herdr, without force: %v", got)
	}
	if len(git.removed) != 0 {
		t.Fatalf("git must not be used for an open worktree: %v", git.removed)
	}
	mm, reload := m.Update(msg)
	m = mm.(HopModel)
	if reload == nil || !m.loading || m.pending {
		t.Fatalf("a removal must be followed by a reload: loading=%v", m.loading)
	}
	// The reload lands; the cursor goes to the worktree's repository.
	mm, _ = m.Update(loadedMsg{cands: []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/zzz", Label: "acme/zzz", OpenState: hop.OpenClosed},
		{Kind: hop.KindRepo, Path: "/r/api", Label: "acme/api", OpenState: hop.OpenClosed},
	}, gen: m.loadGen, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if c, ok := m.selected(); !ok || c.Path != "/r/api" {
		t.Errorf("cursor must land on the repository, got %+v", c)
	}
}

func TestDeleteWorktreeThroughGitWhenNoWorkspaceIsOpen(t *testing.T) {
	m, fc, git := hopForDelete(t)
	cursorOnPath(t, &m, "/w/two")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd := mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Y'}})
	m = mm.(HopModel)
	if cmd == nil {
		t.Fatal("Y must start the removal")
	}
	cmd()
	if got := git.removed; len(got) != 1 || got[0] != "/r/api /w/two force=false" {
		t.Fatalf("closed worktree goes through git, without force: %v", got)
	}
	if len(fc.calls) != 0 {
		t.Fatalf("herdr must not be used for a closed worktree: %v", fc.calls)
	}
}

func TestDeleteWorktreeAnyOtherKeyIsNo(t *testing.T) {
	m, fc, git := hopForDelete(t)
	m.input.SetValue("two")
	m.refilter()
	cursorOnPath(t, &m, "/w/two")
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'n'}}, {Type: tea.KeyEsc}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'x'}}} {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
		m = mm.(HopModel)
		if m.confirm == nil {
			t.Fatal("ctrl-d must ask")
		}
		mm, cmd := m.Update(key)
		m = mm.(HopModel)
		if cmd != nil || m.confirm != nil || m.pending || m.quit {
			t.Fatalf("%v must be a no: cmd=%v confirm=%v pending=%v quit=%v", key, cmd != nil, m.confirm != nil, m.pending, m.quit)
		}
		if m.input.Value() != "two" || m.input.Prompt != "hop> " {
			t.Fatalf("query and prompt must come back after %v: %q %q", key, m.input.Value(), m.input.Prompt)
		}
	}
	if len(fc.calls)+len(git.removed) != 0 {
		t.Fatalf("nothing may be removed: %v %v", fc.calls, git.removed)
	}
}

func TestDeleteWorktreeRefusals(t *testing.T) {
	m, fc, git := hopForDelete(t)
	try := func(path string) HopModel {
		cursorOnPath(t, &m, path)
		mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
		if cmd != nil || mm.(HopModel).confirm != nil {
			t.Fatalf("%s: must be refused without asking", path)
		}
		out := mm.(HopModel)
		if out.errMsg == "" {
			t.Fatalf("%s: a refusal must say why", path)
		}
		return out
	}
	if got := try("/r/api"); !strings.Contains(got.errMsg, "only worktree rows") {
		t.Errorf("repository row: %q", got.errMsg)
	}
	m.errMsg = ""
	if got := try("/w/three"); !strings.Contains(got.errMsg, "worktree you are in") {
		t.Errorf("current worktree: %q", got.errMsg)
	}
	m.errMsg = ""
	if got := try("/w/five"); !strings.Contains(got.errMsg, "worktree you are in") {
		t.Errorf("worktree containing the pane's directory: %q", got.errMsg)
	}
	m.errMsg = ""
	if got := try("/w/four"); !strings.Contains(got.errMsg, "several workspaces") || !strings.Contains(got.errMsg, "2 are open") {
		t.Errorf("shared worktree: %q", got.errMsg)
	}
	m.errMsg = ""
	if got := try("/w/six"); !strings.Contains(got.errMsg, "another workspace has a pane in this worktree") || !strings.Contains(got.errMsg, "/w/six/docs") {
		t.Errorf("worktree with another workspace's pane inside: %q", got.errMsg)
	}
	m.errMsg = ""
	// Snapshot failed this load: Current and OpenCount are unknown for
	// every row, even after the worktree states have arrived.
	saved := m.occupancy
	m.occupancy = hop.Occupancy{}
	if got := try("/w/two"); !strings.Contains(got.errMsg, "herdr snapshot failed") {
		t.Errorf("snapshot failed: %q", got.errMsg)
	}
	// A pane whose directory herdr did not report might be anywhere.
	m.occupancy = saved
	m.occupancy.UnknownPanes = 1
	m.errMsg = ""
	if got := try("/w/two"); !strings.Contains(got.errMsg, "where every pane is") {
		t.Errorf("unknown pane: %q", got.errMsg)
	}
	m.occupancy = saved
	m.errMsg = ""
	// Authoritative open states not yet in: neither tool can be chosen.
	m.wtStateGen = m.loadGen - 1
	if got := try("/w/two"); !strings.Contains(got.errMsg, "still loading") {
		t.Errorf("states pending: %q", got.errMsg)
	}
	m.wtStateGen = m.loadGen
	m.errMsg = ""
	// Unknown open state (herdr worktree list failed).
	for i := range m.cands {
		if m.cands[i].Path == "/w/two" {
			m.cands[i].OpenState = hop.OpenUnknown
		}
	}
	if got := try("/w/two"); !strings.Contains(got.errMsg, "open state unknown") {
		t.Errorf("unknown state: %q", got.errMsg)
	}
	if len(fc.calls)+len(git.removed) != 0 {
		t.Fatalf("nothing may be removed: %v %v", fc.calls, git.removed)
	}
}

func TestDeleteWorktreeDirtyCheckoutIsExplained(t *testing.T) {
	m, fc, git := hopForDelete(t)
	fc.removeErr = errors.New("herdr worktree: fatal: '/w/one' contains modified or untracked files, use --force to delete it (dirty_worktree_requires_force)")
	m.input.SetValue("feat-one")
	m.refilter()
	cursorOnPath(t, &m, "/w/one")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd := mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm, reload := mm.(HopModel).Update(cmd())
	m = mm.(HopModel)
	if reload != nil || m.loading || m.pending {
		t.Fatal("a failed removal must not reload")
	}
	if !strings.Contains(m.errMsg, "modified or untracked files") || !strings.Contains(m.errMsg, "commit, stash or clean") {
		t.Errorf("advice expected, got %q", m.errMsg)
	}
	// No reload happened, so the list must already match the (now empty)
	// input: every row, no leftover filter or highlight.
	if m.input.Value() != "" || len(m.view) != len(m.cands) || len(m.matches) != 0 {
		t.Errorf("input %q, view %d/%d rows, %d highlights: the list must match the input", m.input.Value(), len(m.view), len(m.cands), len(m.matches))
	}
	// git's wording for a closed worktree gets the same advice.
	m.errMsg = ""
	git.removeErr = errors.New("git worktree remove: fatal: '/w/two' contains modified or untracked files, use --force to delete it")
	cursorOnPath(t, &m, "/w/two")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd = mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm, _ = mm.(HopModel).Update(cmd())
	if got := mm.(HopModel).errMsg; !strings.Contains(got, "commit, stash or clean") {
		t.Errorf("advice expected, got %q", got)
	}
}

func TestDeleteWorktreeIgnoresTypingWhileRemoving(t *testing.T) {
	// Between the yes and the reload the query is empty on purpose; keys
	// typed in that window must not become a filter the reload then hides
	// the repository behind.
	m, _, _ := hopForDelete(t)
	m.input.SetValue("feat-one")
	m.refilter()
	cursorOnPath(t, &m, "/w/one")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd := mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = mm.(HopModel)
	if m.input.Value() != "" {
		t.Fatalf("typing while the removal runs must be ignored, got %q", m.input.Value())
	}
	mm, _ = m.Update(cmd())
	m = mm.(HopModel)
	if !m.loading || m.input.Value() != "" {
		t.Errorf("the reload must see an empty query: loading=%v query=%q", m.loading, m.input.Value())
	}
}

func TestDeleteWorktreeQuestionFitsTheLineAtEveryWidth(t *testing.T) {
	m, _, _ := hopForDelete(t)
	for i := range m.cands {
		if m.cands[i].Path == "/w/two" {
			m.cands[i].Branch = strings.Repeat("very-long-branch-name-", 5)
		}
	}
	m.refilter()
	cursorOnPath(t, &m, "/w/two")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = mm.(HopModel)
	if m.confirm == nil {
		t.Fatal("ctrl-d must ask")
	}
	// The question is rendered per frame from the current width, so a
	// resize during the confirmation is honoured; it never wraps, and it
	// gives up the name before the "(y/N)".
	for _, w := range []int{200, 50, 28, 12, 6} {
		mm, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 20})
		m = mm.(HopModel)
		line := strings.SplitN(stripANSI(m.View()), "\n", 2)[0]
		if lipgloss.Width(line) > w {
			t.Errorf("width %d: input line %d cols: %q", w, lipgloss.Width(line), line)
		}
		p := m.confirmPrompt()
		switch {
		case w >= 200 && !strings.HasPrefix(p, "delete worktree very-long"):
			t.Errorf("width %d: full question expected: %q", w, p)
		case w == 50 && (!strings.HasPrefix(p, "delete very-long") || !strings.HasSuffix(p, "…? (y/N) ")):
			t.Errorf("width %d: shortened name expected: %q", w, p)
		case w == 12 && p != "delete? (y/":
			t.Errorf("width %d: bare question clipped expected: %q", w, p)
		}
	}
}

func TestDeleteWorktreeReChecksHerdrAtYes(t *testing.T) {
	// The rows say /w/two is closed; between the question and the yes
	// another herdr action opens a workspace on it. The deletion must go
	// through herdr with that workspace, not through git on stale rows —
	// and if a pane moved in, must not happen at all.
	m, fc, git := hopForDelete(t)
	cursorOnPath(t, &m, "/w/two")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = mm.(HopModel)
	ws := "ws9"
	fc.worktrees["/r/api"][1].OpenWorkspaceID = &ws
	fc.snap.Workspaces = append(fc.snap.Workspaces, herdr.Workspace{ID: "ws9", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/two", IsLinkedWorktree: true}})
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = mm.(HopModel)
	cmd()
	if got := fc.calls; len(got) != 1 || got[0] != "wtremove ws9 force=false" {
		t.Fatalf("the fresh open state must route through herdr: %v (git: %v)", got, git.removed)
	}
	if len(git.removed) != 0 {
		t.Fatalf("git must not act on the stale closed state: %v", git.removed)
	}
	// Now a pane of yet another workspace is inside: refused, nothing runs.
	// (The first removal's result was not delivered; clear its pending.)
	m.pending = false
	fc.calls = nil
	cwd := "/w/two/pkg"
	fc.snap.Workspaces = append(fc.snap.Workspaces, herdr.Workspace{ID: "ws10", ActiveTabID: "t10"})
	fc.snap.Panes = append(fc.snap.Panes, herdr.Pane{ID: "p10", WorkspaceID: "ws10", Cwd: &cwd})
	cursorOnPath(t, &m, "/w/two")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd = mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm, _ = mm.(HopModel).Update(cmd())
	m = mm.(HopModel)
	if len(fc.calls)+len(git.removed) != 0 || !strings.Contains(m.errMsg, "another workspace has a pane in this worktree") {
		t.Errorf("a pane that moved in must refuse the deletion: calls=%v git=%v err=%q", fc.calls, git.removed, m.errMsg)
	}
	// And a checkout re-created for another branch at the same path is not
	// what was confirmed.
	m.errMsg, m.pending = "", false
	fc.snap.Workspaces = fc.snap.Workspaces[:len(fc.snap.Workspaces)-1]
	fc.snap.Panes = fc.snap.Panes[:len(fc.snap.Panes)-1]
	fc.worktrees["/r/api"][1].Branch = "feat-other"
	cursorOnPath(t, &m, "/w/two")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	mm, cmd = mm.(HopModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm, _ = mm.(HopModel).Update(cmd())
	m = mm.(HopModel)
	if len(fc.calls)+len(git.removed) != 0 || !strings.Contains(m.errMsg, "changed since the list was loaded") {
		t.Errorf("a re-created worktree must refuse the deletion: calls=%v git=%v err=%q", fc.calls, git.removed, m.errMsg)
	}
}

func TestRemoteIdentitiesResolveOnlyForRepositoryQueries(t *testing.T) {
	// Resolving costs one git process per repository; nothing but a query
	// naming a repository or PR reads the result, so no such query, no
	// resolver — not on load, not for name or branch queries, and never in
	// the worktree-mode picker (which offers no clone row).
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/o/api", Label: "o/api", OpenState: hop.OpenClosed},
		{Kind: hop.KindWorktree, Path: "/w/feat", Label: "feat", Branch: "feat", RepoRoot: "/r/o/api", RepoLabel: "o/api", OpenState: hop.OpenClosed},
	}
	git := &fakeCloner{remotes: map[string]string{"/r/o/api": "https://github.com/o/api"}}
	cfg := config.Config{Root: "/r", SearchPaths: []string{"/r"}, DefaultHost: "github.com", CloneProtocol: "https"}
	m := NewHop(cfg, &fakeClient{}, git, log.New(io.Discard, "", 0), false)
	mm, cmd := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.resolveStarted != 0 {
		t.Fatal("load must not start the resolver")
	}
	// tea.Batch collapses to the single remaining command: the worktree
	// states, not the resolver.
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			if c != nil {
				if _, isResolve := c().(resolvedMsg); isResolve {
					t.Fatal("load's batch must not contain the resolver")
				}
			}
		}
	case resolvedMsg:
		t.Fatal("load must not run the resolver")
	}
	m = typeKeys(m, "feat")
	if m.resolveStarted != 0 || !m.idsPending() {
		t.Fatal("a branch query must not start the resolver")
	}
	// A repository-shaped query starts it, once.
	m.input.SetValue("")
	m.refilter()
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o/ap")})
	m = mm.(HopModel)
	if m.resolveStarted != 1 || cmd == nil {
		t.Fatalf("a repository query must start the resolver: started=%d cmd=%v", m.resolveStarted, cmd != nil)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m = mm.(HopModel)
	if m.resolveCancel == nil {
		t.Fatal("typing on must not cancel the running resolver")
	}
	// Its result lands (through the batch the key returned) and the clone
	// row question is settled: o/api exists, so no clone row.
	m = run(t, m, cmd)
	if m.idsPending() || m.cloneRow != nil {
		t.Fatalf("resolved: pending=%v cloneRow=%v", m.idsPending(), m.cloneRow != nil)
	}
	// Resolved once per load: a later repository query asks nothing new.
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = mm.(HopModel)
	if m.resolveStarted != 1 {
		t.Fatal("no second resolver within a load")
	}
	// A reload with the repository query still in the input resolves again.
	mm, reload := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = mm.(HopModel)
	if m.resolveStarted != 0 {
		t.Fatal("reload resets the resolver state")
	}
	_ = reload
	mm, cmd = m.Update(loadedMsg{cands: cands, warn: nil, gen: m.loadGen, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.resolveStarted != m.loadGen {
		t.Fatal("a load landing under a repository query must start the resolver")
	}
}

func TestWorktreeModeNeverResolvesRemoteIdentities(t *testing.T) {
	cands := []hop.Candidate{{Kind: hop.KindRepo, Path: "/r/o/api", Label: "o/api", OpenState: hop.OpenClosed, Current: true}}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}, DefaultHost: "github.com", CloneProtocol: "https"}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), true)
	mm, _ := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.wt == nil {
		t.Fatal("precondition: jumped to the branch screen")
	}
	if m.resolveStarted != 0 {
		t.Fatal("the branch screen jump must not resolve remote identities")
	}
}

func TestWorktreeStatesWaitUntilThePickerIsShown(t *testing.T) {
	// A load that jumps straight to the branch screen (prefix+t in a
	// repository) skips the herdr worktree-list pass — the branch screen
	// never reads the open states — and esc back to the picker starts it.
	cands := []hop.Candidate{
		{Kind: hop.KindRepo, Path: "/r/o/api", Label: "o/api", OpenState: hop.OpenClosed, Current: true},
		{Kind: hop.KindWorktree, Path: "/w/feat", Label: "feat", Branch: "feat", RepoRoot: "/r/o/api", RepoLabel: "o/api", OpenState: hop.OpenClosed},
	}
	fc := &fakeClient{worktrees: map[string][]herdr.Worktree{"/r/o/api": {{Path: "/w/feat", IsLinkedWorktree: true}}}}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, fc, &fakeCloner{}, log.New(io.Discard, "", 0), true)
	mm, cmd := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.wt == nil {
		t.Fatal("precondition: jumped to the branch screen")
	}
	if m.wtStateStarted != 0 {
		t.Fatal("the jump must not start the worktree-state pass")
	}
	// Deliver the branch screen's refs so esc is accepted, and make sure
	// no wtStateMsg is among the load's commands.
	for _, c := range cmd().(tea.BatchMsg) {
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case wtStateMsg:
			t.Fatal("load's batch must not fetch worktree states while on the branch screen")
		case refsLoadedMsg:
			mm, _ = m.Update(msg)
			m = mm.(HopModel)
		}
	}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt != nil || cmd == nil || m.wtStateStarted != 1 {
		t.Fatalf("esc must show the picker and start the pass: wt=%v cmd=%v started=%d", m.wt != nil, cmd != nil, m.wtStateStarted)
	}
	msg, ok := cmd().(wtStateMsg)
	if !ok {
		t.Fatalf("expected wtStateMsg, got %T", msg)
	}
	mm, _ = m.Update(msg)
	m = mm.(HopModel)
	if m.wtStateGen != m.loadGen {
		t.Fatal("the states must be authoritative after the pass")
	}
	// Once done for this load, nothing starts it again.
	if c := m.startWorktreeStates(); c != nil {
		t.Fatal("no second pass within a load")
	}
}

func TestWorktreeStatesStartWithThePickerLoad(t *testing.T) {
	// The ordinary picker still fetches the states right after the load.
	cands := []hop.Candidate{{Kind: hop.KindWorktree, Path: "/w/feat", Label: "feat", Branch: "feat", RepoRoot: "/r/o/api", RepoLabel: "o/api", OpenState: hop.OpenClosed}}
	m := NewHop(config.Config{SearchPaths: []string{"/r"}}, &fakeClient{}, &fakeCloner{}, log.New(io.Discard, "", 0), false)
	mm, cmd := m.Update(loadedMsg{cands: cands, warn: nil, gen: 1, occupancy: hop.Occupancy{OK: true}})
	m = mm.(HopModel)
	if m.wtStateStarted != 1 || cmd == nil {
		t.Fatalf("the picker load must start the pass: started=%d cmd=%v", m.wtStateStarted, cmd != nil)
	}
	if _, ok := cmd().(wtStateMsg); !ok {
		t.Fatal("the load's command must be the worktree-state pass")
	}
}
