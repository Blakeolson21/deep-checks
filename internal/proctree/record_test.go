//go:build unix

package proctree

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWriteRecordAndReadRecords_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, time.July, 21, 16, 37, 26, 0, time.UTC)
	rec := Record{
		LeaderPID:   4242,
		LeaderStart: started,
		Descendants: []Proc{{PID: 4243, PPID: 4242, PGID: 4243, Started: started}},
		Groups:      []int{4243},
	}

	if err := WriteRecord(dir, rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	got, err := ReadRecords(dir)
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ReadRecords returned %d records, want 1", len(got))
	}
	if got[0].LeaderPID != 4242 || len(got[0].Descendants) != 1 {
		t.Fatalf("round-tripped record = %+v", got[0])
	}
	if !got[0].Descendants[0].Started.Equal(started) {
		t.Fatalf("descendant start = %v, want %v", got[0].Descendants[0].Started, started)
	}
	if len(got[0].Groups) != 1 || got[0].Groups[0] != 4243 {
		t.Fatalf("groups = %v, want [4243]", got[0].Groups)
	}
}

func TestReadRecords_MissingDirIsNotAnError(t *testing.T) {
	got, err := ReadRecords(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("ReadRecords on a missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records, want 0", len(got))
	}
}

// TestReadRecords_SkipsUnparseableFiles keeps a truncated record - the expected
// artifact of a daemon SIGKILLed mid-write - from blocking recovery of the
// records that are intact.
func TestReadRecords_SkipsUnparseableFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "999.json"), []byte("{trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(dir, Record{LeaderPID: 4242, LeaderStart: time.Now()}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadRecords(dir)
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(got) != 1 || got[0].LeaderPID != 4242 {
		t.Fatalf("ReadRecords = %+v, want only the intact record", got)
	}
}

func TestRemoveRecord_DeletesTheFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRecord(dir, Record{LeaderPID: 4242, LeaderStart: time.Now()}); err != nil {
		t.Fatal(err)
	}

	RemoveRecord(dir, 4242)

	got, err := ReadRecords(dir)
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("record survived RemoveRecord: %+v", got)
	}
}

// TestReapRecord_SkipsWhenLeaderPIDWasRecycled is the restart-safety guard. A
// record outlives the daemon that wrote it, so by the time recovery reads it the
// leader pid may belong to an unrelated process. Recording the leader's start
// time is what makes that distinguishable; without the check, recovery would
// SIGKILL a stranger.
func TestReapRecord_SkipsWhenLeaderPIDWasRecycled(t *testing.T) {
	recorded := time.Date(2026, time.July, 21, 16, 37, 26, 0, time.UTC)
	rec := Record{
		LeaderPID:   4242,
		LeaderStart: recorded,
		Descendants: []Proc{{PID: 4243, PPID: 4242, PGID: 4243, Started: recorded}},
	}

	var signalled []int
	defer swapRecordReapForTest(
		// The leader pid exists again, but as a different process.
		[]Proc{{PID: 4242, PPID: 1, PGID: 4242, Started: recorded.Add(time.Hour)}},
		map[int]time.Time{4243: recorded.Add(time.Hour)},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	ReapRecord(rec)

	if len(signalled) != 0 {
		t.Fatalf("ReapRecord signalled %v after pid reuse, want none", signalled)
	}
}

// TestReapRecord_KillsSurvivingDescendantsWhoseStartTimesMatch covers the case
// recovery exists for: the leader is long gone, but descendants it leaked are
// still burning CPU under PPID=1 with no trail back to anything.
func TestReapRecord_KillsSurvivingDescendantsWhoseStartTimesMatch(t *testing.T) {
	started := time.Date(2026, time.July, 21, 17, 9, 0, 0, time.UTC)
	rec := Record{
		LeaderPID:   4242,
		LeaderStart: started,
		Descendants: []Proc{{PID: 4243, PPID: 4242, PGID: 4243, Started: started}},
	}

	var signalled []int
	defer swapRecordReapForTest(
		// Leader is gone; the orphaned descendant survives, reparented to 1.
		[]Proc{{PID: 4243, PPID: 1, PGID: 4243, Started: started}},
		map[int]time.Time{4243: started},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	ReapRecord(rec)

	if len(signalled) != 1 || signalled[0] != 4243 {
		t.Fatalf("ReapRecord signalled %v, want [4243]", signalled)
	}
}

// TestReapRecord_KillsVerifiedGroup confirms a recorded group is killed when its
// leader pid is still the same process the record captured.
func TestReapRecord_KillsVerifiedGroup(t *testing.T) {
	started := time.Date(2026, time.July, 21, 18, 0, 0, 0, time.UTC)
	rec := Record{
		LeaderPID:   4242,
		LeaderStart: started,
		Descendants: []Proc{{PID: 4243, PPID: 1, PGID: 4243, Started: started}},
		Groups:      []int{4243},
	}

	var signalled []int
	defer swapRecordReapForTest(
		[]Proc{{PID: 4243, PPID: 1, PGID: 4243, Started: started}},
		map[int]time.Time{4243: started},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	ReapRecord(rec)

	// -4243 is the group kill; 4243 is the per-pid descendant kill.
	if len(signalled) != 2 || signalled[0] != -4243 || signalled[1] != 4243 {
		t.Fatalf("ReapRecord signalled %v, want [-4243 4243]", signalled)
	}
}

// TestReapRecord_SkipsGroupWhenLeaderPIDWasRecycled is the group-kill analogue of
// the leader-recycle guard: a records-are-days-old pgid whose leader pid now
// belongs to an unrelated process must not have its whole group SIGKILLed.
func TestReapRecord_SkipsGroupWhenLeaderPIDWasRecycled(t *testing.T) {
	recorded := time.Date(2026, time.July, 21, 18, 30, 0, 0, time.UTC)
	rec := Record{
		LeaderPID:   4242,
		LeaderStart: recorded,
		Descendants: []Proc{{PID: 4243, PPID: 1, PGID: 4243, Started: recorded}},
		Groups:      []int{4243},
	}

	var signalled []int
	defer swapRecordReapForTest(
		// The group leader pid exists again, but as a different process.
		[]Proc{{PID: 4243, PPID: 1, PGID: 4243, Started: recorded.Add(time.Hour)}},
		map[int]time.Time{4243: recorded.Add(time.Hour)},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	ReapRecord(rec)

	if len(signalled) != 0 {
		t.Fatalf("ReapRecord signalled %v after group-leader pid reuse, want none", signalled)
	}
}

// swapRecordReapForTest stubs both seams ReapRecord uses: the full listing that
// decides whether the leader is still alive, and the targeted start-time lookup
// Kill verifies descendants with.
func swapRecordReapForTest(snap []Proc, times map[int]time.Time, kill func(int, syscall.Signal) error) func() {
	prevSnap := snapshotFunc
	snapshotFunc = func() ([]Proc, error) { return snap, nil }
	restore := swapForTest(
		func([]int) (map[int]time.Time, error) { return times, nil },
		kill,
	)
	return func() {
		restore()
		snapshotFunc = prevSnap
	}
}
