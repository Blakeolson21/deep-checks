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

// Clear drops any mark for lane. A lane that just completed an invocation is
// demonstrably healthy, so its mark - including one written from a misread
// banner - must not outlive that evidence.
func (s *Store) Clear(lane string) error {
	if s == nil || s.path == "" || lane == "" {
		return nil
	}
	if _, present := s.load().Lanes[lane]; !present {
		return nil
	}
	return s.mutate(func(current *state) {
		delete(current.Lanes, lane)
	})
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
