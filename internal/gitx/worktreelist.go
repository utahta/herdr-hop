package gitx

import (
	"fmt"
	"strings"
)

// WorktreeListing is the parsed output of `git worktree list --porcelain -z`
// for one repository.
type WorktreeListing struct {
	// RepoRoot is the main checkout: the first entry git prints.
	RepoRoot string
	// Worktrees holds every entry, the main checkout included.
	Worktrees []WorktreeEntry
}

// WorktreeEntry is one worktree of the listing.
type WorktreeEntry struct {
	Path string
	// Branch is the short branch name; "" when the worktree is detached.
	Branch string
	// IsLinked is false for the main checkout (the first entry).
	IsLinked   bool
	IsBare     bool
	IsPrunable bool
}

// WorktreeList runs `git worktree list --porcelain -z` in repo. Any checkout
// of the repository works: a linked worktree lists its main checkout and all
// of its siblings. -z (NUL-delimited, so paths may contain newlines) and the
// prunable annotation need Git 2.36+; on older git the command itself fails
// and the error is returned — the caller treats it like any listing failure.
func (g *Git) WorktreeList(repo string) (WorktreeListing, error) {
	out, err := g.runRaw(repo, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return WorktreeListing{}, err
	}
	var listing WorktreeListing
	cur := -1 // index of the entry the attribute lines belong to
	for line := range strings.SplitSeq(out, "\x00") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "": // entry separator
			cur = -1
		case "worktree":
			listing.Worktrees = append(listing.Worktrees, WorktreeEntry{
				Path:     val,
				IsLinked: len(listing.Worktrees) > 0,
			})
			cur = len(listing.Worktrees) - 1
		case "branch":
			if cur >= 0 {
				listing.Worktrees[cur].Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "bare":
			if cur >= 0 {
				listing.Worktrees[cur].IsBare = true
			}
		case "prunable":
			if cur >= 0 {
				listing.Worktrees[cur].IsPrunable = true
			}
		}
	}
	if len(listing.Worktrees) == 0 {
		return WorktreeListing{}, fmt.Errorf("git worktree list: empty listing for %s", repo)
	}
	listing.RepoRoot = listing.Worktrees[0].Path
	return listing, nil
}
