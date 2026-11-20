//go:build windows

package main

import (
	"errors"
	"os"
)

// Self-update is Linux/macOS only (canSelfUpdate); there is no lock here.
func lockFile(*os.File) error                    { return errors.New("agent self-update is not supported on Windows") }
func unlockFile(*os.File) error                  { return nil }
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
func openLockNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
func checkOwnedByMe(os.FileInfo) error { return nil }
