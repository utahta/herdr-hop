// Package herdr wraps the herdr CLI ($HERDR_BIN_PATH).
package herdr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Client is the herdr operations this plugin needs. Implemented by CLI; mockable in tests.
type Client interface {
	Snapshot() (*Snapshot, error)
	WorktreeList(repo string) (*WorktreeList, error)
	// WorkspaceCreate creates and focuses a workspace; label may be empty.
	WorkspaceCreate(cwd, label string) error
	WorkspaceFocus(id string) error
	WorktreeOpen(repoRoot, path string) error
	// WorktreeCreate creates a worktree of the repository at repo for branch
	// (checking it out if it is an existing local branch, otherwise creating
	// it from base or HEAD) and opens it as a focused workspace.
	WorktreeCreate(repo, branch, base string) error
	// WorktreeRemove removes the worktree checkout behind an open workspace
	// and closes the workspace. herdr refuses a checkout with modified or
	// untracked files unless force is set. The branch is kept.
	WorktreeRemove(workspaceID string, force bool) error
	// WorkspaceLabel returns the current label of a workspace.
	WorkspaceLabel(id string) (string, error)
	// WorkspaceRename sets a workspace's label.
	WorkspaceRename(id, label string) error
	PluginPaneOpen(pluginID, entrypoint string) error
}

// CLI runs the herdr binary.
type CLI struct {
	Bin string
}

// NewCLI uses $HERDR_BIN_PATH, falling back to "herdr" on PATH.
func NewCLI() *CLI {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	return &CLI{Bin: bin}
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *CLI) run(args ...string) (json.RawMessage, error) {
	cmd := exec.Command(c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// herdr writes the error envelope to stderr on failure (stdout is empty);
	// on success the result envelope is on stdout. Check both.
	for _, out := range [][]byte{stderr.Bytes(), stdout.Bytes()} {
		var env envelope
		if len(bytes.TrimSpace(out)) == 0 || json.Unmarshal(out, &env) != nil {
			continue
		}
		if env.Error != nil {
			return nil, fmt.Errorf("herdr %s: %s (%s)", args[0], env.Error.Message, env.Error.Code)
		}
		if runErr == nil && env.Result != nil {
			return env.Result, nil
		}
	}
	if runErr != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) == 0 {
			msg = bytes.TrimSpace(stdout.Bytes())
		}
		return nil, fmt.Errorf("herdr %s: %w: %s", args[0], runErr, msg)
	}
	return nil, fmt.Errorf("herdr %s: unexpected output: %s", args[0], bytes.TrimSpace(stdout.Bytes()))
}

func (c *CLI) Snapshot() (*Snapshot, error) {
	raw, err := c.run("api", "snapshot")
	if err != nil {
		return nil, err
	}
	var r struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	return &r.Snapshot, nil
}

func (c *CLI) WorktreeList(repo string) (*WorktreeList, error) {
	raw, err := c.run("worktree", "list", "--cwd", repo)
	if err != nil {
		return nil, err
	}
	var r WorktreeList
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse worktree list: %w", err)
	}
	return &r, nil
}

func (c *CLI) WorkspaceCreate(cwd, label string) error {
	args := []string{"workspace", "create", "--cwd", cwd, "--focus"}
	if label != "" {
		args = append(args, "--label", label)
	}
	_, err := c.run(args...)
	return err
}

func (c *CLI) WorkspaceFocus(id string) error {
	_, err := c.run("workspace", "focus", id)
	return err
}

func (c *CLI) WorktreeOpen(repoRoot, path string) error {
	_, err := c.run("worktree", "open", "--cwd", repoRoot, "--path", path, "--focus")
	return err
}

func (c *CLI) WorktreeCreate(repo, branch, base string) error {
	args := []string{"worktree", "create", "--cwd", repo, "--branch", branch, "--focus"}
	if base != "" {
		args = append(args, "--base", base)
	}
	_, err := c.run(args...)
	return err
}

func (c *CLI) WorktreeRemove(workspaceID string, force bool) error {
	args := []string{"worktree", "remove", "--workspace", workspaceID}
	if force {
		args = append(args, "--force")
	}
	_, err := c.run(args...)
	return err
}

func (c *CLI) WorkspaceLabel(id string) (string, error) {
	raw, err := c.run("workspace", "get", id)
	if err != nil {
		return "", err
	}
	var r struct {
		Workspace Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("parse workspace: %w", err)
	}
	return r.Workspace.Label, nil
}

func (c *CLI) WorkspaceRename(id, label string) error {
	_, err := c.run("workspace", "rename", id, label)
	return err
}

func (c *CLI) PluginPaneOpen(pluginID, entrypoint string) error {
	_, err := c.run("plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", entrypoint)
	return err
}
