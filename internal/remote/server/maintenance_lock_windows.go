//go:build windows

package server

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockMaintenanceFile(lock *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(lock.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockMaintenanceFile(lock *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, &overlapped)
}
