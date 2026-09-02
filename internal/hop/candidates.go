// Package hop builds the unified candidate list shown by the hop picker and
// executes the action chosen for a row.
//
// Rows come from three sources and are merged by normalized path:
//   - repositories found by scanning the configured search paths,
//   - linked worktrees reported by `git worktree list`,
//   - workspaces currently open in herdr (`herdr api snapshot`).
//
// A worktree row wins over a repo row on the same path, because opening a
// worktree through `herdr workspace create` would bypass herdr's worktree
// bookkeeping. When herdr state cannot be fetched, rows are marked unknown
// rather than guessed so that Enter never creates a duplicate workspace.
package hop

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/scan"
)

// Kind is the row type.
type Kind int

const (
	KindRepo Kind = iota
	KindWorktree
	KindWorkspace
	// KindUnknown is a scanned checkout whose .git is a file (a probable linked
	// worktree) for which `git worktree list` failed. It must never be opened
	// via workspace create, so it is shown but not actionable.
	KindUnknown
	// KindClone is a synthetic row offered by the UI when the query parses as
	// a clone target that does not exist locally. It is never produced by Build.
	KindClone
	// KindPull is a synthetic row offered by the UI when the query is a pull
	// request URL of a repository that exists locally: Enter fetches the PR
	// head and opens it as a worktree. Path is the repository root.
	KindPull
	// KindClonePull is KindPull for a repository that is not checked out yet:
	// Enter clones it first. Path is the clone destination.
	KindClonePull
	// KindNote is a synthetic, non-actionable guidance row (e.g. "a checkout
	// exists at the clone destination but has no remote to verify against").
	KindNote
)

func (k Kind) String() string {
	switch k {
	case KindRepo:
		return "repo"
	case KindWorktree:
		return "worktree"
	case KindWorkspace:
		return "workspace"
	case KindClone:
		return "clone"
	case KindPull:
		return "pull"
	case KindClonePull:
		return "clone+pull"
	case KindNote:
		return "note"
	default:
		return "unknown"
	}
}

// OpenState says whether a workspace for this row is known to be open.
type OpenState int

const (
	// OpenUnknown: herdr state could not be fetched. "Not fetched" is not
	// "not open" — Enter must not create a workspace in this state.
	OpenUnknown OpenState = iota
	OpenClosed
	OpenOpen
)

// Candidate is one row in the hop list.
type Candidate struct {
	Kind Kind
	// Path is the normalized directory. Empty for path-unknown workspace rows.
	Path string
	// Label is the display name (path relative to a search root, or workspace label).
	Label string
	// Branch is set for worktree rows.
	Branch string
	// RepoRoot is set for worktree rows (source.repo_root).
	RepoRoot string
	// RepoLabel is the display label of RepoRoot (relative to a search
	// root), used to name the parent workspace. Set for worktree rows.
	RepoLabel string
	// OpenState is the open/closed/unknown status.
	OpenState OpenState
	// OpenWorkspaceID is the switch target when OpenState == OpenOpen: for
	// worktree rows it is worktree list's open_workspace_id; for repo rows the
	// lowest-numbered path-matching workspace; for workspace rows the workspace itself.
	OpenWorkspaceID string
	// OpenCount is the number of workspaces matching this row's path (badge only).
	OpenCount int
	// RepoID is "host/owner/repo" (lower-cased) derived from the origin remote,
	// or "" when unknown. It identifies the repository independently of where
	// it is checked out or how it is labelled.
	RepoID string
	// PRBranch is, on an expanded pull row, the remote branch chosen as the
	// pull request's source ("" until the user has to choose).
	PRBranch string
	// RepoPaths are the comparison identities (clone.RepoPathOf) of every
	// remote's effective fetch URL: full namespace path, non-default port
	// kept. Used to match a pull request URL to a checkout, including a
	// fork checkout whose "upstream" remote is the PR's repository.
	RepoPaths []string
	// Current is true when this row is where the picker was invoked from:
	// the herdr-focused workspace resolved to this row's path.
	Current bool
}

// HasRepoPath reports whether one of the candidate's remotes is repoPath.
func (c Candidate) HasRepoPath(repoPath string) bool {
	return repoPath != "" && slices.Contains(c.RepoPaths, repoPath)
}

// EffectiveRoot is the repository a worktree would be created from: the
// repository itself for repo rows, the main checkout for worktree rows, ""
// for rows that are not repositories.
func (c Candidate) EffectiveRoot() (root, label string, ok bool) {
	switch c.Kind {
	case KindRepo:
		return c.Path, c.Label, true
	case KindWorktree:
		if c.RepoRoot != "" {
			return c.RepoRoot, c.RepoLabel, true
		}
	}
	return "", "", false
}

// IsOpen reports whether a workspace is known to be open for this row.
func (c Candidate) IsOpen() bool { return c.OpenState == OpenOpen }

// Lister is the subset of herdr.Client the load path needs: the snapshot
// during Build, and `herdr worktree list` for the background worktree-state
// pass (WorktreeStates).
type Lister interface {
	Snapshot() (*herdr.Snapshot, error)
	WorktreeList(repo string) (*herdr.WorktreeList, error)
}

// WorktreeLister enumerates a repository's worktrees (gitx.Git). Build uses
// git, not herdr: the herdr server handles list calls strictly one at a
// time, which costs ~150ms at popup time, while git runs truly in parallel.
type WorktreeLister interface {
	WorktreeList(repo string) (gitx.WorktreeListing, error)
}

// RemoteResolver reads a checkout's remotes.
type RemoteResolver interface {
	// RemoteFetchURLs returns each remote's name and the URL `git fetch
	// <name>` uses, in one call: this runs once per scanned repository, and
	// one git process per remote would dominate the resolution time.
	// Cancelling ctx must terminate the underlying processes.
	RemoteFetchURLs(ctx context.Context, repo string) ([]gitx.Remote, error)
}

// RepoIdentity is a repository's remote-derived identity: the origin's
// "host/owner/repo" and the comparison path of every remote's fetch URL.
type RepoIdentity struct {
	ID    string
	Paths []string
}

// repoIDDir is the checkout whose remotes identify a row ("" when the row
// has no repository identity).
func repoIDDir(c Candidate) string {
	if c.Path == "" || (c.Kind != KindRepo && c.Kind != KindWorktree && c.Kind != KindUnknown) {
		return ""
	}
	if c.Kind == KindWorktree && c.RepoRoot != "" {
		return c.RepoRoot // all worktrees share the repository's remotes
	}
	return c.Path
}

// ResolveRepoIDs resolves the remote identity of every distinct repository
// among cands, concurrently, one RemoteFetchURLs call per repository.
// Failures are ignored (the repository stays absent from the result):
// identity is a best-effort hint used to match input to checkouts, never to
// act on its own. It does not touch cands — the picker runs this in the
// background and applies the result with ApplyRepoIDs when it arrives.
func ResolveRepoIDs(ctx context.Context, r RemoteResolver, cands []Candidate) map[string]RepoIdentity {
	var dirs []string
	seen := map[string]bool{}
	for _, c := range cands {
		if d := repoIDDir(c); d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	ids := map[string]RepoIdentity{}
	for _, res := range Parallel(dirs, func(d string) idResult {
		res := idResult{dir: d}
		remotes, err := r.RemoteFetchURLs(ctx, d)
		if err != nil {
			return res
		}
		for _, rm := range remotes {
			// RemoteIdentity, not clone.Parse: configured remotes may carry a
			// query or fragment that clone input would reject.
			rp, id, ok := clone.RemoteIdentity(rm.URL)
			if !ok {
				continue
			}
			if rm.Name == "origin" {
				res.id = id
			}
			if !slices.Contains(res.paths, rp) {
				res.paths = append(res.paths, rp)
			}
		}
		return res
	}) {
		ids[res.dir] = RepoIdentity{ID: res.id, Paths: res.paths}
	}
	return ids
}

// ApplyRepoIDs writes resolved identities onto the candidates they belong to.
func ApplyRepoIDs(cands []Candidate, ids map[string]RepoIdentity) {
	for i := range cands {
		if id, ok := ids[repoIDDir(cands[i])]; ok {
			cands[i].RepoID = id.ID
			cands[i].RepoPaths = id.Paths
		}
	}
}

type listResult struct {
	path string
	list gitx.WorktreeListing
	err  error
}

type idResult struct {
	dir, id string
	paths   []string
}

// parallelism bounds how many herdr/git processes run at once during a load.
const parallelism = 8

// Parallel runs fn over items with bounded concurrency and returns the
// results in the order of items, so callers stay deterministic.
func Parallel[T, R any](items []T, fn func(T) R) []R {
	out := make([]R, len(items))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, it T) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(it)
		}(i, it)
	}
	wg.Wait()
	return out
}

// Build scans searchPaths, enumerates worktrees with git, and merges herdr
// state (one snapshot round trip) into the candidate list. Failures never
// abort the list: the partial result is returned together with a non-nil
// error so the UI can show both. Rows whose state could not be determined
// are marked (OpenUnknown / KindUnknown) rather than guessed.
//
// The open state Build assigns to worktree rows is provisional (derived from
// the snapshot, which only knows herdr's own worktree workspaces); the
// authoritative state comes from WorktreeStates, which the picker runs in
// the background and applies with ApplyWorktreeStates.
func Build(h Lister, g WorktreeLister, targets []scan.Target, searchPaths []string) ([]Candidate, error) {
	l, err := Load(h, g, targets, searchPaths)
	return l.Cands, err
}

// Loaded is Build's result with what the candidates alone cannot tell:
// where herdr's panes are, and whether the snapshot was read at all
// (Occupancy.OK). Without the snapshot every row's Current and OpenCount
// are unknown (they read as false and 0), which matters to anything that
// must not act on a row someone is using — see CanRemove.
type Loaded struct {
	Cands []Candidate
	// Occupancy is where herdr's panes are (OccupancyOf the snapshot), for
	// the checks that must not act on a directory someone is using.
	Occupancy Occupancy
}

// Load is Build, reporting the occupancy alongside the candidates.
func Load(h Lister, g WorktreeLister, targets []scan.Target, searchPaths []string) (Loaded, error) {
	repos := scan.Repos(targets)
	var errs []string

	// `git worktree list` run in X returns every worktree of X's repository,
	// so a repository is listed once, and only repositories that can
	// actually have linked worktrees are listed (git keeps all
	// linked-worktree metadata in the main checkout's .git/worktrees, so a
	// main checkout scanned without it has none). The calls run
	// concurrently. Two phases keep the "list each repository once" rule:
	//   1. main checkouts (".git" directory) with worktree metadata —
	//      distinct repositories, so they are listed in parallel;
	//   2. checkouts with a ".git" file that phase 1 did not cover (linked
	//      worktrees whose main checkout is outside the scanned
	//      directories). Several of these may belong to one repository, so
	//      they are listed one at a time, each successful list covering its
	//      siblings; otherwise the same repository would be queried once per
	//      worktree, and a failure of a redundant query would be reported
	//      even though the repository was listed fine.
	covered := map[string]bool{}
	failed := map[string]bool{}
	var worktrees []gitx.WorktreeListing
	accept := func(res listResult) {
		if res.err != nil {
			errs = append(errs, res.err.Error())
			failed[res.path] = true
			return
		}
		root := scan.Normalize(res.list.RepoRoot)
		if covered[root] {
			return // same repository listed twice: keep the first
		}
		covered[root] = true
		for _, wt := range res.list.Worktrees {
			covered[scan.Normalize(wt.Path)] = true
		}
		worktrees = append(worktrees, res.list)
	}
	list := func(p string) listResult {
		l, err := g.WorktreeList(p)
		return listResult{path: p, list: l, err: err}
	}
	var mains, linked []string
	for _, r := range repos {
		switch {
		case r.GitIsFile:
			linked = append(linked, r.Path)
		case r.HasWorktrees:
			mains = append(mains, r.Path)
		}
	}
	for _, res := range Parallel(mains, list) {
		accept(res)
	}
	for _, p := range linked {
		if covered[p] {
			continue
		}
		accept(list(p))
	}

	snap, err := h.Snapshot()
	if err != nil {
		errs = append(errs, err.Error())
		snap = nil
	}
	out := Loaded{Cands: Merge(repos, worktrees, failed, snap, searchPaths), Occupancy: OccupancyOf(snap)}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// Merge is the pure combination step (no I/O), so it can be tested directly.
//   - failed: scanned paths for which WorktreeList failed (not covered by a successful list).
//   - snap == nil means the snapshot could not be fetched: no workspace rows,
//     and repo and worktree rows get OpenUnknown.
//
// The worktree open state Merge derives from the snapshot is provisional:
// the snapshot only knows herdr's own worktree workspaces, while herdr's
// authoritative open_workspace_id also counts a plain workspace sitting on
// the worktree's path. ApplyWorktreeStates overwrites it once the background
// `herdr worktree list` pass delivers.
func Merge(repos []scan.Repo, worktrees []gitx.WorktreeListing, failed map[string]bool, snap *herdr.Snapshot, searchPaths []string) []Candidate {
	byPath := map[string]*Candidate{}
	var order []string
	add := func(c Candidate) {
		if existing, ok := byPath[c.Path]; ok {
			// worktree > repo/unknown
			if c.Kind == KindWorktree && existing.Kind != KindWorktree {
				*existing = c
			}
			return
		}
		cc := c
		byPath[c.Path] = &cc
		order = append(order, c.Path)
	}

	// Paths covered by a successful worktree list. A .git-file checkout not
	// covered by any list (its own list failed) has an unknown kind.
	listed := map[string]bool{}
	for _, l := range worktrees {
		listed[scan.Normalize(l.RepoRoot)] = true
		for _, wt := range l.Worktrees {
			listed[scan.Normalize(wt.Path)] = true
		}
	}
	// Without a snapshot there is no provisional open state at all: neither
	// a repo nor a worktree row may claim to be closed (Enter would create a
	// duplicate workspace).
	unknownState := OpenClosed
	if snap == nil {
		unknownState = OpenUnknown
	}
	for _, r := range repos {
		p := scan.Normalize(r.Path)
		kind := KindRepo
		if r.GitIsFile && !listed[p] {
			kind = KindUnknown
		}
		add(Candidate{Kind: kind, Path: p, Label: label(p, searchPaths), OpenState: unknownState})
	}
	for _, l := range worktrees {
		root := scan.Normalize(l.RepoRoot)
		for _, wt := range l.Worktrees {
			if !wt.IsLinked || wt.IsBare || wt.IsPrunable {
				continue
			}
			p := scan.Normalize(wt.Path)
			add(Candidate{Kind: KindWorktree, Path: p, Label: label(p, searchPaths), Branch: wt.Branch, RepoRoot: root, RepoLabel: label(root, searchPaths), OpenState: unknownState})
		}
	}

	var standalone []Candidate
	if snap != nil {
		type wsMatch struct {
			id     string
			number int
			// worktree: the workspace is herdr's worktree workspace for this
			// path (Worktree.CheckoutPath). Only these are trusted for the
			// provisional worktree open state.
			worktree bool
		}
		matches := map[string][]wsMatch{}
		for _, ws := range snap.Workspaces {
			p := workspacePath(ws, snap)
			// Merge only into actionable rows. A KindUnknown row cannot be
			// opened, so a workspace merged into it would become unreachable;
			// keep it as a standalone (focusable) row instead.
			if p != "" {
				if row, ok := byPath[p]; ok && row.Kind != KindUnknown {
					matches[p] = append(matches[p], wsMatch{ws.ID, ws.Number, ws.Worktree != nil})
					if ws.Focused {
						row.Current = true
					}
					continue
				}
			}
			standalone = append(standalone, Candidate{
				Kind: KindWorkspace, Path: p, Label: workspaceLabel(ws, p, searchPaths),
				OpenState: OpenOpen, OpenWorkspaceID: ws.ID, OpenCount: 1, Current: ws.Focused,
			})
		}
		for p, ms := range matches {
			c := byPath[p]
			c.OpenCount = len(ms)
			sort.Slice(ms, func(i, j int) bool { return ms[i].number < ms[j].number })
			switch c.Kind {
			case KindRepo:
				c.OpenState = OpenOpen
				c.OpenWorkspaceID = ms[0].id
			case KindWorktree:
				// Provisional: trust only herdr worktree workspaces here;
				// ApplyWorktreeStates delivers the authoritative answer.
				for _, m := range ms {
					if m.worktree {
						c.OpenState = OpenOpen
						c.OpenWorkspaceID = m.id
						break
					}
				}
			}
			// unknown rows are never actionable.
		}
	}

	out := make([]Candidate, 0, len(order)+len(standalone))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	sort.SliceStable(standalone, func(i, j int) bool { return standalone[i].Label < standalone[j].Label })
	out = append(out, standalone...)
	return out
}

// WorktreeStateResult is the authoritative worktree open state of one load,
// gathered from `herdr worktree list`.
type WorktreeStateResult struct {
	// Open maps a worktree path to the workspace it is open in ("" when the
	// worktree is closed). Only paths whose repository was listed
	// successfully appear.
	Open map[string]string
	// OK holds the repository roots whose list succeeded; worktrees of a
	// root missing here have an unknown state.
	OK map[string]bool
}

// WorktreeStates asks herdr where each worktree of cands is open. herdr's
// open_workspace_id is the only authority: it also counts a plain workspace
// sitting on the worktree's path, which the snapshot cannot tell apart from
// an unrelated workspace. The herdr server handles the calls one at a time
// (~20ms each), so the picker runs this in the background and applies the
// result with ApplyWorktreeStates. The herdr CLI takes no context, so
// cancellation is checked between calls.
func WorktreeStates(ctx context.Context, h Lister, cands []Candidate) WorktreeStateResult {
	var roots []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c.Kind == KindWorktree && c.RepoRoot != "" && !seen[c.RepoRoot] {
			seen[c.RepoRoot] = true
			roots = append(roots, c.RepoRoot)
		}
	}
	res := WorktreeStateResult{Open: map[string]string{}, OK: map[string]bool{}}
	for _, root := range roots {
		if ctx.Err() != nil {
			return res
		}
		l, err := h.WorktreeList(root)
		if err != nil {
			continue
		}
		res.OK[scan.Normalize(l.Source.RepoRoot)] = true
		for _, wt := range l.Worktrees {
			if !wt.IsLinkedWorktree {
				continue
			}
			id := ""
			if wt.OpenWorkspaceID != nil {
				id = *wt.OpenWorkspaceID
			}
			res.Open[scan.Normalize(wt.Path)] = id
		}
	}
	return res
}

// ApplyWorktreeStates overwrites the provisional worktree open states with
// herdr's answer: open in the reported workspace, closed when the listing
// covered the repository but reports no workspace, and unknown when the
// listing failed (Open then refuses rather than risking a duplicate).
func ApplyWorktreeStates(cands []Candidate, st WorktreeStateResult) {
	for i := range cands {
		c := &cands[i]
		if c.Kind != KindWorktree {
			continue
		}
		if !st.OK[c.RepoRoot] {
			c.OpenState = OpenUnknown
			c.OpenWorkspaceID = ""
			continue
		}
		if id := st.Open[c.Path]; id != "" {
			c.OpenState = OpenOpen
			c.OpenWorkspaceID = id
		} else {
			c.OpenState = OpenClosed
			c.OpenWorkspaceID = ""
		}
	}
}

// workspacePath applies the path-resolution rules; "" means unknown.
func workspacePath(ws herdr.Workspace, snap *herdr.Snapshot) string {
	if ws.Worktree != nil && ws.Worktree.CheckoutPath != "" {
		return scan.Normalize(ws.Worktree.CheckoutPath)
	}
	var focused string
	for _, l := range snap.Layouts {
		if l.TabID == ws.ActiveTabID {
			focused = l.FocusedPaneID
			break
		}
	}
	if focused == "" {
		return ""
	}
	for _, p := range snap.Panes {
		if p.ID != focused {
			continue
		}
		if p.Cwd != nil && *p.Cwd != "" {
			return scan.Normalize(*p.Cwd)
		}
		if p.ForegroundCwd != nil && *p.ForegroundCwd != "" {
			return scan.Normalize(*p.ForegroundCwd)
		}
		return ""
	}
	return ""
}

func workspaceLabel(ws herdr.Workspace, path string, searchPaths []string) string {
	if path != "" {
		return label(path, searchPaths)
	}
	if ws.Label != "" {
		return ws.Label
	}
	return ws.ID
}

// label strips the longest matching search path prefix and, in the
// ROOT/host/owner/repo layout, the host component: nearly every row carries
// the same host, the picker's path column still shows it, and workspaces are
// already labelled "owner/repo". A leading component counts as a host when
// it contains a dot, which no plain directory layer normally does.
func label(path string, searchPaths []string) string {
	best := ""
	for _, sp := range searchPaths {
		sp = scan.Normalize(sp)
		if rel, err := filepath.Rel(sp, path); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			if len(sp) > len(best) {
				best = sp
			}
		}
	}
	if best == "" {
		return path
	}
	rel, _ := filepath.Rel(best, path)
	if host, rest, ok := strings.Cut(rel, "/"); ok && strings.Contains(host, ".") && rest != "" {
		return rest
	}
	return rel
}
