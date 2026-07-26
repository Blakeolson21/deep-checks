# Transitive process-tree reap

Date: 2026-07-25
Status: approved, branch 1 of 3

## Problem

Gate run `01KY36EAXX53WZSEC4ZYSMY4F0` (repo `433465ced6fd`) ran its `test` step from
16:37 to 18:03 on 2026-07-21 and reported `status=completed exit_code=0`. Its claude
agent invocation spawned six `xctest` processes between 17:09 and 17:15. Three days
later those six were still alive at roughly 50% CPU each with `PPID=1`, consuming
about half the machine and slowing every other gate run on the box. The run completed
green while leaking them, and the harness force-removed the worktree out from under
them.

### Root cause

`ConfigureShellCommand` in `internal/shellenv/shell_command_unix.go` sets only
`Setpgid: true`. That is a process *group*, not a session. Both `cmd.Cancel` and
`TerminateShellCommandGroup` reap with `syscall.Kill(-pid, SIGKILL)`, which reaches
only processes whose pgid still equals the leader pid.

Claude Code's CLI Bash tool spawns its shell with Node `detached: true`, which calls
`setsid()`. Those children get their own session and their own process group, so the
group kill provably cannot reach them. There is no process-tree walk anywhere in the
codebase.

`internal/agent/reap_unix_test.go` already documents this escape: its "escaped" helper
calls `syscall.Setsid()`, and the test asserts only that the pipe closes, then SIGKILLs
the escapee by hand in `t.Cleanup`. The bug is encoded as expected behavior.

## Scope

Three gated increments. This spec covers branch 1.

- **Branch 1 (this spec)** — transitive reap, descendant tracking, startup recovery,
  and the AGENTS.md correction.
- **Branch 2** — enforced per-step timeout (`step_timeout`), unlimited when unset.
- **Branch 3** — worktree-removal guard, and routing `stepCmd` through
  `ConfigureShellCommand`.

Branch 1 ships first so every later gate run on the box already benefits from the
fixed reaper.

## Design

### `internal/proctree`

A new package with one purpose: enumerate and kill process trees. It knows nothing
about runs, steps, or config.

```
Snapshot() ([]Proc, error)
Descendants(snap []Proc, leaderPID int) []Proc
Kill(procs []Proc)
KillGroups(groups []int, recorded []Proc)
```

`Snapshot` shells out to `ps -Ao pid=,ppid=,pgid=,lstart=`. Shelling out is
deliberate: `internal/daemon/proc_unix.go` already establishes this convention
(`psExecutable()`, `LC_ALL=C`) precisely so one implementation covers macOS and Linux.
`lstart` is last in the format because it contains spaces.

Whitespace is normalized before the start time is parsed. The existing
`parseProcessStartTime` layout `"Mon Jan 2 15:04:05 2006"` does not tolerate the
double space macOS emits for single-digit days.

`Descendants` walks `ppid` links transitively from the leader, and additionally
includes any process sharing the leader's pgid.

`Kill` re-reads each pid's start time and skips any mismatch. Between snapshot and
signal a pid can be recycled, and killing a recycled pid is exactly the failure mode
this change exists to avoid. It hard-refuses pid <= 1, the current pid, and the current
process's ancestors.

`KillGroups` applies the same identity rule to the higher-blast-radius group kill. It
re-reads each sampled group leader's start time before signalling the group. A group
whose leader was not sampled, has exited, or has been replaced by a recycled pid fails
closed; the start-time-guarded per-pid `Kill` still covers sampled members. The
unguarded group-kill primitive is package-private, so live teardown and startup
recovery share the guarded API.

These re-reads use targeted `ps -p <pids>` lookups, not a full listing. During
implementation a full `ps -A` on the reap path regressed
`TestDefaultShellCommandOutput_TimesOut` from milliseconds to 592ms, because the reap
runs on every command teardown including short git subprocesses, and a listing of the
~1000 processes on the box costs tens of milliseconds (hundreds under `-race`). Paying
that per command would have been a self-inflicted version of the slowdown this change
exists to fix. With nothing to kill, which is the overwhelmingly common case, the kill
helpers cost nothing at all.

The protected-pid set needs a full listing to walk ancestry, so it is computed once per
process and cached rather than per reap.

### Why a snapshot alone is insufficient

There are two reap paths and they differ in a way that matters:

- `cmd.Cancel` runs while the leader is **alive**. Snapshot-then-kill works because
  ppid links are intact.
- `TerminateShellCommandGroup` runs after `cmd.Wait` returned, so the leader is
  **already dead** and its children have reparented to launchd or init. A snapshot
  taken at that point has no trail back to the step.

The motivating incident exited 0, so it took the second path. Periodic descendant
tracking is therefore the primary mechanism, not a supplement.

This is why the reap path ends up running no process listing of its own at all: the
poller owns snapshots, and a listing taken at reap time is both expensive and, on the
path that matters, useless. The consequence is that a leader cancelled before its first
sample has an empty union, which is why the first sample is taken after 1s rather than
after a full tick.

### Tracker

`StartShellCommand` registers each started leader. `TerminateShellCommandGroup`
deregisters it and reaps the accumulated union. Wiring it at those two points covers
every existing call site with no edits, which matters because there are many and
missing one silently reintroduces the leak. Package-level state matches the existing
`shellCommandJobs` `sync.Map` idiom in `shell_command_windows.go`.

A single background ticker takes one global snapshot per tick regardless of how many
leaders are registered, and attributes descendants to each. Default tick is 15s.

Each tick records, per leader, the descendant pids with their start times **and every
distinct pgid observed**. Reap uses both: the pid union catches anything a poll saw,
and killing each verified observed pgid catches processes spawned after the last poll
by a still-living tracked ancestor. Before a group kill, the reaper confirms that the
sampled group leader still has the same start time. A `setsid()` child gets its own
pgid, so it enters the pgid set and descendant union the first time it is sampled; its
own later children are covered as a group. If the group leader was never sampled or
its pid was recycled, the group kill is skipped and the recorded members are still
handled individually.

Residual gap: a process that both spawns and loses its parent within a single tick
window is still missed. Nothing short of ptrace or a PID namespace closes that. The
incident's `xctest` processes lived about 16 minutes, so they would be sampled roughly
60 times.

### Persistence and startup recovery

The tracker writes `<NM_HOME>/proctrees/<leader>.json` holding the leader pid and start
time, the descendant set, and the observed groups. `shellenv` exposes
`SetProcessRecordDir(dir)`, called once by the daemon. Unset is a no-op, so the CLI and
tests do not touch disk.

The record carries process identity only. It does not carry the worktree path or run id
that the original sketch called for: `shellenv` has no access to run context, and
nothing in this branch needs them. Branch 3's worktree-removal guard does, and will add
them at a layer that knows.

`recoverOnStartup` sweeps that directory alongside the existing `reapOrphanedServers`,
reusing the per-pid and group-leader start-time guards and the `otherDaemonAlive` skip
so a second daemon never reaps a live daemon's trees.

## Testing

The failing-first test: a leader spawns a child that calls `syscall.Setsid()` and
outlives it. The child's pgid is its own pid, so `syscall.Kill(-leader, SIGKILL)`
cannot reach it. This fails against current `main` and passes after the fix.

A second test covers the tracker path: the leader exits 0 first, so the escapee is
already reparented and unreachable from a post-mortem snapshot. Ablating the tracked
union was verified to make it fail, confirming the union rather than the walk is what
carries that case.

Both tests wait for the poller to have actually sampled the escapee before asserting.
Whether a sample landed before the reap is the difference between the guarantee under
test and the documented residual gap, and under `-race` a `ps` over a thousand
processes is slow enough to lose that race by accident.

`TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder` pins sampling off, so
the escaped pipe holder survives the reap. That keeps the test proving what it was
written to prove - that WaitDelay alone bounds `Wait` - which matters because the reaper
is best-effort and WaitDelay is the backstop when it fails.

`internal/agent/reap_unix_test.go` is extended rather than rewritten. The plan had been
to rewrite it, on the reading that its "escaped" case asserted the opposite of the
desired behavior. On inspection that case is the agent-layer twin of
`TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder` above: its leader
exits inside the first sampling interval, so nothing was ever sampled and WaitDelay is
genuinely the only thing under test. Rewriting it would have deleted backstop coverage
by the same reasoning that keeps the shellenv one. It keeps its behavior and gains a
comment saying why the escapee survives there.

What the file was actually missing is a reap assertion of its own. Every existing reap
test in it uses a grandchild that stays in the leader's group, so all of them still pass
when `startNativeAgentCommand` is reverted from `shellenv.StartShellCommand` to a bare
`cmd.Start()` - verified by ablation. That swap drops the leader from the tracker and
makes every `setsid()` escapee unreachable, with no failing test and no symptom short of
an orphan at PPID 1 burning a core. `TestNativeAgentCommand_TerminateReapsSetsidEscapeeAfterSampling`
pins that wiring: it holds the leader alive past the first sample, then requires the
escapee to be gone after `terminate()`.

## Documentation

`AGENTS.md` claims `ConfigureShellCommand` "creates a process-tree boundary and
installs `cmd.Cancel` to kill the whole tree". It creates a process-*group* boundary,
which is strictly weaker. That line is corrected in this branch regardless of which
code lands. The doc comments on `ConfigureShellCommand` and
`TerminateShellCommandGroup` make the same overclaim and are corrected with it.

## Rejected alternatives

- **Native process enumeration** (Linux `/proc`, macOS `sysctl KERN_PROC_ALL`). Avoids
  a subprocess spawn per tick and is faster, but requires two platform
  implementations. Rejected for consistency with the existing `ps` convention; revisit
  if tick cost ever shows up in profiles.
- **`Setsid` instead of `Setpgid` for the leader.** POSIX has no way to signal a
  session as a unit, so this does not help.
- **PID namespaces.** Linux only, and the primary platform here is macOS.

## Bootstrapping risk

This branch changes the reaper that the gate runs for branches 2 and 3 depend on.
After each `nm-smart-run`, verify the gate terminated cleanly and left no strays
(`ps aux | grep -c xctest`, plus a sweep for stray `no-mistakes` children).
