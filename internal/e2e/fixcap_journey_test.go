//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fixCapScenario makes the review step ladder: every review round returns the
// same blocking auto-fix finding, so nothing but a budget can stop it. This is
// the shape the real 2026-08-05 runs had when they reached 9 to 12 rounds.
func fixCapScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fix-cap-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found a defect"
    structured:
      findings:
        - id: "cap-1"
          severity: error
          file: "feature.txt"
          line: 1
          description: "still not right"
          action: auto-fix
      summary: "found 1 issue"
      risk_level: high
      risk_rationale: "defect keeps reappearing"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fix-cap scenario: %v", err)
	}
	return path
}

// TestAxiFixCapJourney drives a never-converging review through the real daemon
// and CLI with auto_fix.review: 1 configured, and proves the configured limit is
// a hard ceiling on the whole run: the automatic round spends it, the gate then
// states why it will not fund another, an ordinary `respond --action fix` is
// refused instead of laddering, and only an explicit --override-fix-cap buys one
// more round. Without the ceiling this run would fix forever.
func TestAxiFixCapJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: fixCapScenario(t)})

	h.CommitChange("init-cap", "seed.txt", "seed\n", "seed for fix cap")
	initWorktree := h.AddWorktree("init-cap")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// auto_fix is a non-executing field, so the pushed branch's copy applies.
	h.CommitChange("feature/cap", ".no-mistakes.yaml",
		"ignore_patterns:\n  - '*.generated.go'\nallow_repo_commands: true\nauto_fix:\n  review: 1\n",
		"cap review fix rounds at 1")
	h.CommitChange("feature/cap", "feature.txt", "change\n", "add feature change")
	fw := h.AddWorktree("feature/cap")

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", "bound the review fix rounds"); err != nil {
		t.Fatalf("axi run (expected to stop at gate, exit 0): %v\n%s", err, out)
	}

	// One automatic fix round spends the budget, so the run parks in fix_review.
	gated := waitForStepStatus(t, h, "feature/cap", types.StepReview, types.StepStatusFixReview, 90*time.Second)
	if gated == nil {
		t.Fatal("expected feature/cap run to park after its funded fix round")
	}
	reviewStep, ok := findStep(gated.Steps, types.StepReview)
	if !ok {
		t.Fatal("no review step recorded")
	}
	if reviewStep.RoundCount != 2 {
		t.Fatalf("review ran %d rounds under auto_fix.review: 1, want 2 (initial + 1 funded)", reviewStep.RoundCount)
	}

	// The gate states the spent budget instead of looking like an ordinary park.
	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status (capped): %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "auto_fix.review cap 1 reached") {
		t.Errorf("axi status did not state the spent fix-round cap:\n%s", statusOut)
	}
	if !strings.Contains(statusOut, "--override-fix-cap") {
		t.Errorf("axi status did not name the override that re-funds a round:\n%s", statusOut)
	}

	// An ordinary fix is refused, and refusing it leaves the gate answerable.
	fixOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", "cap-1")
	if err == nil {
		t.Fatalf("fix past the cap was accepted:\n%s", fixOut)
	}
	if !strings.Contains(fixOut, "auto_fix.review cap 1 reached") {
		t.Errorf("refusal did not state the cap:\n%s", fixOut)
	}
	stillGated := h.ActiveRun("feature/cap")
	if stillGated == nil {
		t.Fatal("refused fix ended the run")
	}
	if step, ok := findStep(stillGated.Steps, types.StepReview); !ok || step.RoundCount != 2 {
		t.Fatalf("refused fix still ran a round: %+v", step)
	}

	// The override funds exactly one more round; the run parks again after it.
	overrideOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", "cap-1", "--override-fix-cap")
	if err != nil {
		t.Fatalf("override fix: %v\n%s", err, overrideOut)
	}
	afterOverride := waitForStepRoundCount(t, h, "feature/cap", types.StepReview, 3, 90*time.Second)
	if afterOverride == nil {
		t.Fatal("override did not fund a third review round")
	}

	// And the cap re-arms: the next ordinary fix is refused again.
	secondFixOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", "cap-1")
	if err == nil {
		t.Fatalf("cap did not re-arm after the override:\n%s", secondFixOut)
	}
	if !strings.Contains(secondFixOut, "auto_fix.review cap 1 reached") {
		t.Errorf("re-armed refusal did not state the cap:\n%s", secondFixOut)
	}

	// The park is a decision point, not a dead end: approving clears it.
	if out, err := h.RunInDir(fw, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("axi respond approve: %v\n%s", err, out)
	}
	completed := h.WaitForRun("feature/cap", 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("feature/cap run status = %s, want completed", completed.Status)
	}
}

func waitForStepRoundCount(t *testing.T, h *Harness, branch string, stepName types.StepName, want int, timeout time.Duration) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runs := h.Runs()
		for i := range runs {
			run := runs[i]
			if run.Branch != branch {
				continue
			}
			if step, ok := findStep(run.Steps, stepName); ok && step.RoundCount >= want {
				return &run
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("step %s for branch %s did not reach %d rounds in %v", stepName, branch, want, timeout)
	return nil
}
