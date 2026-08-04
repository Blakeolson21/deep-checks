package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
)

// Reproduces the 2026-08-04 03:44-03:58 UTC incident: roughly a dozen gate runs
// (01KZ5DX4Y4R9Z0AQN3B53STP5Y, 01KZ5DHW3R6H8KWFCN29KCBA61, 01KZ5BV1Z2W2DVV7018PZM7CC0,
// 01KZ5DS8CEXH48J5VG6G6DRC0D, ...) each fell back onto a Codex lane whose quota was
// exhausted until Aug 7, and each burned a full agent spawn rediscovering it.
// Once one run has marked the lane, later runs must skip it and use the next lane.
func TestFallbackSkipsAQuotaMarkedLaneAndUsesTheNextOne(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	clock := func() time.Time { return now }

	codex := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	claude := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	chain := NewFallback([]Agent{
		WithLaneHealth(codex, store, clock),
		WithLaneHealth(claude, store, clock),
	})

	// First run discovers the dead lane the expensive way.
	if _, err := chain.Run(context.Background(), RunOpts{}); err != nil {
		t.Fatalf("first run must fail over to claude: %v", err)
	}
	if codex.calls != 1 || claude.calls != 1 {
		t.Fatalf("first run calls: codex=%d claude=%d, want 1/1", codex.calls, claude.calls)
	}

	// Every later run must consume the persisted mark instead of respawning codex.
	for i := 0; i < 5; i++ {
		if _, err := chain.Run(context.Background(), RunOpts{}); err != nil {
			t.Fatalf("run %d must succeed on claude: %v", i+2, err)
		}
	}
	if codex.calls != 1 {
		t.Fatalf("codex was invoked %d times, want 1 (the marked lane must be skipped)", codex.calls)
	}
	if claude.calls != 6 {
		t.Fatalf("claude calls = %d, want 6", claude.calls)
	}
}

func TestFallbackFailsWithEveryLaneResetTimeWhenAllLanesAreExhausted(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	clock := func() time.Time { return now }

	claude := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
		return nil, errors.New("claude exited: exit status 1: You've hit your session limit · resets 9:15 PM")
	}}
	codex := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	chain := NewFallback([]Agent{
		WithLaneHealth(claude, store, clock),
		WithLaneHealth(codex, store, clock),
	})

	_, err := chain.Run(context.Background(), RunOpts{})
	if err == nil {
		t.Fatalf("expected the run to fail when every lane is exhausted")
	}
	msg := err.Error()
	for _, want := range []string{
		"claude",
		"codex",
		time.Date(2026, 8, 4, 21, 15, 0, 0, time.Local).Format("2006-01-02 15:04 MST"),
		time.Date(2026, 8, 7, 23, 6, 0, 0, time.Local).Format("2006-01-02 15:04 MST"),
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("all-lanes-exhausted error %q must contain %q", msg, want)
		}
	}
	var outageErr *LaneOutageError
	if errors.As(err, &outageErr) {
		t.Fatalf("the aggregate must not masquerade as a single-lane outage: %v", err)
	}
}

// A last lane that fails for an ordinary reason must still surface that reason,
// not be recast as a quota problem just because an earlier lane was exhausted.
func TestFallbackKeepsANonQuotaFinalFailure(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	clock := func() time.Time { return now }

	codex := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	claude := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
		return nil, errors.New("claude exited: exit status 2: parse failure")
	}}
	chain := NewFallback([]Agent{
		WithLaneHealth(codex, store, clock),
		WithLaneHealth(claude, store, clock),
	})

	_, err := chain.Run(context.Background(), RunOpts{})
	if err == nil {
		t.Fatalf("expected a failure")
	}
	if !strings.Contains(err.Error(), "parse failure") {
		t.Fatalf("error %q must keep the last lane's real failure", err)
	}
}

// Marks recorded by an earlier process are honored on the very first invocation
// of a fresh chain, so a daemon restart does not restart the rediscovery cost.
func TestFallbackHonorsMarksWrittenByAnEarlierProcess(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	clock := func() time.Time { return now }
	if err := store.Mark(lanehealth.Outage{
		Lane:   "codex",
		Until:  now.Add(3 * 24 * time.Hour),
		Reason: "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	codex := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return &Result{Text: "should not run"}, nil
	}}
	claude := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	chain := NewFallback([]Agent{
		WithLaneHealth(codex, store, clock),
		WithLaneHealth(claude, store, clock),
	})

	res, err := chain.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("Text = %q, want ok", res.Text)
	}
	if codex.calls != 0 {
		t.Fatalf("codex calls = %d, want 0", codex.calls)
	}
}
