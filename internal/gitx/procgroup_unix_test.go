//go:build unix

package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeGroup simulates a process group for the killer: alive until SIGKILL,
// or until the test flips it.
type fakeGroup struct {
	mu    sync.Mutex
	alive bool
	calls []syscall.Signal
	err   error // returned for SIGTERM
}

func (g *fakeGroup) kill(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sig != 0 {
		g.calls = append(g.calls, sig)
	}
	if sig == syscall.SIGTERM && g.err != nil {
		return g.err
	}
	if !g.alive {
		return syscall.ESRCH
	}
	if sig == syscall.SIGKILL {
		g.alive = false
	}
	return nil
}

func (g *fakeGroup) signals() []syscall.Signal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]syscall.Signal(nil), g.calls...)
}

func (g *fakeGroup) exit() { g.mu.Lock(); g.alive = false; g.mu.Unlock() }

func newTestKiller(g *fakeGroup) *groupKiller {
	return &groupKiller{grace: 200 * time.Millisecond, kill: g.kill}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestGroupKillerStopsWhenGroupExitsOnSIGTERM(t *testing.T) {
	g := &fakeGroup{alive: true}
	k := newTestKiller(g)
	if err := k.cancel(1234); err != nil {
		t.Fatal(err)
	}
	g.exit() // whole group honoured SIGTERM
	k.done()
	time.Sleep(2 * k.grace)
	if got := g.signals(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Errorf("no SIGKILL expected: %v", got)
	}
}

func TestGroupKillerKeepsEscalationWhileHelperLives(t *testing.T) {
	g := &fakeGroup{alive: true}
	k := newTestKiller(g)
	if err := k.cancel(1234); err != nil {
		t.Fatal(err)
	}
	k.done() // leader reaped, but the group (helper) is still alive
	waitFor(t, func() bool {
		s := g.signals()
		return len(s) == 2 && s[1] == syscall.SIGKILL
	}, "SIGKILL escalation after leader exit")
	g.mu.Lock()
	alive := g.alive
	g.mu.Unlock()
	if alive {
		t.Error("group should be dead after SIGKILL")
	}
}

func TestGroupKillerStopsWhenGroupExitsAfterLeader(t *testing.T) {
	g := &fakeGroup{alive: true}
	k := newTestKiller(g)
	if err := k.cancel(1234); err != nil {
		t.Fatal(err)
	}
	k.done()
	time.Sleep(k.grace / 4)
	g.exit() // helper exits on its own before the deadline
	time.Sleep(2 * k.grace)
	if got := g.signals(); len(got) != 1 {
		t.Errorf("no SIGKILL expected once the group vanished: %v", got)
	}
}

func TestGroupKillerNoWatcherOnESRCH(t *testing.T) {
	g := &fakeGroup{alive: false, err: syscall.ESRCH}
	k := newTestKiller(g)
	if err := k.cancel(1234); err != nil {
		t.Fatalf("ESRCH must be treated as success, got %v", err)
	}
	if k.stop != nil || !k.finished {
		t.Error("no watcher must run when the group is already gone")
	}
}

func TestGroupKillerRaceDoneVsEscalate(t *testing.T) {
	for range 200 {
		g := &fakeGroup{alive: true}
		k := newTestKiller(g)
		k.grace = time.Hour
		if err := k.cancel(1); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); g.exit() }()
		go func() { defer wg.Done(); k.escalate() }()
		go func() { defer wg.Done(); k.done() }()
		wg.Wait()
		k.mu.Lock()
		if k.stop != nil {
			close(k.stop)
			k.stop = nil
		}
		k.mu.Unlock()
		k.escalate()
		for _, s := range g.signals()[1:] {
			if s != syscall.SIGKILL {
				t.Fatalf("unexpected signal %v", s)
			}
		}
		if n := len(g.signals()); n > 2 {
			t.Fatalf("double kill: %v", g.signals())
		}
	}
}

// TestCloneCancelKillsHelpers simulates git spawning a helper that keeps
// stderr open (like ssh / git-remote-https). Cancelling must terminate both
// processes and Clone must return promptly.
func TestCloneCancelKillsHelpers(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"sleep 300 &\n" + // helper inheriting stderr
		"echo \"$$ $!\" > " + pidFile + "\n" +
		"echo 'Cloning...' >&2\n" +
		"wait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	progressed := make(chan struct{}, 1)
	go func() {
		got <- (&Git{Bin: bin}).Clone(ctx, "https://example.com/o/r", filepath.Join(dir, "d"), func(string) {
			select {
			case progressed <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-progressed:
	case <-time.After(5 * time.Second):
		t.Fatal("git never started")
	}
	cancel()
	var err error
	select {
	case err = <-got:
	case <-time.After(killGrace + 5*time.Second):
		t.Fatal("Clone did not return after cancel")
	}
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v", err)
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for pid := range strings.FieldsSeq(strings.TrimSpace(string(b))) {
		deadline := time.Now().Add(killGrace + 3*time.Second)
		for time.Now().Before(deadline) {
			if exec.Command("kill", "-0", pid).Run() != nil {
				break // gone
			}
			time.Sleep(50 * time.Millisecond)
		}
		if exec.Command("kill", "-0", pid).Run() == nil {
			exec.Command("kill", "-9", pid).Run()
			t.Errorf("process %s survived cancellation", pid)
		}
	}
}

// Real processes: the leader honours SIGTERM but its helper ignores it.
// Escalation must SIGKILL the helper and Clone must return.
func TestCloneCancelEscalatesForStubbornHelper(t *testing.T) {
	old := killGrace
	killGrace = 300 * time.Millisecond
	defer func() { killGrace = old }()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"sh -c 'trap \"\" TERM; echo helper $$ >> " + pidFile + "; sleep 300' &\n" +
		"echo leader $$ >> " + pidFile + "\n" +
		"echo 'Cloning...' >&2\n" +
		"wait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	started := make(chan struct{}, 1)
	go func() {
		got <- (&Git{Bin: bin}).Clone(ctx, "https://example.com/o/r", filepath.Join(dir, "d"), func(string) {
			select {
			case started <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("git never started")
	}
	time.Sleep(100 * time.Millisecond) // let the helper register its pid
	cancel()
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(killGrace + 5*time.Second):
		t.Fatal("Clone did not return: stubborn helper kept the pipe open")
	}
	b, _ := os.ReadFile(pidFile)
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		waitFor(t, func() bool { return exec.Command("kill", "-0", f[1]).Run() != nil }, f[0]+" to die")
	}
}

func TestRunCtxCancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	pidFile := filepath.Join(dir, "pids")
	script := "#!/bin/sh\n" +
		"sh -c 'trap \"\" TERM; echo helper $$ >> " + pidFile + "; sleep 300' &\n" +
		"echo leader $$ >> " + pidFile + "\n" +
		"wait\n"
	os.WriteFile(bin, []byte(script), 0o755)
	old := killGrace
	killGrace = 300 * time.Millisecond
	defer func() { killGrace = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&Git{Bin: bin}).RunCtx(ctx, dir, nil, "ls-remote")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err=%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("RunCtx took %v: helper kept it alive", time.Since(start))
	}
	b, _ := os.ReadFile(pidFile)
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && exec.Command("kill", "-0", f[1]).Run() == nil {
			time.Sleep(50 * time.Millisecond)
		}
		if exec.Command("kill", "-0", f[1]).Run() == nil {
			exec.Command("kill", "-9", f[1]).Run()
			t.Errorf("%s survived", f[0])
		}
	}
}
