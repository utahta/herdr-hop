package hop

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/scan"
)

// Occupant is a directory some herdr pane or workspace is in.
type Occupant struct {
	Path        string
	WorkspaceID string
	// Current marks the workspace the picker was invoked from.
	Current bool
	// Managed marks a herdr-managed worktree workspace (one `herdr worktree
	// remove` can close). A plain workspace whose pane merely sits in a
	// checkout is not one, and blocks the checkout's removal.
	Managed bool
}

// Occupancy is where herdr's panes are, taken from the raw snapshot rather
// than from the candidate rows: the rows carry one path per workspace (the
// focused pane of the active tab), while a workspace may have panes in
// other tabs and directories. It is the authority for "is anyone using
// this directory".
type Occupancy struct {
	// OK is false when the snapshot could not be read; nothing is known then.
	OK        bool
	Occupants []Occupant
	// UnknownPanes counts panes whose directory herdr did not report. Such
	// a pane may be anywhere, so while there is one no worktree can be
	// proven unused.
	UnknownPanes int
}

// OccupancyOf builds the occupancy from a snapshot: every workspace's
// checkout and every pane's cwd and foreground cwd. nil (the snapshot
// failed) gives OK == false.
func OccupancyOf(snap *herdr.Snapshot) Occupancy {
	if snap == nil {
		return Occupancy{}
	}
	occ := Occupancy{OK: true}
	focused, managed := map[string]bool{}, map[string]bool{}
	for _, ws := range snap.Workspaces {
		focused[ws.ID] = ws.Focused
		managed[ws.ID] = managedWorktreeWorkspace(ws)
		if ws.Worktree != nil && ws.Worktree.CheckoutPath != "" {
			occ.Occupants = append(occ.Occupants, Occupant{Path: scan.Normalize(ws.Worktree.CheckoutPath), WorkspaceID: ws.ID, Current: ws.Focused, Managed: managed[ws.ID]})
		}
	}
	for _, p := range snap.Panes {
		known := false
		for _, cwd := range []*string{p.Cwd, p.ForegroundCwd} {
			if cwd != nil && *cwd != "" {
				known = true
				occ.Occupants = append(occ.Occupants, Occupant{Path: scan.Normalize(*cwd), WorkspaceID: p.WorkspaceID, Current: focused[p.WorkspaceID], Managed: managed[p.WorkspaceID]})
			}
		}
		if !known {
			occ.UnknownPanes++
		}
	}
	return occ
}

// managedWorktreeWorkspace reports whether ws is a herdr-managed linked
// worktree workspace — the kind `herdr worktree remove --workspace` accepts.
func managedWorktreeWorkspace(ws herdr.Workspace) bool {
	return ws.Worktree != nil && ws.Worktree.IsLinkedWorktree
}

// CanRemove reports why Remove would refuse c, or nil. occ (OccupancyOf
// the load's snapshot) is every directory a pane or workspace is in: a
// worktree that is one of them, or contains one, is in use and refused, on
// path boundaries so /w/feat does not claim /w/feature/x — except for the
// panes of the worktree's own workspace, which herdr closes with it.
// Without the snapshot, or with a pane whose directory is unknown, nothing
// can be proven unused and every deletion is refused. The UI asks
// CanRemove before posing the confirmation question, so a refusal is shown
// at once instead of after a "yes".
func CanRemove(c Candidate, occ Occupancy) error {
	switch {
	case c.Kind != KindWorktree:
		return ErrNotWorktreeRow
	case !occ.OK:
		return ErrRemoveSnapshotUnknown
	case occ.UnknownPanes > 0:
		return fmt.Errorf("%w (%d)", ErrRemovePanesUnknown, occ.UnknownPanes)
	case c.Current:
		return ErrRemoveCurrent
	case c.OpenState == OpenUnknown:
		return ErrWorktreeStateUnknown
	case c.OpenCount > 1:
		return fmt.Errorf("%w (%d are open)", ErrRemoveShared, c.OpenCount)
	}
	for _, o := range occ.Occupants {
		if c.Path == "" || o.Path == "" {
			continue
		}
		inside := o.Path == c.Path || strings.HasPrefix(o.Path, c.Path+"/")
		if !inside {
			continue
		}
		if o.Current {
			return ErrRemoveCurrent
		}
		if c.IsOpen() && o.WorkspaceID == c.OpenWorkspaceID && o.Managed {
			continue // the worktree's own (herdr-managed) workspace goes with it
		}
		return fmt.Errorf("%w (%s)", ErrRemoveInside, o.Path)
	}
	return nil
}

// RemoveClient is what RemoveNow needs from herdr: the snapshot and
// worktree list to re-check with, and the removal itself.
type RemoveClient interface {
	Lister
	WorktreeRemover
}

// Refresh re-reads, from herdr, everything CanRemove relies on for c: the
// snapshot (occupancy, how many workspaces are on the checkout and whether
// one is the current one) and the worktree list (whether a workspace is
// open for it, and which). The rows were built when the picker opened; by
// the time the user confirms, another herdr action may have opened or
// closed a workspace on the very checkout — deciding on the old rows would
// be a check-then-act race. A snapshot failure leaves Occupancy.OK false
// and a list failure leaves the state unknown; CanRemove refuses both.
//
// The worktree list is also where the confirmed worktree is checked to be
// the one still at that path: still listed, still a linked (not prunable)
// worktree of the same repository, on the same branch. A checkout removed
// and re-created for another branch while the question was on screen is
// not what the user confirmed; ErrRemoveChanged is returned.
//
// The list's open_workspace_id names whatever workspace sits at the path,
// a plain one included (a pane that cd'ed into the checkout). Only a
// herdr-managed worktree workspace can be closed by `herdr worktree remove`,
// so the fresh row is marked open only for one of those; a plain workspace
// stays an occupant and blocks the removal (CanRemove).
func Refresh(h Lister, c Candidate) (Candidate, Occupancy, error) {
	fresh := c
	fresh.OpenCount, fresh.Current = 0, false
	fresh.OpenState, fresh.OpenWorkspaceID = OpenUnknown, ""
	snap, err := h.Snapshot()
	if err != nil {
		return fresh, Occupancy{}, nil // CanRemove: snapshot unknown
	}
	occ := OccupancyOf(snap)
	known, managed := map[string]bool{}, map[string]bool{}
	for _, ws := range snap.Workspaces {
		known[ws.ID] = true
		managed[ws.ID] = managedWorktreeWorkspace(ws)
		if workspacePath(ws, snap) == c.Path {
			fresh.OpenCount++
			fresh.Current = fresh.Current || ws.Focused
		}
	}
	l, err := h.WorktreeList(c.RepoRoot)
	if err != nil {
		return fresh, occ, nil // CanRemove: state unknown
	}
	if scan.Normalize(l.Source.RepoRoot) != c.RepoRoot {
		return fresh, occ, fmt.Errorf("%w (now listed under %s)", ErrRemoveChanged, l.Source.RepoRoot)
	}
	var entry *herdr.Worktree
	for i := range l.Worktrees {
		if scan.Normalize(l.Worktrees[i].Path) == c.Path {
			entry = &l.Worktrees[i]
			break
		}
	}
	switch {
	case entry == nil:
		return fresh, occ, fmt.Errorf("%w (no longer a worktree of this repository)", ErrRemoveChanged)
	case !entry.IsLinkedWorktree:
		return fresh, occ, fmt.Errorf("%w (it is the main checkout)", ErrRemoveChanged)
	case entry.IsPrunable:
		return fresh, occ, fmt.Errorf("%w (its directory is gone: `git worktree prune`)", ErrRemoveChanged)
	case branchOf(*entry) != c.Branch:
		// "" is a state of its own (detached): named → other, named →
		// detached and detached → named are all a different worktree.
		return fresh, occ, fmt.Errorf("%w (now on %s, confirmed %s)", ErrRemoveChanged, describeBranch(branchOf(*entry)), describeBranch(c.Branch))
	}
	fresh.OpenState = OpenClosed
	if id := entry.OpenWorkspaceID; id != nil && *id != "" {
		switch {
		case managed[*id]:
			// A herdr-managed worktree workspace: removed along with the
			// checkout, through herdr.
			fresh.OpenState, fresh.OpenWorkspaceID = OpenOpen, *id
			fresh.OpenCount = max(fresh.OpenCount, 1)
		case known[*id]:
			// A plain workspace sitting in the checkout. Refused here, not
			// left to the occupancy check: the snapshot predates the list,
			// and a pane that moved into the checkout in between is not
			// among the occupants it knows.
			return fresh, occ, fmt.Errorf("%w (workspace %s)", ErrRemoveInside, *id)
		default:
			// A workspace the snapshot has never seen: it was opened between
			// the two reads, and neither its kind nor where its panes are is
			// known. Not "closed" — undecidable; ask for a reload.
			return fresh, occ, fmt.Errorf("%w (a workspace opened on it meanwhile)", ErrRemoveChanged)
		}
	}
	return fresh, occ, nil
}

// branchOf is a listed worktree's branch in the candidates' convention:
// "" when detached, whatever herdr puts in the field then.
func branchOf(wt herdr.Worktree) string {
	if wt.IsDetached {
		return ""
	}
	return wt.Branch
}

func describeBranch(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}

// RemoveNow is Remove on freshly re-read state (Refresh): the deletion is
// decided and routed by what herdr says now, not by the rows the picker
// was opened with.
func RemoveNow(h RemoveClient, g GitWorktreeRemover, c Candidate, force bool) error {
	fresh, occ, err := Refresh(h, c)
	if err != nil {
		return err
	}
	return Remove(h, g, fresh, occ, force)
}

// Remove deletes a worktree checkout, never its branch. An open worktree
// goes through herdr (which closes the workspace as well); one no workspace
// is open for goes through git. Both refuse a checkout with modified or
// untracked files unless force is set. Refused (see CanRemove): rows that
// are not worktrees, the worktree in use by the picker's own pane, one
// whose open state is unknown, and one with several workspaces open on it
// — herdr closes only the workspace it is given, and the others would be
// left pointing at a directory that no longer exists.
func Remove(h WorktreeRemover, g GitWorktreeRemover, c Candidate, occ Occupancy, force bool) error {
	if err := CanRemove(c, occ); err != nil {
		return err
	}
	if c.IsOpen() {
		return h.WorktreeRemove(c.OpenWorkspaceID, force)
	}
	return g.WorktreeRemove(c.RepoRoot, c.Path, force)
}

// WorkspaceLabel derives the workspace label from a candidate label. Only a
// path relative to a search root in the "host/owner/repo" or "owner/repo"
// layout yields "owner/repo" (so a fork and its upstream are
// distinguishable in the workspace list). An absolute path — a repository
// outside every search root — carries no owner information, so only its
// base name is used; anything else also falls back to the last element.
func WorkspaceLabel(label string) string {
	if filepath.IsAbs(label) {
		return filepath.Base(label)
	}
	parts := strings.Split(strings.Trim(label, "/"), "/")
	switch {
	case len(parts) >= 3:
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	case len(parts) == 2:
		return label
	default:
		return path.Base(label)
	}
}

// Opener is the subset of herdr.Client needed to act on a candidate.
type Opener interface {
	WorkspaceCreate(cwd, label string) error
	WorkspaceFocus(id string) error
	WorktreeOpen(repoRoot, path string) error
	ParentRelabeler
}

// ParentRelabeler is what RelabelParent needs from herdr.
// WorktreeRemover is the herdr side of Remove: a worktree that is open as a
// workspace is removed through herdr, which also closes the workspace.
type WorktreeRemover interface {
	WorktreeRemove(workspaceID string, force bool) error
}

// GitWorktreeRemover is the git side of Remove, for worktrees no workspace
// is open for.
type GitWorktreeRemover interface {
	WorktreeRemove(repo, path string, force bool) error
}

type ParentRelabeler interface {
	WorktreeList(repo string) (*herdr.WorktreeList, error)
	WorkspaceLabel(id string) (string, error)
	WorkspaceRename(id, label string) error
}

// RelabelParent gives the parent workspace that herdr creates for a
// repository's main checkout (when a worktree is created or opened) the same
// "owner/repo" label that workspaces opened from the picker get, so both
// paths name a repository the same way. herdr labels that workspace with
// the checkout's directory name; only that default label is replaced, so a
// label the user chose by hand is left alone. Failures are returned but are
// cosmetic: the worktree itself is already open.
func RelabelParent(h ParentRelabeler, repoRoot, label string) error {
	if label == "" {
		return nil
	}
	l, err := h.WorktreeList(repoRoot)
	if err != nil {
		return err
	}
	if l.Source.SourceWorkspaceID == nil || *l.Source.SourceWorkspaceID == "" {
		return nil
	}
	id := *l.Source.SourceWorkspaceID
	current, err := h.WorkspaceLabel(id)
	if err != nil {
		return err
	}
	if current == label || current != filepath.Base(l.Source.RepoRoot) {
		return nil
	}
	return h.WorkspaceRename(id, label)
}

var (
	ErrForceNotAllowed = errors.New("force-create is only available for repo rows")
	// ErrOpenStateUnknown: herdr state was not available, so creating a
	// workspace could duplicate one. Ctrl-N (force) or reload are the ways out.
	ErrOpenStateUnknown = errors.New("open state unknown (herdr snapshot failed): ctrl-r to reload, or ctrl-n to create anyway")
	// ErrWorktreeStateUnknown: like ErrOpenStateUnknown, but worktrees have
	// no force-create escape hatch (ctrl-n is repo-only), so only a reload
	// is offered. It surfaces when `herdr worktree list` — the authority on
	// where a worktree is open — failed for the repository (Enter during the
	// provisional window queues instead of reaching Open, so a snapshot
	// failure alone never shows this).
	ErrWorktreeStateUnknown = errors.New("open state unknown (herdr worktree list failed): ctrl-r to reload")
	// ErrCloneRow: clone rows are handled by the UI (they need git), not Open.
	ErrCloneRow = errors.New("clone rows are executed by the UI")
	// ErrNotWorktreeRow is returned by Remove for anything but a worktree.
	ErrNotWorktreeRow = errors.New("only worktree rows can be deleted")
	// ErrRemoveCurrent refuses to delete the worktree the picker was
	// invoked from: the pane's directory would vanish under it.
	ErrRemoveCurrent = errors.New("this is the worktree you are in: switch elsewhere first")
	// ErrRemoveShared refuses a worktree that several workspaces are open
	// on: only one of them would be closed with it.
	ErrRemoveShared = errors.New("several workspaces are open on this worktree: close the extra ones first")
	// ErrRemoveInside refuses a worktree some other workspace's pane sits
	// in: closing it is not ours to do.
	ErrRemoveInside = errors.New("another workspace has a pane in this worktree: close it first")
	// ErrRemoveChanged refuses when the worktree at the confirmed path is
	// no longer the one that was confirmed (removed, re-created for another
	// branch, prunable, or listed under another repository).
	ErrRemoveChanged = errors.New("the worktree changed since the list was loaded: ctrl-r to reload and confirm again")
	// ErrRemovePanesUnknown refuses every deletion while some pane's
	// directory is unknown: it might be in the worktree.
	ErrRemovePanesUnknown = errors.New("cannot tell where every pane is (herdr reported no directory for some): ctrl-r to reload")
	// ErrRemoveSnapshotUnknown refuses every deletion while the herdr
	// snapshot could not be read: which worktrees are in use is unknown.
	ErrRemoveSnapshotUnknown = errors.New("cannot tell which worktrees are in use (herdr snapshot failed): ctrl-r to reload")
	// ErrKindUnknown: probable linked worktree whose worktree list failed.
	ErrKindUnknown = errors.New("cannot determine whether this is a worktree (git worktree list failed): ctrl-r to reload")
)

// Open performs the Enter action for a row:
//   - repo:      focus the open workspace, or create one with `workspace create`
//   - worktree:  focus the open workspace, or open it with `worktree open`
//   - workspace: focus it
//   - unknown / clone: not actionable here (see the returned errors)
//
// force creates a new workspace even when one is open or its state is unknown
// (repo rows only).
func Open(h Opener, c Candidate, force bool) error {
	switch c.Kind {
	case KindRepo:
		if force {
			return h.WorkspaceCreate(c.Path, WorkspaceLabel(c.Label))
		}
		switch c.OpenState {
		case OpenOpen:
			return h.WorkspaceFocus(c.OpenWorkspaceID)
		case OpenClosed:
			return h.WorkspaceCreate(c.Path, WorkspaceLabel(c.Label))
		default:
			return ErrOpenStateUnknown
		}
	case KindWorktree:
		if force {
			return ErrForceNotAllowed
		}
		if c.OpenState == OpenUnknown {
			// `herdr worktree list` is the authoritative source of a
			// worktree's open state; without it, opening could duplicate a
			// workspace.
			return ErrWorktreeStateUnknown
		}
		if c.IsOpen() {
			return h.WorkspaceFocus(c.OpenWorkspaceID)
		}
		if err := h.WorktreeOpen(c.RepoRoot, c.Path); err != nil {
			return err
		}
		// The parent workspace is named after the repository, not after the
		// worktree checkout (whose path is typically ~/.herdr/worktrees/repo/branch).
		return RelabelParent(h, c.RepoRoot, WorkspaceLabel(c.RepoLabel))
	case KindWorkspace:
		if force {
			return ErrForceNotAllowed
		}
		return h.WorkspaceFocus(c.OpenWorkspaceID)
	case KindClone, KindPull, KindClonePull, KindNote:
		return ErrCloneRow
	default:
		return ErrKindUnknown
	}
}
