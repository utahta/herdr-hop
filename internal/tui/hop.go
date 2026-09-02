// Package tui implements the popup UI: the hop picker with an integrated
// clone row, and the branch screen used to create a worktree.
package tui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/config"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/hop"
	"github.com/utahta/herdr-hop/internal/scan"
)

var (
	styleDim = lipgloss.NewStyle().Faint(true)
	// styleCursor is the "> " pointer: bold red, matching intellij-wt.
	styleCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	// cursorRowBg is the faint backdrop under the cursor row: unlike a
	// reversed bar it keeps every foreground color and just lifts the whole
	// row. A fixed dark gray — tuned for dark terminal themes.
	cursorRowBg = lipgloss.Color("236")
	// styleMatch highlights the runes the query matched, fzf-style.
	styleMatch = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	// styleChip renders key names in the help line as badge-like chips:
	// background color reads as shape and stays legible on any theme.
	styleChip = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("253"))
	// styleRule is the separator rule under the input: a medium gray — dim
	// sinks into dark themes.
	styleRule = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	// styleCount is the match counter next to the rule.
	styleCount = lipgloss.NewStyle().Foreground(lipgloss.Color("178"))
	// Prompt tones: muted blue for the hop picker, muted green for the
	// worktree flow, so the first line says which screen this is.
	stylePromptHop      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("110"))
	stylePromptWorktree = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("108"))
	// styleBranch tints worktree rows (branch name and tree marker) a muted
	// violet, so the tree structure reads by hue: repositories stay in the
	// default color, their worktrees hang below in this one. Hue only —
	// weight is reserved for the cursor row.
	styleBranch = lipgloss.NewStyle().Foreground(lipgloss.Color("139"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleOpen   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleKind   = map[hop.Kind]lipgloss.Style{
		hop.KindRepo:      lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		hop.KindWorktree:  lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		hop.KindWorkspace: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		hop.KindUnknown:   lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		hop.KindClone:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		hop.KindPull:      lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		hop.KindClonePull: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		hop.KindNote:      lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	}
)

type loadedMsg struct {
	cands []hop.Candidate
	warn  error
	// gen is the load generation the candidates belong to (see loadGen).
	gen int
	// occupancy is where herdr's panes are, from the load's snapshot; its
	// OK is false when the snapshot could not be read (see hop.Loaded).
	occupancy hop.Occupancy
}

// resolvedMsg carries the background remote-identity resolution of one load
// generation. Stale generations are discarded.
type resolvedMsg struct {
	gen int
	ids map[string]hop.RepoIdentity
}

// wtStateMsg carries the authoritative worktree open states of one load
// generation (`herdr worktree list`, run in the background because the herdr
// server handles the calls one at a time). Stale generations are discarded.
type wtStateMsg struct {
	gen    int
	states hop.WorktreeStateResult
}
type doneMsg struct{ err error }

// removedMsg reports a worktree deletion; repoRoot is where the cursor
// should land after the reload that follows a success.
type removedMsg struct {
	repoRoot string
	err      error
}

// Clone messages carry the operation id they belong to so that a message
// from an earlier clone (finished after the user moved on) is ignored.
type cloneProgressMsg struct {
	op   int
	line string
}
type cloneDoneMsg struct {
	op   int
	dest string
	err  error
}

// cloneState tracks an in-flight git clone started from the clone row.
type cloneState struct {
	op       int // operation id; incremented per clone
	target   clone.Target
	dest     string
	running  bool
	canceled bool // Esc was pressed: never open a workspace for this op
	cancel   context.CancelFunc
	progress []string
	events   chan tea.Msg
	// thenPR, when set, continues with this pull request in the cloned
	// repository instead of opening a workspace (the clone+pull row).
	thenPR *clone.PR
}

// HopModel is the picker's model (tea.Model).
type HopModel struct {
	cfg config.Config
	h   herdr.Client
	git GitOps
	log *log.Logger
	// forge answers PR titles and states for the branch screen; nil when
	// no forge client is available (gh not installed).
	forge PRInfoSource
	// worktreeMode: started with --mode worktree, so Enter on a repository
	// goes to the branch screen instead of opening a workspace.
	worktreeMode bool
	// wt is the active branch screen, nil while the picker is shown.
	wt     *branchState
	wtOp   int
	input  textinput.Model
	cands  []hop.Candidate
	labels []string
	// view holds row indexes: < len(cands) index cands, >= len(cands) index
	// extra (synthetic rows: clone, pull, clone+pull) at idx-len(cands).
	view  []int
	extra []hop.Candidate
	// notes are display suffixes for candidate rows (e.g. "#123" on the
	// existing worktree of a pull request).
	notes map[int]string
	// matches holds the matched rune positions of filtered rows, keyed by
	// candidate index, so the view can highlight why a row matched.
	matches map[int]rowMatch
	// cloneRow is the synthetic clone candidate for the current query, or nil.
	cloneRow *hop.Candidate
	// collapsed holds normalized repo roots whose worktree group is folded in
	// the tree view. Keyed by path (not candidate index) so the fold state
	// survives reloads and re-sorts, and is restored when the query is cleared.
	collapsed map[string]bool
	// tree is the grouped (empty-query) view's metadata; inactive while the
	// flat fuzzy-filtered list is shown.
	tree treeState
	// curPR is the pull request the current query denotes, or nil.
	curPR  *clone.PR
	cursor int
	// top is the first visible list row: the cursor walks within the window
	// [top, top+listRows()) and the window slides only when the cursor
	// crosses one of its edges.
	top    int
	width  int
	height int

	// loadGen counts load generations, starting at 1 for the initial load
	// (never 0: resolvedGen's zero value must mean "nothing resolved yet").
	// resolvedGen is the generation whose remote identities have been
	// applied; identities are resolved when resolvedGen == loadGen.
	loadGen     int
	resolvedGen int
	// resolveStarted is the generation a resolver has been started for (0:
	// none). Identities are resolved lazily — only once a query names a
	// repository or PR — since nothing else reads them; most picker uses
	// (a name, a branch) never pay for the git processes at all.
	resolveStarted int
	// resolveCancel stops the running resolver's git processes; the
	// generation guard alone would only discard its result.
	resolveCancel context.CancelFunc
	// wtStateGen / wtStateCancel are the same pair for the background
	// worktree-state pass (wtStateMsg). Worktree open states are
	// authoritative when wtStateGen == loadGen; before that they are the
	// snapshot-derived provisional guess.
	wtStateGen    int
	wtStateCancel context.CancelFunc
	// wtStateStarted is the generation the worktree-state pass has been
	// started for (0: none). The pass runs right after a load — except when
	// the load jumps straight to the branch screen (prefix+t), which never
	// reads the states; it then waits until esc brings the picker back.
	wtStateStarted int
	// queuedWorktree is the worktree a user hit Enter on before the
	// authoritative state arrived: the open runs on wtStateMsg ("" = none).
	queuedWorktree string

	loading bool
	pending bool // an open/create command is in flight; ignore Enter/Ctrl-N
	// confirm is set while ctrl-d awaits a yes/no on deleting a worktree:
	// the input shows the question instead of the query, which is kept
	// for when the answer is no.
	confirm *removeConfirm
	// focusPath, when set, names the row the next load should put the
	// cursor on (the repository whose worktree was just deleted).
	focusPath string
	// occupancy mirrors the current load's loadedMsg.occupancy.
	occupancy hop.Occupancy
	clone     cloneState
	// pr is the in-flight pull-request operation, nil when none.
	pr     *prState
	prOp   int
	warn   string // non-fatal (partial herdr failure)
	errMsg string // fatal for the current action; dismiss with any key
	quit   bool
}

// NewHop builds the model. worktreeMode makes Enter on a repository open
// the branch screen (create a worktree) instead of a workspace.
func NewHop(cfg config.Config, h herdr.Client, git GitOps, lg *log.Logger, worktreeMode bool) HopModel {
	ti := textinput.New()
	// The prompt doubles as the title (one line, like fzf). The word names
	// what the field takes, the tone names the flow: worktree mode picks a
	// repository first (green, like the branch screen that follows), the
	// hop picker stays blue.
	ti.Prompt = "hop> "
	ti.PromptStyle = stylePromptHop
	if worktreeMode {
		ti.Prompt = "repo> "
		ti.PromptStyle = stylePromptWorktree
	}
	ti.Focus()
	return HopModel{cfg: cfg, h: h, git: git, log: lg, input: ti, loading: true, worktreeMode: worktreeMode, collapsed: map[string]bool{}, loadGen: 1}
}

// WithForge equips the model with a source of pull request details for the
// branch screen. Without one, branches still carry their PR numbers (those
// come from git), just no titles or states.
func (m HopModel) WithForge(f PRInfoSource) HopModel {
	m.forge = f
	return m
}

// removeConfirm is the pending "delete this worktree?" question.
type removeConfirm struct {
	cand  hop.Candidate
	name  string // what the question names (branch, else label), sanitized
	query string // the filter to restore when the answer is no
}

// confirmPrompt renders the question for the current width, so that a
// resize during the confirmation cannot wrap the input line listRows
// counts as one. The name gives way first, then the word "worktree", then
// the name altogether; whatever remains is clipped to the width.
func (m HopModel) confirmPrompt() string {
	// One cell stays free for the input's cursor, drawn after the prompt.
	name := m.confirm.name
	full := "delete worktree " + name + "? (y/N) "
	if m.width <= 0 || lipgloss.Width(full) < m.width {
		return full
	}
	const lead, tail = "delete ", "? (y/N) "
	if avail := m.width - lipgloss.Width(lead) - lipgloss.Width(tail) - 1; avail >= 4 {
		return lead + ansi.Truncate(name, avail, "…") + tail
	}
	return ansi.Truncate("delete? (y/N) ", max(m.width-1, 0), "")
}

// askRemove turns the input into the deletion question for c. The query is
// kept aside and comes back on any answer but yes.
func (m *HopModel) askRemove(c hop.Candidate) {
	name := c.Branch
	if name == "" {
		name = c.Label
	}
	m.confirm = &removeConfirm{cand: c, name: clone.Sanitize(name), query: m.input.Value()}
	m.input.SetValue("")
	// The prompt text itself is rendered per frame (confirmPrompt), so it
	// follows the width; only the style is set here.
	m.input.PromptStyle = styleErr.Bold(true)
}

// endRemove leaves the confirmation, restoring the prompt. The query comes
// back on a no; a yes clears it, so the reload that follows shows the
// worktree's repository (which the filter for the deleted worktree would
// not have matched) and the cursor can land on it.
func (m *HopModel) endRemove(yes bool) {
	if m.confirm == nil {
		return
	}
	if yes {
		m.input.SetValue("")
	} else {
		m.input.SetValue(m.confirm.query)
	}
	m.input.CursorEnd()
	m.confirm = nil
	// The list must match the input again at once: a failed deletion does
	// not reload, and an empty input over the old filter's rows (and its
	// highlights and synthetic rows) would be a lie.
	m.refilter()
	m.input.Prompt, m.input.PromptStyle = "hop> ", stylePromptHop
	if m.worktreeMode {
		m.input.Prompt, m.input.PromptStyle = "repo> ", stylePromptWorktree
	}
}

// remove runs the deletion of c in the background, on state re-read from
// herdr at that moment (hop.RemoveNow): the rows' state served to decide
// whether to ask, not to act.
func (m HopModel) remove(c hop.Candidate) tea.Cmd {
	h, git := m.h, m.git
	return func() tea.Msg {
		return removedMsg{repoRoot: c.RepoRoot, err: hop.RemoveNow(h, git, c, false)}
	}
}

// removeErrorText makes the two refusals a user can act on read as advice:
// a dirty checkout (git's and herdr's wording differ) and everything else
// verbatim.
func removeErrorText(err error) string {
	s := err.Error()
	if strings.Contains(s, "modified or untracked files") || strings.Contains(s, "dirty_worktree_requires_force") {
		return "not deleted: the worktree has modified or untracked files; commit, stash or clean them first"
	}
	return "not deleted: " + s
}

// keyHint is one entry of the help line.
type keyHint struct{ key, desc string }

// renderKeyHints styles the help line so the key names stand out while the
// descriptions stay quiet: keys as chips, dimmed text, spacing instead of
// separator characters.
func renderKeyHints(hints []keyHint) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = styleChip.Render(" "+h.key+" ") + " " + styleDim.Render(h.desc)
	}
	return strings.Join(parts, "  ")
}

// rowSeg is one styled fragment of a list row. quiet segments (the path
// column) are not bolded on the cursor row, only un-dimmed.
type rowSeg struct {
	text  string
	style lipgloss.Style
	quiet bool
}

// segsWidth is the display width of a row's segments.
func segsWidth(segs []rowSeg) int {
	w := 0
	for _, s := range segs {
		w += lipgloss.Width(s.text)
	}
	return w
}

// matchedSegs splits raw into matched/unmatched runs so the runes the query
// hit light up. pos indexes the raw text; Sanitize may drop runes, so
// highlighting is skipped when the displayed copy differs from the matched
// one.
func matchedSegs(raw string, base lipgloss.Style, pos []int, quiet bool) []rowSeg {
	shown := clone.Sanitize(raw)
	if len(pos) == 0 || shown != raw {
		return []rowSeg{{text: shown, style: base, quiet: quiet}}
	}
	set := make(map[int]bool, len(pos))
	for _, p := range pos {
		set[p] = true
	}
	var segs []rowSeg
	runes := []rune(shown)
	for start := 0; start < len(runes); {
		end := start
		for end < len(runes) && set[end] == set[start] {
			end++
		}
		st, q := base, quiet
		if set[start] {
			st, q = styleMatch, false
		}
		segs = append(segs, rowSeg{text: string(runes[start:end]), style: st, quiet: q})
		start = end
	}
	return segs
}

// rowSegs assembles row i's left cell: the primary name, kind tag, tree
// badge, branch and status badges. The path column is appended by the
// caller, which aligns it across rows. Segments so the cursor row can be
// re-rendered with emphasis: every segment bold with its Faint dimming
// lifted, colors kept — text emphasis rather than a reversed/background
// bar, which reads as glare.
func (m HopModel) rowSegs(i int) []rowSeg {
	c, _ := m.rowAt(i)
	disp := clone.Sanitize
	var segs []rowSeg
	add := func(text string, style lipgloss.Style) {
		segs = append(segs, rowSeg{text: text, style: style})
	}
	mi := m.matches[m.view[i]]
	if m.tree.active && m.tree.child[m.view[i]] {
		add("└─ ", styleBranch)
	}
	switch {
	case c.Kind == hop.KindWorktree && c.Branch != "":
		// A worktree reads by its branch; the checkout path (its label) is
		// machine-made noise that lives in the path column.
		segs = append(segs, matchedSegs(c.Branch, styleBranch, mi.branch, false)...)
	case c.Kind == hop.KindWorktree:
		// Detached: no branch to show, but the row is still a worktree and
		// keeps the tree hue.
		segs = append(segs, matchedSegs(c.Label, styleBranch, mi.label, false)...)
	default:
		segs = append(segs, matchedSegs(c.Label, lipgloss.NewStyle(), mi.label, false)...)
	}
	switch c.Kind {
	case hop.KindRepo, hop.KindWorktree:
		// The dominant kinds carry no tag: worktrees already read as such
		// from the tree indent.
	default:
		add("  "+c.Kind.String(), styleKind[c.Kind].Faint(true))
	}
	if m.tree.active {
		if n := m.tree.count[m.view[i]]; n > 0 {
			// ASCII fold markers: - open (tab folds), + folded (tab expands).
			// Triangle glyphs render as tiny dots in some fonts.
			marker, word := "-", "worktrees"
			if m.collapsed[c.Path] {
				marker = "+"
			}
			if n == 1 {
				word = "worktree"
			}
			add(fmt.Sprintf("  %s %d %s", marker, n, word), styleDim)
		}
	}
	switch {
	case c.Kind == hop.KindClone, c.Kind == hop.KindClonePull:
		add("  "+disp(c.Branch), styleDim) // URL
	case c.Kind == hop.KindPull:
		add("  ("+disp(c.Branch)+")", styleDim) // repository label
	case c.Branch != "" && c.Kind != hop.KindWorktree:
		add(" (", styleDim)
		segs = append(segs, matchedSegs(c.Branch, styleDim, mi.branch, false)...)
		add(")", styleDim)
	}
	if idx := m.view[i]; idx < len(m.cands) {
		if n := disp(prNote(m.notes, idx)); n != "" {
			add(n, styleOpen)
		}
	}
	return segs
}

// badge is row i's open-state glyph, shown in its own column between the
// left cell and the path: ● open (●N: N workspaces), ? state unknown.
// OpenState says whether it is open (worktree list or snapshot); OpenCount
// is only how many the snapshot confirmed, so a snapshot failure does not
// hide a known-open row.
func (m HopModel) badge(i int) (string, lipgloss.Style) {
	c, _ := m.rowAt(i)
	switch {
	case c.OpenState == hop.OpenOpen && c.OpenCount > 1:
		return fmt.Sprintf("●%d", c.OpenCount), styleOpen
	case c.OpenState == hop.OpenOpen:
		return "●", styleOpen
	case (c.Kind == hop.KindRepo || c.Kind == hop.KindWorktree) && c.OpenState == hop.OpenUnknown:
		return "?", styleWarn
	default:
		return "", styleDim
	}
}

// pathColumn is row i's dim absolute path. A worktree shows its branch as
// the primary text, so a query that hit its label (the checkout path)
// lights up here instead.
func (m HopModel) pathColumn(i int) []rowSeg {
	c, _ := m.rowAt(i)
	var pos []int
	if c.Kind == hop.KindWorktree && c.Branch != "" {
		if mi := m.matches[m.view[i]]; len(mi.label) > 0 && strings.HasSuffix(c.Path, c.Label) {
			off := len([]rune(c.Path)) - len([]rune(c.Label))
			for _, p := range mi.label {
				pos = append(pos, p+off)
			}
		}
	}
	return matchedSegs(c.Path, styleDim, pos, true)
}

// truncateSegs clips a row to width display columns, ending it with an
// ellipsis: a row that wraps would break the one-line-per-row geometry the
// viewport math relies on.
func truncateSegs(segs []rowSeg, width int) []rowSeg {
	used := 0
	for i, s := range segs {
		w := lipgloss.Width(s.text)
		if used+w <= width {
			used += w
			continue
		}
		budget := width - used - 1 // one column for the ellipsis
		if budget < 0 {
			return append([]rowSeg{}, segs[:i]...)
		}
		var b strings.Builder
		for _, r := range s.text {
			rw := lipgloss.Width(string(r))
			if budget < rw {
				break
			}
			budget -= rw
			b.WriteRune(r)
		}
		return append(append([]rowSeg{}, segs[:i]...), rowSeg{text: b.String() + "…", style: s.style, quiet: s.quiet})
	}
	return segs
}

// rowMatch is the matched rune positions of one filtered row.
type rowMatch struct {
	label  []int // rune indexes into Candidate.Label
	branch []int // rune indexes into Candidate.Branch
}

// treeState describes a grouped view: which candidate indexes render as
// indented children, and how many worktrees each repo row groups. Folding
// only applies to the empty-query view; the filtered view keeps the grouped
// rendering but ignores (and must not edit) the fold state.
type treeState struct {
	active   bool         // grouped rendering (indented children) applies
	foldable bool         // Tab folding applies (empty-query view only)
	child    map[int]bool // candidate index -> rendered indented under its repo
	count    map[int]int  // repo candidate index -> grouped worktree count
}

func (m HopModel) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.load(m.loadGen)) }

// load builds the candidate list. Remote identities are not resolved here:
// they take longer than everything else combined and only URL matching needs
// them, so they are resolved lazily (startResolve, once a query names a
// repository or PR) and arrive via resolvedMsg.
func (m HopModel) load(gen int) tea.Cmd {
	cfg, h, git, lg := m.cfg, m.h, m.git, m.log
	return func() tea.Msg {
		start := time.Now()
		l, err := hop.Load(timedLister{h, lg}, timedGitLister{git, lg}, cfg.ScanTargets(), cfg.SearchPaths)
		lg.Printf("load gen %d: build %v (%d candidates)", gen, time.Since(start), len(l.Cands))
		return loadedMsg{cands: l.Cands, warn: err, gen: gen, occupancy: l.Occupancy}
	}
}

// timedGitLister logs each `git worktree list` duration, mirroring
// timedLister for the git side of the load.
type timedGitLister struct {
	g  hop.WorktreeLister
	lg *log.Logger
}

func (t timedGitLister) WorktreeList(repo string) (gitx.WorktreeListing, error) {
	s := time.Now()
	l, err := t.g.WorktreeList(repo)
	t.lg.Printf("git worktree list %s: %v (err=%v)", repo, time.Since(s), err)
	return l, err
}

// timedLister logs each herdr round trip's duration. The load runs right
// when the popup opens — the moment the herdr server is busiest — so these
// calls can cost far more than they do against an idle server, and this log
// is where that shows up.
type timedLister struct {
	h  hop.Lister
	lg *log.Logger
}

func (t timedLister) Snapshot() (*herdr.Snapshot, error) {
	s := time.Now()
	snap, err := t.h.Snapshot()
	t.lg.Printf("herdr snapshot: %v (err=%v)", time.Since(s), err)
	return snap, err
}

func (t timedLister) WorktreeList(repo string) (*herdr.WorktreeList, error) {
	s := time.Now()
	l, err := t.h.WorktreeList(repo)
	t.lg.Printf("herdr worktree list %s: %v (err=%v)", repo, time.Since(s), err)
	return l, err
}

// startLoad begins a new load generation: in-flight background passes
// (identity resolution, worktree states) belong to the previous generation,
// so they are cancelled (their results would be discarded by the generation
// guard anyway).
func (m *HopModel) startLoad() tea.Cmd {
	if m.resolveCancel != nil {
		m.resolveCancel()
		m.resolveCancel = nil
	}
	if m.wtStateCancel != nil {
		m.wtStateCancel()
		m.wtStateCancel = nil
	}
	m.queuedWorktree = ""
	m.loadGen++
	m.loading = true
	m.resolveStarted = 0
	m.wtStateStarted = 0
	return m.load(m.loadGen)
}

func (m HopModel) open(c hop.Candidate, force bool) tea.Cmd {
	h := m.h
	return func() tea.Msg { return doneMsg{hop.Open(h, c, force)} }
}

// busy reports whether a load, an open command, a clone or a pull-request
// operation is in flight. While busy, candidate actions and reloads are
// ignored: stale candidates must not be acted on, and concurrent loads
// could complete out of order.
func (m HopModel) busy() bool {
	return m.loading || m.pending || m.clone.running || m.pr.running()
}

// idsPending reports whether the current generation's remote identities have
// not been applied yet.
func (m HopModel) idsPending() bool { return m.resolvedGen != m.loadGen }

// queryNeedsIDs reports whether the current query is matched against remote
// identities: it parses as a pull-request URL or a clone target.
func (m HopModel) queryNeedsIDs() bool {
	q := m.input.Value()
	if _, err := clone.ParsePR(q); err == nil {
		return true
	}
	_, err := clone.Parse(q, m.cfg.DefaultHost, m.cfg.CloneProtocol)
	return err == nil
}

// resolvingQuery: the query needs remote identities and they have not
// arrived yet. Clone/PR rows cannot be decided, and the cursor may sit on a
// fuzzy hit that merely resembles the named repository — so the view shows
// "resolving remotes..." and candidate actions are ignored until the
// resolvedMsg lands.
func (m HopModel) resolvingQuery() bool {
	return m.idsPending() && !m.loading && m.queryNeedsIDs()
}

// cancelAll stops every background git process the model owns. Called
// before the program exits so nothing outlives the popup.
func (m HopModel) cancelAll() {
	if m.resolveCancel != nil {
		m.resolveCancel()
	}
	if m.wtStateCancel != nil {
		m.wtStateCancel()
	}
	if m.clone.cancel != nil {
		m.clone.cancel()
	}
	if m.pr != nil && m.pr.cancel != nil {
		m.pr.cancel()
	}
	if m.wt != nil && m.wt.prCancel != nil {
		m.wt.prCancel()
	}
	if m.wt != nil && m.wt.prInfoCancel != nil {
		m.wt.prInfoCancel()
	}
}

func (m HopModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.WindowSizeMsg); !ok && m.wt != nil {
		// Branch screen owns everything except window sizing. Background
		// messages of the picker (a late loadedMsg) are still applied.
		switch msg.(type) {
		case loadedMsg, resolvedMsg, wtStateMsg, cloneProgressMsg, cloneDoneMsg, doneMsg, prCheckedMsg, prFetchedMsg, prDoneMsg:
		default:
			return m.updateBranch(msg)
		}
	}
	// Pull-request operation messages and, while it runs, keys.
	if mm, cmd, handled := m.updatePR(msg); handled {
		if mm.quit {
			mm.cancelAll()
		}
		return mm, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollToCursor()
		return m, nil
	case loadedMsg:
		if msg.gen != m.loadGen {
			return m, nil // a stale load that a newer one superseded
		}
		m.loading = false
		m.log.Printf("loaded %d candidates (search_paths=%v)", len(msg.cands), m.cfg.SearchPaths)
		m.cands = msg.cands
		m.occupancy = msg.occupancy
		m.labels = make([]string, len(m.cands))
		for i, c := range m.cands {
			m.labels[i] = c.Label
		}
		var warns []string
		if msg.warn != nil {
			warns = append(warns, msg.warn.Error())
		}
		if len(m.cfg.SearchPaths) == 0 {
			warns = append(warns, config.EnvRoot+" is not set and no search_paths configured")
		}
		m.warn = strings.Join(warns, "\n")
		if m.warn != "" {
			m.log.Printf("warn: %s", m.warn)
		}
		m.refilter()
		if m.focusPath != "" {
			// After a deletion the cursor goes to the repository the
			// worktree belonged to, not back to the top.
			for i, c := range m.cands {
				if c.Path == m.focusPath {
					m.cursorTo(i)
					break
				}
			}
			m.focusPath = ""
		}
		// Worktree mode with a recognizable current repository skips the
		// repo picker: prefix+t goes straight to the branch screen of the
		// repository the user is in (esc still returns to the picker for
		// choosing another one). Only on the initial load — a reload must
		// never yank a user who esc'd back to the picker — and only while
		// they have not started typing a filter.
		var branchCmd tea.Cmd
		if m.worktreeMode && msg.gen == 1 && m.wt == nil && m.input.Value() == "" {
			if root, label, ok := m.currentRepo(); ok {
				m, branchCmd = m.enterBranchScreen(root, label)
			}
		}
		// The authoritative worktree open states are fetched in the
		// background — unless the load went straight to the branch screen,
		// which never reads them; leaveBranchScreen starts the pass then.
		var wtStates tea.Cmd
		if m.wt == nil {
			wtStates = m.startWorktreeStates()
		}
		// Remote identities are resolved only when a query asks for them
		// (startResolve, from the input handler); a query typed during the
		// load, or kept across a reload, asks now.
		var resolve tea.Cmd
		if m.queryNeedsIDs() {
			resolve = m.startResolve()
		}
		return m, tea.Batch(wtStates, branchCmd, resolve)
	case wtStateMsg:
		if msg.gen != m.loadGen {
			return m, nil // states for candidates a newer load replaced
		}
		if m.wtStateCancel != nil {
			m.wtStateCancel() // release the context; the work is done
			m.wtStateCancel = nil
		}
		m.wtStateGen = msg.gen
		hop.ApplyWorktreeStates(m.cands, msg.states)
		if m.queuedWorktree != "" {
			// An Enter arrived before the authoritative state: run it now.
			path := m.queuedWorktree
			m.queuedWorktree = ""
			for _, c := range m.cands {
				if c.Kind == hop.KindWorktree && c.Path == path {
					m.log.Printf("open %s %s force=false (queued)", c.Kind, c.Path)
					return m, m.open(c, false)
				}
			}
			m.pending = false // the row is gone; nothing to open
		}
		return m, nil
	case resolvedMsg:
		if msg.gen != m.loadGen {
			return m, nil // resolved for candidates a newer load replaced
		}
		if m.resolveCancel != nil {
			m.resolveCancel() // release the context; the work is done
			m.resolveCancel = nil
		}
		m.resolvedGen = msg.gen
		hop.ApplyRepoIDs(m.cands, msg.ids)
		// Rebuild the view only when the identities can change it (the query
		// denotes a repository or PR): buildFiltered re-anchors the cursor,
		// and a cursor the user parked must not jump for an invisible update.
		if m.queryNeedsIDs() {
			m.refilter()
		}
		return m, nil
	case removedMsg:
		m.pending = false
		if msg.err != nil {
			m.errMsg = removeErrorText(msg.err)
			m.log.Printf("remove worktree: %v", msg.err)
			return m, nil
		}
		m.log.Printf("remove worktree: done")
		m.focusPath = msg.repoRoot
		return m, m.startLoad()
	case doneMsg:
		m.pending = false
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			m.log.Printf("error: %v", msg.err)
			return m, nil
		}
		m.quit = true
		m.cancelAll() // the resolver may still be running; do not outlive the popup
		return m, tea.Quit
	case cloneProgressMsg:
		if msg.op != m.clone.op {
			return m, nil // stale operation
		}
		m.clone.progress = append(m.clone.progress, msg.line)
		if len(m.clone.progress) > 6 {
			m.clone.progress = m.clone.progress[1:]
		}
		return m, m.waitCloneEvent()
	case cloneDoneMsg:
		if msg.op != m.clone.op {
			return m, nil // stale operation
		}
		m.clone.running = false
		if m.clone.cancel != nil {
			m.clone.cancel()
		}
		if m.clone.canceled {
			// UI contract: after Esc, never open a workspace, even if git
			// had already finished by the time the key was processed.
			m.errMsg = "clone canceled"
			if msg.err == nil {
				m.errMsg += " (git had already finished; " + msg.dest + " exists, ctrl-r to reload)"
			}
			m.log.Printf("clone canceled: %s", m.clone.dest)
			return m, nil
		}
		if msg.err != nil {
			// Never surface or log the raw URL: it may carry user:token@.
			m.errMsg = m.clone.target.Mask(msg.err.Error())
			m.log.Printf("clone failed: %s", clone.Sanitize(m.errMsg))
			return m, nil
		}
		m.log.Printf("clone ok: %s", msg.dest)
		if pr := m.clone.thenPR; pr != nil {
			// clone+pull: register the new checkout as a candidate so that a
			// retry after a PR failure finds it (pull row, not clone again),
			// then continue with the pull request in it.
			t := pr.Target()
			m.cands = append(m.cands, hop.Candidate{
				Kind: hop.KindRepo, Path: msg.dest, Label: t.Host + "/" + t.Owner + "/" + t.Repo,
				OpenState: hop.OpenClosed, RepoID: t.ID(), RepoPaths: []string{pr.RepoPath},
			})
			m.labels = append(m.labels, m.cands[len(m.cands)-1].Label)
			m.refilter()
			return m.startPR(*pr, msg.dest, "")
		}
		m.pending = true
		h, dest, label := m.h, msg.dest, m.clone.target.Owner+"/"+m.clone.target.Repo
		return m, func() tea.Msg { return doneMsg{h.WorkspaceCreate(dest, label)} }
	case tea.KeyMsg:
		if m.clone.running {
			if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyEsc {
				// Record the user's intent first: the process may already have
				// finished, in which case cancel() is a no-op and the pending
				// cloneDoneMsg reports success.
				m.clone.canceled = true
				m.clone.cancel()
			}
			return m, nil
		}
		if m.errMsg != "" {
			switch msg.Type {
			case tea.KeyCtrlC, tea.KeyEsc:
				m.cancelAll()
				return m, tea.Quit
			case tea.KeyCtrlR:
				// The error text advertises ctrl-r as "reload": do it in one
				// keypress rather than merely dismissing the message.
				m.errMsg = ""
				m.clone.progress = nil
				if m.busy() {
					return m, nil
				}
				return m, m.startLoad()
			default:
				m.errMsg = ""
				m.clone.progress = nil
				return m, nil
			}
		}
		if m.confirm != nil {
			// The question takes every key: y deletes, anything else is no.
			c := m.confirm.cand
			yes := msg.Type == tea.KeyRunes && (string(msg.Runes) == "y" || string(msg.Runes) == "Y")
			m.endRemove(yes)
			if yes {
				m.pending = true
				m.log.Printf("remove worktree %s (open=%v)", c.Path, c.IsOpen())
				return m, m.remove(c)
			}
			return m, nil
		}
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelAll()
			return m, tea.Quit
		case tea.KeyCtrlD:
			c, ok := m.selected()
			if !ok || m.busy() || m.resolvingQuery() {
				return m, nil
			}
			if c.Kind == hop.KindWorktree && m.wtStateGen != m.loadGen {
				// Whether a workspace is open for it decides which tool
				// removes it; wait for the authoritative state (~150ms).
				m.errMsg = "open state still loading: try again in a moment"
				return m, nil
			}
			if err := hop.CanRemove(c, m.occupancy); err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
			m.askRemove(c)
			return m, nil
		case tea.KeyEnter:
			c, ok := m.selected()
			if !ok || m.busy() || m.resolvingQuery() {
				return m, nil
			}
			switch c.Kind {
			case hop.KindClone:
				if m.worktreeMode {
					return m, nil
				}
				return m.startClone()
			case hop.KindPull:
				if m.curPR == nil {
					return m, nil
				}
				return m.startPR(*m.curPR, c.Path, c.PRBranch)
			case hop.KindClonePull:
				if m.worktreeMode || m.curPR == nil {
					return m, nil
				}
				return m.startClonePR(*m.curPR)
			case hop.KindNote:
				m.errMsg = c.Label
				return m, nil
			}
			if m.worktreeMode {
				if repo, label, ok := worktreeRepoFor(c); ok {
					return m.enterBranchScreen(repo, label)
				}
				m.errMsg = "select a repository or worktree row to create a worktree from"
				return m, nil
			}
			if c.Kind == hop.KindWorktree && m.wtStateGen != m.loadGen {
				// The authoritative open state (herdr worktree list) has not
				// arrived: queue the open instead of acting on the
				// provisional state, and run it on wtStateMsg (~150ms).
				m.pending = true
				m.queuedWorktree = c.Path
				m.log.Printf("open %s %s queued until worktree states arrive", c.Kind, c.Path)
				return m, nil
			}
			m.pending = true
			m.log.Printf("open %s %s force=false", c.Kind, c.Path)
			return m, m.open(c, false)
		case tea.KeyCtrlT:
			c, ok := m.selected()
			if !ok || m.busy() || m.resolvingQuery() {
				return m, nil
			}
			if repo, label, ok := worktreeRepoFor(c); ok {
				return m.enterBranchScreen(repo, label)
			}
			return m, nil
		case tea.KeyCtrlN:
			if c, ok := m.selected(); ok && c.Kind == hop.KindRepo && !m.busy() && !m.resolvingQuery() {
				m.pending = true
				m.log.Printf("open %s %s force=true", c.Kind, c.Path)
				return m, m.open(c, true)
			}
			return m, nil
		case tea.KeyCtrlR:
			if m.busy() {
				return m, nil
			}
			return m, m.startLoad()
		case tea.KeyTab:
			m.toggleGroup()
			return m, nil
		case tea.KeyUp, tea.KeyCtrlP:
			m.moveCursor(-1)
			return m, nil
		case tea.KeyDown:
			m.moveCursor(1)
			return m, nil
		}
		if m.pending {
			// A command is in flight (an open, a deletion): typing now
			// would edit a query the reload that follows is meant to see
			// as it is — empty, after a deletion.
			return m, nil
		}
	}
	var cmd tea.Cmd
	before := m.input.Value()
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != before {
		m.refilter()
		// A query that names a repository or PR needs the remote identities
		// (clone row, pull rows): resolve them now if not yet done. Once
		// started, a resolver runs to completion even if the query stops
		// needing it — restarting later would cost the same work again.
		if m.queryNeedsIDs() {
			cmd = tea.Batch(cmd, m.startResolve())
		}
	}
	return m, cmd
}

// startWorktreeStates starts the background pass that asks herdr for the
// authoritative open state of every worktree (one `herdr worktree list`
// per repository with worktrees; the server answers them one at a time,
// ~50ms each, so doing this during Build would dominate the load). Until
// its wtStateMsg lands, worktree rows show the provisional state and Enter
// on one is queued. No-op while loading, or once started for this load.
func (m *HopModel) startWorktreeStates() tea.Cmd {
	if m.loading || m.wtStateGen == m.loadGen || m.wtStateStarted == m.loadGen {
		return nil
	}
	if m.wtStateCancel != nil {
		m.wtStateCancel()
	}
	wctx, wcancel := context.WithCancel(context.Background())
	m.wtStateCancel = wcancel
	m.wtStateStarted = m.loadGen
	h, gen, lg := m.h, m.loadGen, m.log
	// The pass gets its own copy of the candidates: its Apply* writes to
	// m.cands while a resolver may still be reading its own copy.
	cands := slices.Clone(m.cands)
	return func() tea.Msg {
		start := time.Now()
		st := hop.WorktreeStates(wctx, timedLister{h, lg}, cands)
		lg.Printf("load gen %d: worktree states for %d roots in %v", gen, len(st.OK), time.Since(start))
		return wtStateMsg{gen: gen, states: st}
	}
}

// startResolve starts resolving the remote identities of the current load's
// candidates, unless that is done or under way, or no load has landed. The
// git processes it spawns (one `git remote -v` per repository, see
// hop.ResolveRepoIDs) are the most expensive part of a load, so they run
// only for the queries that use their result — see resolveStarted.
func (m *HopModel) startResolve() tea.Cmd {
	if m.loading || m.resolvedGen == m.loadGen || m.resolveStarted == m.loadGen {
		return nil
	}
	if m.resolveCancel != nil {
		m.resolveCancel()
	}
	rctx, rcancel := context.WithCancel(context.Background())
	m.resolveCancel = rcancel
	m.resolveStarted = m.loadGen
	git, gen, lg := m.git, m.loadGen, m.log
	cands := slices.Clone(m.cands)
	lg.Printf("load gen %d: resolving remote identities (the query names a repository)", gen)
	return func() tea.Msg {
		start := time.Now()
		ids := hop.ResolveRepoIDs(rctx, git, cands)
		lg.Printf("load gen %d: resolved %d repo identities in %v", gen, len(ids), time.Since(start))
		return resolvedMsg{gen: gen, ids: ids}
	}
}

// refilter recomputes the visible rows and the synthetic clone row.
//
// When the query parses as a clone target, candidates that ARE that
// repository (by origin identity or destination path) are always shown first,
// even if the raw query (e.g. a full URL) does not fuzzy-match their label.
// The clone row is offered only when no such candidate exists.
func (m *HopModel) refilter() {
	q := m.input.Value()
	m.cloneRow = nil
	m.curPR = nil
	m.extra = nil
	m.notes = nil
	m.matches = nil
	m.tree = treeState{}
	// An empty query shows the tree: worktrees grouped under their
	// repository. A query shows the fuzzy-ranked matches, still grouped —
	// the fold state is kept but not applied, so every match stays
	// reachable by typing.
	if strings.TrimSpace(q) == "" {
		m.buildTree()
		if m.cursor >= m.rowCount() {
			m.cursor = max(0, m.rowCount()-1)
		}
		m.scrollToCursor()
		return
	}
	m.buildFiltered(q)
	if m.idsPending() {
		// Remote identities have not arrived: neither the PR rows nor the
		// clone row can be decided yet. The fuzzy result stands; the
		// resolvedMsg handler refilters once the identities are in, and the
		// UI ignores candidate actions in the meantime (resolvingQuery).
		if m.cursor >= m.rowCount() {
			m.cursor = max(0, m.rowCount()-1)
		}
		m.scrollToCursor()
		return
	}
	base := len(m.cands)

	// A pull request URL is recognised before plain clone input: clone.Parse
	// would read ".../pull/123" as owner "pull", repo "123". Matching
	// checkouts and the pull rows are shown regardless of the fuzzy result
	// (the URL is longer than any label and never fuzzy-matches).
	if pr, err := clone.ParsePR(q); err == nil {
		m.curPR = &pr
		first, extra, notes := m.prRows(pr)
		m.notes = notes
		m.extra = extra
		rows := append([]int(nil), first...)
		for i := range extra {
			rows = append(rows, base+i)
		}
		m.view = prependUnique(m.groupRows(rows), m.view)
		// Drop rows that must only be reachable through a pull row.
		kept := m.view[:0]
		for _, idx := range m.view {
			if idx < base && m.notes[idx] == hiddenNote {
				continue
			}
			kept = append(kept, idx)
		}
		m.view = kept
		// Cursor starts on the first pull row (or the existing worktree row).
		m.cursor = 0
		for i, idx := range m.view {
			if idx >= base || m.notes[idx] != "" {
				m.cursor = i
				break
			}
		}
		m.scrollToCursor()
		return
	}

	if t, err := clone.Parse(q, m.cfg.DefaultHost, m.cfg.CloneProtocol); err == nil {
		same := m.identityMatches(t)
		switch {
		case len(same) > 0:
			m.view = prependUnique(m.groupRows(same), m.view)
			m.cursor = 0 // the checkout the input denotes
		case !m.worktreeMode:
			// Worktree mode exists to pick an existing repository; cloning is
			// not part of that flow, so no clone row is offered there (and
			// Enter never clones). Identity matching above still applies.
			m.cloneRow = m.cloneRowFor(t)
			if m.cloneRow != nil {
				m.extra = []hop.Candidate{*m.cloneRow}
				m.view = append(m.view, base)
			}
		}
	}
	if m.cursor >= m.rowCount() {
		m.cursor = max(0, m.rowCount()-1)
	}
	m.scrollToCursor()
}

// groups maps each repository row to its scanned worktrees: repoIdx is repo
// path -> candidate index, children is repo candidate index -> worktree
// candidate indexes (only worktrees whose main checkout is a candidate).
func (m HopModel) groups() (repoIdx map[string]int, children map[int][]int) {
	repoIdx = map[string]int{}
	for i, c := range m.cands {
		if c.Kind == hop.KindRepo {
			repoIdx[c.Path] = i
		}
	}
	children = map[int][]int{}
	for i, c := range m.cands {
		if c.Kind == hop.KindWorktree {
			if p, ok := repoIdx[c.RepoRoot]; ok {
				children[p] = append(children[p], i)
			}
		}
	}
	return repoIdx, children
}

// buildFiltered fills view with the query's matches, still grouped: a
// matching repository brings all its worktrees, a matching worktree brings
// its repository. Two stages, so that a repo-name query never lands the
// cursor on a worktree:
//
//  1. direct: the raw query against a repo's label, or a worktree's label +
//     branch;
//  2. composite (only when nothing in the group matched directly): every
//     whitespace-separated word must fuzzy-match one of the parent repo
//     label, the worktree label, or the branch — per field, not against a
//     concatenation, so a word cannot straddle a field boundary. This is
//     what makes "repo branch" (and "branch repo") queries work.
//
// Groups are ordered by stage, then score, then original position; the
// cursor lands on the best direct row (its repository on a tie) or, for a
// composite-only group, on its best worktree.
func (m *HopModel) buildFiltered(q string) {
	m.tree = treeState{active: true, child: map[int]bool{}, count: map[int]int{}}
	m.matches = map[int]rowMatch{}
	lq := []rune(strings.ToLower(q))
	var words [][]rune
	for w := range strings.FieldsSeq(strings.ToLower(q)) {
		words = append(words, []rune(w))
	}
	// direct matches the raw query against label (+ " " + branch); the
	// returned positions are rune indexes split back into label and branch,
	// for highlighting.
	direct := func(label, branch string) (score int, labelPos, branchPos []int, ok bool) {
		hay := label
		if branch != "" {
			hay += " " + branch
		}
		score, pos, ok := fuzzyMatch(lq, []rune(strings.ToLower(hay)))
		if !ok {
			return 0, nil, nil, false
		}
		n := len([]rune(label))
		for _, p := range pos {
			if p < n {
				labelPos = append(labelPos, p)
			} else if p > n { // p == n is the separator space
				branchPos = append(branchPos, p-n-1)
			}
		}
		return score, labelPos, branchPos, ok
	}
	// composite: every word matches at least one field; score is the sum of
	// each word's best per-field score, and the matched positions are
	// gathered per field for highlighting.
	composite := func(fields ...string) (int, [][]int, bool) {
		sum := 0
		fieldPos := make([][]int, len(fields))
		for _, w := range words {
			best, bestField, ok := 0, 0, false
			var bestPos []int
			for fi, f := range fields {
				if s, p, o := fuzzyMatch(w, []rune(strings.ToLower(f))); o && (!ok || s > best) {
					best, bestField, bestPos, ok = s, fi, p, true
				}
			}
			if !ok {
				return 0, nil, false
			}
			sum += best
			fieldPos[bestField] = append(fieldPos[bestField], bestPos...)
		}
		return sum, fieldPos, true
	}

	type unit struct {
		stage  int // 0 direct, 1 composite
		score  int
		rows   []int // candidate indexes, group head first
		cursor int   // candidate index the cursor should land on
	}
	var units []unit
	_, children := m.groups()
	grouped := map[int]bool{}
	for _, ws := range children {
		for _, w := range ws {
			grouped[w] = true
		}
	}

	for i, c := range m.cands {
		if grouped[i] {
			continue // scored within its repository's group
		}
		switch c.Kind {
		case hop.KindRepo:
			kids := children[i]
			repoScore, repoPos, _, repoOK := direct(c.Label, "")
			if repoOK {
				m.matches[i] = rowMatch{label: repoPos}
			}
			best, ok := repoScore, repoOK // best direct score: orders the group
			// A sparse match scores zero or below while still being a match,
			// so "no worktree chosen yet" needs its own flag — a zero
			// sentinel would leave the cursor on a repo that never matched.
			cursor, cursorScore, cursorSet := i, 0, false
			var hit []int // directly matching worktrees
			for _, w := range kids {
				wc := m.cands[w]
				if s, lp, bp, o := direct(wc.Label, wc.Branch); o {
					m.matches[w] = rowMatch{label: lp, branch: bp}
					hit = append(hit, w)
					if !ok || s > best {
						best, ok = s, true
					}
					if !cursorSet || s > cursorScore {
						cursor, cursorScore, cursorSet = w, s, true
					}
				}
			}
			if ok { // stage 0: something in the group matched directly
				rows := []int{i}
				if repoOK {
					// The repo itself matched: show everything and keep the
					// cursor on the repo, even when a worktree outscores it
					// on label overlap — Enter must not open a worktree the
					// user did not name.
					rows = append(rows, kids...)
					cursor = i
				} else {
					rows = append(rows, hit...)
				}
				units = append(units, unit{0, best, rows, cursor})
				continue
			}
			if s, fp, o := composite(c.Label); o {
				// Every word matched the repo label alone: a repo query
				// written with spaces — show the whole group, cursor on it.
				m.matches[i] = rowMatch{label: fp[0]}
				units = append(units, unit{1, s, append([]int{i}, kids...), i})
				continue
			}
			rows, best, cursor := []int{i}, 0, i
			for _, w := range kids {
				wc := m.cands[w]
				if s, fp, o := composite(c.Label, wc.Label, wc.Branch); o {
					m.matches[w] = rowMatch{label: fp[1], branch: fp[2]}
					if _, seen := m.matches[i]; !seen && len(fp[0]) > 0 {
						m.matches[i] = rowMatch{label: fp[0]}
					}
					rows = append(rows, w)
					if s > best || cursor == i {
						best, cursor = s, w
					}
				}
			}
			if cursor != i {
				units = append(units, unit{1, best, rows, cursor})
			}
		case hop.KindWorktree: // orphan: no repo row to group under
			if s, lp, bp, o := direct(c.Label, c.Branch); o {
				m.matches[i] = rowMatch{label: lp, branch: bp}
				units = append(units, unit{0, s, []int{i}, i})
			} else if s, fp, o := composite(c.RepoLabel, c.Label, c.Branch); o {
				m.matches[i] = rowMatch{label: fp[1], branch: fp[2]}
				units = append(units, unit{1, s, []int{i}, i})
			}
		default:
			if s, lp, _, o := direct(c.Label, ""); o {
				m.matches[i] = rowMatch{label: lp}
				units = append(units, unit{0, s, []int{i}, i})
			} else if s, fp, o := composite(c.Label); o {
				m.matches[i] = rowMatch{label: fp[0]}
				units = append(units, unit{1, s, []int{i}, i})
			}
		}
	}

	sort.SliceStable(units, func(a, b int) bool {
		if units[a].stage != units[b].stage {
			return units[a].stage < units[b].stage
		}
		return units[a].score > units[b].score
	})
	var view []int
	m.cursor = 0
	for ui, u := range units {
		for _, r := range u.rows {
			if grouped[r] {
				m.tree.child[r] = true
			}
			if ui == 0 && r == u.cursor {
				m.cursor = len(view)
			}
			view = append(view, r)
		}
	}
	m.view = view
}

// groupRows arranges forced matches (identity matches for a clone URL, the
// checkout rows of a pull-request URL) the way buildFiltered arranges fuzzy
// matches: a repository is followed by all its worktrees, a worktree is
// preceded by its repository, each row emitted once in order of first
// appearance. Synthetic rows (indexes past cands) pass through unchanged.
func (m *HopModel) groupRows(idxs []int) []int {
	_, children := m.groups()
	parentOf := map[int]int{}
	for p, ws := range children {
		for _, w := range ws {
			parentOf[w] = p
		}
	}
	var out []int
	seen := map[int]bool{}
	add := func(r int) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range idxs {
		if r >= len(m.cands) {
			add(r)
			continue
		}
		switch m.cands[r].Kind {
		case hop.KindRepo:
			add(r)
			for _, w := range children[r] {
				m.tree.child[w] = true
				add(w)
			}
		case hop.KindWorktree:
			if p, ok := parentOf[r]; ok {
				add(p)
				m.tree.child[r] = true
			}
			add(r)
		default:
			add(r)
		}
	}
	return out
}

// buildTree fills view with the grouped empty-query rows: each repository is
// followed by its scanned worktrees, indented, unless its group is folded;
// the group of the repository the picker was invoked from comes first.
// Worktrees whose main checkout is not among the candidates, and rows that
// are not part of any group, keep their flat position.
func (m *HopModel) buildTree() {
	repoIdx, children := m.groups()
	m.tree = treeState{active: true, foldable: true, child: map[int]bool{}, count: map[int]int{}}
	for p, ws := range children {
		m.tree.count[p] = len(ws)
		for _, w := range ws {
			m.tree.child[w] = true
		}
	}

	view := make([]int, 0, len(m.cands))
	seen := map[int]bool{}
	emit := func(p int) { // a repo row and, unless folded, its children
		if seen[p] {
			return
		}
		seen[p] = true
		view = append(view, p)
		if !m.collapsed[m.cands[p].Path] {
			view = append(view, children[p]...)
		}
	}
	if pin, ok := m.currentGroupParent(repoIdx); ok {
		emit(pin)
	}
	// Repositories open as a workspace, then the standalone workspace rows,
	// come next: with the pinned current group they make the top of the
	// list "what is open right now", which is the most likely hop target.
	// Both open states are known at first paint (repo rows from the
	// snapshot, workspace rows by definition). Orphan worktrees stay put
	// even when open: their state is confirmed by a background pass after
	// the first paint, and ordering by it would reshuffle the rows under
	// the user.
	for i, c := range m.cands {
		if c.Kind == hop.KindRepo && c.IsOpen() {
			emit(i)
		}
	}
	for i, c := range m.cands {
		if c.Kind == hop.KindWorkspace {
			seen[i] = true
			view = append(view, i)
		}
	}
	for i, c := range m.cands {
		switch {
		case seen[i]: // already placed in an earlier section
		case m.tree.child[i]: // shown under its repo (or hidden when folded)
		case c.Kind == hop.KindRepo:
			emit(i)
		default:
			view = append(view, i)
		}
	}
	m.view = view
}

// currentRepo is the repository the picker was invoked from, as a worktree
// would be created from it: the current row's own effective root first (a
// worktree knows its main checkout even when that checkout lies outside the
// search paths and has no row of its own), then the group parent lookup,
// which also covers a pane sitting in a subdirectory of a checkout.
func (m HopModel) currentRepo() (root, label string, ok bool) {
	for _, c := range m.cands {
		if c.Current {
			if root, label, ok := c.EffectiveRoot(); ok {
				return root, label, true
			}
		}
	}
	repoIdx, _ := m.groups()
	if pin, ok := m.currentGroupParent(repoIdx); ok {
		return m.cands[pin].Path, m.cands[pin].Label, true
	}
	return "", "", false
}

// currentGroupParent is the candidate index of the repository the picker was
// invoked from: the herdr-focused workspace's row, mapped to itself for a
// repository and to the main checkout for a worktree. A focused workspace
// that is not itself a checkout row — typically because its pane sits in a
// subdirectory of one — is mapped to the deepest repo or worktree candidate
// containing its path (on path boundaries: /r/api must not claim
// /r/api-old/sub).
func (m HopModel) currentGroupParent(repoIdx map[string]int) (int, bool) {
	cur := ""
	for _, c := range m.cands {
		if !c.Current {
			continue
		}
		switch c.Kind {
		case hop.KindRepo:
			if i, ok := repoIdx[c.Path]; ok {
				return i, true
			}
		case hop.KindWorktree:
			if i, ok := repoIdx[c.RepoRoot]; ok {
				return i, true
			}
		}
		if c.Path != "" {
			cur = c.Path
		}
	}
	if cur == "" {
		return 0, false
	}
	best, bestLen, found := 0, -1, false
	for i, c := range m.cands {
		if c.Kind != hop.KindRepo && c.Kind != hop.KindWorktree {
			continue
		}
		if c.Path == "" || (cur != c.Path && !strings.HasPrefix(cur, c.Path+"/")) {
			continue
		}
		if len(c.Path) <= bestLen {
			continue
		}
		p := i
		if c.Kind == hop.KindWorktree {
			pi, ok := repoIdx[c.RepoRoot]
			if !ok {
				continue
			}
			p = pi
		}
		best, bestLen, found = p, len(c.Path), true
	}
	return best, found
}

// toggleGroup folds or unfolds the worktree group under the cursor (Tab).
// While filtering it does nothing: the fold state must not be edited through
// a view that does not show it.
func (m *HopModel) toggleGroup() {
	if !m.tree.foldable {
		return
	}
	c, ok := m.selected()
	if !ok {
		return
	}
	idx := m.view[m.cursor] // candidate index under the cursor
	var root string
	switch {
	case c.Kind == hop.KindRepo && m.tree.count[idx] > 0:
		root = c.Path
	case c.Kind == hop.KindWorktree && m.tree.child[idx]:
		root = c.RepoRoot
	}
	if root == "" {
		return
	}
	folding := !m.collapsed[root]
	if folding {
		m.collapsed[root] = true
	} else {
		delete(m.collapsed, root)
	}
	keep := idx
	if folding && m.tree.child[idx] {
		// The selected worktree is about to disappear: land on its repo.
		for i, cc := range m.cands {
			if cc.Kind == hop.KindRepo && cc.Path == root {
				keep = i
				break
			}
		}
	}
	m.buildTree()
	m.cursorTo(keep)
}

// cursorTo puts the cursor on the row showing candidate index idx (the first
// row when that candidate is not visible) and re-anchors the window.
func (m *HopModel) cursorTo(idx int) {
	m.cursor = 0
	for i, v := range m.view {
		if v == idx {
			m.cursor = i
			break
		}
	}
	m.scrollToCursor()
}

// listRows is how many candidate rows fit below the header lines viewList
// writes. Derived from model state only, so Update can scroll with the same
// geometry View renders with (View cannot persist the scroll offset).
func (m HopModel) listRows() int {
	used := 2
	if m.warn != "" {
		used += strings.Count(m.warn, "\n") + 1
	}
	if m.pending {
		used++
	}
	if m.resolvingQuery() {
		used++
	}
	rows := m.height - used - 2
	if rows < 1 {
		rows = 10
	}
	return rows
}

// scrollStart is the first visible row: the stored viewport top, clamped to
// the row count and pulled along when the cursor has moved outside of it.
func (m HopModel) scrollStart() int {
	rows := m.listRows()
	top := max(0, min(m.top, m.rowCount()-rows))
	top = min(top, m.cursor)         // cursor above the window: slide up to it
	return max(top, m.cursor-rows+1) // cursor below the window: slide down to it
}

func (m *HopModel) scrollToCursor() { m.top = m.scrollStart() }

// moveCursor moves the cursor by delta rows, sliding the window only when
// the cursor crosses one of its edges. The stored top is first re-anchored
// to the window actually rendered: the header may have grown or shrunk since
// top was last saved (e.g. the "opening..." line appearing), and the move
// must be relative to what the user currently sees, not to a stale top.
func (m *HopModel) moveCursor(delta int) {
	m.top = m.scrollStart()
	m.cursor = max(0, min(m.cursor+delta, m.rowCount()-1))
	m.scrollToCursor()
}

// startClonePR runs the clone+pull row: clone, then continue with the PR.
func (m HopModel) startClonePR(pr clone.PR) (HopModel, tea.Cmd) {
	m, cmd := m.startCloneTarget(pr.Target())
	if m.clone.running {
		p := pr
		m.clone.thenPR = &p
	}
	return m, cmd
}

// identityMatches returns indexes of candidates that are the repository t
// refers to. Identity is decided by the origin remote (RepoID) or by the
// destination path — never by the display label, which depends on where the
// search root happens to be.
func (m HopModel) identityMatches(t clone.Target) []int {
	dest := m.destFor(t)
	id := t.ID()
	var out []int
	for i, c := range m.cands {
		if c.Path == "" {
			continue
		}
		if c.RepoID == id || (dest != "" && c.Path == dest) {
			out = append(out, i)
		}
	}
	return out
}

// destFor returns the normalized clone destination for t, or "" if the root
// is unset or the destination would be invalid.
func (m HopModel) destFor(t clone.Target) string {
	if m.cfg.Root == "" {
		return ""
	}
	// Normalize the (existing) root so dest compares equal to scanned paths.
	dest, err := t.Dest(scan.Normalize(m.cfg.Root))
	if err != nil {
		return ""
	}
	return dest
}

// cloneRowFor builds the synthetic clone row for t.
func (m HopModel) cloneRowFor(t clone.Target) *hop.Candidate {
	label := t.Host + "/" + t.Owner + "/" + t.Repo
	// Branch carries the display URL (redacted); the real URL is re-parsed
	// from the input when the clone starts.
	return &hop.Candidate{Kind: hop.KindClone, Path: m.destFor(t), Label: label, Branch: t.SafeURL()}
}

func prependUnique(first, rest []int) []int {
	seen := make(map[int]bool, len(first))
	out := make([]int, 0, len(first)+len(rest))
	for _, i := range first {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	for _, i := range rest {
		if !seen[i] {
			out = append(out, i)
		}
	}
	return out
}

func (m HopModel) rowCount() int { return len(m.view) }

// rowAt returns the candidate for a visible row index; synthetic rows
// (clone, pull, clone+pull) live in extra and are addressed past cands.
func (m HopModel) rowAt(i int) (hop.Candidate, bool) {
	if i < 0 || i >= len(m.view) {
		return hop.Candidate{}, false
	}
	idx := m.view[i]
	if idx < len(m.cands) {
		return m.cands[idx], true
	}
	if j := idx - len(m.cands); j < len(m.extra) {
		return m.extra[j], true
	}
	return hop.Candidate{}, false
}

func (m HopModel) selected() (hop.Candidate, bool) { return m.rowAt(m.cursor) }

// startClone validates the input and launches git clone for the clone row.
func (m HopModel) startClone() (HopModel, tea.Cmd) {
	t, err := clone.Parse(m.input.Value(), m.cfg.DefaultHost, m.cfg.CloneProtocol)
	if err != nil {
		m.errMsg = err.Error()
		return m, nil
	}
	return m.startCloneTarget(t)
}

// startCloneTarget validates config and launches git clone of t.
func (m HopModel) startCloneTarget(t clone.Target) (HopModel, tea.Cmd) {
	root, err := m.cfg.RequireRoot()
	if err != nil {
		m.errMsg = err.Error() + " (set it in config.toml as root = \"...\")"
		return m, nil
	}
	dest, err := t.Dest(root)
	if err != nil {
		m.errMsg = err.Error()
		return m, nil
	}
	if _, err := os.Stat(dest); err == nil {
		m.errMsg = "already exists: " + dest + " (ctrl-r to reload the list)"
		return m, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		m.errMsg = err.Error()
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	op := m.clone.op + 1
	m.clone = cloneState{op: op, target: t, dest: dest, running: true, cancel: cancel, events: make(chan tea.Msg, 64)}
	m.log.Printf("clone %s -> %s", clone.Sanitize(t.SafeURL()), clone.Sanitize(dest))
	git, events := m.git, m.clone.events
	go func() {
		// gitx already masks and sanitizes; do it again here so no Cloner
		// implementation can push credentials or terminal control sequences
		// into the message queue.
		err := git.Clone(ctx, t.URL, dest, func(line string) {
			events <- cloneProgressMsg{op, clone.Scrub(line, t.Mask)}
		})
		if err != nil {
			err = errors.New(clone.Scrub(err.Error(), t.Mask))
		}
		events <- cloneDoneMsg{op, dest, err}
	}()
	return m, m.waitCloneEvent()
}

func (m HopModel) waitCloneEvent() tea.Cmd {
	events := m.clone.events
	return func() tea.Msg { return <-events }
}

// worktreeRepoFor returns the repository a worktree should be created from
// for a row: the repo itself, or a worktree's main repository.
func worktreeRepoFor(c hop.Candidate) (repo, label string, ok bool) {
	switch c.Kind {
	case hop.KindRepo:
		return c.Path, c.Label, true
	case hop.KindWorktree:
		if c.RepoRoot != "" {
			return c.RepoRoot, c.RepoLabel, true
		}
	}
	return "", "", false
}

// inputView renders a text input through the display boundary: a copy of
// the input is given the sanitized value and drawn, so the widget's own
// styling is kept while any control or format characters in the value
// (e.g. a remote branch name prefilled from git) cannot reach the terminal.
// The real input keeps its value: it is what git/herdr will receive.
func inputView(in textinput.Model) string {
	v := in.Value()
	safe := clone.Sanitize(v)
	if safe == v {
		return in.View()
	}
	shown := in // copy; the widget is a value type
	atEnd := in.Position() >= len([]rune(v))
	shown.SetValue(safe)
	if atEnd {
		shown.CursorEnd()
	} else {
		shown.SetCursor(min(in.Position(), len([]rune(safe))))
	}
	return shown.View()
}

func (m HopModel) View() string {
	if m.quit {
		return ""
	}
	if m.wt != nil {
		return m.viewBranch()
	}
	var b strings.Builder
	writeln := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
		}
		b.WriteByte('\n')
	}
	// Display boundary: labels (file system paths), branch names, herdr and
	// git messages all originate outside the program and are sanitized on
	// the displayed copy only; the model keeps the real values.
	disp := clone.Sanitize
	// Header and warning lines are clipped to the width like list rows: a
	// wrapped line breaks the one-line-per-row geometry of the viewport.
	clip := func(line string) string {
		if m.width > 0 {
			return ansi.Truncate(line, m.width, "…")
		}
		return line
	}
	in := m.input
	if m.confirm != nil {
		in.Prompt = m.confirmPrompt()
	}
	writeln(inputView(in))
	if m.errMsg != "" {
		writeln(styleErr.Render("error: " + disp(m.errMsg)))
		for _, p := range m.clone.progress {
			writeln("  ", styleDim.Render(disp(p)))
		}
		writeln(styleDim.Render("press any key to continue, esc to close"))
		return b.String()
	}
	if m.warn != "" {
		// One line per warning; listRows accounts for them.
		for w := range strings.SplitSeq(m.warn, "\n") {
			writeln(clip(styleWarn.Render("warning: " + disp(w))))
		}
	}
	if m.pr.running() {
		line := fmt.Sprintf("%s %s", m.pr.phase, disp(m.pr.pr.Label()))
		if m.pr.note != "" {
			line += ": " + disp(m.pr.note)
		}
		if m.pr.cancellable() {
			line += "  (esc to cancel)"
		}
		writeln(styleDim.Render(line))
		return b.String()
	}
	if m.clone.running {
		writeln(styleDim.Render(fmt.Sprintf("cloning %s -> %s", disp(m.clone.target.SafeURL()), disp(m.clone.dest))))
		for _, p := range m.clone.progress {
			writeln("  ", disp(p))
		}
		writeln(styleDim.Render("esc to cancel"))
		return b.String()
	}
	if m.pending {
		writeln(styleDim.Render("opening...")) // listRows accounts for this line
	}
	if m.loading {
		writeln(styleDim.Render("loading..."))
		return b.String()
	}
	// The counter doubles as the separator between the input and the
	// results: a rule fills the remaining width, fzf-style. The key hints
	// live under it, out of the way of the query.
	counter := fmt.Sprintf(" %d/%d ", len(m.view), len(m.cands)+len(m.extra))
	sep := styleCount.Render(counter)
	if w := m.width - lipgloss.Width(counter); w > 0 {
		sep += styleRule.Render(strings.Repeat("─", w))
	}
	writeln(sep)
	hints := []keyHint{
		{"enter", "open/switch/clone"}, {"tab", "fold"}, {"ctrl-t", "worktree"},
		{"ctrl-n", "new workspace"}, {"ctrl-d", "delete worktree"}, {"ctrl-r", "reload"}, {"esc", "close"},
	}
	if m.worktreeMode {
		hints = []keyHint{{"enter", "choose branch"}, {"tab", "fold"}, {"ctrl-d", "delete worktree"}, {"ctrl-r", "reload"}, {"esc", "close"}}
	}
	if m.confirm != nil {
		hints = []keyHint{{"y", "delete the checkout (the branch is kept)"}, {"any other key", "keep it"}}
	}
	writeln(clip(renderKeyHints(hints)))
	if m.resolvingQuery() {
		// The query names a repository or PR whose identity matching is
		// still resolving; listRows accounts for this line.
		writeln(styleDim.Render("resolving remotes..."))
	}
	if m.rowCount() == 0 {
		if !m.resolvingQuery() {
			// While resolving, "no matches" would contradict the resolving
			// line: matches may still appear.
			writeln(styleDim.Render("no matches"))
		}
		return b.String()
	}
	rows := m.listRows()
	start := m.scrollStart()
	// Two passes: the left cells of every row (not just the visible ones,
	// so scrolling never shifts the column) decide where the dim path
	// column starts, capped so pathological labels cannot push it away.
	allSegs := make([][]rowSeg, m.rowCount())
	leftW := 0
	for i := range m.view {
		allSegs[i] = m.rowSegs(i)
		leftW = max(leftW, segsWidth(allSegs[i]))
	}
	if m.width > 0 {
		leftW = min(leftW, m.width*6/10)
	}
	// The open-state glyphs form their own aligned column between the left
	// cell and the path.
	badgeW := 0
	for i := range m.view {
		b, _ := m.badge(i)
		badgeW = max(badgeW, lipgloss.Width(b))
	}
	for i := start; i < m.rowCount() && i < start+rows; i++ {
		c, _ := m.rowAt(i)
		segs := allSegs[i]
		badge, badgeStyle := m.badge(i)
		if badgeW > 0 || c.Path != "" {
			// A left cell wider than the (capped) column is clipped first,
			// so the glyph and path columns still fit on the line.
			segs = truncateSegs(segs, leftW)
			if pad := leftW - segsWidth(segs); pad > 0 {
				segs = append(segs, rowSeg{text: strings.Repeat(" ", pad)})
			}
		}
		if badgeW > 0 {
			cell := badge + strings.Repeat(" ", badgeW-lipgloss.Width(badge))
			segs = append(segs, rowSeg{text: "  "}, rowSeg{text: cell, style: badgeStyle})
		}
		if c.Path != "" {
			segs = append(segs, rowSeg{text: "  "})
			segs = append(segs, m.pathColumn(i)...)
		}
		if m.width > 2 {
			segs = truncateSegs(segs, m.width-2) // 2: the marker column
		}
		if i == m.cursor {
			// The backdrop spans the full width, so the padding carries it.
			if pad := m.width - 2 - segsWidth(segs); pad > 0 {
				segs = append(segs, rowSeg{text: strings.Repeat(" ", pad), quiet: true})
			}
			b.WriteString(styleCursor.Background(cursorRowBg).Render("> "))
			for _, s := range segs {
				if s.quiet {
					// The path column stays un-bolded but sheds its dimming:
					// the whole row brightens, which is what marks the
					// cursor on rows that are mostly path.
					b.WriteString(s.style.UnsetFaint().Background(cursorRowBg).Render(s.text))
				} else {
					b.WriteString(s.style.Bold(true).UnsetFaint().Background(cursorRowBg).Render(s.text))
				}
			}
		} else {
			b.WriteString("  ")
			for _, s := range segs {
				b.WriteString(s.style.Render(s.text))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
