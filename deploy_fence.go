package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Deploy fence (astra P1-01).
//
// A failed deploy may roll reachd back to the previous binary. The new
// binary may already have rewritten the database (schema migration, legacy
// Argon2 agent tokens migrated to sha256), which the old binary cannot read,
// so the binary rollback must restore the pre-deploy snapshot too. Restoring
// the snapshot loses every write made after it. Writes split into:
//
//   - discardable: re-derived or repeated automatically after rollback:
//     heartbeat observations, lazy token migration, last_seen/probe times,
//     events, rate limits, maintenance marks (expired commands/requests,
//     retiring_timed_out), automatic update queueing, pending registrations.
//   - durable: operator or enrollment decisions that change who can reach
//     what: provision, approve/deny, disable/enable/remove/retire (including
//     their failure records), invite claim/consume, service token create or
//     revoke, logout and revoke-all, process title / update policy changes.
//
// `deploy-fence record` stores high-water marks before the new binary
// starts; `deploy-fence check` lists durable writes made after them. The
// deploy script restores the snapshot only when check reports none.
// Every durable action above either writes audit_log or one of the
// timestamped columns checked below; the classification is unit-tested
// against the real handlers.

type deployFence struct {
	RecordedAt string `json:"recorded_at"`
	AuditMaxID int64  `json:"audit_max_id"`
}

// discardableAudit are audit actions written by automatic processes whose
// effect is re-created or harmless to lose.
var discardableAudit = map[string]bool{
	"machine.retiring_timed_out": true,
}

func recordDeployFence(ctx context.Context, db *sql.DB, now time.Time) (deployFence, error) {
	f := deployFence{RecordedAt: now.UTC().Format(time.RFC3339)}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit_log`).Scan(&f.AuditMaxID); err != nil {
		return f, err
	}
	return f, nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) bool {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk) == nil && name == column {
			return true
		}
	}
	return false
}

// durableWritesSince returns a human-readable list of durable writes made
// after the fence. An empty list means restoring the snapshot loses nothing
// that matters.
func durableWritesSince(ctx context.Context, db *sql.DB, f deployFence) ([]string, error) {
	var out []string
	rows, err := db.QueryContext(ctx, `SELECT id,action,COALESCE(actor,''),COALESCE(machine_id,''),created_at FROM audit_log WHERE id>? ORDER BY id`, f.AuditMaxID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var action, actor, machine, at string
		if err := rows.Scan(&id, &action, &actor, &machine, &at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if discardableAudit[action] {
			continue
		}
		if action == "machine.agent_update.queue" && actor == "system" {
			continue // auto policy; requeued by the next heartbeat
		}
		out = append(out, fmt.Sprintf("audit #%d %s by %s machine=%s at %s", id, action, actor, machine, at))
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	type probe struct {
		table, column, label, where string
	}
	probes := []probe{
		// Approved/provisioned enrollments (god code or invite) need no
		// audit row; pending ones are discardable (the installer retries).
		{"requests", "approved_at", "request approved", "status IN ('approved','provisioning','provisioned')"},
		{"invites", "claimed_at", "invite claimed", ""},
		{"invites", "consumed_at", "invite consumed", ""},
		{"service_tokens", "created_at", "service token created", ""},
		{"service_tokens", "revoked_at", "service token revoked", ""},
		{"jwt_blacklist", "created_at", "session logged out", ""},
		{"machines", "created_at", "machine created", ""},
	}
	for _, p := range probes {
		if !columnExists(ctx, db, p.table, p.column) {
			continue
		}
		q := `SELECT COUNT(*) FROM ` + p.table + ` WHERE ` + p.column + `>=?`
		if p.where != "" {
			q += ` AND ` + p.where
		}
		var n int
		if err := db.QueryRowContext(ctx, q, f.RecordedAt).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%d x %s (%s.%s >= %s)", n, p.label, p.table, p.column, f.RecordedAt))
		}
	}
	return out, nil
}

// cliDeployFence: reachd deploy-fence record|check --config C --fence FILE.
// Opens the database without migrating it.
func cliDeployFence(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "record" && args[0] != "check") {
		return errors.New("usage: reachd deploy-fence record|check --config PATH --fence FILE")
	}
	mode := args[0]
	fs, cfgPath := baseFS("deploy-fence", args[1:])
	fencePath := fs.String("fence", "", "fence file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *fencePath == "" {
		return errors.New("--fence is required")
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if mode == "record" {
		f, err := recordDeployFence(ctx, store.db, time.Now())
		if err != nil {
			return err
		}
		b, _ := json.Marshal(f)
		return os.WriteFile(*fencePath, append(b, '\n'), 0o600)
	}
	b, err := os.ReadFile(*fencePath)
	if err != nil {
		return err
	}
	var f deployFence
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	writes, err := durableWritesSince(ctx, store.db, f)
	if err != nil {
		return err
	}
	if len(writes) == 0 {
		fmt.Println("no durable writes since the deploy fence; snapshot restore loses only discardable data")
		return nil
	}
	fmt.Println("durable writes since the deploy fence (restoring the snapshot would lose them):")
	fmt.Println("  " + strings.Join(writes, "\n  "))
	return errDurableWrites
}

var errDurableWrites = errors.New("durable writes found")

// cliAdminProbeToken prints a 3-minute admin JWT for the first user, for
// deploy readiness checks (an authenticated GET /api/admin/health proves the
// database and auth path work, not just that the port listens). Works on
// databases without users.session_version (pre-hardening schema).
func cliAdminProbeToken(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("admin-probe-token", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.JWTSecret == "" {
		return errors.New("jwt_secret is not configured")
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var id, username, role string
	if err := store.db.QueryRowContext(ctx, `SELECT id,username,role FROM users ORDER BY created_at LIMIT 1`).Scan(&id, &username, &role); err != nil {
		return err
	}
	var sv int64
	if columnExists(ctx, store.db, "users", "session_version") {
		_ = store.db.QueryRowContext(ctx, `SELECT session_version FROM users WHERE id=?`, id).Scan(&sv)
	}
	tok, err := SignJWTVersion(cfg.JWTSecret, id, username, role, sv, 3*time.Minute)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
