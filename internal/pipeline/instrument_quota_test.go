package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
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

// TestPerfRecording_QuotaOutageRecordsQuotaCategory proves an invocation that
// died on provider quota exhaustion is recorded under the dedicated "quota"
// failure category, for both lane-outage shapes. Before this category existed,
// the skip case landed in "other" and a marked lane whose recorded reason
// excerpt embedded "codex exited: ..." landed in "exit", so quota cost was
// invisible in the stats (2026-08-04 incident).
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

// bannerReportingAgent models a real adapter on the dominant incident shape: an
// invocation that launched, hit the provider's quota banner, and reported that
// attempt with its raw stderr - which is what every shipped adapter does, from
// below the lane-health wrapper that later classifies the outage.
type bannerReportingAgent struct {
	name string
	err  error
}

func (a *bannerReportingAgent) Name() string { return a.name }

func (a *bannerReportingAgent) Close() error { return nil }

func (a *bannerReportingAgent) ReportsAgentAttempts() bool { return true }

func (a *bannerReportingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.OnAttempt != nil {
		opts.OnAttempt(agent.Attempt{
			Agent:       a.name,
			Err:         a.err,
			StartedAt:   time.Now(),
			CompletedAt: time.Now(),
		})
	}
	return nil, a.err
}

// TestPerfRecording_LiveQuotaBannerRecordsQuotaCategory covers the invocation
// that actually burned time hitting the wall, which is the one the incident was
// about. The adapter reports its attempt with the raw banner text before any
// outage verdict exists, so the recorded row landed in "exit" and undercounted
// exactly the failures the category was added to measure.
func TestPerfRecording_LiveQuotaBannerRecordsQuotaCategory(t *testing.T) {
	database, _, run, _ := setupTest(t)

	const banner = "codex exited: exit status 1: You've hit your usage limit. " +
		"Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 7th, 2026 11:06 PM."
	now := time.Date(2026, 8, 4, 3, 44, 0, 0, time.Local)
	store := lanehealth.NewStore(
		filepath.Join(t.TempDir(), "lane-health.json"),
		func() time.Time { return now },
	)
	lane := agent.WithLaneHealth(
		&bannerReportingAgent{name: "codex", err: errors.New(banner)},
		store,
		func() time.Time { return now },
	)
	wrapped := &perfRecordingAgent{
		inner:    lane,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
	}

	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); err == nil {
		t.Fatal("expected the quota failure to surface")
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
}
