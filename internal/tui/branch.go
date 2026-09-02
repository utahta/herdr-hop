package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/forge"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/worktree"
)

// GitOps is what the picker needs from git (gitx.Git satisfies it).
type GitOps interface {
	Clone(ctx context.Context, url, dest string, progress func(string)) error
	RemoteFetchURLs(ctx context.Context, repo string) ([]gitx.Remote, error)
	WorktreeList(repo string) (gitx.WorktreeListing, error)
	WorktreeRemove(repo, path string, force bool) error
	Refs(repo string) ([]string, error)
	Remotes(repo string) ([]string, error)
	FetchURL(repo, name string) (string, error)
	FetchRefspecs(repo, name string) ([]string, error)
	BranchExists(repo, name string) (bool, error)
	ConfigGet(repo, key string) (string, error)
	ConfigSet(repo, key, value string) error
	Fetch(repo string) error
	FetchRef(ctx context.Context, repo, remoteOrURL, refspec string) error
	LsRemotePRs(ctx context.Context, repo, remote string, mask []string) ([]gitx.PRRef, error)
	LsRemotePR(ctx context.Context, repo, src, headRef string) (gitx.RemoteState, error)
	SetUpstream(repo, branch, upstream string) error
}

type refsLoadedMsg struct {
	op       int
	branches []worktree.Branch
	err      error
}
type fetchDoneMsg struct {
	op  int
	err error
}
type worktreeDoneMsg struct {
	op  int
	err error
}

// remotePRResult is one remote's outcome of a PR-heads fetch. repo is the
// repository the remote points at as the forge addresses it, the key under
// which PR details are later asked for.
type remotePRResult struct {
	remote string
	repo   clone.ForgeRepo
	heads  []worktree.PRHead
	err    error
}

// PRInfoSource answers what a forge knows about pull requests by number.
// Missing numbers are simply absent from the result. An implementation may
// return forge.ErrUnsupportedHost for hosts it cannot talk to.
type PRInfoSource interface {
	PullRequests(ctx context.Context, repo clone.ForgeRepo, numbers []int) (map[int]worktree.PRInfo, error)
}

// prInfoResult is one repository's outcome of a PR-details fetch.
type prInfoResult struct {
	repo  clone.ForgeRepo
	infos map[int]worktree.PRInfo
	err   error
}

// prInfoMsg carries PR titles and states fetched from the forge for the
// heads of PR generation gen. request numbers the fetches within the screen:
// only the latest one's answer is applied, so a superseded fetch (cancelled,
// but possibly answering late with what it had) cannot overwrite a newer one.
type prInfoMsg struct {
	op      int
	gen     int
	request int
	results []prInfoResult
}

// prHeadsMsg carries pull-request heads fetched from the remotes, one entry
// per remote so that a remote that answered (even with zero heads) replaces
// its previous annotations while a failing remote keeps them. gen is the
// fetch generation: results of a superseded fetch are dropped.
type prHeadsMsg struct {
	op  int
	gen int
	// remotes is the full set considered this round; annotations of remotes
	// no longer in it (deleted, or their URL became unrecognisable) are
	// dropped, since no result row will ever replace them.
	remotes []string
	results []remotePRResult
	// listErr is set when the remote list itself could not be read; nothing
	// is updated then.
	listErr error
}

// branchState is the "choose a branch for a new worktree" screen. It is
// entered from the picker for one repository and returns to it on Esc.
type branchState struct {
	op     int // operation id for async messages of this screen
	repo   string
	label  string
	input  textinput.Model
	filter string // list filter saved while in the remote-name state
	// raw is the branch list as loaded from git; branches is raw annotated
	// with PR information and reordered. Both are kept so that whichever of
	// the two loads (refs, PR heads) finishes last, the display is rebuilt
	// from the latest of both.
	raw []worktree.Branch
	// prHeads holds the latest successful result per remote.
	prHeads  map[string][]worktree.PRHead
	prGen    int // generation of the latest PR-heads fetch
	prCancel context.CancelFunc
	// prRepo maps a remote to the repository it points at; prInfo holds the
	// forge's answer per repository and PR number. Details fetched for an
	// earlier generation stay on display until the new ones arrive.
	prRepo       map[string]clone.ForgeRepo
	prInfo       map[clone.ForgeRepo]map[int]worktree.PRInfo
	prInfoCancel context.CancelFunc
	// prInfoReq counts the details fetches started on this screen; the
	// answer of any but the latest is dropped (cancelling does not
	// guarantee silence: a fetch may finish just before, or return partial
	// answers together with the cancellation error).
	prInfoReq int
	// prInfoAsked identifies the last details request (generation plus the
	// numbers asked for), so that the two loads whose arrival triggers a
	// request — refs and PR heads, in either order — do not ask twice for
	// the same thing.
	prInfoAsked string
	branches    []worktree.Branch
	names       []string
	view        []int
	cursor      int
	// top is the first visible row, anchored like the picker's (the cursor
	// walks inside the window, the window slides at its edges).
	top int
	// matches holds the matched rune positions per branch index for
	// highlighting (indexes into SearchText: name, then the PR labels).
	matches map[int][]int
	// titleMatches holds, per branch index, which of the branch's PRs had
	// its title hit by the query's words, and where (title-only matches,
	// see refilter).
	titleMatches map[int]titleMatch
	// locals is every existing local branch name, in-use ones included:
	// the display list hides those, but a new name must not collide.
	locals map[string]bool
	// inUse maps an in-use local branch name to its worktree path, for the
	// collision hint (the row itself is not shown — this screen only
	// creates worktrees; opening existing ones is the picker's job).
	inUse   map[string]string
	loading bool
	// refsReady is true only after a successful refs load. Until then the
	// local-branch set is unknown, so nothing may be created: a name that
	// collides with an existing local branch would make herdr check that
	// branch out instead of creating a new one.
	refsReady bool
	fetching  bool
	creating  bool
	// remote is set after a remote branch was chosen: the screen is then in
	// the "local branch name" state, where the input holds the name of the
	// local branch to create from that remote (prefilled with its short
	// name). This keeps the fuzzy filter and the new name apart.
	remote *worktree.Branch
	status string
}

func newBranchState(op int, repo, label string) *branchState {
	ti := textinput.New()
	ti.Prompt = "new worktree> "
	ti.PromptStyle = stylePromptWorktree
	ti.Focus()
	return &branchState{op: op, repo: repo, label: label, input: ti, loading: true}
}

func (b *branchState) busy() bool { return b.loading || b.fetching || b.creating }

// rowCount counts the leading "new branch" row plus the branch rows.
// There are no rows until refs have loaded, and none in the name state.
func (b *branchState) rowCount() int {
	if b.remote != nil || !b.refsReady {
		return 0
	}
	return len(b.view) + 1
}

// selected returns the branch under the cursor, or nil for the "new branch"
// row (the first row: creating is this screen's primary action).
func (b *branchState) selected() (*worktree.Branch, bool) {
	if b.remote != nil || !b.refsReady || b.cursor < 0 || b.cursor >= b.rowCount() {
		return nil, false
	}
	if b.cursor == 0 {
		return nil, true // new-branch row
	}
	br := b.branches[b.view[b.cursor-1]]
	return &br, true
}

func (b *branchState) refilter() {
	query := b.input.Value()
	b.matches = map[int][]int{}
	b.titleMatches = map[int]titleMatch{}
	if strings.TrimSpace(query) == "" {
		b.view = make([]int, len(b.names))
		for i := range b.names {
			b.view[i] = i
		}
	} else {
		q := []rune(strings.ToLower(query))
		// Two tiers: a fuzzy hit on the name or PR label ranks first; a
		// branch whose PR title contains every word of the query comes
		// after those. Fuzzy matching a whole title would hit nearly any
		// branch with a PR, so titles are matched by words instead.
		type scored struct{ i, tier, score int }
		var res []scored
		for i, n := range b.names {
			if sc, pos, ok := fuzzyMatch(q, []rune(strings.ToLower(n))); ok {
				res = append(res, scored{i, 0, sc})
				b.matches[i] = pos
			} else if tm, ok := titleWordsMatch(query, b.branches[i].PRs); ok {
				res = append(res, scored{i, 1, 0})
				b.titleMatches[i] = tm
			}
		}
		sort.SliceStable(res, func(a, c int) bool {
			if res[a].tier != res[c].tier {
				return res[a].tier < res[c].tier
			}
			return res[a].score > res[c].score
		})
		b.view = make([]int, len(res))
		for i, r := range res {
			b.view[i] = r.i
		}
	}
	// The cursor follows the intent: an empty query means "make one" (the
	// new row, so prefix+t then Enter cuts an auto-named worktree in two
	// keys); typing means "find one" (the best match, so Enter cannot
	// create a half-typed name by accident — forcing a create is one ↑
	// away). No match at all puts the new row back under the cursor.
	switch {
	case strings.TrimSpace(query) == "" || len(b.view) == 0:
		b.cursor = 0
	default:
		b.cursor = 1
	}
}

func (b *branchState) setBranches(bs []worktree.Branch) {
	b.raw = bs
	b.rebuild()
}

// applyPRResults merges one fetch round into the per-remote state: a remote
// that answered replaces its previous heads (zero heads removes stale
// annotations), a failing remote keeps what it had.
func (b *branchState) applyPRResults(remotes []string, results []remotePRResult) {
	if b.prHeads == nil {
		b.prHeads = map[string][]worktree.PRHead{}
	}
	current := map[string]bool{}
	for _, r := range remotes {
		current[r] = true
	}
	for r := range b.prHeads {
		if !current[r] {
			delete(b.prHeads, r) // remote gone: its annotations go with it
		}
	}
	if b.prRepo == nil {
		b.prRepo = map[string]clone.ForgeRepo{}
	}
	for _, r := range results {
		if r.err != nil {
			continue
		}
		b.prHeads[r.remote] = r.heads
		if r.repo != (clone.ForgeRepo{}) {
			b.prRepo[r.remote] = r.repo
		}
	}
	b.rebuild()
}

// applyPRInfo merges one round of forge answers: a repository that answered
// in full replaces its previous details; one that failed keeps what it had,
// updated with whatever partial answers arrived before the failure (the
// forge returns those alongside the error).
func (b *branchState) applyPRInfo(results []prInfoResult) {
	if b.prInfo == nil {
		b.prInfo = map[clone.ForgeRepo]map[int]worktree.PRInfo{}
	}
	for _, r := range results {
		if r.err == nil {
			b.prInfo[r.repo] = r.infos
			continue
		}
		if len(r.infos) == 0 {
			continue
		}
		merged := make(map[int]worktree.PRInfo, len(b.prInfo[r.repo])+len(r.infos))
		maps.Copy(merged, b.prInfo[r.repo])
		maps.Copy(merged, r.infos)
		b.prInfo[r.repo] = merged
	}
	b.rebuild()
}

// lookupPRInfo answers AttachPRInfo from the per-repository details.
func (b *branchState) lookupPRInfo(h worktree.PRHead) (worktree.PRInfo, bool) {
	info, ok := b.prInfo[b.prRepo[h.Remote]][h.Number]
	return info, ok
}

// prInfoWanted lists, per repository, the PR numbers of the listed branches
// whose details are worth fetching — every one, so a refresh (ctrl-f) also
// picks up state changes such as a merge.
func (b *branchState) prInfoWanted() map[clone.ForgeRepo][]int {
	want := map[clone.ForgeRepo]map[int]bool{}
	for _, br := range b.branches {
		for _, pr := range br.PRs {
			h := pr.Head
			repo, ok := b.prRepo[h.Remote]
			if !ok {
				continue
			}
			if want[repo] == nil {
				want[repo] = map[int]bool{}
			}
			want[repo][h.Number] = true
		}
	}
	out := map[clone.ForgeRepo][]int{}
	for repo, nums := range want {
		for n := range nums {
			out[repo] = append(out[repo], n)
		}
		sort.Ints(out[repo])
	}
	return out
}

func (b *branchState) allPRHeads() []worktree.PRHead {
	var remotes []string
	for r := range b.prHeads {
		remotes = append(remotes, r)
	}
	sort.Strings(remotes) // deterministic annotation order
	var all []worktree.PRHead
	for _, r := range remotes {
		all = append(all, b.prHeads[r]...)
	}
	return all
}

// rebuild recomputes the displayed branches from raw + prHeads, keeping the
// cursor on the branch it was on.
func (b *branchState) rebuild() {
	// Keep the new-branch row only when the user chose it over existing
	// matches; a cursor parked there because nothing matched must follow
	// the matches that this rebuild may add (e.g. "#12" once PR heads land).
	selectedRef, onNew := "", false
	if br, ok := b.selected(); ok {
		if br != nil {
			selectedRef = br.Ref
		} else {
			onNew = len(b.view) > 0
		}
	}
	all := worktree.AnnotatePRs(b.raw, b.allPRHeads())
	all = worktree.AttachPRInfo(all, b.lookupPRInfo)
	b.locals = worktree.Locals(all)
	b.inUse = map[string]string{}
	b.branches = b.branches[:0]
	for _, br := range all {
		if br.InUse() {
			// Already a worktree: not creatable, so not listed. The picker
			// is where existing worktrees are opened.
			b.inUse[br.Name] = br.WorktreePath
			continue
		}
		b.branches = append(b.branches, br)
	}
	b.names = make([]string, len(b.branches))
	for i, br := range b.branches {
		b.names[i] = br.SearchText()
	}
	b.refilter()
	switch {
	case onNew:
		b.cursor = 0
	case selectedRef != "":
		for i, idx := range b.view {
			if b.branches[idx].Ref == selectedRef {
				b.cursor = i + 1 // row 0 is the new-branch row
				break
			}
		}
	}
}

// newName is what the user typed, to be used as a new branch name, exactly
// as typed (no trimming: see worktree.Make). In the remote-name state it is
// the whole input; otherwise it is the input only when it does not exactly
// name an existing branch (then it is just a filter).
func (b *branchState) newName() string {
	v := b.input.Value()
	if b.remote != nil {
		return v
	}
	// Compare against the real branch names — b.names holds the search text
	// ("feature #12" once PR annotations arrive), and matching against that
	// would turn typing an existing name into an attempt to create it.
	for _, br := range b.branches {
		if br.Name == v {
			return ""
		}
	}
	return v
}

func (m HopModel) loadRefs() tea.Cmd {
	git, repo, op, lg := m.git, m.wt.repo, m.wt.op, m.log
	return func() tea.Msg {
		start := time.Now()
		remotes, err := git.Remotes(repo)
		if err != nil {
			return refsLoadedMsg{op: op, err: err}
		}
		lines, err := git.Refs(repo)
		if err != nil {
			return refsLoadedMsg{op: op, err: err}
		}
		lg.Printf("worktree op=%d: refs: %d refs in %v", op, len(lines), time.Since(start).Round(time.Millisecond))
		return refsLoadedMsg{op: op, branches: worktree.ParseRefs(lines, remotes)}
	}
}

func (m HopModel) fetch() tea.Cmd {
	git, repo, op := m.git, m.wt.repo, m.wt.op
	return func() tea.Msg { return fetchDoneMsg{op, git.Fetch(repo)} }
}

// createWorktree runs the plan: herdr creates and opens the worktree, then
// the upstream is set when the plan asks for it.
func (m HopModel) createWorktree(p worktree.Plan) tea.Cmd {
	h, git, repo, op := m.h, m.git, m.wt.repo, m.wt.op
	label := hop.WorkspaceLabel(m.wt.label)
	usedAt := m.wt.inUse[p.Branch]
	return func() tea.Msg {
		if p.Creates {
			// Authoritative collision check right before creating: the
			// branch list is a case-sensitive snapshot, but git on a
			// case-insensitive file system resolves "Feature" to an existing
			// "feature", which herdr would then check out instead.
			exists, err := git.BranchExists(repo, p.Branch)
			if err != nil {
				return worktreeDoneMsg{op, err}
			}
			if exists {
				if usedAt != "" {
					return worktreeDoneMsg{op, fmt.Errorf("%w: %q is already checked out in a worktree (%s): open it from the picker", worktree.ErrNameTaken, p.Branch, usedAt)}
				}
				return worktreeDoneMsg{op, fmt.Errorf("%w: %q resolves to an existing local branch", worktree.ErrNameTaken, p.Branch)}
			}
		}
		if err := h.WorktreeCreate(repo, p.Branch, p.Base); err != nil {
			return worktreeDoneMsg{op, err}
		}
		if p.Upstream != "" {
			if err := git.SetUpstream(repo, p.Branch, p.Upstream); err != nil {
				return worktreeDoneMsg{op, fmt.Errorf("worktree created, but setting upstream failed: %w", err)}
			}
		}
		if err := hop.RelabelParent(h, repo, label); err != nil {
			return worktreeDoneMsg{op, fmt.Errorf("worktree created, but renaming the parent workspace failed: %w", err)}
		}
		return worktreeDoneMsg{op, nil}
	}
}

// enterBranchScreen switches the picker to the branch screen for repo.
func (m HopModel) enterBranchScreen(repo, label string) (HopModel, tea.Cmd) {
	m.wtOp++
	m.wt = newBranchState(m.wtOp, repo, label)
	m.log.Printf("worktree: choose branch for %s", clone.Sanitize(repo))
	return m, tea.Batch(textinput.Blink, m.loadRefs(), m.loadPRHeads())
}

// leaveBranchScreen returns to the picker. The returned command, if any,
// fetches the worktree open states the picker needs and the branch screen
// did not (a load that jumped straight here skipped that pass).
func (m HopModel) leaveBranchScreen() (HopModel, tea.Cmd) {
	if m.wt != nil && m.wt.prCancel != nil {
		m.wt.prCancel() // no ls-remote outlives the screen
	}
	if m.wt != nil && m.wt.prInfoCancel != nil {
		m.wt.prInfoCancel()
	}
	m.wt = nil
	return m, m.startWorktreeStates()
}

// prHeadsTimeout bounds each ls-remote; annotations are best effort.
const prHeadsTimeout = 10 * time.Second

// loadPRHeads starts a new generation of PR-head fetching: every remote
// whose effective fetch URL looks like a repository is asked for its pull
// and merge request heads, concurrently. A previous generation still
// running is cancelled. Failures are logged, never shown.
func (m HopModel) loadPRHeads() tea.Cmd {
	b := m.wt
	if b.prCancel != nil {
		b.prCancel()
	}
	if b.prInfoCancel != nil {
		b.prInfoCancel() // details of the old generation are being replaced
	}
	b.prGen++
	ctx, cancel := context.WithCancel(context.Background())
	b.prCancel = cancel
	git, repo, op, gen, lg := m.git, b.repo, b.op, b.prGen, m.log
	return func() tea.Msg {
		defer cancel()
		start := time.Now()
		// One `git remote -v` yields every remote's name and effective
		// fetch URL, and doubles as the list of URLs to mask from errors;
		// asking per remote (name list, then a config read each, then the
		// URL list again for masking) cost a git process apiece.
		remotes, err := git.RemoteFetchURLs(ctx, repo)
		if err != nil {
			return prHeadsMsg{op: op, gen: gen, listErr: err}
		}
		var targets, mask []string
		repos := map[string]clone.ForgeRepo{}
		for _, r := range remotes {
			if r.URL == "" {
				continue
			}
			mask = append(mask, r.URL)
			if _, _, ok := clone.RemoteIdentity(r.URL); ok {
				targets = append(targets, r.Name)
				// The forge is addressed by scheme, host, port and
				// owner/name — not by the comparison identity, which folds
				// scheme and port away.
				repos[r.Name], _ = clone.ParseForgeRepo(r.URL)
			}
		}
		lg.Printf("worktree op=%d gen=%d: pr heads: %d remotes listed in %v", op, gen, len(remotes), time.Since(start).Round(time.Millisecond))
		results := hop.Parallel(targets, func(remote string) remotePRResult {
			rctx, rcancel := context.WithTimeout(ctx, prHeadsTimeout)
			defer rcancel()
			rstart := time.Now()
			refs, err := git.LsRemotePRs(rctx, repo, remote, mask)
			lg.Printf("worktree op=%d gen=%d: pr heads %s: %d refs in %v err=%s ctx=%s", op, gen, clone.Sanitize(remote), len(refs), time.Since(rstart).Round(time.Millisecond), logErr(err), ctxReason(rctx))
			if err != nil {
				return remotePRResult{remote: remote, repo: repos[remote], err: err}
			}
			hs := []worktree.PRHead{}
			for _, r := range refs {
				hs = append(hs, worktree.PRHead{Remote: r.Remote, Number: r.Number, SHA: r.SHA})
			}
			return remotePRResult{remote: remote, repo: repos[remote], heads: hs}
		})
		lg.Printf("worktree op=%d gen=%d: pr heads: %d remotes in %v", op, gen, len(targets), time.Since(start).Round(time.Millisecond))
		return prHeadsMsg{op: op, gen: gen, remotes: targets, results: results}
	}
}

// prInfoTimeout bounds each forge query; details are best effort.
const prInfoTimeout = 15 * time.Second

// loadPRInfo asks the forge for the title and state of every PR shown, one
// query per repository, concurrently. Nothing runs without a forge (gh not
// installed) or without PRs to ask about. Failures are logged, never shown.
func (m HopModel) loadPRInfo() tea.Cmd {
	b := m.wt
	if m.forge == nil {
		return nil
	}
	wanted := b.prInfoWanted()
	if len(wanted) == 0 {
		return nil
	}
	repos := make([]clone.ForgeRepo, 0, len(wanted))
	for repo := range wanted {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].String() < repos[j].String() })
	// Both the refs and the PR heads trigger a request when they arrive
	// (whichever lands second has the full picture); the same question is
	// not asked twice within a generation.
	var key strings.Builder
	fmt.Fprintf(&key, "%d", b.prGen)
	for _, repo := range repos {
		fmt.Fprintf(&key, "|%s:%v", repo, wanted[repo])
	}
	if key.String() == b.prInfoAsked {
		return nil
	}
	b.prInfoAsked = key.String()
	if b.prInfoCancel != nil {
		b.prInfoCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.prInfoCancel = cancel
	b.prInfoReq++
	forge, op, gen, req, lg := m.forge, b.op, b.prGen, b.prInfoReq, m.log
	return func() tea.Msg {
		defer cancel()
		start := time.Now()
		results := hop.Parallel(repos, func(repo clone.ForgeRepo) prInfoResult {
			rctx, rcancel := context.WithTimeout(ctx, prInfoTimeout)
			defer rcancel()
			rstart := time.Now()
			infos, err := forge.PullRequests(rctx, repo, wanted[repo])
			// Logged here, in the command, so a superseded request (whose
			// message the handler drops) still leaves its trace: how long
			// it ran and whether the deadline or a cancellation ended it.
			lg.Printf("worktree op=%d gen=%d req=%d: pr details %s: asked %d, answered %d in %v err=%s ctx=%s",
				op, gen, req, clone.Sanitize(repo.String()), len(wanted[repo]), len(infos), time.Since(rstart).Round(time.Millisecond), logErr(err), ctxReason(rctx))
			return prInfoResult{repo: repo, infos: infos, err: err}
		})
		lg.Printf("worktree op=%d gen=%d req=%d: pr details: %d repositories in %v", op, gen, req, len(repos), time.Since(start).Round(time.Millisecond))
		return prInfoMsg{op: op, gen: gen, request: req, results: results}
	}
}

// logErr renders an error for a log line with its text sanitized (git's and
// gh's stderr quote remote-controlled strings), "<nil>" for none.
func logErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return clone.Sanitize(err.Error())
}

// ctxReason names how a context stands, for logs: "live", "deadline" or
// "canceled" — the last two both make gh report "signal: killed".
func ctxReason(ctx context.Context) string {
	switch ctx.Err() {
	case nil:
		return "live"
	case context.DeadlineExceeded:
		return "deadline"
	default:
		return "canceled"
	}
}

// updateBranch handles messages while the branch screen is active.
func (m HopModel) updateBranch(msg tea.Msg) (tea.Model, tea.Cmd) {
	b := m.wt
	switch msg := msg.(type) {
	case refsLoadedMsg:
		if msg.op != b.op {
			return m, nil
		}
		b.loading = false
		if msg.err != nil {
			b.refsReady = false
			m.errMsg = msg.err.Error()
			m.log.Printf("worktree: refs: %v", msg.err)
			return m, nil
		}
		b.refsReady = true
		b.setBranches(msg.branches)
		// PR heads may have landed first; with the branches known the
		// details of their PRs can be asked for now.
		return m, m.loadPRInfo()
	case fetchDoneMsg:
		if msg.op != b.op {
			return m, nil
		}
		b.fetching = false
		if msg.err != nil {
			// `git fetch --all` fails as a whole when any one remote is
			// unreachable, but the others may well have fetched. Show the
			// error, yet still reload the refs and the PR annotations —
			// each remote's annotation update can fail individually anyway.
			m.errMsg = msg.err.Error()
			m.log.Printf("worktree: fetch: %v", msg.err)
			b.loading = true
			return m, tea.Batch(m.loadRefs(), m.loadPRHeads())
		}
		b.status = "fetched"
		b.loading = true
		return m, tea.Batch(m.loadRefs(), m.loadPRHeads())
	case prHeadsMsg:
		if msg.op != b.op || msg.gen != b.prGen {
			return m, nil // superseded generation or another screen
		}
		if msg.listErr != nil {
			m.log.Printf("worktree: pull request heads: %s", clone.Sanitize(msg.listErr.Error()))
			return m, nil // keep everything: the remote list itself failed
		}
		for _, r := range msg.results {
			if r.err != nil {
				m.log.Printf("worktree: pull request heads: %s: %s", clone.Sanitize(r.remote), clone.Sanitize(r.err.Error()))
			}
		}
		b.applyPRResults(msg.remotes, msg.results)
		return m, m.loadPRInfo()
	case prInfoMsg:
		if msg.op != b.op || msg.gen != b.prGen || msg.request != b.prInfoReq {
			// Another screen, a superseded generation, or a fetch that a
			// later one within this generation replaced.
			return m, nil
		}
		for _, r := range msg.results {
			if r.err != nil && !errors.Is(r.err, forge.ErrUnsupportedHost) {
				m.log.Printf("worktree: pull request details: %s: %s", clone.Sanitize(r.repo.String()), clone.Sanitize(r.err.Error()))
			}
		}
		b.applyPRInfo(msg.results)
		return m, nil
	case worktreeDoneMsg:
		if msg.op != b.op {
			return m, nil
		}
		b.creating = false
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			m.log.Printf("worktree: create: %v", msg.err)
			return m, nil
		}
		m.log.Printf("worktree: created and opened")
		m.cancelAll()
		m.quit = true
		return m, tea.Quit
	case tea.KeyMsg:
		if m.errMsg != "" {
			// The error text advertises ctrl-r (retry) and esc (back): honour
			// them in one keypress instead of merely dismissing the message.
			// refsReady is untouched, so a failed load still blocks creation.
			m.errMsg = ""
			switch msg.Type {
			case tea.KeyCtrlC:
				m.cancelAll()
				return m, tea.Quit
			case tea.KeyCtrlR:
				if b.busy() {
					return m, nil
				}
				b.loading = true
				b.status = ""
				return m, m.loadRefs()
			case tea.KeyEsc:
				if b.busy() {
					return m, nil
				}
				if b.remote != nil {
					b.remote = nil
					b.status = ""
					b.input.SetValue(b.filter)
					b.refilter()
					return m, nil
				}
				return m.leaveBranchScreen()
			}
			return m, nil
		}
		switch msg.Type {
		case tea.KeyCtrlC:
			m.cancelAll()
			return m, tea.Quit
		case tea.KeyEsc:
			if b.busy() {
				return m, nil
			}
			if b.remote != nil {
				// back to the list, restoring the filter that found the remote
				b.remote = nil
				b.status = ""
				b.input.SetValue(b.filter)
				b.refilter()
				return m, nil
			}
			return m.leaveBranchScreen()
		case tea.KeyCtrlR:
			if b.busy() {
				return m, nil
			}
			b.loading = true
			b.status = ""
			return m, m.loadRefs()
		case tea.KeyCtrlF:
			if b.busy() || !b.refsReady {
				return m, nil
			}
			b.fetching = true
			b.status = ""
			return m, m.fetch()
		case tea.KeyEnter:
			if b.busy() || !b.refsReady {
				return m, nil
			}
			return m.submitBranch()
		case tea.KeyUp, tea.KeyCtrlP:
			b.top = m.branchScrollStart()
			if b.cursor > 0 {
				b.cursor--
			}
			b.top = m.branchScrollStart()
			return m, nil
		case tea.KeyDown:
			b.top = m.branchScrollStart()
			if b.cursor < b.rowCount()-1 {
				b.cursor++
			}
			b.top = m.branchScrollStart()
			return m, nil
		}
	}
	var cmd tea.Cmd
	before := b.input.Value()
	b.input, cmd = b.input.Update(msg)
	if b.input.Value() != before && b.remote == nil {
		b.refilter()
	}
	return m, cmd
}

// submitBranch turns the current selection into a plan and runs it.
//
// Choosing a remote branch never creates anything directly: it switches to
// the local-name state with the input prefilled with the remote's short
// name. Enter there creates that local branch tracking the remote, or — if
// the name was edited — a differently named branch based on the remote.
func (m HopModel) submitBranch() (tea.Model, tea.Cmd) {
	b := m.wt
	locals := b.locals
	var sel *worktree.Branch
	var newName string
	if b.remote != nil {
		sel = b.remote
		newName = b.newName()
		if newName == "" {
			m.errMsg = "enter the local branch name to create from " + sel.Name
			return m, nil
		}
	} else {
		s, ok := b.selected()
		if !ok {
			return m, nil
		}
		sel = s
		newName = b.newName()
		if sel != nil && sel.IsRemote() {
			b.remote = sel
			b.filter = b.input.Value()
			b.input.SetValue(sel.Short)
			b.input.CursorEnd()
			if locals[sel.Short] {
				b.status = fmt.Sprintf("local branch %q already exists: enter a different name to create from %s", sel.Short, sel.Name)
			} else {
				b.status = fmt.Sprintf("local branch to create from %s (enter: create, esc: back)", sel.Name)
			}
			return m, nil
		}
	}
	plan, err := worktree.Make(sel, newName, locals, time.Now())
	if err != nil {
		m.errMsg = err.Error()
		if path, ok := b.inUse[newName]; ok {
			// The name collides with a branch that already lives in a
			// worktree (hidden from this list): say where, and where to go.
			m.errMsg = fmt.Sprintf("%q is already checked out in a worktree (%s): open it from the picker", newName, path)
		}
		return m, nil
	}
	b.creating = true
	m.log.Printf("worktree: create --cwd %s --branch %s --base %q upstream=%q", b.repo, plan.Branch, plan.Base, plan.Upstream)
	return m, m.createWorktree(plan)
}

func (m HopModel) viewBranch() string {
	b := m.wt
	var sb strings.Builder
	writeln := func(parts ...string) {
		for _, p := range parts {
			sb.WriteString(p)
		}
		sb.WriteByte('\n')
	}
	// Display boundary: everything that originates outside the program
	// (git ref names, worktree paths, error text quoting them, repository
	// labels, and the input value when it was prefilled from a ref) is
	// sanitized here, on the displayed copy only. The values kept in the
	// model stay intact for git/herdr.
	disp := clone.Sanitize
	clip := func(line string) string {
		if m.width > 0 {
			return ansi.Truncate(line, m.width, "…")
		}
		return line
	}
	writeln(inputView(b.input))
	if m.errMsg != "" {
		writeln(styleErr.Render("error: " + disp(m.errMsg)))
		writeln(styleDim.Render("press any key to continue"))
		return sb.String()
	}
	// Same layout as the picker: counter + rule, then the key hints with the
	// repository this screen creates worktrees for.
	counter := fmt.Sprintf(" %d/%d ", len(b.view), len(b.branches))
	sep := styleCount.Render(counter)
	if w := m.width - lipgloss.Width(counter); w > 0 {
		sep += styleRule.Render(strings.Repeat("─", w))
	}
	writeln(sep)
	hints := []keyHint{{"enter", "create"}, {"ctrl-f", "fetch"}, {"ctrl-r", "reload"}, {"esc", "back"}}
	writeln(clip(renderKeyHints(hints) + "  " + styleDim.Render(disp(b.label))))
	if b.status != "" {
		writeln(clip(styleWarn.Render(disp(b.status))))
	}
	switch {
	case b.loading:
		writeln(styleDim.Render("loading branches..."))
		return sb.String()
	case b.fetching:
		writeln(styleDim.Render("fetching..."))
		return sb.String()
	case b.creating:
		writeln(styleDim.Render("creating worktree..."))
		return sb.String()
	case !b.refsReady:
		writeln(styleErr.Render("failed to load branches"), "  ", styleDim.Render("ctrl-r: retry  esc: back"))
		return sb.String()
	case b.remote != nil:
		writeln(styleDim.Render(fmt.Sprintf("  --branch %s --base %s", disp(b.newName()), disp(b.remote.Ref))))
		return sb.String()
	}
	rows := m.branchListRows()
	start := m.branchScrollStart()
	// Two passes, as in the picker: the names of every row (not just the
	// visible ones, so scrolling never shifts the column) decide where the
	// PR column starts, capped so one long name cannot push it off-screen.
	allSegs := make([][]rowSeg, b.rowCount())
	leftW := 0
	for i := range allSegs {
		allSegs[i] = m.branchRowSegs(i)
		leftW = max(leftW, segsWidth(allSegs[i]))
	}
	if m.width > 0 {
		leftW = min(leftW, m.width*6/10)
	}
	prW := 0
	for i := range allSegs {
		prW = max(prW, segsWidth(m.branchPRColumn(i)))
	}
	for i := start; i < b.rowCount() && i < start+rows; i++ {
		segs := allSegs[i]
		if prW > 0 {
			segs = truncateSegs(segs, leftW)
			if pad := leftW - segsWidth(segs); pad > 0 {
				segs = append(segs, rowSeg{text: strings.Repeat(" ", pad)})
			}
			if pr := m.branchPRColumn(i); len(pr) > 0 {
				segs = append(segs, rowSeg{text: "  "})
				segs = append(segs, pr...)
				if title := m.branchTitleColumn(i); len(title) > 0 {
					if pad := prW - segsWidth(pr); pad > 0 {
						segs = append(segs, rowSeg{text: strings.Repeat(" ", pad)})
					}
					segs = append(segs, rowSeg{text: "  "})
					segs = append(segs, title...)
				}
			}
		}
		if m.width > 2 {
			segs = truncateSegs(segs, m.width-2) // 2: the marker column
		}
		if i == b.cursor {
			if pad := m.width - 2 - segsWidth(segs); pad > 0 {
				segs = append(segs, rowSeg{text: strings.Repeat(" ", pad), quiet: true})
			}
			sb.WriteString(styleCursor.Background(cursorRowBg).Render("> "))
			for _, s := range segs {
				if s.quiet {
					sb.WriteString(s.style.Background(cursorRowBg).Render(s.text))
				} else {
					sb.WriteString(s.style.Bold(true).UnsetFaint().Background(cursorRowBg).Render(s.text))
				}
			}
		} else {
			sb.WriteString("  ")
			for _, s := range segs {
				sb.WriteString(s.style.Render(s.text))
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// branchRowSegs assembles the name cell of row i: the leading new-branch
// row, or a branch name with its remote-name prefix dimmed. PR labels live
// in their own column (branchPRColumn).
func (m HopModel) branchRowSegs(i int) []rowSeg {
	b := m.wt
	disp := clone.Sanitize
	var segs []rowSeg
	add := func(text string, style lipgloss.Style) {
		segs = append(segs, rowSeg{text: text, style: style})
	}
	if i > 0 {
		br := b.branches[b.view[i-1]]
		// Highlight positions index SearchText (name + PR labels); only the
		// ones inside the name light up here.
		var pos []int
		for _, p := range b.matches[b.view[i-1]] {
			if p < len([]rune(br.Name)) {
				pos = append(pos, p)
			}
		}
		base := lipgloss.NewStyle()
		if rest, ok := strings.CutPrefix(br.Name, br.Remote+"/"); br.IsRemote() && ok {
			// The remote-name prefix is a branch name's "host": dim it
			// instead of tagging the row with a word. Split on the remote's
			// own name — remote names may contain "/" themselves.
			remote := br.Remote
			plen := len([]rune(remote)) + 1
			var ppos, rpos []int
			for _, q := range pos {
				if q < plen {
					ppos = append(ppos, q)
				} else {
					rpos = append(rpos, q-plen)
				}
			}
			segs = append(segs, matchedSegs(remote+"/", styleDim, ppos, false)...)
			segs = append(segs, matchedSegs(rest, base, rpos, false)...)
		} else {
			segs = append(segs, matchedSegs(br.Name, base, pos, false)...)
		}
	} else {
		if n := b.newName(); n != "" {
			add(disp(n), lipgloss.NewStyle())
			add("  new", styleOpen.Faint(true))
			add("  (from HEAD)", styleDim)
		} else {
			add(worktree.AutoName(time.Now()), lipgloss.NewStyle())
			add("  new", styleOpen.Faint(true))
			add("  (auto name, from HEAD)", styleDim)
		}
	}
	return segs
}

// branchPRColumn is the PR cell of row i: the branch's PR labels
// ("#12", "upstream#12 origin#7"), with the runes a "#12" query hit lit up.
// Empty for the new-branch row and branches without a PR.
func (m HopModel) branchPRColumn(i int) []rowSeg {
	b := m.wt
	if i == 0 {
		return nil
	}
	br := b.branches[b.view[i-1]]
	if !br.HasPR() {
		return nil
	}
	// SearchText is "name label label…"; positions past the name and its
	// separating space index the joined labels. Each label is styled by
	// its own PR's state, so the positions are split per label.
	off := len([]rune(br.Name)) + 1
	var pos []int
	for _, p := range b.matches[b.view[i-1]] {
		if p >= off {
			pos = append(pos, p-off)
		}
	}
	var segs []rowSeg
	start := 0
	for j, pr := range br.PRs {
		n := len([]rune(pr.Label))
		var mine []int
		for _, p := range pos {
			if p >= start && p < start+n {
				mine = append(mine, p-start)
			}
		}
		if j > 0 {
			segs = append(segs, rowSeg{text: " "})
		}
		// A dimmed label (settled or unknown PR) stays dim under the cursor
		// too — the cursor row lifts dimming from its text, which would
		// make every PR look alive at the moment it is selected.
		segs = append(segs, matchedSegs(pr.Label, prLabelStyle(pr.Info), mine, !pr.Info.Alive())...)
		start += n + 1
	}
	return segs
}

// prLabelStyle colours a PR number only once the forge has confirmed the
// PR is alive (open or draft). Until then — and for merged or closed PRs,
// or when no forge is available — the number stays in the dim tone of the
// details cell, so colour means exactly one thing: "this one is still
// going", and nothing flashes green before sinking.
func prLabelStyle(info worktree.PRInfo) lipgloss.Style {
	if info.Alive() {
		return styleOpen
	}
	return styleDim
}

// titleMatch records a title-only hit: which PR of the branch, and the
// rune positions of the query's words in its title.
type titleMatch struct {
	pr  int
	pos []int
}

// titleWordsMatch tries wordsMatch against every PR title of a branch and
// reports the first hit.
func titleWordsMatch(query string, prs []worktree.BranchPR) (titleMatch, bool) {
	for j, pr := range prs {
		if pos, ok := wordsMatch(query, pr.Info.Title); ok {
			return titleMatch{pr: j, pos: pos}, true
		}
	}
	return titleMatch{}, false
}

// shownPR picks the PR whose details the row shows when a branch has more
// than one: the PR a title search hit, else the first one still alive,
// else the first with details at all. ok is false when nothing is known.
func (b *branchState) shownPR(idx int) (worktree.BranchPR, []int, bool) {
	prs := b.branches[idx].PRs
	if tm, ok := b.titleMatches[idx]; ok && tm.pr < len(prs) {
		return prs[tm.pr], tm.pos, true
	}
	for _, pr := range prs {
		if pr.Info.Alive() {
			return pr, nil, true
		}
	}
	for _, pr := range prs {
		if pr.Info.HasInfo() {
			return pr, nil, true
		}
	}
	return worktree.BranchPR{}, nil, false
}

// branchTitleColumn is the details cell of row i: the shown PR's state,
// when it is anything but plainly open, followed by its title, with the
// runes a title search hit lit up. Emphasis follows what one is looking
// for: an open PR's title reads in the normal tone, a draft's and a settled
// (merged, closed) PR's whole cell is dimmed. When several PRs share the
// commit the cell shows one of them (shownPR), and a label whose PR is not
// the shown one is still coloured by its own state in the PR column. Empty
// until the forge has answered.
func (m HopModel) branchTitleColumn(i int) []rowSeg {
	b := m.wt
	if i == 0 {
		return nil
	}
	pr, pos, ok := b.shownPR(b.view[i-1])
	if !ok {
		return nil
	}
	switch pr.Info.State {
	case worktree.PRDraft, worktree.PRMerged, worktree.PRClosed:
		segs := []rowSeg{{text: string(pr.Info.State) + " ", style: styleDim, quiet: true}}
		return append(segs, matchedSegs(pr.Info.Title, styleDim, pos, true)...)
	default:
		return matchedSegs(pr.Info.Title, lipgloss.NewStyle(), pos, false)
	}
}

// wordsMatch reports whether every whitespace-separated word of query
// occurs in text (case-insensitively) and returns the rune positions of the
// occurrences, for highlighting. An empty query matches nothing.
func wordsMatch(query, text string) ([]int, bool) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 || text == "" {
		return nil, false
	}
	lower := []rune(strings.ToLower(text))
	seen := map[int]bool{}
	for _, w := range words {
		wr := []rune(w)
		at := runeIndex(lower, wr)
		if at < 0 {
			return nil, false
		}
		for k := range wr {
			seen[at+k] = true
		}
	}
	pos := make([]int, 0, len(seen))
	for p := range seen {
		pos = append(pos, p)
	}
	sort.Ints(pos)
	return pos, true
}

// runeIndex is strings.Index over rune slices.
func runeIndex(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if slices.Equal(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// branchListRows mirrors viewBranch's header accounting, so cursor movement
// scrolls with the same geometry the screen renders with.
func (m HopModel) branchListRows() int {
	used := 3 // input, counter, hints
	if m.wt != nil && m.wt.status != "" {
		used++
	}
	rows := m.height - used - 1
	if rows < 1 {
		rows = 10
	}
	return rows
}

// branchScrollStart anchors the branch list's viewport the way the picker's
// scrollStart does.
func (m HopModel) branchScrollStart() int {
	b := m.wt
	rows := m.branchListRows()
	top := max(0, min(b.top, b.rowCount()-rows))
	top = min(top, b.cursor)
	return max(top, b.cursor-rows+1)
}
