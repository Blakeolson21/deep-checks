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

func TestStoreClearRemovesAMarkObservedBeforeTheInvocation(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour), ObservedAt: now}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := store.ClearObservedBefore("codex", now.Add(time.Minute)); err != nil {
		t.Fatalf("ClearObservedBefore: %v", err)
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("cleared lane must not report an outage")
	}
}

// A mark with no observation time - a legacy row, or one written by hand -
// carries no evidence about when it was seen, so a success still clears it.
func TestStoreClearRemovesAMarkWithNoObservationTime(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := store.ClearObservedBefore("codex", now); err != nil {
		t.Fatalf("ClearObservedBefore: %v", err)
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("a mark with no observation time must be cleared by a success")
	}
}

// The cooldown has to be sticky across overlapping runs: an invocation
// authorized before the account ran out still completes, and its success is not
// evidence about a banner another run hit while it was streaming.
func TestStoreClearKeepsAMarkObservedAfterTheInvocationStarted(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	startedAt := now
	observedAt := now.Add(5 * time.Second)
	if err := store.Mark(Outage{
		Lane:       "codex",
		Until:      now.Add(3 * time.Hour),
		ObservedAt: observedAt,
		Reason:     "usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	now = observedAt.Add(time.Minute)
	if err := store.ClearObservedBefore("codex", startedAt); err != nil {
		t.Fatalf("ClearObservedBefore: %v", err)
	}
	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("a mark observed after the invocation started must survive it")
	}
	if err := store.ClearObservedBefore("codex", observedAt); err != nil {
		t.Fatalf("ClearObservedBefore at the observation: %v", err)
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("an invocation that started no earlier than the mark must clear it")
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
	if err := store.ClearObservedBefore("codex", time.Now()); err != nil {
		t.Fatalf("nil store ClearObservedBefore: %v", err)
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

// A mark can only be undone by the reset it recorded or by a successful
// invocation, and the mark itself suppresses the invocation - so a reset stated
// days out would never self-correct after the operator restores quota on the
// same account, which is what the banner's own "purchase more credits" remedy
// does. One probe per interval bounds that.
func TestStoreClaimsOneProbePerIntervalThroughALongMark(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{
		Lane:       "codex",
		Until:      now.Add(4 * 24 * time.Hour),
		ObservedAt: now,
		Reason:     "usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	if store.ClaimProbe("codex") {
		t.Fatalf("the mark must be trusted for its first interval")
	}
	now = now.Add(ProbeInterval - time.Minute)
	if store.ClaimProbe("codex") {
		t.Fatalf("a probe must not be claimed before the interval elapses")
	}
	now = now.Add(time.Minute)
	if !store.ClaimProbe("codex") {
		t.Fatalf("a probe must be claimed once the interval has elapsed")
	}
	// The claim is durable, so concurrent runs and later runs do not all probe.
	if store.ClaimProbe("codex") {
		t.Fatalf("a second probe must not be claimed inside the same interval")
	}
	if NewStore(store.path, func() time.Time { return now }).ClaimProbe("codex") {
		t.Fatalf("another process must observe the claim already spent")
	}
	if _, live := store.Outage("codex"); !live {
		t.Fatalf("claiming a probe must not clear the mark")
	}

	now = now.Add(ProbeInterval)
	if !store.ClaimProbe("codex") {
		t.Fatalf("the next interval must allow another probe")
	}
}

// Re-marking a lane restarts its probe clock: the new banner is fresh evidence.
func TestStoreRemarkRestartsTheProbeClock(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(72 * time.Hour), ObservedAt: now}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	now = now.Add(ProbeInterval)
	if !store.ClaimProbe("codex") {
		t.Fatalf("expected the first probe to be claimed")
	}
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(72 * time.Hour), ObservedAt: now}); err != nil {
		t.Fatalf("re-Mark: %v", err)
	}
	now = now.Add(ProbeInterval - time.Minute)
	if store.ClaimProbe("codex") {
		t.Fatalf("a fresh mark must be trusted for a full interval again")
	}
}

// A row with no ObservedAt - written by an older build, or by hand - must start
// its probe clock rather than be probed immediately.
func TestStoreStartsTheProbeClockForAMarkWithNoObservationTime(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(72 * time.Hour)}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if store.ClaimProbe("codex") {
		t.Fatalf("a mark with no observation time must not be probed on sight")
	}
	now = now.Add(ProbeInterval)
	if !store.ClaimProbe("codex") {
		t.Fatalf("the probe must become due one interval after the clock started")
	}
}

// Every invocation of a marked lane asks for a probe, and all but one per
// interval are refused. A refusal that still rewrites the state file makes the
// common case pay a blocking lock plus an atomic replace for no state change,
// and every replace is another window for a lock-free reader to race a rename.
func TestStoreClaimProbeDoesNotRewriteStateWhenNoProbeIsDue(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if err := store.Mark(Outage{
		Lane:       "codex",
		Until:      now.Add(4 * 24 * time.Hour),
		ObservedAt: now,
		Reason:     "usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	// The state file is replaced by rename, so a write gives the path a new
	// identity; an unchanged identity means nothing was written.
	before, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}

	now = now.Add(ProbeInterval - time.Minute)
	for i := 0; i < 5; i++ {
		if store.ClaimProbe("codex") {
			t.Fatalf("a probe must not be claimed before the interval elapses")
		}
	}
	if store.ClaimProbe("unmarked") {
		t.Fatalf("an unmarked lane needs no probe")
	}
	after, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat state after refusals: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("a refused probe must not rewrite the state file")
	}

	now = now.Add(time.Minute)
	if !store.ClaimProbe("codex") {
		t.Fatalf("a probe must still be claimed once the interval has elapsed")
	}
	claimed, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat state after claim: %v", err)
	}
	if os.SameFile(after, claimed) {
		t.Fatalf("a claimed probe must be persisted")
	}
}

func TestStoreClaimProbeRefusesUnmarkedAndExpiredLanes(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	store := testStore(t, &now)
	if store.ClaimProbe("codex") {
		t.Fatalf("an unmarked lane needs no probe")
	}
	if err := store.Mark(Outage{Lane: "codex", Until: now.Add(time.Minute), ObservedAt: now}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if store.ClaimProbe("codex") {
		t.Fatalf("an expired mark is not a live outage to probe")
	}
	var nilStore *Store
	if nilStore.ClaimProbe("codex") {
		t.Fatalf("a nil store must never claim a probe")
	}
}
