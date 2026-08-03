package lifecycle

import (
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ActiveRuns returns all pending/running pipeline runs from the local state DB.
func ActiveRuns(p *paths.Paths) ([]*db.Run, error) {
	if p == nil {
		return nil, nil
	}
	dbPath := p.DB()
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat database: %w", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return database.GetActiveRuns()
}

func RunList(runs []*db.Run) string {
	if len(runs) == 0 {
		return ""
	}
	out := "active pipeline runs:\n"
	for _, run := range runs {
		out += fmt.Sprintf("  %s  %s  %s  %s\n", run.ID, run.Status, run.Branch, ShortSHA(run.HeadSHA))
	}
	return out
}

func ShortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

// Activity describes what is actually happening to an active run. Stopping the
// daemon means something different for each: a parked or idle run survives it,
// because startup recovery resumes a parked gate and an idle row was never in
// flight, but a run executing a step loses that step and fails with its
// pipeline commits stranded in the local gate repository.
type Activity string

const (
	// ActivityExecuting means a daemon is running a step for this run now.
	ActivityExecuting Activity = "executing"
	// ActivityParked means the run is blocked at a gate awaiting the agent.
	ActivityParked Activity = "parked"
	// ActivityIdle means no daemon is driving the run: it is queued behind
	// another run, or a row left over from a daemon that is no longer running.
	ActivityIdle Activity = "idle"
)

// ActiveRun pairs an active run with what the daemon is doing with it.
type ActiveRun struct {
	Run      *db.Run
	Activity Activity
	// Step is the step the run is executing or parked on, empty when the run
	// has no step to point at.
	Step types.StepName
}

// ClassifyActiveRuns returns every pending/running run in the local state DB
// annotated with its Activity. daemonRunning reports whether a daemon is
// serving this NM_HOME; when no daemon is, nothing can be mid-step, because
// startup recovery is what reconciles the rows a dead daemon left behind.
//
// Classification is deliberately asymmetric: a run is reported as parked or
// idle only on positive evidence, and anything else a live daemon still owns
// counts as executing. A run between two steps has neither a running step nor
// a gate marker, and guessing "safe" there is what a destructive lifecycle
// command must never do.
func ClassifyActiveRuns(p *paths.Paths, daemonRunning bool) ([]ActiveRun, error) {
	runs, err := ActiveRuns(p)
	if err != nil || len(runs) == 0 {
		return nil, err
	}

	classified := make([]ActiveRun, 0, len(runs))
	if !daemonRunning {
		for _, run := range runs {
			classified = append(classified, ActiveRun{Run: run, Activity: ActivityIdle})
		}
		return classified, nil
	}

	database, err := db.Open(p.DB())
	if err != nil {
		return nil, err
	}
	defer database.Close()
	for _, run := range runs {
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			return nil, fmt.Errorf("get steps for run %s: %w", run.ID, err)
		}
		classified = append(classified, classifyRun(run, steps))
	}
	return classified, nil
}

func classifyRun(run *db.Run, steps []*db.StepResult) ActiveRun {
	// A queued run has not started, so it owns no step and no worktree state.
	if run.Status == types.RunPending {
		return ActiveRun{Run: run, Activity: ActivityIdle}
	}
	for _, step := range steps {
		switch step.Status {
		case types.StepStatusRunning, types.StepStatusFixing:
			return ActiveRun{Run: run, Activity: ActivityExecuting, Step: step.StepName}
		case types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			return ActiveRun{Run: run, Activity: ActivityParked, Step: step.StepName}
		}
	}
	if run.AwaitingAgentSince != nil {
		return ActiveRun{Run: run, Activity: ActivityParked}
	}
	return ActiveRun{Run: run, Activity: ActivityExecuting}
}

// ExecutingRuns filters to the runs a daemon stop would kill mid-step.
func ExecutingRuns(runs []ActiveRun) []ActiveRun {
	executing := make([]ActiveRun, 0, len(runs))
	for _, run := range runs {
		if run.Activity == ActivityExecuting {
			executing = append(executing, run)
		}
	}
	return executing
}

// ActiveRunList renders classified runs for an operator-facing refusal.
func ActiveRunList(runs []ActiveRun) string {
	return activeRunList("active pipeline runs:", runs)
}

// ExecutingRunList renders the subset that is mid-step.
func ExecutingRunList(runs []ActiveRun) string {
	return activeRunList("pipeline runs executing a step:", runs)
}

func activeRunList(header string, runs []ActiveRun) string {
	if len(runs) == 0 {
		return ""
	}
	out := header + "\n"
	for _, run := range runs {
		out += fmt.Sprintf("  %s  %s  %s  %s  %s", run.Run.ID, run.Run.Status, run.Run.Branch, ShortSHA(run.Run.HeadSHA), run.Activity)
		if run.Step != "" {
			out += "  step=" + string(run.Step)
		}
		out += "\n"
	}
	return out
}
