package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Host-wide sshd policy (daybreak P1-5, round 3 P1-D).
//
// Reach may RESTART a host sshd that the administrator already enabled at
// boot. It must never ENABLE one (boot-start or start a disabled service)
// unless the operator opted in at install time (--install-openssh /
// setup.ps1 -InstallOpenSSH). Without that, Reach serves SSH itself on
// loopback (internal-sshd) instead.
//
// One decision function is used by the Linux daemon repair path and the
// Windows installer/daemon, and setup.ps1 mirrors it (see
// Ensure-ReachOpenSSHServer), so they cannot drift again.

// sshdServiceState is what the host says about one sshd service.
type sshdServiceState struct {
	Manager string // "systemd", "sysv", "windows"
	Name    string
	// BootEnabled: systemd is-enabled, a SysV rc*.d S-link, or Windows
	// StartupType Automatic.
	BootEnabled bool
	// StartupType is the Windows start mode as reported (Automatic,
	// Manual, Disabled); empty elsewhere.
	StartupType string
}

type sshdAction int

const (
	sshdLeaveAlone     sshdAction = iota // do nothing (and report why)
	sshdStartOnly                        // start now; boot setting unchanged
	sshdEnableAndStart                   // opt-in only: set boot start and start
)

// decideSSHDAction is the single policy.
func decideSSHDAction(st sshdServiceState, optIn bool) (sshdAction, string) {
	if st.BootEnabled {
		return sshdStartOnly, ""
	}
	if optIn {
		return sshdEnableAndStart, ""
	}
	why := fmt.Sprintf("%s service %s is not enabled at boot", st.Manager, st.Name)
	if st.StartupType != "" {
		why = fmt.Sprintf("Windows service %s has StartupType %s", st.Name, st.StartupType)
	}
	return sshdLeaveAlone, why + "; Reach does not enable a host-wide sshd without --install-openssh"
}

// errSSHDNotPermitted reports that policy forbids starting the host sshd.
var errSSHDNotPermitted = errors.New("host sshd is disabled and Reach may not enable it")

// unixSSHDProbe abstracts the service manager for tests.
type unixSSHDProbe struct {
	lookPath  func(string) (string, error)
	run       func(ctx context.Context, name string, args ...string) ([]byte, error)
	sysvLinks func(svc string) bool
}

func defaultUnixSSHDProbe() unixSSHDProbe {
	return unixSSHDProbe{
		lookPath: exec.LookPath,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		sysvLinks: sysvBootEnabled,
	}
}

// sysvBootEnabled reports whether a SysV init script starts at boot in a
// multi-user runlevel (an S??svc link in rc2.d..rc5.d).
func sysvBootEnabled(svc string) bool {
	for _, dir := range []string{"/etc/rc2.d", "/etc/rc3.d", "/etc/rc4.d", "/etc/rc5.d", "/etc/rc.d/rc2.d", "/etc/rc.d/rc3.d", "/etc/rc.d/rc5.d"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if len(n) > 3 && n[0] == 'S' && strings.TrimLeft(n[1:], "0123456789") == svc {
				return true
			}
		}
	}
	return false
}

// startUnixSSHDService starts the first boot-enabled sshd service. When
// systemd is present only systemd is consulted: a unit that systemd reports
// disabled is never started through the SysV "service" wrapper (round 3
// P1-D). "service" is used only on hosts without systemctl, and only for a
// script that is linked to start at boot.
func startUnixSSHDService(ctx context.Context, p unixSSHDProbe, services []string, logf func(string, ...any)) error {
	var reasons []string
	if _, err := p.lookPath("systemctl"); err == nil {
		for _, svc := range services {
			if svc == "" {
				continue
			}
			_, err := p.run(ctx, "systemctl", "is-enabled", "--quiet", svc)
			st := sshdServiceState{Manager: "systemd", Name: svc, BootEnabled: err == nil}
			action, why := decideSSHDAction(st, false)
			if action == sshdLeaveAlone {
				reasons = append(reasons, why)
				continue
			}
			if out, err := p.run(ctx, "systemctl", "start", svc); err == nil {
				logf("started ssh service %s", svc)
				return nil
			} else {
				reasons = append(reasons, fmt.Sprintf("systemctl start %s: %s", svc, strings.TrimSpace(string(out))))
			}
		}
		return fmt.Errorf("%w: %s", errSSHDNotPermitted, strings.Join(reasons, "; "))
	}
	if _, err := p.lookPath("service"); err != nil {
		return errors.New("no systemctl or service command to start sshd")
	}
	for _, svc := range services {
		if svc == "" {
			continue
		}
		st := sshdServiceState{Manager: "sysv", Name: svc, BootEnabled: p.sysvLinks(svc)}
		action, why := decideSSHDAction(st, false)
		if action == sshdLeaveAlone {
			reasons = append(reasons, why)
			continue
		}
		if out, err := p.run(ctx, "service", svc, "start"); err == nil {
			logf("started ssh service %s", svc)
			return nil
		} else {
			reasons = append(reasons, fmt.Sprintf("service %s start: %s", svc, strings.TrimSpace(string(out))))
		}
	}
	return fmt.Errorf("%w: %s", errSSHDNotPermitted, strings.Join(reasons, "; "))
}

// windowsSSHDScript returns the PowerShell that applies the policy to the
// Windows sshd service. setup.ps1 Ensure-ReachOpenSSHServer does the same.
func windowsSSHDScript(optIn bool) string {
	opt := "$false"
	if optIn {
		opt = "$true"
	}
	return `
$optIn = ` + opt + `
$svc = Get-Service -Name sshd -ErrorAction SilentlyContinue
if (-not $svc) { throw 'sshd service is not installed' }
$mode = (Get-CimInstance Win32_Service -Filter "Name='sshd'").StartMode
if ($mode -ne 'Auto' -and -not $optIn) {
  throw "Windows sshd StartupType is $mode; Reach does not change it without --install-openssh (setup.ps1 -InstallOpenSSH)"
}
if ($mode -ne 'Auto') { Set-Service -Name sshd -StartupType Automatic }
if ((Get-Service -Name sshd).Status -ne 'Running') { Start-Service -Name sshd }
(Get-Service -Name sshd).Status
`
}
