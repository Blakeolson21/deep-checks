//go:build unix

package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
//
// They are unix-only because they observe real process lifetimes: the fixtures
// are /bin/sh scripts and the liveness checks are signal 0, neither of which
// exists on Windows, where this package's tests also run. The platform-agnostic
// halves of the same bounds live in git_bounds_test.go.

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
		// The kill route: Wait reports the process's own *exec.ExitError and
		// drops the context error, so without explain rejoining it this reads as
		// a bare "signal: killed" that never mentions the bound that fired.
		assertNamesTheCeiling(t, err)
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

// writeFakeGitLeavingAPipeHolder writes a fake git that prints stdout, leaves a
// descendant holding the inherited stdout pipe, and exits 0 itself. That is the
// exact shape that expires the WaitDelay: the process is gone, but the pipe
// stays open, so exec's copying goroutines never see EOF.
func writeFakeGitLeavingAPipeHolder(t *testing.T, binDir, stdout string) {
	t.Helper()
	gpidFile := filepath.Join(binDir, "grandchild.pid")
	writeFakeGit(t, binDir, "echo '"+stdout+"'\n/bin/sleep 600 &\necho $! > "+gpidFile+"\nexit 0\n")
	t.Cleanup(func() { reapPIDFile(gpidFile) })
}

// TestRun_WaitDelayExpiryNamesTheBoundThatFired covers the second bound's
// diagnosis. git exits 0, a surviving descendant still holds the inherited
// stdout pipe, and the WaitDelay expires, so exec reports ErrWaitDelay for a
// command that actually succeeded. Callers here fail closed on error, so this
// has to say which bound fired rather than surface as an unexplained failure.
func TestRun_WaitDelayExpiryNamesTheBoundThatFired(t *testing.T) {
	binDir := fakeGitDir(t)
	writeFakeGitLeavingAPipeHolder(t, binDir, "M  tracked.go")

	restoreDelay := commandWaitDelay
	commandWaitDelay = time.Second
	t.Cleanup(func() { commandWaitDelay = restoreDelay })

	_, err := Run(context.Background(), t.TempDir(), "status", "--porcelain")
	if err == nil {
		t.Fatal("expected an error once the wait delay expired")
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("error does not unwrap to exec.ErrWaitDelay, so a caller cannot tell this bound from a real git failure: %v", err)
	}
	if !strings.Contains(err.Error(), "wait delay") {
		t.Errorf("error does not name the wait delay that fired: %v", err)
	}
}

// TestOutput_WaitDelayExpiryKeepsWhatExecAlreadyCopied pins the mechanism the
// 60s delay is chosen against, because getting it wrong points at the wrong
// mitigation. exec does not throw the captured output away on ErrWaitDelay: the
// copying goroutine drains everything git wrote before blocking on the pipe the
// descendant holds open, and Output returns that buffer alongside the error. So
// the value at risk from a tight delay is not "the output is gone" but "the
// output stopped being provably complete", which is why an expiry cannot be
// passed through as success and the delay must instead be wide enough that
// scheduler starvation never reaches it.
func TestOutput_WaitDelayExpiryKeepsWhatExecAlreadyCopied(t *testing.T) {
	binDir := fakeGitDir(t)
	const line = "M  tracked.go"
	writeFakeGitLeavingAPipeHolder(t, binDir, line)

	restoreDelay := commandWaitDelay
	commandWaitDelay = time.Second
	t.Cleanup(func() { commandWaitDelay = restoreDelay })

	dir := t.TempDir()
	cmd := newCommand(context.Background(), "status", "--porcelain")
	defer cmd.close()
	cmd.setDir(dir)
	cmd.setEnv(NonInteractiveEnv(dir))

	out, err := cmd.output()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("expected exec.ErrWaitDelay from a git that exited 0 behind a surviving pipe holder, got %v", err)
	}
	if !strings.Contains(string(out), line) {
		t.Errorf("exec discarded the output it had already copied, got %q: the 60s delay is justified by output that stops being provably complete, not by output that is thrown away", out)
	}
}
