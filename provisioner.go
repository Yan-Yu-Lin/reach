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
	probeTunnel  func(context.Context, int) (bool, string)
	observedMu   sync.Mutex
	healthMu     sync.RWMutex
	healthReport []map[string]any
}

type CreateMachineInput struct {
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
	return &Provisioner{store: store, cfg: cfg, probeTunnel: checkTunnelPort}
}

func (p *Provisioner) CreateMachine(ctx context.Context, in CreateMachineInput) (CreateMachineResult, error) {
	if err := ValidateSlug(in.Slug); err != nil {
		return CreateMachineResult{}, err
	}
	if err := ValidateTargetUser(in.TargetUser); err != nil {
		return CreateMachineResult{}, err
	}
	if _, err := ValidateSSHPublicKey(in.Pubkey); err != nil {
		return CreateMachineResult{}, fmt.Errorf("invalid tunnel public key: %w", err)
	}
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
	agentHash, err := HashSecret(agentToken)
	if err != nil {
		return res, err
	}
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
	port, err := allocatePortTx(ctx, tx, hub)
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
	if err := tx.Commit(); err != nil {
		return res, err
	}

	if p.cfg.ProvisioningEnabled {
		if err := p.createSystemAccount(ctx, unixUser, port, in.Pubkey, in.ExpiresAt); err != nil {
			p.markProvisionFailed(ctx, machineID, tunnelID, err)
			_ = p.cleanupSystem(ctx, unixUser)
			return res, err
		}
	}
	_, err = p.store.db.ExecContext(ctx, `UPDATE machines SET status='offline', observed_state=?, cleanup_state=NULL, updated_at=? WHERE id=?`, ObservedOffline, nowUTC(), machineID)
	if err != nil {
		return res, err
	}
	p.store.Audit(ctx, "machine.create", "system", machineID, fmt.Sprintf("slug=%s tunnel=%s port=%d", in.Slug, unixUser, port))
	res.Machine, _ = p.GetMachine(ctx, machineID)
	res.Tunnel, _ = p.GetTunnel(ctx, tunnelID)
	res.Hub = hub
	res.AgentToken = agentToken
	return res, nil
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

func allocatePortTx(ctx context.Context, tx *sql.Tx, h Hub) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT remote_port FROM tunnels WHERE hub_id=? AND status NOT IN ('retired') AND remote_port BETWEEN ? AND ?`, h.ID, h.PortStart, h.PortEnd)
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
		if used[port] || reservedTunnelPort(port) || portListening(port) {
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
	line := authorizedKeyLine(port, pubkey, expiresAt)
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

func authorizedKeyLine(port int, pubkey, expiresAt string) string {
	opts := []string{"restrict", "port-forwarding", fmt.Sprintf("permitlisten=\"127.0.0.1:%d\"", port)}
	if expiresAt != "" {
		if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			opts = append(opts, fmt.Sprintf("expiry-time=\"%s\"", t.UTC().Format("20060102150405Z")))
		}
	}
	return strings.Join(opts, ",") + " " + strings.TrimSpace(pubkey)
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

func (p *Provisioner) SetDesiredState(ctx context.Context, idOrSlug, desired, actor string) (Machine, error) {
	m, tunnels, err := p.getMachineAndTunnels(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	now := nowUTC()
	gen := m.DesiredGeneration + 1
	switch desired {
	case DesiredActive:
		for _, t := range tunnels {
			_ = p.enableTunnelAuth(ctx, t)
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status=CASE WHEN observed_state='online' THEN 'online' ELSE 'offline' END, disabled_at=NULL, cleanup_state=NULL, updated_at=? WHERE id=?`, desired, gen, now, actor, now, m.ID)
		if err == nil {
			_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='offline' WHERE machine_id=? AND status='disabled'`, m.ID)
		}
		_ = p.QueueCommand(ctx, m.ID, gen, "sync_keys", map[string]any{"enabled": true}, time.Hour)
		_ = p.QueueCommand(ctx, m.ID, gen, "start_tunnel", nil, time.Hour)
	case DesiredDisabled:
		for _, t := range tunnels {
			_ = p.disableTunnelAuth(ctx, t)
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='disabled', disabled_at=?, cleanup_state=NULL, updated_at=? WHERE id=?`, desired, gen, now, actor, now, now, m.ID)
		if err == nil {
			_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='disabled' WHERE machine_id=?`, m.ID)
		}
		_ = p.QueueCommand(ctx, m.ID, gen, "stop_tunnel", nil, time.Hour)
		_ = p.QueueCommand(ctx, m.ID, gen, "sync_keys", map[string]any{"enabled": false}, time.Hour)
	case DesiredRetiring:
		for _, t := range tunnels {
			_ = p.disableTunnelAuth(ctx, t)
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE machines SET desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='retiring', cleanup_state='pending', updated_at=? WHERE id=?`, desired, gen, now, actor, now, m.ID)
		if err == nil {
			_, _ = p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='disabled' WHERE machine_id=?`, m.ID)
		}
		_ = p.QueueCommand(ctx, m.ID, gen, "uninstall", map[string]any{"remove_agent": true, "remove_config": true, "remove_data": true, "remove_admin_keys": true, "stop_local_user_sshd": true}, 24*time.Hour)
	case DesiredRetired:
		for _, t := range tunnels {
			cleanupUser := t.UnixUser
			if t.OriginalUnixUser != nil && *t.OriginalUnixUser != "" {
				cleanupUser = *t.OriginalUnixUser
			}
			if t.Status != "retired" {
				_ = p.removeTunnelSystem(ctx, cleanupUser)
			}
			_, _ = p.retireTunnelRow(ctx, t)
		}
		original := m.Slug
		if m.OriginalSlug != nil && *m.OriginalSlug != "" {
			original = *m.OriginalSlug
		}
		retiredSlug, slugErr := p.retiredSlug(ctx, original, m.ID)
		if slugErr != nil {
			return Machine{}, slugErr
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE machines SET slug=?, original_slug=COALESCE(NULLIF(original_slug,''),?), desired_state=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, status='retired', cleanup_state=COALESCE(cleanup_state,'server-only'), retired_at=COALESCE(retired_at,?), updated_at=? WHERE id=?`, retiredSlug, original, desired, gen, now, actor, now, now, m.ID)
	default:
		return Machine{}, fmt.Errorf("invalid desired state %q", desired)
	}
	if err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.desired."+desired, actor, m.ID, "")
	return p.GetMachine(ctx, m.ID)
}

func (p *Provisioner) SetUpdatePolicy(ctx context.Context, idOrSlug, policy, actor string) (Machine, error) {
	policy, err := normalizeUpdatePolicy(policy)
	if err != nil {
		return Machine{}, err
	}
	m, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	now := nowUTC()
	if _, err := p.store.db.ExecContext(ctx, `UPDATE machines SET update_policy=?, updated_at=? WHERE id=?`, policy, now, m.ID); err != nil {
		return Machine{}, err
	}
	p.store.Audit(ctx, "machine.update_policy.update", actor, m.ID, "policy="+policy)
	return p.GetMachine(ctx, m.ID)
}

func (p *Provisioner) SetProcessTitleConfig(ctx context.Context, idOrSlug string, cfg *ProcessTitleConfig, actor string) (Machine, error) {
	m, err := p.GetMachine(ctx, idOrSlug)
	if err != nil {
		return Machine{}, err
	}
	var raw any
	if cfg != nil {
		clean, err := normalizeDesiredProcessTitleConfig(*cfg)
		if err != nil {
			return Machine{}, err
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
	if _, err := p.store.db.ExecContext(ctx, `UPDATE machines SET process_title_config_json=?, desired_generation=?, desired_changed_at=?, desired_changed_by=?, updated_at=? WHERE id=?`, raw, gen, now, actor, now, m.ID); err != nil {
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

func (p *Provisioner) retireTunnelRow(ctx context.Context, t Tunnel) (Tunnel, error) {
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
		if err := p.store.db.QueryRowContext(ctx, `SELECT MIN(remote_port) FROM tunnels WHERE hub_id=? AND remote_port<0`, t.HubID).Scan(&min); err != nil {
			return t, err
		}
		retiredPort = -1
		if min.Valid {
			retiredPort = int(min.Int64) - 1
		}
	}
	_, err := p.store.db.ExecContext(ctx, `UPDATE tunnels SET status='retired', original_unix_user=COALESCE(NULLIF(original_unix_user,''),?), original_remote_port=COALESCE(NULLIF(original_remote_port,0),?), unix_user=?, remote_port=? WHERE id=?`, originalUser, originalPort, retiredUser, retiredPort, t.ID)
	if err != nil {
		return t, err
	}
	t.UnixUser = retiredUser
	t.RemotePort = retiredPort
	t.Status = "retired"
	return t, nil
}

func (p *Provisioner) QueueCommand(ctx context.Context, machineID string, generation int64, typ string, payload any, ttl time.Duration) error {
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
	_, err = p.store.db.ExecContext(ctx, `INSERT INTO agent_commands (id,machine_id,generation,type,payload_json,status,created_at,expires_at) VALUES (?,?,?,?,?,?,?,?)`, id, machineID, generation, typ, nullable(string(raw)), "pending", nowUTC(), nullable(expires))
	return err
}

func (p *Provisioner) disableTunnelAuth(ctx context.Context, t Tunnel) error {
	if err := ValidateUnixUser(t.UnixUser); err != nil {
		return err
	}
	ak := filepath.Join(p.cfg.TunnelHomeBase, t.UnixUser, ".ssh", "authorized_keys")
	if _, err := os.Stat(ak); err == nil {
		_ = os.Rename(ak, ak+".disabled")
	}
	_ = killUser(ctx, t.UnixUser)
	return nil
}

func (p *Provisioner) enableTunnelAuth(ctx context.Context, t Tunnel) error {
	if err := ValidateUnixUser(t.UnixUser); err != nil {
		return err
	}
	ak := filepath.Join(p.cfg.TunnelHomeBase, t.UnixUser, ".ssh", "authorized_keys")
	if _, err := os.Stat(ak); os.IsNotExist(err) {
		if _, err := os.Stat(ak + ".disabled"); err == nil {
			_ = os.Rename(ak+".disabled", ak)
		}
	}
	return nil
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

func (p *Provisioner) removeTunnelSystem(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	var errs []string
	_ = killUser(ctx, unixUser)
	if err := run(ctx, "userdel", unixUser); err != nil && !strings.Contains(err.Error(), "does not exist") {
		errs = append(errs, err.Error())
	}
	conf := filepath.Join(p.cfg.SSHDConfigDir, "90-"+unixUser+".conf")
	if err := os.Remove(conf); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err.Error())
	}
	home := filepath.Join(p.cfg.TunnelHomeBase, unixUser)
	if err := validateHomePath(home); err != nil {
		errs = append(errs, err.Error())
	} else if err := os.RemoveAll(home); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (p *Provisioner) cleanupSystem(ctx context.Context, unixUser string) error {
	return p.removeTunnelSystem(ctx, unixUser)
}

func validateHomePath(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/var/lib/rt-") {
		return fmt.Errorf("refusing unsafe home path %s", clean)
	}
	return ValidateUnixUser(filepath.Base(clean))
}

func killUser(ctx context.Context, unixUser string) error {
	if err := ValidateUnixUser(unixUser); err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, "pkill", "-TERM", "-u", unixUser).Run()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.CommandContext(ctx, "pgrep", "-u", unixUser).Run() != nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = exec.CommandContext(ctx, "pkill", "-KILL", "-u", unixUser).Run()
	return nil
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

func (p *Provisioner) ListMachines(ctx context.Context) ([]MachineWithTunnels, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT `+machineCols+` FROM machines ORDER BY created_at DESC`)
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

func (p *Provisioner) RunHealthCheck(ctx context.Context) ([]map[string]any, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE status NOT IN ('disabled','retired','failed')`)
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
	var report []map[string]any
	for _, t := range tunnels {
		probe := p.probeTunnel
		if probe == nil {
			probe = checkTunnelPort
		}
		ok, detail := probe(ctx, t.RemotePort)
		now := nowUTC()
		state := "unreachable"
		lastSuccess := sql.NullString{}
		probeErr := detail
		if ok {
			state = "reachable"
			lastSuccess = sql.NullString{String: now, Valid: true}
			probeErr = ""
		}
		var previousProbe, previousProbeError sql.NullString
		err := p.store.db.QueryRowContext(ctx, `SELECT probe_state,probe_error FROM hub_observations WHERE tunnel_id=?`, t.ID).Scan(&previousProbe, &previousProbeError)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		nextTunnelStatus := "offline"
		if ok {
			nextTunnelStatus = "online"
		}
		if _, err := p.store.db.ExecContext(ctx, `UPDATE tunnels SET status=?,last_seen_at=CASE WHEN ?='online' THEN ? ELSE last_seen_at END WHERE id=? AND status NOT IN ('disabled','retired','failed')`, nextTunnelStatus, nextTunnelStatus, now, t.ID); err != nil {
			return nil, err
		}
		if _, err := p.store.db.ExecContext(ctx, `INSERT INTO hub_observations (tunnel_id,machine_id,probe_state,last_probe_at,last_success_at,probe_error) VALUES (?,?,?,?,?,?) ON CONFLICT(tunnel_id) DO UPDATE SET probe_state=excluded.probe_state,last_probe_at=excluded.last_probe_at,last_success_at=COALESCE(excluded.last_success_at,hub_observations.last_success_at),probe_error=excluded.probe_error`, t.ID, t.MachineID, state, now, lastSuccess, nullable(probeErr)); err != nil {
			return nil, err
		}
		machineChanged, observed, status, err := p.recomputeObserved(ctx, t.MachineID)
		if err != nil {
			return nil, err
		}
		healthChanged := t.Status != nextTunnelStatus || !previousProbe.Valid || previousProbe.String != state || previousProbeError.String != probeErr
		if machineChanged || healthChanged {
			p.publish("machine.health_changed", t.MachineID, map[string]any{"machine_id": t.MachineID, "observed_state": observed, "status": status, "probe_state": state})
		}
		report = append(report, map[string]any{"tunnel_id": t.ID, "machine_id": t.MachineID, "port": t.RemotePort, "ok": ok, "probe_state": state, "detail": detail, "checked_at": now})
	}
	p.healthMu.Lock()
	p.healthReport = report
	p.healthMu.Unlock()
	return report, nil
}

func (p *Provisioner) recomputeObserved(ctx context.Context, machineID string) (bool, string, string, error) {
	p.observedMu.Lock()
	defer p.observedMu.Unlock()

	var desired, old, oldStatus string
	var hbAt, agentTunnel, localSSH, probe sql.NullString
	if err := p.store.db.QueryRowContext(ctx, `SELECT desired_state,observed_state,status FROM machines WHERE id=?`, machineID).Scan(&desired, &old, &oldStatus); err != nil {
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
	effective := observed
	if desired == DesiredDisabled || desired == DesiredRetiring || desired == DesiredRetired {
		effective = desired
	}
	changed := observed != old || effective != oldStatus
	if changed {
		if _, err := p.store.db.ExecContext(ctx, `UPDATE machines SET observed_state=?,status=?,updated_at=? WHERE id=?`, observed, effective, nowUTC(), machineID); err != nil {
			return false, "", "", err
		}
	}
	return changed, observed, effective, nil
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
