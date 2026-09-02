//go:build !unix

package gitx

import (
	"os/exec"
	"time"
)

// groupKiller is a no-op where process groups are unavailable.
type groupKiller struct{}

// setupProcessGroup: rely on the default kill of the direct child and on
// WaitDelay to release the pipes. The caller must invoke done() after Wait.
func setupProcessGroup(cmd *exec.Cmd) *groupKiller {
	cmd.WaitDelay = 3 * time.Second
	return &groupKiller{}
}

// setupProcessGroupDetached: no sessions here; same as setupProcessGroup.
func setupProcessGroupDetached(cmd *exec.Cmd) *groupKiller {
	return setupProcessGroup(cmd)
}

func (k *groupKiller) done() {}
