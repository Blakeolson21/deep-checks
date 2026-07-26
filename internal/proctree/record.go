package proctree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Record is the on-disk trail a live process tree leaves behind so a restarted
// daemon can finish reaping it.
//
// It exists because the in-memory descendant union dies with the daemon. If the
// daemon is SIGKILLed - which is exactly what the OOM killer does when leaked
// worker pools exhaust the host - every leaked descendant becomes unattributable
// the moment its parent exits and the kernel rewrites its ppid to 1. The record
// is the only thing that can still name them afterwards.
//
// The recorded start times are what make the record safe to act on later.
// LeaderStart protects the whole-tree ownership decision, while each
// descendant's start time protects both its per-pid kill and any group kill it
// leads. Pids are recycled, so bare pid or pgid lists read minutes or days after
// the fact would be a licence to kill strangers.
type Record struct {
	LeaderPID   int       `json:"leader_pid"`
	LeaderStart time.Time `json:"leader_started_at"`
	Descendants []Proc    `json:"descendants,omitempty"`
	Groups      []int     `json:"groups,omitempty"`
}

// WriteRecord persists rec as <dir>/<leaderPID>.json.
//
// The write goes to a temp file and is then renamed, so a daemon killed
// mid-write leaves either the old record or the new one, never a half-parsed
// one that recovery would have to guess about.
func WriteRecord(dir string, rec Record) error {
	if rec.LeaderPID <= 0 {
		return fmt.Errorf("proctree: invalid leader pid %d", rec.LeaderPID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	final := recordPath(dir, rec.LeaderPID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// ReadRecords loads every record in dir. A missing directory is not an error:
// it just means no tree was ever tracked under this root.
//
// Unparseable files are skipped rather than failing the sweep. A truncated
// record is the expected artifact of an abrupt daemon death, and it must not
// stop the intact records from being recovered.
func ReadRecords(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.LeaderPID <= 0 {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// RemoveRecord deletes a leader's record. It is a no-op if the file is gone.
func RemoveRecord(dir string, leaderPID int) {
	_ = os.Remove(recordPath(dir, leaderPID))
	_ = os.Remove(recordPath(dir, leaderPID) + ".tmp")
}

// ReapRecord kills whatever still survives from a recorded tree.
//
// If the leader pid is alive with a matching start time the tree still belongs
// to a live command, so nothing is killed: another daemon owns it. Otherwise the
// leader is gone and any recorded descendant that is still running with a
// matching start time is an orphan, which is precisely what this recovers.
//
// Recorded groups go through KillGroups, which re-verifies each group leader's
// start time before signalling. That guard matters most here: a record survives
// daemon restarts and can be days old, so a bare pgid list would be a licence to
// SIGKILL a whole group that a recycled pid has since led.
func ReapRecord(rec Record) {
	snap, err := snapshotFunc()
	if err != nil {
		return
	}
	for _, p := range snap {
		if p.PID == rec.LeaderPID && sameProcess(rec.LeaderStart, p.Started) {
			return // still live; not ours to reap
		}
	}
	KillGroups(rec.Groups, rec.Descendants)
	Kill(rec.Descendants)
}

func recordPath(dir string, leaderPID int) string {
	return filepath.Join(dir, strconv.Itoa(leaderPID)+".json")
}
