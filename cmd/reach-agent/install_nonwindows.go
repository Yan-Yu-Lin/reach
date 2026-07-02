//go:build !windows

package main

import (
	"context"
	"os/exec"
	"path/filepath"
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
func ensureWindowsOpenSSHServer(ctx context.Context) error       { return nil }
func stopWindowsPersistence(ctx context.Context)                 {}
func (d *Daemon) startWindowsLocalSSH(ctx context.Context) error { return nil }
func windowsDistroString() string                                { return "" }

func applyAuthorizedKeysPermissions(authFile, user string, chownFiles bool) error {
	if chownFiles {
		_ = exec.Command("chown", "-R", user+":"+user, filepath.Dir(authFile)).Run()
	}
	return nil
}
