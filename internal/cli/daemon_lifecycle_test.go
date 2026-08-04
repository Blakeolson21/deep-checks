package cli

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/update"
)

func TestDaemonStopRefusesWithActiveRunsAndListsThem(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse while active runs exist")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	for _, want := range []string{
		"refusing daemon stop",
		"2 active pipeline runs",
		"feature-a",
		"aaa111",
		"feature-b",
		"bbb222",
		"--force",
	} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("daemon stop refusal should contain %q, got output %q error %v", want, out, err)
		}
	}
}

func TestDaemonStopForceOverridesActiveRunGuard(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop --force should stop the daemon")
	}
	if !strings.Contains(out, "FORCE: daemon stop") {
		t.Fatalf("force output should be loud, got %q", out)
	}
}

func TestDaemonRestartRefusesWithActiveRuns(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	startCalled := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStartFn = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart")
	if err == nil {
		t.Fatal("daemon restart should refuse while active runs exist")
	}
	if stopCalled || startCalled {
		t.Fatalf("daemon restart should not stop/start after refusing; stop=%t start=%t", stopCalled, startCalled)
	}
	if !strings.Contains(out+err.Error(), "refusing daemon restart") || !strings.Contains(out+err.Error(), "feature-a") {
		t.Fatalf("daemon restart refusal should list active runs, got output %q error %v", out, err)
	}
}

func TestLifecycleCommandsWriteCallerAttributionToCLILog(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths) error { return nil }
	daemonStartFn = func(*paths.Paths) error { return nil }
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("daemon", "restart", "--force")
	if err != nil {
		t.Fatalf("daemon restart --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("daemon", "stop", "--abandon-executing-runs")
	if err != nil {
		t.Fatalf("daemon stop --abandon-executing-runs failed: %v\n%s", err, out)
	}
	// Self-update is disabled in this fork build, so `update` refuses. Caller
	// attribution is logged before the command body runs, so the refusal is the
	// stronger case to assert here: it proves the lifecycle audit trail survives
	// a command that fails. Previously this passed only because isDevVersion
	// made `update` a silent no-op under `go test`, so it never exercised the
	// real path either way.
	out, err = executeCmd("update", "--force")
	if err == nil {
		t.Fatalf("update --force should refuse while self-update is disabled\n%s", out)
	}
	if !errors.Is(err, update.ErrSelfUpdateDisabled) {
		t.Fatalf("update --force error = %v, want ErrSelfUpdateDisabled\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(nmHome, "logs", "cli.log"))
	if err != nil {
		t.Fatalf("read cli.log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"lifecycle FORCE command=daemon.stop",
		"lifecycle FORCE command=daemon.restart",
		"lifecycle FORCE command=update",
		"force=true",
		"pid=",
		"ppid=",
		"parent_cmdline=",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("cli.log should contain %q, got %q", want, log)
		}
	}
	// The two flags authorize different destruction, so the forensic trail has
	// to distinguish them: --force alone is not --abandon-executing-runs.
	if !strings.Contains(log, "command=daemon.stop force=true abandon_executing=false") {
		t.Fatalf("cli.log should record --force without --abandon-executing-runs, got %q", log)
	}
	if !strings.Contains(log, "command=daemon.stop force=false abandon_executing=true") {
		t.Fatalf("cli.log should record --abandon-executing-runs distinctly, got %q", log)
	}
}

func createLifecycleGuardRuns(t *testing.T, p *paths.Paths) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := database.InsertRun(repo.ID, "feature-a", "aaa111", "000"); err != nil {
		t.Fatalf("insert pending run: %v", err)
	}
	running, err := database.InsertRun(repo.ID, "feature-b", "bbb222", "000")
	if err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	if err := database.UpdateRunStatus(running.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

// A run that is executing a step is the case --force must not cover: stopping
// the daemon cancels the step and strands the run's pipeline commits in the
// local gate. Regression for the 2026-08-02 cutover incident, where
// `daemon stop --force` killed a run mid-test-step.
func TestDaemonStopForceRefusesRunExecutingAStep(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-executing", types.StepTest, types.StepStatusRunning)
	stubDaemonAlive(t)
	stopCalled := stubDaemonStop(t)

	out, err := executeCmd("daemon", "stop", "--force")
	if err == nil {
		t.Fatal("daemon stop --force should refuse while a run is executing a step")
	}
	if *stopCalled {
		t.Fatal("daemon stop --force should not stop the daemon while a run is executing a step")
	}
	for _, want := range []string{
		"refusing daemon stop",
		"executing a step",
		"feature-executing",
		"step=test",
		"--abandon-executing-runs",
	} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("executing-run refusal should contain %q, got output %q error %v", want, out, err)
		}
	}
}

func TestDaemonRestartForceRefusesRunExecutingAStep(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-executing", types.StepReview, types.StepStatusFixing)
	stubDaemonAlive(t)
	stopCalled := stubDaemonStop(t)
	startCalled := false
	prevStart := daemonStartFn
	daemonStartFn = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStartFn = prevStart })

	out, err := executeCmd("daemon", "restart", "--force")
	if err == nil {
		t.Fatal("daemon restart --force should refuse while a run is executing a step")
	}
	if *stopCalled || startCalled {
		t.Fatal("daemon restart --force should not touch the daemon while a run is executing a step")
	}
	if !strings.Contains(out+err.Error(), "refusing daemon restart") {
		t.Fatalf("restart refusal missing, got output %q error %v", out, err)
	}
}

// The cutover case --force exists for: a run parked at a gate keeps
// status=running for as long as the agent takes to answer, and startup
// recovery resumes it, so stopping the daemon is recoverable.
func TestDaemonStopForceAllowsRunParkedAtAGate(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-parked", types.StepReview, types.StepStatusAwaitingApproval)
	stubDaemonAlive(t)
	stopCalled := stubDaemonStop(t)

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force should proceed for a parked run, got %v (output %q)", err, out)
	}
	if !*stopCalled {
		t.Fatal("daemon stop --force should stop the daemon for a parked run")
	}
	if !strings.Contains(out, "parked") {
		t.Fatalf("force warning should report the run as parked, got %q", out)
	}
}

func TestDaemonStopAbandonExecutingRunsOverridesExecutingGuard(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-executing", types.StepTest, types.StepStatusRunning)
	stubDaemonAlive(t)
	stopCalled := stubDaemonStop(t)

	out, err := executeCmd("daemon", "stop", "--abandon-executing-runs")
	if err != nil {
		t.Fatalf("--abandon-executing-runs should proceed, got %v (output %q)", err, out)
	}
	if !*stopCalled {
		t.Fatal("--abandon-executing-runs should stop the daemon")
	}
	if !strings.Contains(out, "FORCE: daemon stop") {
		t.Fatalf("--abandon-executing-runs should log the force warning, got %q", out)
	}
}

// With no daemon serving this NM_HOME nothing can be mid-step, so leftover
// rows must not escalate into the executing refusal.
func TestDaemonStopForceAllowsLeftoverRunsWhenDaemonIsDown(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-stale", types.StepTest, types.StepStatusRunning)
	prev := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return false, nil }
	t.Cleanup(func() { daemonIsRunningFn = prev })
	stopCalled := stubDaemonStop(t)

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force should proceed when the daemon is down, got %v (output %q)", err, out)
	}
	if !*stopCalled {
		t.Fatal("daemon stop --force should stop the daemon when only leftover rows exist")
	}
}

// A pending row is not a queued run. It is the window inside startRun where
// the daemon is building the worktree and fetching the trusted default branch,
// and a restart fails it rather than resuming it, so a live daemon holding one
// is driving it right now.
func TestDaemonStopForceRefusesPendingRunUnderLiveDaemon(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedPendingLifecycleRun(t, paths.WithRoot(nmHome), "feature-starting")
	stubDaemonAlive(t)
	stopCalled := stubDaemonStop(t)

	out, err := executeCmd("daemon", "stop", "--force")
	if err == nil {
		t.Fatal("daemon stop --force should refuse a pending run a live daemon is starting")
	}
	if *stopCalled {
		t.Fatal("daemon stop --force should not stop the daemon while a run is starting")
	}
	if !strings.Contains(out+err.Error(), "executing a step") || !strings.Contains(out+err.Error(), "feature-starting") {
		t.Fatalf("pending-run refusal should name the run, got output %q error %v", out, err)
	}
}

// `daemon start` with a daemon already running refreshes a drifted managed
// service definition by stopping and restarting it, which kills an executing
// step exactly like an unguarded `daemon stop`.
func TestDaemonStartRefusesRunExecutingAStep(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-executing", types.StepTest, types.StepStatusRunning)
	stubDaemonAlive(t)
	startCalled := stubDaemonStart(t)

	out, err := executeCmd("daemon", "start")
	if err == nil {
		t.Fatal("daemon start should refuse while a run is executing a step")
	}
	if *startCalled {
		t.Fatal("daemon start should not touch the daemon after refusing")
	}
	for _, want := range []string{
		"refusing daemon start",
		"executing a step",
		"feature-executing",
		"step=test",
		"--abandon-executing-runs",
	} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("daemon start refusal should contain %q, got output %q error %v", want, out, err)
		}
	}

	out, err = executeCmd("daemon", "start", "--abandon-executing-runs")
	if err != nil {
		t.Fatalf("daemon start --abandon-executing-runs should proceed, got %v (output %q)", err, out)
	}
	if !*startCalled {
		t.Fatal("daemon start --abandon-executing-runs should start the daemon")
	}
}

// Starting the daemon is how an operator recovers from a dead or wedged one,
// so leftover rows must never refuse it: the service-refresh path that could
// destroy work is reachable only when the daemon answers healthy.
func TestDaemonStartProceedsWhenNoHealthyDaemonOwnsTheLeftoverRuns(t *testing.T) {
	for name, probe := range map[string]func(*paths.Paths) (bool, error){
		"daemon down":         func(*paths.Paths) (bool, error) { return false, nil },
		"health probe failed": func(*paths.Paths) (bool, error) { return false, errors.New("dead socket") },
	} {
		t.Run(name, func(t *testing.T) {
			nmHome := t.TempDir()
			t.Setenv("NM_HOME", nmHome)
			seedLifecycleRunAtStep(t, paths.WithRoot(nmHome), "feature-stale", types.StepTest, types.StepStatusRunning)
			prev := daemonIsRunningFn
			daemonIsRunningFn = probe
			t.Cleanup(func() { daemonIsRunningFn = prev })
			startCalled := stubDaemonStart(t)

			out, err := executeCmd("daemon", "start")
			if err != nil {
				t.Fatalf("daemon start should proceed, got %v (output %q)", err, out)
			}
			if !*startCalled {
				t.Fatal("daemon start should start the daemon")
			}
		})
	}
}

// An unclean daemon death is the ordinary way to reach leftover run rows, and
// it leaves the exact artifacts that make the IPC health probe error rather
// than answer: a socket file with nothing listening. Failing closed on that
// error classified the dead daemon's rows as steps still executing, so the
// operator recovering a crashed daemon was told to pass
// --abandon-executing-runs to a daemon whose recorded process no longer
// exists. Proof of death outranks an unproven probe.
func TestDaemonRestartAfterUncleanDeathDoesNotDemandAbandoningExecutingRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}
	// t.TempDir() embeds the test name, and sockaddr_un caps a bound socket
	// path at 104 bytes on macOS, so this root has to stay short.
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	seedLifecycleRunAtStep(t, p, "feature-crashed", types.StepTest, types.StepStatusRunning)
	seedUncleanDaemonDeath(t, p)

	stopCalled := stubDaemonStop(t)
	startCalled := stubDaemonStart(t)

	out, err := executeCmd("daemon", "restart")
	if err == nil {
		t.Fatal("daemon restart should still refuse while leftover run rows exist")
	}
	refusal := out + err.Error()
	if strings.Contains(refusal, "executing a step") || strings.Contains(refusal, "--abandon-executing-runs") {
		t.Fatalf("a provably dead daemon cannot be executing a step, got output %q error %v", out, err)
	}
	if !strings.Contains(refusal, "--force") {
		t.Fatalf("leftover rows should be recoverable with --force, got output %q error %v", out, err)
	}

	out, err = executeCmd("daemon", "restart", "--force")
	if err != nil {
		t.Fatalf("daemon restart --force should proceed after an unclean death, got %v (output %q)", err, out)
	}
	if !*stopCalled || !*startCalled {
		t.Fatalf("daemon restart --force should restart the daemon; stop=%t start=%t", *stopCalled, *startCalled)
	}
}

// seedUncleanDaemonDeath leaves the artifacts of a daemon killed without
// teardown: a pid file naming a process that has already exited and been
// reaped, and a socket file that was bound and then abandoned.
func seedUncleanDaemonDeath(t *testing.T, p *paths.Paths) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn throwaway process: %v", err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.Socket(), Net: "unix"})
	if err != nil {
		t.Fatalf("bind socket: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close socket: %v", err)
	}
}

func stubDaemonStart(t *testing.T) *bool {
	t.Helper()
	called := false
	prev := daemonStartFn
	daemonStartFn = func(*paths.Paths) error {
		called = true
		return nil
	}
	t.Cleanup(func() { daemonStartFn = prev })
	return &called
}

func stubDaemonAlive(t *testing.T) {
	t.Helper()
	prev := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
	t.Cleanup(func() { daemonIsRunningFn = prev })
}

func stubDaemonStop(t *testing.T) *bool {
	t.Helper()
	called := false
	prev := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		called = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prev })
	return &called
}

// seedPendingLifecycleRun inserts one run left in the pending window, before
// any step row exists.
func seedPendingLifecycleRun(t *testing.T, p *paths.Paths, branch string) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, branch, "ddd444", "000")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.Status != types.RunPending {
		t.Fatalf("seeded run status = %s, want %s", run.Status, types.RunPending)
	}
}

// seedLifecycleRunAtStep inserts one running run whose step is left in status,
// which is how the guard tells an executing run from a parked one.
func seedLifecycleRunAtStep(t *testing.T, p *paths.Paths, branch string, step types.StepName, status types.StepStatus) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, branch, "ccc333", "000")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	sr, err := database.InsertStepResult(run.ID, step)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	switch status {
	case types.StepStatusAwaitingApproval, types.StepStatusFixReview:
		if err := database.ParkStepForApproval(run.ID, sr.ID, status, 0, nil); err != nil {
			t.Fatalf("park step: %v", err)
		}
	case types.StepStatusRunning:
		if err := database.StartStep(sr.ID); err != nil {
			t.Fatalf("start step: %v", err)
		}
	default:
		if err := database.StartStep(sr.ID); err != nil {
			t.Fatalf("start step: %v", err)
		}
		if err := database.UpdateStepStatus(sr.ID, status); err != nil {
			t.Fatalf("set step status: %v", err)
		}
	}
}
