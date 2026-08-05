//go:build !unix

package proctree

import (
	"errors"
	"syscall"
	"time"
)

// errUnsupported keeps the package buildable on non-unix platforms. Windows
// gets whole-tree cleanup from the kill-on-close job object in
// internal/shellenv/shell_command_windows.go, which is a stronger guarantee than
// this package provides, so there is nothing to reimplement here.
var errUnsupported = errors.New("proctree: process enumeration is unix-only")

func snapshot() ([]Proc, error) { return nil, errUnsupported }

func startTimes([]int) (map[int]time.Time, error) { return nil, errUnsupported }

func killProcess(int, syscall.Signal) error { return errUnsupported }

// killGroup is a no-op outside unix.
func killGroup(int) {}

// processGroup has no meaning outside unix; 0 is never a kill candidate.
func processGroup() int { return 0 }
