package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_CREATE, 0o600); err != nil {
		return nil, err
	} else {
		_ = f.Close()
		_ = os.Chmod(path, 0o600)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CheckReady(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire database connection: %w", err)
	}
	defer conn.Close()

	var quickCheck string
	if err := conn.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quickCheck); err != nil {
		return fmt.Errorf("sqlite quick_check: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("sqlite quick_check: %s", quickCheck)
	}
	var foreignKeys, busyTimeout int
	var journalMode string
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read journal_mode pragma: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("sqlite foreign_keys pragma is disabled")
	}
	if busyTimeout < 5000 {
		return fmt.Errorf("sqlite busy_timeout is %dms, want at least 5000ms", busyTimeout)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("sqlite journal_mode is %q, want WAL", journalMode)
	}
	var tables int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('machines','events','jwt_blacklist')`).Scan(&tables); err != nil {
		return fmt.Errorf("check database schema: %w", err)
	}
	if tables != 3 {
		return errors.New("database schema is incomplete; run reachd serve to migrate it")
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context, cfg Config) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS machines (
			id TEXT PRIMARY KEY,
			slug TEXT UNIQUE NOT NULL,
			original_slug TEXT,
			display_name TEXT,
			target_user TEXT,
			owner_user_id TEXT,
			status TEXT DEFAULT 'pending',
			desired_state TEXT DEFAULT 'active',
			desired_generation INTEGER DEFAULT 1,
			desired_changed_at TEXT,
			desired_changed_by TEXT,
			observed_state TEXT DEFAULT 'unknown',
			update_policy TEXT DEFAULT 'manual',
			cleanup_state TEXT,
			mode TEXT,
			local_port INTEGER DEFAULT 22,
			persistence TEXT,
			distro TEXT,
			arch TEXT,
			provision_error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT,
			expires_at TEXT,
			disabled_at TEXT,
			retired_at TEXT,
			process_title_config_json TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS hubs (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			public_host TEXT NOT NULL,
			ssh_port INTEGER NOT NULL,
			proxyjump_alias TEXT,
			api_url TEXT,
			port_range_start INTEGER NOT NULL,
			port_range_end INTEGER NOT NULL,
			status TEXT DEFAULT 'active',
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS tunnels (
			id TEXT PRIMARY KEY,
			machine_id TEXT NOT NULL REFERENCES machines(id),
			hub_id TEXT NOT NULL REFERENCES hubs(id),
			unix_user TEXT NOT NULL,
			original_unix_user TEXT,
			remote_port INTEGER NOT NULL,
			original_remote_port INTEGER,
			tunnel_pubkey TEXT NOT NULL,
			status TEXT DEFAULT 'pending',
			last_seen_at TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			UNIQUE(hub_id, remote_port),
			UNIQUE(hub_id, unix_user)
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role TEXT DEFAULT 'admin',
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS access_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id),
			label TEXT,
			public_key TEXT NOT NULL,
			kind TEXT,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS machine_grants (
			machine_id TEXT NOT NULL REFERENCES machines(id),
			user_id TEXT NOT NULL REFERENCES users(id),
			role TEXT NOT NULL DEFAULT 'ssh',
			expires_at TEXT,
			PRIMARY KEY(machine_id, user_id)
		);`,
		`CREATE TABLE IF NOT EXISTS requests (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			client_secret_hash TEXT NOT NULL,
			status TEXT DEFAULT 'pending',
			invite_code_hash TEXT,
			setup_token_hash TEXT,
			setup_token_expires_at TEXT,
			metadata_json TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			approved_at TEXT,
			denied_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS invites (
			code_hash TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			used_by TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			action TEXT NOT NULL,
			actor TEXT,
			machine_id TEXT,
			details TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS agent_status (
			machine_id TEXT PRIMARY KEY REFERENCES machines(id),
			agent_version TEXT,
			transport TEXT,
			transport_state TEXT,
			local_ssh_state TEXT,
			local_ssh_error TEXT,
			tunnel_state TEXT,
			tunnel_pid INTEGER,
			tunnel_error TEXT,
			last_error TEXT,
			raw_json TEXT,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS agent_observations (
			machine_id TEXT PRIMARY KEY REFERENCES machines(id),
			agent_version TEXT,
			install_id TEXT,
			applied_generation INTEGER DEFAULT 0,
			heartbeat_at TEXT NOT NULL,
			agent_state TEXT,
			transport TEXT,
			transport_state TEXT,
			local_ssh_state TEXT,
			local_ssh_error TEXT,
			tunnel_state TEXT,
			tunnel_pid INTEGER,
			tunnel_error TEXT,
			persistence_backend TEXT,
			persistence_quality TEXT,
			persistence_reboot_safe INTEGER DEFAULT 0,
			authorized_key_fingerprints TEXT,
			last_error TEXT,
			raw_json TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS hub_observations (
			tunnel_id TEXT PRIMARY KEY REFERENCES tunnels(id),
			machine_id TEXT NOT NULL REFERENCES machines(id),
			probe_state TEXT NOT NULL,
			last_probe_at TEXT NOT NULL,
			last_success_at TEXT,
			probe_error TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS agent_commands (
			id TEXT PRIMARY KEY,
			machine_id TEXT NOT NULL REFERENCES machines(id),
			generation INTEGER NOT NULL,
			type TEXT NOT NULL,
			payload_json TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			sent_at TEXT,
			acked_at TEXT,
			expires_at TEXT,
			last_error TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			type TEXT NOT NULL,
			machine_id TEXT,
			payload_json TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS rate_limits (
			bucket TEXT NOT NULL,
			key TEXT NOT NULL,
			count INTEGER NOT NULL,
			reset_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(bucket,key)
		);`,
		`CREATE TABLE IF NOT EXISTS jwt_blacklist (
			token_hash TEXT PRIMARY KEY,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS service_tokens (
			id TEXT PRIMARY KEY,
			label TEXT NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			role TEXT NOT NULL,
			created_by TEXT,
			created_at TEXT NOT NULL,
			last_used_at TEXT,
			revoked_at TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_tunnels_machine ON tunnels(machine_id);`,
		`CREATE INDEX IF NOT EXISTS idx_tunnels_status ON tunnels(status);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_status ON requests(status);`,
		`CREATE INDEX IF NOT EXISTS idx_agent_commands_machine_status ON agent_commands(machine_id,status);`,
		`CREATE INDEX IF NOT EXISTS idx_agent_commands_delivery ON agent_commands(machine_id,status,sent_at,created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_hub_observations_machine_probe ON hub_observations(machine_id,last_probe_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_jwt_blacklist_expires ON jwt_blacklist(expires_at);`,
		`CREATE INDEX IF NOT EXISTS idx_rate_limits_reset ON rate_limits(reset_at);`,
		`CREATE INDEX IF NOT EXISTS idx_service_tokens_hash ON service_tokens(token_hash);`,
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st); err != nil {
			return err
		}
	}
	for _, col := range []struct{ table, name, decl string }{
		{"machines", "agent_token_hash", "TEXT"},
		{"machines", "original_slug", "TEXT"},
		{"machines", "desired_state", "TEXT DEFAULT 'active'"},
		{"machines", "desired_generation", "INTEGER DEFAULT 1"},
		{"machines", "desired_changed_at", "TEXT"},
		{"machines", "desired_changed_by", "TEXT"},
		{"machines", "observed_state", "TEXT DEFAULT 'unknown'"},
		{"machines", "update_policy", "TEXT DEFAULT 'manual'"},
		{"machines", "cleanup_state", "TEXT"},
		{"machines", "process_title_config_json", "TEXT"},
		{"tunnels", "original_unix_user", "TEXT"},
		{"tunnels", "original_remote_port", "INTEGER"},
		{"requests", "metadata_json", "TEXT"},
	} {
		if err := s.ensureColumn(ctx, col.table, col.name, col.decl); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machines SET original_slug=slug WHERE original_slug IS NULL OR original_slug=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machines SET desired_state=CASE WHEN status='disabled' THEN 'disabled' WHEN status='retired' THEN 'retired' ELSE COALESCE(NULLIF(desired_state,''),'active') END WHERE desired_state IS NULL OR desired_state=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machines SET observed_state=CASE WHEN status='active' THEN 'online' WHEN status='offline' THEN 'offline' ELSE COALESCE(NULLIF(observed_state,''),'unknown') END WHERE observed_state IS NULL OR observed_state=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machines SET update_policy='manual' WHERE update_policy IS NULL OR update_policy=''`); err != nil {
		return err
	}
	if err := s.releaseRetiredSlugs(ctx); err != nil {
		return err
	}
	if err := s.releaseRetiredTunnels(ctx); err != nil {
		return err
	}
	return s.seed(ctx, cfg)
}

func (s *Store) releaseRetiredSlugs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,slug,COALESCE(NULLIF(original_slug,''),slug) FROM machines WHERE desired_state='retired' OR status='retired'`)
	if err != nil {
		return err
	}
	type retired struct{ id, slug, original string }
	var retiredRows []retired
	for rows.Next() {
		var r retired
		if err := rows.Scan(&r.id, &r.slug, &r.original); err != nil {
			return err
		}
		retiredRows = append(retiredRows, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range retiredRows {
		if strings.Contains(r.slug, "--retired-") {
			continue
		}
		next, err := s.retiredSlug(ctx, r.original, r.id)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE machines SET slug=?, original_slug=? WHERE id=?`, next, r.original, r.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) retiredSlug(ctx context.Context, original, machineID string) (string, error) {
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
		var existing string
		err = s.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE slug=? AND id<>?`, candidate, machineID).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate retired slug for %s", original)
}

func (s *Store) releaseRetiredTunnels(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE tunnels SET original_unix_user=unix_user WHERE original_unix_user IS NULL OR original_unix_user=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tunnels SET original_remote_port=remote_port WHERE original_remote_port IS NULL OR original_remote_port=0`); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,hub_id,unix_user,remote_port FROM tunnels WHERE status='retired'`)
	if err != nil {
		return err
	}
	type retiredTunnel struct {
		id       string
		hubID    string
		unixUser string
		port     int
	}
	var retired []retiredTunnel
	for rows.Next() {
		var t retiredTunnel
		if err := rows.Scan(&t.id, &t.hubID, &t.unixUser, &t.port); err != nil {
			return err
		}
		retired = append(retired, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range retired {
		newUser := t.unixUser
		if !strings.HasPrefix(newUser, "retired-") {
			newUser = "retired-" + t.id
		}
		newPort := t.port
		if newPort > 0 {
			var min sql.NullInt64
			if err := s.db.QueryRowContext(ctx, `SELECT MIN(remote_port) FROM tunnels WHERE hub_id=? AND remote_port<0`, t.hubID).Scan(&min); err != nil {
				return err
			}
			newPort = -1
			if min.Valid {
				newPort = int(min.Int64) - 1
			}
		}
		if newUser != t.unixUser || newPort != t.port {
			if _, err := s.db.ExecContext(ctx, `UPDATE tunnels SET unix_user=?, remote_port=? WHERE id=?`, newUser, newPort, t.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == column {
			found = true
			break
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

func (s *Store) seed(ctx context.Context, cfg Config) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO hubs (id,name,public_host,ssh_port,proxyjump_alias,api_url,port_range_start,port_range_end,status,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, cfg.DefaultHub.ID, cfg.DefaultHub.Name, cfg.DefaultHub.PublicHost, cfg.DefaultHub.SSHPort, cfg.DefaultHub.ProxyJumpAlias, cfg.DefaultHub.APIURL, cfg.DefaultHub.PortStart, cfg.DefaultHub.PortEnd, "active", now)
	if err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if cfg.InitialAdmin.PasswordHash == "" {
			return errors.New("initial_admin.password_hash is required when creating the first user")
		}
		uid, err := RandomID("u")
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id,username,password_hash,role,created_at) VALUES (?,?,?,?,?)`, uid, cfg.InitialAdmin.Username, cfg.InitialAdmin.PasswordHash, "owner", now); err != nil {
			return err
		}
		for _, k := range cfg.InitialAdmin.PublicKeys {
			kind, err := ValidateSSHPublicKey(k.PublicKey)
			if err != nil {
				return fmt.Errorf("invalid initial admin public key %q: %w", k.Label, err)
			}
			kid, err := RandomID("ak")
			if err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO access_keys (id,user_id,label,public_key,kind,created_at) VALUES (?,?,?,?,?,?)`, kid, uid, k.Label, k.PublicKey, kind, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) Audit(ctx context.Context, action, actor, machineID, details string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_log (action,actor,machine_id,details,created_at) VALUES (?,?,?,?,?)`, action, actor, nullable(machineID), nullable(details), nowUTC())
}

func scanMachine(rows interface{ Scan(dest ...any) error }) (Machine, error) {
	var m Machine
	var originalSlug, displayName, targetUser, ownerUserID, mode, persistence, distro, arch, provisionError, updatedAt, expiresAt, disabledAt, retiredAt, processTitleConfig sql.NullString
	var desiredChangedAt, desiredChangedBy, cleanupState, updatePolicy sql.NullString
	var desiredState, observedState sql.NullString
	var desiredGeneration sql.NullInt64
	err := rows.Scan(&m.ID, &m.Slug, &originalSlug, &displayName, &targetUser, &ownerUserID, &m.Status, &desiredState, &observedState, &desiredGeneration, &updatePolicy, &desiredChangedAt, &desiredChangedBy, &cleanupState, &mode, &m.LocalPort, &persistence, &distro, &arch, &provisionError, &m.CreatedAt, &updatedAt, &expiresAt, &disabledAt, &retiredAt, &processTitleConfig)
	m.OriginalSlug = stringPtrFromNull(originalSlug)
	m.DisplayName = stringPtrFromNull(displayName)
	m.TargetUser = stringPtrFromNull(targetUser)
	m.OwnerUserID = stringPtrFromNull(ownerUserID)
	m.DesiredState = desiredState.String
	if m.DesiredState == "" {
		m.DesiredState = DesiredActive
	}
	m.ObservedState = observedState.String
	if m.ObservedState == "" {
		m.ObservedState = ObservedUnknown
	}
	m.DesiredGeneration = desiredGeneration.Int64
	if m.DesiredGeneration == 0 {
		m.DesiredGeneration = 1
	}
	m.UpdatePolicy = updatePolicy.String
	if m.UpdatePolicy == "" {
		m.UpdatePolicy = "manual"
	}
	m.DesiredChangedAt = stringPtrFromNull(desiredChangedAt)
	m.DesiredChangedBy = stringPtrFromNull(desiredChangedBy)
	m.CleanupState = stringPtrFromNull(cleanupState)
	m.Mode = stringPtrFromNull(mode)
	m.Persistence = stringPtrFromNull(persistence)
	m.Distro = stringPtrFromNull(distro)
	m.Arch = stringPtrFromNull(arch)
	m.ProvisionError = stringPtrFromNull(provisionError)
	m.UpdatedAt = stringPtrFromNull(updatedAt)
	m.ExpiresAt = stringPtrFromNull(expiresAt)
	m.DisabledAt = stringPtrFromNull(disabledAt)
	m.RetiredAt = stringPtrFromNull(retiredAt)
	if processTitleConfig.Valid && processTitleConfig.String != "" {
		var cfg ProcessTitleConfig
		if json.Unmarshal([]byte(processTitleConfig.String), &cfg) == nil {
			m.ProcessTitleConfig = &cfg
		}
	}
	return m, err
}

const machineCols = `id,slug,original_slug,display_name,target_user,owner_user_id,status,desired_state,observed_state,desired_generation,update_policy,desired_changed_at,desired_changed_by,cleanup_state,mode,local_port,persistence,distro,arch,provision_error,created_at,updated_at,expires_at,disabled_at,retired_at,process_title_config_json`

func scanTunnel(rows interface{ Scan(dest ...any) error }) (Tunnel, error) {
	var t Tunnel
	var originalUnix, lastSeenAt, expiresAt sql.NullString
	var originalPort sql.NullInt64
	err := rows.Scan(&t.ID, &t.MachineID, &t.HubID, &t.UnixUser, &originalUnix, &t.RemotePort, &originalPort, &t.TunnelPubkey, &t.Status, &lastSeenAt, &t.CreatedAt, &expiresAt)
	t.OriginalUnixUser = stringPtrFromNull(originalUnix)
	t.OriginalRemotePort = intPtrFromNull(originalPort)
	t.LastSeenAt = stringPtrFromNull(lastSeenAt)
	t.ExpiresAt = stringPtrFromNull(expiresAt)
	return t, err
}

const tunnelCols = `id,machine_id,hub_id,unix_user,original_unix_user,remote_port,original_remote_port,tunnel_pubkey,status,last_seen_at,created_at,expires_at`
