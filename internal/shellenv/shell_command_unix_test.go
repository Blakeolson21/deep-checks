//go:build unix

package shellenv

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

	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit pins the
// success-path guarantee that keeps the daemon alive: when a leader configured
// with ConfigureShellCommand exits 0 but leaves a grandchild alive in its
// process group (a test runner's worker pool), TerminateShellCommandGroup
// SIGKILLs the whole group. cmd.Cancel only fires on cancellation, so without
// this the grandchild leaks and orphan pools pile up across runs until the host
// OOMs and the OS kills the daemon.
func TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The leader backgrounds a long-lived grandchild (stdio detached so it does
	// not hold the inherited pipes open), records its pid, and exits 0.
	script := "( sleep 120 >/dev/null 2>&1 ) & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}

	grandchild := readPID(t, pidFile, 5*time.Second)
	if syscall.Kill(grandchild, 0) != nil {
		t.Fatalf("precondition failed: grandchild %d should still be alive before reap", grandchild)
	}

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup; group leaked", grandchild)
	}
}

// TestConfigureShellCommand_CancelReapsSetsidEscapedChild pins the guarantee the
// process-group kill cannot provide on its own.
//
// Setpgid isolates the leader in its own process group, and both cmd.Cancel and
// TerminateShellCommandGroup reap with kill(-leaderPID). That signal reaches only
// processes whose pgid still equals the leader pid. A child that calls setsid()
// gets its own session and its own process group, so it is unreachable by the
// group kill - and setsid() is exactly what Node's `detached: true` does, which is
// how Claude Code's CLI Bash tool spawns its shell.
//
// This is the leak behind gate run 01KY36EAXX53WZSEC4ZYSMY4F0: six xctest
// processes spawned under an agent invocation were still alive three days later
// at ~50% CPU each with PPID=1. Reaping requires walking the process tree, not
// signalling a single group.
func TestConfigureShellCommand_CancelReapsSetsidEscapedChild(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_SETSID_HELPER=leader",
		"NM_SHELLENV_SETSID_PID="+pidFile,
	)
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// The escapee writes its pid only after setsid() returns, so a readable pid
	// means it has already left the leader's process group.
	escaped := readPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })

	pgid, err := syscall.Getpgid(escaped)
	if err != nil {
		t.Fatalf("precondition: read escapee pgid: %v", err)
	}
	if pgid == cmd.Process.Pid {
		t.Fatalf("precondition failed: escapee %d is still in the leader's group %d, "+
			"so this test would not exercise the escape", escaped, cmd.Process.Pid)
	}

	waitForSampledDescendant(t, cmd.Process.Pid, escaped, 15*time.Second)

	cancel()
	<-waitErr

	if !pidGoneWithin(escaped, 10*time.Second) {
		t.Fatalf("setsid escapee %d survived cancellation of leader %d (escapee pgid %d): "+
			"kill(-%d) cannot reach a process that left the group; the reaper must walk the tree",
			escaped, cmd.Process.Pid, pgid, cmd.Process.Pid)
	}
}

// TestTerminateShellCommandGroup_ReapsSetsidEscapeeAfterLeaderExits is the exact
// shape of the motivating incident, and it is strictly harder than the
// cancellation case.
//
// Gate run 01KY36EAXX53WZSEC4ZYSMY4F0 reported status=completed exit_code=0 while
// leaking six xctest processes. Because the leader exited cleanly, the reap ran
// from TerminateShellCommandGroup - after cmd.Wait returned, so after the leader
// was already dead. By then the kernel has rewritten the escapee's ppid to 1, and
// a snapshot taken at that moment has no trail back to this command at all.
//
// Only a descendant set sampled while the leader was still alive can catch this,
// which is why the poller is load-bearing rather than a nice-to-have.
func TestTerminateShellCommandGroup_ReapsSetsidEscapeeAfterLeaderExits(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()

	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_SETSID_HELPER=leader-exits",
		"NM_SHELLENV_SETSID_PID="+pidFile,
	)
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	leaderPID := cmd.Process.Pid

	escaped := readPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	waitForSampledDescendant(t, leaderPID, escaped, 15*time.Second)

	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader Wait: %v", err)
	}

	// Precondition: the leader is gone and the escapee has been reparented, so
	// no ppid link from the escapee reaches leaderPID any more.
	snap, err := proctree.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := pidSetOf(proctree.Descendants(snap, leaderPID)); got[escaped] {
		t.Fatalf("precondition failed: escapee %d is still reachable from dead leader %d, "+
			"so this test would not exercise the reparented path", escaped, leaderPID)
	}

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(escaped, 10*time.Second) {
		t.Fatalf("setsid escapee %d survived clean exit of leader %d: this is the "+
			"leak that left six xctest processes alive for three days", escaped, leaderPID)
	}
}

func pidSetOf(procs []proctree.Proc) map[int]bool {
	out := make(map[int]bool, len(procs))
	for _, p := range procs {
		out[p.PID] = true
	}
	return out
}

// TestSetsidEscapeHelper is the re-exec helper for the setsid escape tests. The
// leader spawns a child and stays alive; the child calls setsid() to leave the
// leader's process group, then records its pid and idles. Neither writes to the
// inherited pipes, so these tests exercise reaping rather than pipe teardown.
func TestSetsidEscapeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_SETSID_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_SETSID_HELPER=escaped",
			"NM_SHELLENV_SETSID_PID="+os.Getenv("NM_SHELLENV_SETSID_PID"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(helperIdle)
		os.Exit(0)
	case "leader-exits":
		// Same as "leader", but exits cleanly once the escapee has been sampled
		// a few times, leaving it orphaned behind an exit code of 0.
		child := exec.Command(os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_SETSID_HELPER=escaped",
			"NM_SHELLENV_SETSID_PID="+os.Getenv("NM_SHELLENV_SETSID_PID"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(600 * time.Millisecond)
		os.Exit(0)
	case "escaped":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(os.Getenv("NM_SHELLENV_SETSID_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(helperIdle)
		os.Exit(0)
	}
}

// helperIdle is how long a re-exec helper lingers. It must outlast the reap
// assertions so a survivor is a real leak rather than a helper that timed out.
const helperIdle = 120 * time.Second

// TestTerminateShellCommandGroup_NoopOnNilOrUnstarted guards the cheap safety
// contract: a nil command, or one that was never started (no Process), must be
// a no-op rather than panic or signal an arbitrary pid.
func TestTerminateShellCommandGroup_NoopOnNilOrUnstarted(t *testing.T) {
	TerminateShellCommandGroup(nil)
	cmd := exec.Command("/bin/sh", "-c", "true") // never Start()ed: cmd.Process is nil
	TerminateShellCommandGroup(cmd)
}

func TestCombinedOutputShellCommand_ReturnsCleanExitWithInheritedPipeGrandchild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf 'leader done\\n'; sleep 30 & exit 0")
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("CombinedOutputShellCommand() error = %v; output %q", err, out)
	}
	if got, want := string(out), "leader done\n"; got != want {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want %q", got, want)
	}
}

// TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder isolates the
// WaitDelay backstop from the reaper on purpose.
//
// Sampling is pinned off so the escaped pipe holder survives the reap, which is
// the only way to prove WaitDelay alone bounds Wait. That matters because the
// reaper is best-effort - `ps` can be unavailable or a kill can be denied - and
// WaitDelay is what stops a wedged parser in that case.
func TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder(t *testing.T) {
	defer setTrackerTickForTest(time.Hour)()
	readyFile := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_PIPE_HELPER=leader",
		"NM_SHELLENV_PIPE_READY="+readyFile,
	)
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	escapedPID := parseEscapedPID(t, string(out))
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("CombinedOutputShellCommand() error = %v, want %v; output %q", err, exec.ErrWaitDelay, out)
	}
	if !strings.Contains(string(out), "leader done\n") {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want leader output", out)
	}
}

func TestShellOutputPipeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_PIPE_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_PIPE_HELPER=escaped",
			"NM_SHELLENV_PIPE_READY="+os.Getenv("NM_SHELLENV_PIPE_READY"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_SHELLENV_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func readPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

func parseEscapedPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "escaped pid ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "escaped pid "))
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("output %q did not contain escaped pid", output)
	return 0
}

func waitForHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func setTrackerTickForTest(d time.Duration) func() {
	trackerMu.Lock()
	prevTick, prevFirst := trackerTick, trackerFirstSample
	trackerTick, trackerFirstSample = d, d
	trackerMu.Unlock()
	return func() {
		trackerMu.Lock()
		trackerTick, trackerFirstSample = prevTick, prevFirst
		trackerMu.Unlock()
	}
}

// waitForSampledDescendant blocks until the poller has recorded descendantPID
// under leaderPID.
//
// Tests that assert on reaping must wait for this rather than sleeping: whether
// a sample landed before the reap is the difference between the guarantee under
// test and the documented residual gap, and under the race detector a `ps` over
// a thousand processes is slow enough to lose that race by accident.
func waitForSampledDescendant(t *testing.T, leaderPID, descendantPID int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if trackedDescendantSampled(leaderPID, descendantPID) {
			return
		}
		select {
		case <-sampleSignal():
		case <-deadline:
			t.Fatalf("timed out waiting for the poller to sample descendant %d under leader %d",
				descendantPID, leaderPID)
		}
	}
}
