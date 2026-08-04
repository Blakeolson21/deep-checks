package lanehealth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/filelock"
)

// maxLanes bounds the persisted state so a misconfigured agent list cannot
// grow the file forever. Entries closest to expiry are dropped first.
const maxLanes = 32

// Store persists lane outages in a small JSON file under NM_HOME so a mark
// discovered by one run is honored by every concurrent run and by every later
// run, including after a daemon restart. Without that, each run pays a full
// agent spawn to rediscover the same exhausted lane - the 2026-08-04 incident,
// where roughly a dozen runs failed one after another on the same dead Codex
// quota.
//
// Reads are lock-free and see whole files only, because writes land via
// os.Rename. Writes take a short advisory file lock so two runs marking
// different lanes at the same moment cannot lose one another's mark.
type Store struct {
	path string
	now  func() time.Time
}

// NewStore returns a Store persisting at path. now is injectable for
// deterministic tests; nil means time.Now.
func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now}
}

type state struct {
	Lanes map[string]Outage `json:"lanes"`
}

// Outage reports the live outage for lane, if any. A mark whose reset time has
// arrived is not live: the lane is presumed recovered and gets tried again.
func (s *Store) Outage(lane string) (Outage, bool) {
	if s == nil || s.path == "" {
		return Outage{}, false
	}
	current := s.load()
	outage, ok := current.Lanes[lane]
	if !ok || !outage.Until.After(s.now()) {
		return Outage{}, false
	}
	return outage, true
}

// Mark records an outage, replacing any existing mark for the same lane.
func (s *Store) Mark(outage Outage) error {
	if s == nil || s.path == "" || outage.Lane == "" {
		return nil
	}
	return s.mutate(func(current *state) {
		current.Lanes[outage.Lane] = outage
	})
}

// ClearObservedBefore drops the mark for lane when it was observed no later
// than startedAt. A lane that just completed an invocation is demonstrably
// healthy, so its mark - including one written from a misread banner - must not
// outlive that evidence.
//
// The cutoff is what makes the mark sticky across concurrent runs: an
// invocation authorized before the provider ran out of quota still completes,
// and its success says nothing about a banner another run hit while it was
// streaming. Clearing that fresher mark would send the next run right back into
// the dead lane, which is the burst this package exists to stop. A mark with no
// ObservedAt - a legacy row, or one written by hand - carries no such evidence
// and is always cleared.
func (s *Store) ClearObservedBefore(lane string, startedAt time.Time) error {
	if s == nil || s.path == "" || lane == "" {
		return nil
	}
	current := s.load()
	if !clearable(current, lane, startedAt) {
		return nil
	}
	return s.mutate(func(current *state) {
		if clearable(*current, lane, startedAt) {
			delete(current.Lanes, lane)
		}
	})
}

func clearable(current state, lane string, startedAt time.Time) bool {
	outage, present := current.Lanes[lane]
	return present && !outage.ObservedAt.After(startedAt)
}

// ClaimProbe reports whether the caller may send one probe invocation through
// a lane that is currently marked, and durably records the claim so concurrent
// runs and later runs do not all probe the same lane at once.
//
// It answers true only when the claim was written, so a lock or write failure
// keeps the lane skipped rather than turning every run into a probe. A mark
// with no ObservedAt - a legacy row, or one written by hand - starts its probe
// clock at the first claim instead of being probed immediately.
//
// Every invocation of a marked lane asks, and all but one per interval are
// refused, so a refusal is decided from a lock-free read and takes the
// exclusive lock only when it is going to write. Reading stale state can only
// understate how long ago the last probe was, so no probe is lost, and the
// decision is made again under the lock before anything is recorded.
func (s *Store) ClaimProbe(lane string) bool {
	if s == nil || s.path == "" || lane == "" {
		return false
	}
	now := s.now()
	if write, _ := probeDecision(s.load(), lane, now); !write {
		return false
	}
	claimed := false
	err := s.mutate(func(current *state) {
		write, claim := probeDecision(*current, lane, now)
		if !write {
			return
		}
		outage := current.Lanes[lane]
		outage.LastProbeAt = now
		current.Lanes[lane] = outage
		claimed = claim
	})
	if err != nil {
		return false
	}
	return claimed
}

// probeDecision reports whether lane's probe clock has to be written, and
// whether that write is a claim the caller may probe on. Starting the clock for
// a mark with no observation time is a write that is not a claim.
func probeDecision(current state, lane string, now time.Time) (write, claim bool) {
	outage, ok := current.Lanes[lane]
	if !ok || !outage.Until.After(now) {
		return false, false
	}
	since := lastProbeReference(outage)
	if since.IsZero() {
		return true, false
	}
	if now.Sub(since) < ProbeInterval {
		return false, false
	}
	return true, true
}

func lastProbeReference(outage Outage) time.Time {
	if outage.LastProbeAt.After(outage.ObservedAt) {
		return outage.LastProbeAt
	}
	return outage.ObservedAt
}

// Snapshot returns every live outage, ordered by lane name.
func (s *Store) Snapshot() []Outage {
	if s == nil || s.path == "" {
		return nil
	}
	now := s.now()
	current := s.load()
	live := make([]Outage, 0, len(current.Lanes))
	for _, outage := range current.Lanes {
		if outage.Until.After(now) {
			live = append(live, outage)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Lane < live[j].Lane })
	return live
}

func (s *Store) mutate(apply func(*state)) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create lane health directory: %w", err)
	}
	lock, err := filelock.Acquire(s.path + ".lock")
	if err != nil {
		return fmt.Errorf("lock lane health state: %w", err)
	}
	defer lock.Release()

	current := s.load()
	apply(&current)
	s.prune(&current)
	return s.save(current)
}

// load fails open: an unreadable or corrupt state file means "every lane
// healthy", which degrades to the pre-cooldown behavior instead of wedging
// every run behind a file it cannot parse.
func (s *Store) load() state {
	data, err := os.ReadFile(s.path)
	if err == nil {
		var parsed state
		if json.Unmarshal(data, &parsed) == nil && parsed.Lanes != nil {
			return parsed
		}
	}
	return state{Lanes: map[string]Outage{}}
}

func (s *Store) prune(current *state) {
	now := s.now()
	for lane, outage := range current.Lanes {
		if !outage.Until.After(now) {
			delete(current.Lanes, lane)
		}
	}
	if len(current.Lanes) <= maxLanes {
		return
	}
	lanes := make([]Outage, 0, len(current.Lanes))
	for _, outage := range current.Lanes {
		lanes = append(lanes, outage)
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Until.After(lanes[j].Until) })
	kept := make(map[string]Outage, maxLanes)
	for _, outage := range lanes[:maxLanes] {
		kept[outage.Lane] = outage
	}
	current.Lanes = kept
}

// save writes atomically via rename so a concurrent reader never observes a
// partial file.
func (s *Store) save(current state) error {
	data, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode lane health state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".lane-health-*")
	if err != nil {
		return fmt.Errorf("create lane health temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write lane health state: %w", firstErr(writeErr, closeErr))
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace lane health state: %w", err)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
