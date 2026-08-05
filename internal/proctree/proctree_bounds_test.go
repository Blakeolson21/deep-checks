//go:build unix

package proctree

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSnapshot_FailsWhenPSDoesNotReturnWithinTheBound pins the ceiling on the
// package's own subprocesses.
//
// Kill and KillGroups run from reapProcessTree, which is installed as
// cmd.Cancel, and os/exec calls Cancel synchronously and arms cmd.WaitDelay only
// once it returns. A ps that never returns therefore hangs cancellation and
// disables the WaitDelay pipe backstop at the same time, turning a leaked
// grandchild into a wedged step.
func TestSnapshot_FailsWhenPSDoesNotReturnWithinTheBound(t *testing.T) {
	defer stubWedgedPS(t)()

	done := make(chan error, 1)
	go func() {
		_, err := snapshot()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("snapshot succeeded against a ps that never returns")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot never returned; an unbounded ps inside cmd.Cancel wedges " +
			"cancellation and disables the WaitDelay backstop")
	}
}

// TestStartTimes_WedgedPSIsAnErrorNotAnEmptyResult keeps the bound fail-closed.
// Every kill guard treats an absent pid as "already exited", so reading a wedged
// ps as an empty process table would make the whole package fail open.
func TestStartTimes_WedgedPSIsAnErrorNotAnEmptyResult(t *testing.T) {
	defer stubWedgedPS(t)()

	type result struct {
		times map[int]time.Time
		err   error
	}
	done := make(chan result, 1)
	go func() {
		times, err := startTimes([]int{os.Getpid()})
		done <- result{times, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("startTimes returned %v with no error; a wedged ps must not read "+
				"as 'every pid exited'", got.times)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startTimes never returned against a ps that never returns")
	}
}

// TestStartTimes_HandlesAPIDListLargerThanOneArgvEntry covers the long-running
// step. The pid list rides in a single argv entry, which the kernel caps
// (MAX_ARG_STRLEN is 128 KiB on Linux), so an unbatched lookup stops verifying -
// and therefore stops reaping - exactly on the trees that accumulated the most
// descendants.
func TestStartTimes_HandlesAPIDListLargerThanOneArgvEntry(t *testing.T) {
	self := os.Getpid()
	// Every pid stays inside the range macOS ps accepts (it rejects anything
	// above its own pid ceiling outright), while the joined list still runs to
	// several hundred kilobytes - well past the Linux per-entry cap.
	pids := make([]int, 0, 60_001)
	pids = append(pids, self)
	for pid := 1; pid <= 60_000; pid++ {
		pids = append(pids, pid)
	}

	got, err := startTimes(pids)
	if err != nil {
		t.Fatalf("startTimes over %d pids: %v", len(pids), err)
	}
	if _, ok := got[self]; !ok {
		t.Fatalf("startTimes over %d pids lost this process (%d); "+
			"verification would fail closed and nothing would be reaped", len(pids), self)
	}
}

// TestComputeProtectedPIDs_IncludesOwnProcessGroup guards the group kill's blast
// radius: signalling our own group takes down the daemon and its supervisor
// together, not one process.
func TestComputeProtectedPIDs_IncludesOwnProcessGroup(t *testing.T) {
	prev := snapshotFunc
	snapshotFunc = func() ([]Proc, error) { return nil, errors.New("no listing available") }
	defer func() { snapshotFunc = prev }()

	got := computeProtectedPIDs()

	pgrp := syscall.Getpgrp()
	if pgrp > 1 && !got[pgrp] {
		t.Fatalf("own process group %d missing from the protected set %v", pgrp, got)
	}
}

// stubWedgedPS points the package at a ps that never returns and shortens the
// bound so the wedge is observable inside a test.
func stubWedgedPS(t *testing.T) func() {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "ps")
	// exec, not a plain call: runPS bounds its child with exec.CommandContext,
	// which signals only the pid it started. A shell that forks the sleep and
	// waits on it leaves that sleep alive and reparented to pid 1 when the bound
	// kills the shell, so this helper would leak a real orphan per run - the
	// exact leak class this package exists to close. exec makes the stub one
	// process, which is also what a genuinely wedged ps is.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 120\n"), 0o755); err != nil {
		t.Fatalf("write ps stub: %v", err)
	}
	prevPath, prevTimeout, prevDelay := psPath, psTimeout, psWaitDelay
	psPath = func() string { return stub }
	psTimeout, psWaitDelay = 200*time.Millisecond, 100*time.Millisecond
	return func() {
		psPath, psTimeout, psWaitDelay = prevPath, prevTimeout, prevDelay
	}
}
