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
