// Package gitx runs the git CLI.
//
// Repository-dependent commands always take the repository directory
// explicitly: herdr starts plugin processes in the plugin's own directory,
// never in a repository, so a bare "git ..." would operate on the wrong tree.
//
// Output from git that is passed upward (clone progress, error messages) has
// any credentials in the clone URL masked here, at the boundary, so callers
// can log or display it. The masking itself lives in package clone so the UI
// can apply the identical rules as a second layer.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/utahta/herdr-hop/internal/clone"
)

// Git executes git commands.
type Git struct {
	Bin string
}

// New returns a Git using "git" from PATH.
func New() *Git { return &Git{Bin: "git"} }

// Run executes git in repo and returns trimmed stdout. repo must be non-empty.
//
// On failure the error carries git's stderr, passed through sanitizeOutput
// so that it can be shown or logged: terminal control sequences are removed
// and, when the caller supplies the URLs the command may echo (see
// RunMasked), credentials in them are masked.
func (g *Git) Run(repo string, args ...string) (string, error) {
	return g.RunMasked(repo, nil, args...)
}

// WorktreeRemove removes the linked worktree at path from the repository at
// repo (`git worktree remove`). git refuses a checkout with modified or
// untracked files unless force is set. The branch is kept.
func (g *Git) WorktreeRemove(repo, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", path)
	_, err := g.Run(repo, args...)
	return err
}

// RunMasked is Run with a list of URLs whose credentials must not appear in
// the returned error (e.g. the repository's remotes for a fetch).
func (g *Git) RunMasked(repo string, urls []string, args ...string) (string, error) {
	out, err := g.runRaw(repo, urls, args...)
	return strings.TrimSpace(out), err
}

// RunCtx is RunMasked with a context: cancelling ctx terminates git and its
// whole process group (ssh and other helpers included), and the call is
// guaranteed to return shortly afterwards. Use it for anything that talks to
// the network (fetch, ls-remote) so a timeout or a cancelled UI action never
// leaves processes behind. stdout is returned untrimmed.
func (g *Git) RunCtx(ctx context.Context, repo string, urls []string, args ...string) (string, error) {
	return g.runCtx(ctx, repo, urls, false, args...)
}

// runCtxQuiet is RunCtx for background work that must never prompt, in any
// form:
//
//   - GIT_TERMINAL_PROMPT=0 disables git's own terminal prompts
//   - detaching from the controlling terminal (Setsid) keeps ssh away from
//     /dev/tty
//   - DISPLAY and SSH_ASKPASS are removed and SSH_ASKPASS_REQUIRE=never is
//     set, so ssh cannot fall back to a graphical askpass dialog (key
//     passphrase, host-key confirmation) either
//   - GIT_ASKPASS=false blocks git's graphical credential prompt while
//     credential helpers (cached, non-interactive) keep working
//
// Anything that would need to ask fails immediately instead of blocking the
// UI behind an invisible prompt until the timeout. Explicit, user-initiated
// operations (fetch, clone, the route decision) keep normal behaviour.
func (g *Git) runCtxQuiet(ctx context.Context, repo string, urls []string, args ...string) (string, error) {
	return g.runCtx(ctx, repo, urls, true, args...)
}

// quietEnv is the environment for runCtxQuiet.
func quietEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "DISPLAY", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "GIT_ASKPASS":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS_REQUIRE=never", "GIT_ASKPASS=false")
}

func (g *Git) runCtx(ctx context.Context, repo string, urls []string, quiet bool, args ...string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("gitx: repository directory is required")
	}
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = repo
	var killer *groupKiller
	if quiet {
		cmd.Env = quietEnv()
		killer = setupProcessGroupDetached(cmd)
	} else {
		killer = setupProcessGroup(cmd)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	killer.done()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, sanitizeOutput(stderr.String(), urls))
	}
	return stdout.String(), nil
}

// FetchRefspecs returns remote.<name>.fetch of repo (empty when none),
// byte-for-byte: a refspec may name refs that end in whitespace, so the
// output is read untrimmed and split on newlines only.
func (g *Git) FetchRefspecs(repo, name string) ([]string, error) {
	out, err := g.runRaw(repo, nil, "config", "--get-all", "--", "remote."+name+".fetch")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil // unset
		}
		return nil, err
	}
	var specs []string
	for l := range strings.SplitSeq(out, "\n") {
		if l != "" {
			specs = append(specs, l)
		}
	}
	return specs, nil
}

// ConfigGet returns a git config value of repo, "" when unset.
func (g *Git) ConfigGet(repo, key string) (string, error) {
	out, err := g.Run(repo, "config", "--get", "--", key)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil // unset
		}
		return "", err
	}
	return out, nil
}

// ConfigSet sets a git config value in repo.
func (g *Git) ConfigSet(repo, key, value string) error {
	_, err := g.Run(repo, "config", "--", key, value)
	return err
}

// Remote is a configured remote and the URL `git fetch <name>` uses (the
// first configured URL; a remote may list several, but only the first is
// used for fetching).
type Remote struct {
	Name string
	URL  string
}

// RemoteFetchURLs returns every remote of repo with its fetch URL, in
// remote-name order, using a single git invocation — the picker resolves
// remotes for every scanned repository, where one process per remote
// (Remotes + FetchURL each) dominates the load time. A repository without
// remotes yields nil. Cancelling ctx terminates git; the picker resolves in
// the background and cancels on reload and exit.
//
// The URLs come from `git remote -v`, not from reading remote.<name>.url out
// of the config: only git itself applies url.<base>.insteadOf rewriting, and
// the identity of a checkout must be judged by the URL git actually fetches
// from. -v prints exactly one fetch line per remote, carrying the first
// configured URL — the one `git fetch` uses.
func (g *Git) RemoteFetchURLs(ctx context.Context, repo string) ([]Remote, error) {
	out, err := g.runCtx(ctx, repo, nil, false, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var remotes []Remote
	hasOrigin := false
	for line := range strings.SplitSeq(out, "\n") {
		name, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		// The fetch line may carry annotations after the marker (a partial
		// clone prints "… (fetch) [blob:none]"), so the marker is not
		// necessarily the suffix.
		url, note, ok := strings.Cut(rest, " (fetch)")
		if !ok || url == "" || (note != "" && !strings.HasPrefix(note, " ")) {
			continue
		}
		hasOrigin = hasOrigin || name == "origin"
		remotes = append(remotes, Remote{Name: name, URL: url})
	}
	// `git remote -v` does not list remotes in the legacy .git/remotes/<name>
	// file format, but `git fetch origin` and `git remote get-url origin`
	// still resolve them. Ask for origin explicitly when -v did not have it,
	// so such a checkout keeps its identity; the extra process only runs for
	// repositories without an origin.
	if !hasOrigin {
		if u, err := g.runCtx(ctx, repo, nil, false, "remote", "get-url", "--", "origin"); err == nil {
			if u = strings.TrimSpace(u); u != "" {
				remotes = append(remotes, Remote{Name: "origin", URL: u})
			}
		}
	}
	return remotes, nil
}

// FetchURL returns the URL `git fetch <name>` actually connects to: the
// first configured URL of the remote (a remote may list several, but only
// the first is used for fetching). "" when the remote has none.
func (g *Git) FetchURL(repo, name string) (string, error) {
	out, err := g.Run(repo, "remote", "get-url", "--", name)
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// maskURLsFor collects everything that must be masked from the output of a
// network command against src: the repository's remote URLs, src as given,
// and the URL git actually uses after url.<base>.insteadOf rewriting —
// which may inject credentials into an innocent-looking URL, and which git
// and ssh echo in their errors. `ls-remote --get-url` resolves the rewrite
// without touching the network; failures fall back to src alone.
func (g *Git) maskURLsFor(repo, src string) ([]string, error) {
	urls, err := g.RemoteURLs(repo)
	if err != nil {
		return nil, err
	}
	urls = append(urls, src)
	if eff, err := g.Run(repo, "ls-remote", "--get-url", "--", src); err == nil && eff != "" && eff != src {
		urls = append(urls, eff)
	}
	return urls, nil
}

// FetchRef fetches refspec from remoteOrURL into repo. The URL as given,
// its insteadOf-rewritten form, and the remote's URLs are masked from any
// error text.
func (g *Git) FetchRef(ctx context.Context, repo, remoteOrURL, refspec string) error {
	urls, err := g.maskURLsFor(repo, remoteOrURL)
	if err != nil {
		return err
	}
	_, err = g.RunCtx(ctx, repo, urls, "fetch", "--no-tags", "--", remoteOrURL, refspec)
	return err
}

// RemoteState is what LsRemotePR learns about a remote repository in one
// round trip: the pull request head, every branch head, and which branch is
// the default (HEAD).
type RemoteState struct {
	// HeadSHA is the commit of the requested PR head ref ("" when the
	// remote does not advertise it).
	HeadSHA string
	// Branches maps short branch names to their commit.
	Branches map[string]string
	// DefaultBranch is the short name HEAD points at ("" when unknown).
	DefaultBranch string
}

// LsRemotePR asks src (a remote name or URL) for the PR head, all branch
// heads and the default branch in a single round trip, so the values are a
// consistent snapshot: a branch whose head equals the PR head at this
// moment IS the PR's source branch, regardless of local fetch state.
func (g *Git) LsRemotePR(ctx context.Context, repo, src, headRef string) (RemoteState, error) {
	urls, err := g.maskURLsFor(repo, src)
	if err != nil {
		return RemoteState{}, err
	}
	out, err := g.RunCtx(ctx, repo, urls, "ls-remote", "--symref", "--", src, "HEAD", headRef, "refs/heads/*")
	if err != nil {
		return RemoteState{}, err
	}
	st := RemoteState{Branches: map[string]string{}}
	for line := range strings.SplitSeq(out, "\n") {
		left, right, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(left, "ref: refs/heads/") && right == "HEAD":
			st.DefaultBranch = strings.TrimPrefix(left, "ref: refs/heads/")
		case right == headRef:
			st.HeadSHA = left
		case strings.HasPrefix(right, "refs/heads/"):
			st.Branches[strings.TrimPrefix(right, "refs/heads/")] = left
		}
	}
	return st, nil
}

// PRRef is one pull/merge request head advertised by a remote.
type PRRef struct {
	Remote string
	Number int
	SHA    string
}

// LsRemotePRs asks remote for the heads of its pull requests (GitHub:
// refs/pull/N/head) and merge requests (GitLab: refs/merge-requests/N/head)
// in one round trip. Hosts that have neither simply return nothing. An
// error means the query failed (as opposed to "no PRs"); the caller decides
// whether that is worth reporting. mask lists the URLs whose credentials
// must not appear in the error (the repository's remote URLs); the caller
// has them from RemoteFetchURLs, so they are not read again here — every
// git process costs the same fixed spawn time, which adds up per remote.
func (g *Git) LsRemotePRs(ctx context.Context, repo, remote string, mask []string) ([]PRRef, error) {
	out, err := g.runCtxQuiet(ctx, repo, mask, "ls-remote", "--refs", "--", remote, "refs/pull/*/head", "refs/merge-requests/*/head")
	if err != nil {
		return nil, err
	}
	var refs []PRRef
	for line := range strings.SplitSeq(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		var n int
		switch {
		case strings.HasPrefix(ref, "refs/pull/") && strings.HasSuffix(ref, "/head"):
			n, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(ref, "refs/pull/"), "/head"))
		case strings.HasPrefix(ref, "refs/merge-requests/") && strings.HasSuffix(ref, "/head"):
			n, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(ref, "refs/merge-requests/"), "/head"))
		}
		if n > 0 && sha != "" {
			refs = append(refs, PRRef{Remote: remote, Number: n, SHA: sha})
		}
	}
	return refs, nil
}

// runRaw is RunMasked without trimming stdout. Machine-readable output that
// may legitimately end in whitespace (ref names can contain U+00A0 and
// similar) must be read through this so nothing is stripped from it.
func (g *Git) runRaw(repo string, urls []string, args ...string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("gitx: repository directory is required")
	}
	cmd := exec.Command(g.Bin, args...)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, sanitizeOutput(stderr.String(), urls))
	}
	return stdout.String(), nil
}

// sanitizeOutput is the single treatment for git/ssh output that will be
// displayed or logged: terminal control sequences are removed, credentials
// of the given URLs are masked (raw and decoded userinfo, any host case),
// and the result is sanitized again (see clone.Scrub for the order). Multi-line output is handled line
// by line so that the sanitizer's per-line rules apply; trailing whitespace
// and empty lines are dropped.
func sanitizeOutput(s string, urls []string) string {
	// Register each URL twice: as configured and after sanitizing. The
	// output is sanitized before masking (see clone.Scrub), so a credential
	// that contains control sequences in the configured URL only matches
	// its sanitized spelling.
	maskers := make([]func(string) string, 0, 2*len(urls))
	for _, u := range urls {
		maskers = append(maskers, clone.NewMasker(u))
		if su := clone.Sanitize(u); su != u {
			maskers = append(maskers, clone.NewMasker(su))
		}
	}
	var out []string
	for line := range strings.SplitSeq(strings.ReplaceAll(s, "\r", "\n"), "\n") {
		line = strings.TrimSpace(clone.Scrub(line, maskers...))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, " | ")
}

// Remotes returns the configured remote names of repo, byte-for-byte: a
// remote name may end in whitespace such as U+00A0, and trimming it would
// make ParseRefs mis-split that remote's refs.
func (g *Git) Remotes(repo string) ([]string, error) {
	out, err := g.runRaw(repo, nil, "remote")
	if err != nil {
		return nil, err
	}
	var names []string
	for n := range strings.SplitSeq(out, "\n") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// RemoteURLs returns every URL git will actually use for repo's remotes
// (fetch and push), so that output of commands that talk to remotes can be
// masked. `git remote get-url --all` is used rather than the raw config
// because url.<base>.insteadOf rewrites may expand a harmless-looking
// configured URL ("gh:o/r") into one carrying credentials, and git prints
// the expanded form in its errors. The raw configured values are included
// as well. A repo without remotes yields nil.
func (g *Git) RemoteURLs(repo string) ([]string, error) {
	names, err := g.Remotes(repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var urls []string
	add := func(out string) {
		for u := range strings.SplitSeq(out, "\n") {
			if u = strings.TrimSpace(u); u != "" && !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	for _, n := range names {
		// "--" ends option parsing: a remote named "-x" must not be taken
		// for an option, or its effective URL would silently go unmasked.
		if out, err := g.Run(repo, "remote", "get-url", "--all", "--", n); err == nil {
			add(out)
		}
		if out, err := g.Run(repo, "remote", "get-url", "--push", "--all", "--", n); err == nil {
			add(out)
		}
	}
	if out, err := g.Run(repo, "config", "--get-regexp", `^remote\..*\.(url|pushurl)$`); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if _, u, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				add(u)
			}
		}
	}
	return urls, nil
}

// Refs lists local and remote branch refs of repo, one per line as
// "<refname>\t<upstream>\t<sha>\t<worktreepath>": the full ref name, the
// upstream ref of a local branch (refs/remotes/..., empty when unset), the
// commit it points at, and, for a local branch checked out in some
// worktree, that worktree's path. The worktree path may itself contain
// tabs, which is why it is the last field: consumers split on the first
// three tabs. `git branch -a` is avoided because its output is meant for
// humans (e.g. "origin/HEAD -> origin/main").
//
// Ref names are returned byte-for-byte: only the record separator ("\n")
// is removed, never whitespace, since e.g. a trailing U+00A0 is part of a
// valid ref name and trimming it would make the caller act on a different ref.
func (g *Git) Refs(repo string) ([]string, error) {
	out, err := g.runRaw(repo, nil, "for-each-ref", "--format=%(refname)%09%(upstream)%09%(objectname)%09%(worktreepath)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// BranchExists asks git whether a local branch of that exact name resolves
// in repo. This is the authoritative check before creating a branch: a Go
// map of names cannot know that on a case-insensitive file system
// refs/heads/Feature resolves to an existing refs/heads/feature.
//
// Only exit status 0 (exists) and 1 (does not exist) are answers; any other
// status, or git failing to start, is returned as an error so that a broken
// check can never be mistaken for "the branch does not exist".
func (g *Git) BranchExists(repo, name string) (bool, error) {
	_, err := g.Run(repo, "show-ref", "--verify", "--quiet", "--", "refs/heads/"+name)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// Fetch runs `git fetch --all --prune` in repo. Its error output may echo
// remote URLs (and ssh diagnostics with user@host), so the repository's
// remote URLs are masked from the returned error.
func (g *Git) Fetch(repo string) error {
	urls, err := g.RemoteURLs(repo)
	if err != nil {
		return err
	}
	_, err = g.RunMasked(repo, urls, "fetch", "--all", "--prune")
	return err
}

// SetUpstream points branch at upstream (e.g. "origin/feature").
func (g *Git) SetUpstream(repo, branch, upstream string) error {
	_, err := g.Run(repo, "branch", "--set-upstream-to="+upstream, branch)
	return err
}

// Clone runs `git clone --progress url dest`, streaming progress lines to
// progress (may be nil). It is not repository-dependent, so it has no Dir.
//
// Cancelling ctx terminates git and every helper it spawned, and Clone is
// guaranteed to return shortly afterwards (see setupProcessGroup).
func (g *Git) Clone(ctx context.Context, url, dest string, progress func(line string)) error {
	cmd := exec.CommandContext(ctx, g.Bin, "clone", "--progress", url, dest)
	killer := setupProcessGroup(cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	mask := clone.NewMasker(url)
	if su := clone.Sanitize(url); su != url {
		raw := mask
		sanitizedMask := clone.NewMasker(su)
		mask = func(s string) string { return sanitizedMask(raw(s)) }
	}
	// Read stderr concurrently so that Wait (which enforces WaitDelay and
	// closes the pipe) is never blocked behind the reader. The pipe is always
	// drained to EOF: if the reader stopped early, a helper writing a lot of
	// stderr would block on a full pipe and never exit, and Clone would hang.
	// Lines are read as bounded fragments (progressLineLimit) so that an
	// unbounded line cannot grow memory; the excess is discarded.
	var last []string
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer io.Copy(io.Discard, stderr) // drain whatever the loop did not consume
		for line := range progressLines(stderr) {
			// clone.Scrub sanitizes, masks, and sanitizes again: a control
			// sequence inside a credential must not defeat the mask.
			line = strings.TrimSpace(clone.Scrub(line, mask))
			if line == "" {
				continue
			}
			last = append(last, line)
			if len(last) > 5 {
				last = last[1:]
			}
			if progress != nil {
				progress(line)
			}
		}
	}()

	waitErr := cmd.Wait()
	// The git leader is reaped, but helpers may remain in the process group.
	// groupKiller.done keeps escalation active while the group still exists
	// and only stands down once the whole group has gone.
	killer.done()
	<-readDone
	if ctx.Err() != nil {
		return fmt.Errorf("git clone: %w", ctx.Err())
	}
	if waitErr != nil {
		return fmt.Errorf("git clone: %w: %s", waitErr, strings.Join(last, " | "))
	}
	return nil
}

// progressLineLimit caps how much of one stderr line is kept for display.
// Anything beyond it is read and discarded, never buffered.
const progressLineLimit = 8 * 1024

// progressLines yields git's stderr split on '\n' or '\r' (git rewrites
// progress lines with CR). Each yielded line is at most progressLineLimit
// bytes; the rest of an over-long line is consumed and dropped. The reader
// is always consumed to EOF or error, so the pipe never backs up.
func progressLines(r io.Reader) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		br := bufio.NewReader(r)
		var buf []byte
		discarding := false
		flush := func() bool {
			line := string(buf)
			buf = buf[:0]
			discarding = false
			return yield(line)
		}
		stopped := false
		for {
			chunk, err := br.ReadSlice('\n')
			// ReadSlice returns data up to and including the delimiter, or the
			// buffer content with bufio.ErrBufferFull, or a final chunk with io.EOF.
			for len(chunk) > 0 {
				if i := bytes.IndexByte(chunk, '\r'); i >= 0 && !(i == len(chunk)-1 && chunk[i] == '\n') {
					if !stopped && !discarding {
						buf = appendBounded(buf, chunk[:i], &discarding)
					}
					if !stopped && !flush() {
						stopped = true
					}
					chunk = chunk[i+1:]
					continue
				}
				if chunk[len(chunk)-1] == '\n' {
					if !stopped && !discarding {
						buf = appendBounded(buf, chunk[:len(chunk)-1], &discarding)
					}
					if !stopped && !flush() {
						stopped = true
					}
					chunk = nil
					continue
				}
				if !stopped && !discarding {
					buf = appendBounded(buf, chunk, &discarding)
				}
				chunk = nil
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if !stopped && len(buf) > 0 {
					flush()
				}
				return
			}
		}
	}
}

// appendBounded appends src to dst up to progressLineLimit; once the limit
// is hit, discarding is set and nothing more is kept for this line.
func appendBounded(dst, src []byte, discarding *bool) []byte {
	room := progressLineLimit - len(dst)
	if room <= 0 {
		*discarding = true
		return dst
	}
	if len(src) > room {
		*discarding = true
		src = src[:room]
	}
	return append(dst, src...)
}
