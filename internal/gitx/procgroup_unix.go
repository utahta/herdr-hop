//go:build unix

package gitx

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a cancelled git process group gets to exit after
// SIGTERM before it is SIGKILLed. A variable so tests can shorten it.
var killGrace = 2 * time.Second

// groupProbeInterval is how often the group is checked for having exited
// while the SIGKILL escalation is pending.
const groupProbeInterval = 50 * time.Millisecond

// groupKiller terminates git's whole process group on cancellation.
//
// git spawns helpers (ssh, git-remote-https, ...) that inherit the stderr
// pipe; if they outlived git the progress reader would never see EOF and
// Clone would hang. Killing the group (negative pid) covers them.
//
// Escalation to SIGKILL is decided by whether the *group* still exists
// (kill(-pgid, 0)), not by whether the git leader has been reaped: the leader
// may honour SIGTERM while a helper ignores it. Conversely, once the group is
// gone its id may be reused by unrelated processes, so the group is probed
// frequently and escalation stops the moment it disappears. All state goes
// through mu so a probe/kill cannot race with finish().
type groupKiller struct {
	mu       sync.Mutex
	pgid     int
	finished bool // group known to be gone (or never signalled); no more kills
	stop     chan struct{}
	grace    time.Duration
	kill     func(pid int, sig syscall.Signal) error // syscall.Kill; replaced in tests
}

// setupProcessGroup makes cmd the leader of a new process group and installs
// cancellation handling. The caller must invoke done() after cmd.Wait.
func setupProcessGroup(cmd *exec.Cmd) *groupKiller {
	return setupProcessGroupWith(cmd, false)
}

// setupProcessGroupDetached additionally detaches the command from the
// controlling terminal (a new session): background git/ssh must fail
// instead of prompting on /dev/tty. A session leader is also a process
// group leader, so the group-kill cancellation works unchanged.
func setupProcessGroupDetached(cmd *exec.Cmd) *groupKiller {
	return setupProcessGroupWith(cmd, true)
}

func setupProcessGroupWith(cmd *exec.Cmd, detach bool) *groupKiller {
	k := &groupKiller{grace: killGrace, kill: syscall.Kill}
	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error { return k.cancel(cmd.Process.Pid) }
	// Once git itself has exited (or been killed), do not wait forever for
	// helpers to release the pipes: after this delay Wait closes them.
	cmd.WaitDelay = killGrace + time.Second
	return k
}

// cancel sends SIGTERM to the group and starts the escalation watcher
// unless the group is already gone (ESRCH).
func (k *groupKiller) cancel(pid int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished || k.stop != nil {
		return nil
	}
	k.pgid = -pid
	err := k.kill(k.pgid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		k.finished = true
		return nil // already gone: a normal race, not a failure; no watcher
	}
	if err != nil {
		return err
	}
	k.stop = make(chan struct{})
	go k.watch(k.stop)
	return nil
}

// watch probes the group until it is gone or the grace period elapses, then
// SIGKILLs it if it is still alive.
func (k *groupKiller) watch(stop chan struct{}) {
	deadline := time.NewTimer(k.grace)
	defer deadline.Stop()
	tick := time.NewTicker(groupProbeInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if !k.alive() {
				return
			}
		case <-deadline.C:
			k.escalate()
			return
		}
	}
}

// alive probes the group with signal 0; on ESRCH it marks the killer finished.
func (k *groupKiller) alive() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return false
	}
	if err := k.kill(k.pgid, 0); errors.Is(err, syscall.ESRCH) {
		k.finished = true
		return false
	}
	return true
}

// escalate SIGKILLs the group if it still exists.
func (k *groupKiller) escalate() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return
	}
	if err := k.kill(k.pgid, 0); errors.Is(err, syscall.ESRCH) {
		k.finished = true
		return
	}
	_ = k.kill(k.pgid, syscall.SIGKILL)
	k.finished = true // nothing further will be signalled to this pgid
}

// done is called after Wait has reaped the leader. The leader exiting does
// not mean the group is gone (helpers may remain), so it only re-probes: if
// the group has vanished, escalation stops immediately (no stale-pgid kill);
// otherwise the watcher keeps running until the group exits or is killed.
func (k *groupKiller) done() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stop == nil || k.finished {
		return
	}
	if err := k.kill(k.pgid, 0); errors.Is(err, syscall.ESRCH) {
		k.finished = true
		close(k.stop)
		k.stop = nil
	}
}
