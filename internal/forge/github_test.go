package forge

import (
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/utahta/herdr-hop/internal/clone"
	"github.com/utahta/herdr-hop/internal/worktree"
)

func TestStateOf(t *testing.T) {
	cases := []struct {
		node prNode
		want worktree.PRState
	}{
		{prNode{State: "OPEN"}, worktree.PROpen},
		{prNode{State: "OPEN", IsDraft: true}, worktree.PRDraft},
		{prNode{State: "MERGED", IsDraft: true}, worktree.PRMerged},
		{prNode{State: "CLOSED"}, worktree.PRClosed},
	}
	for _, c := range cases {
		if got := stateOf(&c.node); got != c.want {
			t.Errorf("%+v: got %q want %q", c.node, got, c.want)
		}
	}
}

func TestEndpoint(t *testing.T) {
	cases := map[clone.ForgeRepo]string{
		{Scheme: "https", Host: "github.com", Owner: "o", Name: "r"}:                "graphql",
		{Scheme: "https", Host: "ghe.example", Owner: "o", Name: "r"}:               "graphql",
		{Scheme: "https", Host: "ghe.example", Port: "8443", Owner: "o", Name: "r"}: "https://ghe.example:8443/api/graphql",
		{Scheme: "http", Host: "ghe.example", Owner: "o", Name: "r"}:                "http://ghe.example/api/graphql",
		{Scheme: "http", Host: "ghe.example", Port: "8080", Owner: "o", Name: "r"}:  "http://ghe.example:8080/api/graphql",
	}
	for repo, want := range cases {
		if got := endpoint(repo); got != want {
			t.Errorf("%s: got %q want %q", repo, got, want)
		}
	}
}

// fakeGH is a stand-in gh: a script that fails the way gh does for a host
// without a token, and hangs when asked to (to be cancelled).
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/gh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKnowsHostCachesOnlyDefiniteAnswers(t *testing.T) {
	// A cancelled check must not brand the host as unauthenticated.
	g := &GitHub{gh: fakeGH(t, "sleep 5"), hosts: map[string]bool{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if known, err := g.knowsHost(ctx, "github.com"); known || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled check is undecided: known=%v err=%v", known, err)
	}
	if _, seen := g.hosts["github.com"]; seen {
		t.Error("a cancelled check must not be cached")
	}
	// A failure for any other reason is reported, not cached, and not
	// mistaken for "no token".
	g = &GitHub{gh: fakeGH(t, "echo 'keyring: dbus unavailable' >&2; exit 2"), hosts: map[string]bool{}}
	known, err := g.knowsHost(context.Background(), "github.com")
	if known || err == nil || !strings.Contains(err.Error(), "keyring: dbus unavailable") {
		t.Fatalf("an undiagnosed failure must carry its cause: known=%v err=%v", known, err)
	}
	if _, seen := g.hosts["github.com"]; seen {
		t.Error("an undiagnosed failure must not be cached")
	}
	if _, err := g.PullRequests(context.Background(), ghRepo("o", "r"), []int{1}); err == nil || errors.Is(err, ErrUnsupportedHost) {
		t.Errorf("PullRequests must propagate the failure, not report an unsupported host: %v", err)
	}
	// gh's own "no token" answer is a definite no.
	g = &GitHub{gh: fakeGH(t, "echo 'no oauth token found for github.com' >&2; exit 1"), hosts: map[string]bool{}}
	if known, err := g.knowsHost(context.Background(), "github.com"); known || err != nil {
		t.Fatalf("no token means no: known=%v err=%v", known, err)
	}
	if known, seen := g.hosts["github.com"]; !seen || known {
		t.Error("a definite no must be cached")
	}
	if _, err := g.PullRequests(context.Background(), ghRepo("o", "r"), []int{1}); !errors.Is(err, ErrUnsupportedHost) {
		t.Errorf("no token -> ErrUnsupportedHost, got %v", err)
	}
	// And so is success.
	g = &GitHub{gh: fakeGH(t, "echo gho_x"), hosts: map[string]bool{}}
	if known, err := g.knowsHost(context.Background(), "github.com"); !known || err != nil {
		t.Fatalf("a token means yes: known=%v err=%v", known, err)
	}
	if known, seen := g.hosts["github.com"]; !seen || !known {
		t.Error("a definite yes must be cached")
	}
}

func ghRepo(owner, name string) clone.ForgeRepo {
	return clone.ForgeRepo{Scheme: "https", Host: "github.com", Owner: owner, Name: name}
}

// recordingGH is a fake gh that logs its arguments and answers one PR.
func recordingGH(t *testing.T) (*GitHub, func() []string) {
	t.Helper()
	log := t.TempDir() + "/args"
	g := &GitHub{gh: fakeGH(t, `printf '%s\n' "$@" > `+log+`; echo '{"data":{"repository":{"p1":{"number":1,"title":"T","state":"OPEN"}}}}'`), hosts: map[string]bool{"github.com": true, "ghe.example": true}}
	return g, func() []string {
		args, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(args)), "\n")
	}
}

func TestQueryInlinesRepositoryAsStringLiterals(t *testing.T) {
	// A repository called "123" or "true" must reach GraphQL as a string:
	// the names are written into the query as literals, never passed as
	// (type-converted) -F fields or as variables.
	g, args := recordingGH(t)
	infos, err := g.PullRequests(context.Background(), ghRepo("123", "true"), []int{1})
	if err != nil || infos[1].Title != "T" {
		t.Fatalf("infos=%v err=%v", infos, err)
	}
	got := args()
	if slices.Contains(got, "-F") {
		t.Errorf("typed -F used: %v", got)
	}
	var query string
	for _, a := range got {
		if q, ok := strings.CutPrefix(a, "query="); ok {
			query = q
		}
	}
	if !strings.Contains(query, `repository(owner:"123",name:"true")`) {
		t.Errorf("owner/name must be string literals in the query: %q", query)
	}
	if n := len(slices.DeleteFunc(slices.Clone(got), func(a string) bool { return !strings.HasPrefix(a, "-f") })); n != 1 {
		t.Errorf("only the query is passed as a field: %v", got)
	}
	// github.com: the graphql shorthand endpoint under the bare host name.
	if !slices.Contains(got, "graphql") || !slices.Contains(got, "github.com") {
		t.Errorf("shorthand endpoint expected: %v", got)
	}
}

func TestQueryOffDefaultUsesFullURLAndBareHostname(t *testing.T) {
	// gh's --hostname takes a bare host name (it rejects host:port) and
	// always assumes the default port; a remote on another port needs the
	// full endpoint URL. The account is looked up by the bare name, the
	// request goes where the remote does.
	cases := map[clone.ForgeRepo]string{
		{Scheme: "https", Host: "ghe.example", Port: "8443", Owner: "o", Name: "r"}: "https://ghe.example:8443/api/graphql",
	}
	for repo, url := range cases {
		g, args := recordingGH(t)
		if _, err := g.PullRequests(context.Background(), repo, []int{1}); err != nil {
			t.Fatal(err)
		}
		got := args()
		for i, a := range got {
			if a == "--hostname" && got[i+1] != "ghe.example" {
				t.Errorf("%s: --hostname must be the bare host, got %q", repo, got[i+1])
			}
		}
		if !slices.Contains(got, url) || slices.Contains(got, "graphql") {
			t.Errorf("%s: full URL endpoint %s expected: %v", repo, url, got)
		}
		if !slices.Contains(got, "--hostname") {
			t.Errorf("%s: --hostname missing: %v", repo, got)
		}
	}
}

func TestPlainHTTPEndpointIsRefused(t *testing.T) {
	// A token must never travel over plain HTTP, whatever the git remote
	// uses. gh is not even run.
	g, args := recordingGH(t)
	for _, repo := range []clone.ForgeRepo{
		{Scheme: "http", Host: "ghe.example", Owner: "o", Name: "r"},
		{Scheme: "http", Host: "ghe.example", Port: "8080", Owner: "o", Name: "r"},
	} {
		if _, err := g.PullRequests(context.Background(), repo, []int{1}); !errors.Is(err, ErrInsecureEndpoint) {
			t.Errorf("%s: got %v", repo, err)
		}
	}
	if got := args(); len(got) != 1 || got[0] != "" {
		t.Errorf("gh must not have been run: %v", got)
	}
}

func TestEnterpriseTokenVariablesReachOnlyTheGHHost(t *testing.T) {
	// GH_ENTERPRISE_TOKEN makes gh treat *any* host as authenticated and
	// send the token there. Hosts come from git remotes, so the variables
	// are withheld — except for the host the user named in GH_HOST.
	t.Setenv("GH_ENTERPRISE_TOKEN", "secret")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "secret2")
	t.Setenv("GH_HOST", "ghe.example")
	log := t.TempDir() + "/env"
	script := `printf '%s|%s\n' "${GH_ENTERPRISE_TOKEN:-unset}" "${GITHUB_ENTERPRISE_TOKEN:-unset}" > ` + log + `; echo tok`
	g := &GitHub{gh: fakeGH(t, script), hosts: map[string]bool{}}
	seen := func() string { b, _ := os.ReadFile(log); return strings.TrimSpace(string(b)) }

	if known, err := g.knowsHost(context.Background(), "attacker.example"); err != nil || !known {
		t.Fatalf("fake gh answers yes: known=%v err=%v", known, err)
	}
	if seen() != "unset|unset" {
		t.Errorf("enterprise tokens must be withheld from an untrusted host, gh saw %q", seen())
	}
	if _, err := g.knowsHost(context.Background(), "ghe.example"); err != nil {
		t.Fatal(err)
	}
	if seen() != "secret|secret2" {
		t.Errorf("the GH_HOST host may use the enterprise tokens, gh saw %q", seen())
	}
	// The same environment rule applies to the API call itself.
	g.hosts["attacker.example"] = true
	script = `printf '%s\n' "${GH_ENTERPRISE_TOKEN:-unset}" > ` + log + `; echo '{"data":{"repository":{}}}'`
	g.gh = fakeGH(t, script)
	if _, err := g.PullRequests(context.Background(), clone.ForgeRepo{Scheme: "https", Host: "attacker.example", Owner: "o", Name: "r"}, []int{1}); err != nil {
		t.Fatal(err)
	}
	if seen() != "unset" {
		t.Errorf("api call to an untrusted host must not see the token, gh saw %q", seen())
	}
}

func TestGraphqlString(t *testing.T) {
	if got := graphqlString(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("got %s", got)
	}
}

func TestPullRequestsBatchesByAHundred(t *testing.T) {
	// 61 numbers (a real repository's count) go in one query; 101 in two.
	log := t.TempDir() + "/calls"
	g := &GitHub{gh: fakeGH(t, `echo call >> `+log+`; echo '{"data":{"repository":{}}}'`), hosts: map[string]bool{"github.com": true}}
	calls := func() int { b, _ := os.ReadFile(log); return strings.Count(string(b), "call") }
	nums := func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i + 1
		}
		return out
	}
	if _, err := g.PullRequests(context.Background(), ghRepo("o", "r"), nums(61)); err != nil || calls() != 1 {
		t.Errorf("61 numbers: err=%v calls=%d", err, calls())
	}
	os.Remove(log)
	if _, err := g.PullRequests(context.Background(), ghRepo("o", "r"), nums(101)); err != nil || calls() != 2 {
		t.Errorf("101 numbers: err=%v calls=%d", err, calls())
	}
}

func TestLogLinesAreSanitized(t *testing.T) {
	// The repository name is URL-decoded from the remote and gh's stderr is
	// remote-controlled: neither may forge a log line.
	var buf strings.Builder
	g, _ := recordingGH(t)
	g.Log = log.New(&buf, "", 0)
	g.gh = fakeGH(t, "printf 'boom\\nforged line\\033[31m' >&2; exit 3")
	g.hosts["github.com"] = true
	repo := clone.ForgeRepo{Scheme: "https", Host: "github.com", Owner: "o\nforged", Name: "r"}
	g.PullRequests(context.Background(), repo, []int{1})
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.HasPrefix(line, "forged") || !strings.HasPrefix(line, "forge ") {
			t.Errorf("log line not sanitized: %q", line)
		}
		if strings.Contains(line, "\033") {
			t.Errorf("control sequence in log: %q", line)
		}
	}
}
