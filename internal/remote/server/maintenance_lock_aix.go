//go:build aix

package server

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockMaintenanceFile(lock *os.File) (bool, error) {
	fileLock := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: io.SeekStart,
		Start:  0,
		Len:    1,
	}
	err := unix.FcntlFlock(lock.Fd(), unix.F_SETLK, &fileLock)
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockMaintenanceFile(lock *os.File) error {
	fileLock := unix.Flock_t{
		Type:   unix.F_UNLCK,
		Whence: io.SeekStart,
		Start:  0,
		Len:    1,
	}
	return unix.FcntlFlock(lock.Fd(), unix.F_SETLK, &fileLock)
}
