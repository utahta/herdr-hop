package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestRunRequiresRepo(t *testing.T) {
	if _, err := New().Run("", "status"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRunInRepo(t *testing.T) {
	dir := initRepo(t)
	out, err := New().Run(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out != "main" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestClone(t *testing.T) {
	src := initRepo(t)
	dest := filepath.Join(t.TempDir(), "h", "o", "r")
	var lines []string
	err := New().Clone(context.Background(), src, dest, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatal("clone missing .git")
	}
	if len(lines) == 0 {
		t.Error("expected progress output")
	}
}

func TestRefsFetchSetUpstream(t *testing.T) {
	// upstream repo with a branch, then a clone that has it as origin.
	up := initRepo(t)
	g := New()
	if _, err := g.Run(up, "branch", "feature"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "clone")
	if err := g.Clone(context.Background(), up, dest, nil); err != nil {
		t.Fatal(err)
	}
	refs, err := g.Refs(dest)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(refs, "\n") + "\n"
	realDest, _ := filepath.EvalSymlinks(dest)
	// Field order: refname, upstream, sha, worktreepath (path last; the
	// trailing field of the last line survives because Refs never trims).
	// The expected object id comes from git itself so the test holds for
	// SHA-1 and SHA-256 repositories alike.
	headSHA, _ := g.Run(dest, "rev-parse", "HEAD")
	// A fresh clone's main tracks origin/main.
	if !strings.Contains(joined, "refs/heads/main\trefs/remotes/origin/main\t"+headSHA+"\t"+realDest+"\n") {
		t.Errorf("main line malformed in %q (want sha %s, dest %s)", joined, headSHA, realDest)
	}
	for _, want := range []string{"refs/remotes/origin/main\t\t", "refs/remotes/origin/feature\t\t"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, refs)
		}
	}
	// A new upstream branch appears only after Fetch.
	if _, err := g.Run(up, "branch", "later"); err != nil {
		t.Fatal(err)
	}
	if err := g.Fetch(dest); err != nil {
		t.Fatal(err)
	}
	refs, _ = g.Refs(dest)
	if !strings.Contains(strings.Join(refs, " "), "refs/remotes/origin/later") {
		t.Errorf("fetch did not bring origin/later: %v", refs)
	}
	// SetUpstream
	if _, err := g.Run(dest, "branch", "feature", "origin/feature", "--no-track"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetUpstream(dest, "feature", "origin/feature"); err != nil {
		t.Fatal(err)
	}
	out, _ := g.Run(dest, "rev-parse", "--abbrev-ref", "feature@{upstream}")
	if out != "origin/feature" {
		t.Errorf("upstream = %q", out)
	}
}

func TestFetchErrorIsMaskedAndSanitized(t *testing.T) {
	repo := initRepo(t)
	g := New()
	if _, err := g.Run(repo, "remote", "add", "origin", "https://user:tok3n@example.com/o/r"); err != nil {
		t.Fatal(err)
	}
	// Remotes configured outside this plugin may carry credentials in the
	// query or fragment; clone input rejects those, fetch must still mask them.
	// Both userinfo and a secret query: git prints the URL without the
	// userinfo but with the query (and a trailing "/").
	g.Run(repo, "remote", "add", "both", "https://user:pw@example.com/o/r?token=secretb")
	g.Run(repo, "remote", "add", "q", "https://example.com/o/r?token=secretq")
	g.Run(repo, "remote", "add", "a", "https://example.com/o/r?access_token=secreta")
	g.Run(repo, "remote", "add", "f", "https://example.com/o/r#secretf")
	// Fake git: real git for everything except fetch, which fails loudly.
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = fetch ]; then\n" +
		"  printf 'fatal: Authentication failed for \\047https://user:tok3n@example.com/o/r\\047\\n' >&2\n" +
		"  printf 'user:tok3n@example.com: denied \\033]52;c;c2VjcmV0\\007\\033[31mred\\033[0m\\n' >&2\n" +
		"  printf 'error: https://example.com/o/r?token=secretq https://example.com/o/r?access_token=secreta https://example.com/o/r#secretf\\n' >&2\n" +
		"  printf 'fatal: unable to access \\047https://example.com/o/r?token=secretb/\\047: 403\\n' >&2\n" +
		"  exit 128\n" +
		"fi\n" +
		"exec /usr/bin/env git \"$@\"\n"
	os.WriteFile(bin, []byte(script), 0o755)
	err := (&Git{Bin: bin}).Fetch(repo)
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, bad := range []string{"tok3n", "\x1b", "\x07", "52;c", "secretq", "secreta", "secretf", "secretb", "pw@", "token=", "access_token="} {
		if strings.Contains(msg, bad) {
			t.Errorf("leaked %q in %q", bad, msg)
		}
	}
	if !strings.Contains(msg, "https://example.com/o/r") || !strings.Contains(msg, "example.com: denied red") {
		t.Errorf("text lost: %q", msg)
	}
}

// fakeFailingFetch returns a git wrapper whose fetch prints msg and fails;
// every other subcommand is delegated to the real git.
func fakeFailingFetch(t *testing.T, msg string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = fetch ]; then printf '%s\\n' \"" + strings.ReplaceAll(msg, "'", "'\\''") + "\" >&2; exit 128; fi\n" +
		"exec /usr/bin/env git \"$@\"\n"
	os.WriteFile(bin, []byte(script), 0o755)
	return bin
}

func TestFetchErrorSplitCredentialIsMasked(t *testing.T) {
	repo := initRepo(t)
	g := New()
	g.Run(repo, "remote", "add", "origin", "https://user:token@example.com/o/r")
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = fetch ]; then printf 'user:\\033[31mtoken@example.com\\033[0m: denied\\n' >&2; exit 128; fi\n" +
		"exec /usr/bin/env git \"$@\"\n"
	os.WriteFile(bin, []byte(script), 0o755)
	err := (&Git{Bin: bin}).Fetch(repo)
	if err == nil || strings.Contains(err.Error(), "token") {
		t.Errorf("err=%v", err)
	}
}

func TestFetchMasksCredentialWithControlSequencesInConfiguredURL(t *testing.T) {
	repo := initRepo(t)
	g := New()
	// The configured URL itself carries a control sequence inside the secret.
	if _, err := g.Run(repo, "remote", "add", "origin", "https://user:to\x1b[31mken@example.com/o/r"); err != nil {
		t.Fatal(err)
	}
	err := (&Git{Bin: fakeFailingFetch(t, "fatal: user:token@example.com: denied")}).Fetch(repo)
	if err == nil || strings.Contains(err.Error(), "token") {
		t.Errorf("sanitized spelling of the credential leaked: %v", err)
	}
}

func TestRemoteURLs(t *testing.T) {
	repo := initRepo(t)
	g := New()
	if urls, err := g.RemoteURLs(repo); err != nil || len(urls) != 0 {
		t.Fatalf("no remotes: %v %v", urls, err)
	}
	g.Run(repo, "remote", "add", "origin", "https://a/o/r")
	g.Run(repo, "remote", "add", "up", "git@b:o/r.git")
	g.Run(repo, "remote", "set-url", "--push", "up", "https://c/o/r")
	urls, err := g.RemoteURLs(repo)
	if err != nil || len(urls) != 3 {
		t.Errorf("urls=%v err=%v", urls, err)
	}
	// insteadOf: the configured value is harmless, the effective URL is not.
	g.Run(repo, "remote", "add", "gh", "gh:o/r.git")
	g.Run(repo, "config", "url.https://u:tok3n@example.com/.insteadOf", "gh:")
	urls, _ = g.RemoteURLs(repo)
	joined := strings.Join(urls, " ")
	if !strings.Contains(joined, "https://u:tok3n@example.com/o/r.git") || !strings.Contains(joined, "gh:o/r.git") {
		t.Errorf("effective and configured URLs expected: %v", urls)
	}
	if err := (&Git{Bin: fakeFailingFetch(t, "fatal: unable to access 'https://u:tok3n@example.com/o/r.git'")}).Fetch(repo); err == nil || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("insteadOf-expanded URL leaked: %v", err)
	}
	// A remote whose name starts with "-" must still have its effective URL resolved.
	g.Run(repo, "remote", "add", "--", "-x", "gh:o/dash.git")
	urls, _ = g.RemoteURLs(repo)
	if !strings.Contains(strings.Join(urls, " "), "https://u:tok3n@example.com/o/dash.git") {
		t.Errorf("remote \"-x\": effective URL missing: %v", urls)
	}
	if err := (&Git{Bin: fakeFailingFetch(t, "fatal: unable to access 'https://u:tok3n@example.com/o/dash.git'")}).Fetch(repo); err == nil || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("remote \"-x\": leaked: %v", err)
	}
}

func TestRunErrorIsSanitized(t *testing.T) {
	repo := initRepo(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '\\033[2Jfatal: \\033]0;x\\007bad\\n' >&2; exit 1\n"), 0o755)
	_, err := (&Git{Bin: bin}).Run(repo, "status")
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), "fatal: bad") {
		t.Errorf("err=%q", err)
	}
}

func TestRefsKeepUnicodeWhitespace(t *testing.T) {
	repo := initRepo(t)
	g := New()
	name := "feat\u00a0" // trailing NO-BREAK SPACE is a valid ref name
	if _, err := g.Run(repo, "branch", "--", name); err != nil {
		t.Skipf("git refused the branch name on this platform: %v", err)
	}
	refs, err := g.Refs(repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range refs {
		if strings.HasPrefix(r, "refs/heads/"+name+"\t") {
			found = true
		}
	}
	if !found {
		t.Errorf("ref with trailing U+00A0 not preserved: %q", refs)
	}
}

func TestRemotesKeepUnicodeWhitespace(t *testing.T) {
	repo := initRepo(t)
	g := New()
	name := "foo/bar\u00a0"
	if _, err := g.Run(repo, "remote", "add", "--", name, "https://example.com/o/r"); err != nil {
		t.Skipf("git refused the remote name: %v", err)
	}
	names, err := g.Remotes(repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("remote name with trailing U+00A0 not preserved: %q", names)
	}
}

func TestBranchExists(t *testing.T) {
	repo := initRepo(t)
	g := New()
	g.Run(repo, "branch", "feature")
	for name, want := range map[string]bool{"feature": true, "nope": false, "main": true} {
		got, err := g.BranchExists(repo, name)
		if err != nil || got != want {
			t.Errorf("%q: got %v err=%v", name, got, err)
		}
	}
	// On a case-insensitive file system git resolves Feature to feature;
	// either answer is acceptable here, but it must not error.
	if _, err := g.BranchExists(repo, "Feature"); err != nil {
		t.Error(err)
	}
	// A fatal git failure is an error, never "does not exist".
	for _, code := range []int{128, 129, 2} {
		bin := filepath.Join(t.TempDir(), "git")
		os.WriteFile(bin, fmt.Appendf(nil, "#!/bin/sh\necho 'fatal: broken' >&2; exit %d\n", code), 0o755)
		exists, err := (&Git{Bin: bin}).BranchExists(repo, "x")
		if err == nil || exists {
			t.Errorf("exit %d: got exists=%v err=%v", code, exists, err)
		}
	}
	// A git that cannot be started is an error too.
	if _, err := (&Git{Bin: "/nonexistent/git"}).BranchExists(repo, "x"); err == nil {
		t.Error("unstartable git must be an error")
	}
}

func TestFetchRefspecsKeepWhitespace(t *testing.T) {
	repo := initRepo(t)
	g := New()
	g.Run(repo, "remote", "add", "origin", "https://example.com/o/r")
	spec := "^refs/heads/feat\u00a0"
	if _, err := g.Run(repo, "config", "--add", "remote.origin.fetch", spec); err != nil {
		t.Fatal(err)
	}
	specs, err := g.FetchRefspecs(repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range specs {
		if s == spec {
			found = true
		}
	}
	if !found {
		t.Errorf("trailing U+00A0 lost: %q", specs)
	}
	if specs2, _ := g.FetchRefspecs(repo, "nonexistent"); specs2 != nil {
		t.Errorf("missing remote: %q", specs2)
	}
}

func TestConfigGetSet(t *testing.T) {
	repo := initRepo(t)
	g := New()
	if v, err := g.ConfigGet(repo, "branch.pr/1.hop-pr"); err != nil || v != "" {
		t.Fatalf("unset: %q %v", v, err)
	}
	if err := g.ConfigSet(repo, "branch.pr/1.hop-pr", "github.com/o/r#1"); err != nil {
		t.Fatal(err)
	}
	if v, err := g.ConfigGet(repo, "branch.pr/1.hop-pr"); err != nil || v != "github.com/o/r#1" {
		t.Errorf("get: %q %v", v, err)
	}
	if _, err := (&Git{Bin: "/nonexistent/git"}).ConfigGet(repo, "x.y"); err == nil {
		t.Error("unstartable git must be an error, not unset")
	}
}

func TestFetchURLAndFetchRef(t *testing.T) {
	up := initRepo(t)
	g := New()
	// Simulate a PR head on the "server".
	g.Run(up, "update-ref", "refs/pull/4/head", "HEAD")
	repo := initRepo(t)
	g.Run(repo, "remote", "add", "origin", up)
	g.Run(repo, "remote", "set-url", "--add", "origin", "https://second.example/o/r")
	if u, err := g.FetchURL(repo, "origin"); err != nil || u != up {
		t.Fatalf("FetchURL = %q %v (want first URL only)", u, err)
	}
	if u, err := g.FetchURL(repo, "nope"); err != nil || u != "" {
		t.Errorf("missing remote: %q %v", u, err)
	}
	if err := g.FetchRef(context.Background(), repo, "origin", "+refs/pull/4/head:refs/hop/pr/abc/4"); err != nil {
		t.Fatal(err)
	}
	if out, _ := g.Run(repo, "rev-parse", "refs/hop/pr/abc/4"); out == "" {
		t.Error("local ref not created")
	}
	// Direct URL fetch works too and survives prune.
	if err := g.FetchRef(context.Background(), repo, up, "+refs/pull/4/head:refs/hop/pr/abc/4"); err != nil {
		t.Fatal(err)
	}
	g.Fetch(repo)
	if out, _ := g.Run(repo, "rev-parse", "refs/hop/pr/abc/4"); out == "" {
		t.Error("refs/hop ref must survive fetch --prune")
	}
}

func TestLsRemotePR(t *testing.T) {
	up := initRepo(t)
	g := New()
	g.Run(up, "branch", "feature")
	g.Run(up, "update-ref", "refs/pull/4/head", "refs/heads/feature")
	repo := initRepo(t)
	g.Run(repo, "remote", "add", "origin", up)
	st, err := g.LsRemotePR(context.Background(), repo, "origin", "refs/pull/4/head")
	if err != nil {
		t.Fatal(err)
	}
	if st.DefaultBranch != "main" || st.HeadSHA == "" {
		t.Fatalf("state: %+v", st)
	}
	if st.Branches["feature"] != st.HeadSHA || st.Branches["main"] == "" {
		t.Errorf("branches: %+v", st.Branches)
	}
	// Unknown head ref: no error, just empty HeadSHA.
	st, err = g.LsRemotePR(context.Background(), repo, "origin", "refs/pull/99/head")
	if err != nil || st.HeadSHA != "" {
		t.Errorf("missing head: %+v err=%v", st, err)
	}
	if _, err := g.LsRemotePR(context.Background(), repo, "/nonexistent/x", "refs/pull/1/head"); err == nil {
		t.Error("unreachable src must error")
	}
}

func TestLsRemotePRs(t *testing.T) {
	up := initRepo(t)
	g := New()
	g.Run(up, "update-ref", "refs/pull/4/head", "HEAD")
	g.Run(up, "update-ref", "refs/merge-requests/9/head", "HEAD")
	g.Run(up, "update-ref", "refs/pull/x/head", "HEAD") // not a number: ignored
	repo := initRepo(t)
	g.Run(repo, "remote", "add", "origin", up)
	refs, err := g.LsRemotePRs(context.Background(), repo, "origin", nil)
	if err != nil {
		t.Fatal(err)
	}
	nums := map[int]bool{}
	for _, r := range refs {
		nums[r.Number] = true
		if r.SHA == "" || r.Remote != "origin" {
			t.Errorf("ref = %+v", r)
		}
	}
	if len(refs) != 2 || !nums[4] || !nums[9] {
		t.Errorf("refs = %+v", refs)
	}
	// Failure is an error, not "no PRs".
	g.Run(repo, "remote", "add", "bad", "/nonexistent/repo")
	if _, err := g.LsRemotePRs(context.Background(), repo, "bad", nil); err == nil {
		t.Error("expected error for an unreachable remote")
	}
}

func TestRemoteFetchURLs(t *testing.T) {
	dir := initRepo(t)
	g := New()
	if rs, err := g.RemoteFetchURLs(context.Background(), dir); err != nil || rs != nil {
		t.Fatalf("no remotes: %v %v", rs, err)
	}
	for _, args := range [][]string{
		// url.<base>.insteadOf must be applied: identity is judged by the URL
		// git actually fetches from, not the raw configured value.
		{"config", "url.https://github.com/.insteadOf", "gh:"},
		{"remote", "add", "origin", "gh:o/r.git"},
		{"remote", "add", "upstream", "https://github.com/up/r"},
		// a second URL of a remote is not fetched from: only the first counts
		{"remote", "set-url", "--add", "origin", "https://mirror.example/o/r"},
		// a partial-clone remote prints "(fetch) [blob:none]": still a fetch line
		{"config", "remote.upstream.promisor", "true"},
		{"config", "remote.upstream.partialclonefilter", "blob:none"},
	} {
		if _, err := g.Run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := g.RemoteFetchURLs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Remote{
		{Name: "origin", URL: "https://github.com/o/r.git"},
		{Name: "upstream", URL: "https://github.com/up/r"},
	}
	if len(rs) != len(want) || rs[0] != want[0] || rs[1] != want[1] {
		t.Fatalf("got %v want %v", rs, want)
	}
}

func TestRemoteFetchURLsLegacyOrigin(t *testing.T) {
	// An origin in the legacy .git/remotes/<name> file format is invisible to
	// `git remote -v` but still resolved by `git fetch` / `git remote
	// get-url`, so RemoteFetchURLs must fall back to asking for it.
	dir := initRepo(t)
	g := New()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "remotes"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "URL: https://github.com/o/legacy.git\nPull: refs/heads/main:refs/remotes/origin/main\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "remotes", "origin"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// The format is deprecated ("nominated for removal" as of git 2.55); once
	// git itself stops resolving it there is nothing to stay compatible with.
	if u, err := g.FetchURL(dir, "origin"); err != nil || u == "" {
		t.Skipf("this git does not resolve legacy .git/remotes files (url=%q err=%v)", u, err)
	}
	rs, err := g.RemoteFetchURLs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0] != (Remote{Name: "origin", URL: "https://github.com/o/legacy.git"}) {
		t.Fatalf("got %v", rs)
	}
}

func TestCloneProgressIsRedacted(t *testing.T) {
	// A fake git that prints the URL it was given to stderr and fails.
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"fatal: Authentication failed for '$3'\" >&2\nexit 128\n"), 0o755)
	g := &Git{Bin: bin}
	var lines []string
	err := g.Clone(context.Background(), "https://u:tok@example.com/o/r", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, l := range lines {
		if strings.Contains(l, "tok") {
			t.Errorf("progress leaked credentials: %q", l)
		}
	}
	if !strings.Contains(err.Error(), "https://example.com/o/r") || strings.Contains(err.Error(), "tok") {
		t.Errorf("error: %v", err)
	}
}

func TestCloneProgressIsRedactedScpForm(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"fatal: could not read from $3\" >&2\nexit 128\n"), 0o755)
	var lines []string
	err := (&Git{Bin: bin}).Clone(context.Background(), "tok3n@example.com:owner/repo.git", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, l := range lines {
		if strings.Contains(l, "tok3n") || !strings.Contains(l, "example.com:owner/repo.git") {
			t.Errorf("progress: %q", l)
		}
	}
	if strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), "example.com:owner/repo.git") {
		t.Errorf("error: %v", err)
	}
}

// ssh reports "user@host: Permission denied (publickey)" without the
// repository path, so the whole-URL replacement alone would not catch it.
func TestCloneSSHDiagnosticIsMasked(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"echo 'tok3n@example.com: Permission denied (publickey).' >&2\n" +
		"echo 'fatal: Could not read from remote repository.' >&2\n" +
		"exit 128\n"
	os.WriteFile(bin, []byte(script), 0o755)
	var lines []string
	err := (&Git{Bin: bin}).Clone(context.Background(), "tok3n@example.com:owner/repo.git", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	if err == nil {
		t.Fatal("expected failure")
	}
	if len(lines) == 0 || lines[0] != "example.com: Permission denied (publickey)." {
		t.Errorf("progress: %q", lines)
	}
	if strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), "example.com: Permission denied") {
		t.Errorf("error: %v", err)
	}
	// Same for scheme URLs: "https://user:pw@host" may appear as "user:pw@host".
	os.WriteFile(bin, []byte("#!/bin/sh\necho 'error: u:pw@example.com: access denied' >&2\nexit 128\n"), 0o755)
	lines = nil
	err = (&Git{Bin: bin}).Clone(context.Background(), "https://u:pw@example.com/o/r", filepath.Join(dir, "d2"), func(l string) { lines = append(lines, l) })
	if err == nil || strings.Contains(err.Error(), "pw@") || len(lines) != 1 || lines[0] != "error: example.com: access denied" {
		t.Errorf("scheme: lines=%q err=%v", lines, err)
	}
}

// A helper that writes far more than bufio.Scanner's 64 KiB token limit on a
// single line, then keeps writing. Clone must keep draining and return.
func TestCloneDrainsHugeStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"head -c 300000 /dev/zero | tr '\\0' 'x' >&2\n" + // 300 KB, no newline
		"printf '\\nsecond line\\n' >&2\n" +
		"head -c 200000 /dev/zero | tr '\\0' 'y' >&2\n" +
		"exit 128\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	done := make(chan error, 1)
	go func() {
		done <- (&Git{Bin: bin}).Clone(context.Background(), "https://example.com/o/r", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Clone hung on oversized stderr")
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if len(lines[0]) != progressLineLimit || lines[1] != "second line" || len(lines[2]) != progressLineLimit {
		t.Errorf("lengths: %d %q %d", len(lines[0]), lines[1], len(lines[2]))
	}
}

func TestProgressLinesSplitting(t *testing.T) {
	var got []string
	for l := range progressLines(strings.NewReader("a\rb\nc\r\nd")) {
		got = append(got, l)
	}
	want := []string{"a", "b", "c", "", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestCloneOutputIsSanitized(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '\\033[2J\\033]52;c;c2VjcmV0\\007\\033[31mfatal:\\033[0m bad thing\\r\\n' >&2\n" +
		"exit 128\n"
	os.WriteFile(bin, []byte(script), 0o755)
	var lines []string
	err := (&Git{Bin: bin}).Clone(context.Background(), "https://example.com/o/r", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, s := range append(lines, err.Error()) {
		for _, r := range s {
			if r < 0x20 && r != '\t' || r == 0x7f {
				t.Errorf("control byte %q survived in %q", r, s)
			}
		}
		if strings.Contains(s, "52;c") {
			t.Errorf("OSC payload survived: %q", s)
		}
	}
	if len(lines) != 1 || lines[0] != "fatal: bad thing" {
		t.Errorf("progress: %q", lines)
	}
}

func TestCloneLowercasedHostDiagnosticIsMasked(t *testing.T) {
	for _, raw := range []string{"ssh://tok@EXAMPLE.com/owner/repo", "tok@EXAMPLE.com:owner/repo.git"} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "git")
		os.WriteFile(bin, []byte("#!/bin/sh\necho 'tok@example.com: Permission denied (publickey).' >&2\nexit 128\n"), 0o755)
		var lines []string
		err := (&Git{Bin: bin}).Clone(context.Background(), raw, filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
		if err == nil {
			t.Fatal("expected failure")
		}
		for _, s := range append(lines, err.Error()) {
			if strings.Contains(s, "tok") {
				t.Errorf("%q: leaked: %q", raw, s)
			}
		}
	}
}

func TestClonePercentEncodedUserinfoIsMasked(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	os.WriteFile(bin, []byte("#!/bin/sh\necho 'tok3n@example.com: Permission denied (publickey).' >&2\nexit 128\n"), 0o755)
	var lines []string
	err := (&Git{Bin: bin}).Clone(context.Background(), "ssh://tok%33n@example.com/o/r", filepath.Join(dir, "d"), func(l string) { lines = append(lines, l) })
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, s := range append(lines, err.Error()) {
		if strings.Contains(s, "tok3n") || strings.Contains(s, "tok%33n") {
			t.Errorf("leaked: %q", s)
		}
	}
	if len(lines) != 1 || lines[0] != "example.com: Permission denied (publickey)." {
		t.Errorf("progress: %q", lines)
	}
}

func TestCloneFailure(t *testing.T) {
	err := New().Clone(context.Background(), filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "d"), nil)
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("err=%v", err)
	}
}

func TestLsRemotePRsIsNonInteractive(t *testing.T) {
	// The background annotation fetch must run with git's terminal prompts
	// disabled; a fake git reports the environment it received.
	repo := initRepo(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = ls-remote ]; then echo \"prompt=$GIT_TERMINAL_PROMPT askreq=$SSH_ASKPASS_REQUIRE askpass=${SSH_ASKPASS:-unset} display=${DISPLAY:-unset} gitask=$GIT_ASKPASS\" >&2; exit 128; fi\n" +
		"exec /usr/bin/env git \"$@\"\n"
	os.WriteFile(bin, []byte(script), 0o755)
	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_ASKPASS", "/usr/bin/some-askpass")
	_, err := (&Git{Bin: bin}).LsRemotePRs(context.Background(), repo, "origin", nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"prompt=0", "askreq=never", "askpass=unset", "display=unset", "gitask=false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	// The user-initiated route decision keeps normal behaviour.
	os.WriteFile(bin, []byte("#!/bin/sh\nif [ \"$1\" = ls-remote ]; then echo \"prompt=${GIT_TERMINAL_PROMPT:-unset}\" >&2; exit 128; fi\nexec /usr/bin/env git \"$@\"\n"), 0o755)
	_, err = (&Git{Bin: bin}).LsRemotePR(context.Background(), repo, "origin", "refs/pull/1/head")
	if err == nil || !strings.Contains(err.Error(), "prompt=unset") {
		t.Errorf("explicit operations must not be altered: %v", err)
	}
}

func TestDirectURLInsteadOfRewriteIsMasked(t *testing.T) {
	repo := initRepo(t)
	g := New()
	g.Run(repo, "config", "url.https://u:tok3n@example.com/.insteadOf", "https://example.com/")
	// A fake git that echoes the rewritten URL in its failure, as real git
	// and ssh do; the config resolution still goes through real git.
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"fetch) echo \"fatal: unable to access 'https://u:tok3n@example.com/o/r/': 403\" >&2; exit 128;;\n" +
		"ls-remote)\n" +
		"  if [ \"$2\" = --get-url ]; then exec /usr/bin/env git \"$@\"; fi\n" +
		"  echo \"fatal: could not read from 'https://u:tok3n@example.com/o/r'\" >&2; exit 128;;\n" +
		"*) exec /usr/bin/env git \"$@\";;\n" +
		"esac\n"
	os.WriteFile(bin, []byte(script), 0o755)
	fg := &Git{Bin: bin}
	err := fg.FetchRef(context.Background(), repo, "https://example.com/o/r", "+refs/pull/1/head:refs/hop/pr/x/1")
	if err == nil || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("fetch: rewritten URL leaked: %v", err)
	}
	if _, err := fg.LsRemotePR(context.Background(), repo, "https://example.com/o/r", "refs/pull/1/head"); err == nil || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("ls-remote: rewritten URL leaked: %v", err)
	}
}
