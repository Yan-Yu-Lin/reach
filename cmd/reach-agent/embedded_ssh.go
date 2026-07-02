package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"reach/internal/wscarrier"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type dialTransport func(context.Context) (net.Conn, string, error)

type embeddedTunnel struct {
	conn      net.Conn
	client    *ssh.Client
	listener  net.Listener
	localAddr string
	logger    *log.Logger
	logFile   io.Closer
}

func (d *Daemon) openEmbeddedTunnel(ctx context.Context, transport string) (*embeddedTunnel, error) {
	dialer, err := d.transportDialer(transport)
	if err != nil {
		return nil, err
	}
	conn, hostKeyName, err := dialer(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := d.sshClientConfig()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, hostKeyName, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", d.cfg.Tunnel.RemotePort)
	ln, err := client.Listen("tcp", listenAddr)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("remote listen %s failed: %w", listenAddr, err)
	}
	localAddr := net.JoinHostPort(d.cfg.Tunnel.LocalHost, strconv.Itoa(d.cfg.Tunnel.LocalPort))
	logFile := d.tunnelLogWriter()
	logger := log.New(io.MultiWriter(d.logger.Writer(), logFile), "reach-tunnel ", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("embedded ssh tunnel connected transport=%s remote=%s local=%s", transport, listenAddr, localAddr)
	return &embeddedTunnel{conn: conn, client: client, listener: ln, localAddr: localAddr, logger: logger, logFile: logFile}, nil
}

func (d *Daemon) transportDialer(transport string) (dialTransport, error) {
	hostKeyName := net.JoinHostPort(d.cfg.Tunnel.HubHost, strconv.Itoa(d.cfg.Tunnel.HubSSHPort))
	switch transport {
	case "direct":
		addr := hostKeyName
		return func(ctx context.Context) (net.Conn, string, error) {
			d := &net.Dialer{Timeout: 8 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", addr)
			return conn, hostKeyName, err
		}, nil
	case "websocket":
		ws := d.cfg.Transport.WebSocket
		if !ws.Enabled {
			return nil, fmt.Errorf("websocket transport is not enabled")
		}
		carrier := firstNonEmpty(ws.Carrier, "reach")
		if carrier != "reach" {
			return nil, fmt.Errorf("embedded websocket transport supports reach carrier, got %q", carrier)
		}
		if ws.URL == "" || ws.PathPrefix == "" {
			return nil, fmt.Errorf("websocket url and path_prefix are required")
		}
		return func(ctx context.Context) (net.Conn, string, error) {
			conn, err := wscarrier.Dial(ctx, normalizeWSURL(ws.URL), ws.PathPrefix)
			return conn, hostKeyName, err
		}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", transport)
	}
}

func (d *Daemon) sshClientConfig() (*ssh.ClientConfig, error) {
	key, err := os.ReadFile(d.cfg.Tunnel.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("read tunnel key: %w", err)
	}
	signer, err := parseTunnelPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse tunnel key: %w", err)
	}
	hostKeyCB, err := knownhosts.New(d.cfg.Tunnel.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts: %w", err)
	}
	hostKeyName := net.JoinHostPort(d.cfg.Tunnel.HubHost, strconv.Itoa(d.cfg.Tunnel.HubSSHPort))
	hostKeyAlgorithms, err := pinnedHostKeyAlgorithms(d.cfg.Tunnel.KnownHosts, hostKeyName)
	if err != nil {
		return nil, fmt.Errorf("load pinned host key algorithms: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:              d.cfg.Tunnel.TunnelUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           8 * time.Second,
	}
	if err := applySSHCompatOptions(cfg, d.cfg.Tunnel.SSHCompat.TunnelOptions); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applySSHCompatOptions(cfg *ssh.ClientConfig, opts []string) error {
	for _, raw := range opts {
		key, vals, ok := parseSSHOption(raw)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "kexalgorithms":
			cfg.Config.KeyExchanges = vals
		case "hostkeyalgorithms":
			requested := append([]string(nil), vals...)
			if len(cfg.HostKeyAlgorithms) > 0 {
				vals = intersectAlgorithms(vals, cfg.HostKeyAlgorithms)
				if len(vals) == 0 {
					return fmt.Errorf("HostKeyAlgorithms=%s has no overlap with pinned known_hosts key types %s", strings.Join(requested, ","), strings.Join(cfg.HostKeyAlgorithms, ","))
				}
			}
			cfg.HostKeyAlgorithms = vals
		case "ciphers":
			cfg.Config.Ciphers = vals
		case "macs":
			cfg.Config.MACs = vals
		}
	}
	return nil
}

func parseSSHOption(raw string) (string, []string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, false
	}
	key, value, ok := strings.Cut(raw, "=")
	if !ok {
		return "", nil, false
	}
	key = strings.TrimSpace(key)
	var vals []string
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		v = strings.TrimLeft(v, "+^")
		v = strings.TrimPrefix(v, "-")
		if v != "" {
			vals = append(vals, v)
		}
	}
	return key, vals, key != "" && len(vals) > 0
}

func (t *embeddedTunnel) serve(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer recoverLog("embedded tunnel shutdown goroutine")
		select {
		case <-ctx.Done():
			_ = t.listener.Close()
			_ = t.client.Close()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		_ = t.listener.Close()
		_ = t.client.Close()
		if t.conn != nil {
			_ = t.conn.Close()
		}
		if t.logFile != nil {
			_ = t.logFile.Close()
		}
	}()
	keepaliveDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				_ = t.listener.Close()
				keepaliveDone <- fmt.Errorf("ssh keepalive panic: %v", r)
			}
		}()
		t.keepalive(ctx, keepaliveDone)
	}()
	for {
		select {
		case err := <-keepaliveDone:
			if err != nil {
				return err
			}
		default:
		}
		remote, err := t.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				select {
				case keepaliveErr := <-keepaliveDone:
					return keepaliveErr
				default:
					return err
				}
			}
			return err
		}
		go func() {
			defer recoverLog("embedded tunnel forward goroutine")
			t.forward(ctx, remote)
		}()
	}
}

func (t *embeddedTunnel) keepalive(ctx context.Context, out chan<- error) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			out <- nil
			return
		case <-ticker.C:
			// OpenSSH may answer REQUEST_FAILURE for this global request; either
			// reply proves the encrypted transport is still alive. Only I/O errors
			// are fatal. Set a transport deadline so half-open TCP/WebSocket paths
			// cannot leave SendRequest blocked forever.
			if t.conn != nil {
				_ = t.conn.SetDeadline(time.Now().Add(15 * time.Second))
			}
			_, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil)
			if t.conn != nil {
				_ = t.conn.SetDeadline(time.Time{})
			}
			if err != nil {
				_ = t.listener.Close()
				out <- fmt.Errorf("ssh keepalive failed: %w", err)
				return
			}
		}
	}
}

func (t *embeddedTunnel) forward(ctx context.Context, remote net.Conn) {
	defer remote.Close()
	d := net.Dialer{Timeout: 8 * time.Second}
	local, err := d.DialContext(ctx, "tcp", t.localAddr)
	if err != nil {
		t.logger.Printf("local dial %s failed: %v", t.localAddr, err)
		return
	}
	defer local.Close()
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, remote); _ = closeWrite(local); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(remote, local); _ = closeWrite(remote); copyDone <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-copyDone:
		<-copyDone
	}
}

func closeWrite(c net.Conn) error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
