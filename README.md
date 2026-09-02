# herdr-hop

A [herdr](https://herdr.dev) plugin for getting to a repository fast: fuzzy-pick a repository, a git worktree, or an already-open workspace and hop to it; type a repository you don't have yet and clone it; pick a branch and open it as a new worktree. One popup.

```
hop> utahta/herdr
 4/180 ────────────────────────────────────────────────────────
 enter  open/switch/clone   tab  fold   ctrl-t  worktree  …
> utahta/herdr-hop           ●   ~/src/github.com/utahta/herdr-hop
  └─ feat                    ●   ~/.herdr/worktrees/herdr-hop/feat
  utahta/herdr-prompt.nvim       ~/src/github.com/utahta/herdr-prompt.nvim
  utahta/herdr-new  clone  https://github.com/utahta/herdr-new.git
```

## What it does

- **Hop** – scans your source directories for repositories (and linked worktrees), merges in the workspaces herdr already has open, and opens the one you pick as a herdr workspace — or switches to it if it is open.
- **Clone** – when what you typed looks like a repository (`owner/repo`, `host/owner/repo`, or a git URL) and you don't have it, a `clone` row appears. Selecting it clones into `ROOT/host/owner/repo` and opens it.
- **Worktree** – `ctrl-t` on a repository shows its local and remote branches; choose one (or type a new name) and herdr creates the worktree and opens it.

Workspaces are labelled `owner/repo`, so a fork and its upstream are easy to tell apart.

## Requirements

- herdr 0.8.0 or later
- Git 2.36 or later (the picker uses `git worktree list --porcelain -z`)
- For the install: `curl` or `wget`, `tar`, `shasum` or `sha256sum`, and OpenSSH 8.1+ (`ssh-keygen`, used to verify the release signature) — all present on current macOS and Linux by default
- Go 1.26 or later is only needed when no prebuilt binary can be used
- Optional: the GitHub CLI (`gh`, logged in) for pull request titles and states on the worktree screen

macOS and Linux (amd64 / arm64).

## Install

```sh
herdr plugin install utahta/herdr-hop
```

This downloads a prebuilt binary from GitHub Releases and verifies it against a signed checksum before use. If your platform has no binary or a verification tool is missing, it builds from source with Go instead.

Or, from a checkout:

```sh
git clone https://github.com/utahta/herdr-hop
cd herdr-hop && go build -o herdr-hop .
herdr plugin link "$PWD"
```

## Configure

herdr-hop needs to know where your repositories live. Ask herdr where the plugin's config directory is and create `config.toml` there:

```sh
herdr plugin config-dir utahta.hop
# e.g. ~/.config/herdr/plugins/config/utahta.hop
```

```toml
# Where `clone` puts repositories, laid out as ROOT/host/owner/repo.
# Also the first place that is scanned. (HERDR_HOP_ROOT overrides this.)
root = "~/src"

# Additional directories to scan for repositories. Optional.
search_paths = ["~/work"]

# How many directory levels below each search path to look. Default 3.
# ROOT is always scanned at least 3 deep so cloned repositories are found.
depth = 3

# URL scheme used for `owner/repo` and `host/owner/repo` input: "https" or "ssh".
clone_protocol = "https"

# Host assumed for `owner/repo` input.
default_host = "github.com"
```

## Keybindings

herdr does not let a plugin declare keys, so add these to herdr's own `config.toml` (by default `~/.config/herdr/config.toml`; herdr follows `XDG_CONFIG_HOME`). Pick any free keys; these don't collide with herdr's defaults:

```toml
[[keys.command]]
key = "prefix+f"
type = "plugin_action"
command = "utahta.hop.open"
description = "hop: repositories / worktrees / workspaces"

[[keys.command]]
key = "prefix+t"
type = "plugin_action"
command = "utahta.hop.worktree"
description = "hop: new worktree"
```

Then `herdr server reload-config`.

Without a key you can still run `herdr plugin action invoke utahta.hop.open`.

## Using it

### The picker (`prefix+f`)

Type to filter; the matched characters light up. Repository and worktree rows read as such from the tree structure; anything else (`workspace`, `clone`, `pull`, …) carries a dim kind tag after its label. The glyph column before the path shows the open state: `●` means a workspace for the row exists (`●2`: two of them), `?` means herdr's state could not be read.

Worktrees are grouped under their repository (`- 2 worktrees`, indented `└─` rows); while the query is empty the repository you invoked the picker from comes first, followed by the repositories that are open as a workspace, and `tab` folds or unfolds the selected group. Typing keeps the grouping but ignores the fold state, so folded worktrees always stay reachable: a repository match brings all its worktrees, a worktree or branch match brings its repository, and a multi-word query like `myrepo mybranch` (in either order) narrows a repository down to the matching worktree. Clearing the query brings the fold state back.

| Key | Action |
|---|---|
| `enter` | Open the repository as a workspace, switch to it if it is open, open the worktree, or clone (on a `clone` row) |
| `tab` | Fold / unfold the selected repository's worktree group (empty query only) |
| `ctrl-t` | Create a worktree from the selected repository (or the selected worktree's repository) |
| `ctrl-n` | Create a new workspace even if one is already open (repository rows only) |
| `ctrl-d` | Delete the selected worktree's checkout, after a `y` (worktree rows only; the branch is kept) |
| `ctrl-r` | Rescan |
| `esc` | Close (or cancel a running clone) |

The `clone` row appears only when the repository is not found among your checkouts. Checkouts are recognised by their remotes, so pasting `https://github.com/acme/api` finds yours even if it lives under a different name or path.

If herdr's state cannot be read, rows are marked `?` and `enter` will not create anything. On a repository row `ctrl-n` creates a workspace anyway; worktree rows only offer `ctrl-r` to retry.

`ctrl-d` on a worktree row asks `delete worktree <branch>? (y/N)` in the input line; `y` re-reads herdr's state at that moment and removes the checkout (through herdr when a workspace is open for it, which also closes that workspace; through `git worktree remove` otherwise) and reloads the list (query cleared) with the cursor on the worktree's repository. Any other key keeps it. The branch is never deleted. Refused, with a hint: a checkout with modified or untracked files, the worktree you invoked the picker from, any worktree some other workspace's pane sits in (any pane of any tab, at the root or in a subdirectory) or that several workspaces are open on — close those first — every deletion while herdr's snapshot could not be read or reports no directory for some pane, since who is where is then unknown, and a worktree that changed while the question was on screen (removed and re-created for another branch, say) — reload and confirm again. A plain workspace whose pane merely sits in the checkout is not the worktree's own workspace and blocks the removal like any other.

### The worktree screen (`ctrl-t`, or `prefix+t`)

`prefix+t` opens this screen directly for the repository you are in (the workspace's repository, a worktree's main checkout, or the repository containing the pane's directory); `esc` falls back to a repository picker for choosing another one.

| Key | Action |
|---|---|
| `enter` | Local branch: check it out in a new worktree. Remote branch: asks for the local branch name (prefilled), then creates it from the remote. `new` row (the first row; selected while the query is empty or matches nothing): create the typed name — or an automatic `wt/YYYYMMDD-HHMM` — from `HEAD` |
| `ctrl-f` | `git fetch --all --prune` and reload |
| `ctrl-r` | Reload branches |
| `esc` | Back |

When you keep a remote branch's own name, the new local branch tracks it. Give it a different name and it is created from the remote without tracking. Branches already checked out in a worktree are not listed — this screen only creates worktrees; open existing ones from the picker. Typing such a name anyway is refused with a hint pointing at its worktree.

Worktrees land where herdr keeps them (`~/.herdr/worktrees` by default) and are grouped under the repository's workspace.

### Pull requests

Paste a GitHub pull request URL (`…/pull/N`) or GitLab merge request URL (`…/-/merge_requests/N`) into the picker, then select the `pull` row to open it in a new worktree.

herdr-hop looks for a non-default remote branch whose tip matches the PR head. A unique match is checked out as a tracking branch, so later PR updates arrive with a plain `git pull`; if several branches match, you are asked which one to use. This is a heuristic — verify the selected branch when working with forks or deleted source branches.

If no suitable branch is found, the PR is opened on an untracked `pr/N` branch. Pasting the same URL again refreshes the fetched PR head and shows the `git merge` command that updates the existing worktree.

When several checkouts match the repository, one `pull` row is shown for each. If none exists, a `clone+pull` row clones the repository first.

On the worktree screen, branches matching PR heads carry their PR number in a column right of the branch names, loaded in the background, refreshed with `ctrl-f`, and searchable (type `#12`).

With the [GitHub CLI](https://cli.github.com) (`gh`) installed and logged in to the repository's host, `gh api graphql` calls per repository (one per 100 PRs) add each PR's title, without delaying the list. Colour means "still alive": an open PR has a green number and a plain title, a `draft` a green number and a dimmed title, and a `merged` or `closed` PR is dimmed throughout. Numbers are dim until the details arrive, so nothing lights up only to sink again; without `gh`, or for a host `gh` has no account for (GitLab, an enterprise host you have not logged in to), they simply stay that way. Titles are searched by words: `main loop` finds "Rework the main loop"; name and number matches rank first.

Two guards protect the token `gh` would send: details are fetched over https only (a remote on plain HTTP gets numbers only), and `GH_ENTERPRISE_TOKEN` / `GITHUB_ENTERPRISE_TOKEN`, which `gh` would present to *any* host, are passed on only for the host named in `GH_HOST`. Credentials stored by `gh auth login` are per host and used as usual.

## Notes on safety

Credentials that appear in remote URLs are masked, and terminal control sequences are stripped, from everything herdr-hop displays or logs. Cancelling a clone or fetch terminates git and every helper it started.

Logs go to `herdr-hop.log` in the plugin state directory (`herdr plugin config-dir` shows the config directory; state lives beside it, by default `~/.local/state/herdr/plugins/utahta.hop/`).

## Development

```sh
go test -race ./...
go build -o herdr-hop . && herdr plugin link "$PWD"
```

`herdr-hop tui --mode hop|worktree` is what the popup runs; `herdr-hop open
--mode …` is what the actions run to open that popup.

## License

MIT
