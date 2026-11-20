//go:build !windows

package main

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Daybreak round 3 P2-E: the machine was active before the update; after
// it the hub claims "disabled". The broken new binary has no tunnel. It
// must not confirm itself (and must not raise the floor).
func TestConfirmGateIgnoresHubDisabledAfterUpdate(t *testing.T) {
	dir, _ := pendingUpdateFixture(t)
	d := NewDaemon(defaultConfig())
	d.logger = log.New(io.Discard, "", 0)
	d.desired.DesiredState = "disabled"
	d.desired.DesiredConfig.TunnelEnabled = false
	d.status.Tunnel.State = "stopped"
	d.status.LocalSSH.State = "down"
	d.confirmPendingUpdate(context.Background(), reachInstallState{DataDir: dir})
	if st, _ := readUpdateState(dir); st.State != updPending {
		t.Fatalf("update confirmed without proof: %s", st.State)
	}
	if f, _ := readReleaseFloor(dir); f != 0 {
		t.Fatalf("floor raised to %d", f)
	}
	// A machine that was already disabled before the update confirms on
	// HTTP alone (it has no tunnel to prove).
	st, _ := readUpdateState(dir)
	st.WasActive = false
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	d.confirmPendingUpdate(context.Background(), reachInstallState{DataDir: dir})
	if st, _ := readUpdateState(dir); st.State != updConfirmed {
		t.Fatalf("disabled-before-update machine not confirmed: %s", st.State)
	}
}

// The login proof really logs in: against a live internal-sshd it succeeds
// and leaves authorized_keys as it was; with the wrong user/port it fails.
func TestLocalSSHLoginProbe(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(auth, []byte(syncGoodKey+" reach:m_probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostKey := filepath.Join(dir, "host_key")
	if err := ensureInternalHostKey(hostKey); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = runInternalSSHD(ctx, internalSSHDOptions{Port: port, AuthFile: auth, HostKeyPath: hostKey, Shell: "/bin/sh"})
	}()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cfg := defaultConfig()
	cfg.MachineID = "m_probe"
	cfg.LocalSSH.AuthFile = auth
	cfg.LocalSSH.InternalSSHD = true
	cfg.LocalSSH.Port = port
	d := NewDaemon(cfg)
	d.logger = log.New(io.Discard, "", 0)
	before, _ := os.ReadFile(auth)
	if err := d.localSSHLoginProbe(ctx); err != nil {
		t.Fatalf("login probe against a working sshd failed: %v", err)
	}
	after, _ := os.ReadFile(auth)
	if string(after) != string(before) {
		t.Fatalf("one-time key left behind:\n%s", after)
	}
	// An sshd that accepts the login but cannot run the command (broken
	// shell) is not proof either.
	broken := filepath.Join(dir, "broken-shell")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\necho broken\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port2 := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()
	go func() {
		_ = runInternalSSHD(ctx, internalSSHDOptions{Port: port2, AuthFile: auth, HostKeyPath: hostKey, Shell: broken})
	}()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port2))); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.cfg.LocalSSH.Port = port2
	if err := d.localSSHLoginProbe(ctx); err == nil {
		t.Fatal("login probe accepted a session whose command did not run")
	}
	d.cfg.LocalSSH.Port = port + 1 // nothing listens there
	if err := d.localSSHLoginProbe(ctx); err == nil {
		t.Fatal("login probe succeeded without an sshd")
	}
	if b, _ := os.ReadFile(auth); strings.Contains(string(b), "update-confirm") {
		t.Fatal("one-time key left behind after a failed probe")
	}
}
