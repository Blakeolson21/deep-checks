package git

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests model the leak that left spinning orphan test binaries behind for
// 17+ hours: a git child that stops making progress and is never bounded. See
// the defaultCommandTimeout comment for the ThreadSanitizer fork mechanism.

// fakeGitDir returns a directory placed first on PATH, for the caller to write
// a fake `git` into. Scripts must use absolute /bin utilities: the child runs
// with NonInteractiveEnv and cannot be assumed to inherit a usable PATH.
func fakeGitDir(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir
}

func writeFakeGit(t *testing.T, binDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake git never recorded a PID at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// reapPIDFile is test hygiene only. It must never be the thing that reaps a
// leak the production code was supposed to bound.
func reapPIDFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// TestRun_BoundsGitGivenADeadlineFreeContext is the core regression. Most
// callers, and every test fixture in this repo, pass context.Background(). A
// git child that never exits under such a context blocked cmd.Wait forever,
// which is what turned a wedged fork into a permanent orphan.
func TestRun_BoundsGitGivenADeadlineFreeContext(t *testing.T) {
	binDir := fakeGitDir(t)
	pidFile := filepath.Join(binDir, "git.pid")
	// exec replaces the shell, so the recorded PID stays git's direct child.
	writeFakeGit(t, binDir, "echo $$ > "+pidFile+"\nexec /bin/sleep 600\n")
	t.Cleanup(func() { reapPIDFile(pidFile) })

	restore := defaultCommandTimeout
	defaultCommandTimeout = 2 * time.Second
	t.Cleanup(func() { defaultCommandTimeout = restore })

	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), t.TempDir(), "status", "--porcelain")
		done <- err
	}()

	gitPID := readPID(t, pidFile)

	const bound = 30 * time.Second
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when git exceeded its bound")
		}
	case <-time.After(bound):
		t.Fatalf("git.Run did not return within %s for a git child that never exits under a deadline-free context: without a default bound the caller blocks forever and the child is orphaned when the caller is killed", bound)
	}

	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(gitPID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(gitPID) {
		t.Fatalf("git pid %d survived git.Run: an unbounded child reparents to init and spins there indefinitely", gitPID)
	}
}

// TestRun_ReturnsWhenCancelledChildLeavesAPipeHolder pins the WaitDelay
// backstop. Cancelling kills git's own PID, but a grandchild still holding the
// inherited stdout pipe keeps cmd.Wait, and the pipe read inside cmd.Output,
// blocked indefinitely without a WaitDelay.
func TestRun_ReturnsWhenCancelledChildLeavesAPipeHolder(t *testing.T) {
	binDir := fakeGitDir(t)
	gpidFile := filepath.Join(binDir, "grandchild.pid")
	writeFakeGit(t, binDir, "/bin/sleep 600 &\necho $! > "+gpidFile+"\nwait\n")
	t.Cleanup(func() { reapPIDFile(gpidFile) })

	// Shorten the production delay so this stays a fast test; the delay's real
	// value is guarded by TestCommandWaitDelay_StaysGenerousEnoughForALoadedHost.
	restoreDelay := commandWaitDelay
	commandWaitDelay = 2 * time.Second
	t.Cleanup(func() { commandWaitDelay = restoreDelay })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, t.TempDir(), "status", "--porcelain")
		done <- err
	}()

	readPID(t, gpidFile) // the pipe-holding grandchild exists

	const bound = 30 * time.Second
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a cancelled git command")
		}
	case <-time.After(bound):
		t.Fatalf("git.Run did not return within %s after its context was cancelled: a grandchild holding the inherited stdout pipe wedges cmd.Wait without a WaitDelay backstop", bound)
	}
}

// TestEveryPackageHelperBoundsItsGitChild covers the exec sites that do not go
// through runInDir. Two of them, FindGitRoot and FindMainRepoRoot, take no
// context at all and are the first two statements of branchsync's inspect,
// which is the function named in the incident's own stack: a bound that reached
// only runInDir left the reported failure fully reachable.
func TestEveryPackageHelperBoundsItsGitChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(dir string)
	}{
		{"FindGitRoot", func(dir string) { _, _ = FindGitRoot(dir) }},
		{"FindMainRepoRoot", func(dir string) { _, _ = FindMainRepoRoot(dir) }},
		{"InitBare", func(dir string) { _ = InitBare(context.Background(), filepath.Join(dir, "new.git")) }},
		{"IsDetachedHEAD", func(dir string) { _, _ = IsDetachedHEAD(context.Background(), dir) }},
		{"RefExists", func(dir string) { _, _ = RefExists(context.Background(), dir, "refs/heads/main") }},
		{"Run", func(dir string) { _, _ = Run(context.Background(), dir, "status", "--porcelain") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := fakeGitDir(t)
			pidFile := filepath.Join(binDir, "git.pid")
			writeFakeGit(t, binDir, "echo $$ > "+pidFile+"\nexec /bin/sleep 600\n")
			t.Cleanup(func() { reapPIDFile(pidFile) })

			restore := defaultCommandTimeout
			defaultCommandTimeout = 2 * time.Second
			t.Cleanup(func() { defaultCommandTimeout = restore })

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(t.TempDir())
			}()

			gitPID := readPID(t, pidFile)

			const bound = 30 * time.Second
			select {
			case <-done:
			case <-time.After(bound):
				t.Fatalf("%s did not return within %s for a git child that never exits: an unbounded exec site blocks its caller forever and orphans the child", tc.name, bound)
			}

			deadline := time.Now().Add(5 * time.Second)
			for pidAlive(gitPID) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if pidAlive(gitPID) {
				t.Fatalf("%s left git pid %d alive: an unbounded child reparents to init and spins there indefinitely", tc.name, gitPID)
			}
		})
	}
}

// TestCommandWaitDelay_StaysGenerousEnoughForALoadedHost guards a value that
// reads like a tidy-up candidate but is load-bearing. When the delay expires,
// exec reports ErrWaitDelay and discards the output of a git command that
// exited 0, and callers in this package fail closed on error, so a tight delay
// converts host load into a false "not clean" verdict that blocks custody
// recovery. Verified directly: a command exiting 0 with a pipe-holding
// descendant returns ErrWaitDelay and empty output. The delay therefore has to
// clear scheduler starvation on a loaded host (the incident host sat at load
// 160), not merely a fast local run.
func TestCommandWaitDelay_StaysGenerousEnoughForALoadedHost(t *testing.T) {
	const floor = 30 * time.Second
	if commandWaitDelay < floor {
		t.Fatalf("commandWaitDelay = %s, below the %s floor: a delay this tight makes a loaded host report succeeding git commands as failures, and callers here fail closed on that error", commandWaitDelay, floor)
	}
}

func TestCommandTimeout_GivesNetworkSubcommandsTheLongerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"plumbing", []string{"rev-parse", "HEAD"}, defaultCommandTimeout},
		{"network fetch", []string{"fetch", "--no-tags", "origin"}, networkCommandTimeout},
		{"network behind a leading flag", []string{"--git-dir=/tmp/x.git", "fetch", "origin"}, networkCommandTimeout},
		{"plumbing behind a leading flag", []string{"--git-dir=/tmp/x.git", "rev-parse", "HEAD"}, defaultCommandTimeout},
		{"no subcommand", []string{"--version"}, defaultCommandTimeout},
		// A global option whose value is a separate argument must not let that
		// value pose as the subcommand; RefExists passes exactly this shape.
		{"plumbing behind -C", []string{"-C", "/tmp/repo", "rev-parse", "HEAD"}, defaultCommandTimeout},
		{"network behind -C", []string{"-C", "/tmp/repo", "fetch", "origin"}, networkCommandTimeout},
		{"network behind separated --git-dir", []string{"--git-dir", "/tmp/x.git", "push", "origin"}, networkCommandTimeout},
		{"network behind -c", []string{"-c", "gc.auto=0", "fetch", "origin"}, networkCommandTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandTimeout(tc.args); got != tc.want {
				t.Errorf("commandTimeout(%q) = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}
