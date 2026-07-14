//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package server

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockMaintenanceFile(lock *os.File) (bool, error) {
	err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockMaintenanceFile(lock *os.File) error {
	return unix.Flock(int(lock.Fd()), unix.LOCK_UN)
}
