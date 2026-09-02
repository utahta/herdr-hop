package clone

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in, proto string
		want      Target
		err       bool
	}{
		{"utahta/herdr-hop", "https", Target{"github.com", "utahta", "herdr-hop", "https://github.com/utahta/herdr-hop.git"}, false},
		{"utahta/herdr-hop", "ssh", Target{"github.com", "utahta", "herdr-hop", "git@github.com:utahta/herdr-hop.git"}, false},
		{"gitlab.com/g/p.git", "https", Target{"gitlab.com", "g", "p", "https://gitlab.com/g/p.git"}, false},
		{"https://github.com/o/r", "ssh", Target{"github.com", "o", "r", "https://github.com/o/r"}, false},
		{"https://github.com/o/r.git", "https", Target{"github.com", "o", "r", "https://github.com/o/r.git"}, false},
		{"ssh://git@github.com/o/r.git", "https", Target{"github.com", "o", "r", "ssh://git@github.com/o/r.git"}, false},
		{"ssh://git@host.example:2222/o/r", "https", Target{"host.example", "o", "r", "ssh://git@host.example:2222/o/r"}, false},
		{"git@github.com:o/r.git", "https", Target{"github.com", "o", "r", "git@github.com:o/r.git"}, false},
		{"github.com:o/r", "https", Target{"github.com", "o", "r", "github.com:o/r"}, false},
		{"  o/r  ", "https", Target{"github.com", "o", "r", "https://github.com/o/r.git"}, false},
		{"", "https", Target{}, true},
		{"just-a-name", "https", Target{}, true},
		{"a/b/c/d", "https", Target{}, true},
		{"o/../r", "https", Target{}, true},
		{"https://github.com/onlyowner", "https", Target{}, true},
		{"https://../o/r", "https", Target{}, true},
		{"https://user:secret@example.com/%zz/o/r", "https", Target{}, true},
		{"https://host/o/r.git?next=x/y/z", "https", Target{}, true},
		{"https://host/o/r.git#a/b/c", "https", Target{}, true},
		{"https://host/o/r?x=1", "https", Target{}, true},
		{"https://host:8443/o/r.git", "https", Target{"host", "o", "r", "https://host:8443/o/r.git"}, false},
		{"https://host/deep/path/o/r.git", "https", Target{"host", "o", "r", "https://host/deep/path/o/r.git"}, false},
		{"https://./o/r", "https", Target{}, true},
		{"..:o/r", "https", Target{}, true},
		{"../o/r", "https", Target{}, true},
		{"o/..", "https", Target{}, true},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in, "github.com", tc.proto)
		if tc.err {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %+v err=%v want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"https://user:tok3n@example.com/o/r.git": "https://example.com/o/r.git",
		"https://tok3n@example.com/o/r":          "https://example.com/o/r",
		"https://github.com/o/r.git":             "https://github.com/o/r.git",
		"https://host/repo?token=secret":         "https://host/repo",
		"https://host/repo?access_token=s#frag":  "https://host/repo",
		"https://u:p@host/repo?x=1#secret":       "https://host/repo",
		"ssh://git@github.com/o/r.git":           "ssh://github.com/o/r.git",
		"git@github.com:o/r.git":                 "github.com:o/r.git",
		"user:secret@host:o/r":                   "host:o/r",
		"github.com:o/r":                         "github.com:o/r",
		"o/r":                                    "o/r",
		// unparsable scheme URL: never echoed back
		"https://user:secret@example.com/%zz/o/r": RedactedInvalid,
		"https://user:secret@[bad/o/r":            RedactedInvalid,
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	tg, _ := Parse("https://u:p@h.com/o/r", "github.com", "https")
	if tg.URL != "https://u:p@h.com/o/r" || tg.SafeURL() != "https://h.com/o/r" || tg.ID() != "h.com/o/r" {
		t.Errorf("target: %+v safe=%q id=%q", tg, tg.SafeURL(), tg.ID())
	}
}

func TestMask(t *testing.T) {
	tg, _ := Parse("tok3n@example.com:owner/repo.git", "github.com", "https")
	if got := tg.Mask("tok3n@example.com: Permission denied (publickey)."); got != "example.com: Permission denied (publickey)." {
		t.Errorf("got %q", got)
	}
	if got := tg.Mask("failed tok3n@example.com:owner/repo.git me@other"); got != "failed example.com:owner/repo.git me@other" {
		t.Errorf("got %q", got)
	}
	tg, _ = Parse("https://u:pw@example.com/o/r", "github.com", "https")
	if got := tg.Mask("u:pw@example.com: denied https://u:pw@example.com/o/r"); got != "example.com: denied https://example.com/o/r" {
		t.Errorf("got %q", got)
	}
	tg, _ = Parse("o/r", "github.com", "https")
	if got := tg.Mask("a@b unchanged"); got != "a@b unchanged" {
		t.Errorf("got %q", got)
	}
}

func TestQueryAndFragmentRejected(t *testing.T) {
	for _, in := range []string{"https://host/o/r.git?next=x/y/z", "https://host/o/r#x/y"} {
		if _, err := Parse(in, "github.com", "https"); !errors.Is(err, ErrQueryOrFragment) {
			t.Errorf("%q: got %v", in, err)
		}
	}
}

func TestMaskerDecodedUserinfo(t *testing.T) {
	mask := NewMasker("ssh://tok%33n@example.com/o/r")
	for in, want := range map[string]string{
		"tok3n@example.com: Permission denied (publickey).": "example.com: Permission denied (publickey).",
		"tok%33n@example.com: nope":                         "example.com: nope",
		"ssh://tok%33n@example.com/o/r failed":              "ssh://example.com/o/r failed",
		"other@example.com stays":                           "other@example.com stays",
	} {
		if got := mask(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	mask = NewMasker("https://u%40x:p%23w@example.com:8443/o/r")
	for in, want := range map[string]string{
		"u@x:p#w@example.com:8443: denied": "example.com:8443: denied",
		"u@x:p#w@example.com: denied":      "example.com: denied",
		"u@x@example.com: denied":          "example.com: denied",
	} {
		if got := mask(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMaskerCoversUserinfoStrippedURL(t *testing.T) {
	mask := NewMasker("https://user:pw@example.com/repo?token=secret#frag")
	for in, want := range map[string]string{
		// what git actually prints over HTTP: userinfo gone, query kept, trailing "/"
		"fatal: unable to access 'https://example.com/repo?token=secret#frag/': 403": "fatal: unable to access 'https://example.com/repo/': 403",
		"https://example.com/repo?token=secret":                                      "https://example.com/repo",
		"https://user:pw@example.com/repo?token=secret#frag":                         "https://example.com/repo",
		"user:pw@example.com: denied":                                                "example.com: denied",
		"https://example.com/repo unrelated":                                         "https://example.com/repo unrelated",
	} {
		got := mask(in)
		if got != want || strings.Contains(got, "secret") || strings.Contains(got, "pw") {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	// No userinfo but a secret query: the full-URL replacement covers it,
	// also when git drops a fragment from the configured URL.
	for _, raw := range []string{"https://example.com/repo?token=secret", "https://example.com/repo?token=secret#frag"} {
		mask = NewMasker(raw)
		if got := mask("error: https://example.com/repo?token=secret/"); strings.Contains(got, "secret") {
			t.Errorf("%q: leaked: %q", raw, got)
		}
	}
}

func TestMaskCoversSanitizedSpelling(t *testing.T) {
	tg := Target{URL: "https://user:to\x1b[31mken@example.com/o/r"}
	if got := Scrub("user:token@example.com: denied", tg.Mask); strings.Contains(got, "token") {
		t.Errorf("leaked: %q", got)
	}
}

func TestMaskerLowercasedHost(t *testing.T) {
	for _, raw := range []string{"ssh://tok@EXAMPLE.com/owner/repo", "tok@EXAMPLE.com:owner/repo.git", "ssh://t%6fk@Example.COM:2222/o/r"} {
		mask := NewMasker(raw)
		for _, in := range []string{
			"tok@example.com: Permission denied (publickey).",
			"tok@EXAMPLE.com: Permission denied (publickey).",
			"tok@example.com:2222: Permission denied",
		} {
			if got := mask(in); strings.Contains(got, "tok") {
				t.Errorf("%q / %q: leaked: %q", raw, in, got)
			}
		}
	}
}

func TestMaskerUserinfoCaseSensitive(t *testing.T) {
	mask := NewMasker("ssh://tok@example.com/o/r")
	if got := mask("TOK@example.com is someone else"); got != "TOK@example.com is someone else" {
		t.Errorf("userinfo must match exactly: %q", got)
	}
}

func TestSafeURLNeverLeaksSecret(t *testing.T) {
	for _, raw := range []string{
		"https://user:secret@example.com/o/r",
		"https://user:secret@example.com/%zz/o/r",
		"ssh://user:secret@host/o/r",
		"user:secret@host:o/r",
	} {
		if got := (Target{URL: raw}).SafeURL(); strings.Contains(got, "secret") {
			t.Errorf("%q -> %q leaks secret", raw, got)
		}
	}
}

func TestDest(t *testing.T) {
	tg := Target{Host: "github.com", Owner: "o", Repo: "r"}
	got, err := tg.Dest("/root")
	if err != nil || got != filepath.Join("/root", "github.com", "o", "r") {
		t.Errorf("got %q err=%v", got, err)
	}
	for _, bad := range []Target{{Host: "..", Owner: "o", Repo: "r"}, {Host: "h", Owner: "../x", Repo: "r"}, {Host: "h", Owner: "o", Repo: ""}} {
		if d, err := bad.Dest("/root"); err == nil {
			t.Errorf("%+v: expected error, got %q", bad, d)
		}
	}
}
