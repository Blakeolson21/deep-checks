package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
)

// attemptReportingAgent models a real adapter: it reports its own attempts and
// emits each one from below the lane wrapper, carrying the raw provider error
// exactly as runWithRetry does.
type attemptReportingAgent struct {
	name  string
	runs  []func() (*Result, error)
	calls int
}

func (a *attemptReportingAgent) Name() string { return a.name }

func (a *attemptReportingAgent) Close() error { return nil }

func (a *attemptReportingAgent) ReportsAgentAttempts() bool { return true }

func (a *attemptReportingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	var result *Result
	var err error
	for _, run := range a.runs {
		a.calls++
		startedAt := time.Now()
		result, err = run()
		emitAgentAttempt(opts, a.name, result, err, startedAt, time.Now())
		if err == nil {
			return result, nil
		}
	}
	return nil, err
}

// The failure this covers is the dominant incident shape: an invocation that
// actually ran and died on the provider's quota banner. The adapter reports
// that attempt with its raw stderr before the lane wrapper has classified
// anything, so a consumer reading the attempt stream sees a plain exit failure
// and the quota cost stays invisible.
func TestWithLaneHealthReportsTheTerminalAttemptAsTheOutageItClassified(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &attemptReportingAgent{name: "codex", runs: []func() (*Result, error){
		func() (*Result, error) { return nil, errors.New(codexQuotaStderr) },
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	var attempts []Attempt
	_, err := lane.Run(context.Background(), RunOpts{
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	})
	if err == nil {
		t.Fatalf("expected the quota failure to surface")
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if !IsQuotaOutage(attempts[0].Err) {
		t.Fatalf("reported attempt error %v must be the quota outage the caller received", attempts[0].Err)
	}
	if attempts[0].Agent != "codex" {
		t.Fatalf("attempt agent = %q, want codex", attempts[0].Agent)
	}
}

// Retries below the wrapper are separate rows and describe their own raw
// failures; only the terminal attempt carries the lane verdict.
func TestWithLaneHealthLeavesEarlierRetryAttemptsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	transient := errors.New("codex exited: exit status 1: overloaded_error")
	inner := &attemptReportingAgent{name: "codex", runs: []func() (*Result, error){
		func() (*Result, error) { return nil, transient },
		func() (*Result, error) { return nil, errors.New(codexQuotaStderr) },
	}}
	lane := WithLaneHealth(inner, store, func() time.Time { return now })

	var attempts []Attempt
	if _, err := lane.Run(context.Background(), RunOpts{
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	}); err == nil {
		t.Fatalf("expected the quota failure to surface")
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if !errors.Is(attempts[0].Err, transient) {
		t.Fatalf("first attempt error = %v, want the untouched transient failure", attempts[0].Err)
	}
	if IsQuotaOutage(attempts[0].Err) {
		t.Fatalf("a retried transient failure must not be reported as a quota outage")
	}
	if !IsQuotaOutage(attempts[1].Err) {
		t.Fatalf("terminal attempt error %v must be the quota outage", attempts[1].Err)
	}
}

// A non-quota failure keeps the adapter's own error, and a success is reported
// exactly once with its result intact.
func TestWithLaneHealthPassesThroughAttemptsItDoesNotClassify(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	failure := errors.New("codex exited: exit status 1: stream disconnected before completion")
	cases := []struct {
		name    string
		run     func() (*Result, error)
		wantErr error
	}{
		{
			name:    "non-quota failure",
			run:     func() (*Result, error) { return nil, failure },
			wantErr: failure,
		},
		{
			name: "success",
			run:  func() (*Result, error) { return &Result{Text: "ok"}, nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := laneTestStore(t, &now)
			inner := &attemptReportingAgent{name: "codex", runs: []func() (*Result, error){tc.run}}
			lane := WithLaneHealth(inner, store, func() time.Time { return now })

			var attempts []Attempt
			_, err := lane.Run(context.Background(), RunOpts{
				OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
			})
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run error = %v, want %v", err, tc.wantErr)
			}
			if len(attempts) != 1 {
				t.Fatalf("attempts = %d, want 1", len(attempts))
			}
			if tc.wantErr == nil {
				if attempts[0].Err != nil {
					t.Fatalf("successful attempt error = %v, want nil", attempts[0].Err)
				}
				if attempts[0].Result == nil || attempts[0].Result.Text != "ok" {
					t.Fatalf("successful attempt must keep its result, got %+v", attempts[0].Result)
				}
				return
			}
			if !errors.Is(attempts[0].Err, tc.wantErr) {
				t.Fatalf("attempt error = %v, want the adapter's own %v", attempts[0].Err, tc.wantErr)
			}
		})
	}
}

// A lane skipped while marked never reaches the adapter that would report it,
// so the wrapper reports the attempt itself. Without it the skip is invisible
// whenever a fallback set moves on to another lane, which is exactly when the
// invocation still costs the operator a lane.
func TestWithLaneHealthReportsASkippedLaneAsAnAttempt(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	if err := store.Mark(lanehealth.Outage{
		Lane:       "codex",
		Until:      now.Add(3 * time.Hour),
		ObservedAt: now,
		Reason:     "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	inner := &attemptReportingAgent{name: "codex", runs: []func() (*Result, error){
		func() (*Result, error) { return &Result{Text: "must not run"}, nil },
	}}
	healthy := &attemptReportingAgent{name: "claude", runs: []func() (*Result, error){
		func() (*Result, error) { return &Result{Text: "ok"}, nil },
	}}
	lanes := NewFallback([]Agent{
		WithLaneHealth(inner, store, func() time.Time { return now }),
		healthy,
	})

	var attempts []Attempt
	result, err := lanes.Run(context.Background(), RunOpts{
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("result = %+v, want the healthy lane's ok", result)
	}
	if inner.calls != 0 {
		t.Fatalf("marked lane was invoked %d times, want 0", inner.calls)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want the skipped lane and the healthy one", len(attempts))
	}
	if attempts[0].Agent != "codex" || !IsQuotaOutage(attempts[0].Err) {
		t.Fatalf("first attempt = %q/%v, want the skipped codex lane's quota outage", attempts[0].Agent, attempts[0].Err)
	}
	if attempts[1].Agent != "claude" || attempts[1].Err != nil {
		t.Fatalf("second attempt = %q/%v, want the healthy claude lane", attempts[1].Agent, attempts[1].Err)
	}
}

// A lane that does not report its own attempts is reported by the fallback
// wrapper from this wrapper's return value, which already carries the verdict.
// The lane wrapper must not add a second attempt for the same invocation.
func TestWithLaneHealthDoesNotDoubleReportALaneThatReportsNoAttempts(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := laneTestStore(t, &now)
	inner := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		return nil, errors.New(codexQuotaStderr)
	}}
	healthy := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	lanes := NewFallback([]Agent{
		WithLaneHealth(inner, store, func() time.Time { return now }),
		WithLaneHealth(healthy, store, func() time.Time { return now }),
	})

	var attempts []Attempt
	if _, err := lanes.Run(context.Background(), RunOpts{
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want one per lane", len(attempts))
	}
	if !IsQuotaOutage(attempts[0].Err) {
		t.Fatalf("first attempt error %v must be the quota outage", attempts[0].Err)
	}
}
