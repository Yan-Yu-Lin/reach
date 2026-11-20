package main

import (
	"reflect"
	"testing"
)

func TestParseWideListeners(t *testing.T) {
	ss := []byte("LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\nLISTEN 0 128 127.0.0.1:22 0.0.0.0:*\nLISTEN 0 128 [::]:22 [::]:*\nLISTEN 0 128 [::1]:22 [::]:*\nLISTEN 0 128 0.0.0.0:2222 0.0.0.0:*\n")
	got := parseWideListeners(ss)
	if !reflect.DeepEqual(got, []string{"0.0.0.0:22", "[::]:22"}) {
		t.Fatalf("ss parse = %v", got)
	}
	lsof := []byte("COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\nlaunchd 1 root 3u IPv6 0x1 0t0 TCP *:22 (LISTEN)\nsshd 9 root 4u IPv4 0x2 0t0 TCP 127.0.0.1:22 (LISTEN)\n")
	if got := parseWideListeners(lsof); !reflect.DeepEqual(got, []string{"*:22"}) {
		t.Fatalf("lsof parse = %v", got)
	}
	if got := parseWideListeners([]byte("LISTEN 0 128 127.0.0.1:22 0.0.0.0:*\n")); len(got) != 0 {
		t.Fatalf("loopback-only flagged: %v", got)
	}
}

// Daybreak P1-5: a system install with no sshd must use the loopback
// internal server running sessions as the target user, never root.
func TestSystemInstallInternalSSHDRunsAsTargetUser(t *testing.T) {
	if got := internalSSHDRunAs(installOptions{InstallMode: "system", TargetUser: "alice"}); got != "alice" {
		t.Fatalf("run-as = %q", got)
	}
	if got := internalSSHDRunAs(installOptions{InstallMode: "user", TargetUser: "alice"}); got != "" {
		t.Fatalf("user install run-as = %q", got)
	}
}
