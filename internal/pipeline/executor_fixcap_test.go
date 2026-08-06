package pipeline

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These tests pin the per-run fix-round cap. The measured defect: three runs on
// 2026-08-05 configured auto_fix.review: 2, spent that budget on rounds 2 and 3,
// and then laddered to 9-12 rounds because every later round was funded by an
// agent `respond --action fix`, which the auto-fix budget never bound.

const (
	autoFixableFinding = `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`
	askUserFinding     = `{"findings":[{"id":"review-1","severity":"error","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`
)

// laddering models a step that never converges: every round returns a blocking
// finding, so something must stop funding rounds or the run runs forever. The
// round counter is atomic because the executor runs the step on its own
// goroutine while the test reads the count.
func laddering(name types.StepName, findings string, calls *atomic.Int64) *adaptiveCallStep {
	return &adaptiveCallStep{
		name: name,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls.Add(1)
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      findings,
			}, nil
		},
	}
}

// respondWhenParked retries a response until the executor has published its
// gate, so a test never races the park. It retries only the not-yet-parked
// error; every other error (including a cap refusal) is returned as-is.
func respondWhenParked(t *testing.T, exec *Executor, step types.StepName, action types.ApprovalAction, opts RespondOptions) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.RespondWithOptions(step, action, opts)
		if err == nil || !strings.Contains(err.Error(), "no step awaiting approval") {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatalf("step %s never parked for a response", step)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForCalls(t *testing.T, calls *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step executed %d rounds, want %d", calls.Load(), want)
}

func reviewRounds(t *testing.T, database *db.DB, runID string) []*db.StepRound {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, s := range steps {
		if s.StepName == types.StepReview {
			rounds, err := database.GetRoundsByStep(s.ID)
			if err != nil {
				t.Fatalf("get rounds: %v", err)
			}
			return rounds
		}
	}
	t.Fatal("no review step recorded")
	return nil
}

// A laddering run must halt at the configured cap no matter who funds the
// rounds. This is the regression for the measured defect.
func TestExecutor_ConfiguredAutoFixLimitCapsUserFundedFixRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}}

	var calls atomic.Int64
	exec := NewExecutor(database, p, cfg, nil, []Step{laddering(types.StepReview, autoFixableFinding, &calls)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	// 1 initial + 2 auto-fix rounds spends the whole budget.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	waitForCalls(t, &calls, 3)

	err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{FindingIDs: []string{"review-1"}})
	if err == nil {
		t.Fatal("expected the fix past the cap to be refused")
	}
	if !strings.Contains(err.Error(), "auto_fix.review cap 2 reached") {
		t.Fatalf("refusal does not state the cap: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("refused fix still ran a round: %d rounds", got)
	}

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionApprove, RespondOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

// The cap counts every funded round, not just the automatic ones: a run whose
// findings are all ask-user never spends the budget automatically, and used to
// be re-fundable forever by an agent.
func TestExecutor_FixCapCountsUserFundedRoundsWhenAutoFixNeverRuns(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}}

	var calls atomic.Int64
	exec := NewExecutor(database, p, cfg, nil, []Step{laddering(types.StepReview, askUserFinding, &calls)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForCalls(t, &calls, 1)
	for round := 2; round <= 3; round++ {
		if err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{FindingIDs: []string{"review-1"}}); err != nil {
			t.Fatalf("fix round %d: %v", round, err)
		}
		waitForCalls(t, &calls, int64(round))
	}

	err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{FindingIDs: []string{"review-1"}})
	if err == nil || !strings.Contains(err.Error(), "auto_fix.review cap 2 reached") {
		t.Fatalf("third user fix was not capped: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("step ran %d rounds, want 3", got)
	}

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionApprove, RespondOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	<-done
}

// The park has to say why, or the driving agent has no way to tell an exhausted
// budget from an ordinary gate.
func TestExecutor_FixCapParkRecordsStatedReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}}

	var calls atomic.Int64
	exec := NewExecutor(database, p, cfg, nil, []Step{laddering(types.StepReview, autoFixableFinding, &calls)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	waitForCalls(t, &calls, 3)

	want := "auto_fix.review cap 2 reached; 1 finding open"
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) == 1 && steps[0].LastActivity != nil {
			got = *steps[0].LastActivity
			if strings.Contains(got, want) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(got, want) {
		t.Fatalf("step activity = %q, want it to contain %q", got, want)
	}

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionApprove, RespondOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	<-done
}

// An explicit override re-funds exactly one round, and the round records that
// it was funded past the cap.
func TestExecutor_FixCapOverrideFundsOneRoundAndIsRecorded(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}

	var calls atomic.Int64
	exec := NewExecutor(database, p, cfg, nil, []Step{laddering(types.StepReview, autoFixableFinding, &calls)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	// 1 initial + 1 auto-fix round spends the budget.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	waitForCalls(t, &calls, 2)

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{
		FindingIDs:     []string{"review-1"},
		OverrideFixCap: true,
	}); err != nil {
		t.Fatalf("override fix: %v", err)
	}
	waitForCalls(t, &calls, 3)

	rounds := reviewRounds(t, database, run.ID)
	if len(rounds) != 3 {
		t.Fatalf("recorded %d rounds, want 3", len(rounds))
	}
	if rounds[2].Trigger != db.RoundTriggerFixCapOverride {
		t.Fatalf("round 3 trigger = %q, want %q", rounds[2].Trigger, db.RoundTriggerFixCapOverride)
	}
	if !rounds[2].IsFixRound() {
		t.Fatal("an overridden round is still a fix round")
	}

	// The override authorizes one round, not a lifted cap.
	err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{FindingIDs: []string{"review-1"}})
	if err == nil || !strings.Contains(err.Error(), "auto_fix.review cap 1 reached") {
		t.Fatalf("cap did not re-arm after an override: %v", err)
	}

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionApprove, RespondOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	<-done
}

// No configured limit means no cap: the default review budget is 0 (auto-fix
// off) and agent-driven fix rounds must keep working exactly as before.
func TestExecutor_UnsetAutoFixLimitLeavesFixRoundsUnbounded(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}

	var calls atomic.Int64
	exec := NewExecutor(database, p, cfg, nil, []Step{laddering(types.StepReview, askUserFinding, &calls)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForCalls(t, &calls, 1)
	for round := 2; round <= 4; round++ {
		if err := respondWhenParked(t, exec, types.StepReview, types.ActionFix, RespondOptions{FindingIDs: []string{"review-1"}}); err != nil {
			t.Fatalf("uncapped fix round %d was refused: %v", round, err)
		}
		waitForCalls(t, &calls, int64(round))
	}

	if err := respondWhenParked(t, exec, types.StepReview, types.ActionApprove, RespondOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	<-done
}
