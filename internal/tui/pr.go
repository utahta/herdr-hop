package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/worktree"
)

// prPhase is where a pull-request operation is.
type prPhase int

const (
	prPhaseNone  prPhase = iota
	prPhaseCheck         // route decision + existing checks: cancellable
	prPhaseFetch         // git fetch: cancellable
	prPhaseHerdr         // herdr worktree create/open: not cancellable
)

func (p prPhase) String() string {
	switch p {
	case prPhaseCheck:
		return "checking"
	case prPhaseFetch:
		return "fetching"
	case prPhaseHerdr:
		return "creating worktree"
	}
	return ""
}

// openTarget is an existing worktree to open.
type openTarget struct{ repoRoot, path string }

// prPlan is the outcome of the check stage: either a choice the user must
// make, or the fetch (optional) and the herdr action to run.
type prPlan struct {
	// choices, when non-empty, are equally plausible source branches; the
	// picker expands them into rows and the operation ends here.
	choices []string
	// fetch, when fetchSpec is non-empty, runs before the herdr action.
	fetchSrc  string
	fetchSpec string
	// exactly one of the following is the herdr action:
	focus  string         // focus this workspace
	open   *openTarget    // open this existing worktree
	create *worktree.Plan // create (or check out) via herdr worktree create
	// recordProvenance: after a fallback create, write branch.<n>.hop-pr.
	recordProvenance bool
	// mergeHint: an existing fallback worktree was opened and its PR ref
	// refreshed; tell the user how to merge instead of quitting silently.
	mergeHint bool
}

type prCheckedMsg struct {
	op   int
	plan prPlan
	err  error
}
type prFetchedMsg struct {
	op  int
	err error
}
type prDoneMsg struct {
	op        int
	err       error
	mergeHint bool
}

// prState is an in-flight "open this pull request as a worktree" operation.
type prState struct {
	op       int
	pr       clone.PR
	repo     string
	label    string // parent workspace label
	chosen   string // user-chosen source branch (from an expanded row), or ""
	phase    prPhase
	note     string
	canceled bool
	cancel   context.CancelFunc
	ctx      context.Context
	plan     prPlan
}

func (s *prState) running() bool { return s != nil && s.phase != prPhaseNone }

// cancellable reports whether Esc/Ctrl-C may still abort the operation:
// only while git is doing the work. Once herdr is creating or opening the
// worktree the outcome cannot be undone from here, so keys are ignored.
func (s *prState) cancellable() bool {
	return s != nil && (s.phase == prPhaseCheck || s.phase == prPhaseFetch)
}

// startPR begins the PR operation for repo. chosenBranch is non-empty when
// the user picked a source branch from an expanded row.
func (m HopModel) startPR(pr clone.PR, repo, chosenBranch string) (HopModel, tea.Cmd) {
	m.prOp++
	ctx, cancel := context.WithCancel(context.Background())
	st := &prState{op: m.prOp, pr: pr, repo: repo, label: hop.WorkspaceLabel(m.labelForRepo(repo)), chosen: chosenBranch, phase: prPhaseCheck, cancel: cancel, ctx: ctx}
	m.pr = st
	m.log.Printf("pr: %s -> %s", clone.Sanitize(pr.Label()), clone.Sanitize(repo))
	h, git := m.h, m.git
	op := st.op
	return m, func() tea.Msg {
		plan, err := prCheck(ctx, h, git, pr, repo, chosenBranch)
		return prCheckedMsg{op, plan, err}
	}
}

// labelForRepo finds the display label of a repository root among the
// candidates (for the parent workspace label); falls back to the path.
func (m HopModel) labelForRepo(repo string) string {
	for _, c := range m.cands {
		if root, label, ok := c.EffectiveRoot(); ok && root == repo {
			return label
		}
	}
	return repo
}

// prFetchSource picks where to fetch from: the remote whose effective fetch
// URL is the PR's repository (isRemote=true), else the repository URL.
func prFetchSource(git GitOps, repo string, pr clone.PR) (src string, isRemote bool, err error) {
	names, err := git.Remotes(repo)
	if err != nil {
		return "", false, err
	}
	for _, n := range names {
		u, err := git.FetchURL(repo, n)
		if err != nil || u == "" {
			continue
		}
		if clone.RepoPathOf(u) == pr.RepoPath {
			return n, true, nil
		}
	}
	return pr.RepoURL, false, nil
}

// prCheck decides the whole plan without changing anything (§5–§7 of the
// design): route decision against the server's snapshot, then the checks of
// the chosen route.
func prCheck(ctx context.Context, h herdr.Client, git GitOps, pr clone.PR, repo, chosen string) (prPlan, error) {
	src, isRemote, err := prFetchSource(git, repo, pr)
	if err != nil {
		return prPlan{}, err
	}
	// Without a matching remote there is nothing to hang a tracking branch
	// on: skip the matching entirely and take the fallback route.
	if !isRemote {
		return prFallback(h, git, pr, repo, src)
	}
	// A fallback artefact of this very PR (a pr/<N> branch whose recorded
	// provenance matches) pins the operation to the fallback route, even if
	// the source branch has become trackable since (a widened refspec, a
	// restored branch): the picker already shows that worktree as the thing
	// Enter will open, and switching routes here would create a second
	// worktree beside it. A pr/<N> of another PR (or of unknown origin)
	// does not pin anything — the tracked route uses a different name.
	if pinned, err := hasFallbackArtifact(git, pr, repo); err != nil {
		return prPlan{}, err
	} else if pinned {
		return prFallback(h, git, pr, repo, src)
	}
	// The tracked-branch route needs more than the ref itself: git only
	// recognises refs/remotes/<remote>/X as an upstream when the remote's
	// fetch mapping covers refs/heads/X. A single-branch clone would let us
	// fetch the ref and create the worktree, and then fail on
	// --set-upstream-to — a partial state discovered too late. Decide here.
	specs, err := git.FetchRefspecs(repo, src)
	if err != nil {
		return prPlan{}, err
	}
	covers := func(branch string) bool {
		return worktree.RefspecsCover(specs, "refs/heads/"+branch, "refs/remotes/"+src+"/"+branch)
	}
	if chosen != "" && !covers(chosen) {
		return prFallback(h, git, pr, repo, src)
	}
	branch := chosen
	if branch == "" {
		st, err := git.LsRemotePR(ctx, repo, src, pr.HeadRef())
		if err != nil {
			return prPlan{}, err
		}
		if st.HeadSHA == "" {
			return prPlan{}, fmt.Errorf("the remote does not advertise %s (does the pull request exist?)", pr.HeadRef())
		}
		// Origin inference first, over ALL branches: whether a branch is
		// trackable (fetch mapping) says nothing about whether it is the
		// PR's source, so filtering by coverage here could turn an
		// ambiguous match into a confidently wrong unique one.
		var matches []string
		for name, sha := range st.Branches {
			if sha == st.HeadSHA {
				matches = append(matches, name)
			}
		}
		sort.Strings(matches) // map order is random; keep the choice rows stable
		switch {
		// The unique match is trusted only when the default branch is known
		// and the match is not it: with an unknown default ("" — the remote
		// sent no HEAD symref), a deleted source branch whose commit happens
		// to sit on the default branch would be mistaken for the source.
		case len(matches) == 1 && st.DefaultBranch != "" && matches[0] != st.DefaultBranch:
			branch = matches[0]
			// Only now does trackability matter: an identified source that
			// the fetch mapping cannot track goes to the fallback route.
			if !covers(branch) {
				return prFallback(h, git, pr, repo, src)
			}
		case len(matches) > 1:
			// Equal today does not mean equal tomorrow; a wrong upstream
			// would pull someone else's changes. The user picks.
			return prPlan{choices: matches}, nil
		default:
			// No match, or only the default branch (probably a coincidence:
			// the source branch was deleted and main happens to point at the
			// same commit). SHA equality is a heuristic, not provenance.
			return prFallback(h, git, pr, repo, src)
		}
	}
	return prRemoteBranch(h, git, repo, src, branch)
}

// hasFallbackArtifact reports whether a local pr/<N> branch exists whose
// recorded provenance is exactly this pull request.
func hasFallbackArtifact(git GitOps, pr clone.PR, repo string) (bool, error) {
	lines, err := git.Refs(repo)
	if err != nil {
		return false, err
	}
	for _, b := range worktree.ParseRefs(lines, nil) {
		if b.Ref != "refs/heads/"+pr.BranchName() {
			continue
		}
		v, err := git.ConfigGet(repo, pr.ProvenanceKey())
		if err != nil {
			return false, err
		}
		return v == pr.Provenance(), nil
	}
	return false, nil
}

// prRemoteBranch is §6: delegate to the ordinary tracked-branch machinery.
func prRemoteBranch(h herdr.Client, git GitOps, repo, remote, branch string) (prPlan, error) {
	// The server-side name is about to become a local branch name and a
	// remote-tracking ref; validate it like the ordinary flow does before
	// anything is fetched. "HEAD" would even address the symbolic
	// refs/remotes/<remote>/HEAD.
	if !worktree.ValidName(branch) {
		return prPlan{}, fmt.Errorf("%w: %q cannot be used as a local branch name", worktree.ErrBadName, branch)
	}
	upstreamRef := "refs/remotes/" + remote + "/" + branch
	lines, err := git.Refs(repo)
	if err != nil {
		return prPlan{}, err
	}
	var local *worktree.Branch
	for _, b := range worktree.ParseRefs(lines, nil) {
		if b.Ref == "refs/heads/"+branch {
			bb := b
			local = &bb
			break
		}
	}
	if local != nil {
		// The local branch is reused only when it tracks exactly the chosen
		// remote branch; anything else is another use of the name.
		if local.Upstream != upstreamRef {
			return prPlan{}, fmt.Errorf("%w: local branch %q does not track %s", worktree.ErrLocalExists, branch, upstreamRef)
		}
		if local.InUse() {
			return prExistingWorktree(h, repo, branch, "")
		}
		// Branch exists and tracks the right remote: just check it out.
		return prPlan{create: &worktree.Plan{Branch: branch}}, nil
	}
	if resolves, err := git.BranchExists(repo, branch); err != nil {
		return prPlan{}, err
	} else if resolves {
		return prPlan{}, fmt.Errorf("%w: %q resolves to an existing local branch with different letter case", worktree.ErrNameTaken, branch)
	}
	return prPlan{
		fetchSrc:  remote,
		fetchSpec: "+refs/heads/" + branch + ":" + upstreamRef,
		create:    &worktree.Plan{Branch: branch, Base: upstreamRef, Upstream: upstreamRef, Creates: true},
	}, nil
}

// prFallback is §7: the dedicated-ref route with pr/<N> and provenance.
func prFallback(h herdr.Client, git GitOps, pr clone.PR, repo, src string) (prPlan, error) {
	branch := pr.BranchName()
	provenanceErr := func(where string) error {
		v, err := git.ConfigGet(repo, pr.ProvenanceKey())
		switch {
		case err != nil:
			return err
		case v == pr.Provenance():
			return nil
		case v == "":
			return fmt.Errorf("branch %s already exists%s but was not created for this pull request; rename or remove it first", branch, where)
		default:
			return fmt.Errorf("branch %s already exists%s for a different pull request (%s); rename or remove it first", branch, where, v)
		}
	}
	plan := prPlan{fetchSrc: src, fetchSpec: "+" + pr.HeadRef() + ":" + pr.LocalRef()}

	list, err := h.WorktreeList(repo)
	if err != nil {
		return prPlan{}, err
	}
	for _, wt := range list.Worktrees {
		if wt.Branch != branch {
			continue
		}
		switch {
		case !wt.IsLinkedWorktree:
			return prPlan{}, fmt.Errorf("%s is checked out in the main checkout (%s); open that workspace instead", branch, wt.Path)
		case wt.IsPrunable:
			return prPlan{}, fmt.Errorf("the worktree of %s (%s) no longer exists on disk; run `git worktree prune` in %s and retry", branch, wt.Path, repo)
		}
		if err := provenanceErr(" (worktree " + wt.Path + ")"); err != nil {
			return prPlan{}, err
		}
		// Existing worktree: refresh the dedicated ref, then just open it.
		plan.mergeHint = true
		if wt.OpenWorkspaceID != nil && *wt.OpenWorkspaceID != "" {
			plan.focus = *wt.OpenWorkspaceID
		} else {
			plan.open = &openTarget{repoRoot: list.Source.RepoRoot, path: wt.Path}
		}
		return plan, nil
	}

	lines, err := git.Refs(repo)
	if err != nil {
		return prPlan{}, err
	}
	for _, b := range worktree.ParseRefs(lines, nil) {
		if b.Ref != "refs/heads/"+branch {
			continue
		}
		if b.InUse() {
			return prPlan{}, fmt.Errorf("%w: %s", worktree.ErrInUse, b.WorktreePath)
		}
		if err := provenanceErr(""); err != nil {
			return prPlan{}, err
		}
		// Branch exists for this PR but has no worktree: refresh the ref and
		// check the branch out (it is not moved).
		plan.create = &worktree.Plan{Branch: branch}
		plan.mergeHint = true
		return plan, nil
	}
	if resolves, err := git.BranchExists(repo, branch); err != nil {
		return prPlan{}, err
	} else if resolves {
		return prPlan{}, fmt.Errorf("%w: %q resolves to an existing local branch with different letter case", worktree.ErrNameTaken, branch)
	}
	plan.create = &worktree.Plan{Branch: branch, Base: pr.LocalRef(), Creates: true}
	plan.recordProvenance = true
	return plan, nil
}

// prExistingWorktree turns "branch is checked out somewhere" into an open
// or an in-use error, based on herdr's worktree list.
func prExistingWorktree(h herdr.Client, repo, branch, _ string) (prPlan, error) {
	list, err := h.WorktreeList(repo)
	if err != nil {
		return prPlan{}, err
	}
	for _, wt := range list.Worktrees {
		if wt.Branch != branch {
			continue
		}
		switch {
		case !wt.IsLinkedWorktree:
			return prPlan{}, fmt.Errorf("%s is checked out in the main checkout (%s); open that workspace instead", branch, wt.Path)
		case wt.IsPrunable:
			return prPlan{}, fmt.Errorf("the worktree of %s (%s) no longer exists on disk; run `git worktree prune` in %s and retry", branch, wt.Path, repo)
		}
		if wt.OpenWorkspaceID != nil && *wt.OpenWorkspaceID != "" {
			return prPlan{focus: *wt.OpenWorkspaceID}, nil
		}
		return prPlan{open: &openTarget{repoRoot: list.Source.RepoRoot, path: wt.Path}}, nil
	}
	return prPlan{}, fmt.Errorf("%w", worktree.ErrInUse)
}

// prFetchCmd runs the plan's fetch (cancellable).
func (m HopModel) prFetchCmd() tea.Cmd {
	st := m.pr
	git, repo, op, ctx, plan := m.git, st.repo, st.op, st.ctx, st.plan
	return func() tea.Msg {
		return prFetchedMsg{op, git.FetchRef(ctx, repo, plan.fetchSrc, plan.fetchSpec)}
	}
}

// prHerdrCmd runs the herdr part of the plan (not cancellable).
func (m HopModel) prHerdrCmd() tea.Cmd {
	st := m.pr
	h, git, pr, repo, op, plan, label := m.h, m.git, st.pr, st.repo, st.op, st.plan, st.label
	return func() tea.Msg {
		switch {
		case plan.focus != "":
			if err := h.WorkspaceFocus(plan.focus); err != nil {
				return prDoneMsg{op: op, err: err}
			}
		case plan.open != nil:
			if err := h.WorktreeOpen(plan.open.repoRoot, plan.open.path); err != nil {
				return prDoneMsg{op: op, err: err}
			}
			if err := hop.RelabelParent(h, plan.open.repoRoot, label); err != nil {
				return prDoneMsg{op: op, err: fmt.Errorf("worktree opened, but renaming the parent workspace failed: %w", err)}
			}
		case plan.create != nil:
			p := plan.create
			if p.Creates {
				// The fetch between the check stage and here can take a
				// while; ask git again so a branch created meanwhile is not
				// silently checked out (and then given an upstream or a
				// provenance it never had). Same rule as the branch screen.
				exists, err := git.BranchExists(repo, p.Branch)
				if err != nil {
					return prDoneMsg{op: op, err: err}
				}
				if exists {
					return prDoneMsg{op: op, err: fmt.Errorf("%w: %q was created while fetching", worktree.ErrNameTaken, p.Branch)}
				}
			}
			if err := h.WorktreeCreate(repo, p.Branch, p.Base); err != nil {
				return prDoneMsg{op: op, err: err}
			}
			if p.Upstream != "" {
				if err := git.SetUpstream(repo, p.Branch, p.Upstream); err != nil {
					return prDoneMsg{op: op, err: fmt.Errorf("worktree created, but setting upstream failed: %w", err)}
				}
			}
			if plan.recordProvenance {
				if err := git.ConfigSet(repo, pr.ProvenanceKey(), pr.Provenance()); err != nil {
					// Partial success by design: the worktree is open. Give
					// the exact recovery command; do not retry or roll back.
					return prDoneMsg{op: op, err: fmt.Errorf(
						"the worktree is open, but recording the pull request failed (%v); to let herdr-hop recognise it next time, run: git config %s %s",
						err, pr.ProvenanceKey(), pr.Provenance())}
				}
			}
			if err := hop.RelabelParent(h, repo, label); err != nil {
				return prDoneMsg{op: op, err: fmt.Errorf("worktree created, but renaming the parent workspace failed: %w", err)}
			}
		}
		return prDoneMsg{op: op, mergeHint: plan.mergeHint}
	}
}

// updatePR handles the PR operation's messages and keys. handled is false
// when the message was not for it.
func (m HopModel) updatePR(msg tea.Msg) (HopModel, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case prCheckedMsg:
		if m.pr == nil || msg.op != m.pr.op {
			return m, nil, true
		}
		if m.pr.canceled {
			return m.finishPR(nil), nil, true
		}
		if msg.err != nil {
			return m.finishPR(msg.err), nil, true
		}
		if len(msg.plan.choices) > 0 {
			return m.expandPRChoices(msg.plan.choices), nil, true
		}
		m.pr.plan = msg.plan
		if msg.plan.fetchSpec != "" {
			m.pr.phase, m.pr.note = prPhaseFetch, clone.Sanitize(clone.Redact(msg.plan.fetchSrc))
			return m, m.prFetchCmd(), true
		}
		m.pr.phase, m.pr.note = prPhaseHerdr, ""
		return m, m.prHerdrCmd(), true
	case prFetchedMsg:
		if m.pr == nil || msg.op != m.pr.op {
			return m, nil, true
		}
		if m.pr.canceled {
			return m.finishPR(nil), nil, true
		}
		if msg.err != nil {
			return m.finishPR(msg.err), nil, true
		}
		m.pr.phase, m.pr.note = prPhaseHerdr, ""
		return m, m.prHerdrCmd(), true
	case prDoneMsg:
		if m.pr == nil || msg.op != m.pr.op {
			return m, nil, true
		}
		if msg.err != nil {
			return m.finishPR(msg.err), nil, true
		}
		st := m.pr
		m.log.Printf("pr ok: %s", clone.Sanitize(st.pr.Label()))
		if msg.mergeHint {
			// The worktree is open, but stay so the hint is readable.
			hint := fmt.Sprintf("opened; the PR ref was refreshed — merge updates with: git merge %s", st.pr.LocalRef())
			m.log.Print(hint)
			st.cancel()
			m.pr = nil
			m.errMsg = hint
			return m, nil, true
		}
		st.cancel()
		m.pr = nil
		m.quit = true
		return m, tea.Quit, true
	case tea.KeyMsg:
		if !m.pr.running() {
			return m, nil, false
		}
		if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
			if m.pr.cancellable() {
				m.pr.canceled = true
				m.pr.cancel()
			}
			// During the herdr phase keys are simply ignored.
		}
		return m, nil, true
	}
	return m, nil, false
}

// finishPR ends the operation with an error (or, when err is nil, as
// cancelled) and shows the result; the picker stays open.
func (m HopModel) finishPR(err error) HopModel {
	st := m.pr
	st.cancel()
	switch {
	case st.canceled:
		m.errMsg = "canceled"
		m.log.Printf("pr canceled: %s", clone.Sanitize(st.pr.Label()))
	case err != nil:
		m.errMsg = clone.Scrub(err.Error(), clone.NewMasker(st.pr.RepoURL))
		m.log.Printf("pr failed: %s", m.errMsg)
	}
	m.pr = nil
	return m
}

// expandPRChoices ends the operation and replaces the rows with one pull
// row per equally plausible source branch, for the user to choose.
func (m HopModel) expandPRChoices(branches []string) HopModel {
	st := m.pr
	st.cancel()
	repo, pr := st.repo, st.pr
	m.pr = nil
	label := m.labelForRepo(repo)
	m.extra = nil
	for _, b := range branches {
		m.extra = append(m.extra, hop.Candidate{
			Kind: hop.KindPull, Path: repo, Label: fmt.Sprintf("%s → %s", pr.Label(), b),
			Branch: label, RepoRoot: repo, RepoLabel: label, PRBranch: b,
		})
	}
	m.view = m.view[:0]
	for i := range m.extra {
		m.view = append(m.view, len(m.cands)+i)
	}
	m.notes = nil
	m.cursor = 0
	return m
}

// prRows computes the synthetic rows for a parsed PR URL. Candidates whose
// RepoPath matches get one pull row per distinct repository (noting an
// existing pr/<N> worktree); a checkout that merely sits at the clone
// destination but cannot be verified (no matching remote) gets a
// non-actionable note row; with no match at all (hop mode only) a
// clone+pull row is offered.
func (m *HopModel) prRows(pr clone.PR) (first []int, extra []hop.Candidate, notes map[int]string) {
	notes = map[int]string{}
	type rootInfo struct {
		root, label string
	}
	var roots []rootInfo
	seen := map[string]bool{}
	for i, c := range m.cands {
		if !c.HasRepoPath(pr.RepoPath) {
			continue
		}
		root, label, ok := c.EffectiveRoot()
		if !ok {
			continue
		}
		if c.Kind == hop.KindWorktree && c.Branch == pr.BranchName() {
			// The pr/N worktree itself is reachable only through the pull
			// row (provenance check); see the doc comment above.
			notes[i] = hiddenNote
			if !seen[root] {
				seen[root] = true
				roots = append(roots, rootInfo{root, label})
			}
			continue
		}
		first = append(first, i)
		if !seen[root] {
			seen[root] = true
			roots = append(roots, rootInfo{root, label})
		}
	}
	if len(roots) == 0 {
		// A checkout at the clone destination whose identity cannot be
		// verified (path is a lossy identity: last two elements, no port)
		// must be neither cloned over nor operated on.
		dest := m.destFor(pr.Target())
		for _, c := range m.cands {
			if dest != "" && c.Path == dest {
				return nil, []hop.Candidate{{
					Kind:  hop.KindNote,
					Path:  dest,
					Label: fmt.Sprintf("a checkout exists at %s but has no remote matching this pull request; set a remote to verify it", c.Label),
				}}, notes
			}
		}
		if m.worktreeMode {
			return nil, nil, notes
		}
		t := pr.Target()
		return nil, []hop.Candidate{{
			Kind: hop.KindClonePull, Path: dest, Label: pr.Label(), Branch: clone.Sanitize(t.SafeURL()), RepoRoot: dest,
		}}, notes
	}
	for _, r := range roots {
		row := hop.Candidate{
			Kind: hop.KindPull, Path: r.root, Label: pr.Label(), Branch: r.label, RepoRoot: r.root, RepoLabel: r.label,
		}
		for _, c := range m.cands {
			if c.Kind == hop.KindWorktree && c.RepoRoot == r.root && c.Branch == pr.BranchName() {
				row.Branch = r.label + " · existing worktree " + c.Label
				break
			}
		}
		extra = append(extra, row)
	}
	return first, extra, notes
}

// hiddenNote marks a candidate that must not be shown while a PR query is
// active (the pr/N worktree, which is offered through the pull row instead).
const hiddenNote = "\x00hidden"

func prNote(notes map[int]string, i int) string {
	if notes[i] == hiddenNote {
		return ""
	}
	if n, ok := notes[i]; ok {
		return " " + strings.TrimSpace(n)
	}
	return ""
}
