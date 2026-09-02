package clone

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScrubMasksSplitCredentials(t *testing.T) {
	mask := NewMasker("https://user:token@host/o/r")
	in := "fatal: user:\x1b[31mtoken@host\x1b[0m denied https://user:tok\x1b[1men@host/o/r"
	got := Scrub(in, mask)
	if strings.Contains(got, "token") || strings.Contains(got, "\x1b") {
		t.Errorf("leaked: %q", got)
	}
	if got != "fatal: host denied https://host/o/r" {
		t.Errorf("got %q", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain text":                            "plain text",
		"tab\tkept":                             "tab\tkept",
		"\x1b[2Jcleared":                        "cleared",
		"\x1b[31merror\x1b[0m: bad":             "error: bad",
		"\x1b]52;c;c2VjcmV0\x07after":           "after",
		"\x1b]0;evil title\x1b\\after":          "after",
		"\x1b]52;c;unterminated":                "",
		"a\rb\nc":                               "abc",
		"bell\x07 del\x7f":                      "bell del",
		"c1 \u009b31m csi \u009d0;t\u009c ok":   "c1  csi  ok",
		"\x1bM reverse index":                   " reverse index",
		"\x1bP dcs payload \x1b\\ visible":      " visible",
		"日本語 ok":                                "日本語 ok",
		"fatal: unable to access 'https://x/r'": "fatal: unable to access 'https://x/r'",
		// raw 8-bit C1 bytes (invalid UTF-8) must not survive the fast path
		string([]byte{0x9b, '2', 'J'}) + "after":            "after",
		string([]byte{'x', 0x9d, '0', ';', 't', 0x9c, 'y'}): "xy",
		string([]byte{'a', 0xff, 'b'}):                      "ab", // stray invalid byte dropped
		"\u009b2Jafter":                                     "after",
	}
	for in, want := range map[string]string{
		"a\u202eb":          "ab",  // RLO
		"a\u2066b\u2069c":   "abc", // LRI ... PDI
		"zero\u200bwidth":   "zerowidth",
		"soft\u00adhyphen":  "softhyphen",
		"emoji 👍🏽 ok":       "emoji 👍🏽 ok",
		"e\u0301 combining": "e\u0301 combining",
		"한국어 ok":            "한국어 ok",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if got := Sanitize("日本語\x1b[31m赤\x1b[0m"); got != "日本語赤" {
		t.Errorf("multibyte: %q", got)
	}
	if !utf8.ValidString(Sanitize(string([]byte{0xe6, 0x97, 0xa5, 0x9b, 0x32, 0x4a}))) {
		t.Error("output must be valid UTF-8")
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
