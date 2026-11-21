//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func lockFile(f *os.File) error   { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// openNoFollow opens path read-only and refuses a symlink.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// openLockNoFollow opens (creating) a lock file and refuses a symlink.
func openLockNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

func checkOwnedByMe(st os.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not %d", sys.Uid, os.Geteuid())
	}
	return nil
}

const oNoFollow = syscall.O_NOFOLLOW
