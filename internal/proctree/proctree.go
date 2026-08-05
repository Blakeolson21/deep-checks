// Package proctree enumerates and kills process trees.
//
// It exists because a process group is not a process tree. Setpgid isolates a
// spawned leader in its own group, but a descendant that calls setsid() - which
// is what Node's `detached: true` does, and therefore what Claude Code's CLI
// Bash tool does - leaves that group and becomes unreachable by kill(-pgid),
// along with everything beneath it. Reaping such a descendant requires walking
// ppid links, not signalling a group.
//
// The package knows nothing about runs, steps, or configuration. It is
// deliberately small so its blast radius is auditable: every per-pid kill and
// group kill is guarded by a freshly re-read start time, and both also refuse a
// protected pid - pid 0 and 1, the current process, its ancestors, and the
// leader of its own process group.
package proctree

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// startTimeTolerance bounds how far a recorded process start time may drift from
// the one the kernel reports now and still be considered the same process. It
// absorbs clock quirks and ps's one-second resolution. It mirrors
// orphanStartTimeTolerance in internal/daemon/recover_servers.go.
const startTimeTolerance = 2 * time.Second

// Proc is one row of a process snapshot.
type Proc struct {
	PID     int
	PPID    int
	PGID    int
	Started time.Time
}

// snapshotFunc, startTimesFunc and killFunc are swapped in tests. Signalling
// real pids from a unit test is not something to do on a developer's machine.
var (
	snapshotFunc   = snapshot
	startTimesFunc = startTimes
	killFunc       = killProcess
)

// Snapshot lists every visible process.
func Snapshot() ([]Proc, error) { return snapshotFunc() }

// parseSnapshot turns `ps -Ao pid=,ppid=,pgid=,lstart=` output into Procs.
// lstart is last in the format because it is the only field containing spaces.
//
// Whitespace is normalized before parsing: macOS pads single-digit days with a
// second space ("Jul  3"), and Go's "Jan 2" layout rejects that outright.
// Unparseable lines are skipped rather than failing the whole snapshot, since a
// partial process list still lets the reaper do useful work.
func parseSnapshot(out string, loc *time.Location) []Proc {
	if loc == nil {
		loc = time.Local
	}
	var procs []Proc
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			continue
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil || pgid < 0 {
			continue
		}
		started, err := time.ParseInLocation(startTimeLayout, strings.Join(fields[3:], " "), loc)
		if err != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, PGID: pgid, Started: started})
	}
	return procs
}

// startTimeLayout matches ps lstart output after whitespace normalization.
const startTimeLayout = "Mon Jan 2 15:04:05 2006"

// Descendants returns every process below leaderPID in snap, excluding the
// leader itself.
//
// Two independent criteria are used, because either one alone has a blind spot:
//
//   - Transitive ppid links catch a setsid() escapee and everything beneath it,
//     which a group-based reap cannot reach.
//   - Shared pgid catches a process whose intermediate parent already exited, at
//     which point the kernel rewrites its ppid to 1 and the trail to the leader
//     is gone.
//
// The pgid criterion is applied only when the leader is itself a group leader
// (PGID == PID), which ConfigureShellCommand guarantees via Setpgid. Without
// that check, a leader that inherited the daemon's group would drag every
// sibling process - including the daemon - into the result.
func Descendants(snap []Proc, leaderPID int) []Proc {
	byPID := make(map[int]Proc, len(snap))
	children := make(map[int][]int, len(snap))
	for _, p := range snap {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	leader, ok := byPID[leaderPID]
	if !ok {
		return nil
	}

	// Seeding the visited set with the leader both excludes it from the result
	// and makes a ppid cycle terminate.
	seen := map[int]bool{leaderPID: true}
	var out []Proc

	queue := append([]int(nil), children[leaderPID]...)
	if leader.PGID == leader.PID {
		for _, p := range snap {
			if p.PGID == leader.PGID && p.PID != leaderPID {
				queue = append(queue, p.PID)
			}
		}
	}

	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := byPID[pid]; ok {
			out = append(out, p)
		}
		queue = append(queue, children[pid]...)
	}
	return out
}

// Kill SIGKILLs each process whose recorded start time still matches the one the
// kernel reports, skipping anything that cannot be verified.
//
// Verification is the whole point. Between the sample that produced procs and
// this call, the kernel can recycle a pid onto an unrelated process; signalling
// it would be precisely the collateral damage this package exists to prevent. If
// the check cannot be made, nothing is killed - refusing to reap is always
// recoverable, killing the wrong process is not.
//
// Verification is targeted rather than a full process listing on purpose. This
// runs on every command teardown in the harness, including short-lived git
// subprocesses, and a full `ps -A` costs tens of milliseconds (hundreds under
// the race detector) with roughly a thousand processes on the box. Paying that
// per command would be a self-inflicted version of the slowdown this package
// exists to fix. With nothing to kill - the overwhelmingly common case - Kill
// costs nothing at all.
func Kill(procs []Proc) {
	if len(procs) == 0 {
		return
	}
	protected := protectedPIDs()
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		if !protected[p.PID] {
			pids = append(pids, p.PID)
		}
	}
	if len(pids) == 0 {
		return
	}
	actual, err := startTimesFunc(pids)
	if err != nil {
		return
	}
	for _, want := range procs {
		if protected[want.PID] {
			continue
		}
		now, ok := actual[want.PID]
		if !ok {
			continue // already gone
		}
		if !sameProcess(want.Started, now) {
			continue // pid recycled onto something unrelated
		}
		_ = killFunc(want.PID, syscall.SIGKILL)
	}
}

// KillGroups SIGKILLs each group whose leader pid is still the same process that
// sampling recorded.
//
// A group kill is the highest-blast-radius operation here: a stale pgid does not
// signal one wrong process, it signals every member of whatever group that pid
// now leads. Pids are recycled, and the gap between sampling and reaping can be
// long - a step in the motivating incident ran 86 minutes, and a persisted
// record can be days old - so an unverified pgid is a licence to SIGKILL an
// unrelated group.
//
// The leader's sampled start time comes from recorded, which works because a
// setsid() escapee leads its own group and is therefore always sampled into the
// descendant union alongside it. A group with no recorded leader fails closed
// and is skipped; the start-time-guarded per-pid Kill still covers its members.
//
// Protected pids are refused before verification for the same reason the per-pid
// kill refuses them: no walk that starts from a Setpgid-isolated leader can
// reach our own ancestry today, but the cost of a future one that does is the
// daemon signalling its own group, so the guard belongs on the wider-blast-radius
// operation rather than only on the narrower one.
func KillGroups(groups []int, recorded []Proc) {
	if len(groups) == 0 {
		return
	}
	protected := protectedPIDs()
	sampledStart := make(map[int]time.Time, len(recorded))
	for _, p := range recorded {
		sampledStart[p.PID] = p.Started
	}

	verify := make([]int, 0, len(groups))
	for _, pgid := range groups {
		if protected[pgid] {
			continue
		}
		if _, ok := sampledStart[pgid]; ok {
			verify = append(verify, pgid)
		}
	}
	if len(verify) == 0 {
		return
	}
	actual, err := startTimesFunc(verify)
	if err != nil {
		return
	}
	for _, pgid := range verify {
		now, ok := actual[pgid]
		if !ok {
			continue // group leader already gone
		}
		if !sameProcess(sampledStart[pgid], now) {
			continue // pid recycled onto an unrelated group leader
		}
		killGroup(pgid)
	}
}

var (
	protectedOnce sync.Once
	protectedSet  map[int]bool
)

// protectedPIDs is the set that must never be signalled: pid 0 and 1, the
// current process, every ancestor of the current process, and the leader of the
// current process's own group. Under the daemon those ancestors include the
// daemon itself and the service supervisor, and the group leader is what a
// mistaken group kill would take down wholesale.
//
// Strictly this is belt and braces - our own ancestors sit above us in the tree
// and cannot appear among a command's descendants - but the cost of a bug in the
// walk is killing the daemon or init, so the guard stays. The set is computed
// once per process because ancestry does not meaningfully change, which keeps
// the full listing it needs off the per-command path.
func protectedPIDs() map[int]bool {
	protectedOnce.Do(func() { protectedSet = computeProtectedPIDs() })
	return protectedSet
}

func computeProtectedPIDs() map[int]bool {
	protected := map[int]bool{0: true, 1: true, os.Getpid(): true}
	if pgrp := processGroup(); pgrp > 1 {
		protected[pgrp] = true
	}
	snap, err := snapshotFunc()
	if err != nil {
		return protected
	}
	byPID := make(map[int]Proc, len(snap))
	for _, p := range snap {
		byPID[p.PID] = p
	}
	for pid := os.Getpid(); ; {
		p, ok := byPID[pid]
		if !ok || protected[p.PPID] {
			break
		}
		protected[p.PPID] = true
		pid = p.PPID
	}
	return protected
}

func sameProcess(recorded, actual time.Time) bool {
	if recorded.IsZero() || actual.IsZero() {
		return false
	}
	diff := actual.Sub(recorded)
	if diff < 0 {
		diff = -diff
	}
	return diff <= startTimeTolerance
}
