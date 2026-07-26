package daemon

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PROBE ONLY: does a run whose skipped_steps is NULL (a row written before the
// column existed) resume into push?
func TestProbeLegacyNullSkipSetResumesIntoPush(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, headSHA := setupTestGitRepo(t, p, d, "legacy-null-skip")

	run, err := d.InsertRun(repo.ID, "main", headSHA, headSHA, []types.StepName{types.StepPush})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a row written before skipped_steps existed.
	raw, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE runs SET skipped_steps = NULL WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	push := &mockPassStep{name: types.StepPush}
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepReview}, push}
		})
	}()
	defer func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			_ = client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never accepted an approval")
		}
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			var response ipc.RespondResult
			err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
				RunID:  run.ID,
				Step:   types.StepReview,
				Action: types.ActionApprove,
			}, &response)
			_ = client.Close()
			if err == nil {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	final := waitForRunTerminalState(t, d, run.ID)
	t.Logf("PROBE: run status=%s push_exec_count=%d", final.Status, push.execCnt.Load())
	if push.execCnt.Load() != 0 {
		t.Logf("PROBE RESULT: legacy NULL plan RESUMED INTO PUSH (exec=%d)", push.execCnt.Load())
	} else {
		t.Logf("PROBE RESULT: push was not executed")
	}
}
