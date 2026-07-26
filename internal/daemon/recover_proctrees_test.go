//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// TestReapOrphanedProcessTrees_ReapsRecordedOrphans is the restart half of the
// leak fix. The six xctest processes from gate run 01KY36EAXX53WZSEC4ZYSMY4F0
// outlived not just their step but every daemon that followed, because nothing
// at startup knew they existed. A persisted record is what makes them findable.
func TestReapOrphanedProcessTrees_ReapsRecordedOrphans(t *testing.T) {
	p := testPaths(t)
	started := time.Date(2026, time.July, 21, 17, 9, 0, 0, time.UTC)
	rec := proctree.Record{
		LeaderPID:   424242,
		LeaderStart: started,
		Descendants: []proctree.Proc{{PID: 424243, PPID: 424242, PGID: 424243, Started: started}},
	}
	if err := proctree.WriteRecord(p.ProcTreesDir(), rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	var reaped []int
	restore := swapProcTreeReaper(func(r proctree.Record) { reaped = append(reaped, r.LeaderPID) })
	defer restore()

	reapOrphanedProcessTrees(p)

	if len(reaped) != 1 || reaped[0] != 424242 {
		t.Fatalf("reaped %v, want [424242]", reaped)
	}
	if remaining := recordCount(t, p); remaining != 0 {
		t.Fatalf("%d records survived the sweep, want 0", remaining)
	}
}

// TestReapOrphanedProcessTrees_SkipsWhenAnotherDaemonIsAlive mirrors the guard on
// reapOrphanedServers. The daemon is machine-wide, so a second one sweeping
// records would SIGKILL process trees belonging to the live daemon's running
// steps.
func TestReapOrphanedProcessTrees_SkipsWhenAnotherDaemonIsAlive(t *testing.T) {
	p := testPaths(t)
	writeLiveDaemonPIDFile(t, p)

	if err := proctree.WriteRecord(p.ProcTreesDir(), proctree.Record{
		LeaderPID:   424242,
		LeaderStart: time.Now(),
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	var reaped []int
	restore := swapProcTreeReaper(func(r proctree.Record) { reaped = append(reaped, r.LeaderPID) })
	defer restore()

	reapOrphanedProcessTrees(p)

	if len(reaped) != 0 {
		t.Fatalf("reaped %v while another daemon was alive, want none", reaped)
	}
	if remaining := recordCount(t, p); remaining != 1 {
		t.Fatalf("%d records remain, want the live daemon's record left untouched", remaining)
	}
}

func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	t.Setenv("NM_HOME", t.TempDir())
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	return p
}

// writeLiveDaemonPIDFile spawns a real, long-lived process and records it as the
// daemon.
//
// It must not be the current process: otherDaemonAlive short-circuits on its own
// pid, since a record pointing at itself is by definition not a predecessor.
func writeLiveDaemonPIDFile(t *testing.T, p *paths.Paths) {
	t.Helper()
	other := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := other.Start(); err != nil {
		t.Fatalf("start stand-in daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = other.Process.Kill()
		_ = other.Wait()
	})

	started, err := processStartTime(other.Process.Pid)
	if err != nil {
		t.Skipf("cannot read stand-in daemon start time: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.PIDFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonPIDFile(p.PIDFile(), daemonPIDFile{PID: other.Process.Pid, StartedAt: started}); err != nil {
		t.Fatalf("writeDaemonPIDFile: %v", err)
	}
}

func recordCount(t *testing.T, p *paths.Paths) int {
	t.Helper()
	records, err := proctree.ReadRecords(p.ProcTreesDir())
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	return len(records)
}

func swapProcTreeReaper(fn func(proctree.Record)) func() {
	prev := reapProcTreeRecordFunc
	reapProcTreeRecordFunc = fn
	return func() { reapProcTreeRecordFunc = prev }
}
