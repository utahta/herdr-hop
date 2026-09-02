package herdr

// Snapshot is the subset of `herdr api snapshot` this plugin uses.
type Snapshot struct {
	Workspaces []Workspace `json:"workspaces"`
	Layouts    []Layout    `json:"layouts"`
	Panes      []Pane      `json:"panes"`
}

// Workspace mirrors WorkspaceInfo.
type Workspace struct {
	ID          string             `json:"workspace_id"`
	Label       string             `json:"label"`
	Number      int                `json:"number"`
	ActiveTabID string             `json:"active_tab_id"`
	Focused     bool               `json:"focused"`
	Worktree    *WorkspaceWorktree `json:"worktree"`
}

// WorkspaceWorktree mirrors WorkspaceWorktreeInfo.
type WorkspaceWorktree struct {
	CheckoutPath     string `json:"checkout_path"`
	RepoRoot         string `json:"repo_root"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

// Layout mirrors the per-tab layout entry.
type Layout struct {
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	FocusedPaneID string `json:"focused_pane_id"`
}

// Pane mirrors PaneInfo. Cwd fields are nullable.
type Pane struct {
	ID            string  `json:"pane_id"`
	TabID         string  `json:"tab_id"`
	WorkspaceID   string  `json:"workspace_id"`
	Cwd           *string `json:"cwd"`
	ForegroundCwd *string `json:"foreground_cwd"`
}

// WorktreeList is the result of `herdr worktree list --cwd <repo>`.
type WorktreeList struct {
	Source struct {
		RepoRoot string `json:"repo_root"`
		// SourceWorkspaceID is the workspace herdr keeps for the main checkout
		// (the "parent" that worktree workspaces are grouped under), if open.
		SourceWorkspaceID *string `json:"source_workspace_id"`
	} `json:"source"`
	Worktrees []Worktree `json:"worktrees"`
}

// Worktree mirrors WorktreeInfo.
type Worktree struct {
	Path             string  `json:"path"`
	Branch           string  `json:"branch"`
	IsDetached       bool    `json:"is_detached"`
	Label            string  `json:"label"`
	IsLinkedWorktree bool    `json:"is_linked_worktree"`
	IsPrunable       bool    `json:"is_prunable"`
	OpenWorkspaceID  *string `json:"open_workspace_id"`
}
