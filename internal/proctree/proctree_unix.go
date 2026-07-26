//go:build unix

package proctree

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// snapshot enumerates processes with ps rather than /proc or sysctl.
//
// This follows the convention already set by internal/daemon/proc_unix.go: one
// ps-based implementation covers macOS and Linux, where native enumeration would
// need two platform-specific implementations. LC_ALL/LANG are pinned to C so the
// lstart month and weekday names stay parseable under any locale.
func snapshot() ([]Proc, error) {
	cmd := exec.Command(psExecutable(), "-Ao", "pid=,ppid=,pgid=,lstart=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parseSnapshot(string(out), time.Local), nil
}

// startTimes looks up the start time of specific pids, so verifying a handful of
// kill candidates does not require listing every process on the box.
//
// A pid that has already exited is simply absent from the result; ps exits
// nonzero when none of the requested pids exist, which is a normal outcome here
// rather than a failure.
func startTimes(pids []int) (map[int]time.Time, error) {
	if len(pids) == 0 {
		return map[int]time.Time{}, nil
	}
	list := make([]string, 0, len(pids))
	for _, pid := range pids {
		list = append(list, strconv.Itoa(pid))
	}
	cmd := exec.Command(psExecutable(), "-p", strings.Join(list, ","), "-o", "pid=,lstart=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, fmt.Errorf("read process start times: %w", err)
		}
		// Nonzero exit with no matching pids: everything already exited.
	}
	result := make(map[int]time.Time, len(pids))
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
		result[pid] = started
	}
	return result, nil
}

func killProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// KillGroup SIGKILLs a whole process group. It complements Kill: the pid set
// from a snapshot is only as fresh as the snapshot, whereas a group kill also
// reaches processes spawned since then by a still-living member of that group.
//
// ESRCH ("no such group") is the expected, benign outcome when the group already
// drained, so no error is reported.
func KillGroup(pgid int) {
	if pgid <= 1 || pgid == os.Getpid() {
		return
	}
	_ = killFunc(-pgid, syscall.SIGKILL)
}

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
