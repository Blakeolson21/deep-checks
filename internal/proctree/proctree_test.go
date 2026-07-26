//go:build unix

package proctree

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestParseSnapshot_ReadsPIDPPIDPGIDAndStartTime(t *testing.T) {
	out := "    1     0     1 Wed Jul 15 06:33:44 2026\n" +
		"  307 39181 39181 Fri Jul 24 18:46:26 2026\n"

	got := parseSnapshot(out, time.UTC)

	if len(got) != 2 {
		t.Fatalf("parseSnapshot returned %d procs, want 2: %+v", len(got), got)
	}
	if got[0].PID != 1 || got[0].PPID != 0 || got[0].PGID != 1 {
		t.Errorf("first proc = %+v, want pid 1 ppid 0 pgid 1", got[0])
	}
	want := time.Date(2026, time.July, 24, 18, 46, 26, 0, time.UTC)
	if !got[1].Started.Equal(want) {
		t.Errorf("second proc start = %v, want %v", got[1].Started, want)
	}
}

// TestParseSnapshot_HandlesSingleDigitDayPadding covers the macOS lstart format
// for days 1-9, which pads with a second space ("Jul  3"). Go's "Jan 2" layout
// rejects that padding outright, so the parser must normalize whitespace rather
// than reuse the layout in internal/daemon/proc_unix.go verbatim.
func TestParseSnapshot_HandlesSingleDigitDayPadding(t *testing.T) {
	got := parseSnapshot("  132     1   132 Fri Jul  3 23:11:45 2026\n", time.UTC)

	if len(got) != 1 {
		t.Fatalf("parseSnapshot returned %d procs, want 1", len(got))
	}
	want := time.Date(2026, time.July, 3, 23, 11, 45, 0, time.UTC)
	if !got[0].Started.Equal(want) {
		t.Fatalf("start = %v, want %v", got[0].Started, want)
	}
}

func TestParseSnapshot_SkipsMalformedLines(t *testing.T) {
	out := "\n" +
		"garbage\n" +
		"   -1     0     1 Wed Jul 15 06:33:44 2026\n" +
		"  400   399   399 not a real date\n" +
		"  500   499   499 Wed Jul 15 06:33:44 2026\n"

	got := parseSnapshot(out, time.UTC)

	if len(got) != 1 || got[0].PID != 500 {
		t.Fatalf("parseSnapshot = %+v, want only pid 500", got)
	}
}

// TestDescendants_WalksTransitivelyAcrossProcessGroups is the core of the fix:
// a setsid() child has its own pgid, so a group-based reap misses it and every
// process beneath it. The walk must follow ppid links regardless of pgid.
func TestDescendants_WalksTransitivelyAcrossProcessGroups(t *testing.T) {
	snap := []Proc{
		{PID: 100, PPID: 1, PGID: 100},   // leader
		{PID: 200, PPID: 100, PGID: 100}, // ordinary child, same group
		{PID: 300, PPID: 200, PGID: 300}, // setsid escapee, own group
		{PID: 400, PPID: 300, PGID: 300}, // escapee's child
		{PID: 900, PPID: 1, PGID: 900},   // unrelated
	}

	got := pidSet(Descendants(snap, 100))

	for _, pid := range []int{200, 300, 400} {
		if !got[pid] {
			t.Errorf("descendant %d missing from %v", pid, got)
		}
	}
	if got[900] {
		t.Errorf("unrelated pid 900 included in %v", got)
	}
	if got[100] {
		t.Errorf("leader 100 must not be listed as its own descendant: %v", got)
	}
}

// TestDescendants_IncludesGroupMembersMissingAPPIDLink covers the reparented
// case: once an intermediate parent exits, the kernel rewrites ppid to 1 and the
// trail back to the leader is gone. Anything still carrying the leader's pgid is
// still ours, so pgid is a second, independent way in.
func TestDescendants_IncludesGroupMembersMissingAPPIDLink(t *testing.T) {
	snap := []Proc{
		{PID: 100, PPID: 1, PGID: 100},
		{PID: 500, PPID: 1, PGID: 100}, // reparented, still in the leader's group
	}

	if got := pidSet(Descendants(snap, 100)); !got[500] {
		t.Fatalf("reparented group member 500 missing from %v", got)
	}
}

func TestDescendants_TerminatesOnPPIDCycle(t *testing.T) {
	snap := []Proc{
		{PID: 100, PPID: 1, PGID: 100},
		{PID: 200, PPID: 100, PGID: 100},
		{PID: 300, PPID: 200, PGID: 100},
		{PID: 201, PPID: 202, PGID: 700}, // mutually-referential pair, unrelated
		{PID: 202, PPID: 201, PGID: 700},
	}

	done := make(chan map[int]bool, 1)
	go func() { done <- pidSet(Descendants(snap, 100)) }()

	select {
	case got := <-done:
		if !got[200] || !got[300] {
			t.Fatalf("expected 200 and 300 in %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Descendants did not terminate on a ppid cycle")
	}
}

func TestDescendants_UnknownLeaderYieldsNothing(t *testing.T) {
	snap := []Proc{{PID: 100, PPID: 1, PGID: 100}}

	if got := Descendants(snap, 12345); len(got) != 0 {
		t.Fatalf("Descendants for an unknown leader = %+v, want empty", got)
	}
}

// TestKill_SkipsRecycledPID is the pid-reuse guard. Between the snapshot and the
// signal the kernel can hand a pid to an unrelated process; killing it would be
// the exact class of collateral damage this package exists to prevent. A start
// time that no longer matches means the pid was recycled.
func TestKill_SkipsRecycledPID(t *testing.T) {
	recorded := time.Date(2026, time.July, 21, 17, 9, 0, 0, time.UTC)
	victim := Proc{PID: 4242, PPID: 100, PGID: 4242, Started: recorded}

	var signalled []int
	defer swapForTest(
		// Same pid, different start time: a different process now.
		func([]int) (map[int]time.Time, error) {
			return map[int]time.Time{4242: recorded.Add(time.Hour)}, nil
		},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	Kill([]Proc{victim})

	if len(signalled) != 0 {
		t.Fatalf("Kill signalled recycled pids %v, want none", signalled)
	}
}

func TestKill_SignalsPIDWhoseStartTimeStillMatches(t *testing.T) {
	started := time.Date(2026, time.July, 21, 17, 9, 0, 0, time.UTC)
	victim := Proc{PID: 4242, PPID: 100, PGID: 4242, Started: started}

	var signalled []int
	defer swapForTest(
		func([]int) (map[int]time.Time, error) { return map[int]time.Time{4242: started}, nil },
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	Kill([]Proc{victim})

	if len(signalled) != 1 || signalled[0] != 4242 {
		t.Fatalf("Kill signalled %v, want [4242]", signalled)
	}
}

// TestKill_RefusesSelfInitAndOwnAncestors is the blast-radius guard. A bug in the
// walk that ever produced our own pid, our parent, or pid 1 must not be able to
// take down the daemon or the machine's init.
func TestKill_RefusesSelfInitAndOwnAncestors(t *testing.T) {
	started := time.Date(2026, time.July, 21, 17, 9, 0, 0, time.UTC)
	self := os.Getpid()
	parent := os.Getppid()
	procs := []Proc{
		{PID: 1, PPID: 0, PGID: 1, Started: started},
		{PID: self, PPID: parent, PGID: self, Started: started},
		{PID: parent, PPID: 1, PGID: parent, Started: started},
	}

	var signalled []int
	defer swapForTest(
		func([]int) (map[int]time.Time, error) {
			return map[int]time.Time{1: started, self: started, parent: started}, nil
		},
		func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil },
	)()

	Kill(procs)

	if len(signalled) != 0 {
		t.Fatalf("Kill signalled protected pids %v, want none", signalled)
	}
}

func pidSet(procs []Proc) map[int]bool {
	out := make(map[int]bool, len(procs))
	for _, p := range procs {
		out[p.PID] = true
	}
	return out
}

// swapForTest replaces the start-time lookup and the kill syscall, and forces
// the protected-pid set to be recomputed from a synthetic process table so the
// cached real one cannot leak between tests.
func swapForTest(times func([]int) (map[int]time.Time, error), kill func(int, syscall.Signal) error) func() {
	prevTimes, prevKill, prevSet := startTimesFunc, killFunc, protectedSet
	startTimesFunc, killFunc = times, kill
	protectedOnce.Do(func() {})
	protectedSet = map[int]bool{0: true, 1: true, os.Getpid(): true, os.Getppid(): true}
	return func() {
		startTimesFunc, killFunc, protectedSet = prevTimes, prevKill, prevSet
	}
}
