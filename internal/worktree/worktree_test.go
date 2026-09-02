package worktree

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseRefs(t *testing.T) {
	got := ParseRefs([]string{
		"refs/heads/nb\u00a0\t\ts1\t", // trailing NO-BREAK SPACE must survive
		"refs/remotes/origin/HEAD\t\ts0\t",
		"refs/heads/main\trefs/remotes/origin/main\tabc123\t/src/repo",
		"refs/remotes/origin/feature/x\t\ts2\t",
		"refs/heads/feature/x\t\ts2\t",
		"refs/remotes/upstream/main\t\ts3\t",
		"refs/remotes/foo/bar/feature\t\ts4\t", // remote named "foo/bar"
		"refs/remotes/foo/bar/HEAD\t\ts4\t",
		"refs/tags/v1\t\ts5\t", // ignored
		"malformed-line",
		"",
	}, []string{"origin", "upstream", "foo/bar"})
	want := []Branch{
		{Name: "nb\u00a0", Ref: "refs/heads/nb\u00a0", Short: "nb\u00a0", SHA: "s1"},
		{Name: "main", Ref: "refs/heads/main", Short: "main", WorktreePath: "/src/repo", SHA: "abc123", Upstream: "refs/remotes/origin/main"},
		{Name: "feature/x", Ref: "refs/heads/feature/x", Short: "feature/x", SHA: "s2"},
		{Name: "origin/feature/x", Ref: "refs/remotes/origin/feature/x", Remote: "origin", Short: "feature/x", SHA: "s2"},
		{Name: "upstream/main", Ref: "refs/remotes/upstream/main", Remote: "upstream", Short: "main", SHA: "s3"},
		{Name: "foo/bar/feature", Ref: "refs/remotes/foo/bar/feature", Remote: "foo/bar", Short: "feature", SHA: "s4"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if !branchEq(got[i], want[i]) {
			t.Errorf("%d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func branchEq(a, b Branch) bool {
	return a.Name == b.Name && a.Ref == b.Ref && a.Remote == b.Remote && a.Short == b.Short && a.WorktreePath == b.WorktreePath && a.SHA == b.SHA && a.Upstream == b.Upstream
}

func TestParseRefsFourFields(t *testing.T) {
	got := ParseRefs([]string{
		"refs/heads/free\t\tsha1\t",                                // no worktree: must NOT be "in use"
		"refs/heads/used\trefs/remotes/origin/used\tsha2\t/w/path", // upstream, sha, worktree
		"refs/heads/tabby\t\tsha3\t/w/with\ttab",                   // worktree path containing a tab
		"refs/remotes/origin/main\t\tsha4\t",                       // remotes never have a worktree
		"refs/heads/short\t/w/old",                                 // fewer than 3 tabs: skipped
		"refs/heads/bare",                                          // skipped
	}, []string{"origin"})
	want := map[string]Branch{
		"free":        {WorktreePath: "", SHA: "sha1"},
		"used":        {WorktreePath: "/w/path", SHA: "sha2", Upstream: "refs/remotes/origin/used"},
		"tabby":       {WorktreePath: "/w/with\ttab", SHA: "sha3"},
		"origin/main": {WorktreePath: "", SHA: "sha4"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d branches: %+v", len(got), got)
	}
	for _, b := range got {
		w, ok := want[b.Name]
		if !ok || b.WorktreePath != w.WorktreePath || b.SHA != w.SHA || b.Upstream != w.Upstream {
			t.Errorf("%s: got wt=%q sha=%q up=%q want %+v", b.Name, b.WorktreePath, b.SHA, b.Upstream, w)
		}
		if b.Name == "free" && b.InUse() {
			t.Error("branch without a worktree must not be in use")
		}
	}
}

func TestAnnotatePRs(t *testing.T) {
	bs := []Branch{
		{Name: "main", Ref: "refs/heads/main", SHA: "m"},
		{Name: "feat", Ref: "refs/heads/feat", SHA: "f"},
		{Name: "origin/main", Remote: "origin", SHA: "m"},
		{Name: "origin/feat", Remote: "origin", SHA: "f"},
		{Name: "origin/other", Remote: "origin", SHA: "o"},
	}
	got := AnnotatePRs(bs, []PRHead{{Remote: "origin", Number: 12, SHA: "f"}, {Remote: "origin", Number: 30, SHA: "f"}})
	names := func(b []Branch) string {
		var s []string
		for _, x := range b {
			s = append(s, x.Name+strings.Join(x.PRLabels(), ""))
		}
		return strings.Join(s, ",")
	}
	// Order is preserved; only labels are added.
	if names(got) != "main,feat#12#30,origin/main,origin/feat#12#30,origin/other" {
		t.Errorf("got %s", names(got))
	}
	if got[1].SearchText() != "feat #12 #30" {
		t.Errorf("search text: %q", got[1].SearchText())
	}
	// Input untouched.
	if bs[1].HasPR() {
		t.Error("input must not be modified")
	}
	// Two remotes: labels carry the remote name.
	got = AnnotatePRs(bs, []PRHead{{Remote: "origin", Number: 1, SHA: "f"}, {Remote: "upstream", Number: 7, SHA: "m"}})
	if names(got) != "mainupstream#7,featorigin#1,origin/mainupstream#7,origin/featorigin#1,origin/other" {
		t.Errorf("multi-remote: %s", names(got))
	}
	// No heads: order unchanged, no labels.
	if names(AnnotatePRs(bs, nil)) != "main,feat,origin/main,origin/feat,origin/other" {
		t.Errorf("no heads: %s", names(AnnotatePRs(bs, nil)))
	}
}

func TestParseRefsWithoutRemoteList(t *testing.T) {
	got := ParseRefs([]string{"refs/remotes/foo/bar/feature\t\ts\t"}, nil)
	if len(got) != 1 || got[0].Remote != "foo" || got[0].Short != "bar/feature" {
		t.Errorf("fallback split: %+v", got)
	}
}

func TestRefspecsCover(t *testing.T) {
	std := []string{"+refs/heads/*:refs/remotes/origin/*"}
	single := []string{"+refs/heads/main:refs/remotes/origin/main"}
	dst := func(b string) string { return "refs/remotes/origin/" + b }
	cases := []struct {
		specs []string
		ref   string
		dst   string
		want  bool
	}{
		{std, "refs/heads/feature", dst("feature"), true},
		{std, "refs/heads/a/b", dst("a/b"), true},
		{single, "refs/heads/main", dst("main"), true},
		{single, "refs/heads/feature", dst("feature"), false},
		{nil, "refs/heads/feature", dst("feature"), false},
		{[]string{"refs/heads/qa/*:refs/remotes/origin/qa/*"}, "refs/heads/qa/x", dst("qa/x"), true},
		{[]string{"refs/heads/qa/*:refs/remotes/origin/qa/*"}, "refs/heads/feature", dst("feature"), false},
		// mapping onto another namespace does not produce the expected ref
		{[]string{"+refs/heads/*:refs/remotes/custom/*"}, "refs/heads/feature", dst("feature"), false},
		{[]string{"+refs/heads/*:refs/remotes/custom/*"}, "refs/heads/feature", "refs/remotes/custom/feature", true},
		// source-only refspec creates no persistent remote-tracking ref
		{[]string{"refs/heads/feature"}, "refs/heads/feature", dst("feature"), false},
		// a negative refspec excludes even when a positive matches
		{[]string{"+refs/heads/*:refs/remotes/origin/*", "^refs/heads/feature"}, "refs/heads/feature", dst("feature"), false},
		{[]string{"+refs/heads/*:refs/remotes/origin/*", "^refs/heads/feature"}, "refs/heads/other", dst("other"), true},
		{[]string{"^refs/heads/qa/*", "+refs/heads/*:refs/remotes/origin/*"}, "refs/heads/qa/x", dst("qa/x"), false},
		// refspecs are byte-exact: a trailing U+00A0 is part of the ref, and
		// a negative refspec naming it must exclude exactly that branch.
		{[]string{"+refs/heads/*:refs/remotes/origin/*", "^refs/heads/feat\u00a0"}, "refs/heads/feat\u00a0", dst("feat\u00a0"), false},
		{[]string{"+refs/heads/*:refs/remotes/origin/*", "^refs/heads/feat\u00a0"}, "refs/heads/feat", dst("feat"), true},
	}
	for _, tc := range cases {
		if got := RefspecsCover(tc.specs, tc.ref, tc.dst); got != tc.want {
			t.Errorf("%v %q -> %q: got %v", tc.specs, tc.ref, tc.dst, got)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"feature", "feat/x-1", "wt/20260830-1204", "a.b", "@foo", "feature/foo", "release/v1.2", "a/b.c/d", "head", "HEADS"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a..b", "-", "-x", "/x", "x/", "x.lock", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "@", "a@{b", "a//b", ".x", "x.", "foo/.bar", "foo.lock/bar", "a/b.", "a/.lock", "HEAD"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestMake(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC)
	locals := map[string]bool{"main": true, "feature": true}
	local := &Branch{Name: "feature", Short: "feature"}
	inUse := &Branch{Name: "main", Short: "main", WorktreePath: "/src/repo"}
	remoteFeature := &Branch{Name: "origin/feature", Ref: "refs/remotes/origin/feature", Remote: "origin", Short: "feature"}
	remoteNew := &Branch{Name: "origin/new", Ref: "refs/remotes/origin/new", Remote: "origin", Short: "new"}
	remoteBadShort := &Branch{Name: "origin/-release", Ref: "refs/remotes/origin/-release", Remote: "origin", Short: "-release"}

	cases := []struct {
		name string
		sel  *Branch
		new  string
		want Plan
		err  error
	}{
		{"local checkout", local, "", Plan{Branch: "feature"}, nil},
		{"local ignores new name", local, "whatever", Plan{Branch: "feature"}, nil},
		{"local in use", inUse, "", Plan{}, ErrInUse},
		{"remote tracked", remoteNew, "", Plan{Branch: "new", Base: "refs/remotes/origin/new", Upstream: "refs/remotes/origin/new", Creates: true}, nil},
		{"remote tracked via same name", remoteNew, "new", Plan{Branch: "new", Base: "refs/remotes/origin/new", Upstream: "refs/remotes/origin/new", Creates: true}, nil},
		{"remote short name invalid locally", remoteBadShort, "", Plan{}, ErrBadName},
		{"remote short name invalid, renamed", remoteBadShort, "release", Plan{Branch: "release", Base: "refs/remotes/origin/-release", Creates: true}, nil},
		{"remote same name but local exists", remoteFeature, "feature", Plan{}, ErrLocalExists},
		{"remote with local conflict", remoteFeature, "", Plan{}, ErrLocalExists},
		{"remote new name", remoteFeature, "foo", Plan{Branch: "foo", Base: "refs/remotes/origin/feature", Creates: true}, nil},
		{"remote new name taken", remoteFeature, "main", Plan{}, ErrNameTaken},
		{"remote bad name", remoteFeature, "a b", Plan{}, ErrBadName},
		{"new from head", nil, "topic", Plan{Branch: "topic", Creates: true}, nil},
		{"auto name", nil, "", Plan{Branch: "wt/20260830-1204", Creates: true}, nil},
		{"blank is not auto", nil, "  ", Plan{}, ErrBadName},
		{"nbsp kept", nil, "feat\u00a0", Plan{Branch: "feat\u00a0", Creates: true}, nil},
		{"remote nbsp tracked", &Branch{Name: "origin/feat\u00a0", Ref: "refs/remotes/origin/feat\u00a0", Remote: "origin", Short: "feat\u00a0"}, "feat\u00a0",
			Plan{Branch: "feat\u00a0", Base: "refs/remotes/origin/feat\u00a0", Upstream: "refs/remotes/origin/feat\u00a0", Creates: true}, nil},
		{"new taken", nil, "main", Plan{}, ErrNameTaken},
		{"new bad", nil, "x..y", Plan{}, ErrBadName},
	}
	for _, tc := range cases {
		got, err := Make(tc.sel, tc.new, locals, now)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: err=%v want %v", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v err=%v want %+v", tc.name, got, err, tc.want)
		}
	}
}
