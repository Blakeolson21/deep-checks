//go:build unix

package shellenv

import (
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// trackerTick is how often the poller samples the process table. It is a
// variable so tests can shorten it.
//
// The value trades leak coverage against cost. One tick is one `ps` invocation
// for the whole process, no matter how many leaders are registered, so 15s is
// cheap even under a full pipeline. The residual gap is a process that both
// spawns and loses its parent inside a single tick; nothing short of ptrace or a
// PID namespace closes that. For calibration, the six xctest processes from gate
// run 01KY36EAXX53WZSEC4ZYSMY4F0 lived about 16 minutes, so they would have been
// sampled roughly 60 times.
var trackerTick = 15 * time.Second

// trackerFirstSample bounds how long a leader can run before it is sampled at
// all. It is much shorter than trackerTick because the first sample is the one
// that decides whether an early-cancelled command is reapable.
var trackerFirstSample = time.Second

// trackedLeader accumulates everything ever observed beneath one leader.
//
// It is a union over time, never a current-state view. That is the entire point:
// by the time a cleanly-exited leader is reaped, the kernel has already erased
// the ppid links that would let anyone reconstruct its descendants. Only samples
// taken while the leader was alive survive that.
type trackedLeader struct {
	pid         int
	started     time.Time
	descendants map[int]proctree.Proc
	groups      map[int]bool
}

// poller is the currently running sampler, or nil when no leader is registered.
//
// Each generation owns its own stop and done channels rather than sharing a
// WaitGroup. A shared WaitGroup would have Add called under trackerMu while Wait
// ran outside it, which is the counter-reuse race the race detector rightly
// flags: a new poller can start the instant the lock is released, before the old
// one has been waited on.
type poller struct {
	stop chan struct{}
	done chan struct{}
}

var (
	trackerMu    sync.Mutex
	trackedByPID = map[int]*trackedLeader{}
	activePoller *poller
	recordDir    string
)

// SetProcessRecordDir points descendant tracking at a directory where it
// persists one record per live leader, and returns a func restoring the previous
// value.
//
// The daemon calls this once at startup with a directory under NM_HOME. It is
// unset everywhere else - the CLI and the test suite have no NM_HOME worth
// writing into, and an empty dir means records are kept in memory only.
func SetProcessRecordDir(dir string) func() {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	prev := recordDir
	recordDir = dir
	return func() {
		trackerMu.Lock()
		defer trackerMu.Unlock()
		recordDir = prev
	}
}

// trackLeader registers a started command leader and starts the poller if it is
// not already running.
func trackLeader(pid int, started time.Time) {
	if pid <= 1 {
		return
	}
	trackerMu.Lock()
	defer trackerMu.Unlock()
	trackedByPID[pid] = &trackedLeader{
		pid:         pid,
		started:     started,
		descendants: map[int]proctree.Proc{},
		groups:      map[int]bool{},
	}
	if activePoller == nil {
		activePoller = &poller{stop: make(chan struct{}), done: make(chan struct{})}
		go pollDescendants(activePoller)
	}
}

// takeTrackedLeader removes a leader's accumulated set and returns it. Removal
// is what makes repeated TerminateShellCommandGroup calls idempotent: the second
// call finds nothing left to reap.
func takeTrackedLeader(pid int) ([]proctree.Proc, []int) {
	trackerMu.Lock()
	entry, ok := trackedByPID[pid]
	if ok {
		delete(trackedByPID, pid)
	}
	dir := recordDir
	var stopping *poller
	if len(trackedByPID) == 0 && activePoller != nil {
		stopping = activePoller
		activePoller = nil
		close(stopping.stop)
	}
	trackerMu.Unlock()

	// Join the retiring poller so no sampler goroutine outlives the last
	// tracked leader. A record cannot be resurrected after RemoveRecord in any
	// case - a sample that no longer finds the leader in trackedByPID writes
	// nothing - so this is goroutine hygiene and deterministic teardown, not a
	// correctness guard.
	if stopping != nil {
		<-stopping.done
	}
	// The record is dropped before the reap rather than after: if this process
	// dies mid-reap, a restarted daemon re-deriving the tree from a fresh
	// snapshot is safer than one replaying a stale pid list.
	if dir != "" {
		proctree.RemoveRecord(dir, pid)
	}
	if !ok {
		return nil, nil
	}

	procs := make([]proctree.Proc, 0, len(entry.descendants))
	for _, p := range entry.descendants {
		procs = append(procs, p)
	}
	groups := make([]int, 0, len(entry.groups))
	for pgid := range entry.groups {
		groups = append(groups, pgid)
	}
	return procs, groups
}

// pollDescendants folds each leader's current descendants into its accumulated
// union. One snapshot serves every registered leader, so the cost is one `ps`
// per round regardless of how many commands are in flight.
//
// It samples until stopped, re-reading the interval each round so a
// test that shortens it takes effect on an already-running poller. The intervals
// are read under trackerMu because this goroutine outlives the caller that set
// them.
func pollDescendants(p *poller) {
	defer close(p.done)
	stop := p.stop

	// The first sample comes quickly rather than after a full tick. A command
	// cancelled in its first seconds would otherwise have an empty union, and
	// the reap path runs no listing of its own, so anything it had already
	// spawned would be unreachable. Short-lived commands finish before this
	// fires and pay nothing.
	interval := trackerInterval(true)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			sampleDescendantsOnce()
			timer.Reset(trackerInterval(false))
		}
	}
}

func trackerInterval(first bool) time.Duration {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	if first && trackerFirstSample < trackerTick {
		return trackerFirstSample
	}
	return trackerTick
}

func sampleDescendantsOnce() {
	// Signal once per round on every exit path, including rounds that find
	// nothing. Firing only when descendants turn up would leave a waiter with no
	// way to learn that a round completed empty.
	defer notifySampled()

	trackerMu.Lock()
	leaders := make([]int, 0, len(trackedByPID))
	for pid := range trackedByPID {
		leaders = append(leaders, pid)
	}
	trackerMu.Unlock()
	if len(leaders) == 0 {
		return
	}

	snap, err := proctree.Snapshot()
	if err != nil {
		return
	}

	for _, pid := range leaders {
		found := proctree.Descendants(snap, pid)
		if len(found) == 0 {
			continue
		}
		trackerMu.Lock()
		entry, ok := trackedByPID[pid]
		if ok {
			for _, p := range found {
				entry.descendants[p.PID] = p
				// Record the group too. A setsid() escapee leads its own group,
				// so killing that group later also reaches children it spawned
				// after this sample was taken.
				if p.PGID != pid && p.PGID > 1 {
					entry.groups[p.PGID] = true
				}
			}
		}
		rec, dir := snapshotRecordLocked(entry, ok)
		trackerMu.Unlock()

		if dir != "" {
			_ = proctree.WriteRecord(dir, rec)
		}
	}
}

// snapshotRecordLocked copies a leader's accumulated union into a Record for
// persistence. The copy is taken under the lock so the write itself - which
// touches the filesystem - happens outside it.
func snapshotRecordLocked(entry *trackedLeader, ok bool) (proctree.Record, string) {
	if !ok || recordDir == "" {
		return proctree.Record{}, ""
	}
	rec := proctree.Record{LeaderPID: entry.pid, LeaderStart: entry.started}
	for _, p := range entry.descendants {
		rec.Descendants = append(rec.Descendants, p)
	}
	for pgid := range entry.groups {
		rec.Groups = append(rec.Groups, pgid)
	}
	return rec, recordDir
}

// sampledCh is closed and replaced after every sample so a test can wait for the
// poller to have actually run rather than sleeping and hoping. Production code
// never reads it.
var sampledCh = make(chan struct{})

func notifySampled() {
	trackerMu.Lock()
	prev := sampledCh
	sampledCh = make(chan struct{})
	trackerMu.Unlock()
	close(prev)
}

// trackedDescendantSampled reports whether the poller has recorded descendantPID
// under leaderPID.
func trackedDescendantSampled(leaderPID, descendantPID int) bool {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	entry, ok := trackedByPID[leaderPID]
	if !ok {
		return false
	}
	_, found := entry.descendants[descendantPID]
	return found
}

func sampleSignal() <-chan struct{} {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	return sampledCh
}
