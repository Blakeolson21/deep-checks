//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiCleanRebasedRunWithSkippedPushReturnsCustodyJourney reproduces the
// custody deadlock caught on dogfood runs 01KZ3Z8WZPDG7FHAG7QJF4S5DY and
// 01KZ4176DNSQCTVXXE0NNS8K2K with the real binary, in the exact production
// shape: a CLEAN run (the agent finds nothing and commits nothing) whose branch
// needed a rebase, with push skipped.
//
// The rebase step was then the only writer of the run head, and it recorded
// that head only on the run row. The pipeline worktree is detached, so the
// rebased commits were referenced by no ref at all: branch sync classified the
// branch pipeline_owned, and `axi sync --recover` refused with
// blocked_recover_gate_diverged because the gate branch still sat at the
// submitted head. The operator had no exit.
//
// The journey asserts the end-user experience, not the mechanism: the run
// completes, the gate branch carries the rebased head, `axi sync --recover`
// returns custody, the operator worktree ends up on the rebased head with its
// own file content plus the advanced base, and a fresh run starts cleanly.
func TestAxiCleanRebasedRunWithSkippedPushReturnsCustodyJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.CommitChange("init-skip-push", "seed.txt", "seed\n", "seed skip-push init")
	initWorktree := h.AddWorktree("init-skip-push")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/skip-push-rebase"
	submitted := h.CommitChange(branch, "feature.txt", "feature\n", "add feature")

	// The default branch advances before the run, so the pipeline's own rebase
	// step has to replay the operator's commit onto the newer base.
	h.CommitChange("main", "upstream-advance.txt", "advance\n", "upstream advance")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("advance upstream main: %v\n%s", err, out)
	}

	operator := h.AddWorktree(branch)
	runOut, err := h.RunInDir(operator, "axi", "run", "--yes",
		"--skip", "push,pr,ci",
		"--intent", "validate the feature with delivery skipped")
	if err != nil {
		t.Fatalf("clean run with skipped delivery: %v\n%s", err, runOut)
	}
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed\n%s", run.Status, runOut)
	}
	if run.HeadSHA == submitted {
		t.Fatalf("pipeline head never moved (%s); the rebase did not run, so this is not the reported shape", run.HeadSHA)
	}

	// The fix: the rebased head the run recorded is anchored on the gate branch
	// ref. Without it nothing references those commits once the detached
	// pipeline worktree is removed.
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	gateHeadBytes, err := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("gate branch head: %v\n%s", err, gateHeadBytes)
	}
	gateHead := strings.TrimSpace(string(gateHeadBytes))
	if gateHead != run.HeadSHA {
		t.Fatalf("gate branch ref = %s but the run recorded head %s; the rebased head is stranded", gateHead, run.HeadSHA)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatalf("operator branch moved without an explicit recovery: %s", got)
	}

	// What the operator sees before recovering: the branch is in pipeline
	// custody and the offered exit is the guarded custody return.
	checkOut, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
	if checkErr == nil {
		t.Fatalf("sync --check should report the branch as blocked, got success:\n%s", checkOut)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("sync --check output missing %q:\n%s", want, checkOut)
		}
	}

	// The deadlock: this used to refuse with blocked_recover_gate_diverged.
	recoverOut, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("custody recovery refused after a clean rebased run with push skipped: %v\n%s", err, recoverOut)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned", "no-mistakes axi run --intent"} {
		if !strings.Contains(recoverOut, want) {
			t.Errorf("recover output missing %q:\n%s", want, recoverOut)
		}
	}

	// The operator's branch now holds the rebased head: their own change
	// survived, and the advanced base came with it.
	if got, gitErr := h.runGit(context.Background(), operator, "rev-parse", "HEAD"); gitErr != nil || strings.TrimSpace(string(got)) != run.HeadSHA {
		t.Fatalf("operator HEAD after recovery = %s (err %v), want the pipeline head %s", strings.TrimSpace(string(got)), gitErr, run.HeadSHA)
	}
	feature, readErr := os.ReadFile(filepath.Join(operator, "feature.txt"))
	if readErr != nil || strings.TrimSpace(string(feature)) != "feature" {
		t.Fatalf("operator file content lost after recovery: %q (err %v)", string(feature), readErr)
	}
	if _, statErr := os.Stat(filepath.Join(operator, "upstream-advance.txt")); statErr != nil {
		t.Fatalf("recovered head did not bring the advanced base into the worktree: %v", statErr)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "status", "--porcelain"); gitErr != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worktree not clean after recovery: %q (err %v)", string(out), gitErr)
	}

	// Custody is genuinely back: a fresh run starts instead of being blocked.
	freshOut, err := h.RunInDir(operator, "axi", "run", "--yes", "--skip", "push,pr,ci",
		"--intent", "validate again on the recovered head")
	if err != nil {
		t.Fatalf("fresh run after custody recovery: %v\n%s", err, freshOut)
	}
}
