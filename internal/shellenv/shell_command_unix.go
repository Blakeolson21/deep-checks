//go:build unix

package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/proctree"
)

// defaultWaitDelay is the pipe backstop installed on cmd.WaitDelay, mirroring
// the Windows helper. After a cancelled command's leader is signalled, a
// grandchild that inherited and still holds open the leader's stdout/stderr
// pipe would otherwise wedge cmd.Wait (and any in-flight pipe Read) forever.
// A nonzero WaitDelay lets the exec package close those inherited pipes and
// return instead of blocking. It is a worst-case ceiling only: on a clean exit
// the pipes close immediately and Wait returns without waiting.
const defaultWaitDelay = 5 * time.Second

// ConfigureShellCommand isolates cmd in its own process group (Setpgid) and
// installs a cmd.Cancel that reaps the resulting process tree when cmd's context
// is cancelled. exec.CommandContext otherwise only kills the direct child PID,
// leaving grandchildren (a test runner's worker processes, an agent-spawned
// git/build/editor) running and holding the worktree locked.
//
// A process group is not a process tree, and the difference is not academic. A
// descendant that calls setsid() gets its own session and its own group, so
// kill(-leaderPID) provably cannot reach it - and setsid() is what Node's
// `detached: true` does, which is how Claude Code's CLI Bash tool spawns its
// shell. Reaching those descendants needs the ppid walk in internal/proctree,
// which reapProcessTree performs alongside the group kill.
//
// Cancellation is only half the lifecycle: cmd.Cancel never fires when the
// command exits on its own (success or failure). Use RunShellCommand,
// OutputShellCommand, or CombinedOutputShellCommand for one-shot commands, or
// use StartShellCommand and defer TerminateShellCommandGroup immediately after
// a successful start when the caller needs manual pipe handling. If a parser
// reads stdout/stderr until EOF, the goroutine that owns Wait should terminate
// the group when the leader exits so inherited pipe holders cannot wedge the
// parser.
//
// Apply this to every long-lived subprocess no-mistakes spawns on behalf of a
// cancellable step/agent invocation.
func ConfigureShellCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Install the WaitDelay backstop unless the caller picked one explicitly
	// (the short login-shell probe uses a tighter bound of its own).
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultWaitDelay
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := reapProcessTree(cmd.Process.Pid)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// reapProcessTree kills the process group led by pid and every descendant that
// has left it, returning the error from signalling the group.
//
// Order matters. The snapshot is taken BEFORE the leader is signalled: the
// instant the leader dies the kernel rewrites its children's ppid to 1 and
// reparents them to launchd or init, which erases the only trail back to this
// command. A walk performed afterwards has nothing to follow.
//
// The group kill still runs first among the signals because it is one syscall
// that handles the common case. The tree walk is what catches a descendant that
// called setsid() and left the group, which is the case the group kill provably
// cannot reach.
// This path deliberately runs no process listing of its own. Snapshots are the
// poller's job (tracker_unix.go), because a listing here would cost tens of
// milliseconds on every command teardown - including every short git subprocess
// - and would be too late to help anyway once the leader has exited. When
// nothing escaped, which is the overwhelmingly common case, this is a single
// syscall.
func reapProcessTree(pid int) error {
	descendants, trackedGroups := takeTrackedLeader(pid)

	err := syscall.Kill(-pid, syscall.SIGKILL)

	// Kill each distinct group the descendants occupy. A setsid() escapee leads
	// its own group, so this also reaches children it spawned since the last
	// sample, which no pid list can cover.
	groups := make(map[int]bool, len(descendants))
	for _, pgid := range trackedGroups {
		groups[pgid] = true
	}
	for _, p := range descendants {
		if p.PGID != pid {
			groups[p.PGID] = true
		}
	}
	for pgid := range groups {
		proctree.KillGroup(pgid)
	}
	proctree.Kill(descendants)
	return err
}

// StartShellCommand starts cmd after ConfigureShellCommand has prepared its
// process-group lifecycle. Unix needs no extra setup beyond cmd.Start, but the
// wrapper keeps call sites aligned with Windows job-object setup.
func StartShellCommand(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Registering here rather than at each call site is deliberate: there are
	// many call sites, and one that forgot to register would silently
	// reintroduce the leak with no visible symptom until a host ran out of CPU.
	trackLeader(cmd.Process.Pid, time.Now())
	return nil
}

// TerminateShellCommandGroup SIGKILLs the whole process group led by a command
// configured with ConfigureShellCommand. It is the success/failure-path
// counterpart to cmd.Cancel: callers defer it right after a successful Start so
// the group is reaped however Run returns - clean exit, parse error, or
// wait error - not only on context cancellation.
//
// Why this matters: Setpgid puts each agent/command in its own group, but a
// test runner's worker pool, a build watcher, or a dev server the agent spawned
// can outlive the leader. On a normal exit nothing signals the group, so those
// grandchildren reparent to init and keep running (and keep their memory). They
// accumulate across runs until the host is out of memory, at which point the OS
// OOM-killer reaps processes - including the daemon - with an uncatchable
// SIGKILL, surfacing as "daemon crashed during execution". Reaping the group on
// every exit path closes that leak so the test step can never take the daemon
// down.
//
// It is safe to call unconditionally after Wait: the group persists only while
// a member is alive, so when the leader exited cleanly with no survivors the
// kill is a harmless no-op (ESRCH). A nil or never-started command is a no-op.
//
// By the time this runs the leader is already dead, so a live snapshot can no
// longer link anything back to it - the kernel has rewritten its children's ppid
// to 1. What makes this path work is the descendant union the poller accumulated
// while the leader was still alive; see tracker_unix.go. Calling it twice is
// harmless: the union is consumed on the first call.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = reapProcessTree(cmd.Process.Pid)
}
