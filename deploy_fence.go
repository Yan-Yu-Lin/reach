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
// starts; `deploy-fence check` lists durable actions made after them. The
// deploy script restores the snapshot only when check reports none.
//
// The decision rests on the durable_actions journal (durable.go): every
// durable action writes its row in the same transaction as the mutation,
// or as an intent before an OS side effect, so a successful action always
// leaves a row (daybreak round 3 P1-A, astra R2-02). The older audit and
// timestamp probes are kept as an extra, never as the only, signal: they
// can add blockers, they cannot clear one.

type deployFence struct {
	RecordedAt string `json:"recorded_at"`
	AuditMaxID int64  `json:"audit_max_id"`
	// JournalMaxID is MAX(durable_actions.id) at record time; 0 when the
	// table did not exist yet (the fence is recorded on the pre-deploy
	// schema, before the new binary migrates it).
	JournalMaxID int64 `json:"journal_max_id"`
	// JournalPresent is false when the fence was recorded without the
	// journal; check then fails closed if the table is missing too.
	JournalPresent bool `json:"journal_present"`
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	return n > 0, err
}

// discardableAudit are audit actions written by automatic processes whose
// effect is re-created or harmless to lose.
var discardableAudit = map[string]bool{
	"machine.retiring_timed_out": true,
}

func recordDeployFence(ctx context.Context, db *sql.DB, now time.Time) (deployFence, error) {
	f := deployFence{RecordedAt: now.UTC().Format(time.RFC3339), JournalPresent: true}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit_log`).Scan(&f.AuditMaxID); err != nil {
		return f, err
	}
	ok, err := tableExists(ctx, db, "durable_actions")
	if err != nil {
		return f, err
	}
	if ok {
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM durable_actions`).Scan(&f.JournalMaxID); err != nil {
			return f, err
		}
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
	if !f.JournalPresent {
		return nil, errors.New("fence predates the durable action journal; cannot prove the snapshot is safe")
	}
	ok, err := tableExists(ctx, db, "durable_actions")
	if err != nil {
		return nil, err
	}
	if !ok {
		// The new binary never migrated the DB, so it cannot have taken a
		// durable action through reachd.
		if f.JournalMaxID != 0 {
			return nil, errors.New("durable_actions table disappeared after the fence")
		}
	} else {
		jr, err := db.QueryContext(ctx, `SELECT id,kind,COALESCE(actor,''),COALESCE(machine_id,''),state,created_at FROM durable_actions WHERE id>? ORDER BY id`, f.JournalMaxID)
		if err != nil {
			return nil, err
		}
		for jr.Next() {
			var id int64
			var kind, actor, machine, state, at string
			if err := jr.Scan(&id, &kind, &actor, &machine, &state, &at); err != nil {
				_ = jr.Close()
				return nil, err
			}
			out = append(out, fmt.Sprintf("journal #%d %s (%s) by %s machine=%s at %s", id, kind, state, actor, machine, at))
		}
		if err := jr.Close(); err != nil {
			return nil, err
		}
	}
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
	snapshotPath := fs.String("snapshot", "", "record only: also write a consistent DB snapshot here, after the fence, under the writers lock")
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
	if mode == "record" {
		return recordFenceAndSnapshot(ctx, cfg.DBPath, *fencePath, *snapshotPath)
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
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

// recordFenceAndSnapshot records the fence and then the snapshot, both
// while holding the writers lock exclusively (daybreak round 4 P1-02,
// astra R3-03). Order matters: a write after the fence but before the
// snapshot is in the snapshot AND above the fence, so a rollback refuses
// to restore (safe); the opposite order would let the write vanish.
// Writers of this version (reachd serve, CLI create/disable/remove) hold
// the lock shared, so none can run while it is held.
func recordFenceAndSnapshot(ctx context.Context, dbPath, fencePath, snapshotPath string) error {
	lock, err := acquireWritersExclusive(dbPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	// Clear retired machines' tokens before the snapshot, so a rollback
	// cannot bring them back (daybreak round 4 P1-03).
	if err := clearRetiredTokens(ctx, store.db); err != nil {
		return fmt.Errorf("clear retired tokens: %w", err)
	}
	f, err := recordDeployFence(ctx, store.db, time.Now())
	if err != nil {
		return err
	}
	b, _ := json.Marshal(f)
	if err := writeSyncedFile(fencePath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if snapshotPath == "" {
		return nil
	}
	if _, err := os.Stat(snapshotPath); err == nil {
		return fmt.Errorf("snapshot %s already exists", snapshotPath)
	}
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, snapshotPath); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(snapshotPath, 0o600); err != nil {
		return err
	}
	// Plain open: no WAL pragma, so the snapshot file stays self-contained.
	snap, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		return err
	}
	defer snap.Close()
	var res string
	if err := snap.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil || res != "ok" {
		return fmt.Errorf("snapshot quick_check: %q %v", res, err)
	}
	return nil
}

func writeSyncedFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
