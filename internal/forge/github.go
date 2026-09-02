// Package forge talks to code-hosting services for what git itself cannot
// tell: pull request titles and states. The only implementation shells out
// to the GitHub CLI (gh), so authentication, enterprise hosts and proxies
// are gh's business, not ours.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/worktree"
)

// ErrUnsupportedHost is returned for hosts gh has no account for (GitLab,
// or a GitHub Enterprise host the user never ran `gh auth login` against).
var ErrUnsupportedHost = errors.New("no gh account for this host")

// ErrInsecureEndpoint is returned for repositories whose API would be
// reached over plain HTTP. gh attaches the account's OAuth token to every
// request; that a git remote is served over HTTP does not make it
// acceptable to send an API token, which usually holds far broader rights,
// in the clear.
var ErrInsecureEndpoint = errors.New("API endpoint is not https; refusing to send a token over plain HTTP")

// enterpriseTokenVars are the environment variables gh reads as a token for
// *any* host other than github.com — not for one particular enterprise host.
// With one of them set, `gh auth token --hostname whatever.example` succeeds
// and `gh api --hostname whatever.example` sends that token there. Since the
// hosts asked about here come from git remotes (anything a repository's
// config says), the variables are withheld from gh unless the host is the
// one the user named in GH_HOST.
var enterpriseTokenVars = []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// GitHub fetches pull request details through gh.
type GitHub struct {
	gh string // path of the gh binary
	// Log, when set, receives one line per gh invocation with its duration
	// and outcome, so slow or cancelled fetches can be told apart.
	Log *log.Logger

	mu    sync.Mutex
	hosts map[string]bool // host -> gh has a token for it
}

// NewGitHub returns a GitHub client, or nil when gh is not installed.
func NewGitHub() *GitHub {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	return &GitHub{gh: path, hosts: map[string]bool{}}
}

// prsPerQuery bounds the aliases in one GraphQL query. GitHub's node limit
// is far higher; measured against a repository with 61 PR-bearing
// branches, one query of 61 aliases took the same ~0.7s as one of 50, so
// the batch is sized to cover such repositories in a single round trip
// while keeping a failure's blast radius bounded.
const prsPerQuery = 100

// PullRequests returns title and state for the given PR numbers of repo.
// Numbers the forge does not know are missing from the result;
// ErrUnsupportedHost is returned when gh has no account for the host.
func (g *GitHub) PullRequests(ctx context.Context, repo clone.ForgeRepo, numbers []int) (map[int]worktree.PRInfo, error) {
	if repo.Host == "" || repo.Owner == "" || repo.Name == "" {
		return nil, fmt.Errorf("forge: incomplete repository %+v", repo)
	}
	if repo.Scheme != "https" {
		return nil, ErrInsecureEndpoint
	}
	// gh keeps accounts per host name; scheme and port only say where to
	// connect (see query).
	start := time.Now()
	known, err := g.knowsHost(ctx, repo.Host)
	// Log lines carry the repository name (URL-decoded from the remote,
	// so it may hold anything) and gh's stderr: both are sanitized so a
	// crafted remote cannot forge log lines.
	name := clone.Sanitize(repo.String())
	g.logf("forge %s: auth check %v known=%v err=%s", name, time.Since(start).Round(time.Millisecond), known, errText(err))
	if err != nil {
		return nil, err
	} else if !known {
		return nil, ErrUnsupportedHost
	}
	nums := append([]int(nil), numbers...)
	sort.Ints(nums)
	out := map[int]worktree.PRInfo{}
	batches := (len(nums) + prsPerQuery - 1) / prsPerQuery
	for i := 0; i*prsPerQuery < len(nums); i++ {
		lo, hi := i*prsPerQuery, min((i+1)*prsPerQuery, len(nums))
		before, bstart := len(out), time.Now()
		err := g.query(ctx, repo, nums[lo:hi], out)
		g.logf("forge %s: batch %d/%d: %d PRs in %v, %d answered, err=%s, ctx=%s",
			name, i+1, batches, hi-lo, time.Since(bstart).Round(time.Millisecond), len(out)-before, errText(err), ctxReason(ctx))
		if err != nil {
			return out, err
		}
	}
	g.logf("forge %s: %d PRs in %d batches, %d answered, total %v", name, len(nums), batches, len(out), time.Since(start).Round(time.Millisecond))
	return out, nil
}

// errText renders an error for a log line, sanitized ("<nil>" for none).
func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return clone.Sanitize(err.Error())
}

func (g *GitHub) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log.Printf(format, args...)
	}
}

// ctxReason names why a context ended, for logs: a kill by cancellation (a
// newer fetch replaced this one, the screen was left) and one by the
// deadline both surface from gh as "signal: killed".
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

// knowsHost reports whether gh holds a token for host: (true, nil) when it
// does, (false, nil) when gh says there is none, and (false, err) when the
// question could not be answered — the context ended (the screen was left,
// a refresh superseded the fetch) or gh failed for another reason (a
// keyring problem, a crash), which the caller should surface rather than
// mistake for "not logged in". `gh auth token` is local (no network); only
// the definite answers are cached, for the client's lifetime.
func (g *GitHub) knowsHost(ctx context.Context, host string) (bool, error) {
	g.mu.Lock()
	known, seen := g.hosts[host]
	g.mu.Unlock()
	if seen {
		return known, nil
	}
	cmd := exec.CommandContext(ctx, g.gh, "auth", "token", "--hostname", host)
	cmd.Stdin = nil
	cmd.Env = ghEnv(host)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	switch {
	case err == nil:
		known = true
	case noTokenMessage(stderr.String()):
		known = false
	default:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Errorf("gh auth token --hostname %s: %s", host, firstLine(msg))
	}
	g.mu.Lock()
	g.hosts[host] = known
	g.mu.Unlock()
	return known, nil
}

// noTokenMessage recognises gh's "no oauth token found for <host>" (and the
// wording of older releases) — the one failure that means "not logged in".
func noTokenMessage(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no oauth token") || strings.Contains(s, "not logged in")
}

// prNode mirrors the fields requested per pull request.
type prNode struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"` // OPEN, CLOSED, MERGED
	IsDraft bool   `json:"isDraft"`
}

// query asks for one batch of PRs by number, each under its own alias, and
// merges the answers into out. GraphQL reports an unknown number as a null
// node plus an entry in "errors" (and gh then exits non-zero), so the body
// is decoded whenever it holds data; only a body without data is an error.
//
// Owner and name are written into the query as string literals rather than
// passed as variables: gh nests -f fields under "variables" only for the
// `graphql` shorthand endpoint, not for a full URL, and a full URL is what
// a host on plain HTTP or a non-default port needs (--hostname takes a bare
// host name, which selects the account; the URL says where to connect).
// One request shape thus serves both.
func (g *GitHub) query(ctx context.Context, repo clone.ForgeRepo, numbers []int, out map[int]worktree.PRInfo) error {
	var q strings.Builder
	fmt.Fprintf(&q, "{repository(owner:%s,name:%s){", graphqlString(repo.Owner), graphqlString(repo.Name))
	for _, n := range numbers {
		fmt.Fprintf(&q, "p%d:pullRequest(number:%d){number title state isDraft} ", n, n)
	}
	q.WriteString("}}")
	args := []string{"api", "--hostname", repo.Host, endpoint(repo), "-f", "query=" + q.String()}
	cmd := exec.CommandContext(ctx, g.gh, args...)
	cmd.Stdin = nil
	cmd.Env = ghEnv(repo.Host)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var body struct {
		Data struct {
			Repository map[string]*prNode `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &body); err != nil || body.Data.Repository == nil {
		if runErr != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = runErr.Error()
			}
			return fmt.Errorf("gh api graphql: %s", firstLine(msg))
		}
		return fmt.Errorf("gh api graphql: unexpected response")
	}
	for _, node := range body.Data.Repository {
		if node == nil || node.Number <= 0 {
			continue
		}
		out[node.Number] = worktree.PRInfo{Title: node.Title, State: stateOf(node)}
	}
	return nil
}

func stateOf(n *prNode) worktree.PRState {
	switch n.State {
	case "MERGED":
		return worktree.PRMerged
	case "CLOSED":
		return worktree.PRClosed
	default:
		if n.IsDraft {
			return worktree.PRDraft
		}
		return worktree.PROpen
	}
}

// ghEnv is the environment gh runs with for a request to host: the
// process's own, minus the enterprise token variables unless host is the
// one GH_HOST names (the user's explicit statement of which enterprise
// host that token is for). Stored credentials are per host and unaffected;
// GH_TOKEN applies to github.com only and is left alone.
func ghEnv(host string) []string {
	trusted := strings.EqualFold(host, strings.TrimSpace(os.Getenv("GH_HOST")))
	env := os.Environ()
	if trusted {
		return env
	}
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(enterpriseTokenVars, name) {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// endpoint picks how gh is pointed at the GraphQL API: the `graphql`
// shorthand whenever gh's own idea of the host's API (https, default port;
// api.github.com for github.com) is right, else the full URL built from
// the remote's scheme and port.
func endpoint(repo clone.ForgeRepo) string {
	if repo.Host == "github.com" || (repo.Scheme == "https" && repo.Port == "") {
		return "graphql"
	}
	return repo.APIBase() + "/api/graphql"
}

// graphqlString renders s as a GraphQL string literal. GraphQL's escapes are
// JSON's, so the JSON encoder produces a valid literal.
func graphqlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}
