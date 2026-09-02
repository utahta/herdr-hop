// Package worktree holds the branch-selection rules for creating a herdr
// worktree: which branches to offer, how to name a new one, and which
// `herdr worktree create` arguments a selection maps to. It has no I/O so
// the rules can be tested directly.
package worktree

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Branch is one selectable ref.
type Branch struct {
	// Name is "feature" for a local branch, "origin/feature" for a remote one
	// (display form).
	Name string
	// Ref is the fully qualified ref name (refs/heads/feature,
	// refs/remotes/origin/feature). It is what gets passed to git and herdr
	// as a base or upstream so that a same-named local branch or tag can
	// never make the short form ambiguous.
	Ref string
	// Remote is "origin" for remote branches, "" for local ones.
	Remote string
	// Short is the branch name without the remote prefix ("feature").
	Short string
	// WorktreePath is where a local branch is currently checked out, or "".
	// git refuses to check a branch out in a second worktree, so such a
	// branch cannot be chosen: the branch screen leaves it out of the list
	// and refuses the name with a hint at its worktree if typed.
	WorktreePath string
	// SHA is the commit the branch points at ("" when unknown).
	SHA string
	// Upstream is the upstream ref of a local branch
	// (refs/remotes/origin/feature), "" when unset or remote.
	Upstream string
	// PRs are the pull requests whose head is this commit. Filled by
	// AnnotatePRs; empty until PR heads have been fetched. Several PRs may
	// point at one commit (a reopened PR, a PR per remote), each with its
	// own label, head and details.
	PRs []BranchPR
}

// BranchPR is one pull request attached to a branch.
type BranchPR struct {
	// Label is the display form: "#123", or "upstream#123" when several
	// remotes advertise PRs.
	Label string
	// Head is the advertised head the label was derived from.
	Head PRHead
	// Info is what the forge knows, once AttachPRInfo has run; zero until
	// then (and for good when no forge can answer).
	Info PRInfo
}

// PRState is a pull request's lifecycle state as shown in the list.
type PRState string

const (
	PROpen   PRState = "open"
	PRDraft  PRState = "draft"
	PRMerged PRState = "merged"
	PRClosed PRState = "closed"
)

// PRInfo is what a forge knows about a pull request beyond its head commit.
type PRInfo struct {
	Title string
	State PRState
}

// HasInfo reports whether the forge details have been fetched.
func (p PRInfo) HasInfo() bool { return p.Title != "" || p.State != "" }

// HasPR reports whether at least one pull request points at this branch.
func (b Branch) HasPR() bool { return len(b.PRs) > 0 }

// PRLabels lists the labels of the branch's pull requests, in order.
func (b Branch) PRLabels() []string {
	labels := make([]string, len(b.PRs))
	for i, pr := range b.PRs {
		labels[i] = pr.Label
	}
	return labels
}

// Alive reports whether the PR is still going (open or draft).
func (p PRInfo) Alive() bool { return p.State == PROpen || p.State == PRDraft }

// SearchText is what the fuzzy filter matches against: the name plus the
// PR labels, so "#123" finds the branch of that PR. The PR title is not
// part of it — fuzzy matching over a sentence hits almost anything, so
// titles are searched by words instead (see the branch screen's filter).
func (b Branch) SearchText() string {
	if len(b.PRs) == 0 {
		return b.Name
	}
	return b.Name + " " + strings.Join(b.PRLabels(), " ")
}

// InUse reports whether the branch is checked out in some worktree.
func (b Branch) InUse() bool { return b.WorktreePath != "" }

// IsRemote reports whether the branch lives on a remote.
func (b Branch) IsRemote() bool { return b.Remote != "" }

// ParseRefs turns `git for-each-ref
// --format=%(refname)%09%(upstream)%09%(objectname)%09%(worktreepath)`
// output into branches: locals first (in git's order), then remotes.
// refs/remotes/*/HEAD symbolic refs are skipped — they are not branches.
//
// Each line is "<refname>\t<upstream>\t<sha>\t<worktreepath>". The worktree
// path may contain tabs while the other fields cannot, which is why it is
// the last field: the line is split on the first three tabs. Lines with
// fewer than three tabs are malformed and skipped (there is exactly one
// producer of this format, so no older layout is accepted — a legacy line
// whose path contained a tab would be indistinguishable from this one).
//
// remotes are the configured remote names. A remote name may itself contain
// "/", so refs/remotes/foo/bar/feature is split by the longest configured
// remote that prefixes it ("foo/bar" → branch "feature"). When no configured
// remote matches, the first path segment is taken as the remote.
func ParseRefs(lines []string, remotes []string) []Branch {
	var locals, remoteBranches []Branch
	for _, l := range lines {
		l, up, sha, wt, ok := splitRefLine(strings.TrimSuffix(l, "\n"))
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(l, "refs/heads/"):
			n := strings.TrimPrefix(l, "refs/heads/")
			locals = append(locals, Branch{Name: n, Ref: l, Short: n, WorktreePath: wt, SHA: sha, Upstream: up})
		case strings.HasPrefix(l, "refs/remotes/"):
			rest := strings.TrimPrefix(l, "refs/remotes/")
			remote, short := splitRemote(rest, remotes)
			if short == "" || short == "HEAD" {
				continue
			}
			remoteBranches = append(remoteBranches, Branch{Name: remote + "/" + short, Ref: l, Remote: remote, Short: short, SHA: sha})
		}
	}
	return append(locals, remoteBranches...)
}

// splitRefLine splits a for-each-ref line into refname, upstream, sha and
// worktreepath (the remainder after the third tab, since only the path may
// contain tabs). Never trims: whitespace such as U+00A0 is part of a valid
// ref name or path and must reach git unchanged.
func splitRefLine(line string) (ref, upstream, sha, worktreePath string, ok bool) {
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) < 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// PRHead is a pull request head as advertised by a remote.
type PRHead struct {
	Remote string
	Number int
	SHA    string
}

// AnnotatePRs attaches PR labels to the branches whose commit is a PR head.
// The order is unchanged: the annotation is a hint next to the branch, and
// a list that reorders itself when the (asynchronous) lookup finishes reads
// as a glitch. When PR heads come from more than one remote the labels
// carry the remote name ("upstream#123"); otherwise they are plain "#123".
// Branches with no PR get PRs = nil. The input slice is not modified.
func AnnotatePRs(branches []Branch, heads []PRHead) []Branch {
	remotes := map[string]bool{}
	for _, h := range heads {
		remotes[h.Remote] = true
	}
	multi := len(remotes) > 1
	bySHA := map[string][]BranchPR{}
	for _, h := range heads {
		if h.SHA == "" || h.Number <= 0 {
			continue
		}
		label := fmt.Sprintf("#%d", h.Number)
		if multi {
			label = h.Remote + label
		}
		bySHA[h.SHA] = append(bySHA[h.SHA], BranchPR{Label: label, Head: h})
	}
	out := make([]Branch, len(branches))
	copy(out, branches)
	for i := range out {
		out[i].PRs = nil
		if prs := bySHA[out[i].SHA]; len(prs) > 0 {
			out[i].PRs = append([]BranchPR(nil), prs...)
		}
	}
	return out
}

// AttachPRInfo fills the Info of every attached PR from lookup, which
// answers per head (by remote and number); unknown PRs keep a zero Info.
// The input slice and its PR slices are not modified.
func AttachPRInfo(branches []Branch, lookup func(PRHead) (PRInfo, bool)) []Branch {
	out := make([]Branch, len(branches))
	copy(out, branches)
	for i := range out {
		if len(out[i].PRs) == 0 {
			continue
		}
		prs := make([]BranchPR, len(out[i].PRs))
		copy(prs, out[i].PRs)
		for j := range prs {
			prs[j].Info = PRInfo{}
			if info, ok := lookup(prs[j].Head); ok {
				prs[j].Info = info
			}
		}
		out[i].PRs = prs
	}
	return out
}

// splitRemote splits "<remote>/<branch>" using the longest matching
// configured remote name; falls back to the first "/".
func splitRemote(rest string, remotes []string) (remote, short string) {
	best := ""
	for _, r := range remotes {
		if len(r) > len(best) && strings.HasPrefix(rest, r+"/") {
			best = r
		}
	}
	if best != "" {
		return best, strings.TrimPrefix(rest, best+"/")
	}
	remote, short, _ = strings.Cut(rest, "/")
	return remote, short
}

// AutoName returns the branch name used when the user gives none.
func AutoName(now time.Time) string {
	return "wt/" + now.Format("20060102-1504")
}

// Plan is what to run for a selection.
type Plan struct {
	// Branch is passed as --branch.
	Branch string
	// Creates is true when Branch is meant to be created (from Base or
	// HEAD) rather than checked out. Before running such a plan the caller
	// must confirm with git that no local branch of that name resolves
	// (see gitx.BranchExists): herdr would otherwise check the existing
	// branch out and the upstream would be applied to it.
	Creates bool
	// Base is passed as --base when non-empty. It is a fully qualified ref
	// (refs/remotes/origin/feature), never the ambiguous short form.
	Base string
	// Upstream, when non-empty, is set on Branch after creation
	// (git branch --set-upstream-to=<Upstream> <Branch>); fully qualified.
	Upstream string
}

var (
	// ErrLocalExists: a remote branch was chosen without a new name but a
	// local branch of the same name exists. herdr would silently check out
	// that local branch and we would then rewrite its upstream, so the user
	// must either pick the local row or give a new name.
	ErrLocalExists = errors.New("a local branch with that name already exists: choose the local branch, or enter a new branch name")
	// ErrNameTaken: the requested new branch name is already a local branch.
	ErrNameTaken = errors.New("branch name is already taken by a local branch")
	// ErrBadName: the name is not a valid git branch name.
	ErrBadName = errors.New("invalid branch name")
	// ErrInUse: the local branch is already checked out in another worktree.
	ErrInUse = errors.New("branch is already checked out in another worktree")
)

// reBadName rejects what git check-ref-format forbids anywhere in a name:
// control chars, spaces, ~ ^ : ? * [ \, "..", "@{", a leading "-", a
// trailing "/", empty components ("//"), and a lone "@".
var reBadName = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[\\]|\.\.|@\{|^-|/$|^/|//|^@$`)

// ValidName reports whether name is acceptable as a new branch name. Besides
// the whole-name rules, every "/"-separated component must not start with
// "." nor end with ".lock" (so "foo/.bar" and "foo.lock/bar" are rejected,
// as git would reject them later), and no component may end with ".".
func ValidName(name string) bool {
	if name == "" || name == "HEAD" || reBadName.MatchString(name) {
		return false // "HEAD" is git's pseudo-ref, never a branch name
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Make maps a selection to a Plan, applying the pre-conditions:
//
//   - local branch, no new name:      --branch <local>                                (checkout)
//   - remote branch, no new name:     --branch <short> --base refs/remotes/<remote>/<short>, then set upstream
//     (only if no local <short> exists and <short> is a valid local name)
//   - remote branch, new name:        --branch <new> --base refs/remotes/<remote>/<short> (no upstream)
//   - no branch, new name / auto:     --branch <new>                                   (from HEAD)
//
// locals is the set of existing local branch names. A nil selected branch
// means "create from HEAD"; an empty newName then means auto-generate.
// newName is used exactly as given (no trimming): whitespace such as U+00A0
// can be part of a valid name, and ASCII whitespace makes it invalid — in
// both cases silently altering the name would create a different branch.
// Only the empty string means "auto-generate".
func Make(selected *Branch, newName string, locals map[string]bool, now time.Time) (Plan, error) {
	if selected == nil {
		if newName == "" {
			newName = AutoName(now)
		}
		if !ValidName(newName) {
			return Plan{}, ErrBadName
		}
		if locals[newName] {
			return Plan{}, ErrNameTaken
		}
		return Plan{Branch: newName, Creates: true}, nil
	}
	if !selected.IsRemote() {
		if selected.InUse() {
			return Plan{}, fmt.Errorf("%w: %s", ErrInUse, selected.WorktreePath)
		}
		return Plan{Branch: selected.Name}, nil
	}
	if newName == selected.Short {
		newName = "" // same name as the remote branch: track it
	}
	if newName == "" {
		if locals[selected.Short] {
			return Plan{}, ErrLocalExists
		}
		// The remote's short name is about to become a local branch name;
		// it may be valid on the remote side but not as typed here (e.g.
		// "-release"), so validate it like any other new name.
		if !ValidName(selected.Short) {
			return Plan{}, fmt.Errorf("%w: %q (enter a different local name)", ErrBadName, selected.Short)
		}
		return Plan{Branch: selected.Short, Base: selected.Ref, Upstream: selected.Ref, Creates: true}, nil
	}
	if !ValidName(newName) {
		return Plan{}, ErrBadName
	}
	if locals[newName] {
		return Plan{}, fmt.Errorf("%w: %s", ErrNameTaken, newName)
	}
	return Plan{Branch: newName, Base: selected.Ref, Creates: true}, nil
}

// RefspecsCover reports whether a remote's fetch refspecs map srcRef (e.g.
// refs/heads/feature) onto exactly dstRef (e.g.
// refs/remotes/origin/feature). git only treats a remote-tracking ref as an
// upstream when the remote's configured fetch mapping produces it, so all
// of the following must NOT count as covered:
//
//   - a single-branch clone whose mapping omits the branch
//   - a mapping onto some other namespace (refs/remotes/custom/*)
//   - a source-only refspec ("refs/heads/feature": no destination, no
//     persistent remote-tracking ref)
//   - a source matched by a negative refspec ("^refs/heads/feature")
func RefspecsCover(refspecs []string, srcRef, dstRef string) bool {
	covered := false
	for _, spec := range refspecs {
		// Byte-for-byte: whitespace such as a trailing U+00A0 can be part of
		// a valid ref name, and trimming it would turn a negative refspec
		// into a different ref (and mis-cover the branch it excludes).
		if neg, ok := strings.CutPrefix(spec, "^"); ok {
			// Negative refspecs are source-only patterns; one match excludes.
			if matchGlob(neg, srcRef) != nil {
				return false
			}
			continue
		}
		spec = strings.TrimPrefix(spec, "+")
		src, dst, ok := strings.Cut(spec, ":")
		if !ok || dst == "" {
			continue // no destination: fetches into FETCH_HEAD only
		}
		wild := matchGlob(src, srcRef)
		if wild == nil {
			continue
		}
		// Expand the destination with the part the source wildcard matched.
		if strings.Replace(dst, "*", *wild, 1) == dstRef {
			covered = true // keep scanning: a later negative may still exclude
		}
	}
	return covered
}

// matchGlob matches ref against a refspec pattern with at most one "*" and
// returns what the "*" matched (empty string for a literal match), or nil.
func matchGlob(pattern, ref string) *string {
	pre, post, ok := strings.Cut(pattern, "*")
	if !ok {
		if pattern == ref {
			empty := ""
			return &empty
		}
		return nil
	}
	if strings.HasPrefix(ref, pre) && strings.HasSuffix(ref[len(pre):], post) {
		mid := ref[len(pre) : len(ref)-len(post)]
		return &mid
	}
	return nil
}

// Locals returns the set of local branch names.
func Locals(bs []Branch) map[string]bool {
	m := map[string]bool{}
	for _, b := range bs {
		if !b.IsRemote() {
			m[b.Name] = true
		}
	}
	return m
}
