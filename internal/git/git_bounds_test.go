package git

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The process-level halves of these bounds live in git_wedge_test.go, which is
// unix-only because its fixtures are /bin/sh scripts. Everything here is
// platform-agnostic on purpose: Windows runs this package's tests too, and the
// ceiling table and the delay floor are exactly the values a tidy-up would
// change without noticing what they carry.

// TestCommandWaitDelay_StaysGenerousEnoughForALoadedHost guards a value that
// reads like a tidy-up candidate but is load-bearing. When the delay expires,
// exec force-closes the parent's pipes, abandons whatever had not been copied
// yet, and reports ErrWaitDelay for a git command that exited 0; this package
// returns ("", err) on any error, and callers here fail closed on it, so a
// tight delay converts host load into a false "not clean" verdict that blocks
// custody recovery. The delay is also what separates a merely starved copying
// goroutine from a truncated read, so it has to clear scheduler starvation on a
// loaded host (the incident host sat at load 160), not merely a fast local run.
func TestCommandWaitDelay_StaysGenerousEnoughForALoadedHost(t *testing.T) {
	const floor = 30 * time.Second
	if commandWaitDelay < floor {
		t.Fatalf("commandWaitDelay = %s, below the %s floor: a delay this tight makes a loaded host report succeeding git commands as failures, and callers here fail closed on that error", commandWaitDelay, floor)
	}
}

func TestCommandTimeout_GivesNetworkSubcommandsTheLongerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"plumbing", []string{"rev-parse", "HEAD"}, defaultCommandTimeout},
		{"network fetch", []string{"fetch", "--no-tags", "origin"}, extendedCommandTimeout},
		{"network behind a leading flag", []string{"--git-dir=/tmp/x.git", "fetch", "origin"}, extendedCommandTimeout},
		{"plumbing behind a leading flag", []string{"--git-dir=/tmp/x.git", "rev-parse", "HEAD"}, defaultCommandTimeout},
		{"no subcommand", []string{"--version"}, defaultCommandTimeout},
		// A global option whose value is a separate argument must not let that
		// value pose as the subcommand; RefExists passes exactly this shape.
		{"plumbing behind -C", []string{"-C", "/tmp/repo", "rev-parse", "HEAD"}, defaultCommandTimeout},
		{"network behind -C", []string{"-C", "/tmp/repo", "fetch", "origin"}, extendedCommandTimeout},
		{"network behind separated --git-dir", []string{"--git-dir", "/tmp/x.git", "push", "origin"}, extendedCommandTimeout},
		{"network behind -c", []string{"-c", "gc.auto=0", "fetch", "origin"}, extendedCommandTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandTimeout(tc.args); got != tc.want {
				t.Errorf("commandTimeout(%q) = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}

// TestCommandTimeout_GivesTreeMaterializingSubcommandsTheLongerCeiling covers
// the commands whose runtime scales with repository size rather than finishing
// in seconds. `git worktree add --detach` builds every run's worktree under the
// daemon's deadline-free context, so the plumbing ceiling would newly kill a
// slow-but-healthy checkout on a large repository or a Defender-taxed Windows
// filesystem and fail the whole run.
func TestCommandTimeout_GivesTreeMaterializingSubcommandsTheLongerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"worktree add", []string{"worktree", "add", "--detach", "/tmp/wt", "HEAD"}},
		{"worktree add behind -C", []string{"-C", "/tmp/repo", "worktree", "add", "--detach", "/tmp/wt", "HEAD"}},
		{"worktree add behind --git-dir", []string{"--git-dir=/tmp/x.git", "worktree", "add", "--detach", "/tmp/wt", "HEAD"}},
		{"checkout", []string{"checkout", "--detach", "HEAD"}},
		{"read-tree", []string{"read-tree", "-m", "-u", "HEAD"}},
		{"reset", []string{"reset", "--hard", "HEAD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandTimeout(tc.args); got != extendedCommandTimeout {
				t.Errorf("commandTimeout(%q) = %s, want the extended ceiling %s: this command materializes a working tree, so its cost scales with repository size", tc.args, got, extendedCommandTimeout)
			}
		})
	}
}

// TestCommandTimeout_GivesHookRunningSubcommandsTheLongerCeiling covers the
// commands whose runtime belongs to a repository's hooks rather than to git.
// The pipeline commits without --no-verify, so a pre-commit hook that runs a
// typecheck or a test suite runs inside `git commit`; the plumbing ceiling
// would kill that healthy build and fail the run.
func TestCommandTimeout_GivesHookRunningSubcommandsTheLongerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"commit", []string{"commit", "-m", "no-mistakes: apply agent fixes"}},
		{"commit behind -C", []string{"-C", "/tmp/repo", "commit", "-m", "msg"}},
		{"commit behind --git-dir", []string{"--git-dir=/tmp/x.git", "commit", "-m", "msg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandTimeout(tc.args); got != extendedCommandTimeout {
				t.Errorf("commandTimeout(%q) = %s, want the extended ceiling %s: this command runs repository-supplied hooks, so its cost is not git's", tc.args, got, extendedCommandTimeout)
			}
		})
	}
}

// TestBoundContext_DerivesTheTierCeilingOnlyWhenTheCallerHasNone covers the
// policy the packages that cannot route through newCommand depend on. The
// pipeline steps resolve git through a step-scoped PATH and so build their own
// command; if the exported policy did not derive a ceiling for a deadline-free
// caller, or silently overrode a caller's own tighter window, those sites would
// be bounded differently from every call inside this package.
func TestBoundContext_DerivesTheTierCeilingOnlyWhenTheCallerHasNone(t *testing.T) {
	t.Run("deadline-free plumbing gets the plumbing ceiling", func(t *testing.T) {
		before := time.Now()
		ctx, cancel := BoundContext(context.Background(), "rev-parse", "HEAD")
		defer cancel()
		assertCeilingWithin(t, ctx, before, defaultCommandTimeout)
	})

	t.Run("deadline-free network gets the extended ceiling", func(t *testing.T) {
		before := time.Now()
		ctx, cancel := BoundContext(context.Background(), "fetch", "--no-tags", "origin")
		defer cancel()
		assertCeilingWithin(t, ctx, before, extendedCommandTimeout)
	})

	t.Run("a caller's own deadline is left alone", func(t *testing.T) {
		callerCtx, cancelCaller := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelCaller()
		want, _ := callerCtx.Deadline()

		ctx, cancel := BoundContext(callerCtx, "fetch", "origin")
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok || !got.Equal(want) {
			t.Errorf("deadline = %v (present %t), want the caller's own %v: widening a caller that deliberately bounded itself would undo its pacing", got, ok, want)
		}
	})
}

func assertCeilingWithin(t *testing.T, ctx context.Context, before time.Time, want time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("no deadline derived for a deadline-free caller: a git subprocess built on this context is unbounded and orphans its child when the caller dies")
	}
	if got := deadline.Sub(before); got < want || got > want+time.Minute {
		t.Errorf("ceiling = %s, want %s", got, want)
	}
}

// TestRun_CeilingExpiryNamesTheBoundThisPackageSupplied pins the diagnosis this
// bounding exists to produce. A ceiling expiry reaches the caller by two
// different routes: before the fork it is context.DeadlineExceeded returned
// straight out of exec.Cmd.Start, and after it exec kills git and Wait prefers
// the process's own *exec.ExitError, dropping the context error so that
// "signal: killed" reads as if git died on its own. Callers here fail closed on
// that error - HasUncommittedChanges reads as "not clean" and blocks a custody
// recovery - so both routes have to name the bound and carry the same cause,
// and asserting that is what makes this test independent of which route a given
// run happens to take. The kill route is exercised directly, with a real
// process, by TestRun_BoundsGitGivenADeadlineFreeContext.
func TestRun_CeilingExpiryNamesTheBoundThisPackageSupplied(t *testing.T) {
	restore := defaultCommandTimeout
	defaultCommandTimeout = time.Nanosecond
	t.Cleanup(func() { defaultCommandTimeout = restore })

	_, err := Run(context.Background(), t.TempDir(), "status", "--porcelain")
	if err == nil {
		t.Fatal("expected an error once the derived ceiling had already expired")
	}
	assertNamesTheCeiling(t, err)
}

// assertNamesTheCeiling is the contract both ceiling-expiry routes owe their
// callers, shared so the kill route cannot drift away from the pre-fork route.
func assertNamesTheCeiling(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not unwrap to context.DeadlineExceeded, so a caller cannot tell this package's bound from a git failure: %v", err)
	}
	if !strings.Contains(err.Error(), "internal/git ceiling") {
		t.Errorf("error does not name the ceiling this package supplied, leaving the failure undiagnosable: %v", err)
	}
}
