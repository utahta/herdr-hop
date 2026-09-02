package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/worktree"
)

const prURL = "https://github.com/acme/api/pull/12"

// prPicker is a loaded picker with one checkout of github.com/acme/api.
// Unless the test sets remoteState itself, the server advertises the PR head
// but no matching branch, so operations take the fallback (pr/N) route.
func prPicker(t *testing.T, fc *fakeClient, git *fakeCloner) (HopModel, string) {
	t.Helper()
	root := t.TempDir()
	if git.remotes == nil {
		git.remotes = map[string]string{}
	}
	git.remotes["github.com/acme/api"] = "git@github.com:Acme/Api.git"
	if git.remoteState.st.HeadSHA == "" && git.remoteState.err == nil {
		git.remoteState.st = gitx.RemoteState{HeadSHA: "prsha", Branches: map[string]string{"main": "mainsha"}, DefaultBranch: "main"}
	}
	m := loadedHop(t, root, fc, git, "github.com/acme/api")
	return m, m.cands[0].Path
}

func TestPRRowAppearsForMatchingCheckout(t *testing.T) {
	m, repo := prPicker(t, &fakeClient{}, &fakeCloner{})
	m = typeKeys(m, prURL)
	if m.curPR == nil || m.cloneRow != nil {
		t.Fatalf("PR must be recognised before clone input: pr=%v clone=%v", m.curPR, m.cloneRow)
	}
	// Row 0: the matching repo (not fuzzy-matched by the long URL); row 1: pull.
	if m.rowCount() != 2 {
		t.Fatalf("rows=%d view=%v", m.rowCount(), m.view)
	}
	c0, _ := m.rowAt(0)
	c1, _ := m.rowAt(1)
	if c0.Kind != hop.KindRepo || c1.Kind != hop.KindPull || c1.Path != repo || m.cursor != 1 {
		t.Errorf("rows: %+v / %+v cursor=%d", c0, c1, m.cursor)
	}
	if v := m.View(); !strings.Contains(v, "pull") || !strings.Contains(v, "github.com/acme/api #12") {
		t.Errorf("view:\n%s", v)
	}
}

func TestPRRowsPerDistinctRoot(t *testing.T) {
	root := t.TempDir()
	git := &fakeCloner{remotes: map[string]string{
		"a/api":  "https://github.com/acme/api",
		"b/fork": "https://github.com/me/api",
	}, fetchURLs: map[string]string{}}
	m := loadedHop(t, root, &fakeClient{}, git, "a/api", "b/fork")
	// The fork's upstream is the PR repository: found via RepoPaths.
	forkPath := m.cands[1].Path
	git.remoteNames = []string{"origin", "upstream"}
	git.fetchURLs[forkPath+"\x00upstream"] = "https://github.com/acme/api.git"
	// re-resolve with the upstream in place
	hop.ApplyRepoIDs(m.cands, hop.ResolveRepoIDs(context.Background(), git, m.cands))
	m = typeKeys(m, prURL)
	var pulls []hop.Candidate
	for i := 0; i < m.rowCount(); i++ {
		if c, _ := m.rowAt(i); c.Kind == hop.KindPull {
			pulls = append(pulls, c)
		}
	}
	if len(pulls) != 2 {
		t.Fatalf("expected a pull row per checkout, got %+v", pulls)
	}
	if !strings.Contains(m.View(), "(a/api)") || !strings.Contains(m.View(), "(b/fork)") {
		t.Errorf("rows must name their repository:\n%s", m.View())
	}
}

func TestPREnterFetchesAndCreatesWorktree(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}}
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if !m.pr.running() || cmd == nil {
		t.Fatal("PR operation should start")
	}
	m = run(t, m, cmd)
	if len(git.fetchRefs) != 1 || git.fetchRefs[0] != "origin +refs/pull/12/head:"+m.curPR.LocalRef() {
		t.Errorf("fetch: %v", git.fetchRefs)
	}
	want := "wtcreate " + repo + " pr/12 base=" + m.curPR.LocalRef()
	found := false
	for _, c := range fc.calls {
		if c == want {
			found = true
		}
	}
	if !found || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	if len(git.upstreams) != 0 {
		t.Errorf("no upstream must be set: %v", git.upstreams)
	}
	if strings.Contains(m.curPR.LocalRef(), "acme") {
		t.Errorf("ref must not embed URL text: %s", m.curPR.LocalRef())
	}
	if got := git.config["branch.pr/12.hop-pr"]; got != "github.com/acme/api#12" {
		t.Errorf("provenance must be recorded, got %q", got)
	}
}

func TestPRSameNumberOfAnotherRepositoryIsRefused(t *testing.T) {
	// pr/12 exists, created for the fork's PR #12; now the upstream's #12.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/pr/12\t\tdef\t"}, config: map[string]string{"branch.pr/12.hop-pr": "github.com/me/api#12"}}
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || len(git.fetchRefs) != 0 || !strings.Contains(m.errMsg, "different pull request") {
		t.Errorf("calls=%v fetch=%v err=%q", fc.calls, git.fetchRefs, m.errMsg)
	}
	// Same for an existing worktree of the fork's pr/12.
	fc, git = &fakeClient{}, &fakeCloner{config: map[string]string{"branch.pr/12.hop-pr": "github.com/me/api#12"}}
	m, repo := prPicker(t, fc, git)
	fc.worktrees = map[string][]herdr.Worktree{repo: {{Path: "/wt/pr12", Branch: "pr/12", IsLinkedWorktree: true}}}
	m = typeKeys(m, prURL)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) > 1 || !strings.Contains(m.errMsg, "different pull request") {
		t.Errorf("worktree: calls=%v err=%q", fc.calls, m.errMsg)
	}
	// A pr/12 of unknown provenance is not reused either.
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/pr/12\t\tdef\t"}}
	m, _ = prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || !strings.Contains(m.errMsg, "not created for this pull request") {
		t.Errorf("unknown provenance: calls=%v err=%q", fc.calls, m.errMsg)
	}
}

func TestPRCancelDuringCheckNeverOpens(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{config: map[string]string{"branch.pr/12.hop-pr": "github.com/acme/api#12"}}
	m, repo := prPicker(t, fc, git)
	fc.worktrees = map[string][]herdr.Worktree{repo: {{Path: "/wt/pr12", Branch: "pr/12", IsLinkedWorktree: true}}}
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	// Esc lands while the (non-cancellable) check is running...
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	// ...and its result arrives afterwards: it must not proceed to open.
	m = run(t, m, cmd)
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "wtopen") || strings.HasPrefix(c, "focus") || strings.HasPrefix(c, "wtcreate") {
			t.Errorf("cancelled operation must not touch herdr: %v", fc.calls)
		}
	}
	if !strings.Contains(m.View(), "canceled") || m.quit {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestPRFetchesFromURLWhenNoRemoteMatches(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}}
	m, _ := prPicker(t, fc, git)
	// The checkout's origin is a different repository (a rename); the PR's
	// repository matches via RepoPaths only through a second remote... here
	// none: force the direct-URL path by making origin's URL differ.
	git.remotes[m.cands[0].Path] = "https://github.com/acme/api"
	git.remoteNames = []string{"origin"}
	git.fetchURLs = map[string]string{}
	// origin fetch URL is the PR repo -> remote is used.
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin ") {
		t.Errorf("fetch source: %v", git.fetchRefs)
	}
	// Now a remote that does not match: fetch straight from the URL.
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}}
	m, _ = prPicker(t, fc, git)
	// RepoPaths still match (so the row exists) but FetchURL for origin
	// returns something else at fetch time.
	git.remotes[m.cands[0].Path] = "https://github.com/other/thing"
	m = typeKeys(m, prURL)
	m.cands[0].RepoPaths = []string{"github.com/acme/api"}
	m.refilter()
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "https://github.com/acme/api ") {
		t.Errorf("fetch source: %v", git.fetchRefs)
	}
}

func TestPRExistingBranchRefreshesRefAndChecksOut(t *testing.T) {
	// v2: the dedicated ref is refreshed even for an existing branch; the
	// branch itself is not moved, and the user gets the merge hint.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t", "refs/heads/pr/12\t\tdef\t"}, config: map[string]string{"branch.pr/12.hop-pr": "github.com/acme/api#12"}}
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("ref must be refreshed: %v", git.fetchRefs)
	}
	joined := strings.Join(fc.calls, ",")
	if !strings.Contains(joined, "wtcreate "+repo+" pr/12 base=") || strings.Contains(joined, "base=refs/hop") {
		t.Errorf("existing branch must be checked out, not recreated: %v", fc.calls)
	}
	if m.quit || !strings.Contains(m.errMsg, "git merge refs/hop/pr/") {
		t.Errorf("merge hint expected: quit=%v err=%q", m.quit, m.errMsg)
	}
	// Case-folded collision: PR/12 exists, pr/12 does not -> refuse.
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/PR/12\t\tdef\t"}, existing: map[string]bool{"pr/12": true}}
	m, _ = prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || len(git.fetchRefs) != 0 || !strings.Contains(m.errMsg, "letter case") {
		t.Errorf("collision: calls=%v fetch=%v err=%q", fc.calls, git.fetchRefs, m.errMsg)
	}
}

func TestPRExistingWorktreeIsOpenedNotCreated(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{config: map[string]string{"branch.pr/12.hop-pr": "github.com/acme/api#12"}}
	m, repo := prPicker(t, fc, git)
	// A linked worktree of pr/12 exists.
	wtPath := filepath.Join(t.TempDir(), "pr12")
	fc.worktrees = map[string][]herdr.Worktree{repo: {{Path: wtPath, Branch: "pr/12", IsLinkedWorktree: true}}}
	m.cands = append(m.cands, hop.Candidate{Kind: hop.KindWorktree, Path: wtPath, Label: "pr12", Branch: "pr/12", RepoRoot: repo, RepoLabel: m.cands[0].Label, RepoPaths: m.cands[0].RepoPaths})
	m.labels = append(m.labels, "pr12")
	m = typeKeys(m, prURL)
	// The pull row is still the row to choose (it notes the existing
	// worktree); Enter goes through the PR operation, which verifies the
	// provenance, refreshes the dedicated ref, and then opens the worktree.
	c, _ := m.selected()
	if c.Kind != hop.KindPull || !strings.Contains(c.Branch, "existing worktree") {
		t.Fatalf("selected=%+v", c)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 {
		t.Errorf("ref must be refreshed: %v", git.fetchRefs)
	}
	if len(fc.calls) == 0 || !strings.HasPrefix(fc.calls[0], "wtopen ") {
		t.Errorf("calls=%v", fc.calls)
	}
	if m.quit || !strings.Contains(m.errMsg, "git merge") {
		t.Errorf("stays open with the merge hint: quit=%v err=%q", m.quit, m.errMsg)
	}
}

func TestPRExistingWorktreeOfOtherRepositoryNotOpened(t *testing.T) {
	// The fork's pr/12 worktree is in the candidate list; the upstream's #12
	// must not open it, even via the picker rows.
	fc := &fakeClient{}
	git := &fakeCloner{config: map[string]string{"branch.pr/12.hop-pr": "github.com/me/api#12"}}
	m, repo := prPicker(t, fc, git)
	wtPath := filepath.Join(t.TempDir(), "pr12")
	fc.worktrees = map[string][]herdr.Worktree{repo: {{Path: wtPath, Branch: "pr/12", IsLinkedWorktree: true}}}
	m.cands = append(m.cands, hop.Candidate{Kind: hop.KindWorktree, Path: wtPath, Label: "pr12", Branch: "pr/12", RepoRoot: repo, RepoLabel: m.cands[0].Label, RepoPaths: m.cands[0].RepoPaths})
	m.labels = append(m.labels, "pr12")
	m = typeKeys(m, prURL)
	// Every row reachable for this query either is the pull row or a plain
	// candidate that is not the pr/12 worktree.
	for i := 0; i < m.rowCount(); i++ {
		if c, _ := m.rowAt(i); c.Kind == hop.KindWorktree && c.Branch == "pr/12" {
			t.Fatalf("pr/12 worktree must not be offered as a plain row: %+v", c)
		}
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "wtopen") || strings.HasPrefix(c, "focus") || strings.HasPrefix(c, "wtcreate") {
			t.Errorf("must not open the fork's worktree: %v", fc.calls)
		}
	}
	if !strings.Contains(m.errMsg, "different pull request") {
		t.Errorf("err=%q", m.errMsg)
	}
}

func TestPRRefusesMainCheckoutAndPrunable(t *testing.T) {
	for _, tc := range []struct {
		name string
		wt   herdr.Worktree
		want string
	}{
		{"main checkout", herdr.Worktree{Path: "/main", Branch: "pr/12", IsLinkedWorktree: false}, "main checkout"},
		{"prunable", herdr.Worktree{Path: "/gone", Branch: "pr/12", IsLinkedWorktree: true, IsPrunable: true}, "git worktree prune"},
	} {
		fc := &fakeClient{}
		git := &fakeCloner{refs: []string{"refs/heads/pr/12\t\tabc\t/main"}}
		m, repo := prPicker(t, fc, git)
		fc.worktrees = map[string][]herdr.Worktree{repo: {tc.wt}}
		m = typeKeys(m, prURL)
		mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = run(t, mm.(HopModel), cmd)
		if !strings.Contains(m.errMsg, tc.want) || len(git.fetchRefs) != 0 {
			t.Errorf("%s: err=%q fetch=%v", tc.name, m.errMsg, git.fetchRefs)
		}
		for _, c := range fc.calls {
			if strings.HasPrefix(c, "wtcreate") || strings.HasPrefix(c, "wtopen") {
				t.Errorf("%s: must not create/open: %v", tc.name, fc.calls)
			}
		}
	}
}

func TestPREscCancelsFetchButNotHerdrPhase(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}, fetchBlock: make(chan struct{})}
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	// Deliver stage messages until the fetch phase.
	for i := 0; i < 5 && m.pr.phase != prPhaseFetch; i++ {
		msg := cmd()
		mm, cmd = m.Update(msg)
		m = mm.(HopModel)
	}
	if m.pr.phase != prPhaseFetch || !strings.Contains(m.View(), "esc to cancel") {
		t.Fatalf("phase=%v view:\n%s", m.pr.phase, m.View())
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if !m.pr.canceled {
		t.Fatal("esc must cancel during fetch")
	}
	m = run(t, m, cmd) // fetch returns ctx error -> done
	if m.quit || !strings.Contains(m.View(), "canceled") || m.pr != nil {
		t.Errorf("after cancel: quit=%v pr=%v view:\n%s", m.quit, m.pr, m.View())
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "wtcreate") {
			t.Errorf("worktree must not be created after cancel: %v", fc.calls)
		}
	}
	// herdr phase: Esc and Ctrl-C are ignored and the operation completes.
	fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}}
	m, _ = prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	for i := 0; i < 8 && m.pr.phase != prPhaseHerdr; i++ {
		msg := cmd()
		mm, cmd = m.Update(msg)
		m = mm.(HopModel)
	}
	if m.pr.phase != prPhaseHerdr {
		t.Fatalf("phase=%v", m.pr.phase)
	}
	for _, k := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		mm, c := m.Update(tea.KeyMsg{Type: k})
		m = mm.(HopModel)
		if c != nil || m.pr.canceled {
			t.Errorf("key %v must be ignored during the herdr phase", k)
		}
	}
	m = run(t, m, cmd)
	if !m.quit {
		t.Error("operation must complete and quit")
	}
}

func TestClonePullClonesThenContinues(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}}
	m := loadedHop(t, root, fc, git, "github.com/other/x")
	m = typeKeys(m, prURL)
	c, _ := m.selected()
	if c.Kind != hop.KindClonePull {
		t.Fatalf("expected clone+pull row, got %+v", c)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	dest := filepath.Join(root, "github.com", "acme", "api")
	if git.url != "https://github.com/acme/api" || git.dest != dest {
		t.Errorf("clone: %q -> %q", git.url, git.dest)
	}
	// The fresh clone has no remotes registered in the fake, so the PR is
	// fetched straight from the repository URL (fallback route).
	// After the clone the checkout is a candidate and the PR continued in it.
	found := false
	for _, cand := range m.cands {
		if cand.Path == dest && cand.HasRepoPath("github.com/acme/api") {
			found = true
		}
	}
	if !found {
		t.Error("cloned repository must be added to the candidates")
	}
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "https://github.com/acme/api +refs/pull/12/head:") {
		t.Errorf("fetch=%v", git.fetchRefs)
	}
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+dest+" pr/12") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	// No workspace create for the bare clone (the PR worktree is what opens).
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "create ") {
			t.Errorf("clone+pull must not open a plain workspace: %v", fc.calls)
		}
	}
}

func TestClonePullRetryOffersPullRow(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}, fetchRefErr: errors.New("fatal: could not read")}
	m := loadedHop(t, root, fc, git, "github.com/other/x")
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if m.quit || m.errMsg == "" {
		t.Fatalf("PR step should have failed: quit=%v err=%q", m.quit, m.errMsg)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // dismiss
	m = mm.(HopModel)
	m.input.SetValue("")
	m = typeKeys(m, prURL)
	c, _ := m.selected()
	if c.Kind != hop.KindPull {
		t.Errorf("retry must offer a pull row for the cloned checkout, got %+v", c)
	}
}

func TestPRWorktreeModeNoClonePull(t *testing.T) {
	root := t.TempDir()
	m := loadedHop(t, root, &fakeClient{}, &fakeCloner{}, "github.com/other/x")
	m.worktreeMode = true
	m = typeKeys(m, prURL)
	if m.rowCount() != 0 {
		t.Errorf("worktree mode must not offer clone+pull: %v", m.view)
	}
	// but a matching checkout still gets a pull row
	m, _ = prPicker(t, &fakeClient{}, &fakeCloner{})
	m.worktreeMode = true
	m = typeKeys(m, prURL)
	if c, _ := m.selected(); c.Kind != hop.KindPull {
		t.Errorf("worktree mode: expected pull row, got %+v", c)
	}
}

func TestPRErrorOutputIsMasked(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tabc\t"}, fetchRefErr: errors.New("fatal: unable to access 'https://u:tok@github.com/acme/api': 403")}
	root := t.TempDir()
	git.remotes = map[string]string{"github.com/acme/api": "https://github.com/acme/api"}
	m := loadedHop(t, root, fc, git, "github.com/acme/api")
	m = typeKeys(m, "https://u:tok@github.com/acme/api/pull/12")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if strings.Contains(viewWithoutInput(m), "tok") {
		t.Errorf("credentials leaked:\n%s", m.View())
	}
}

// ---- branch screen annotations

func TestBranchScreenAnnotatesPRs(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{
		refs:    []string{"refs/heads/main\t\tm\t", "refs/heads/feat\t\tf\t", "refs/remotes/origin/feat\t\tf\t"},
		prRefs:  map[string][]gitx.PRRef{"origin": {{Remote: "origin", Number: 12, SHA: "f"}}},
		remotes: map[string]string{},
	}
	root := t.TempDir()
	git.remotes[filepath.Join(root, "github.com/o/r")] = "https://github.com/o/r"
	m := loadedHop(t, root, fc, git, "github.com/o/r")
	git.remotes[m.cands[0].Path] = "https://github.com/o/r"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = run(t, mm.(HopModel), cmd)
	if len(git.lsCalls) != 1 || git.lsCalls[0] != "origin" {
		t.Fatalf("ls-remote calls: %v", git.lsCalls)
	}
	names := strings.Join(m.wt.names, ",")
	if names != "main,feat #12,origin/feat #12" {
		t.Errorf("order must be unchanged, only annotated: %s", names)
	}
	if !strings.Contains(m.View(), "#12") {
		t.Errorf("view:\n%s", m.View())
	}
	// "#12" filters to the PR branches.
	m.wt = typeBranch(m.wt, "#12")
	if len(m.wt.view) != 2 {
		t.Errorf("filter by PR number: %v", m.wt.view)
	}
}

func TestBranchScreenPRHeadsBeforeRefs(t *testing.T) {
	// PR heads arriving before refs must not be lost when refs come in.
	git := &fakeCloner{refs: []string{"refs/heads/feat\t\tf\t"}, remotes: map[string]string{}}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	git.remotes[m.cands[0].Path] = "https://github.com/o/r"
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{{remote: "origin", heads: []worktreeHead{{Remote: "origin", Number: 3, SHA: "f"}}}}})
	m = mm.(HopModel)
	mm, _ = m.Update(refsLoadedMsg{op: m.wt.op, branches: parseRefsForTest(git.refs)})
	m = mm.(HopModel)
	if len(m.wt.branches) != 1 || !m.wt.branches[0].HasPR() {
		t.Errorf("annotation lost: %+v", m.wt.branches)
	}
	// A stale generation is ignored.
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen - 1, remotes: []string{"origin"}, results: []remotePRResult{{remote: "origin"}}})
	m = mm.(HopModel)
	if !m.wt.branches[0].HasPR() {
		t.Error("stale generation must not clear annotations")
	}
}

func TestBranchScreenEscCancelsLsRemote(t *testing.T) {
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, remotes: map[string]string{}, lsBlock: make(chan struct{})}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	git.remotes[m.cands[0].Path] = "https://github.com/o/r"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	// Execute the batch's commands in the background without touching the
	// model: the ls-remote command blocks until its context is cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if batch, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					c()
				}
			}
		}
	}()
	// Refs arrive (the screen is no longer busy), then Esc leaves it.
	mm, _ = m.Update(refsLoadedMsg{op: m.wt.op, branches: parseRefsForTest(git.refs)})
	m = mm.(HopModel)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(HopModel)
	if m.wt != nil {
		t.Fatal("expected to leave the branch screen")
	}
	select {
	case <-done: // returned because Esc cancelled the context
	case <-time.After(5 * time.Second):
		t.Fatal("ls-remote was not cancelled when leaving the branch screen")
	}
}

type worktreeHead = worktree.PRHead

func parseRefsForTest(lines []string) []worktree.Branch { return worktree.ParseRefs(lines, nil) }

// ---- v2: route decision and the remote-branch路

func remoteStateWith(branches map[string]string, def string) struct {
	st  gitx.RemoteState
	err error
} {
	return struct {
		st  gitx.RemoteState
		err error
	}{st: gitx.RemoteState{HeadSHA: "prsha", Branches: branches, DefaultBranch: def}}
}

func TestPRUniqueBranchMatchDelegatesToTrackedFlow(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
	git.remoteState = remoteStateWith(map[string]string{"main": "m2", "feature-x": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.lsPRCalls) != 1 || git.lsPRCalls[0] != "origin refs/pull/12/head" {
		t.Fatalf("ls-remote: %v", git.lsPRCalls)
	}
	if len(git.fetchRefs) != 1 || git.fetchRefs[0] != "origin +refs/heads/feature-x:refs/remotes/origin/feature-x" {
		t.Errorf("fetch: %v", git.fetchRefs)
	}
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" feature-x base=refs/remotes/origin/feature-x") {
		t.Errorf("calls: %v", fc.calls)
	}
	if len(git.upstreams) != 1 || git.upstreams[0] != "feature-x->refs/remotes/origin/feature-x" || !m.quit {
		t.Errorf("upstreams=%v quit=%v", git.upstreams, m.quit)
	}
	if got := git.config["branch.pr/12.hop-pr"]; got != "" {
		t.Errorf("tracked route must not record provenance: %q", got)
	}
}

func TestPRAmbiguousMatchExpandsChoices(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
	git.remoteState = remoteStateWith(map[string]string{"feature": "prsha", "release-candidate": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || len(git.fetchRefs) != 0 {
		t.Fatalf("nothing may run on an ambiguous match: %v %v", fc.calls, git.fetchRefs)
	}
	if m.rowCount() != 2 {
		t.Fatalf("expected one row per candidate branch: %v", m.view)
	}
	var names []string
	for i := 0; i < m.rowCount(); i++ {
		c, _ := m.rowAt(i)
		if c.Kind != hop.KindPull || c.PRBranch == "" {
			t.Fatalf("row %d: %+v", i, c)
		}
		names = append(names, c.PRBranch)
	}
	// Choosing one runs the tracked flow for that very branch.
	m.cursor = 1
	chosen := names[1]
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" "+chosen+" base=refs/remotes/origin/"+chosen) || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	if len(git.lsPRCalls) != 1 {
		t.Errorf("the chosen branch must not be re-matched: %v", git.lsPRCalls)
	}
}

func TestPRDefaultBranchOnlyMatchFallsBack(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
	git.remoteState = remoteStateWith(map[string]string{"main": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("must fall back to the dedicated ref: %v", git.fetchRefs)
	}
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" pr/12 base=refs/hop/pr/") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
}

func TestPRTrackedLocalBranchIsCheckedOut(t *testing.T) {
	// feature-x exists and tracks origin/feature-x: reuse it (no fetch of a
	// base, no upstream rewrite), herdr checks it out.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/feature-x\trefs/remotes/origin/feature-x\tf\t"}}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 0 || len(git.upstreams) != 0 {
		t.Errorf("fetch=%v upstreams=%v", git.fetchRefs, git.upstreams)
	}
	joined := strings.Join(fc.calls, ",")
	if !strings.Contains(joined, "wtcreate "+repo+" feature-x base=") || strings.Contains(joined, "base=refs/") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	// Same name but tracking something else (or nothing): refuse.
	for _, up := range []string{"refs/remotes/origin/other", ""} {
		fc, git = &fakeClient{}, &fakeCloner{refs: []string{"refs/heads/feature-x\t" + up + "\tf\t"}}
		git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
		m, _ = prPicker(t, fc, git)
		m = typeKeys(m, prURL)
		mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = run(t, mm.(HopModel), cmd)
		if len(fc.calls) != 0 || !strings.Contains(m.errMsg, "does not track") {
			t.Errorf("up=%q: calls=%v err=%q", up, fc.calls, m.errMsg)
		}
	}
}

func TestPRReidentifiesWorktreeByNameAndUpstream(t *testing.T) {
	// feature-x tracks origin/feature-x and is checked out in a linked
	// worktree: just open it. review-feature tracks the same remote but has
	// a different name and must not be chosen.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{
		"refs/heads/feature-x\trefs/remotes/origin/feature-x\tf\t/wt/fx",
		"refs/heads/review-feature\trefs/remotes/origin/feature-x\tr\t/wt/review",
	}}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	fc.worktrees = map[string][]herdr.Worktree{repo: {
		{Path: "/wt/fx", Branch: "feature-x", IsLinkedWorktree: true},
		{Path: "/wt/review", Branch: "review-feature", IsLinkedWorktree: true},
	}}
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 0 {
		t.Errorf("open-only: %v", git.fetchRefs)
	}
	joined := strings.Join(fc.calls, ",")
	if !strings.Contains(joined, "wtopen /wt/fx") || strings.Contains(joined, "/wt/review") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
}

func TestPRNoteRowForUnverifiableCheckout(t *testing.T) {
	// A checkout sits at the clone destination but has no remote matching
	// the PR: neither clone+pull nor pull; a guidance note instead.
	root := t.TempDir()
	git := &fakeCloner{remotes: map[string]string{"github.com/acme/api": ""}} // checkout with no remote
	m := loadedHop(t, root, &fakeClient{}, git, "github.com/acme/api")
	m = typeKeys(m, prURL)
	if m.rowCount() != 1 {
		t.Fatalf("rows=%v", m.view)
	}
	c, _ := m.selected()
	if c.Kind != hop.KindNote || !strings.Contains(c.Label, "set a remote") {
		t.Fatalf("row: %+v", c)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	if cmd != nil || git.url != "" || !strings.Contains(m.errMsg, "set a remote") {
		t.Errorf("note row must only explain: cmd=%v cloned=%q err=%q", cmd != nil, git.url, m.errMsg)
	}
}

func TestPRLsRemoteFailureIsAnError(t *testing.T) {
	fc := &fakeClient{}
	git := &fakeCloner{}
	git.remoteState.err = errors.New("fatal: could not read from remote")
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || len(git.fetchRefs) != 0 || m.errMsg == "" {
		t.Errorf("calls=%v fetch=%v err=%q", fc.calls, git.fetchRefs, m.errMsg)
	}
}

func TestPRInvalidSourceBranchNameIsRefused(t *testing.T) {
	for _, bad := range []string{"HEAD", "-release"} {
		fc := &fakeClient{}
		git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
		git.remoteState = remoteStateWith(map[string]string{bad: "prsha", "x": "other"}, "main")
		m, _ := prPicker(t, fc, git)
		m = typeKeys(m, prURL)
		mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = run(t, mm.(HopModel), cmd)
		if len(git.fetchRefs) != 0 || len(fc.calls) != 0 || !strings.Contains(m.errMsg, "local branch name") {
			t.Errorf("%q: fetch=%v calls=%v err=%q", bad, git.fetchRefs, fc.calls, m.errMsg)
		}
	}
}

func TestPRRechecksBranchBeforeCreating(t *testing.T) {
	// A branch of the target name appears while the fetch is running: the
	// create must be refused instead of silently checking it out.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, fetchBlock: make(chan struct{})}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(HopModel)
	// checked -> fetch phase
	for i := 0; i < 4 && m.pr.phase != prPhaseFetch; i++ {
		msg := cmd()
		mm, cmd = m.Update(msg)
		m = mm.(HopModel)
	}
	if m.pr.phase != prPhaseFetch {
		t.Fatalf("phase=%v", m.pr.phase)
	}
	// The racing process creates feature-x, then the fetch completes.
	git.mu.Lock()
	git.existing = map[string]bool{"feature-x": true}
	git.mu.Unlock()
	close(git.fetchBlock)
	m = run(t, m, cmd)
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "wtcreate") {
			t.Errorf("must not create over the raced branch: %v", fc.calls)
		}
	}
	if len(git.upstreams) != 0 || !strings.Contains(m.errMsg, "created while fetching") {
		t.Errorf("upstreams=%v err=%q", git.upstreams, m.errMsg)
	}
}

func TestPRRelabelFailureReportsPartialSuccess(t *testing.T) {
	p := "w9"
	fc := &fakeClient{parent: &p, labels: map[string]string{"w9": "api"}, renameErr: errors.New("socket down")}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if m.quit || !strings.Contains(m.errMsg, "worktree created, but renaming the parent workspace failed") {
		t.Errorf("quit=%v err=%q", m.quit, m.errMsg)
	}
}

func TestPRRestrictedFetchRefspecFallsBack(t *testing.T) {
	// A single-branch clone: origin only maps main. Even though the source
	// branch is identified, the tracked route cannot work (no upstream), so
	// the fallback route is taken before anything is created.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, refspecs: []string{"+refs/heads/main:refs/remotes/origin/main"}}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("must use the dedicated ref: %v", git.fetchRefs)
	}
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" pr/12 base=refs/hop/pr/") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	if len(git.upstreams) != 0 {
		t.Errorf("no upstream may be attempted: %v", git.upstreams)
	}
}

func TestPRUnknownDefaultBranchFallsBack(t *testing.T) {
	// The remote sent no HEAD symref: a unique SHA match cannot be checked
	// against the default branch, so the safe fallback route is taken.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}}
	git.remoteState = remoteStateWith(map[string]string{"main": "prsha"}, "")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("must fall back: %v", git.fetchRefs)
	}
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" pr/12 base=refs/hop/pr/") || !m.quit {
		t.Errorf("calls=%v quit=%v", fc.calls, m.quit)
	}
	if len(git.upstreams) != 0 {
		t.Errorf("no tracking may be set up: %v", git.upstreams)
	}
}

func TestPRCoverageDoesNotResolveAmbiguity(t *testing.T) {
	// Two branches share the PR head SHA; only one is trackable. Coverage
	// must not turn that into a confident unique match — the user chooses.
	fc := &fakeClient{}
	git := &fakeCloner{refs: []string{"refs/heads/main\t\tm\t"}, refspecs: []string{"+refs/heads/release:refs/remotes/origin/release"}}
	git.remoteState = remoteStateWith(map[string]string{"feature": "prsha", "release": "prsha"}, "main")
	m, _ := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(fc.calls) != 0 || len(git.fetchRefs) != 0 {
		t.Fatalf("nothing may run: %v %v", fc.calls, git.fetchRefs)
	}
	if m.rowCount() != 2 {
		t.Fatalf("expected both branches as choices: %v", m.view)
	}
	// Picking the untrackable one falls back to the dedicated ref.
	c0, _ := m.rowAt(0)
	if c0.PRBranch != "feature" { // sorted order
		t.Fatalf("row order: %+v", c0)
	}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("untrackable choice must fall back: %v", git.fetchRefs)
	}
}

func TestPRAnnotationsUpdatePerRemote(t *testing.T) {
	git := &fakeCloner{refs: []string{"refs/heads/a\t\taa\t", "refs/heads/b\t\tbb\t"}, remotes: map[string]string{}}
	m := loadedHop(t, t.TempDir(), &fakeClient{}, git, "github.com/o/r")
	git.remotes[m.cands[0].Path] = "https://github.com/o/r"
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = mm.(HopModel)
	mm, _ = m.Update(refsLoadedMsg{op: m.wt.op, branches: parseRefsForTest(git.refs)})
	m = mm.(HopModel)
	// Round 1: origin says a=#1, upstream says b=#2.
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin", "upstream"}, results: []remotePRResult{
		{remote: "origin", heads: []worktree.PRHead{{Remote: "origin", Number: 1, SHA: "aa"}}},
		{remote: "upstream", heads: []worktree.PRHead{{Remote: "upstream", Number: 2, SHA: "bb"}}},
	}})
	m = mm.(HopModel)
	names := strings.Join(m.wt.names, ",")
	if !strings.Contains(names, "a origin#1") || !strings.Contains(names, "b upstream#2") {
		t.Fatalf("round 1: %v", m.wt.names)
	}
	// Round 2: origin now has no PRs (closed), upstream errors out.
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin", "upstream"}, results: []remotePRResult{
		{remote: "origin", heads: []worktree.PRHead{}},
		{remote: "upstream", err: errors.New("unreachable")},
	}})
	m = mm.(HopModel)
	names = strings.Join(m.wt.names, ",")
	if strings.Contains(names, "#1") {
		t.Errorf("origin's closed PR must disappear: %v", m.wt.names)
	}
	// With PRs left on a single remote, labels lose the remote prefix.
	if !strings.Contains(names, "b #2") {
		t.Errorf("failing upstream keeps its annotations: %v", m.wt.names)
	}
	// The remote list failing keeps everything.
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, listErr: errors.New("boom")})
	m = mm.(HopModel)
	if !strings.Contains(strings.Join(m.wt.names, ","), "b #2") {
		t.Errorf("list failure must keep annotations: %v", m.wt.names)
	}
	// Round 3: upstream was deleted (not in this round's remote set): its
	// annotations disappear even though no result row names it.
	mm, _ = m.Update(prHeadsMsg{op: m.wt.op, gen: m.wt.prGen, remotes: []string{"origin"}, results: []remotePRResult{
		{remote: "origin", heads: []worktree.PRHead{}},
	}})
	m = mm.(HopModel)
	if strings.Contains(strings.Join(m.wt.names, ","), "#2") {
		t.Errorf("annotations of a removed remote must be dropped: %v", m.wt.names)
	}
}

func TestPRExistingFallbackArtifactPinsRoute(t *testing.T) {
	// pr/12 was created by the fallback route earlier; now the source
	// branch is trackable. The operation must stay on the fallback route
	// (refresh + check out the existing branch), not create feature-x too.
	fc := &fakeClient{}
	git := &fakeCloner{
		refs:   []string{"refs/heads/pr/12\t\td\t", "refs/heads/main\t\tm\t"},
		config: map[string]string{"branch.pr/12.hop-pr": "github.com/acme/api#12"},
	}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, repo := prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if len(git.fetchRefs) != 1 || !strings.HasPrefix(git.fetchRefs[0], "origin +refs/pull/12/head:refs/hop/pr/") {
		t.Errorf("fetch: %v", git.fetchRefs)
	}
	joined := strings.Join(fc.calls, ",")
	if !strings.Contains(joined, "wtcreate "+repo+" pr/12 base=") || strings.Contains(joined, "feature-x") {
		t.Errorf("must reuse pr/12, never create feature-x: %v", fc.calls)
	}
	if len(git.upstreams) != 0 || m.quit || !strings.Contains(m.errMsg, "git merge") {
		t.Errorf("upstreams=%v quit=%v err=%q", git.upstreams, m.quit, m.errMsg)
	}
	// A pr/12 of some other PR does not pin: the tracked route proceeds.
	fc, git = &fakeClient{}, &fakeCloner{
		refs:   []string{"refs/heads/pr/12\t\td\t", "refs/heads/main\t\tm\t"},
		config: map[string]string{"branch.pr/12.hop-pr": "github.com/me/api#12"},
	}
	git.remoteState = remoteStateWith(map[string]string{"feature-x": "prsha"}, "main")
	m, repo = prPicker(t, fc, git)
	m = typeKeys(m, prURL)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(t, mm.(HopModel), cmd)
	if !strings.Contains(strings.Join(fc.calls, ","), "wtcreate "+repo+" feature-x base=refs/remotes/origin/feature-x") || !m.quit {
		t.Errorf("foreign pr/12 must not pin the route: calls=%v quit=%v", fc.calls, m.quit)
	}
}
