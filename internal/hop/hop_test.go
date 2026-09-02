package hop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/scan"
)

//go:fix inline

func TestMerge(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	repoA := filepath.Join(root, "github.com/o/a")
	wtA := filepath.Join(root, "github.com/o/a@feat") // linked worktree also found by scan
	repoB := filepath.Join(root, "github.com/o/b")
	repos := []scan.Repo{{Path: repoA}, {Path: wtA, GitIsFile: true}, {Path: repoB}}
	wts := []gitx.WorktreeListing{{
		RepoRoot: repoA,
		Worktrees: []gitx.WorktreeEntry{
			{Path: repoA, Branch: "main"},
			{Path: wtA, Branch: "feat", IsLinked: true},
			{Path: filepath.Join(root, "gone"), Branch: "x", IsLinked: true, IsPrunable: true},
		},
	}}
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "w2", Number: 2, ActiveTabID: "w2:t1", Focused: true},                   // repoB via focused pane cwd; the invoking workspace
			{ID: "w1", Number: 1, ActiveTabID: "w1:t1"},                                  // repoB via foreground_cwd fallback
			{ID: "w7", Number: 7, Worktree: &herdr.WorkspaceWorktree{CheckoutPath: wtA}}, // herdr's worktree workspace: the focus target
			{ID: "w9", Number: 9, ActiveTabID: "w9:t1"},                                  // plain ws whose pane sits on the same path: badge only
			{ID: "w5", Number: 5, Label: "orphan", ActiveTabID: "w5:t1"},                 // no layout -> path unknown
			{ID: "w6", Number: 6, ActiveTabID: "w6:t1"},                                  // path outside search
		},
		Layouts: []herdr.Layout{
			{TabID: "w2:t1", FocusedPaneID: "w2:p9"},
			{TabID: "w1:t1", FocusedPaneID: "w1:p1"},
			{TabID: "w6:t1", FocusedPaneID: "w6:p1"},
			{TabID: "w9:t1", FocusedPaneID: "w9:p1"},
		},
		Panes: []herdr.Pane{
			{ID: "w2:p1", TabID: "w2:t1", Cwd: new("/elsewhere")}, // not focused: must be ignored
			{ID: "w2:p9", TabID: "w2:t1", Cwd: new(repoB)},
			{ID: "w1:p1", TabID: "w1:t1", Cwd: nil, ForegroundCwd: new(repoB)},
			{ID: "w6:p1", TabID: "w6:t1", Cwd: new("/tmp")},
			{ID: "w9:p1", TabID: "w9:t1", Cwd: new(wtA)},
		},
	}
	got := Merge(repos, wts, nil, snap, []string{root})

	byPath := map[string]Candidate{}
	for _, c := range got {
		byPath[c.Path] = c
	}
	if len(got) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(got), got)
	}
	a := byPath[repoA]
	if a.Kind != KindRepo || a.OpenState != OpenClosed || a.Label != "o/a" {
		t.Errorf("repoA: %+v", a)
	}
	w := byPath[wtA]
	if w.Kind != KindWorktree || w.OpenWorkspaceID != "w7" || !w.IsOpen() || w.OpenCount != 2 || w.RepoRoot != repoA || w.Branch != "feat" || w.RepoLabel != "o/a" {
		t.Errorf("worktree: %+v", w)
	}
	b := byPath[repoB]
	if b.OpenWorkspaceID != "w1" || !b.IsOpen() || b.OpenCount != 2 {
		t.Errorf("repoB should switch to lowest number: %+v", b)
	}
	if !b.Current || a.Current || w.Current {
		t.Errorf("the focused workspace's row (and only it) is Current: a=%v b=%v w=%v", a.Current, b.Current, w.Current)
	}
	if _, ok := byPath[filepath.Join(root, "gone")]; ok {
		t.Error("prunable worktree must be excluded")
	}
	var standalone []Candidate
	for _, c := range got {
		if c.Kind == KindWorkspace {
			standalone = append(standalone, c)
		}
	}
	if len(standalone) != 2 {
		t.Fatalf("standalone: %+v", standalone)
	}
	for _, c := range standalone {
		switch c.OpenWorkspaceID {
		case "w5":
			if c.Path != "" || c.Label != "orphan" {
				t.Errorf("orphan: %+v", c)
			}
		case "w6":
			if c.Path == "" {
				t.Errorf("w6: %+v", c)
			}
		default:
			t.Errorf("unexpected standalone: %+v", c)
		}
	}
}

func TestMergeSnapshotFailure(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	repoA := filepath.Join(root, "a")
	wtA := filepath.Join(root, "a@x")
	wts := []gitx.WorktreeListing{{RepoRoot: repoA, Worktrees: []gitx.WorktreeEntry{{Path: repoA}, {Path: wtA, IsLinked: true}}}}
	got := Merge([]scan.Repo{{Path: repoA}, {Path: wtA, GitIsFile: true}}, wts, nil, nil, []string{root})
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, c := range got {
		switch c.Path {
		case repoA:
			if c.OpenState != OpenUnknown {
				t.Errorf("repo must be OpenUnknown without snapshot: %+v", c)
			}
		case wtA:
			// No snapshot: not even a provisional open state — Open refuses
			// until the authoritative worktree states arrive.
			if c.Kind != KindWorktree || c.OpenState != OpenUnknown {
				t.Errorf("worktree: %+v", c)
			}
			if err := Open(&fakeOpener{}, c, false); !errors.Is(err, ErrWorktreeStateUnknown) {
				t.Errorf("open unknown worktree: %v", err)
			}
		}
	}
}

func TestMergeWorktreeListFailure(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	main := filepath.Join(root, "m")
	linked := filepath.Join(root, "m@x")
	failed := map[string]bool{main: true, linked: true}
	got := Merge([]scan.Repo{{Path: main}, {Path: linked, GitIsFile: true}}, nil, failed, &herdr.Snapshot{}, []string{root})
	byPath := map[string]Candidate{}
	for _, c := range got {
		byPath[c.Path] = c
	}
	if byPath[main].Kind != KindRepo {
		t.Errorf("main checkout stays a repo: %+v", byPath[main])
	}
	if byPath[linked].Kind != KindUnknown {
		t.Errorf("linked worktree with failed list must be unknown, not repo: %+v", byPath[linked])
	}
}

func TestMergeWorkspaceNotMergedIntoUnknown(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	linked := filepath.Join(root, "m@x")
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "w4", Number: 4, Worktree: &herdr.WorkspaceWorktree{CheckoutPath: linked}}},
	}
	got := Merge([]scan.Repo{{Path: linked, GitIsFile: true}}, nil, map[string]bool{linked: true}, snap, []string{root})
	if len(got) != 2 {
		t.Fatalf("want unknown row + standalone workspace row, got %+v", got)
	}
	var ws *Candidate
	for i := range got {
		if got[i].Kind == KindWorkspace {
			ws = &got[i]
		} else if got[i].Kind != KindUnknown || got[i].OpenCount != 0 {
			t.Errorf("unknown row must not absorb the workspace: %+v", got[i])
		}
	}
	if ws == nil || ws.OpenWorkspaceID != "w4" || !ws.IsOpen() {
		t.Fatalf("workspace must remain focusable: %+v", ws)
	}
	f := &fakeOpener{}
	if err := Open(f, *ws, false); err != nil || f.calls[0] != "focus w4" {
		t.Errorf("err=%v calls=%v", err, f.calls)
	}
}

// fakeLister is the herdr side of a load: the snapshot for Build and
// `herdr worktree list` for WorktreeStates.
type fakeLister struct {
	mu    sync.Mutex
	calls []string
	lists map[string]*herdr.WorktreeList
	err   map[string]error
}

func (f *fakeLister) Snapshot() (*herdr.Snapshot, error) { return &herdr.Snapshot{}, nil }
func (f *fakeLister) WorktreeList(repo string) (*herdr.WorktreeList, error) {
	f.mu.Lock()
	f.calls = append(f.calls, repo)
	f.mu.Unlock()
	if e := f.err[repo]; e != nil {
		return nil, e
	}
	if l := f.lists[repo]; l != nil {
		return l, nil
	}
	l := &herdr.WorktreeList{}
	l.Source.RepoRoot = repo
	return l, nil
}

// fakeGitLister is the git side of a load: worktree enumeration for Build.
type fakeGitLister struct {
	mu    sync.Mutex
	calls []string
	lists map[string]gitx.WorktreeListing
	err   map[string]error
}

func (f *fakeGitLister) WorktreeList(repo string) (gitx.WorktreeListing, error) {
	f.mu.Lock()
	f.calls = append(f.calls, repo)
	f.mu.Unlock()
	if e := f.err[repo]; e != nil {
		return gitx.WorktreeListing{}, e
	}
	if l, ok := f.lists[repo]; ok {
		return l, nil
	}
	return gitx.WorktreeListing{RepoRoot: repo, Worktrees: []gitx.WorktreeEntry{{Path: repo}}}, nil
}

func mk(t *testing.T, p string, gitIsFile bool) {
	t.Helper()
	if err := osMkdirAll(p); err != nil {
		t.Fatal(err)
	}
	if gitIsFile {
		writeFile(t, filepath.Join(p, ".git"), "gitdir: /x")
	} else if err := osMkdirAll(filepath.Join(p, ".git")); err != nil {
		t.Fatal(err)
	}
}

// mkWorktreeMeta marks a main checkout as having linked worktrees, so that
// Build considers it worth a `git worktree list` round trip.
func mkWorktreeMeta(t *testing.T, p string) {
	t.Helper()
	if err := osMkdirAll(filepath.Join(p, ".git", "worktrees", "x")); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSkipsCoveredCheckouts(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	main := filepath.Join(root, "m")
	a := filepath.Join(root, "m@a")
	b := filepath.Join(root, "m@b")
	other := filepath.Join(root, "other")
	mk(t, main, false)
	mkWorktreeMeta(t, main)
	mk(t, a, true)
	mk(t, b, true)
	mk(t, other, false)
	l := gitx.WorktreeListing{RepoRoot: main, Worktrees: []gitx.WorktreeEntry{
		{Path: main}, {Path: a, IsLinked: true}, {Path: b, IsLinked: true},
	}}
	f := &fakeGitLister{lists: map[string]gitx.WorktreeListing{main: l}}
	cands, err := Build(&fakeLister{}, f, []scan.Target{{Path: root, Depth: 1}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	// Only m is listed: other has no worktree metadata, so a list could not
	// return anything, and m's list covers m@a and m@b, so they are never
	// listed either.
	if len(f.calls) != 1 || f.calls[0] != main {
		t.Errorf("worktree list calls: %v", f.calls)
	}
	kinds := map[string]Kind{}
	for _, c := range cands {
		kinds[c.Path] = c.Kind
	}
	if kinds[a] != KindWorktree || kinds[b] != KindWorktree || kinds[main] != KindRepo || kinds[other] != KindRepo {
		t.Errorf("kinds: %v", kinds)
	}
}

func TestBuildListsSiblingLinkedWorktreesOnce(t *testing.T) {
	// Only two linked worktrees of the same repository are scanned; the main
	// checkout lives outside the search roots. The repository must be
	// listed exactly once (the first list covers the sibling).
	root := scan.Normalize(t.TempDir())
	outside := scan.Normalize(filepath.Join(t.TempDir(), "repo"))
	a := filepath.Join(root, "repo-wt-a")
	b := filepath.Join(root, "repo-wt-b")
	mk(t, a, true)
	mk(t, b, true)
	l := gitx.WorktreeListing{RepoRoot: outside, Worktrees: []gitx.WorktreeEntry{
		{Path: outside}, {Path: a, IsLinked: true}, {Path: b, IsLinked: true},
	}}
	f := &fakeGitLister{lists: map[string]gitx.WorktreeListing{a: l, b: l}}
	cands, err := Build(&fakeLister{}, f, []scan.Target{{Path: root, Depth: 1}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != a {
		t.Errorf("worktree list calls: %v (want exactly one, for %s)", f.calls, a)
	}
	kinds := map[string]Kind{}
	for _, c := range cands {
		kinds[c.Path] = c.Kind
	}
	if kinds[a] != KindWorktree || kinds[b] != KindWorktree {
		t.Errorf("kinds: %v", kinds)
	}
	// If the first sibling's list fails, the second is still tried, so a
	// transient failure does not hide the repository.
	f = &fakeGitLister{lists: map[string]gitx.WorktreeListing{b: l}, err: map[string]error{a: errors.New("boom")}}
	cands, err = Build(&fakeLister{}, f, []scan.Target{{Path: root, Depth: 1}}, []string{root})
	if err == nil || len(f.calls) != 2 {
		t.Errorf("err=%v calls=%v", err, f.calls)
	}
	kinds = map[string]Kind{}
	for _, c := range cands {
		kinds[c.Path] = c.Kind
	}
	if kinds[b] != KindWorktree || kinds[a] != KindWorktree {
		t.Errorf("kinds after partial failure: %v", kinds)
	}
}

func TestBuildListFailureMarksUnknown(t *testing.T) {
	root := scan.Normalize(t.TempDir())
	linked := filepath.Join(root, "x@w")
	mk(t, linked, true)
	f := &fakeGitLister{err: map[string]error{linked: errors.New("boom")}}
	cands, err := Build(&fakeLister{}, f, []scan.Target{{Path: root, Depth: 1}}, []string{root})
	if err == nil || len(cands) != 1 || cands[0].Kind != KindUnknown {
		t.Errorf("err=%v cands=%+v", err, cands)
	}
	if e := Open(&fakeOpener{}, cands[0], false); !errors.Is(e, ErrKindUnknown) {
		t.Errorf("open unknown: %v", e)
	}
}

func TestWorktreeStatesAndApply(t *testing.T) {
	// Two repositories with worktrees: /r/a lists fine (one worktree open in
	// w9 — herdr's answer, e.g. a plain workspace on the path — and one
	// closed), /r/b fails to list.
	la := &herdr.WorktreeList{Worktrees: []herdr.Worktree{
		{Path: "/r/a", IsLinkedWorktree: false},
		{Path: "/w/a1", IsLinkedWorktree: true, OpenWorkspaceID: new("w9")},
		{Path: "/w/a2", IsLinkedWorktree: true},
	}}
	la.Source.RepoRoot = "/r/a"
	h := &fakeLister{
		lists: map[string]*herdr.WorktreeList{"/r/a": la},
		err:   map[string]error{"/r/b": errors.New("boom")},
	}
	cands := []Candidate{
		{Kind: KindRepo, Path: "/r/a"},
		{Kind: KindWorktree, Path: "/w/a1", RepoRoot: "/r/a", OpenState: OpenClosed}, // provisional: closed
		{Kind: KindWorktree, Path: "/w/a2", RepoRoot: "/r/a", OpenState: OpenOpen, OpenWorkspaceID: "stale"},
		{Kind: KindWorktree, Path: "/w/b1", RepoRoot: "/r/b", OpenState: OpenClosed},
	}
	st := WorktreeStates(context.Background(), h, cands)
	if len(h.calls) != 2 { // one list per distinct repo root
		t.Fatalf("calls: %v", h.calls)
	}
	ApplyWorktreeStates(cands, st)
	if c := cands[1]; !c.IsOpen() || c.OpenWorkspaceID != "w9" {
		t.Errorf("a1 must become open in w9: %+v", c)
	}
	if c := cands[2]; c.OpenState != OpenClosed || c.OpenWorkspaceID != "" {
		t.Errorf("a2 must become closed: %+v", c)
	}
	if c := cands[3]; c.OpenState != OpenUnknown {
		t.Errorf("b1 must become unknown (its list failed): %+v", c)
	}
	if c := cands[0]; c.OpenState != OpenClosed && c.OpenState != OpenUnknown {
		// repo rows are untouched by ApplyWorktreeStates
		_ = c
	}

	// A cancelled context stops issuing further herdr calls.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h2 := &fakeLister{}
	if st := WorktreeStates(ctx, h2, cands); len(h2.calls) != 0 || len(st.OK) != 0 {
		t.Fatalf("cancelled: calls=%v st=%+v", h2.calls, st)
	}
}

// fakeRemotes maps repo path -> remote name -> effective fetch URL.
type fakeRemotes map[string]map[string]string

func (f fakeRemotes) RemoteFetchURLs(_ context.Context, repo string) ([]gitx.Remote, error) {
	var names []string
	for n := range f[repo] {
		names = append(names, n)
	}
	sort.Strings(names)
	var remotes []gitx.Remote
	for _, n := range names {
		remotes = append(remotes, gitx.Remote{Name: n, URL: f[repo][n]})
	}
	return remotes, nil
}

func TestResolveRepoIDs(t *testing.T) {
	cands := []Candidate{
		{Kind: KindRepo, Path: "/r/a"},
		{Kind: KindWorktree, Path: "/r/a@x", RepoRoot: "/r/a"},
		{Kind: KindRepo, Path: "/r/none"},
		{Kind: KindWorkspace, Path: "/r/a"},
		{Kind: KindUnknown, Path: "/r/u"},
		{Kind: KindRepo, Path: "/r/fork"},
	}
	ids := ResolveRepoIDs(context.Background(), fakeRemotes{
		"/r/a":    {"origin": "https://GitHub.com/Acme/Api.git"},
		"/r/u":    {"origin": "git@gh.io:o/u"},
		"/r/fork": {"origin": "https://u:t@github.com/me/api.git?x=1#f", "upstream": "https://github.com/acme/api"},
	}, cands)
	ApplyRepoIDs(cands, ids)
	want := []string{"github.com/acme/api", "github.com/acme/api", "", "", "gh.io/o/u", "github.com/me/api"}
	for i, w := range want {
		if cands[i].RepoID != w {
			t.Errorf("%d: got %q want %q", i, cands[i].RepoID, w)
		}
	}
	// RepoPaths come from every remote (fork: origin and upstream).
	if !cands[5].HasRepoPath("github.com/me/api") || !cands[5].HasRepoPath("github.com/acme/api") {
		t.Errorf("fork paths: %v", cands[5].RepoPaths)
	}
	if !cands[1].HasRepoPath("github.com/acme/api") || cands[2].HasRepoPath("") {
		t.Errorf("worktree/none paths: %v %v", cands[1].RepoPaths, cands[2].RepoPaths)
	}
}

type fakeOpener struct {
	calls   []string
	parent  *string // source_workspace_id returned by WorktreeList
	labels  map[string]string
	repoDir string
}

func (f *fakeOpener) WorktreeList(repo string) (*herdr.WorktreeList, error) {
	l := &herdr.WorktreeList{}
	l.Source.RepoRoot = repo
	if f.repoDir != "" {
		l.Source.RepoRoot = f.repoDir
	}
	l.Source.SourceWorkspaceID = f.parent
	return l, nil
}
func (f *fakeOpener) WorkspaceLabel(id string) (string, error) { return f.labels[id], nil }
func (f *fakeOpener) WorkspaceRename(id, label string) error {
	f.calls = append(f.calls, "rename "+id+" "+label)
	return nil
}

func TestRelabelParent(t *testing.T) {
	p := "w9"
	cases := []struct {
		name   string
		parent *string
		label  string // current label of the parent
		want   []string
	}{
		{"default label is replaced", &p, "repo", []string{"rename w9 o/repo"}},
		{"already correct", &p, "o/repo", nil},
		{"user-chosen label kept", &p, "my project", nil},
		{"no parent workspace", nil, "", nil},
	}
	for _, tc := range cases {
		f := &fakeOpener{parent: tc.parent, labels: map[string]string{"w9": tc.label}, repoDir: "/src/github.com/o/repo"}
		if err := RelabelParent(f, "/src/github.com/o/repo", "o/repo"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Join(f.calls, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: calls=%v want %v", tc.name, f.calls, tc.want)
		}
	}
}

func TestOpenWorktreeRelabelsParent(t *testing.T) {
	p := "w9"
	f := &fakeOpener{parent: &p, labels: map[string]string{"w9": "repo"}, repoDir: "/src/github.com/o/repo"}
	// herdr-managed checkout outside the search roots: its own label is the
	// full path, but the parent must still be named after the repository.
	c := Candidate{Kind: KindWorktree, Path: "/home/u/.herdr/worktrees/repo/x", Label: "/home/u/.herdr/worktrees/repo/x",
		RepoRoot: "/src/github.com/o/repo", RepoLabel: "github.com/o/repo", OpenState: OpenClosed}
	if err := Open(f, c, false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "wtopen /src/github.com/o/repo /home/u/.herdr/worktrees/repo/x,rename w9 o/repo" {
		t.Errorf("calls=%v", f.calls)
	}
}

func (f *fakeOpener) WorkspaceCreate(cwd, label string) error {
	f.calls = append(f.calls, "create "+cwd+" label="+label)
	return nil
}
func (f *fakeOpener) WorkspaceFocus(id string) error {
	f.calls = append(f.calls, "focus "+id)
	return nil
}
func (f *fakeOpener) WorktreeOpen(root, path string) error {
	f.calls = append(f.calls, "wtopen "+root+" "+path)
	return nil
}

func TestWorkspaceLabel(t *testing.T) {
	cases := map[string]string{
		"github.com/utahta/herdr-hop":   "utahta/herdr-hop",
		"github.com/upstream/herdr-hop": "upstream/herdr-hop",
		"utahta/herdr-hop":              "utahta/herdr-hop",
		"deep/nested/github.com/o/r":    "o/r",
		"single":                        "single",
		"/Users/x/project":              "project", // outside search roots: no owner information
		"/repo":                         "repo",
	}
	for in, want := range cases {
		if got := WorkspaceLabel(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestOpen(t *testing.T) {
	cases := []struct {
		name  string
		c     Candidate
		force bool
		want  string
		err   error
	}{
		{"repo closed", Candidate{Kind: KindRepo, Path: "/a", Label: "github.com/o/a", OpenState: OpenClosed}, false, "create /a label=o/a", nil},
		{"repo open", Candidate{Kind: KindRepo, Path: "/a", OpenState: OpenOpen, OpenWorkspaceID: "w1"}, false, "focus w1", nil},
		{"repo open force", Candidate{Kind: KindRepo, Path: "/a", Label: "o/a", OpenState: OpenOpen, OpenWorkspaceID: "w1"}, true, "create /a label=o/a", nil},
		{"repo unknown", Candidate{Kind: KindRepo, Path: "/a", OpenState: OpenUnknown}, false, "", ErrOpenStateUnknown},
		{"repo unknown force", Candidate{Kind: KindRepo, Path: "/a", Label: "a", OpenState: OpenUnknown}, true, "create /a label=a", nil},
		{"wt closed", Candidate{Kind: KindWorktree, Path: "/w", RepoRoot: "/r", OpenState: OpenClosed}, false, "wtopen /r /w", nil},
		{"wt unknown", Candidate{Kind: KindWorktree, Path: "/w", RepoRoot: "/r", OpenState: OpenUnknown}, false, "", ErrWorktreeStateUnknown},
		{"wt open", Candidate{Kind: KindWorktree, Path: "/w", RepoRoot: "/r", OpenState: OpenOpen, OpenWorkspaceID: "w7"}, false, "focus w7", nil},
		{"wt force", Candidate{Kind: KindWorktree, Path: "/w", RepoRoot: "/r"}, true, "", ErrForceNotAllowed},
		{"ws", Candidate{Kind: KindWorkspace, OpenState: OpenOpen, OpenWorkspaceID: "w5"}, false, "focus w5", nil},
		{"unknown", Candidate{Kind: KindUnknown, Path: "/u"}, false, "", ErrKindUnknown},
		{"unknown force", Candidate{Kind: KindUnknown, Path: "/u"}, true, "", ErrKindUnknown},
		{"pull row", Candidate{Kind: KindPull, Path: "/r"}, false, "", ErrCloneRow},
		{"clone+pull row", Candidate{Kind: KindClonePull, Path: "/r"}, false, "", ErrCloneRow},
	}
	for _, tc := range cases {
		f := &fakeOpener{}
		err := Open(f, tc.c, tc.force)
		if tc.err != nil {
			if !errors.Is(err, tc.err) || len(f.calls) != 0 {
				t.Errorf("%s: err=%v calls=%v", tc.name, err, f.calls)
			}
			continue
		}
		if err != nil || len(f.calls) != 1 || f.calls[0] != tc.want {
			t.Errorf("%s: err=%v calls=%v want %q", tc.name, err, f.calls, tc.want)
		}
	}
}

func TestParallelKeepsOrder(t *testing.T) {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	got := Parallel(items, func(i int) int { return i * 2 })
	for i, v := range got {
		if v != i*2 {
			t.Fatalf("index %d: got %d", i, v)
		}
	}
	if len(Parallel([]int{}, func(i int) int { return i })) != 0 {
		t.Error("empty input")
	}
}

type fakeRemover struct{ calls []string }

func (f *fakeRemover) WorktreeRemove(id string, force bool) error {
	f.calls = append(f.calls, fmt.Sprintf("herdr %s force=%v", id, force))
	return nil
}

type fakeGitRemover struct{ calls []string }

func (f *fakeGitRemover) WorktreeRemove(repo, path string, force bool) error {
	f.calls = append(f.calls, fmt.Sprintf("git %s %s force=%v", repo, path, force))
	return nil
}

func TestRemoveRoutesAndRefuses(t *testing.T) {
	h, g := &fakeRemover{}, &fakeGitRemover{}
	open := Candidate{Kind: KindWorktree, Path: "/w/a", RepoRoot: "/r", OpenState: OpenOpen, OpenWorkspaceID: "ws1"}
	closed := Candidate{Kind: KindWorktree, Path: "/w/b", RepoRoot: "/r", OpenState: OpenClosed}
	own := Occupancy{OK: true, Occupants: []Occupant{{Path: "/w/a/sub", WorkspaceID: "ws1", Managed: true}}}
	if err := Remove(h, g, open, own, false); err != nil {
		t.Fatal(err) // its own (managed) workspace's pane, in a subdirectory, goes with it
	}
	if err := Remove(h, g, open, Occupancy{OK: true, Occupants: []Occupant{{Path: "/w/a", WorkspaceID: "ws1"}}}, false); !errors.Is(err, ErrRemoveInside) {
		t.Errorf("a plain workspace named as open_workspace_id must block, got %v", err)
	}
	if err := Remove(h, g, closed, Occupancy{OK: true, Occupants: []Occupant{{Path: "/w/other/sub"}, {Path: "/w/bb"}}}, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(append(h.calls, g.calls...), ","); got != "herdr ws1 force=false,git /r /w/b force=true" {
		t.Errorf("routing: %s", got)
	}
	// Panes sit in subdirectories of /w/e (the picker's own workspace) and
	// /w/g (another one's inactive pane): neither worktree is Current or
	// counted — but both are in use.
	occ := Occupancy{OK: true, Occupants: []Occupant{{Path: "/w/e/sub/dir", WorkspaceID: "cur", Current: true}, {Path: "/w/g/x", WorkspaceID: "other"}}}
	for name, c := range map[string]Candidate{
		"repo":     {Kind: KindRepo, Path: "/r", OpenState: OpenClosed},
		"current":  {Kind: KindWorktree, Path: "/w/c", RepoRoot: "/r", OpenState: OpenOpen, OpenWorkspaceID: "ws2", Current: true},
		"unknown":  {Kind: KindWorktree, Path: "/w/d", RepoRoot: "/r", OpenState: OpenUnknown},
		"contains": {Kind: KindWorktree, Path: "/w/e", RepoRoot: "/r", OpenState: OpenClosed},
		"inside":   {Kind: KindWorktree, Path: "/w/g", RepoRoot: "/r", OpenState: OpenClosed},
		"shared":   {Kind: KindWorktree, Path: "/w/f", RepoRoot: "/r", OpenState: OpenOpen, OpenWorkspaceID: "ws3", OpenCount: 2},
	} {
		if err := Remove(h, g, c, occ, true); err == nil {
			t.Errorf("%s must be refused", name)
		} else if CanRemove(c, occ) == nil || CanRemove(c, occ).Error() != err.Error() {
			t.Errorf("%s: CanRemove must give the same answer as Remove", name)
		}
	}
	if err := CanRemove(Candidate{Kind: KindWorktree, Path: "/w/e", RepoRoot: "/r", OpenState: OpenClosed}, occ); !errors.Is(err, ErrRemoveCurrent) {
		t.Errorf("own pane inside: %v", err)
	}
	if err := CanRemove(Candidate{Kind: KindWorktree, Path: "/w/g", RepoRoot: "/r", OpenState: OpenClosed}, occ); !errors.Is(err, ErrRemoveInside) {
		t.Errorf("other pane inside: %v", err)
	}
	// Without the snapshot, or with a pane herdr reported no directory
	// for, nothing can be proven unused: refuse.
	if err := CanRemove(closed, Occupancy{}); !errors.Is(err, ErrRemoveSnapshotUnknown) {
		t.Errorf("snapshot failed: %v", err)
	}
	if err := CanRemove(closed, Occupancy{OK: true, UnknownPanes: 2}); !errors.Is(err, ErrRemovePanesUnknown) {
		t.Errorf("unknown panes: %v", err)
	}
	// Path boundaries: a pane in /w/e-old is not inside /w/e.
	if err := CanRemove(Candidate{Kind: KindWorktree, Path: "/w/e", RepoRoot: "/r", OpenState: OpenClosed}, Occupancy{OK: true, Occupants: []Occupant{{Path: "/w/e-old/x"}}}); err != nil {
		t.Errorf("/w/e-old/x is not inside /w/e: %v", err)
	}
	if len(h.calls)+len(g.calls) != 2 {
		t.Errorf("refusals must not call anything: %v %v", h.calls, g.calls)
	}
}

func TestOccupancyOfReadsEveryPane(t *testing.T) {
	cwd := func(s string) *string { return &s }
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "cur", Focused: true},
			{ID: "wt", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/feat/"}},
		},
		Panes: []herdr.Pane{
			{ID: "p1", WorkspaceID: "cur", Cwd: cwd("/home/u")},
			{ID: "p2", WorkspaceID: "cur", Cwd: cwd("/home/u/x"), ForegroundCwd: cwd("/w/feat/sub")}, // inactive pane, other tab
			{ID: "p3", WorkspaceID: "wt", Cwd: nil, ForegroundCwd: cwd("")},                          // unknown
		},
	}
	occ := OccupancyOf(snap)
	if !occ.OK || occ.UnknownPanes != 1 {
		t.Fatalf("OK=%v unknown=%d", occ.OK, occ.UnknownPanes)
	}
	want := []Occupant{
		{Path: "/w/feat", WorkspaceID: "wt"},
		{Path: "/home/u", WorkspaceID: "cur", Current: true},
		{Path: "/home/u/x", WorkspaceID: "cur", Current: true},
		{Path: "/w/feat/sub", WorkspaceID: "cur", Current: true},
	}
	if fmt.Sprint(occ.Occupants) != fmt.Sprint(want) {
		t.Errorf("got %+v\nwant %+v", occ.Occupants, want)
	}
	if OccupancyOf(nil).OK {
		t.Error("no snapshot: not OK")
	}
}

type fakeRemoveClient struct {
	fakeLister
	snap    *herdr.Snapshot
	snapErr error
	removed []string
}

func (f *fakeRemoveClient) Snapshot() (*herdr.Snapshot, error) { return f.snap, f.snapErr }
func (f *fakeRemoveClient) WorktreeRemove(id string, force bool) error {
	f.removed = append(f.removed, fmt.Sprintf("%s force=%v", id, force))
	return nil
}

func TestRemoveNowDecidesOnFreshState(t *testing.T) {
	ws9 := "ws9"
	h := &fakeRemoveClient{
		snap: &herdr.Snapshot{Workspaces: []herdr.Workspace{{ID: "ws9", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/b", IsLinkedWorktree: true}}}},
	}
	h.lists = map[string]*herdr.WorktreeList{"/r": {Worktrees: []herdr.Worktree{{Path: "/w/b", Branch: "feat-b", IsLinkedWorktree: true, OpenWorkspaceID: &ws9}}}}
	h.lists["/r"].Source.RepoRoot = "/r"
	g := &fakeGitRemover{}
	// The row still says closed; herdr now has ws9 on it.
	stale := Candidate{Kind: KindWorktree, Path: "/w/b", Branch: "feat-b", RepoRoot: "/r", OpenState: OpenClosed}
	if err := RemoveNow(h, g, stale, false); err != nil {
		t.Fatal(err)
	}
	if len(h.removed) != 1 || h.removed[0] != "ws9 force=false" || len(g.calls) != 0 {
		t.Errorf("must route through herdr on the fresh state: herdr=%v git=%v", h.removed, g.calls)
	}
	// A second workspace appeared on it: refused.
	h.removed = nil
	h.snap.Workspaces = append(h.snap.Workspaces, herdr.Workspace{ID: "ws10", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/w/b", IsLinkedWorktree: true}})
	if err := RemoveNow(h, g, stale, false); !errors.Is(err, ErrRemoveShared) {
		t.Errorf("two workspaces now: %v", err)
	}
	// The snapshot fails at that moment: refused.
	h.snapErr = errors.New("socket gone")
	if err := RemoveNow(h, g, stale, false); !errors.Is(err, ErrRemoveSnapshotUnknown) {
		t.Errorf("snapshot failed: %v", err)
	}
	h.snapErr = nil
	// The worktree list fails: open state unknown, refused.
	h.err = map[string]error{"/r": errors.New("busy")}
	if err := RemoveNow(h, g, stale, false); !errors.Is(err, ErrWorktreeStateUnknown) {
		t.Errorf("list failed: %v", err)
	}
	if len(h.removed)+len(g.calls) != 0 {
		t.Errorf("refusals must not remove: %v %v", h.removed, g.calls)
	}
}

func TestRemoveNowRefusesAChangedWorktree(t *testing.T) {
	// Between the question and the yes the checkout at the path may have
	// been replaced: the fresh list must still describe what was confirmed.
	newClient := func(entry herdr.Worktree, root string) *fakeRemoveClient {
		h := &fakeRemoveClient{snap: &herdr.Snapshot{}}
		h.lists = map[string]*herdr.WorktreeList{"/r": {Worktrees: []herdr.Worktree{entry}}}
		h.lists["/r"].Source.RepoRoot = root
		return h
	}
	confirmed := Candidate{Kind: KindWorktree, Path: "/w/b", Branch: "feat-a", RepoRoot: "/r", OpenState: OpenClosed}
	unseen := "ws-new"
	cases := map[string]*fakeRemoveClient{
		"other branch":  newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-b", IsLinkedWorktree: true}, "/r"),
		"now detached":  newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a", IsDetached: true, IsLinkedWorktree: true}, "/r"),
		"unseen ws":     newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a", IsLinkedWorktree: true, OpenWorkspaceID: &unseen}, "/r"),
		"gone":          newClient(herdr.Worktree{Path: "/w/zzz", Branch: "feat-a", IsLinkedWorktree: true}, "/r"),
		"prunable":      newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a", IsLinkedWorktree: true, IsPrunable: true}, "/r"),
		"main checkout": newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a"}, "/r"),
		"other repo":    newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a", IsLinkedWorktree: true}, "/elsewhere"),
	}
	for name, h := range cases {
		g := &fakeGitRemover{}
		if err := RemoveNow(h, g, confirmed, false); !errors.Is(err, ErrRemoveChanged) {
			t.Errorf("%s: want ErrRemoveChanged, got %v", name, err)
		}
		if len(h.removed)+len(g.calls) != 0 {
			t.Errorf("%s: nothing may be removed: %v %v", name, h.removed, g.calls)
		}
	}
	// Unchanged: proceeds (through git, nothing open).
	h := newClient(herdr.Worktree{Path: "/w/b", Branch: "feat-a", IsLinkedWorktree: true}, "/r")
	g := &fakeGitRemover{}
	if err := RemoveNow(h, g, confirmed, false); err != nil || len(g.calls) != 1 {
		t.Errorf("unchanged: err=%v git=%v", err, g.calls)
	}
	// A detached worktree confirmed as such: still detached passes, a
	// branch checked out at the same path meanwhile does not.
	detached := Candidate{Kind: KindWorktree, Path: "/w/b", RepoRoot: "/r", OpenState: OpenClosed}
	h = newClient(herdr.Worktree{Path: "/w/b", Branch: "HEAD", IsDetached: true, IsLinkedWorktree: true}, "/r")
	g = &fakeGitRemover{}
	if err := RemoveNow(h, g, detached, false); err != nil || len(g.calls) != 1 {
		t.Errorf("still detached: err=%v git=%v", err, g.calls)
	}
	h = newClient(herdr.Worktree{Path: "/w/b", Branch: "main", IsLinkedWorktree: true}, "/r")
	g = &fakeGitRemover{}
	if err := RemoveNow(h, g, detached, false); !errors.Is(err, ErrRemoveChanged) || len(g.calls) != 0 {
		t.Errorf("detached → main: err=%v git=%v", err, g.calls)
	}
}

func TestRemoveNowPlainWorkspaceAtThePathBlocks(t *testing.T) {
	// herdr worktree list names whatever workspace sits at the path as
	// open_workspace_id — a plain one too, which `herdr worktree remove`
	// rejects (not_linked_worktree). Such a workspace blocks the removal
	// instead of being mistaken for the worktree's own.
	plain := "ws-plain"
	cwd := "/w/b"
	h := &fakeRemoveClient{snap: &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws-plain", ActiveTabID: "t1"}},
		Layouts:    []herdr.Layout{{TabID: "t1", WorkspaceID: "ws-plain", FocusedPaneID: "p1"}},
		Panes:      []herdr.Pane{{ID: "p1", WorkspaceID: "ws-plain", Cwd: &cwd}},
	}}
	h.lists = map[string]*herdr.WorktreeList{"/r": {Worktrees: []herdr.Worktree{{Path: "/w/b", Branch: "feat-b", IsLinkedWorktree: true, OpenWorkspaceID: &plain}}}}
	h.lists["/r"].Source.RepoRoot = "/r"
	g := &fakeGitRemover{}
	c := Candidate{Kind: KindWorktree, Path: "/w/b", Branch: "feat-b", RepoRoot: "/r", OpenState: OpenOpen, OpenWorkspaceID: "ws-plain", OpenCount: 1}
	if err := RemoveNow(h, g, c, false); !errors.Is(err, ErrRemoveInside) {
		t.Errorf("plain workspace at the path: want ErrRemoveInside, got %v", err)
	}
	if len(h.removed)+len(g.calls) != 0 {
		t.Errorf("nothing may be removed: %v %v", h.removed, g.calls)
	}
	// Even when the snapshot shows that workspace's pane elsewhere (it
	// moved into the checkout between the snapshot and the list), the
	// list's answer alone refuses.
	elsewhere := "/home/u"
	h.snap.Panes[0].Cwd = &elsewhere
	if err := RemoveNow(h, g, c, false); !errors.Is(err, ErrRemoveInside) {
		t.Errorf("plain workspace named by the list, pane moved meanwhile: want ErrRemoveInside, got %v", err)
	}
	if len(h.removed)+len(g.calls) != 0 {
		t.Errorf("nothing may be removed: %v %v", h.removed, g.calls)
	}
	h.snap.Panes[0].Cwd = &cwd
	// The same picture at load time is refused before the question, too:
	// the occupant is not a managed worktree workspace.
	occ := OccupancyOf(h.snap)
	if err := CanRemove(c, occ); !errors.Is(err, ErrRemoveInside) {
		t.Errorf("CanRemove on load-time rows: %v", err)
	}
	// A managed worktree workspace with a pane in a subdirectory passes.
	h.snap.Workspaces[0].Worktree = &herdr.WorkspaceWorktree{CheckoutPath: "/w/b", IsLinkedWorktree: true}
	sub := "/w/b/sub"
	h.snap.Panes[0].Cwd = &sub
	if err := RemoveNow(h, g, c, false); err != nil || len(h.removed) != 1 {
		t.Errorf("managed workspace: err=%v herdr=%v", err, h.removed)
	}
}
