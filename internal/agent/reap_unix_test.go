//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeAgentEscapedPipeHelperEnv = "NM_AGENT_NATIVE_PIPE_HELPER"

const nativeAgentReapEscapeHelperEnv = "NM_AGENT_NATIVE_REAP_HELPER"

// unsampledSpawnDelay is how long the escaped-pipe-holder leader idles before
// spawning its holder. It must exceed internal/shellenv's first sampling
// interval so the holder appears after that sample and before the next one,
// which is a whole tracker tick away.
const unsampledSpawnDelay = 3 * time.Second

// TestNativeAgentCommand_WaitDelayClosesEscapedPipeHolder proves the WaitDelay
// backstop alone, so it deliberately does NOT assert that the escaped pipe
// holder is reaped: nothing beneath the leader was ever sampled, so the reap has
// no pid list to work from. That is why the holder is SIGKILLed by hand in
// t.Cleanup here.
//
// Keeping the holder unsampled has to be arranged deliberately rather than
// assumed. A leader that merely "exits quickly" does not achieve it: this leader
// is a race-instrumented re-exec of the test binary, whose runtime and testing
// startup alone can take about as long as the poller's first sampling interval,
// so the first sample lands inside its lifetime. Instead the leader outlives that
// first sample on purpose and only then spawns the holder, which leaves the next
// sample a full tracker tick away - far beyond the reap. If that ordering is ever
// lost the holder is reaped, the pipe closes on a clean EOF, and the assertions
// below fail rather than quietly testing the reaper instead of the backstop.
//
// The holder leaves the leader's process group at fork through Setpgid rather
// than by calling setsid() once it boots, so "out of reach of kill(-leaderPID)"
// is true from the instant it exists; the precondition below checks that.
//
// This is not a blessing of the escape. It mirrors the deliberate split in
// internal/shellenv, where TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder
// keeps sampling off for the same reason: the reaper is best-effort, and
// WaitDelay is the backstop for exactly the case where it fails to reach a
// descendant. Reaping a setsid() escapee through the native agent path is
// asserted separately by
// TestNativeAgentCommand_TerminateReapsSetsidEscapeeAfterSampling.
func TestNativeAgentCommand_WaitDelayClosesEscapedPipeHolder(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
	cmd.Env = append(os.Environ(),
		nativeAgentEscapedPipeHelperEnv+"=leader",
		"NM_AGENT_NATIVE_PIPE_PID="+pidFile,
	)
	shellenv.ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	defer started.closePipes()

	type readResult struct {
		output string
		err    error
	}
	readCh := make(chan readResult, 1)
	go func() {
		out, err := io.ReadAll(started.stdout)
		readCh <- readResult{output: string(out), err: err}
	}()

	var rr readResult
	select {
	case rr = <-readCh:
	// The budget covers the leader's deliberate idle before it spawns the
	// holder, plus WaitDelay. A reader still blocked past that is the wedge this
	// test exists to rule out.
	case <-time.After(unsampledSpawnDelay + 10*time.Second):
		started.closePipes()
		started.terminate()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Fatal("stdout reader stayed blocked after the leader exited with an escaped pipe holder")
	}

	escapedPID := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	// Precondition: the holder really did leave the leader's process group, so
	// the group kill inside terminate() could not have closed the pipe. Without
	// this the test could pass for the wrong reason - a holder still inside the
	// group is reaped, and the pipe closes on a clean EOF rather than on the
	// backstop under test.
	if pgid, pgidErr := syscall.Getpgid(escapedPID); pgidErr != nil {
		t.Fatalf("precondition: read pipe holder %d pgid: %v", escapedPID, pgidErr)
	} else if pgid == cmd.Process.Pid {
		t.Fatalf("precondition failed: pipe holder %d is still in the leader's group %d, "+
			"so the reaper, not WaitDelay, would close the pipe", escapedPID, cmd.Process.Pid)
	}
	if !strings.Contains(rr.output, "leader done\n") {
		t.Fatalf("stdout output = %q, want leader output", rr.output)
	}
	if rr.err == nil {
		t.Fatalf("stdout read error = nil, want closed-pipe error")
	}
	if err := started.wait(); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait error = %v, want %v", err, exec.ErrWaitDelay)
	}
}

// TestNativeAgentCommand_TerminateReapsSetsidEscapeeAfterSampling pins the wiring
// that makes transitive reaping reachable from the agent path at all.
//
// Every other reap test in this file uses a grandchild that stays in the leader's
// process group, so it is reaped by the single kill(-leaderPID) that Setpgid
// already enables. Those tests keep passing if startNativeAgentCommand is ever
// changed from shellenv.StartShellCommand back to a bare cmd.Start(). That swap
// silently drops the leader from the descendant tracker, and a descendant that
// calls setsid() - which is what Node's `detached: true` does, and therefore what
// Claude Code's CLI Bash tool does to its shell - leaves the group and becomes
// unreachable. The symptom is not a test failure but an orphan at PPID 1 burning
// a core for hours, which is the incident this whole change exists to prevent.
//
// So this test escapes the group on purpose and requires the escapee to be gone
// after terminate(). The leader is held alive past the tracker's first sampling
// interval, because the reap path performs no listing of its own: it can only
// kill what the poller observed while the leader still had its ppid links.
func TestNativeAgentCommand_TerminateReapsSetsidEscapeeAfterSampling(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "escapee.pid")
	exitFile := filepath.Join(dir, "exit")

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentReapEscapeHelper$")
	cmd.Env = append(os.Environ(),
		nativeAgentReapEscapeHelperEnv+"=leader",
		"NM_AGENT_REAP_READY="+readyFile,
		"NM_AGENT_REAP_PID="+pidFile,
		"NM_AGENT_REAP_EXIT="+exitFile,
	)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	defer started.closePipes()

	// Drain both pipes. waitForPipes blocks until each one reports EOF, which
	// only happens if somebody reads them.
	go func() { _, _ = io.ReadAll(started.stdout) }()
	go func() { _, _ = io.ReadAll(started.stderr) }()

	escapeePID := waitForPidFile(t, pidFile, 10*time.Second)
	t.Cleanup(func() {
		// Never leave a real orphan behind, whatever the assertions did.
		_ = syscall.Kill(escapeePID, syscall.SIGKILL)
	})
	if !waitForNativeAgentPipeHelperReady(readyFile, 10*time.Second) {
		t.Fatal("escapee never signalled that it had called setsid")
	}

	// Hold the leader alive past the poller's first sample. The interval lives in
	// internal/shellenv and is not exported, so this waits on wall time with a
	// margin rather than observing the sample directly.
	time.Sleep(3 * time.Second)

	if err := os.WriteFile(exitFile, []byte("go"), 0o644); err != nil {
		t.Fatalf("signal leader to exit: %v", err)
	}
	if err := started.wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// started.wait() returns only after the Wait goroutine has run terminate(),
	// so the reap has already happened by here.
	if !pidGoneWithin(escapeePID, 10*time.Second) {
		t.Fatalf("setsid escapee pid %d survived terminate(); the native agent leader is not "+
			"registered with the descendant tracker, so process-tree reaping cannot reach it "+
			"(check that startNativeAgentCommand still uses shellenv.StartShellCommand)", escapeePID)
	}
}

func TestNativeAgentReapEscapeHelper(t *testing.T) {
	switch os.Getenv(nativeAgentReapEscapeHelperEnv) {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestNativeAgentReapEscapeHelper$")
		child.Env = append(os.Environ(), nativeAgentReapEscapeHelperEnv+"=escaped",
			"NM_AGENT_REAP_READY="+os.Getenv("NM_AGENT_REAP_READY"))
		// Detach the escapee's stdio. Holding the leader's pipes open would wedge
		// the parser and test the WaitDelay backstop instead of the reap.
		child.Stdout = nil
		child.Stderr = nil
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("NM_AGENT_REAP_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		if !waitForNativeAgentPipeHelperReady(os.Getenv("NM_AGENT_REAP_EXIT"), 60*time.Second) {
			os.Exit(3)
		}
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_AGENT_REAP_READY"), []byte("ready"), 0o644)
		time.Sleep(120 * time.Second)
		os.Exit(0)
	}
}

func TestNativeAgentEscapedPipeHelper(t *testing.T) {
	switch os.Getenv(nativeAgentEscapedPipeHelperEnv) {
	case "leader":
		// Idle past the descendant poller's first sample before spawning
		// anything, so the holder is born into the long gap before the next one
		// and is never sampled. Spawning first and exiting fast is the version
		// that does not work: this binary's own race-instrumented startup is
		// comparable to that first interval, so the sample lands while the leader
		// is still alive and the holder gets reaped instead of bounded by
		// WaitDelay. The interval and tick live in internal/shellenv and are not
		// exported, so this waits on wall time with a margin.
		time.Sleep(unsampledSpawnDelay)
		child := exec.Command(os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
		child.Env = append(os.Environ(), nativeAgentEscapedPipeHelperEnv+"=escaped")
		// The holder inherits this leader's stdout/stderr - the agent command's
		// pipes - and leaves the leader's process group at fork, so kill(-leader)
		// cannot reach it and it keeps those write ends open. Setpgid is applied
		// by the parent before exec, so the escape is complete the moment the
		// child exists rather than only once it has scheduled and called setsid().
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("NM_AGENT_NATIVE_PIPE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

// TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit is the regression test for
// the daemon-crash bug behind the agent-spawning test step.
//
// When a repo has no configured test command, the test step asks the agent to
// run the tests itself. That agent (codex here) spawns a test runner whose
// worker pool can outlive it. ConfigureShellCommand isolates the agent in its
// own process group and installs a cmd.Cancel that SIGKILLs the group - but
// cmd.Cancel only fires on context cancellation. On a clean exit (exit 0)
// nothing reaped the group, so the worker grandchildren leaked. Across runs
// those orphans accumulate (each a multi-hundred-MB worker pool) until the host
// is out of memory and the OS OOM-killer SIGKILLs the daemon, which the next
// daemon start reports as "daemon crashed during execution".
//
// The fake codex backgrounds a grandchild whose stdio is detached (so it does
// not hold the agent's stdout pipe open, which would wedge the parser instead
// of exercising the clean-exit leak path), records its pid, prints a valid
// result, and exits 0. After the fix the deferred TerminateShellCommandGroup
// reaps the group on this success path, so the grandchild is gone once Run
// returns. Before the fix it survived.
func TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
# Background a long-lived grandchild that outlives this leader, mirroring a test
# runner's worker pool. Detach its stdio so it does not keep the agent's
# stdout/stderr pipe open.
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "`+pidFile+`"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
exit 0
`, "")

	ca := &codexAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run returned error (the daemon would fail the step, not crash): %v", err)
	}
	if result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	// Once Run has returned, the deferred TerminateShellCommandGroup must have
	// SIGKILLed the whole group. Poll briefly to absorb signal-delivery jitter.
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not orphan a real process
		t.Fatalf("grandchild pid %d still alive after clean agent exit; the process group leaked "+
			"(this is the leak that OOM-kills the daemon)", grandchild)
	}
}

func TestClaudeAgent_LargeStdinReapsGrandchildHoldingPipesOnLeaderExit(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "grandchild.pid")
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "spawn-grandchild")
	t.Setenv("NM_CLAUDE_STDIN_READY", readyFile)
	t.Setenv("NM_CLAUDE_STDIN_PID", pidFile)

	a := newClaudeStdinHelperAgent(t)
	result, err := a.runOnce(context.Background(), RunOpts{
		Prompt: strings.Repeat("p", 2*1024*1024),
		CWD:    dir,
	})
	if err != nil {
		t.Fatalf("Claude run with inherited-pipe holder: %v", err)
	}
	if result.Text != "ok" {
		t.Fatalf("Claude result text = %q, want ok", result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("Claude grandchild pid %d survived clean leader exit", grandchild)
	}
}

func TestCodexAgent_Run_ReapsGrandchildHoldingStdoutPipeOnLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
	( sleep 120 ) &
	echo $! > "`+pidFile+`"
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
	exit 0
	`, "")

	ca := &codexAgent{bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type runResult struct {
		result *Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := ca.Run(ctx, RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
		done <- runResult{result: result, err: err}
	}()

	var rr runResult
	select {
	case rr = <-done:
	case <-time.After(1500 * time.Millisecond):
		cancel()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("agent run did not return after its leader exited while a grandchild held stdout open")
	}

	if rr.err != nil {
		t.Fatalf("Run returned error: %v", rr.err)
	}
	if rr.result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", rr.result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after leader exit with inherited stdout", grandchild)
	}
}

func waitForPidFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

// pidGoneWithin reports whether pid stops existing within the window. kill(pid,
// 0) returns ESRCH once the process is gone (the grandchild reparents to init
// after the leader exits, so init reaps it the moment it is SIGKILLed).
func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func waitForNativeAgentPipeHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
