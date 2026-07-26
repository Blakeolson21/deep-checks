package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunToInfoIncludesImmutableSubmittedHead(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted-head", "base-head", nil)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "pipeline-fix-head"); err != nil {
		t.Fatalf("advance run head: %v", err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}

	info := runToInfo(d, run, nil)
	if info.HeadSHA != "pipeline-fix-head" {
		t.Fatalf("head = %q, want pipeline-fix-head", info.HeadSHA)
	}
	if info.SubmittedHeadSHA == nil || *info.SubmittedHeadSHA != "submitted-head" {
		t.Fatalf("submitted head = %v, want submitted-head", info.SubmittedHeadSHA)
	}
}

func TestStepToInfoIncludesFixSummaries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def", nil)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"}],"summary":"1"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("insert round 1: %v", err)
	}
	sum := "handle nil pointer in executor"
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, &sum, 100); err != nil {
		t.Fatalf("insert round 2: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 1 || info.FixSummaries[0] != sum {
		t.Errorf("fix summaries = %v, want [%q]", info.FixSummaries, sum)
	}
}

func TestStepToInfoNoFixSummariesWithoutFixRounds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def", nil)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 0 {
		t.Errorf("fix summaries = %v, want none", info.FixSummaries)
	}
}

// The daemon is the only source of run data for the axi run drive loop, so a
// skip set it fails to forward is invisible on that surface even though axi
// status (which reads SQLite directly) shows it.
func TestRunToInfoForwardsTheRecordedSkipSet(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base", []types.StepName{types.StepCI, types.StepPush})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	info := runToInfo(d, run, nil)
	if info.SkippedSteps == nil {
		t.Fatal("run info dropped the skip set; the drive loop would never report it")
	}
	if *info.SkippedSteps != "push,ci" {
		t.Errorf("SkippedSteps = %q, want %q", *info.SkippedSteps, "push,ci")
	}

	// A run that skips nothing still reports a value, so a reader can tell it
	// apart from a run whose plan was never recorded.
	none, err := d.InsertRun(repo.ID, "other", "head", "base", nil)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	none, err = d.GetRun(none.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if info := runToInfo(d, none, nil); info.SkippedSteps == nil || *info.SkippedSteps != "" {
		t.Errorf("SkippedSteps = %v, want a pointer to the empty string", info.SkippedSteps)
	}
}
