//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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

// openLockFileSafe opens (creating) dir/name without following symlinks at
// either level and checks what it got (daybreak round 5 N7): the lock
// directory must be a real directory owned by us, not group/other
// writable; the lock file a regular file owned by us, mode 0600, with one
// link. A symlink or a swapped inode is refused instead of silently
// giving another process a different lock.
func openLockFileSafe(dir, name string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("lock dir %s: %w", dir, err)
	}
	defer unix.Close(dfd)
	var dst unix.Stat_t
	if err := unix.Fstat(dfd, &dst); err != nil {
		return nil, err
	}
	if int(dst.Uid) != os.Geteuid() || dst.Mode&0o022 != 0 {
		return nil, fmt.Errorf("lock dir %s: owner uid %d mode %o not acceptable", dir, dst.Uid, dst.Mode&0o777)
	}
	fd, err := unix.Openat(dfd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file %s: %w", filepath.Join(dir, name), err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 || st.Nlink != 1 {
		unix.Close(fd)
		return nil, fmt.Errorf("lock file %s is not a private regular file (mode %o, uid %d, links %d)", filepath.Join(dir, name), st.Mode, st.Uid, st.Nlink)
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir, name)), nil
}

// checkPrivateRegularFile: path is a regular file (not a symlink) owned by
// us with one link and no group/other write (config rotation, N7).
func checkPrivateRegularFile(path string) (os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if ok && (int(sys.Uid) != os.Geteuid() || sys.Nlink != 1) {
		return nil, fmt.Errorf("%s: owner uid %d, links %d; expected ours with one link", path, sys.Uid, sys.Nlink)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is group/other writable (mode %o)", path, st.Mode().Perm())
	}
	return st, nil
}
