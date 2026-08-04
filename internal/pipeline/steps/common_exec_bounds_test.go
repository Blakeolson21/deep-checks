package steps

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// The steps launch git themselves rather than through git.Run, because they
// have to resolve the binary from a step-scoped PATH. That put every step git
// call outside the package that owns the subprocess bounds: sctx.Ctx descends
// from the IPC server's context.Background() and carries no deadline, so a git
// child that never exits blocked its step goroutine forever and was orphaned
// when the daemon died - the incident's own shape, in daemon production code.
// These pin that the step path now carries the same two bounds git.Run does.

func boundsTestStepContext(t *testing.T, ctx context.Context) *pipeline.StepContext {
	t.Helper()
	return &pipeline.StepContext{Ctx: ctx, WorkDir: t.TempDir()}
}

func TestStepGitCmd_CarriesTheSubprocessBoundsGitRunWouldSupply(t *testing.T) {
	sctx := boundsTestStepContext(t, context.Background())

	cmd, cancel := stepGitCmd(sctx, "rev-parse", "HEAD")
	defer cancel()

	if cmd.WaitDelay != git.CommandWaitDelay() {
		t.Errorf("WaitDelay = %s, want %s: without it, cmd.Output here can wedge forever on a pipe an agent-spawned test worker inherited and kept open", cmd.WaitDelay, git.CommandWaitDelay())
	}
	if cmd.WaitDelay == 0 {
		t.Error("WaitDelay is unset, so a surviving descendant holding the inherited stdout pipe has no backstop at all")
	}
}

func TestBoundedGitStep_BoundsADeadlineFreeExecutorContext(t *testing.T) {
	sctx := boundsTestStepContext(t, context.Background())
	if _, ok := sctx.Ctx.Deadline(); ok {
		t.Fatal("the executor context under test must carry no deadline, matching the real one")
	}

	bounded, cancel := boundedGitStep(sctx, "rev-parse", "HEAD")
	defer cancel()

	deadline, ok := bounded.Ctx.Deadline()
	if !ok {
		t.Fatal("no deadline derived: a git child that never exits blocks the step goroutine forever and is orphaned when the daemon is killed")
	}

	// The step path must land on internal/git's policy, not a second number
	// that drifts from it.
	ownerCtx, cancelOwner := git.BoundContext(context.Background(), "rev-parse", "HEAD")
	defer cancelOwner()
	owner, _ := ownerCtx.Deadline()
	if drift := deadline.Sub(owner); drift < -time.Minute || drift > time.Minute {
		t.Errorf("step ceiling differs from what git.Run would supply by %s: the step path is applying its own bound instead of the shared policy", drift)
	}
}

// TestBoundedGitStep_NetworkSubcommandGetsTheLongerCeiling checks the step path
// reads the tier from the shared policy rather than flattening every git call
// to one number: a push or fetch against a real remote must not inherit the
// plumbing ceiling here any more than it does inside internal/git.
func TestBoundedGitStep_NetworkSubcommandGetsTheLongerCeiling(t *testing.T) {
	sctx := boundsTestStepContext(t, context.Background())

	plumbing, cancelPlumbing := boundedGitStep(sctx, "rev-parse", "HEAD")
	defer cancelPlumbing()
	network, cancelNetwork := boundedGitStep(sctx, "fetch", "--no-tags", "origin")
	defer cancelNetwork()

	plumbingDeadline, ok := plumbing.Ctx.Deadline()
	if !ok {
		t.Fatal("plumbing call was left unbounded")
	}
	networkDeadline, ok := network.Ctx.Deadline()
	if !ok {
		t.Fatal("network call was left unbounded")
	}
	if !networkDeadline.After(plumbingDeadline) {
		t.Errorf("fetch deadline %v is not later than rev-parse's %v: the step path is not reading the subcommand tier, so a slow real fetch would be killed as if it were plumbing", networkDeadline, plumbingDeadline)
	}
}

// TestBoundedGitStep_KeepsACallerWindow protects the CI poll loop's ls-remote,
// which deliberately bounds itself far tighter so a hung git transport cannot
// defer timeout detection until the next poll.
func TestBoundedGitStep_KeepsACallerWindow(t *testing.T) {
	callerCtx, cancelCaller := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelCaller()
	want, _ := callerCtx.Deadline()

	bounded, cancel := boundedGitStep(boundsTestStepContext(t, callerCtx), "ls-remote", "origin")
	defer cancel()

	got, ok := bounded.Ctx.Deadline()
	if !ok || !got.Equal(want) {
		t.Errorf("deadline = %v (present %t), want the caller's own %v: widening it would undo the tighter window that step chose on purpose", got, ok, want)
	}
}
