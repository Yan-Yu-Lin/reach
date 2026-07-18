//go:build darwin

package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func durableSync(file *os.File) error {
	if _, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return file.Sync()
}
