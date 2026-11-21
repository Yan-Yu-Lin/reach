package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
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
	_, flagErr := os.Stat(filepath.Join(st.DataDir, "update-confirm-required"))
	switch {
	case flagErr == nil:
		// Legacy flag from an older agent: it wins over any leftover
		// state (astra R3-07); the pre-update state was not recorded, so
		// require the full proof.
	case ust != nil && !ust.terminal():
		if ust.State == updRollingBack || ust.State == updRestartPending {
			return // being rolled back; never confirm
		}
		needProof = ust.WasActive
	default:
		d.updateConfirmed = true // nothing pending
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
func (d *Daemon) localSSHLoginProbe(ctx context.Context) (retErr error) {
	auth := d.cfg.LocalSSH.AuthFile
	if auth == "" {
		return fmt.Errorf("no authorized_keys path configured; cannot prove SSH login")
	}
	signer, err := d.probeSigner()
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
	// The one-time key must be gone before the probe can count as a
	// success: a leftover key is authorization residue, so a failed
	// cleanup fails the probe and the update is not confirmed (daybreak
	// round 5 N10). It is retried on the next confirm attempt; sync_keys
	// removes it too.
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
			d.logger.Printf("could not remove the one-time confirm key: %v", err)
			if retErr == nil {
				retErr = fmt.Errorf("one-time confirm key could not be removed: %w", err)
			}
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
	d.keysMu.Lock()
	defer d.keysMu.Unlock()
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

// probeSigner picks a one-time key type the local sshd accepts: Ed25519
// unless the target's supported key types exclude it (OpenSSH 5.3 /
// RSA-only), then RSA (daybreak round 4 P2-04).
func (d *Daemon) probeSigner() (ssh.Signer, error) {
	supported := d.cfg.Tunnel.SSHCompat.SupportedAuthorizedKeyTypes
	if d.cfg.LocalSSH.InternalSSHD {
		supported = internalSSHDSupportedKeyTypes
	}
	if len(supported) == 0 || keyTypeSupported("ssh-ed25519", supported) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		return ssh.NewSignerFromKey(priv)
	}
	if !keyTypeSupported("ssh-rsa", supported) {
		return nil, fmt.Errorf("local sshd accepts neither ed25519 nor RSA keys (%v)", supported)
	}
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
