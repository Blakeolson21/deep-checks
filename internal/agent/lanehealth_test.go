package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
)

const codexQuotaStderr = "codex exited: exit status 1: You've hit your usage limit. " +
	"Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 7th, 2026 11:06 PM."

func laneTestStore(t *testing.T, now *time.Time) *lanehealth.Store {
	t.Helper()
	return lanehealth.NewStore(
		filepath.Join(t.TempDir(), "lane-health.json"),
		func() time.Time { return *now },
	)
}

func TestWithLaneHealthMarksTheLaneOnAQuotaBanner(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	_, err := lane.Run(context.Background(), RunOpts{Prompt: "x"})
	if err == nil {
		t.Fatalf("expected the quota failure to surface")
	}
	var outageErr *LaneOutageError
	if !errors.As(err, &outageErr) {
		t.Fatalf("error %v must be a *LaneOutageError", err)
	}
	if outageErr.Lane != "codex" {
		t.Fatalf("Lane = %q, want codex", outageErr.Lane)
	}
	if !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("error %q must keep the provider banner", err)
	}
	if _, ok := store.Outage("codex"); !ok {
		t.Fatalf("the quota banner must be persisted as a lane outage")
	}
}

func TestWithLaneHealthSkipsAMarkedLaneWithoutInvokingIt(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	if err := store.Mark(lanehealth.Outage{
		Lane:   "codex",
		Until:  now.Add(3 * time.Hour),
		Reason: "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	_, err := lane.Run(context.Background(), RunOpts{Prompt: "x"})
	if err == nil {
		t.Fatalf("a marked lane must fail fast instead of running")
	}
	if inner.calls != 0 {
		t.Fatalf("marked lane was invoked %d times, want 0", inner.calls)
	}
	var outageErr *LaneOutageError
	if !errors.As(err, &outageErr) {
		t.Fatalf("error %v must be a *LaneOutageError", err)
	}
	if !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("skip error %q must name the recorded reason", err)
	}
}

func TestWithLaneHealthRunsAgainOnceTheMarkExpires(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	until := now.Add(3 * time.Hour)
	if err := store.Mark(lanehealth.Outage{Lane: "codex", Until: until, Reason: "usage limit"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	if _, err := lane.Run(context.Background(), RunOpts{}); err == nil {
		t.Fatalf("lane must be skipped while the mark is live")
	}
	now = until
	res, err := lane.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("lane must run again at the reset time: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("Text = %q, want ok", res.Text)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
}

// The provider banner's own remedy ("purchase more credits") restores the same
// account long before the reset it stated, but the mark suppresses the only
// evidence that could undo it. One probe per interval bounds how long a stale
// multi-day mark can keep a recovered lane unused.
func TestWithLaneHealthProbesALongMarkAndClearsItWhenTheLaneRecovered(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	if err := store.Mark(lanehealth.Outage{
		Lane:       "codex",
		Until:      now.Add(4 * 24 * time.Hour),
		ObservedAt: now,
		Reason:     "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	if _, err := lane.Run(context.Background(), RunOpts{}); err == nil {
		t.Fatalf("the mark must hold for its first interval")
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0 inside the first interval", inner.calls)
	}

	now = now.Add(lanehealth.ProbeInterval)
	res, err := lane.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("the probe must reach the lane: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("Text = %q, want ok", res.Text)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want exactly 1 probe", inner.calls)
	}
	if outage, live := store.Outage("codex"); live {
		t.Fatalf("a successful probe must clear the mark, still marked until %s", outage.Until)
	}
}

// A probe that hits the banner again re-parks the lane for a fresh interval
// instead of letting every later invocation through.
func TestWithLaneHealthReparksWhenTheProbeHitsTheBannerAgain(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	if err := store.Mark(lanehealth.Outage{
		Lane:       "codex",
		Until:      now.Add(4 * 24 * time.Hour),
		ObservedAt: now,
		Reason:     "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	now = now.Add(lanehealth.ProbeInterval)
	if _, err := lane.Run(context.Background(), RunOpts{}); err == nil {
		t.Fatalf("expected the probe to surface the banner again")
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
	for i := 0; i < 3; i++ {
		if _, err := lane.Run(context.Background(), RunOpts{}); err == nil {
			t.Fatalf("the re-marked lane must stay skipped")
		}
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want the lane skipped again after a failed probe", inner.calls)
	}
}

// A lane that succeeds is demonstrably healthy, so any stale mark - including
// one written from a misread banner - is dropped immediately.
func TestWithLaneHealthClearsTheMarkOnSuccess(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	until := now.Add(3 * time.Hour)
	if err := store.Mark(lanehealth.Outage{Lane: "codex", Until: until, Reason: "usage limit"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	now = until
	if _, err := lane.Run(context.Background(), RunOpts{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	now = until.Add(-time.Hour)
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("a successful invocation must clear the lane mark")
	}
}

func TestWithLaneHealthLeavesNonQuotaFailuresUnmarked(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New("codex exited: exit status 1: stream disconnected before completion")
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	_, err := lane.Run(context.Background(), RunOpts{})
	if err == nil {
		t.Fatalf("expected the failure to surface")
	}
	var outageErr *LaneOutageError
	if errors.As(err, &outageErr) {
		t.Fatalf("a non-quota failure must not become a lane outage")
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("a non-quota failure must not mark the lane")
	}
}

// A cancelled run is not evidence about quota, and its partial output can
// carry a banner the provider had only warned about.
func TestWithLaneHealthDoesNotMarkOnACancelledRun(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := lane.Run(ctx, RunOpts{})
	if err == nil {
		t.Fatalf("expected the failure to surface")
	}
	var outageErr *LaneOutageError
	if errors.As(err, &outageErr) {
		t.Fatalf("a cancelled run must not be reported as a lane outage")
	}
	if _, ok := store.Outage("codex"); ok {
		t.Fatalf("a cancelled run must not mark the lane")
	}
}

// A schema-parse failure is the one adapter error whose text quotes the agent's
// OWN final message, and reviewing a repository that merely mentions a quota
// banner - this one does - would otherwise park a healthy lane for days on the
// dated reset the quoted text carries.
func TestWithLaneHealthIgnoresAQuotaBannerQuotedByAgentOutput(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return finalizeTextResult(
			"codex",
			"I reviewed the diff. It pins the provider string \"You've hit your usage limit. "+
				"Visit https://chatgpt.com/codex/settings/usage to purchase more credits or "+
				"try again at Aug 7th, 2026 11:06 PM.\" as a test fixture.",
			json.RawMessage(`{"type":"object"}`),
			TokenUsage{},
		)
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	_, err := lane.Run(context.Background(), RunOpts{})
	if err == nil {
		t.Fatalf("expected the parse failure to surface")
	}
	var parseErr *OutputParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %v must stay a *OutputParseError", err)
	}
	var outageErr *LaneOutageError
	if errors.As(err, &outageErr) {
		t.Fatalf("agent-authored text must not become a lane outage: %v", err)
	}
	if outage, ok := store.Outage("codex"); ok {
		t.Fatalf("agent-authored text must not mark the lane (marked until %s)", outage.Until)
	}
}

func TestWithLaneHealthForwardsCapabilities(t *testing.T) {
	now := time.Now()
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", resumable: true}
	lane := WithLaneHealth(inner, store, nil)
	if !SupportsSessionResume(lane) {
		t.Fatalf("lane-health wrapper must forward session-resume support")
	}
	if WithLaneHealth(nil, store, nil) != nil {
		t.Fatalf("wrapping nil must stay nil")
	}
	bare := WithLaneHealth(inner, nil, nil)
	if bare != Agent(inner) {
		t.Fatalf("wrapping without a store must return the agent unchanged")
	}
}

func TestLaneOutageErrorNamesTheResetTime(t *testing.T) {
	until := time.Date(2026, 8, 7, 23, 6, 0, 0, time.Local)
	err := &LaneOutageError{Lane: "codex", Until: until, Reason: "You've hit your usage limit"}
	msg := err.Error()
	if !strings.Contains(msg, "codex") {
		t.Fatalf("message %q must name the lane", msg)
	}
	if !strings.Contains(msg, until.Format("2006-01-02 15:04 MST")) {
		t.Fatalf("message %q must name the reset time", msg)
	}
}
