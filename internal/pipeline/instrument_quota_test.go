package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// quotaOutageAgent fails every invocation with the lane-outage error the lane
// health wrapper produces, in both of its shapes: the skip (lane already
// marked) and the fresh banner classification.
type quotaOutageAgent struct {
	err error
}

func (a *quotaOutageAgent) Name() string { return "codex" }
func (a *quotaOutageAgent) Close() error { return nil }
func (a *quotaOutageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, a.err
}

// TestPerfRecording_QuotaOutageRecordsQuotaCategory proves an invocation that
// died on provider quota exhaustion is recorded under the dedicated "quota"
// failure category, for both lane-outage shapes. Before this category existed,
// the skip case landed in "other" and a marked lane whose recorded reason
// excerpt embedded "codex exited: ..." landed in "exit", so quota cost was
// invisible in the stats (2026-08-04 incident).
// quotaResumeFailingAgent models a lane whose durable session hits the quota
// wall: resuming fails with a lane outage, a fresh session works (the probe
// found early recovery).
type quotaResumeFailingAgent struct{}

func (quotaResumeFailingAgent) Name() string                { return "codex" }
func (quotaResumeFailingAgent) Close() error                { return nil }
func (quotaResumeFailingAgent) SupportsSessionResume() bool { return true }
func (quotaResumeFailingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.Session != nil && opts.Session.ID != "" {
		return nil, &agent.LaneOutageError{
			Lane:   "codex",
			Until:  time.Date(2026, 8, 7, 23, 6, 0, 0, time.Local),
			Reason: "codex exited: exit status 1: You've hit your usage limit",
		}
	}
	return &agent.Result{SessionID: "sess-q"}, nil
}

// TestPerfRecording_QuotaResumeFailureRecordsQuotaFallbackReason proves a
// resume that died on the lane's quota records the fallback row under the
// dedicated quota reason, not "exit" (the banner excerpt embeds "codex
// exited: ...").
func TestPerfRecording_QuotaResumeFailureRecordsQuotaFallbackReason(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	wrapped := &perfRecordingAgent{
		inner:    quotaResumeFailingAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)

	for r := 1; r <= 2; r++ {
		roundNum = r
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}, nil); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fallback *db.AgentInvocation
	for i := range invs {
		if invs[i].SessionMode == db.InvocationModeFallback {
			fallback = &invs[i]
		}
	}
	if fallback == nil {
		t.Fatal("expected a fallback invocation row")
	}
	got := ""
	if fallback.FallbackReason != nil {
		got = *fallback.FallbackReason
	}
	if got != db.FallbackReasonQuota {
		t.Fatalf("fallback reason = %q, want %q", got, db.FallbackReasonQuota)
	}
}

func TestPerfRecording_QuotaOutageRecordsQuotaCategory(t *testing.T) {
	until := time.Date(2026, 8, 7, 23, 6, 0, 0, time.Local)
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "skipped because the lane is marked",
			err:  &agent.LaneOutageError{Lane: "codex", Until: until},
		},
		{
			name: "marked reason excerpt embeds the provider exit banner",
			err: &agent.LaneOutageError{
				Lane: "codex", Until: until,
				Reason: "codex exited: exit status 1: You've hit your usage limit",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, _, run, _ := setupTest(t)

			wrapped := &perfRecordingAgent{
				inner:    &quotaOutageAgent{err: tc.err},
				db:       database,
				runID:    run.ID,
				stepName: types.StepReview,
				round:    func() int { return 1 },
			}
			if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); !errors.Is(err, tc.err) {
				t.Fatalf("run error = %v, want the lane outage", err)
			}

			invs, err := database.GetAgentInvocationsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 1 {
				t.Fatalf("got %d rows, want 1", len(invs))
			}
			if invs[0].ExitStatus != "error" {
				t.Fatalf("exit status = %q, want error", invs[0].ExitStatus)
			}
			if invs[0].FailureCategory != "quota" {
				t.Fatalf("failure category = %q, want quota", invs[0].FailureCategory)
			}
		})
	}
}
