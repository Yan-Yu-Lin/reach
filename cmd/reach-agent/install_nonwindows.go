//go:build !windows

package main

import (
	"context"
)

func windowsIsElevated() bool                                             { return false }
func applyWindowsModeDefaults(opt *installOptions)                        {}
func windowsCurrentUsername() string                                      { return "" }
func promptDefaultWindows(label, def string, secret bool) (string, error) { return def, nil }
func windowsUserHome(user string) (string, error)                         { return "", nil }
func prepareWindowsLocalSSH(ctx context.Context, opt installOptions, targetHome string, compat SSHCompatConfig) (localSSHPlan, error) {
	return localSSHPlan{}, nil
}
func installWindowsScheduledTask(ctx context.Context, opt installOptions, cfgPath string) error {
	return nil
}
func installWindowsUserPersistence(ctx context.Context, opt installOptions, cfgPath string) (string, error) {
	return "", nil
}
func ensureWindowsOpenSSHServer(ctx context.Context, allowInstall bool) error { return nil }
func stopWindowsPersistence(ctx context.Context, opt installOptions)          {}
func (d *Daemon) startWindowsLocalSSH(ctx context.Context) error              { return nil }
func windowsDistroString() string                                             { return "" }

// applyAuthorizedKeysPermissions is a no-op on Unix: writeAuthorizedKeysFile
// sets mode and owner on the exact inodes it creates. The old recursive
// chown of the .ssh directory followed user-planted symlinks (daybreak P2-3).
func applyAuthorizedKeysPermissions(authFile, user string, chownFiles bool) error {
	return nil
}
