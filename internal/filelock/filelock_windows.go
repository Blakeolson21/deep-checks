//go:build windows

package filelock

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset keeps the locked byte range away from any file content so a
// reader mapping the file is never blocked by the lock itself.
const lockOffset = 0xFFFFFFFF

func lockFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{Offset: lockOffset},
	)
}

func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&windows.Overlapped{Offset: lockOffset},
	)
}
