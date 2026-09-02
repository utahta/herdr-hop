package clone

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// PRKind is the hosting flavour a pull request URL was recognised as.
type PRKind int

const (
	PRGitHub PRKind = iota + 1 // .../owner/repo/pull/N
	PRGitLab                   // .../group/.../repo/-/merge_requests/N
)

// PR is a parsed pull/merge request URL.
type PR struct {
	Kind PRKind
	// Number is the PR / MR number.
	Number int
	// RepoURL is the repository URL with the PR part removed, exactly as
	// typed (scheme, userinfo, host:port and the full namespace path are
	// kept). It is what git is given to fetch or clone from.
	RepoURL string
	// Host is the host name (no port) as typed.
	Host string
	// Owner and Repo are the last two path elements: the identity used for
	// clone destinations and the clone row, like clone input.
	Owner, Repo string
	// RepoPath is the comparison identity: normalised authority plus the
	// full namespace path (see RepoPathOf).
	RepoPath string
}

var (
	ErrNotPR = errors.New("not a pull request URL")

	// GitHub: exactly /<owner>/<repo>/pull/<N>. GitLab: any namespace depth,
	// marked by the "/-/" separator. GitLab is tried first: a GitLab
	// namespace may itself contain a "pull/<n>" segment.
	rePRGitHub = regexp.MustCompile(`^/([^/]+/[^/]+)/pull/([0-9]+)(?:/.*)?$`)
	rePRGitLab = regexp.MustCompile(`^(.*?)/-/merge_requests/([0-9]+)(?:/.*)?$`)
)

// isGitLabHost reports whether the host is recognisably GitLab: gitlab.com,
// its subdomains, or a self-hosted instance whose name contains "gitlab"
// (the common convention).
func isGitLabHost(host string) bool {
	return strings.Contains(strings.ToLower(host), "gitlab")
}

// ParsePR recognises a GitHub pull request URL (https://host/owner/repo/pull/N)
// or a GitLab merge request URL (https://host/group/.../repo/-/merge_requests/N).
// Trailing path segments, query and fragment after the number are ignored.
// Anything else yields ErrNotPR so the caller can fall back to plain clone
// input.
func ParsePR(input string) (PR, error) {
	in := strings.TrimSpace(input)
	if in == "" || !strings.Contains(in, "://") {
		return PR{}, ErrNotPR
	}
	u, err := url.Parse(in)
	if err != nil || u.Host == "" {
		return PR{}, ErrNotPR
	}
	// Match on the path only; the number's trailing junk may live in the
	// query/fragment, which url.Parse has already separated.
	var kind PRKind
	var m []string
	if m = rePRGitLab.FindStringSubmatch(u.Path); m != nil {
		kind = PRGitLab
	} else if m = rePRGitHub.FindStringSubmatch(u.Path); m != nil && !isGitLabHost(u.Hostname()) {
		// On a host known to be GitLab, ".../pull/12" is not a pull request
		// but a valid project URL (namespace ".../pull", project "12"), so
		// the GitHub reading must not apply there. A self-hosted GitLab
		// whose host name does not say so cannot be told apart from GHES;
		// for those, paste the /-/merge_requests/ form GitLab displays.
		kind = PRGitHub
	} else {
		return PR{}, ErrNotPR
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return PR{}, ErrNotPR
	}
	repoPath := strings.Trim(m[1], "/")
	owner, repo, err := splitOwnerRepo(repoPath)
	if err != nil {
		return PR{}, ErrNotPR
	}
	ru := *u
	ru.Path = "/" + repoPath
	ru.RawPath, ru.RawQuery, ru.ForceQuery, ru.Fragment, ru.RawFragment = "", "", false, "", ""
	pr := PR{Kind: kind, Number: n, RepoURL: ru.String(), Host: u.Hostname(), Owner: owner, Repo: repo}
	if _, err := validated(Target{Host: pr.Host, Owner: owner, Repo: repo}); err != nil {
		return PR{}, ErrNotPR
	}
	pr.RepoPath = RepoPathOf(pr.RepoURL)
	return pr, nil
}

// HeadRef is the ref on the remote that holds the PR's head commit.
func (p PR) HeadRef() string {
	if p.Kind == PRGitLab {
		return fmt.Sprintf("refs/merge-requests/%d/head", p.Number)
	}
	return fmt.Sprintf("refs/pull/%d/head", p.Number)
}

// BranchName is the local branch created for the PR.
func (p PR) BranchName() string { return fmt.Sprintf("pr/%d", p.Number) }

// Provenance identifies which repository's PR a local pr/N branch was
// created for: "<RepoPath>#<N>". It is stored in the branch's git config
// (see ProvenanceKey) so that PR #12 of a fork and PR #12 of its upstream —
// both named pr/12 — are never mistaken for one another.
func (p PR) Provenance() string { return fmt.Sprintf("%s#%d", p.RepoPath, p.Number) }

// ProvenanceKey is the git config key holding Provenance for the branch.
func (p PR) ProvenanceKey() string { return "branch." + p.BranchName() + ".hop-pr" }

// RefKey identifies the repository inside ref names: the first 12 hex
// digits of SHA-256(RepoPath). URL-derived text is never embedded in a ref
// directly (a "repo.lock" path element or a "host:port" would be rejected
// by git check-ref-format); hex digits always form a valid ref component.
func (p PR) RefKey() string { return RefKeyOf(p.RepoPath) }

// RefKeyOf is RefKey for an already normalised RepoPath.
func RefKeyOf(repoPath string) string {
	sum := sha256.Sum256([]byte(repoPath))
	return hex.EncodeToString(sum[:])[:12]
}

// LocalRef is the fully qualified ref the PR head is fetched into. It lives
// in its own namespace so that `git fetch --prune` (which only prunes
// refs/remotes/<remote>/*) leaves it alone, and it does not depend on the
// repository-global FETCH_HEAD that any concurrent fetch may overwrite.
func (p PR) LocalRef() string { return fmt.Sprintf("refs/hop/pr/%s/%d", p.RefKey(), p.Number) }

// Label is the display form, e.g. "github.com/owner/repo #123".
func (p PR) Label() string { return fmt.Sprintf("%s/%s/%s #%d", p.Host, p.Owner, p.Repo, p.Number) }

// Target returns the clone target for the repository (used by clone+pull).
func (p PR) Target() Target {
	return Target{Host: p.Host, Owner: p.Owner, Repo: p.Repo, URL: p.RepoURL}
}

// RemoteIdentity parses a configured remote URL leniently (userinfo, query
// and fragment are accepted and ignored) and returns the two identities of
// the repository it points at: repoPath (RepoPathOf) and id (host/owner/repo
// lower-cased, like Target.ID). ok is false when the URL cannot be read as a
// repository URL. The URL itself is never altered; callers keep passing the
// original to git.
func RemoteIdentity(raw string) (repoPath, id string, ok bool) {
	host, path := splitRepoURL(raw)
	if host == "" || path == "" {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return "", "", false
	}
	return RepoPathOf(raw), strings.ToLower(hostOnly(host) + "/" + owner + "/" + repo), true
}

// ForgeRepo is a repository as a forge API client addresses it: where the
// API lives (Scheme, Host, Port) and which repository to ask about (Owner,
// Name). It keeps apart what the comparison identities (RepoPathOf,
// RemoteIdentity) fold together, since an enterprise host on plain HTTP or
// on :8443 is a different endpoint than the same name on https://…:443.
// The zero value is not a valid repository; it is comparable, so it can key
// a map.
type ForgeRepo struct {
	Scheme string // "http" or "https"
	Host   string // lower-cased host name, no port
	Port   string // "" when the scheme's default port is used
	Owner  string
	Name   string
}

// ParseForgeRepo derives the API address of a remote URL's repository. The
// scheme and a non-default port are taken from http(s) URLs; an SSH or git
// URL says nothing about where the API lives, so https on the default port
// is assumed. Owner and Name are the last two path components. ok is false
// when raw is not a repository URL.
func ParseForgeRepo(raw string) (repo ForgeRepo, ok bool) {
	authority, path := splitRepoURL(raw)
	if authority == "" || path == "" {
		return ForgeRepo{}, false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if len(parts) < 2 {
		return ForgeRepo{}, false
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return ForgeRepo{}, false
	}
	repo = ForgeRepo{Scheme: "https", Host: strings.ToLower(hostOnly(authority)), Owner: owner, Name: name}
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		switch scheme := strings.ToLower(u.Scheme); scheme {
		case "http", "https":
			repo.Scheme = scheme
			if p := u.Port(); p != "" && defaultPorts[scheme] != p {
				repo.Port = p
			}
		}
	}
	return repo, true
}

// APIBase is the origin the forge's API is reached at, "scheme://host[:port]".
func (r ForgeRepo) APIBase() string {
	host := r.Host
	if r.Port != "" {
		host += ":" + r.Port
	}
	return r.Scheme + "://" + host
}

// String renders the repository for logs: "scheme://host[:port]/owner/name".
func (r ForgeRepo) String() string {
	return r.APIBase() + "/" + r.Owner + "/" + r.Name
}

// RepoPathOf normalises a repository URL into the comparison identity used
// to decide "is this the same repository": lower-cased host name, an
// explicit non-default port, and the lower-cased full namespace path
// without a trailing "/" or ".git". Scheme, userinfo, query and fragment are
// not part of it, so https://Host:443/Acme/Api.git, ssh://git@host/acme/api
// and git@host:acme/api are all "host/acme/api"; a non-default port is kept
// so that two servers on different ports stay distinct. Empty when raw is
// not a repository URL.
func RepoPathOf(raw string) string {
	host, path := splitRepoURL(raw)
	if host == "" || path == "" {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

var defaultPorts = map[string]string{"http": "80", "https": "443", "ssh": "22", "git": "9418"}

// splitRepoURL returns the authority (host plus non-default port, userinfo
// removed) and the path of a scheme or scp-like repository URL.
func splitRepoURL(raw string) (authority, path string) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", ""
		}
		authority = u.Hostname()
		if p := u.Port(); p != "" && defaultPorts[strings.ToLower(u.Scheme)] != p {
			authority += ":" + p
		}
		return authority, u.Path
	}
	if m := reScp.FindStringSubmatch(raw); m != nil && !strings.Contains(m[2], "/") {
		return m[2], m[3]
	}
	return "", ""
}

func hostOnly(authority string) string {
	if h, _, ok := strings.Cut(authority, ":"); ok {
		return h
	}
	return authority
}
