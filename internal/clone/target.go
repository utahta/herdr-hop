// Package clone parses clone inputs into a URL and a destination under ROOT.
package clone

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Target is a parsed clone request.
type Target struct {
	Host, Owner, Repo string
	// URL is what to pass to git clone.
	URL string
}

// ID is the repository identity "host/owner/repo" (lower-cased) used to
// decide whether a clone target already exists locally.
func (t Target) ID() string {
	return strings.ToLower(t.Host + "/" + t.Owner + "/" + t.Repo)
}

// SafeURL is URL with any userinfo (user:token@) removed. Use it for logs
// and display; pass URL itself only to git.
func (t Target) SafeURL() string { return Redact(t.URL) }

// Mask removes this target's credentials from s. See NewMasker. The URL is
// registered both as given and after Sanitize, because output is sanitized
// before masking and a control sequence inside the typed credential would
// otherwise leave its sanitized spelling unmasked.
func (t Target) Mask(s string) string {
	s = NewMasker(t.URL)(s)
	if su := Sanitize(t.URL); su != t.URL {
		s = NewMasker(su)(s)
	}
	return s
}

// NewMasker returns a function that removes the credentials of rawURL from a
// line of git/ssh output. It is the single implementation used both at the
// git boundary and in the UI. Known strings are replaced, in this order:
//
//  1. the full URL                 tok@host:o/r.git   -> host:o/r.git
//  2. every "userinfo@host" form   tok@host: Permission denied -> host: Permission denied
//
// (2) matters because ssh diagnostics do not contain the repository
// path. Both the raw userinfo (as typed, e.g. "tok%33n") and its
// percent-decoded form ("tok3n", which is what ssh actually prints) are
// registered, as are "user:password" and bare "user"; the host is matched
// with and without its port and in any letter case (DNS names are
// case-insensitive and ssh prints them lower-cased). Only these known
// strings are replaced; unrelated "@" in the output are left alone.
func NewMasker(rawURL string) func(string) string {
	safe := Redact(rawURL)
	// git's HTTP transport reports a URL with the userinfo already stripped but the
	// query and fragment intact ("https://host/repo?token=secret/"). Such
	// intermediate spellings match neither the full URL nor "user@host", so
	// they are registered too and replaced by the safe form. Replacing the
	// substring also removes a query that git printed with a trailing "/".
	// Fragments are never sent, so git may print any of these spellings
	// without one; register each variant with and without its fragment.
	var mids []string
	for _, mid := range append([]string{rawURL}, userinfoStripped(rawURL)...) {
		for _, v := range []string{mid, fragmentStripped(mid)} {
			if v != rawURL && v != safe && !slices.Contains(mids, v) {
				mids = append(mids, v)
			}
		}
	}
	hosts, forms := credentialForms(rawURL)
	// One pattern per (userinfo, host) pair. The userinfo is matched exactly
	// (passwords are case-sensitive; do not over-match unrelated text), the
	// host case-insensitively (DNS names are; ssh and git may print them in
	// any case). The host is kept via the capture group.
	var pats []*regexp.Regexp
	for _, ui := range forms {
		for _, h := range hosts {
			pats = append(pats, regexp.MustCompile(regexp.QuoteMeta(ui)+"@((?i:"+regexp.QuoteMeta(h)+"))"))
		}
	}
	return func(line string) string {
		if safe != rawURL {
			line = strings.ReplaceAll(line, rawURL, safe)
		}
		for _, mid := range mids {
			line = strings.ReplaceAll(line, mid, safe)
		}
		for _, p := range pats {
			line = p.ReplaceAllString(line, "$1")
		}
		return line
	}
}

// fragmentStripped returns the scheme URL without its "#fragment" part
// (unchanged when there is none or the URL is not a scheme URL).
func fragmentStripped(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	if before, _, ok := strings.Cut(s, "#"); ok {
		return before
	}
	return s
}

// userinfoStripped returns the spellings of a scheme URL with only the
// userinfo removed (query and fragment kept): the net/url rendering and a
// plain string edit of the raw text, which can differ in escaping. Empty
// for non-scheme URLs or URLs without userinfo.
func userinfoStripped(raw string) []string {
	if !strings.Contains(raw, "://") {
		return nil
	}
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = nil
		add(u.String())
	}
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
			add(scheme + "://" + rest[at+1:])
		}
	}
	return out
}

// credentialForms returns the host spellings of rawURL (with and without
// port; case is handled by the caller) and every userinfo spelling that may
// show up in output, longest first so "user:pw" is replaced before "user".
// Empty when there is no userinfo. Both lists are de-duplicated.
func credentialForms(rawURL string) (hosts, forms []string) {
	add := func(dst *[]string, s string) {
		if s == "" {
			return
		}
		if slices.Contains(*dst, s) {
			return
		}
		*dst = append(*dst, s)
	}
	if strings.Contains(rawURL, "://") {
		u, err := url.Parse(rawURL)
		if err != nil || u.User == nil {
			return nil, nil
		}
		add(&hosts, u.Host)       // host:port as git prints it
		add(&hosts, u.Hostname()) // host alone as ssh prints it
		// Raw (as typed, possibly percent-encoded) form, sliced from the raw
		// string: url.Userinfo.String() re-encodes and would normalise it.
		if _, after, ok := strings.Cut(rawURL, "://"); ok {
			rest := after
			end := strings.IndexAny(rest, "/?#")
			if end < 0 {
				end = len(rest)
			}
			if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
				add(&forms, rest[:at])
			}
		}
		user := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			add(&forms, user+":"+pw) // decoded user:password
		}
		add(&forms, user) // decoded user alone
		sortByLenDesc(forms)
		return hosts, forms
	}
	if m := reScp.FindStringSubmatch(rawURL); m != nil && m[1] != "" && !strings.Contains(m[2], "/") {
		return []string{m[2]}, []string{strings.TrimSuffix(m[1], "@")}
	}
	return nil, nil
}

func sortByLenDesc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// RedactedInvalid is what Redact returns when a scheme URL cannot be parsed:
// fail closed rather than echo a string that may carry credentials.
const RedactedInvalid = "<invalid URL>"

// Redact strips userinfo from scheme URLs (https://user:token@host/...) and
// scp-like URLs (user@host:path). For scheme URLs the query and fragment
// are dropped as well: a remote configured as https://host/repo?token=...
// carries its credential there, and which parameter is secret cannot be
// known. Invariant: a scheme URL is never returned unchanged unless it
// parsed and had no userinfo, query or fragment.
func Redact(raw string) string {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return RedactedInvalid
		}
		u.User = nil
		u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
		return u.String()
	}
	// scp-like or anything else: drop everything up to the last "@" that
	// precedes the first "/" (covers user@host:path and user:pw@host:path).
	head, tail := raw, ""
	if i := strings.Index(raw, "/"); i >= 0 {
		head, tail = raw[:i], raw[i:]
	}
	if at := strings.LastIndex(head, "@"); at >= 0 {
		return head[at+1:] + tail
	}
	return raw
}

// ErrOutsideRoot is returned when the destination would escape root.
var ErrOutsideRoot = errors.New("destination is outside the clone root")

// Dest returns ROOT/host/owner/repo, refusing anything that would resolve
// outside root (defense in depth on top of component validation in Parse).
func (t Target) Dest(root string) (string, error) {
	for _, s := range []string{t.Host, t.Owner, t.Repo} {
		if !safeComponent(s) {
			return "", ErrInvalid
		}
	}
	dest := filepath.Join(root, t.Host, t.Owner, t.Repo)
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrOutsideRoot
	}
	return dest, nil
}

// safeComponent reports whether s is usable as a single path component.
func safeComponent(s string) bool {
	return reName.MatchString(s) && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

var (
	ErrEmpty   = errors.New("empty input")
	ErrInvalid = errors.New("expected owner/repo, host/owner/repo, or a git URL")
	// ErrQueryOrFragment: such URLs are not something git clone needs, and a
	// "/" inside the query would otherwise be mistaken for the repository path.
	ErrQueryOrFragment = errors.New("clone URL must not contain a query or fragment")

	reScheme = regexp.MustCompile(`^([a-z][a-z0-9+.-]*)://([^/@]+@)?([^/:]+)(:\d+)?/(.+)$`)
	reScp    = regexp.MustCompile(`^([^@/:]+@)?([^/:]+):([^/].*)$`)
	reName   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Parse accepts:
//   - owner/repo                (defaultHost is used)
//   - host/owner/repo
//   - https://host/owner/repo[.git], ssh://[git@]host/owner/repo[.git], git://...
//   - [git@]host:owner/repo[.git]  (scp-like)
//
// For the shorthand forms the URL is built from protocol ("https" or "ssh").
// Full URLs are passed to git unchanged.
func Parse(input, defaultHost, protocol string) (Target, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return Target{}, ErrEmpty
	}
	if reScheme.MatchString(in) {
		// Use the parsed components, not regexp substrings: only u.Path may
		// name the repository, and anything net/url cannot parse (e.g. bad
		// percent-encoding) is rejected because git could not use it either
		// and Redact must be able to parse it.
		u, err := url.Parse(in)
		if err != nil {
			return Target{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return Target{}, ErrQueryOrFragment
		}
		owner, repo, err := splitOwnerRepo(u.Path)
		if err != nil {
			return Target{}, err
		}
		return validated(Target{Host: u.Hostname(), Owner: owner, Repo: repo, URL: in})
	}
	if m := reScp.FindStringSubmatch(in); m != nil && !strings.Contains(m[2], "/") {
		owner, repo, err := splitOwnerRepo(m[3])
		if err != nil {
			return Target{}, err
		}
		return validated(Target{Host: m[2], Owner: owner, Repo: repo, URL: in})
	}
	parts := strings.Split(strings.Trim(in, "/"), "/")
	var t Target
	switch len(parts) {
	case 2:
		t = Target{Host: defaultHost, Owner: parts[0], Repo: parts[1]}
	case 3:
		t = Target{Host: parts[0], Owner: parts[1], Repo: parts[2]}
	default:
		return Target{}, ErrInvalid
	}
	t.Repo = strings.TrimSuffix(t.Repo, ".git")
	if _, err := validated(t); err != nil {
		return Target{}, err
	}
	switch protocol {
	case "ssh":
		t.URL = fmt.Sprintf("git@%s:%s/%s.git", t.Host, t.Owner, t.Repo)
	default:
		t.URL = fmt.Sprintf("https://%s/%s/%s.git", t.Host, t.Owner, t.Repo)
	}
	return t, nil
}

// validated applies the same component checks to every input form so that a
// URL like https://../o/r can never produce a host of "..".
func validated(t Target) (Target, error) {
	for _, s := range []string{t.Host, t.Owner, t.Repo} {
		if !safeComponent(s) {
			return Target{}, ErrInvalid
		}
	}
	return t, nil
}

func splitOwnerRepo(path string) (string, string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", ErrInvalid
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	for _, s := range []string{owner, repo} {
		if !safeComponent(s) {
			return "", "", ErrInvalid
		}
	}
	return owner, repo, nil
}
