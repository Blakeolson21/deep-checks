//go:build unix

package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// TestTracker_PersistsRecordWhileLeaderRuns covers the crash case. The in-memory
// descendant union dies with the daemon, and the OOM killer - the very failure
// leaked processes cause - kills with an uncatchable SIGKILL, so there is no
// chance to flush on the way out. Only a record written while the tree was alive
// lets a restarted daemon finish the job.
func TestTracker_PersistsRecordWhileLeaderRuns(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()
	dir := t.TempDir()
	defer SetProcessRecordDir("")()

	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_SETSID_HELPER=leader",
		"NM_SHELLENV_SETSID_PID="+pidFile,
	)
	ConfigureShellCommand(cmd)
	SetProcessRecordDir(dir)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	defer func() {
		TerminateShellCommandGroup(cmd)
		_ = cmd.Wait()
	}()

	escaped := readPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })

	rec := waitForRecord(t, dir, cmd.Process.Pid, escaped, 10*time.Second)
	if rec.LeaderStart.IsZero() {
		t.Error("record has no leader start time, so recovery could not tell a recycled pid apart")
	}
}

// TestTracker_RemovesRecordAfterReap keeps recovery honest: a record that
// outlived its reap would make the next daemon start walk trees that are already
// gone, and every stale pid it holds is one more chance to signal a recycled pid.
func TestTracker_RemovesRecordAfterReap(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()
	dir := t.TempDir()
	defer SetProcessRecordDir("")()

	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_SETSID_HELPER=leader",
		"NM_SHELLENV_SETSID_PID="+pidFile,
	)
	ConfigureShellCommand(cmd)
	SetProcessRecordDir(dir)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	leaderPID := cmd.Process.Pid

	escaped := readPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	waitForRecord(t, dir, leaderPID, escaped, 10*time.Second)

	TerminateShellCommandGroup(cmd)
	_ = cmd.Wait()

	records, err := proctree.ReadRecords(dir)
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	for _, rec := range records {
		if rec.LeaderPID == leaderPID {
			t.Fatalf("record for reaped leader %d survived: %+v", leaderPID, rec)
		}
	}
}

// TestSetProcessRecordDir_UnsetWritesNothing keeps the CLI and the test suite off
// disk. Only the daemon has an NM_HOME worth persisting into.
func TestSetProcessRecordDir_UnsetWritesNothing(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()
	dir := t.TempDir()
	defer SetProcessRecordDir("")()

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 1")
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	TerminateShellCommandGroup(cmd)
	_ = cmd.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("records written with no record dir configured: %v", entries)
	}
}

// TestTracker_SamplesALeaderRegisteredWhileAPollerIsAlreadyRunning covers the
// ordinary daemon state rather than a corner case: one long agent step keeps a
// poller generation alive for the whole run, so nearly every command registers
// while sampling is already in progress.
//
// If those leaders had to wait out a full trackerTick, a command cancelled a few
// seconds in would reach the reap with an empty union and every setsid() escapee
// beneath it would leak - the exact failure this package exists to close,
// reachable under plain concurrency rather than inside the documented one-tick
// residual gap.
func TestTracker_SamplesALeaderRegisteredWhileAPollerIsAlreadyRunning(t *testing.T) {
	defer setTrackerIntervalsForTest(30*time.Second, 25*time.Millisecond)()

	holder := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 120")
	ConfigureShellCommand(holder)
	if err := StartShellCommand(holder); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	defer func() {
		TerminateShellCommandGroup(holder)
		_ = holder.Wait()
	}()

	// Let the poller finish its first round so it is back on the long tick,
	// which is what a late leader would otherwise have to wait out.
	waitForSampleRound(t, 15*time.Second)

	// The late leader already owns a descendant before it registers, so any
	// sample taken after registration must see it. Registration is separated
	// from Start on purpose: the assertion is about when sampling reaches a
	// leader, not about how quickly a helper can fork.
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	late := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestSetsidEscapeHelper$")
	late.Env = append(os.Environ(),
		"NM_SHELLENV_SETSID_HELPER=leader",
		"NM_SHELLENV_SETSID_PID="+pidFile,
	)
	ConfigureShellCommand(late)
	if err := late.Start(); err != nil {
		t.Fatalf("late leader Start: %v", err)
	}
	latePID := late.Process.Pid
	defer func() {
		_ = syscall.Kill(-latePID, syscall.SIGKILL)
		_ = late.Wait()
	}()

	escaped := readPID(t, pidFile, 15*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })

	trackLeader(latePID, time.Now())
	t.Cleanup(func() { takeTrackedLeader(latePID) })

	waitForSampledDescendant(t, latePID, escaped, 5*time.Second)
}

// TestTracker_RecordDropsDescendantsThatHaveExited bounds the union.
//
// The union is what the reap consumes, and it is persisted in full on every
// change. Left unpruned it grows for the life of the step - the motivating
// incident's ran 86 minutes - until the pid list overflows the single argv entry
// ps receives, at which point verification errors out and the reap kills nothing
// at all. Dropping a pid the process table no longer shows is free: a process
// that is gone needs no signal.
func TestTracker_RecordDropsDescendantsThatHaveExited(t *testing.T) {
	defer setTrackerTickForTest(25 * time.Millisecond)()
	dir := t.TempDir()
	defer SetProcessRecordDir("")()

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The leader waits on the child so the kernel reaps it: a zombie is still
	// listed by ps, and this test is about a descendant that is truly gone.
	script := "sleep 300 & echo $! > " + pidFile + "; wait; sleep 300"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	SetProcessRecordDir(dir)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	leaderPID := cmd.Process.Pid
	defer func() {
		TerminateShellCommandGroup(cmd)
		_ = cmd.Wait()
	}()

	child := readPID(t, pidFile, 15*time.Second)
	waitForRecord(t, dir, leaderPID, child, 15*time.Second)

	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Fatalf("kill sampled child %d: %v", child, err)
	}

	waitForRecordWithoutDescendant(t, dir, leaderPID, child, 15*time.Second)
}

func waitForSampleRound(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-sampleSignal():
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the poller to complete a sampling round")
	}
}

// setTrackerIntervalsForTest sets the tick and the first-sample bound
// independently, which is what separates "sampled at all" from "sampled again".
func setTrackerIntervalsForTest(tick, first time.Duration) func() {
	trackerMu.Lock()
	prevTick, prevFirst := trackerTick, trackerFirstSample
	trackerTick, trackerFirstSample = tick, first
	trackerMu.Unlock()
	return func() {
		trackerMu.Lock()
		trackerTick, trackerFirstSample = prevTick, prevFirst
		trackerMu.Unlock()
	}
}

func waitForRecordWithoutDescendant(t *testing.T, dir string, leaderPID, goneDescendant int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		records, err := proctree.ReadRecords(dir)
		if err == nil {
			for _, rec := range records {
				if rec.LeaderPID != leaderPID {
					continue
				}
				listed := false
				for _, d := range rec.Descendants {
					if d.PID == goneDescendant {
						listed = true
						break
					}
				}
				if !listed {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("record for leader %d still lists exited descendant %d after %s; "+
		"the union grows without bound and eventually overflows the ps argv entry",
		leaderPID, goneDescendant, timeout)
}

func waitForRecord(t *testing.T, dir string, leaderPID, wantDescendant int, timeout time.Duration) proctree.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		records, err := proctree.ReadRecords(dir)
		if err == nil {
			for _, rec := range records {
				if rec.LeaderPID != leaderPID {
					continue
				}
				for _, d := range rec.Descendants {
					if d.PID == wantDescendant {
						return rec
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a record in %s naming leader %d and descendant %d",
		dir, leaderPID, wantDescendant)
	return proctree.Record{}
}
