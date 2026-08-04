package lanehealth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lane-health.json")
	return NewStore(path, func() time.Time { return *now })
}

func TestStoreMarkIsVisibleToASeparateStoreOnTheSameFile(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	path := filepath.Join(t.TempDir(), "lane-health.json")
	clock := func() time.Time { return now }

	writer := NewStore(path, clock)
	if err := writer.Mark(Outage{Lane: "codex", Until: now.Add(3 * time.Hour), Reason: "usage limit"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// A different Store over the same file models the next run - and, after a
	// daemon restart, the next process - reading the mark instead of
	// rediscovering the dead lane itself.
	reader := NewStore(path, clock)
	outage, ok := reader.Outage("codex")
	if !ok {
		t.Fatalf("expected the persisted codex outage to be visible")
	}
	if !outage.Until.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("Until = %s, want %s", outage.Until, now.Add(3*time.Hour))
	}
	if outage.Reason != "usage limit" {
		t.Fatalf("Reason = %q, want %q", outage.Reason, "usage limit")
	}
}

func TestStoreOutageExpiresAtResetTime(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	until := now.Add(time.Hour)
	if err := store.Mark(Outage{Lane: "codex", Until: until, Reason: "usage limit"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("outage must be live before its reset time")
	}
	now = until.Add(-time.Second)
	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("outage must still be live one second before its reset time")
	}
	now = until
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("outage must expire at its reset time")
	}
	now = until.Add(time.Hour)
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("outage must stay expired after its reset time")
	}
}

func TestStoreClearRemovesAMark(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := store.Clear("codex"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("cleared lane must not report an outage")
	}
}

func TestStoreKeepsLanesIndependent(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Mark codex: %v", err)
	}
	if err := store.Mark(Outage{Lane: "claude", Until: now.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("Mark claude: %v", err)
	}
	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("codex outage lost after marking claude")
	}
	if _, ok := store.Outage("claude"); !ok {
		t.Fatalf("claude outage missing")
	}
}

// A corrupt or truncated state file must fail open (every lane healthy) rather
// than wedge every run, and it must not stop a fresh mark from being recorded.
func TestStoreFailsOpenOnCorruptState(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	path := filepath.Join(t.TempDir(), "lane-health.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}
	store := NewStore(path, func() time.Time { return now })
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("corrupt state must report no outage")
	}
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Mark over corrupt state: %v", err)
	}
	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("mark written over corrupt state must be readable")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var store *Store
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("nil store must report no outage")
	}
	if err := store.Mark(Outage{Lane: "codex"}); err != nil {
		t.Fatalf("nil store Mark: %v", err)
	}
	if err := store.Clear("codex"); err != nil {
		t.Fatalf("nil store Clear: %v", err)
	}
}

func TestStorePrunesExpiredEntriesOnWrite(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "stale", Until: now.Add(time.Minute)}); err != nil {
		t.Fatalf("Mark stale: %v", err)
	}
	now = now.Add(time.Hour)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Mark codex: %v", err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got := string(data); strings.Contains(got, `"stale"`) {
		t.Fatalf("expired lane must be pruned from the state file, got %s", got)
	}
}
