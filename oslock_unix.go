//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

func tryFlock(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func flockBlocking(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func unflock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// copyOwner gives f the owner and group of st (used when replacing a
// root-owned config file in place).
func copyOwner(f *os.File, st os.FileInfo) error {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return f.Chown(int(sys.Uid), int(sys.Gid))
	}
	return nil
}
