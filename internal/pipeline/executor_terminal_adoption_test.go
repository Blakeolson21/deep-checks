package pipeline

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// terminalAdoptionFixture models the production topology the custody deadlock
// needs: a bare gate holding refs/heads/feature at the submitted head, and the
// DETACHED pipeline worktree carved from it.
type terminalAdoptionFixture struct {
	gate      string
	workDir   string
	base      string
	submitted string
}

func newTerminalAdoptionFixture(t *testing.T) *terminalAdoptionFixture {
	t.Helper()
	root := t.TempDir()

	seed := filepath.Join(root, "seed")
	if err := exec.Command("git", "init", "-b", "main", seed).Run(); err != nil {
		t.Fatal(err)
	}
	execGit(t, seed, "config", "user.email", "test@test.com")
	execGit(t, seed, "config", "user.name", "Test")
	writeTestFile(t, seed, "file.txt", "base\n")
	execGit(t, seed, "add", "-A")
	execGit(t, seed, "commit", "-m", "base")
	base := gitOut(t, seed, "rev-parse", "HEAD")
	execGit(t, seed, "checkout", "-b", "feature")
	writeTestFile(t, seed, "file.txt", "feature\n")
	execGit(t, seed, "commit", "-am", "feature")
	submitted := gitOut(t, seed, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	if err := exec.Command("git", "init", "--bare", gate).Run(); err != nil {
		t.Fatal(err)
	}
	execGit(t, seed, "push", gate, "refs/heads/feature:refs/heads/feature")

	workDir := filepath.Join(root, "pipeline-wt")
	execGit(t, gate, "worktree", "add", "--detach", workDir, submitted)
	execGit(t, workDir, "config", "user.email", "test@test.com")
	execGit(t, workDir, "config", "user.name", "Test")

	return &terminalAdoptionFixture{gate: gate, workDir: workDir, base: base, submitted: submitted}
}

// selfCommit models the fix-round agent committing on its own: the head moves
// in the detached worktree and no step adopts it.
func (f *terminalAdoptionFixture) selfCommit(t *testing.T, content string) string {
	t.Helper()
	writeTestFile(t, f.workDir, "file.txt", content)
	execGit(t, f.workDir, "commit", "-am", "agent self-commit")
	return gitOut(t, f.workDir, "rev-parse", "HEAD")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// TestExecutor_TerminalizationAdoptsTheHeadItRecords closes the sibling path of
// the custody deadlock. Terminalization is a run-head writer like the four
// ref-advancing steps: when the detached worktree head is a verified descendant
// it is persisted as the run's terminal head. The worktree dies with the run, so
// recording a head no ref holds strands the branch in pipeline custody with no
// working recovery - branchsync compares the gate branch against the recorded
// head and refuses, and --keep-local cannot reach that refusal.
func TestExecutor_TerminalizationAdoptsTheHeadItRecords(t *testing.T) {
	database, p, _, repo := setupTest(t)
	f := newTerminalAdoptionFixture(t)
	run, err := database.InsertRun(repo.ID, "feature", f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	selfCommit := f.selfCommit(t, "feature, fixed by the agent\n")

	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.workDir = f.workDir
	if err := exec.completeRun(run, repo); err != nil {
		t.Fatalf("completeRun: %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != selfCommit {
		t.Fatalf("recorded head = %s, want the observed self-commit %s", got.HeadSHA, selfCommit)
	}
	if got.TerminalHeadVerifiedAt == nil {
		t.Fatal("verified terminal head was not stamped")
	}
	if gateHead := gitOut(t, f.gate, "rev-parse", "refs/heads/feature"); gateHead != selfCommit {
		t.Fatalf("gate branch ref = %s, but the run recorded head_sha = %s; the recorded terminal head is referenced by nothing, which strands the branch in pipeline custody", gateHead, selfCommit)
	}
}

// TestExecutor_TerminalizationRefusesToRecordAnUnadoptableHead is the fail-safe
// half: when another push owns the branch, the guarded adoption refuses, and
// terminalization must then keep the last adopted head rather than record one
// the branch does not hold.
func TestExecutor_TerminalizationRefusesToRecordAnUnadoptableHead(t *testing.T) {
	database, p, _, repo := setupTest(t)
	f := newTerminalAdoptionFixture(t)
	run, err := database.InsertRun(repo.ID, "feature", f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	f.selfCommit(t, "feature, fixed by the agent\n")

	tree := gitOut(t, f.gate, "rev-parse", f.submitted+"^{tree}")
	outOfBand := gitOut(t, f.gate, "-c", "user.email=other@test.com", "-c", "user.name=Other",
		"commit-tree", tree, "-p", f.submitted, "-m", "second push")
	execGit(t, f.gate, "update-ref", "refs/heads/feature", outOfBand)

	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.workDir = f.workDir
	if err := exec.completeRun(run, repo); err != nil {
		t.Fatalf("completeRun: %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != f.submitted {
		t.Fatalf("recorded head = %s, want the last adopted head %s", got.HeadSHA, f.submitted)
	}
	if got.TerminalHeadVerifiedAt != nil {
		t.Fatal("an unadopted head was stamped as the verified terminal head")
	}
	if gateHead := gitOut(t, f.gate, "rev-parse", "refs/heads/feature"); gateHead != outOfBand {
		t.Fatalf("gate branch ref = %s, want the untouched out-of-band head %s", gateHead, outOfBand)
	}
	assertTerminalStatus(t, database, run.ID)
}

func assertTerminalStatus(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "" || got.Status == "running" || got.Status == "pending" {
		t.Fatalf("run status = %q, want a terminal status", got.Status)
	}
}
