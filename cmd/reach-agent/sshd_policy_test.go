package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSvc struct {
	hasSystemctl, hasService bool
	enabled                  map[string]bool // systemd is-enabled
	sysv                     map[string]bool // SysV boot links
	calls                    []string
}

func (f *fakeSvc) probe() unixSSHDProbe {
	return unixSSHDProbe{
		lookPath: func(name string) (string, error) {
			if (name == "systemctl" && f.hasSystemctl) || (name == "service" && f.hasService) {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, call)
			if name == "systemctl" && len(args) > 0 && args[0] == "is-enabled" {
				if f.enabled[args[len(args)-1]] {
					return nil, nil
				}
				return []byte("disabled"), errors.New("exit 1")
			}
			return nil, nil
		},
		sysvLinks: func(svc string) bool { return f.sysv[svc] },
	}
}

func (f *fakeSvc) started() []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, " start") || strings.HasPrefix(c, "systemctl enable") {
			out = append(out, c)
		}
	}
	return out
}

func noLog(string, ...any) {}

// Daybreak round 3 P1-D PoC: systemd says disabled; the daemon must not
// fall through to "service sshd start".
func TestDisabledSystemdSSHDIsNotStartedThroughService(t *testing.T) {
	// A leftover SysV S-link must not matter once systemd says disabled.
	f := &fakeSvc{hasSystemctl: true, hasService: true, enabled: map[string]bool{}, sysv: map[string]bool{"ssh": true, "sshd": true}}
	err := startUnixSSHDService(context.Background(), f.probe(), []string{"ssh", "sshd"}, noLog)
	if !errors.Is(err, errSSHDNotPermitted) {
		t.Fatalf("err = %v", err)
	}
	if s := f.started(); len(s) != 0 {
		t.Fatalf("disabled sshd was started: %v", s)
	}
}

func TestEnabledSystemdSSHDIsRestarted(t *testing.T) {
	f := &fakeSvc{hasSystemctl: true, enabled: map[string]bool{"sshd": true}}
	if err := startUnixSSHDService(context.Background(), f.probe(), []string{"ssh", "sshd"}, noLog); err != nil {
		t.Fatal(err)
	}
	if s := f.started(); len(s) != 1 || s[0] != "systemctl start sshd" {
		t.Fatalf("calls = %v", s)
	}
}

func TestSysVOnlyStartsBootEnabledScripts(t *testing.T) {
	f := &fakeSvc{hasService: true, sysv: map[string]bool{}}
	if err := startUnixSSHDService(context.Background(), f.probe(), []string{"ssh", "sshd"}, noLog); !errors.Is(err, errSSHDNotPermitted) {
		t.Fatalf("err = %v", err)
	}
	if s := f.started(); len(s) != 0 {
		t.Fatalf("non-boot SysV sshd started: %v", s)
	}
	f = &fakeSvc{hasService: true, sysv: map[string]bool{"sshd": true}}
	if err := startUnixSSHDService(context.Background(), f.probe(), []string{"ssh", "sshd"}, noLog); err != nil {
		t.Fatal(err)
	}
	if s := f.started(); len(s) != 1 || s[0] != "service sshd start" {
		t.Fatalf("calls = %v", s)
	}
}

func TestDecideSSHDAction(t *testing.T) {
	cases := []struct {
		st    sshdServiceState
		optIn bool
		want  sshdAction
	}{
		{sshdServiceState{Manager: "windows", Name: "sshd", BootEnabled: true, StartupType: "Automatic"}, false, sshdStartOnly},
		{sshdServiceState{Manager: "windows", Name: "sshd", StartupType: "Manual"}, false, sshdLeaveAlone},
		{sshdServiceState{Manager: "windows", Name: "sshd", StartupType: "Disabled"}, false, sshdLeaveAlone},
		{sshdServiceState{Manager: "windows", Name: "sshd", StartupType: "Disabled"}, true, sshdEnableAndStart},
		{sshdServiceState{Manager: "systemd", Name: "ssh"}, false, sshdLeaveAlone},
	}
	for _, c := range cases {
		if got, _ := decideSSHDAction(c.st, c.optIn); got != c.want {
			t.Errorf("%+v optIn=%v: got %v want %v", c.st, c.optIn, got, c.want)
		}
	}
}

// The Windows script must refuse a non-Automatic sshd without opt-in and
// only call Set-Service when the operator opted in.
func TestWindowsSSHDScriptPolicy(t *testing.T) {
	off := windowsSSHDScript(false)
	if !strings.Contains(off, "$optIn = $false") || !strings.Contains(off, "-ne 'Auto' -and -not $optIn") {
		t.Fatalf("no-opt-in script does not refuse a disabled sshd:\n%s", off)
	}
	// Set-Service only runs after the refusal check.
	if strings.Index(off, "throw \"Windows sshd StartupType") > strings.Index(off, "Set-Service") {
		t.Fatal("Set-Service precedes the policy check")
	}
	if !strings.Contains(windowsSSHDScript(true), "$optIn = $true") {
		t.Fatal("opt-in not passed")
	}
}

func TestDarwinRemoteLoginEnabled(t *testing.T) {
	cases := map[string]bool{
		"disabled services = {\n\t\"com.openssh.sshd\" => disabled\n}": false,
		"disabled services = {\n\t\"com.openssh.sshd\" => enabled\n}":  true,
		"disabled services = {\n\t\"com.openssh.sshd\" => false\n}":    true,
		"disabled services = {\n}":                                     false,
	}
	for in, want := range cases {
		if got := darwinRemoteLoginEnabled(in); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}
