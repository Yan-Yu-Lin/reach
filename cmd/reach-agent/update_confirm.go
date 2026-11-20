package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Update confirmation gate (daybreak P3-2, round 3 P2-E).
//
// The new binary confirms itself only when it has proven the path an
// operator uses to reach the machine. Whether that proof is required comes
// from the update state written BEFORE the update (WasActive), never from
// the hub's answer afterwards: a hub that suddenly says "disabled" cannot
// let a broken binary confirm itself and raise the anti-rollback floor.
//
// For a machine that was active the proof is: the tunnel is connected,
// local SSH answers, and a real public-key SSH login to the local sshd as
// the target user runs a harmless command. The login uses a one-time key
// that is added to authorized_keys for the check and removed right after.

func (d *Daemon) confirmPendingUpdateAfterHeartbeat() {
	if d.updateConfirmed || !d.canSelfUpdate() {
		return
	}
	st, err := loadReachInstallState("")
	if err != nil {
		d.logger.Printf("update confirm skipped: %v", err)
		return
	}
	d.confirmPendingUpdate(context.Background(), st)
}

// confirmPendingUpdate applies the gate for the update recorded in
// st.DataDir and confirms it when the gate passes.
func (d *Daemon) confirmPendingUpdate(ctx context.Context, st reachInstallState) {
	ust, err := readUpdateState(st.DataDir)
	if err != nil {
		d.logger.Printf("update state unreadable; leaving the rollback armed: %v", err)
		return
	}
	needProof := true
	switch {
	case ust != nil && !ust.terminal():
		needProof = ust.WasActive
	case ust == nil:
		if _, err := os.Stat(filepath.Join(st.DataDir, "update-confirm-required")); err != nil {
			d.updateConfirmed = true // nothing pending
			return
		}
		// Legacy flag from an older agent: the pre-update state was not
		// recorded, so require the full proof.
	default:
		d.updateConfirmed = true
		return
	}
	if needProof {
		if err := d.proveSSHPath(ctx); err != nil {
			d.logger.Printf("update not confirmed yet: %v", err)
			return
		}
	}
	if err := confirmUpdate(st); err != nil {
		d.logger.Printf("update confirm failed: %v", err)
		return
	}
	d.updateConfirmed = true
}

// proveSSHPath checks tunnel, local sshd and a real login.
func (d *Daemon) proveSSHPath(ctx context.Context) error {
	snap := d.snapshot()
	if snap.Tunnel.State != "connected" {
		return fmt.Errorf("tunnel is %s", snap.Tunnel.State)
	}
	if snap.LocalSSH.State != "healthy" {
		return fmt.Errorf("local ssh is %s", snap.LocalSSH.State)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return d.localSSHLoginProbe(ctx)
}

// localSSHLoginProbe logs into the local sshd with a one-time key and runs
// a harmless command whose output it checks.
func (d *Daemon) localSSHLoginProbe(ctx context.Context) error {
	auth := d.cfg.LocalSSH.AuthFile
	if auth == "" {
		return fmt.Errorf("no authorized_keys path configured; cannot prove SSH login")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return err
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	// The marker contains " reach:<machine>", so a later sync_keys also
	// removes the line if the agent dies before cleaning it up. The
	// private key only ever exists in memory.
	marker := fmt.Sprintf("reach:%s:update-confirm", d.cfg.MachineID)
	line := pub + " " + marker
	if !d.cfg.LocalSSH.InternalSSHD {
		line = `from="127.0.0.1,::1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding,no-pty ` + line
	}
	if err := d.editAuthorizedKeys(func(lines []string) []string { return append(lines, line) }); err != nil {
		return fmt.Errorf("add one-time key: %w", err)
	}
	defer func() {
		if err := d.editAuthorizedKeys(func(lines []string) []string {
			out := lines[:0]
			for _, l := range lines {
				if !strings.Contains(l, marker) {
					out = append(out, l)
				}
			}
			return out
		}); err != nil {
			d.logger.Printf("could not remove the one-time confirm key (sync_keys will): %v", err)
		}
	}()
	user := d.cfg.LocalSSH.TargetUser
	if user == "" {
		user = currentUsername()
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// Loopback to this host's own sshd; the one-time key is useless
		// to anything else listening there.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	if err := applySSHCompatOptions(cfg, d.cfg.LocalSSH.ClientOptions); err != nil {
		return err
	}
	host := d.cfg.LocalSSH.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(d.cfg.LocalSSH.Port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial local ssh: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("ssh login as %s: %w", user, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run("echo reach-update-ok"); err != nil {
		return fmt.Errorf("ssh command: %w", err)
	}
	if strings.TrimSpace(out.String()) != "reach-update-ok" {
		return fmt.Errorf("ssh command printed %q", strings.TrimSpace(out.String()))
	}
	return nil
}

// editAuthorizedKeys rewrites authorized_keys through the same safe writer
// sync_keys uses.
func (d *Daemon) editAuthorizedKeys(fn func([]string) []string) error {
	auth := d.cfg.LocalSSH.AuthFile
	b, err := readAuthorizedKeysFile(auth)
	if err != nil {
		return err
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	lines = fn(lines)
	return writeAuthorizedKeysFile(auth, d.authKeysAccount(), []byte(strings.Join(lines, "\n")+"\n"))
}
