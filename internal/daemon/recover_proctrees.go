package daemon

import (
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// reapProcTreeRecordFunc is swapped in tests so the sweep can be exercised
// without signalling real pids.
var reapProcTreeRecordFunc = proctree.ReapRecord

// reapOrphanedProcessTrees kills process trees left running by a crashed
// predecessor daemon and deletes their records.
//
// It covers the gap reapOrphanedServers does not: that function only knows about
// long-lived managed servers (opencode, rovodev) that write a PID file of their
// own. Nothing tracked the subprocesses a step or agent spawned, so a test
// runner's workers could outlive their run, their daemon, and every daemon
// after it. In gate run 01KY36EAXX53WZSEC4ZYSMY4F0 six such processes were still
// burning ~50% CPU each three days later.
//
// Safety rules, matching reapOrphanedServers:
//   - If another daemon is alive, skip everything. The daemon is machine-wide,
//     so its running steps own live process trees that must not be killed.
//   - ReapRecord itself refuses to touch a tree whose leader is still alive with
//     a matching start time, and verifies each descendant's start time before
//     signalling, so a recycled pid is never the victim.
func reapOrphanedProcessTrees(p *paths.Paths) {
	dir := p.ProcTreesDir()
	if otherDaemonAlive(p) {
		slog.Info("another daemon appears to be running; skipping process-tree reap", "dir", dir)
		return
	}
	records, err := proctree.ReadRecords(dir)
	if err != nil {
		slog.Warn("read process tree records", "dir", dir, "error", err)
		return
	}
	for _, rec := range records {
		if rec.LeaderPID == 0 {
			continue
		}
		slog.Info("reaping orphaned process tree",
			"leader_pid", rec.LeaderPID, "descendants", len(rec.Descendants))
		reapProcTreeRecordFunc(rec)
		proctree.RemoveRecord(dir, rec.LeaderPID)
	}
}
