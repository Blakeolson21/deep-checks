//go:build unix

package proctree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// psTimeout and psWaitDelay bound every ps this package runs. They are variables
// so tests can shorten them.
//
// The bound is not optional here. Kill and KillGroups run from reapProcessTree,
// which is installed as cmd.Cancel, and os/exec calls Cancel synchronously and
// only arms cmd.WaitDelay once it returns. A ps that never returns would
// therefore hang cancellation and simultaneously disable the WaitDelay pipe
// backstop the harness relies on, turning a leaked grandchild into a wedged
// step. Nothing above bounds it either: the executor's context descends from
// context.Background().
//
// psWaitDelay is the second half of the same guarantee. Cancelling the context
// SIGKILLs ps, but a process wedged in an uninterruptible read cannot take that
// signal, and cmd.Wait would keep blocking on the inherited pipe; a nonzero
// WaitDelay lets the exec package close it and return.
var (
	psTimeout   = 10 * time.Second
	psWaitDelay = 2 * time.Second
)

// psPath resolves the ps binary. It is a variable so a test can point the bound
// at a stand-in that never returns.
var psPath = psExecutable

// runPS executes ps under psTimeout with the locale pinned to C so the lstart
// month and weekday names stay parseable.
//
// A timeout is reported as its own error rather than passed through, because
// killing ps produces an ExitError that callers legitimately read as "none of
// the requested pids exist". Silently reading a wedge as an empty process table
// would make every guard downstream fail open.
func runPS(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psPath(), args...)
	cmd.WaitDelay = psWaitDelay
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, fmt.Errorf("ps did not return within %s: %w", psTimeout, ctxErr)
	}
	return out, err
}

// snapshot enumerates processes with ps rather than /proc or sysctl.
//
// This follows the convention already set by internal/daemon/proc_unix.go: one
// ps-based implementation covers macOS and Linux, where native enumeration would
// need two platform-specific implementations.
func snapshot() ([]Proc, error) {
	out, err := runPS("-Ao", "pid=,ppid=,pgid=,lstart=")
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parseSnapshot(string(out), time.Local), nil
}

// startTimesBatch caps how many pids ride in one ps invocation.
//
// The whole list is a single argv entry, and the kernel caps one entry at
// MAX_ARG_STRLEN (128 KiB on Linux). Past that execve fails outright, startTimes
// returns an error, and every caller fails closed - so an unbatched lookup would
// stop verifying, and therefore stop reaping, exactly on the long-running steps
// that accumulate the most descendants. Batching keeps each argv entry a few
// kilobytes regardless of how large the recorded union grew.
const startTimesBatch = 1024

// startTimes looks up the start time of specific pids, so verifying a handful of
// kill candidates does not require listing every process on the box.
//
// A pid that has already exited is simply absent from the result; ps exits
// nonzero when none of the requested pids exist, which is a normal outcome here
// rather than a failure.
func startTimes(pids []int) (map[int]time.Time, error) {
	result := make(map[int]time.Time, len(pids))
	for start := 0; start < len(pids); start += startTimesBatch {
		end := min(start+startTimesBatch, len(pids))
		if err := startTimesBatchInto(pids[start:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func startTimesBatchInto(pids []int, into map[int]time.Time) error {
	if len(pids) == 0 {
		return nil
	}
	list := make([]string, 0, len(pids))
	for _, pid := range pids {
		list = append(list, strconv.Itoa(pid))
	}
	out, err := runPS("-p", strings.Join(list, ","), "-o", "pid=,lstart=")
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return fmt.Errorf("read process start times: %w", err)
		}
		// Nonzero exit with no matching pids: everything already exited.
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, convErr := strconv.Atoi(fields[0])
		if convErr != nil || pid <= 0 {
			continue
		}
		started, parseErr := time.ParseInLocation(startTimeLayout, strings.Join(fields[1:], " "), time.Local)
		if parseErr != nil {
			continue
		}
		into[pid] = started
	}
	return nil
}

func killProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// killGroup SIGKILLs a whole process group. It complements Kill: the pid set
// from a sample is only as fresh as that sample, whereas a group kill also
// reaches processes spawned since then by a still-living member of that group.
//
// It is deliberately unexported. A group kill has the widest blast radius of
// anything here, and it is only safe once the group leader's identity has been
// re-verified, so the sole way to reach it is through KillGroups, which performs
// that check. Leaving an unguarded version exported would be a footgun that a
// future call site could reach for without knowing the rule.
//
// It consults the same protected set as the per-pid kill - pid 0 and 1, this
// process, its ancestors, and its own process group - because the blast radius
// argument runs the other way here: signalling our own group takes down the
// daemon and the service supervisor with it, not just one process.
//
// ESRCH ("no such group") is the expected, benign outcome when the group already
// drained, so no error is reported.
func killGroup(pgid int) {
	if pgid <= 1 || protectedPIDs()[pgid] {
		return
	}
	_ = killFunc(-pgid, syscall.SIGKILL)
}

// processGroup reports this process's own process-group id so protectedPIDs can
// refuse to signal it.
func processGroup() int { return syscall.Getpgrp() }

func psExecutable() string {
	if path, err := exec.LookPath("ps"); err == nil {
		return path
	}
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}
