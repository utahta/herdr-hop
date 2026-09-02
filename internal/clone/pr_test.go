package clone

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestParsePR(t *testing.T) {
	cases := []struct {
		in       string
		kind     PRKind
		n        int
		repoURL  string
		owner    string
		repo     string
		repoPath string
	}{
		{"https://github.com/utahta/herdr-hop/pull/12", PRGitHub, 12, "https://github.com/utahta/herdr-hop", "utahta", "herdr-hop", "github.com/utahta/herdr-hop"},
		{"https://github.com/o/r/pull/7/files?diff=split#discussion_r1", PRGitHub, 7, "https://github.com/o/r", "o", "r", "github.com/o/r"},
		{"https://github.com/o/r/pull/7/commits/abc", PRGitHub, 7, "https://github.com/o/r", "o", "r", "github.com/o/r"},
		{"https://ghe.example.com:8443/Org/Repo/pull/3", PRGitHub, 3, "https://ghe.example.com:8443/Org/Repo", "Org", "Repo", "ghe.example.com:8443/org/repo"},
		{"https://GitHub.com:443/Acme/Api/pull/1", PRGitHub, 1, "https://GitHub.com:443/Acme/Api", "Acme", "Api", "github.com/acme/api"},
		{"https://gitlab.example.com/group/subgroup/repo/-/merge_requests/42", PRGitLab, 42, "https://gitlab.example.com/group/subgroup/repo", "subgroup", "repo", "gitlab.example.com/group/subgroup/repo"},
		{"https://gitlab.com/g/r/-/merge_requests/5/diffs", PRGitLab, 5, "https://gitlab.com/g/r", "g", "r", "gitlab.com/g/r"},
		{"https://u:tok@github.com/o/r/pull/9", PRGitHub, 9, "https://u:tok@github.com/o/r", "o", "r", "github.com/o/r"},
		// A GitLab namespace containing "pull/12" must not be read as a GitHub PR.
		{"https://gitlab.com/top/group/pull/12/repo/-/merge_requests/3", PRGitLab, 3, "https://gitlab.com/top/group/pull/12/repo", "12", "repo", "gitlab.com/top/group/pull/12/repo"},
		// A GHES host is still read as a GitHub PR.
		{"https://ghe.example.com/o/r/pull/3", PRGitHub, 3, "https://ghe.example.com/o/r", "o", "r", "ghe.example.com/o/r"},
	}
	for _, tc := range cases {
		pr, err := ParsePR(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if pr.Kind != tc.kind || pr.Number != tc.n || pr.RepoURL != tc.repoURL || pr.Owner != tc.owner || pr.Repo != tc.repo || pr.RepoPath != tc.repoPath {
			t.Errorf("%q: got %+v", tc.in, pr)
		}
	}
	for _, bad := range []string{
		"", "o/r", "https://github.com/o/r", "https://github.com/o/r/pull/", "https://github.com/o/r/pull/0",
		"https://github.com/o/r/pull/x", "https://github.com/o/r/pulls/1", "https://github.com/pull/1",
		"https://gitlab.com/g/r/merge_requests/1", "https://github.com/o/r/issues/1", "https://github.com/../r/pull/1",
		"https://github.com/a/b/c/pull/1", // GitHub PRs have exactly owner/repo
		// On a GitLab host, ".../pull/12" is a project URL, never a GitHub PR.
		"https://gitlab.com/acme/tools/pull/12",
		"https://gitlab.example.com/acme/tools/pull/12",
		"https://code.gitlab.internal/o/r/pull/5",
	} {
		if _, err := ParsePR(bad); !errors.Is(err, ErrNotPR) {
			t.Errorf("%q: expected ErrNotPR, got %v", bad, err)
		}
	}
}

func TestPRRefsAndNames(t *testing.T) {
	gh, _ := ParsePR("https://github.com/o/r/pull/12")
	gl, _ := ParsePR("https://gitlab.com/g/r/-/merge_requests/3")
	if gh.HeadRef() != "refs/pull/12/head" || gl.HeadRef() != "refs/merge-requests/3/head" {
		t.Errorf("head refs: %s %s", gh.HeadRef(), gl.HeadRef())
	}
	if gh.BranchName() != "pr/12" || gh.Label() != "github.com/o/r #12" {
		t.Errorf("names: %s %s", gh.BranchName(), gh.Label())
	}
	if gh.Provenance() != "github.com/o/r#12" || gh.ProvenanceKey() != "branch.pr/12.hop-pr" {
		t.Errorf("provenance: %s %s", gh.Provenance(), gh.ProvenanceKey())
	}
	lr := gh.LocalRef()
	if !strings.HasPrefix(lr, "refs/hop/pr/") || !strings.HasSuffix(lr, "/12") || len(gh.RefKey()) != 12 {
		t.Errorf("local ref: %s key=%s", lr, gh.RefKey())
	}
	// The ref must be valid for git even for awkward repository names.
	awkward, _ := ParsePR("https://ghe.example.com:8443/o/repo.lock/pull/1")
	if out, err := exec.Command("git", "check-ref-format", awkward.LocalRef()).CombinedOutput(); err != nil {
		t.Errorf("check-ref-format %s: %v %s", awkward.LocalRef(), err, out)
	}
}

func TestRefKeyIsStableAcrossEquivalentURLs(t *testing.T) {
	a, _ := ParsePR("https://GitHub.com:443/Acme/Api/pull/1")
	b, _ := ParsePR("https://github.com/acme/api.git/pull/1")
	c, _ := ParsePR("https://github.com/acme/api/pull/1")
	if a.RefKey() != b.RefKey() || b.RefKey() != c.RefKey() {
		t.Errorf("keys differ: %s %s %s", a.RefKey(), b.RefKey(), c.RefKey())
	}
	d, _ := ParsePR("https://github.com:8443/acme/api/pull/1")
	if d.RefKey() == c.RefKey() {
		t.Error("non-default port must give a different key")
	}
}

func TestRepoPathOfAndRemoteIdentity(t *testing.T) {
	same := []string{
		"https://github.com/Acme/Api.git",
		"https://GitHub.com:443/acme/api",
		"ssh://git@github.com/acme/api.git",
		"ssh://git@github.com:22/acme/api",
		"git@github.com:acme/api.git",
		"https://u:tok@github.com/acme/api?token=x#f",
	}
	for _, u := range same {
		if got := RepoPathOf(u); got != "github.com/acme/api" {
			t.Errorf("%q: %q", u, got)
		}
		rp, id, ok := RemoteIdentity(u)
		if !ok || rp != "github.com/acme/api" || id != "github.com/acme/api" {
			t.Errorf("RemoteIdentity(%q) = %q %q %v", u, rp, id, ok)
		}
	}
	for u, want := range map[string]string{
		"https://git.example.com:8443/o/r":                "git.example.com:8443/o/r",
		"https://git.example.com:9443/o/r":                "git.example.com:9443/o/r",
		"https://gitlab.example.com/group-a/team/api.git": "gitlab.example.com/group-a/team/api",
		"https://gitlab.example.com/group-b/team/api":     "gitlab.example.com/group-b/team/api",
	} {
		if got := RepoPathOf(u); got != want {
			t.Errorf("%q: got %q want %q", u, got, want)
		}
	}
	// Sub-groups: RepoPath keeps the full namespace, id keeps the last two.
	rp, id, _ := RemoteIdentity("https://gitlab.example.com/group-a/team/api.git")
	if rp != "gitlab.example.com/group-a/team/api" || id != "gitlab.example.com/team/api" {
		t.Errorf("rp=%q id=%q", rp, id)
	}
	for _, bad := range []string{"", "nonsense", "https://host/", "https://host/onlyowner", "/local/path"} {
		if _, _, ok := RemoteIdentity(bad); ok {
			t.Errorf("%q should not be an identity", bad)
		}
	}
}

func TestParseForgeRepoKeepsSchemeAndPort(t *testing.T) {
	cases := map[string]ForgeRepo{
		"https://ghe.example:8443/o/r.git":     {Scheme: "https", Host: "ghe.example", Port: "8443", Owner: "o", Name: "r"},
		"https://ghe.example:443/o/r":          {Scheme: "https", Host: "ghe.example", Owner: "o", Name: "r"},
		"http://ghe.example/o/r":               {Scheme: "http", Host: "ghe.example", Owner: "o", Name: "r"},
		"http://ghe.example:8080/o/r":          {Scheme: "http", Host: "ghe.example", Port: "8080", Owner: "o", Name: "r"},
		"http://ghe.example:80/o/r":            {Scheme: "http", Host: "ghe.example", Owner: "o", Name: "r"},
		"https://GitHub.com/Acme/Api":          {Scheme: "https", Host: "github.com", Owner: "Acme", Name: "Api"},
		"ssh://git@ghe.example:2222/o/r.git":   {Scheme: "https", Host: "ghe.example", Owner: "o", Name: "r"},
		"git@ghe.example:o/r.git":              {Scheme: "https", Host: "ghe.example", Owner: "o", Name: "r"},
		"https://gitlab.example/group/sub/r":   {Scheme: "https", Host: "gitlab.example", Owner: "sub", Name: "r"},
		"https://user:pw@ghe.example:8443/o/r": {Scheme: "https", Host: "ghe.example", Port: "8443", Owner: "o", Name: "r"},
	}
	for raw, want := range cases {
		got, ok := ParseForgeRepo(raw)
		if !ok || got != want {
			t.Errorf("%s: got %+v (%v), want %+v", raw, got, ok, want)
		}
	}
	for _, bad := range []string{"", "not a url", "https://ghe.example/r"} {
		if got, ok := ParseForgeRepo(bad); ok {
			t.Errorf("%q must be rejected, got %+v", bad, got)
		}
	}
	r := cases["http://ghe.example:8080/o/r"]
	if r.APIBase() != "http://ghe.example:8080" || r.String() != "http://ghe.example:8080/o/r" {
		t.Errorf("APIBase=%q String=%q", r.APIBase(), r.String())
	}
}
