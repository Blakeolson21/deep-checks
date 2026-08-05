//go:build unix

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReapRecord_KillsARealOrphanRecoveredFromDisk exercises the crash-recovery
// half of process-tree reaping against real processes, with none of the
// snapshot/kill seams the rest of this package's tests substitute.
//
// The in-memory descendant union dies with the daemon, so when the OOM killer
// SIGKILLs it - which is the failure this whole mechanism exists to survive - the
// only thing that can still name the leaked processes is the record on disk. Every
// other test here proves the decision logic with stubbed kills; this one proves the
// whole chain end to end: a live sample taken while the leader was alive, persisted
// and read back from disk, still identifies and kills a descendant that has since
// been orphaned to ppid 1.
func TestReapRecord_KillsARealOrphanRecoveredFromDisk(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")

	// The shape of an agent step: a leader in its own process group that spawns a
	// long-lived worker and keeps running. Its stdio is detached so nothing here
	// depends on pipe teardown.
	leader := exec.Command("/bin/sh", "-c",
		"sleep 120 >/dev/null 2>&1 & echo $! > "+pidFile+"; sleep 120")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	leaderPID := leader.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) })

	orphanPID := readRecordedPID(t, pidFile, 10*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(orphanPID, syscall.SIGKILL) })

	// Sample while the leader is still alive. This is the poller's job in the
	// daemon, and it is the only moment at which the descendant is still linked
	// to this command.
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rec := Record{LeaderPID: leaderPID, Descendants: Descendants(snap, leaderPID)}
	for _, p := range snap {
		if p.PID == leaderPID {
			rec.LeaderStart = p.Started
		}
	}
	if rec.LeaderStart.IsZero() {
		t.Fatalf("leader %d missing from its own snapshot", leaderPID)
	}
	if !containsPID(rec.Descendants, orphanPID) {
		t.Fatalf("descendant %d was not sampled under leader %d; the record would name nothing to reap",
			orphanPID, leaderPID)
	}

	if err := WriteRecord(dir, rec); err != nil {
		t.Fatalf("write record: %v", err)
	}

	// Kill the leader alone rather than its group, so the descendant survives and
	// the kernel reparents it to pid 1. Waiting on it clears the zombie, without
	// which ReapRecord would still see a live leader and correctly refuse.
	if err := syscall.Kill(leaderPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill leader: %v", err)
	}
	_ = leader.Wait()

	if err := syscall.Kill(orphanPID, 0); err != nil {
		t.Fatalf("precondition failed: descendant %d did not outlive its leader: %v", orphanPID, err)
	}

	// Recover the way a freshly started daemon does: from the directory, not from
	// the value still in memory.
	records, err := ReadRecords(dir)
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	var recovered Record
	for _, r := range records {
		if r.LeaderPID == leaderPID {
			recovered = r
		}
	}
	if recovered.LeaderPID == 0 {
		t.Fatalf("record for leader %d was not recovered from %s", leaderPID, dir)
	}

	ReapRecord(recovered)

	if !recordPIDGoneWithin(orphanPID, 10*time.Second) {
		t.Fatalf("orphan %d survived the recorded reap; a daemon restart cannot clean up after a "+
			"crash, which is how leaked workers accumulated across runs", orphanPID)
	}
}

// TestReapRecord_LeavesARealLiveTreeAlone is the other half of the same
// guarantee, and the more expensive one to get wrong. The daemon is machine-wide,
// so a record whose leader is still running belongs to a live command; reaping it
// would kill a healthy step's worker processes.
func TestReapRecord_LeavesARealLiveTreeAlone(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "worker.pid")

	leader := exec.Command("/bin/sh", "-c",
		"sleep 120 >/dev/null 2>&1 & echo $! > "+pidFile+"; sleep 120")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	leaderPID := leader.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		_ = leader.Wait()
	})

	workerPID := readRecordedPID(t, pidFile, 10*time.Second)

	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rec := Record{LeaderPID: leaderPID, Descendants: Descendants(snap, leaderPID)}
	for _, p := range snap {
		if p.PID == leaderPID {
			rec.LeaderStart = p.Started
		}
	}

	if !containsPID(rec.Descendants, workerPID) {
		t.Fatalf("worker %d was not sampled under live leader %d, so this test would assert nothing",
			workerPID, leaderPID)
	}

	ReapRecord(rec)

	// Liveness is checked through ps rather than kill(pid, 0), which succeeds for
	// a zombie: a SIGKILLed worker stays in the table unreaped because its shell
	// parent never waits on it, and would read as survival.
	if state := processState(t, workerPID); !runningState(state) {
		t.Fatalf("worker %d of a live leader is in state %q after a reap that should have refused",
			workerPID, state)
	}
	if state := processState(t, leaderPID); !runningState(state) {
		t.Fatalf("live leader %d is in state %q after a reap that should have refused", leaderPID, state)
	}
}

// processState returns the ps state letters for pid, or "" when it is gone.
func processState(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runningState(state string) bool {
	return state != "" && !strings.HasPrefix(state, "Z")
}

func containsPID(procs []Proc, pid int) bool {
	for _, p := range procs {
		if p.PID == pid {
			return true
		}
	}
	return false
}

func readRecordedPID(t *testing.T, path string, timeout time.Duration) int {
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

func recordPIDGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
