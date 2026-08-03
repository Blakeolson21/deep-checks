package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRebaseStep_AdvancesGateBranchRef reproduces the custody deadlock observed
// on 2026-08-03 (runs 01KZ3Z8WZPDG7FHAG7QJF4S5DY and 01KZ4176DNSQCTVXXE0NNS8K2K).
//
// Topology mirrors production exactly:
//   - upstream bare repo = origin (GitHub)
//   - gate bare repo holds refs/heads/feature at the submitted head
//   - the pipeline works in a DETACHED worktree of the gate (git.WorktreeAdd
//     uses `worktree add --detach`)
//
// The rebase step moves the detached HEAD onto origin/main and records the new
// head in the DB via UpdateRunHeadSHA, but never writes it to refs/heads/feature
// in the gate. With push skipped, the branch ref is left at the submitted head
// while the run records a head_sha nothing references. branchsync.Recover then
// compares the gate branch head against run.HeadSHA and refuses with
// blocked_recover_gate_diverged, which is the deadlock.
func TestRebaseStep_AdvancesGateBranchRef(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "--initial-branch=main")

	// Seed origin/main.
	seed := t.TempDir()
	gitCmd(t, seed, "init", "--initial-branch=main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	writeFile(t, seed, "base.txt", "base\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")

	// The agent's submitted branch commit, built on the old main.
	gitCmd(t, seed, "checkout", "-b", "feature")
	writeFile(t, seed, "feature.txt", "feature\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-m", "feature change")
	submittedHead := gitCmd(t, seed, "rev-parse", "HEAD")

	// origin/main advances with a non-conflicting commit after submission.
	gitCmd(t, seed, "checkout", "main")
	writeFile(t, seed, "other.txt", "other\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-m", "main advances")
	gitCmd(t, seed, "push", "origin", "main")

	// The gate: a bare repo pointing at origin, holding the submitted branch.
	gate := t.TempDir()
	gitCmd(t, gate, "init", "--bare", "--initial-branch=main")
	gitCmd(t, gate, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "remote", "add", "gate", gate)
	gitCmd(t, seed, "push", "gate", "feature")

	if got := gitCmd(t, gate, "rev-parse", "refs/heads/feature"); got != submittedHead {
		t.Fatalf("gate feature ref = %s, want submitted head %s", got, submittedHead)
	}

	// The pipeline worktree: detached, exactly as git.WorktreeAdd creates it.
	workDir := filepath.Join(t.TempDir(), "wt")
	gitCmd(t, gate, "worktree", "add", "--detach", workDir, submittedHead)
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, workDir, baseSHA, submittedHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.DefaultBranch = "main"

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("rebase step: %v", err)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Fatalf("unexpected approval request: %s", outcome.Findings)
	}

	rebasedHead := gitCmd(t, workDir, "rev-parse", "HEAD")
	if rebasedHead == submittedHead {
		t.Fatalf("rebase did not move HEAD; test setup is wrong")
	}
	if sctx.Run.HeadSHA != rebasedHead {
		t.Fatalf("run head = %s, want rebased head %s", sctx.Run.HeadSHA, rebasedHead)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != rebasedHead {
		t.Fatalf("db head = %s, want rebased head %s", dbRun.HeadSHA, rebasedHead)
	}

	// The defect: the recorded head is referenced by nothing in the gate. When
	// the run ends the worktree is removed and the rebased commits become
	// unreachable, while branchsync.Recover still verifies the preserved head
	// against refs/heads/feature.
	gateHead := gitCmd(t, gate, "rev-parse", "refs/heads/feature")
	if gateHead != rebasedHead {
		t.Fatalf("gate branch ref = %s, but the run recorded head_sha = %s; "+
			"the recorded pipeline head is unreferenced, which strands the branch "+
			"in pipeline_owned with no working recovery", gateHead, rebasedHead)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
