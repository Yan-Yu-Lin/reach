package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxProcessTitleConfigBytes = 4096
	eventRetention             = 7 * 24 * time.Hour
)

type Provisioner struct {
	store *Store
	cfg   Config

	publishEvent func(string, string, map[string]any)
	pruneEvents  func(context.Context, string) error
	// probeTunnel overrides the batched ss/keyscan probe (tests only).
	probeTunnel  func(context.Context, int) (bool, string)
	observedMu   sync.Mutex
	healthMu     sync.RWMutex
	healthReport []map[string]any
	argon        *argonGate
	osOps        tunnelOS
	transitions  machineLocks
	// listListeners overrides `ss -Htln` in tests.
	listListeners func(context.Context) (map[int]bool, error)
	// createAccount overrides createSystemAccount in tests.
	createAccount func(ctx context.Context, unixUser string, port int, pubkey, expiresAt string) error
}

type CreateMachineInput struct {
	// RequestID links the machine to its enrollment so a crashed provision
	// can be detected and cleaned up (daybreak P2-7).
	RequestID   string
	Slug        string
	DisplayName string
	TargetUser  string
	Mode        string
	LocalPort   int
	Persistence string
	Distro      string
	Arch        string
	Pubkey      string
	OwnerUserID string
	HubID       string
	ExpiresAt   string
}

type CreateMachineResult struct {
	Machine    Machine `json:"machine"`
	Tunnel     Tunnel  `json:"tunnel"`
	Hub        Hub     `json:"hub"`
	AgentToken string  `json:"agent_token,omitempty"`
}

func NewProvisioner(store *Store, cfg Config) *Provisioner {
	p := &Provisioner{store: store, cfg: cfg}
	if cfg.ProvisioningEnabled {
		p.osOps = systemTunnelOS{cfg: cfg}
	} else {
		p.osOps = noopTunnelOS{}
	}
	return p
}

func (p *Provisioner) ops() tunnelOS {
	if p.osOps == nil {
		return noopTunnelOS{}
	}
	return p.osOps
}

func (p *Provisioner) CreateMachine(ctx context.Context, in CreateMachineInput) (CreateMachineResult, error) {
	if err := ValidateSlug(in.Slug); err != nil {
		return CreateMachineResult{}, err
	}
	if err := ValidateTargetUser(in.TargetUser); err != nil {
		return CreateMachineResult{}, err
	}
	canonicalKey, err := CanonicalSSHPublicKey(in.Pubkey)
	if err != nil {
		return CreateMachineResult{}, fmt.Errorf("invalid tunnel public key: %w", err)
	}
	in.Pubkey = canonicalKey
	if in.LocalPort == 0 {
		in.LocalPort = 22
	}
	if in.Mode == "" {
		in.Mode = "system"
	}
	if in.HubID == "" {
		in.HubID = p.cfg.DefaultHub.ID
	}
	if in.ExpiresAt == "" && p.cfg.DefaultExpiry > 0 {
		in.ExpiresAt = time.Now().UTC().Add(p.cfg.DefaultExpiry).Format(time.RFC3339)
	}

	var res CreateMachineResult
	machineID, err := RandomID("m")
	if err != nil {
		return res, err
	}
	tunnelID, err := RandomID("t")
	if err != nil {
		return res, err
	}
	short, err := RandomShortID()
	if err != nil {
		return res, err
	}
	agentToken, err := RandomToken("at", 32)
	if err != nil {
		return res, err
	}
	agentHash := HashToken(agentToken)
	unixUser := "rt-" + short
	now := nowUTC()

	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	hub, err := getHubTx(ctx, tx, in.HubID)
	if err != nil {
		return res, err
	}
	dropIn, err := p.ops().DropInPorts()
	if err != nil {
		return res, fmt.Errorf("read sshd drop-ins: %w", err)
	}
	port, err := allocatePortTx(ctx, tx, hub, dropIn)
	if err != nil {
		return res, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO machines (id,slug,original_slug,display_name,target_user,owner_user_id,status,desired_state,observed_state,desired_generation,desired_changed_at,desired_changed_by,cleanup_state,mode,local_port,persistence,distro,arch,agent_token_hash,created_at,updated_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, machineID, in.Slug, in.Slug, nullable(in.DisplayName), nullable(in.TargetUser), nullable(in.OwnerUserID), "provisioning", DesiredActive, ObservedUnknown, 1, now, "system", "pending", in.Mode, in.LocalPort, nullable(in.Persistence), nullable(in.Distro), nullable(in.Arch), agentHash, now, now, nullable(in.ExpiresAt))
	if err != nil {
		return res, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tunnels (id,machine_id,hub_id,unix_user,original_unix_user,remote_port,original_remote_port,tunnel_pubkey,status,created_at,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, tunnelID, machineID, hub.ID, unixUser, unixUser, port, port, in.Pubkey, "offline", now, nullable(in.ExpiresAt))
	if err != nil {
		return res, err
	}
	if in.OwnerUserID != "" {
		_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO machine_grants (machine_id,user_id,role,expires_at) VALUES (?,?,?,?)`, machineID, in.OwnerUserID, "owner", nullable(in.ExpiresAt))
	}
	if in.RequestID != "" {
		// The request must still be ours: if maintenance already reclaimed
		// the lease, create nothing (astra R2-10).
		r, err := tx.ExecContext(ctx, `UPDATE requests SET machine_id=? WHERE id=? AND status='provisioning'`, machineID, in.RequestID)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return res, errProvisioningLeaseLost
		}
	}
	// Intent row commits with the machine row, before any OS change.
	journalID, err := recordDurable(ctx, tx, "machine.provision", machineID, "system", durableIntent)
	if err != nil {
		return res, err
	}
	// Hold the machine lock through the OS phase so provisioning recovery
	// (which retires through the same lock) cannot run alongside it.
	unlock, err := p.lockMachine(machineID)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := tx.Commit(); err != nil {
		return res, err
	}

	create := p.createSystemAccount
	if p.createAccount != nil {
		create = p.createAccount
	} else if !p.cfg.ProvisioningEnabled {
		create = nil
	}
	if create != nil {
		if err := create(ctx, unixUser, port, in.Pubkey, in.ExpiresAt); err != nil {
			// The tunnel row keeps status 'failed' and its port/user as a
			// tombstone; the allocator never reuses non-retired rows.
			p.markProvisionFailed(ctx, machineID, tunnelID, err)
			if cerr := p.ops().Remove(ctx, unixUser); cerr != nil {
				log.Printf("cleanup after failed provision of %s: %v", unixUser, cerr)
			}
			p.store.settleDurable(ctx, journalID, durableFailed)
			return res, err
		}
	}
	up, err := p.store.db.ExecContext(ctx, `UPDATE machines SET status='offline', observed_state=?, cleanup_state=NULL, updated_at=? WHERE id=? AND status='provisioning'`, ObservedOffline, nowUTC(), machineID)
	if err != nil {
		return res, err
	}
	if in.RequestID != "" {
		_, _ = p.store.db.ExecContext(ctx, `UPDATE requests SET provision_phase='os_created' WHERE id=? AND status='provisioning'`, in.RequestID)
	}
	if n, _ := up.RowsAffected(); n != 1 {
		// Someone changed the machine without holding its lock (an older
		// binary, a manual DB edit). Undo what this provision created,
		// under our lock, and keep the tunnel row as a failed tombstone so
		// its port and user stay reserved until cleanup is verified
		// (astra R3-01).
		p.compensateProvision(ctx, machineID, tunnelID, unixUser)
		p.store.settleDurable(ctx, journalID, durableFailed)
		return res, errProvisioningLeaseLost
	}
	p.store.settleDurable(ctx, journalID, durableDone)
	p.store.Audit(ctx, "machine.create", "system", machineID, fmt.Sprintf("slug=%s tunnel=%s port=%d", in.Slug, unixUser, port))
	res.Machine, _ = p.GetMachine(ctx, machineID)
	res.Tunnel, _ = p.GetTunnel(ctx, tunnelID)
	res.Hub = hub
	res.AgentToken = agentToken
	return res, nil
}

// compensateProvision removes the hub account a provision created after its
// final DB update lost, and records the outcome. The tunnel row stays
// non-retired, so the allocator keeps its port.
func (p *Provisioner) compensateProvision(ctx context.Context, machineID, tunnelID, unixUser string) {
	msg := "provision lost a concurrent change; hub account removed"
	if err := p.ops().Remove(ctx, unixUser); err != nil {
		msg = "provision lost a concurrent change; removing the hub account failed: " + err.Error()
	} else if residue, err := p.ops().Residue(ctx, unixUser); err != nil || len(residue) > 0 {
		msg = fmt.Sprintf("provision lost a concurrent change; hub residue remains: %v %v", residue, err)
	}
	_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='cleanup_failed' WHERE id=? AND status<>'retired'`, tunnelID)
	_, _ = p.store.db.ExecContext(ctx, `UPDATE machines SET provision_error=?, updated_at=? WHERE id=?`, msg, nowUTC(), machineID)
	p.store.Audit(ctx, "machine.provision_compensated", "system", machineID, msg)
}

func getHubTx(ctx context.Context, tx *sql.Tx, id string) (Hub, error) {
	var h Hub
	err := tx.QueryRowContext(ctx, `SELECT id,name,public_host,ssh_port,proxyjump_alias,api_url,port_range_start,port_range_end,status,created_at FROM hubs WHERE id=?`, id).Scan(&h.ID, &h.Name, &h.PublicHost, &h.SSHPort, &h.ProxyJumpAlias, &h.APIURL, &h.PortStart, &h.PortEnd, &h.Status, &h.CreatedAt)
	return h, err
}

func reservedTunnelPort(port int) bool {
	if port == 9300 || port == 9401 || port == 9443 {
		return true
	}
	return port >= 9163 && port <= 9171
}

func portListening(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = ln.Close()
		return false
	}
	return true
}

// allocatePortTx picks a free loopback port. Ports are checked against the
// tunnels of every hub row, not only h: all hub rows describe listeners on the
// same machine, and per-hub checks let two "active" tunnels share 9205
// (perf #5). Rows in any state except fully retired keep their port, so a
// cleanup_failed or failed tunnel is a tombstone. Ports still named by a
// Reach sshd drop-in are skipped too (daybreak P1-2).
func allocatePortTx(ctx context.Context, tx *sql.Tx, h Hub, dropIn map[int]bool) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT remote_port FROM tunnels WHERE status<>'retired' AND remote_port BETWEEN ? AND ?
		UNION SELECT original_remote_port FROM tunnels WHERE status='retired' AND original_remote_port BETWEEN ? AND ? AND (cleanup_verified_at IS NULL OR cleanup_verified_at='')`, h.PortStart, h.PortEnd, h.PortStart, h.PortEnd)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return 0, err
		}
		used[port] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for port := h.PortStart; port <= h.PortEnd; port++ {
		if used[port] || dropIn[port] || reservedTunnelPort(port) || portListening(port) {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free ports in hub %s range %d-%d", h.ID, h.PortStart, h.PortEnd)
}

func (p *Provisioner) createSystemAccount(ctx context.Context, unixUser string, port int, pubkey, expiresAt string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	home := filepath.Join(p.cfg.TunnelHomeBase, unixUser)
	if err := validateHomePath(home); err != nil {
		return err
	}
	if err := run(ctx, "useradd", "--system", "--shell", "/usr/sbin/nologin", "--home", home, "--create-home", unixUser); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	if err := run(ctx, "chown", "-R", unixUser+":"+unixUser, home); err != nil {
		return err
	}
	if err := os.Chmod(sshDir, 0o700); err != nil {
		return err
	}
	ak := filepath.Join(sshDir, "authorized_keys")
	line, err := authorizedKeyLine(port, pubkey, expiresAt)
	if err != nil {
		return err
	}
	if err := os.WriteFile(ak, []byte(line+"\n"), 0o600); err != nil {
		return err
	}
	if err := run(ctx, "chown", unixUser+":"+unixUser, ak); err != nil {
		return err
	}
	conf := filepath.Join(p.cfg.SSHDConfigDir, "90-"+unixUser+".conf")
	if err := os.WriteFile(conf, []byte(sshdMatchConfig(unixUser, port)), 0o644); err != nil {
		return err
	}
	if err := run(ctx, "sshd", "-t"); err != nil {
		return fmt.Errorf("sshd -t failed: %w", err)
	}
	if err := run(ctx, "systemctl", "reload", p.cfg.SSHServiceName); err != nil {
		return fmt.Errorf("reload ssh failed: %w", err)
	}
	return nil
}

// authorizedKeyLine builds the hub-side authorized_keys line from a key that
// is re-canonicalized here, so even a legacy DB row cannot inject a second
// line or its own options.
func authorizedKeyLine(port int, pubkey, expiresAt string) (string, error) {
	canonical, err := CanonicalSSHPublicKey(pubkey)
	if err != nil {
		return "", fmt.Errorf("invalid tunnel public key: %w", err)
	}
	opts := []string{"restrict", "port-forwarding", fmt.Sprintf("permitlisten=\"127.0.0.1:%d\"", port)}
	if expiresAt != "" {
		if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			opts = append(opts, fmt.Sprintf("expiry-time=\"%s\"", t.UTC().Format("20060102150405Z")))
		}
	}
	return strings.Join(opts, ",") + " " + canonical, nil
}

func sshdMatchConfig(unixUser string, port int) string {
	return fmt.Sprintf(`# Reach managed reverse tunnel. Do not edit by hand.
Match User %s
    AllowTcpForwarding remote
    PermitListen 127.0.0.1:%d
    PermitOpen none
    GatewayPorts no
    PermitTTY no
    PermitTunnel no
    AllowAgentForwarding no
    X11Forwarding no
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowStreamLocalForwarding no
    PermitUserRC no
    ForceCommand /bin/false
    ClientAliveInterval 30
    ClientAliveCountMax 3
`, unixUser, port)
}

func (p *Provisioner) markProvisionFailed(ctx context.Context, machineID, tunnelID string, err error) {
	msg := err.Error()
	_, _ = p.store.db.ExecContext(ctx, `UPDATE machines SET status='failed', provision_error=?, updated_at=? WHERE id=?`, msg, nowUTC(), machineID)
	_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='failed' WHERE id=?`, tunnelID)
	p.store.Audit(ctx, "machine.provision_failed", "system", machineID, msg)
}

// Cleanup states recorded in machines.cleanup_state (and mirrored into
// machines.status for the failure cases). See docs/EVENTS.md.
const (
	CleanupPending  = "pending"
	CleanupTimedOut = "timed_out"
	CleanupFailed   = "cleanup_failed"
	DisableFailed   = "disable_failed"
	CleanupComplete = "complete"
)

// ErrCleanupFailed wraps any OS-level revoke/cleanup failure. The DB is left
// showing the failure; nothing is reported as disabled or retired.
var ErrCleanupFailed = errors.New("hub-side cleanup failed")

func tunnelCleanupUser(t Tunnel) string {
	if t.OriginalUnixUser != nil && *t.OriginalUnixUser != "" && !strings.HasPrefix(t.UnixUser, "rt-") {
		return *t.OriginalUnixUser
	}
	return t.UnixUser
}

// errConcurrentTransition reports that another transition changed the
// machine between our read and our write.
var errConcurrentTransition = errors.New("machine changed concurrently; retry")

// errInvalidInput marks caller mistakes (HTTP 400), as opposed to storage
// failures (HTTP 5xx).
var errInvalidInput = errors.New("invalid input")

// machineLocks serialises lifecycle transitions per machine so OS revoke /
// restore steps and their DB records cannot interleave (daybreak P1-1).
type machineLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lockMachine takes the in-process mutex and the cross-process file lock of
// one machine (astra R3-01). Every step that reads a machine, changes hub OS
// state for it and records the result runs under it, so another reachd or
// CLI instance cannot interleave its own OS phase.
func (p *Provisioner) lockMachine(id string) (func(), error) {
	unlock := p.transitions.lock(id)
	if p.store == nil || p.store.path == "" {
		return unlock, nil
	}
	unlockFile, err := lockMachineOS(p.store.path, id)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("machine lock: %w", err)
	}
	return func() { unlockFile(); unlock() }, nil
}

func (l *machineLocks) lock(id string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	m := l.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[id] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// casMachine runs a machines UPDATE that must match the generation read at
// the start of the transition; anything else means a concurrent writer.
func (p *Provisioner) casMachine(ctx context.Context, oldGen int64, query string, args ...any) error {
	return casMachineTx(ctx, p.store.db, oldGen, query, args...)
}

// casMachineTx is casMachine inside a caller's transaction. The generation
// check is in the database, so writers in other processes (reachd CLI)
// are caught too, not only goroutines sharing the in-process lock.
func casMachineTx(ctx context.Context, ex sqlExecer, oldGen int64, query string, args ...any) error {
	res, err := ex.ExecContext(ctx, query+` AND desired_generation=?`, append(args, oldGen)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errConcurrentTransition
	}
	return nil
}

func (p *Provisioner) SetDesiredState(ctx context.Context, idOrSlug, desired, actor string) (Machine, error) {
	first, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	unlock, err := p.lockMachine(first.ID)
	if err != nil {
		return Machine{}, err
	}
	defer unlock()
	// Re-read under the lock: the state may have changed while we waited.
	m, tunnels, err := p.getMachineAndTunnels(ctx, first.ID)
	if err != nil {
		return Machine{}, err
	}
	switch desired {
	case DesiredActive, DesiredDisabled, DesiredRetiring, DesiredRetired:
	default:
		return Machine{}, fmt.Errorf("invalid desired state %q", desired)
	}
	// Commit the intent before any OS change (daybreak round 3 P1-A). If it
	// cannot be recorded, nothing happens.
	journalID, err := recordDurable(ctx, p.store.db, "machine.desired."+desired, m.ID, actor, durableIntent)
	if err != nil {
		return Machine{}, err
	}
	out, err := p.setDesiredStateLocked(ctx, m, tunnels, desired, actor)
	if err != nil {
		p.store.settleDurable(ctx, journalID, durableFailed)
	} else {
		p.store.settleDurable(ctx, journalID, durableDone)
	}
	return out, err
}

func (p *Provisioner) setDesiredStateLocked(ctx context.Context, m Machine, tunnels []Tunnel, desired, actor string) (Machine, error) {
	var err error
	now := nowUTC()
	oldGen := m.DesiredGeneration
	gen := oldGen + 1
	switch desired {
	case DesiredActive:
		var enabled []string
		for _, t := range tunnels {
			if t.Status == "retired" || t.Status == "cleanup_failed" {
				continue
			}
			if err := p.ops().EnableAuth(ctx, t.UnixUser); err != nil {
				p.compensateEnable(ctx, m.ID, enabled)
				p.store.Audit(ctx, "machine.enable_failed", actor, m.ID, err.Error())
				return Machine{}, fmt.Errorf("%w: enable %s: %v", ErrCleanupFailed, t.UnixUser, err)
			}
			enabled = append(enabled, t.UnixUser)
		}
		err = p.inTx(ctx, func(tx *sql.Tx) error {
			if err := casMachineTx(ctx, tx, oldGen, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status=CASE WHEN observed_state='online' THEN 'online' ELSE 'offline' END, disabled_at=NULL, cleanup_state=NULL, updated_at=? WHERE id=?`, desired, gen, now, actor, now, m.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE tunnels SET status='offline' WHERE machine_id=? AND status='disabled'`, m.ID)
			return err
		})
		if err != nil {
			// Access was restored but not recorded: take it away again
			// so DB and hub never disagree in the permissive direction.
			p.compensateEnable(ctx, m.ID, enabled)
			return Machine{}, err
		}
		_ = p.QueueCommand(ctx, m.ID, gen, "sync_keys", map[string]any{"enabled": true}, time.Hour)
		_ = p.QueueCommand(ctx, m.ID, gen, "start_tunnel", nil, time.Hour)
	case DesiredDisabled, DesiredRetiring:
		failures := p.disableTunnels(ctx, tunnels)
		if len(failures) > 0 {
			// Record intent and the failure; never claim the machine is
			// disabled while its key may still log in.
			msg := strings.Join(failures, "; ")
			_ = p.casMachine(ctx, oldGen, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status=?, cleanup_state=?, provision_error=?, updated_at=? WHERE id=?`, desired, gen, now, actor, DisableFailed, DisableFailed, msg, now, m.ID)
			p.store.Audit(ctx, "machine.disable_failed", actor, m.ID, msg)
			p.publish("machine.cleanup_failed", m.ID, map[string]any{"machine_id": m.ID, "cleanup_state": DisableFailed, "desired_state": desired})
			return Machine{}, fmt.Errorf("%w: %s", ErrCleanupFailed, msg)
		}
		err = p.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			if desired == DesiredDisabled {
				err = casMachineTx(ctx, tx, oldGen, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='disabled', disabled_at=?, cleanup_state=NULL, provision_error=NULL, updated_at=? WHERE id=?`, desired, gen, now, actor, now, now, m.ID)
			} else {
				err = casMachineTx(ctx, tx, oldGen, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='retiring', cleanup_state=?, provision_error=NULL, updated_at=? WHERE id=?`, desired, gen, now, actor, CleanupPending, now, m.ID)
			}
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE tunnels SET status='disabled' WHERE machine_id=? AND status NOT IN ('retired','cleanup_failed')`, m.ID)
			return err
		})
		if err != nil {
			return Machine{}, err
		}
		if desired == DesiredDisabled {
			_ = p.QueueCommand(ctx, m.ID, gen, "stop_tunnel", nil, time.Hour)
			_ = p.QueueCommand(ctx, m.ID, gen, "sync_keys", map[string]any{"enabled": false}, time.Hour)
		} else {
			_ = p.QueueCommand(ctx, m.ID, gen, "uninstall", map[string]any{"remove_agent": true, "remove_config": true, "remove_data": true, "remove_admin_keys": true, "stop_local_user_sshd": true}, p.retiringTimeout())
		}
	case DesiredRetired:
		return p.retire(ctx, m, tunnels, oldGen, actor)
	default:
		return Machine{}, fmt.Errorf("invalid desired state %q", desired)
	}
	if err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.desired."+desired, actor, m.ID, "")
	return p.GetMachine(ctx, m.ID)
}

// compensateEnable revokes access re-enabled by a transition that could not
// be recorded. If revoking fails too, the machine is marked disable_failed
// (whatever its generation) so the UI shows that hub auth may be on while
// the DB says otherwise; the next disable/retire retries.
func (p *Provisioner) compensateEnable(ctx context.Context, machineID string, users []string) {
	var failed []string
	for _, u := range users {
		if err := p.ops().DisableAuth(ctx, u); err != nil {
			log.Printf("compensating disable of %s failed: %v", u, err)
			failed = append(failed, u+": "+err.Error())
		}
	}
	if len(failed) == 0 {
		return
	}
	msg := "enable was not recorded and revoking it again failed: " + strings.Join(failed, "; ")
	if _, err := p.store.db.ExecContext(ctx, `UPDATE machines SET status=?, cleanup_state=?, provision_error=?, updated_at=? WHERE id=?`, DisableFailed, DisableFailed, msg, nowUTC(), machineID); err != nil {
		log.Printf("record failed compensation for %s: %v", machineID, err)
	}
	p.store.Audit(ctx, "machine.disable_failed", "system", machineID, msg)
	p.publish("machine.cleanup_failed", machineID, map[string]any{"machine_id": machineID, "cleanup_state": DisableFailed})
}

func (p *Provisioner) disableTunnels(ctx context.Context, tunnels []Tunnel) []string {
	var failures []string
	for _, t := range tunnels {
		if t.Status == "retired" || !strings.HasPrefix(t.UnixUser, "rt-") {
			continue
		}
		if err := p.ops().DisableAuth(ctx, t.UnixUser); err != nil {
			failures = append(failures, t.UnixUser+": "+err.Error())
		}
	}
	return failures
}

// retire removes every hub-side trace of the machine and only then frees
// its port and slug. Any failure leaves a cleanup_failed tombstone that keeps
// the Unix user and port reserved; retire-now retries it (daybreak P1-2).
func (p *Provisioner) retire(ctx context.Context, m Machine, tunnels []Tunnel, oldGen int64, actor string) (Machine, error) {
	now := nowUTC()
	gen := oldGen + 1
	var failures []string
	for _, t := range tunnels {
		if t.Status == "retired" {
			continue
		}
		u := tunnelCleanupUser(t)
		if err := ValidateUnixUser(u); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := p.ops().Remove(ctx, u); err != nil {
			failures = append(failures, u+": "+err.Error())
		}
		residue, err := p.ops().Residue(ctx, u)
		if err != nil {
			failures = append(failures, u+": verify: "+err.Error())
		} else if len(residue) > 0 {
			failures = append(failures, u+": still present: "+strings.Join(residue, ", "))
		}
	}
	if len(failures) > 0 {
		msg := strings.Join(failures, "; ")
		// Tombstone: the tunnels stay reserved even if the machine CAS
		// loses, so their user/port are never handed out while residue
		// may remain on the hub.
		_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='cleanup_failed' WHERE machine_id=? AND status<>'retired'`, m.ID)
		_ = p.casMachine(ctx, oldGen, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status=?, cleanup_state=?, provision_error=?, updated_at=? WHERE id=?`, DesiredRetiring, gen, now, actor, CleanupFailed, CleanupFailed, msg, now, m.ID)
		p.store.Audit(ctx, "machine.cleanup_failed", actor, m.ID, msg)
		p.publish("machine.cleanup_failed", m.ID, map[string]any{"machine_id": m.ID, "cleanup_state": CleanupFailed, "desired_state": DesiredRetiring})
		return Machine{}, fmt.Errorf("%w: %s", ErrCleanupFailed, msg)
	}
	original := m.Slug
	if m.OriginalSlug != nil && *m.OriginalSlug != "" {
		original = *m.OriginalSlug
	}
	retiredSlug, err := p.retiredSlug(ctx, original, m.ID)
	if err != nil {
		return Machine{}, err
	}
	// Machine, tunnels and commands finalize in one transaction guarded by
	// the generation CAS: a concurrent writer makes the whole finalize roll
	// back, never leaving retired tunnels under an active machine (astra
	// R2-04). The agent token is revoked: a retired agent can no longer
	// heartbeat or report results (daybreak P2-5).
	err = p.inTx(ctx, func(tx *sql.Tx) error {
		if err := casMachineTx(ctx, tx, oldGen, `UPDATE machines SET slug=?, original_slug=COALESCE(NULLIF(original_slug,''),?), desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='retired', cleanup_state=?, provision_error=NULL, agent_token_hash=NULL, retired_at=COALESCE(retired_at,?), updated_at=? WHERE id=?`, retiredSlug, original, DesiredRetired, gen, now, actor, CleanupComplete, now, now, m.ID); err != nil {
			return err
		}
		for _, t := range tunnels {
			if t.Status == "retired" {
				continue
			}
			if _, err := retireTunnelRowTx(ctx, tx, t); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE agent_commands SET status='cancelled', last_error='machine retired' WHERE machine_id=? AND status IN ('pending','sent')`, m.ID)
		return err
	})
	if errors.Is(err, errConcurrentTransition) {
		// The hub account is already gone but someone changed the machine
		// without its lock. Do not leave it looking active: record a
		// retiring/cleanup_failed tombstone (ports stay reserved, the
		// token is revoked) so retire-now finishes it (astra R3-01).
		msg := "hub account removed but the machine changed concurrently; run retire-now again"
		_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='cleanup_failed' WHERE machine_id=? AND status<>'retired'`, m.ID)
		_, _ = p.store.db.ExecContext(ctx, `UPDATE machines SET desired_state=?, desired_generation=desired_generation+1, status=?, cleanup_state=?, provision_error=?, agent_token_hash=NULL, updated_at=? WHERE id=?`, DesiredRetiring, CleanupFailed, CleanupFailed, msg, now, m.ID)
		p.store.Audit(ctx, "machine.cleanup_failed", actor, m.ID, msg)
		return Machine{}, fmt.Errorf("%w: %s", ErrCleanupFailed, msg)
	}
	if err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.desired."+DesiredRetired, actor, m.ID, "")
	return p.GetMachine(ctx, m.ID)
}

func (p *Provisioner) retiringTimeout() time.Duration {
	if p.cfg.RetiringTimeout > 0 {
		return p.cfg.RetiringTimeout
	}
	return 24 * time.Hour
}

func (p *Provisioner) SetUpdatePolicy(ctx context.Context, idOrSlug, policy, actor string) (Machine, error) {
	policy, err := normalizeUpdatePolicy(policy)
	if err != nil {
		return Machine{}, fmt.Errorf("%w: %v", errInvalidInput, err)
	}
	m, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	now := nowUTC()
	if err := p.store.durableTx(ctx, "machine.update_policy", m.ID, actor, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE machines SET update_policy=?, updated_at=? WHERE id=?`, policy, now, m.ID)
		return err
	}); err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.update_policy.update", actor, m.ID, "policy="+policy)
	return p.GetMachine(ctx, m.ID)
}

func (p *Provisioner) SetProcessTitleConfig(ctx context.Context, idOrSlug string, cfg *ProcessTitleConfig, actor string) (Machine, error) {
	first, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	// Bumps desired_generation, so it takes the same per-machine lock as
	// lifecycle transitions and writes with a generation CAS (astra R2-04).
	unlock, err := p.lockMachine(first.ID)
	if err != nil {
		return Machine{}, err
	}
	defer unlock()
	m, err := p.GetMachine(ctx, first.ID)
	if err != nil {
		return Machine{}, err
	}
	if m.DesiredState == DesiredRetired || m.DesiredState == DesiredRetiring {
		return Machine{}, fmt.Errorf("%w: machine is %s", errInvalidInput, m.DesiredState)
	}
	var raw any
	if cfg != nil {
		clean, err := normalizeDesiredProcessTitleConfig(*cfg)
		if err != nil {
			return Machine{}, fmt.Errorf("%w: %v", errInvalidInput, err)
		}
		if clean != nil {
			b, err := json.Marshal(clean)
			if err != nil {
				return Machine{}, err
			}
			raw = string(b)
		}
	}
	now := nowUTC()
	gen := m.DesiredGeneration + 1
	if err := p.store.durableTx(ctx, "machine.process_title", m.ID, actor, func(tx *sql.Tx) error {
		return casMachineTx(ctx, tx, m.DesiredGeneration, `UPDATE machines SET process_title_config_json=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, updated_at=? WHERE id=?`, raw, gen, now, actor, now, m.ID)
	}); err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.process_title.update", actor, m.ID, "")
	return p.GetMachine(ctx, m.ID)
}

func normalizeDesiredProcessTitleConfig(cfg ProcessTitleConfig) (*ProcessTitleConfig, error) {
	out := ProcessTitleConfig{RotateInterval: strings.TrimSpace(cfg.RotateInterval)}
	if cfg.ProcessTitle != "" {
		out.ProcessTitle = strings.TrimSpace(strings.ReplaceAll(cfg.ProcessTitle, "\x00", " "))
		if len(out.ProcessTitle) > maxProcessTitleConfigBytes {
			out.ProcessTitle = truncateProcessTitleUTF8Bytes(out.ProcessTitle, maxProcessTitleConfigBytes)
		}
	}
	for _, title := range cfg.ProcessTitles {
		title = strings.TrimSpace(strings.ReplaceAll(title, "\x00", " "))
		if title == "" {
			continue
		}
		if len(title) > maxProcessTitleConfigBytes {
			title = truncateProcessTitleUTF8Bytes(title, maxProcessTitleConfigBytes)
		}
		out.ProcessTitles = append(out.ProcessTitles, title)
	}
	if len(out.ProcessTitles) == 0 && out.ProcessTitle == "" {
		return nil, nil
	}
	if out.RotateInterval != "" {
		d, err := time.ParseDuration(out.RotateInterval)
		if err != nil {
			return nil, fmt.Errorf("rotate_interval: %w", err)
		}
		if d < time.Second {
			return nil, fmt.Errorf("rotate_interval must be at least 1s")
		}
	}
	if len(out.ProcessTitles) > 20 {
		return nil, fmt.Errorf("process_titles may contain at most 20 titles")
	}
	return &out, nil
}

func truncateProcessTitleUTF8Bytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		_, size := utf8.DecodeLastRuneInString(s[:max])
		if size <= 0 || size > max {
			max--
		} else {
			max -= size
		}
	}
	return s[:max]
}

func (p *Provisioner) retiredSlug(ctx context.Context, original, machineID string) (string, error) {
	base := NormalizeSlug(original)
	const suffixLen = len("--retired-") + 8
	if len(base) > 32-suffixLen {
		base = strings.Trim(base[:32-suffixLen], "-")
	}
	if len(base) < 3 {
		base = "box"
	}
	for i := 0; i < 20; i++ {
		short, err := RandomShortID()
		if err != nil {
			return "", err
		}
		candidate := base + "--retired-" + short
		if err := ValidateSlug(candidate); err != nil {
			continue
		}
		var existing string
		err = p.store.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE slug=? AND id<>?`, candidate, machineID).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate unique retired slug for %s", original)
}

// inTx runs fn in one transaction.
func (p *Provisioner) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type sqlQueryExecer interface {
	sqlExecer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func retireTunnelRowTx(ctx context.Context, q sqlQueryExecer, t Tunnel) (Tunnel, error) {
	originalUser := t.UnixUser
	if t.OriginalUnixUser != nil && *t.OriginalUnixUser != "" {
		originalUser = *t.OriginalUnixUser
	}
	originalPort := t.RemotePort
	if t.OriginalRemotePort != nil && *t.OriginalRemotePort != 0 {
		originalPort = *t.OriginalRemotePort
	}
	retiredUser := t.UnixUser
	if !strings.HasPrefix(retiredUser, "retired-") {
		retiredUser = "retired-" + t.ID
	}
	retiredPort := t.RemotePort
	if retiredPort > 0 {
		var min sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT MIN(remote_port) FROM tunnels WHERE hub_id=? AND remote_port<0`, t.HubID).Scan(&min); err != nil {
			return t, err
		}
		retiredPort = -1
		if min.Valid {
			retiredPort = int(min.Int64) - 1
		}
	}
	_, err := q.ExecContext(ctx, `UPDATE tunnels SET status='retired', original_unix_user=COALESCE(NULLIF(original_unix_user,''),?), original_remote_port=COALESCE(NULLIF(original_remote_port,0),?), unix_user=?, remote_port=?, cleanup_verified_at=? WHERE id=?`, originalUser, originalPort, retiredUser, retiredPort, nowUTC(), t.ID)
	if err != nil {
		return t, err
	}
	t.UnixUser = retiredUser
	t.RemotePort = retiredPort
	t.Status = "retired"
	return t, nil
}

func (p *Provisioner) QueueCommand(ctx context.Context, machineID string, generation int64, typ string, payload any, ttl time.Duration) error {
	return queueCommandTx(ctx, p.store.db, machineID, generation, typ, payload, ttl)
}

func queueCommandTx(ctx context.Context, ex sqlExecer, machineID string, generation int64, typ string, payload any, ttl time.Duration) error {
	id, err := RandomID("cmd")
	if err != nil {
		return err
	}
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	expires := ""
	if ttl > 0 {
		expires = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	_, err = ex.ExecContext(ctx, `INSERT INTO agent_commands (id,machine_id,generation,type,payload_json,status,created_at,expires_at) VALUES (?,?,?,?,?,?,?,?)`, id, machineID, generation, typ, nullable(string(raw)), "pending", nowUTC(), nullable(expires))
	return err
}

func (p *Provisioner) DisableMachine(ctx context.Context, idOrSlug string, actor string) error {
	_, err := p.SetDesiredState(ctx, idOrSlug, DesiredDisabled, actor)
	return err
}
func (p *Provisioner) EnableMachine(ctx context.Context, idOrSlug string, actor string) error {
	_, err := p.SetDesiredState(ctx, idOrSlug, DesiredActive, actor)
	return err
}
func (p *Provisioner) RemoveMachine(ctx context.Context, idOrSlug string, actor string) error {
	_, err := p.SetDesiredState(ctx, idOrSlug, DesiredRetiring, actor)
	return err
}
func (p *Provisioner) RetireMachineNow(ctx context.Context, idOrSlug string, actor string) error {
	_, err := p.SetDesiredState(ctx, idOrSlug, DesiredRetired, actor)
	return err
}

func validateHomePath(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/var/lib/rt-") {
		return fmt.Errorf("refusing unsafe home path %s", clean)
	}
	return ValidateUnixUser(filepath.Base(clean))
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *Provisioner) GetMachine(ctx context.Context, idOrSlug string) (Machine, error) {
	return scanMachine(p.store.db.QueryRowContext(ctx, `SELECT `+machineCols+` FROM machines WHERE id=? OR slug=?`, idOrSlug, idOrSlug))
}

func (p *Provisioner) GetTunnel(ctx context.Context, id string) (Tunnel, error) {
	return scanTunnel(p.store.db.QueryRowContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE id=?`, id))
}

func (p *Provisioner) getMachineAndTunnels(ctx context.Context, idOrSlug string) (Machine, []Tunnel, error) {
	m, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, nil, err
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE machine_id=?`, m.ID)
	if err != nil {
		return m, nil, err
	}
	defer rows.Close()
	var tunnels []Tunnel
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			return m, nil, err
		}
		tunnels = append(tunnels, t)
	}
	return m, tunnels, rows.Err()
}

func (p *Provisioner) GetMachineDetail(ctx context.Context, idOrSlug string) (MachineWithTunnels, error) {
	m, tunnels, err := p.getMachineAndTunnels(ctx, idOrSlug)
	if err != nil {
		return MachineWithTunnels{}, err
	}
	agent, err := p.loadAgentObservation(ctx, m.ID)
	if err != nil {
		return MachineWithTunnels{}, err
	}
	hub, err := p.loadHubObservation(ctx, m.ID)
	if err != nil {
		return MachineWithTunnels{}, err
	}
	commands, err := p.loadMachineOpenCommands(ctx, m.ID)
	if err != nil {
		return MachineWithTunnels{}, err
	}
	var update *AgentUpdateHint
	if agent != nil {
		update = agentUpdateHintFor(p.cfg, agent.AgentVersion)
	}
	return MachineWithTunnels{Machine: m, Tunnels: tunnels, Agent: agent, HubObservation: hub, Commands: commands, Update: update}, nil
}

func (p *Provisioner) loadAgentObservation(ctx context.Context, machineID string) (*AgentObservation, error) {
	a, err := scanAgentObservation(p.store.db.QueryRowContext(ctx, `SELECT machine_id,agent_version,install_id,applied_generation,heartbeat_at,agent_state,transport,transport_state,local_ssh_state,local_ssh_error,tunnel_state,tunnel_pid,tunnel_error,persistence_backend,persistence_quality,persistence_reboot_safe,authorized_key_fingerprints,last_error FROM agent_observations WHERE machine_id=?`, machineID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (p *Provisioner) loadHubObservation(ctx context.Context, machineID string) (*HubObservation, error) {
	var h HubObservation
	var ok, probeErr sql.NullString
	err := p.store.db.QueryRowContext(ctx, `SELECT tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error FROM hub_observations WHERE machine_id=? ORDER BY last_probe_at DESC,tunnel_id DESC LIMIT 1`, machineID).Scan(&h.TunnelID, &h.MachineID, &h.ProbeState, &h.LastProbeAt, &ok, &probeErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.LastSuccessAt = stringPtrFromNull(ok)
	h.ProbeError = probeErr.String
	return &h, nil
}

func (p *Provisioner) loadMachineOpenCommands(ctx context.Context, machineID string) ([]AgentCommandInfo, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT id,machine_id,generation,type,payload_json,status,created_at,sent_at,acked_at,expires_at,last_error FROM agent_commands WHERE machine_id=? AND status IN ('pending','sent','failed') ORDER BY created_at`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentCommandInfo
	for rows.Next() {
		c, err := scanAgentCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListMachines returns the fleet. Retired machines are history and are left
// out unless includeRetired is set (perf #7).
func (p *Provisioner) ListMachines(ctx context.Context, includeRetired ...bool) ([]MachineWithTunnels, error) {
	q := `SELECT ` + machineCols + ` FROM machines`
	if len(includeRetired) == 0 || !includeRetired[0] {
		q += ` WHERE desired_state<>'retired'`
	}
	rows, err := p.store.db.QueryContext(ctx, q+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	var machines []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		machines = append(machines, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tunnelRows, err := p.store.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	byMachine := map[string][]Tunnel{}
	for tunnelRows.Next() {
		t, err := scanTunnel(tunnelRows)
		if err != nil {
			_ = tunnelRows.Close()
			return nil, err
		}
		byMachine[t.MachineID] = append(byMachine[t.MachineID], t)
	}
	if err := tunnelRows.Close(); err != nil {
		return nil, err
	}

	agents, err := p.loadAgentObservations(ctx)
	if err != nil {
		return nil, err
	}
	hubs, err := p.loadHubObservations(ctx)
	if err != nil {
		return nil, err
	}
	commands, err := p.loadOpenCommands(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MachineWithTunnels, 0, len(machines))
	for _, m := range machines {
		var update *AgentUpdateHint
		if a := agents[m.ID]; a != nil {
			update = agentUpdateHintFor(p.cfg, a.AgentVersion)
		}
		item := MachineWithTunnels{Machine: m, Tunnels: byMachine[m.ID], Agent: agents[m.ID], HubObservation: hubs[m.ID], Commands: commands[m.ID], Update: update}
		out = append(out, item)
	}
	return out, nil
}

func (p *Provisioner) loadAgentObservations(ctx context.Context) (map[string]*AgentObservation, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT machine_id,agent_version,install_id,applied_generation,heartbeat_at,agent_state,transport,transport_state,local_ssh_state,local_ssh_error,tunnel_state,tunnel_pid,tunnel_error,persistence_backend,persistence_quality,persistence_reboot_safe,authorized_key_fingerprints,last_error FROM agent_observations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*AgentObservation{}
	for rows.Next() {
		a, err := scanAgentObservation(rows)
		if err != nil {
			return nil, err
		}
		out[a.MachineID] = &a
	}
	return out, rows.Err()
}

func scanAgentObservation(rows interface{ Scan(dest ...any) error }) (AgentObservation, error) {
	var a AgentObservation
	var agentVersion, installID, agentState, transport, transportState, localSSHState, localSSHError, tunnelState, tunnelError, persistenceBackend, persistenceQuality, lastError sql.NullString
	var pid sql.NullInt64
	var keyFP sql.NullString
	var reboot int
	err := rows.Scan(&a.MachineID, &agentVersion, &installID, &a.AppliedGeneration, &a.HeartbeatAt, &agentState, &transport, &transportState, &localSSHState, &localSSHError, &tunnelState, &pid, &tunnelError, &persistenceBackend, &persistenceQuality, &reboot, &keyFP, &lastError)
	if err != nil {
		return a, err
	}
	a.AgentVersion = agentVersion.String
	a.InstallID = installID.String
	a.AgentState = agentState.String
	a.Transport = transport.String
	a.TransportState = transportState.String
	a.LocalSSHState = localSSHState.String
	a.LocalSSHError = localSSHError.String
	a.TunnelState = tunnelState.String
	a.TunnelError = tunnelError.String
	a.PersistenceBackend = persistenceBackend.String
	a.PersistenceQuality = persistenceQuality.String
	a.LastError = lastError.String
	a.TunnelPID = intPtrFromNull(pid)
	a.PersistenceRebootSafe = reboot != 0
	if keyFP.Valid {
		a.AuthorizedKeyFingerprints = json.RawMessage(keyFP.String)
	}
	return a, nil
}

func (p *Provisioner) loadHubObservations(ctx context.Context) (map[string]*HubObservation, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error FROM (
		SELECT tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error,
			ROW_NUMBER() OVER (PARTITION BY machine_id ORDER BY last_probe_at DESC,tunnel_id DESC) AS row_num
		FROM hub_observations
	) WHERE row_num=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*HubObservation{}
	for rows.Next() {
		var h HubObservation
		var ok, probeErr sql.NullString
		if err := rows.Scan(&h.TunnelID, &h.MachineID, &h.ProbeState, &h.LastProbeAt, &ok, &probeErr); err != nil {
			return nil, err
		}
		h.LastSuccessAt = stringPtrFromNull(ok)
		h.ProbeError = probeErr.String
		out[h.MachineID] = &h
	}
	return out, rows.Err()
}

func (p *Provisioner) loadOpenCommands(ctx context.Context) (map[string][]AgentCommandInfo, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT id,machine_id,generation,type,payload_json,status,created_at,sent_at,acked_at,expires_at,last_error FROM agent_commands WHERE status IN ('pending','sent','failed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]AgentCommandInfo{}
	for rows.Next() {
		c, err := scanAgentCommand(rows)
		if err != nil {
			return nil, err
		}
		out[c.MachineID] = append(out[c.MachineID], c)
	}
	return out, rows.Err()
}

func scanAgentCommand(rows interface{ Scan(dest ...any) error }) (AgentCommandInfo, error) {
	var c AgentCommandInfo
	var payload, sent, acked, exp, last sql.NullString
	err := rows.Scan(&c.ID, &c.MachineID, &c.Generation, &c.Type, &payload, &c.Status, &c.CreatedAt, &sent, &acked, &exp, &last)
	if payload.Valid {
		c.Payload = json.RawMessage(payload.String)
	}
	c.SentAt = stringPtrFromNull(sent)
	c.AckedAt = stringPtrFromNull(acked)
	c.ExpiresAt = stringPtrFromNull(exp)
	c.LastError = last.String
	return c, err
}

func (p *Provisioner) publish(typ, machineID string, data map[string]any) {
	if p.publishEvent != nil {
		p.publishEvent(typ, machineID, data)
	}
}

const (
	// keyscanInterval: a listening tunnel is only re-verified with
	// ssh-keyscan this often. Listener changes are still seen every round.
	keyscanInterval = 5 * time.Minute
	// dormantAfter: a tunnel that is down and whose machine has not sent a
	// heartbeat for this long only gets its unchanged observation row
	// refreshed every dormantRefresh (it is still probed each round via the
	// shared ss output, which costs nothing per tunnel).
	dormantAfter   = 24 * time.Hour
	dormantRefresh = time.Hour
	// hubObservationRefresh bounds how stale last_probe_at may get when
	// nothing changes, so the dashboard still sees the probe is alive.
	hubObservationRefresh = 10 * time.Minute
)

// RunHealthCheck probes every live tunnel. One `ss -Htln` per round replaces
// one fork per tunnel; ssh-keyscan only runs when a listener appears or its
// last success is older than keyscanInterval; DB rows are written only when
// something changed (perf #5, #6).
func (p *Provisioner) RunHealthCheck(ctx context.Context) ([]map[string]any, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE status NOT IN ('disabled','retired','failed','cleanup_failed') AND remote_port>0`)
	if err != nil {
		return nil, err
	}
	var tunnels []Tunnel
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		tunnels = append(tunnels, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	type prevObs struct {
		state, probeErr, lastProbe, lastSuccess string
		ok                                      bool
	}
	prev := map[string]prevObs{}
	obsRows, err := p.store.db.QueryContext(ctx, `SELECT tunnel_id,probe_state,COALESCE(probe_error,''),last_probe_at,COALESCE(last_success_at,'') FROM hub_observations`)
	if err != nil {
		return nil, err
	}
	for obsRows.Next() {
		var id string
		var o prevObs
		if err := obsRows.Scan(&id, &o.state, &o.probeErr, &o.lastProbe, &o.lastSuccess); err != nil {
			_ = obsRows.Close()
			return nil, err
		}
		o.ok = true
		prev[id] = o
	}
	if err := obsRows.Close(); err != nil {
		return nil, err
	}
	lastHeartbeat := map[string]time.Time{}
	hbRows, err := p.store.db.QueryContext(ctx, `SELECT machine_id,heartbeat_at FROM agent_observations`)
	if err != nil {
		return nil, err
	}
	for hbRows.Next() {
		var mid, at string
		if hbRows.Scan(&mid, &at) == nil {
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				lastHeartbeat[mid] = t
			}
		}
	}
	_ = hbRows.Close()

	listening, ssErr := p.listeningPorts(ctx)
	nowT := time.Now().UTC()
	var report []map[string]any
	for _, t := range tunnels {
		po := prev[t.ID]
		var ok bool
		var detail string
		switch {
		case p.probeTunnel != nil:
			// Injected probe (tests).
			ok, detail = p.probeTunnel(ctx, t.RemotePort)
		case ssErr != nil:
			ok, detail = checkTunnelPort(ctx, t.RemotePort)
		case !listening[t.RemotePort]:
			ok, detail = false, "ss: port not listening"
		default:
			lastSuccess, _ := time.Parse(time.RFC3339, po.lastSuccess)
			if po.ok && po.state == "reachable" && nowT.Sub(lastSuccess) < keyscanInterval {
				ok, detail = true, "listening (keyscan cached)"
			} else {
				ok, detail = keyscanTunnel(ctx, t.RemotePort)
			}
		}
		// Dormant tunnels that stay down are written rarely.
		hb, hasHB := lastHeartbeat[t.MachineID]
		dormant := !ok && (!hasHB || nowT.Sub(hb) > dormantAfter)
		state := "unreachable"
		probeErr := detail
		if ok {
			state = "reachable"
			probeErr = ""
		}
		now := nowT.Format(time.RFC3339)
		nextTunnelStatus := "offline"
		if ok {
			nextTunnelStatus = "online"
		}
		lastProbe, _ := time.Parse(time.RFC3339, po.lastProbe)
		refreshAge := hubObservationRefresh
		if dormant {
			refreshAge = dormantRefresh
		}
		changed := !po.ok || po.state != state || po.probeErr != probeErr
		// storedProbeAt is the last_probe_at the DB holds after this round;
		// events carry it so a replayed event never looks fresher than the
		// observation behind it (astra dashboard R2-03).
		storedProbeAt := po.lastProbe
		if changed || (ok && po.state == "reachable" && detail != "listening (keyscan cached)") || nowT.Sub(lastProbe) >= refreshAge {
			storedProbeAt = now
			var lastSuccess any
			if ok {
				lastSuccess = now
			}
			if _, err := p.store.db.ExecContext(ctx, `INSERT INTO hub_observations (tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error) VALUES (?,?,?,?,?,?) ON CONFLICT(tunnel_id) DO UPDATE SET probe_state=excluded.probe_state,last_probe_at=excluded.last_probe_at,last_success_at=COALESCE(excluded.last_success_at,hub_observations.last_success_at),probe_error=excluded.probe_error`, t.ID, t.MachineID, state, now, lastSuccess, nullable(probeErr)); err != nil {
				return nil, err
			}
		}
		if t.Status != nextTunnelStatus {
			if _, err := p.store.db.ExecContext(ctx, `UPDATE tunnels SET status=?,last_seen_at=CASE WHEN ?='online' THEN ? ELSE last_seen_at END WHERE id=? AND status NOT IN ('disabled','retired','failed','cleanup_failed')`, nextTunnelStatus, nextTunnelStatus, now, t.ID); err != nil {
				return nil, err
			}
		}
		// recomputeObserved only writes when the derived state changes, and
		// must run every round because heartbeat staleness is time-based.
		machineChanged, observed, status, err := p.recomputeObserved(ctx, t.MachineID)
		if err != nil {
			return nil, err
		}
		if machineChanged || changed || t.Status != nextTunnelStatus {
			p.publish("machine.health_changed", t.MachineID, map[string]any{"machine_id": t.MachineID, "observed_state": observed, "status": status, "probe_state": state, "last_probe_at": storedProbeAt, "event_at": now})
		}
		report = append(report, map[string]any{"tunnel_id": t.ID, "machine_id": t.MachineID, "port": t.RemotePort, "ok": ok, "probe_state": state, "detail": detail, "checked_at": now})
	}
	p.healthMu.Lock()
	p.healthReport = report
	p.healthMu.Unlock()
	return report, nil
}

// listeningPorts runs `ss -Htln` once and returns loopback TCP listeners.
func (p *Provisioner) listeningPorts(ctx context.Context) (map[int]bool, error) {
	if p.listListeners != nil {
		return p.listListeners(ctx)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ss", "-Htln").Output()
	if err != nil {
		return nil, err
	}
	return parseSSListeners(string(out)), nil
}

// parseSSListeners extracts local ports from `ss -Htln` output, e.g.
// "LISTEN 0 128 127.0.0.1:9208 0.0.0.0:*".
func parseSSListeners(out string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		if n, err := strconv.Atoi(local[i+1:]); err == nil {
			ports[n] = true
		}
	}
	return ports
}

func keyscanTunnel(ctx context.Context, port int) (bool, string) {
	out, err := exec.CommandContext(ctx, "ssh-keyscan", "-T", "3", "-p", strconv.Itoa(port), "127.0.0.1").CombinedOutput()
	if err != nil {
		return false, "listener ok, ssh-keyscan failed: " + strings.TrimSpace(string(out))
	}
	s := strings.TrimSpace(string(out))
	if !strings.Contains(s, "ssh-") && !strings.Contains(s, "ecdsa-") {
		return false, "no ssh host key in banner"
	}
	return true, firstLine(s)
}

func (p *Provisioner) recomputeObserved(ctx context.Context, machineID string) (bool, string, string, error) {
	p.observedMu.Lock()
	defer p.observedMu.Unlock()

	var desired, old, oldStatus string
	var cleanup sql.NullString
	var hbAt, agentTunnel, localSSH, probe sql.NullString
	if err := p.store.db.QueryRowContext(ctx, `SELECT desired_state,observed_state,status,cleanup_state FROM machines WHERE id=?`, machineID).Scan(&desired, &old, &oldStatus, &cleanup); err != nil {
		return false, "", "", err
	}
	if err := p.store.db.QueryRowContext(ctx, `SELECT heartbeat_at,tunnel_state,local_ssh_state FROM agent_observations WHERE machine_id=?`, machineID).Scan(&hbAt, &agentTunnel, &localSSH); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", "", err
	}
	if err := p.store.db.QueryRowContext(ctx, `SELECT probe_state FROM hub_observations WHERE machine_id=? ORDER BY last_probe_at DESC,tunnel_id DESC LIMIT 1`, machineID).Scan(&probe); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", "", err
	}
	observed := ObservedUnknown
	probeReachable := probe.String == "reachable"
	probeUnreachable := probe.String == "unreachable"
	if hbAt.Valid {
		last, err := time.Parse(time.RFC3339, hbAt.String)
		staleHeartbeat := err != nil || time.Since(last) > p.cfg.OfflineAfter*2
		if staleHeartbeat && probeReachable {
			observed = ObservedDegraded
		} else if staleHeartbeat && probeUnreachable {
			observed = ObservedGone
		} else if staleHeartbeat {
			observed = ObservedOffline
		} else if agentTunnel.String == "connected" && probeReachable && localSSH.String == "healthy" {
			observed = ObservedOnline
		} else if agentTunnel.String == "connected" || probeReachable {
			observed = ObservedDegraded
		} else {
			observed = ObservedOffline
		}
	} else if probeReachable {
		observed = ObservedDegraded
	}
	effective := effectiveStatus(desired, cleanup.String, observed)
	changed := observed != old || effective != oldStatus
	if changed {
		// Conditional on the lifecycle columns we read: if a disable/retire
		// landed in between, this stale recompute must not overwrite it.
		res, err := p.store.db.ExecContext(ctx, `UPDATE machines SET observed_state=?,status=?,updated_at=? WHERE id=? AND desired_state=? AND COALESCE(cleanup_state,'')=?`, observed, effective, nowUTC(), machineID, desired, cleanup.String)
		if err != nil {
			return false, "", "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, old, oldStatus, nil
		}
	}
	return changed, observed, effective, nil
}

// effectiveStatus is the single derivation of machines.status. A recorded
// revoke/cleanup failure wins over the desired state, so a heartbeat cannot
// make a machine whose hub access may still work look disabled or plainly
// retiring (astra P2-01). Only a successful retry clears cleanup_state.
func effectiveStatus(desired, cleanup, observed string) string {
	switch cleanup {
	case DisableFailed, CleanupFailed:
		return cleanup
	}
	switch desired {
	case DesiredDisabled, DesiredRetiring, DesiredRetired:
		return desired
	}
	return observed
}

func (p *Provisioner) CachedHealth(ctx context.Context) ([]map[string]any, error) {
	p.healthMu.RLock()
	if p.healthReport != nil {
		out := make([]map[string]any, len(p.healthReport))
		copy(out, p.healthReport)
		p.healthMu.RUnlock()
		return out, nil
	}
	p.healthMu.RUnlock()
	rows, err := p.store.db.QueryContext(ctx, `SELECT tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error FROM hub_observations ORDER BY last_probe_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var report []map[string]any
	for rows.Next() {
		var id, mid, state, at string
		var success, e sql.NullString
		if err := rows.Scan(&id, &mid, &state, &at, &success, &e); err != nil {
			return nil, err
		}
		report = append(report, map[string]any{"tunnel_id": id, "machine_id": mid, "probe_state": state, "last_probe_at": at, "last_success_at": success.String, "probe_error": e.String})
	}
	return report, rows.Err()
}

func checkTunnelPort(ctx context.Context, port int) (bool, string) {
	portStr := strconv.Itoa(port)
	ssOut, ssErr := exec.CommandContext(ctx, "ss", "-H", "-tln", "sport", "=", ":"+portStr).CombinedOutput()
	if ssErr == nil {
		if !strings.Contains(string(ssOut), ":"+portStr) {
			return false, "ss: port not listening"
		}
	} else {
		addr := net.JoinHostPort("127.0.0.1", portStr)
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false, err.Error()
		}
		_ = conn.Close()
	}
	cmd := exec.CommandContext(ctx, "ssh-keyscan", "-T", "3", "-p", portStr, "127.0.0.1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "listener ok, ssh-keyscan failed: " + strings.TrimSpace(string(out))
	}
	s := strings.TrimSpace(string(out))
	if !strings.Contains(s, "ssh-") && !strings.Contains(s, "ecdsa-") {
		return false, "no ssh host key in banner"
	}
	return true, firstLine(s)
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return s
}

func (p *Provisioner) StartHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.HealthInterval)
	go func() {
		defer ticker.Stop()
		if _, err := p.RunHealthCheck(ctx); err != nil {
			log.Printf("health check error: %v", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := p.RunHealthCheck(ctx); err != nil {
					log.Printf("health check error: %v", err)
				}
			}
		}
	}()
}

func (p *Provisioner) ExpireOnce(ctx context.Context) error {
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	if _, err := p.store.db.ExecContext(ctx, `DELETE FROM jwt_blacklist WHERE expires_at<=?`, now); err != nil {
		return err
	}
	eventCutoff := nowTime.Add(-eventRetention).Format(time.RFC3339)
	if p.pruneEvents != nil {
		if err := p.pruneEvents(ctx, eventCutoff); err != nil {
			return err
		}
	} else if _, err := p.store.db.ExecContext(ctx, `DELETE FROM events WHERE created_at<?`, eventCutoff); err != nil {
		return err
	}
	if _, err := p.store.db.ExecContext(ctx, `DELETE FROM rate_limits WHERE reset_at<=?`, now); err != nil {
		return err
	}
	if _, err := p.store.db.ExecContext(ctx, `UPDATE requests SET status='expired' WHERE status IN ('pending','approved') AND expires_at < ?`, now); err != nil {
		return err
	}
	if _, err := p.store.db.ExecContext(ctx, `UPDATE requests SET status='expired' WHERE status='approved' AND setup_token_expires_at IS NOT NULL AND setup_token_expires_at < ?`, now); err != nil {
		return err
	}
	if err := p.recoverStuckProvisioning(ctx, nowTime); err != nil {
		return err
	}
	// Release invite claims whose request expired or was denied without
	// provisioning, so a legitimate invite is not burned by a failed attempt.
	if _, err := p.store.db.ExecContext(ctx, `UPDATE invites SET claimed_by_request_id=NULL, claimed_at=NULL WHERE used_by IS NULL AND claimed_by_request_id IN (SELECT id FROM requests WHERE status IN ('expired','denied'))`); err != nil {
		return err
	}
	if err := p.expireCommands(ctx, now); err != nil {
		return err
	}
	if err := p.flagStuckRetiring(ctx, nowTime); err != nil {
		return err
	}
	if err := p.verifyRetiredTunnels(ctx); err != nil {
		log.Printf("verify retired tunnels: %v", err)
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT id FROM machines WHERE desired_state NOT IN ('disabled','retiring','retired') AND expires_at IS NOT NULL AND expires_at < ?`, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := p.DisableMachine(ctx, id, "system-expire"); err != nil {
			log.Printf("auto-expire disable %s: %v", id, err)
		}
	}
	return nil
}

// provisioningLease bounds how long a request may stay "provisioning".
const provisioningLease = 10 * time.Minute

// errProvisioningLeaseLost: the request left the provisioning state while
// this provision ran (maintenance reclaimed the expired lease).
var errProvisioningLeaseLost = errors.New("provisioning lease lost")

// provisionRecoveryActor marks retires started by provisioning recovery so
// maintenance keeps retrying their cleanup.
const provisionRecoveryActor = "system-provision-recovery"

// Provisioning phases (requests.provision_phase), daybreak round 4 P2-02:
//
//	claimed           setup token consumed, nothing created yet
//	os_created        machine/tunnel rows and the hub account exist
//	response_pending  request provisioned; the agent may not have the token
//	delivered         the agent heartbeated with its token
//	cleanup_pending   recovery took over: revoke and retire the machine
//	cleaned           the machine is retired (or nothing was created)
//
// Every transition before delivered is resumable: recovery picks up any
// phase whose lease expired, and cleanup_pending until it is cleaned.
const (
	phaseClaimed         = "claimed"
	phaseOSCreated       = "os_created"
	phaseResponsePending = "response_pending"
	phaseDelivered       = "delivered"
	phaseCleanupPending  = "cleanup_pending"
	phaseCleaned         = "cleaned"
)

// responseLease is how long an agent has to heartbeat after its provision
// response was committed. Agents heartbeat within seconds of installing.
const responseLease = time.Hour

// recoverStuckProvisioning handles a reachd crash (or a lost response) in
// the middle of a provision (daybreak P2-7, round 3 P2-A, round 4 P2-02,
// astra R2-10, R3-05). The attempt cannot be resumed: the agent token only
// ever existed in the response. So:
//
//  1. claim: with a CAS on the request (status and phase), move it to
//     cleanup_pending, revoke the machine's agent token and journal it,
//     all in one transaction. cleanup_pending is the durable retry marker.
//  2. cleanup: every cleanup_pending request with a machine is retired
//     through the normal locked, verified path; only when the machine is
//     retired does the request become cleaned. Failures are retried by
//     the next maintenance run, forever, never dropped.
func (p *Provisioner) recoverStuckProvisioning(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-provisioningLease).Format(time.RFC3339)
	respCutoff := now.Add(-responseLease).Format(time.RFC3339)
	rows, err := p.store.db.QueryContext(ctx, `SELECT id FROM requests WHERE
		(status='provisioning' AND (provisioning_started_at IS NULL OR provisioning_started_at<?))
		OR (status='provisioned' AND provision_phase='response_pending' AND provisioning_started_at<?)`, cutoff, respCutoff)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		var machineID string
		claimed := false
		err := p.inTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE requests SET status='expired', provision_phase=? WHERE id=? AND (
				(status='provisioning' AND (provisioning_started_at IS NULL OR provisioning_started_at<?))
				OR (status='provisioned' AND provision_phase='response_pending' AND provisioning_started_at<?))`, phaseCleanupPending, id, cutoff, respCutoff)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil // finished or reclaimed meanwhile
			}
			claimed = true
			var m sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM requests WHERE id=?`, id).Scan(&m); err != nil {
				return err
			}
			machineID = m.String
			if machineID != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE machines SET agent_token_hash=NULL, provision_error=COALESCE(provision_error,'provision interrupted; Reach is removing it'), updated_at=? WHERE id=?`, nowUTC(), machineID); err != nil {
					return err
				}
			}
			_, err = recordDurable(ctx, tx, "request.provisioning_recovered", machineID, "system", durableDone)
			return err
		})
		if err != nil {
			return err
		}
		if claimed {
			p.store.Audit(ctx, "request.provisioning_interrupted", "system", machineID, "request="+id)
		}
	}
	return p.runProvisionCleanup(ctx)
}

// runProvisionCleanup retires the machine of every cleanup_pending request
// until it is retired, then marks the request cleaned.
func (p *Provisioner) runProvisionCleanup(ctx context.Context) error {
	// Requests recovered by the previous release carry no phase; put their
	// unfinished machines into the phase machine.
	if _, err := p.store.db.ExecContext(ctx, `UPDATE requests SET provision_phase=? WHERE status='expired' AND (provision_phase IS NULL OR provision_phase='') AND machine_id IN (SELECT id FROM machines WHERE desired_changed_by=? AND desired_state<>'retired')`, phaseCleanupPending, provisionRecoveryActor); err != nil {
		return err
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT id,COALESCE(machine_id,'') FROM requests WHERE provision_phase=?`, phaseCleanupPending)
	if err != nil {
		return err
	}
	type pending struct{ req, machine string }
	var list []pending
	for rows.Next() {
		var pd pending
		if err := rows.Scan(&pd.req, &pd.machine); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, pd)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, pd := range list {
		if pd.machine != "" {
			m, err := p.GetMachine(ctx, pd.machine)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("provisioning cleanup %s: %v", pd.req, err)
				continue
			}
			if err == nil && m.DesiredState != DesiredRetired {
				if _, err := p.SetDesiredState(ctx, pd.machine, DesiredRetired, provisionRecoveryActor); err != nil {
					log.Printf("provisioning cleanup: retire %s: %v (retried by the next maintenance run)", pd.machine, err)
					continue
				}
			}
		}
		if _, err := p.store.db.ExecContext(ctx, `UPDATE requests SET provision_phase=? WHERE id=? AND provision_phase=?`, phaseCleaned, pd.req, phaseCleanupPending); err != nil {
			return err
		}
	}
	return nil
}

// expireCommands marks commands past expires_at so they stop showing as open
// work (perf #7).
func (p *Provisioner) expireCommands(ctx context.Context, now string) error {
	_, err := p.store.db.ExecContext(ctx, `UPDATE agent_commands SET status='expired', last_error=COALESCE(last_error,'expired before the agent acknowledged it') WHERE status IN ('pending','sent') AND expires_at IS NOT NULL AND expires_at<=?`, now)
	return err
}

// flagStuckRetiring marks machines whose agent never confirmed uninstall.
// It does not retire them: the operator decides with retire-now, which runs
// the full verified cleanup.
func (p *Provisioner) flagStuckRetiring(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-p.retiringTimeout()).Format(time.RFC3339)
	rows, err := p.store.db.QueryContext(ctx, `SELECT id FROM machines WHERE desired_state='retiring' AND (cleanup_state IS NULL OR cleanup_state=?) AND desired_changed_at IS NOT NULL AND desired_changed_at<?`, CleanupPending, cutoff)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		res, err := p.store.db.ExecContext(ctx, `UPDATE machines SET cleanup_state=?, updated_at=? WHERE id=? AND desired_state='retiring' AND (cleanup_state IS NULL OR cleanup_state=?)`, CleanupTimedOut, nowUTC(), id, CleanupPending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.store.Audit(ctx, "machine.retiring_timed_out", "system", id, "")
			p.publish("machine.retiring_timed_out", id, map[string]any{"machine_id": id, "cleanup_state": CleanupTimedOut})
		}
	}
	return nil
}

// verifyRetiredTunnels checks tunnels retired by older releases (which did
// not verify cleanup) and releases their original port once nothing is left
// on the hub. Until then the allocator treats the port as taken.
func (p *Provisioner) verifyRetiredTunnels(ctx context.Context) error {
	rows, err := p.store.db.QueryContext(ctx, `SELECT id,original_unix_user FROM tunnels WHERE status='retired' AND (cleanup_verified_at IS NULL OR cleanup_verified_at='') LIMIT 50`)
	if err != nil {
		return err
	}
	type pending struct{ id, user string }
	var list []pending
	for rows.Next() {
		var id string
		var u sql.NullString
		if err := rows.Scan(&id, &u); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, pending{id, u.String})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range list {
		if ValidateUnixUser(t.user) != nil {
			continue
		}
		residue, err := p.ops().Residue(ctx, t.user)
		if err != nil {
			return err
		}
		if len(residue) > 0 {
			log.Printf("retired tunnel %s (%s) still has hub residue: %s", t.id, t.user, strings.Join(residue, ", "))
			continue
		}
		if _, err := p.store.db.ExecContext(ctx, `UPDATE tunnels SET cleanup_verified_at=? WHERE id=?`, nowUTC(), t.id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provisioner) StartMaintenanceLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.MaintenanceInterval)
	go func() {
		defer ticker.Stop()
		if err := p.ExpireOnce(ctx); err != nil {
			log.Printf("maintenance error: %v", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.ExpireOnce(ctx); err != nil {
					log.Printf("maintenance error: %v", err)
				}
			}
		}
	}()
}
