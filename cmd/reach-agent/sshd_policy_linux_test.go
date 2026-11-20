//go:build linux

package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end version of the daybreak round 3 P1-D PoC on Linux: the real
// Daemon.startLocalSSH with fake systemctl/service binaries on PATH. A
// disabled systemd sshd must not be started through "service".
func TestDaemonDoesNotStartDisabledSystemdSSHD(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> "+logFile+"\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("systemctl", `case "$1" in is-enabled) exit 1 ;; esac; exit 0`+"\n")
	write("service", "exit 0\n")
	t.Setenv("PATH", dir)
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.cfg.LocalSSH.ServiceNames = []string{"ssh", "sshd"}
	if err := d.startLocalSSH(context.Background()); err == nil {
		t.Fatal("startLocalSSH succeeded with a disabled sshd")
	}
	b, _ := os.ReadFile(logFile)
	calls := string(b)
	if strings.Contains(calls, "service ") || strings.Contains(calls, "systemctl start") || strings.Contains(calls, "systemctl enable") {
		t.Fatalf("disabled sshd was started:\n%s", calls)
	}
}
