package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"testing"
	"time"

	"reach/internal/wscarrier"

	"golang.org/x/crypto/ssh"
)

func TestParseSSHOptionAndApplyCompat(t *testing.T) {
	key, vals, ok := parseSSHOption("HostKeyAlgorithms=+ssh-rsa,rsa-sha2-256")
	if !ok || key != "HostKeyAlgorithms" || len(vals) != 2 || vals[0] != "ssh-rsa" || vals[1] != "rsa-sha2-256" {
		t.Fatalf("unexpected parse: key=%q vals=%v ok=%v", key, vals, ok)
	}
	cfg := testSSHClientConfig()
	if err := applySSHCompatOptions(cfg, []string{
		"KexAlgorithms=diffie-hellman-group14-sha1",
		"Ciphers=aes128-ctr",
		"MACs=hmac-sha1",
		"HostKeyAlgorithms=ssh-rsa",
	}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Config.KeyExchanges[0]; got != "diffie-hellman-group14-sha1" {
		t.Fatalf("KeyExchanges[0]=%q", got)
	}
	if got := cfg.Config.Ciphers[0]; got != "aes128-ctr" {
		t.Fatalf("Ciphers[0]=%q", got)
	}
	if got := cfg.Config.MACs[0]; got != "hmac-sha1" {
		t.Fatalf("MACs[0]=%q", got)
	}
	if got := cfg.HostKeyAlgorithms[0]; got != "ssh-rsa" {
		t.Fatalf("HostKeyAlgorithms[0]=%q", got)
	}
}

func TestTransportDialerDirectAndWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := startTestTCPServer(t, "direct-ok")
	host, portText, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portText)
	d := NewDaemon(defaultConfig())
	d.cfg.Tunnel.HubHost = host
	d.cfg.Tunnel.HubSSHPort = port
	dialer, err := d.transportDialer("direct")
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertReadLine(t, conn, "direct-ok\n")
	_ = conn.Close()

	sshTarget := startTestTCPServer(t, "websocket-ok")
	wsListen := listenAddr(t)
	wsHost, wsPortText, _ := net.SplitHostPort(wsListen)
	wsPort, _ := strconv.Atoi(wsPortText)
	go func() {
		_ = wscarrier.Serve(ctx, wsListen, sshTarget, log.New(io.Discard, "", 0))
	}()
	waitTCP(t, wsListen)
	d.cfg.Tunnel.HubHost = wsHost
	d.cfg.Tunnel.HubSSHPort = wsPort
	d.cfg.Transport.WebSocket.Enabled = true
	d.cfg.Transport.WebSocket.Carrier = "reach"
	d.cfg.Transport.WebSocket.URL = "ws://" + wsListen
	d.cfg.Transport.WebSocket.PathPrefix = "ws/tunnel/test"
	dialer, err = d.transportDialer("websocket")
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err = dialer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertReadLine(t, conn, "websocket-ok\n")
	_ = conn.Close()
}

func testSSHClientConfig() *ssh.ClientConfig { return &ssh.ClientConfig{} }

func TestApplyCompatIntersectsPinnedHostKeyAlgorithms(t *testing.T) {
	cfg := testSSHClientConfig()
	cfg.HostKeyAlgorithms = []string{"ssh-ed25519", "ssh-rsa"}
	if err := applySSHCompatOptions(cfg, []string{"HostKeyAlgorithms=ecdsa-sha2-nistp256,ssh-rsa"}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostKeyAlgorithms) != 1 || cfg.HostKeyAlgorithms[0] != "ssh-rsa" {
		t.Fatalf("HostKeyAlgorithms=%v, want [ssh-rsa]", cfg.HostKeyAlgorithms)
	}
}

func TestApplyCompatRejectsUnpinnedHostKeyAlgorithm(t *testing.T) {
	cfg := testSSHClientConfig()
	cfg.HostKeyAlgorithms = []string{"ssh-ed25519"}
	if err := applySSHCompatOptions(cfg, []string{"HostKeyAlgorithms=ssh-rsa"}); err == nil {
		t.Fatal("expected no-overlap error")
	}
}

func startTestTCPServer(t *testing.T, line string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, line+"\n")
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

func listenAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", addr)
}

func assertReadLine(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("read %q want %q", got, want)
	}
}
