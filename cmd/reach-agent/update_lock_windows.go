//go:build windows

package main

import (
	"errors"
	"os"
)

// Self-update is Linux/macOS only (canSelfUpdate); there is no lock here.
func lockFile(*os.File) error   { return errors.New("agent self-update is not supported on Windows") }
func unlockFile(*os.File) error { return nil }
