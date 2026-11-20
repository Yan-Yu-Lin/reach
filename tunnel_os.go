package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tunnelOS is every root-level operation the control plane performs on a
// tunnel account after it exists. Each method must either finish completely
// or return an error; callers fail closed on error (daybreak P1-2).
//
// The interface is also the RPC surface planned for the separate root helper
// (docs/design/2026-11-21-reachd-privilege-split.md).
type tunnelOS interface {
	// DisableAuth stops new logins and terminates existing sessions.
	DisableAuth(ctx context.Context, unixUser string) error
	// EnableAuth restores a disabled authorized_keys file.
	EnableAuth(ctx context.Context, unixUser string) error
	// Remove deletes the account, home, sshd drop-in and processes, then
	// validates and reloads sshd.
	Remove(ctx context.Context, unixUser string) error
	// Residue reports anything still present for unixUser. An empty result
	// with a nil error is the only proof that cleanup is complete.
	Residue(ctx context.Context, unixUser string) ([]string, error)
	// DropInPorts returns the ports named by PermitListen in existing
	// Reach drop-ins so the allocator never hands out a port an old account
	// may still bind.
	DropInPorts() (map[int]bool, error)
}

// noopTunnelOS is used when provisioning_enabled is false (development).
type noopTunnelOS struct{}

func (noopTunnelOS) DisableAuth(context.Context, string) error         { return nil }
func (noopTunnelOS) EnableAuth(context.Context, string) error          { return nil }
func (noopTunnelOS) Remove(context.Context, string) error              { return nil }
func (noopTunnelOS) Residue(context.Context, string) ([]string, error) { return nil, nil }
func (noopTunnelOS) DropInPorts() (map[int]bool, error)                { return map[int]bool{}, nil }

type systemTunnelOS struct{ cfg Config }

func (o systemTunnelOS) homeOf(unixUser string) (string, error) {
	home := filepath.Join(o.cfg.TunnelHomeBase, unixUser)
	if err := validateHomePath(home); err != nil {
		return "", err
	}
	return home, nil
}

func (o systemTunnelOS) dropInOf(unixUser string) string {
	return filepath.Join(o.cfg.SSHDConfigDir, "90-"+unixUser+".conf")
}

func (o systemTunnelOS) DisableAuth(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	home, err := o.homeOf(unixUser)
	if err != nil {
		return err
	}
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	if _, err := os.Lstat(ak); err == nil {
		if err := os.Rename(ak, ak+".disabled"); err != nil {
			return fmt.Errorf("disable authorized_keys: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat authorized_keys: %w", err)
	}
	if _, err := os.Lstat(ak); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("authorized_keys still present after disable")
	}
	return killUser(ctx, unixUser)
}

func (o systemTunnelOS) EnableAuth(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	home, err := o.homeOf(unixUser)
	if err != nil {
		return err
	}
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	if _, err := os.Lstat(ak); err == nil {
		return nil
	}
	if err := os.Rename(ak+".disabled", ak); err != nil {
		return fmt.Errorf("enable authorized_keys: %w", err)
	}
	return nil
}

func (o systemTunnelOS) Remove(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	var errs []string
	if err := killUser(ctx, unixUser); err != nil {
		errs = append(errs, err.Error())
	}
	if err := run(ctx, "userdel", unixUser); err != nil && !strings.Contains(err.Error(), "does not exist") {
		errs = append(errs, err.Error())
	}
	if err := os.Remove(o.dropInOf(unixUser)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err.Error())
	}
	if home, err := o.homeOf(unixUser); err != nil {
		errs = append(errs, err.Error())
	} else if err := os.RemoveAll(home); err != nil {
		errs = append(errs, err.Error())
	}
	if err := run(ctx, "sshd", "-t"); err != nil {
		errs = append(errs, "sshd -t: "+err.Error())
	} else if err := run(ctx, "systemctl", "reload", o.cfg.SSHServiceName); err != nil {
		errs = append(errs, "reload ssh: "+err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (o systemTunnelOS) Residue(ctx context.Context, unixUser string) ([]string, error) {
	if err := ValidateUnixUser(unixUser); err != nil {
		return nil, err
	}
	var residue []string
	userExists := false
	if _, err := user.Lookup(unixUser); err == nil {
		userExists = true
		residue = append(residue, "unix user exists")
	} else {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			return nil, fmt.Errorf("lookup %s: %w", unixUser, err)
		}
	}
	home, err := o.homeOf(unixUser)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(home); err == nil {
		residue = append(residue, "home directory exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Lstat(o.dropInOf(unixUser)); err == nil {
		residue = append(residue, "sshd drop-in exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if userExists {
		alive, err := userHasProcesses(ctx, unixUser)
		if err != nil {
			return nil, err
		}
		if alive {
			residue = append(residue, "processes still running")
		}
	}
	return residue, nil
}

var permitListenRE = regexp.MustCompile(`(?m)^\s*PermitListen\s+127\.0\.0\.1:(\d+)\s*$`)

func (o systemTunnelOS) DropInPorts() (map[int]bool, error) {
	out := map[int]bool{}
	matches, err := filepath.Glob(filepath.Join(o.cfg.SSHDConfigDir, "90-rt-*.conf"))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		for _, sub := range permitListenRE.FindAllStringSubmatch(string(b), -1) {
			if p, err := strconv.Atoi(sub[1]); err == nil {
				out[p] = true
			}
		}
	}
	return out, nil
}

// userHasProcesses uses pgrep's exit status: 0 = found, 1 = none.
func userHasProcesses(ctx context.Context, unixUser string) (bool, error) {
	err := exec.CommandContext(ctx, "pgrep", "-u", unixUser).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("pgrep -u %s: %w", unixUser, err)
}

// killUser terminates every process of unixUser and fails if any survive.
func killUser(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	if _, err := user.Lookup(unixUser); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil
		}
		return fmt.Errorf("lookup %s: %w", unixUser, err)
	}
	_ = exec.CommandContext(ctx, "pkill", "-TERM", "-u", unixUser).Run()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := userHasProcesses(ctx, unixUser)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = exec.CommandContext(ctx, "pkill", "-KILL", "-u", unixUser).Run()
	for i := 0; i < 8; i++ {
		time.Sleep(250 * time.Millisecond)
		alive, err := userHasProcesses(ctx, unixUser)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
	}
	return fmt.Errorf("processes of %s survived SIGKILL", unixUser)
}
