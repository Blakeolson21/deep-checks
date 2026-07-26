//go:build !unix

package proctree

import (
	"errors"
	"syscall"
)

// errUnsupported keeps the package buildable on non-unix platforms. Windows
// gets whole-tree cleanup from the kill-on-close job object in
// internal/shellenv/shell_command_windows.go, which is a stronger guarantee than
// this package provides, so there is nothing to reimplement here.
var errUnsupported = errors.New("proctree: process enumeration is unix-only")

func snapshot() ([]Proc, error) { return nil, errUnsupported }

func killProcess(int, syscall.Signal) error { return errUnsupported }

// KillGroup is a no-op outside unix.
func KillGroup(int) {}
