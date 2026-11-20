package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Rollback-safe security migrations (daybreak round 4 P1-03).
//
// Two migrations revoke credentials by changing DB rows: the one-time
// session_version bump (pre-upgrade JWTs) and clearing tokens of retired
// machines. A deploy rollback restores the pre-deploy DB snapshot and would
// bring both back. Neither is a journalled operator action, so instead:
//
//   - JWTs: the deploy rotates jwt_secret in config.yaml before the new
//     binary first starts, while the session migration is still pending.
//     The rollback never restores config.yaml, so the old binary also
//     verifies with the new secret and every pre-upgrade JWT stays dead.
//   - Retired tokens: cleared in the live DB before the fence and the
//     snapshot (an UPDATE the old schema understands), so the snapshot
//     itself no longer contains them.

const sessionMigrationName = "2026-11-21-invalidate-pre-session-version-jwts"

var jwtSecretLineRE = regexp.MustCompile(`(?m)^jwt_secret:[^\n]*$`)

// rotateJWTSecret replaces the jwt_secret line of a YAML config in place,
// atomically, keeping mode and owner. The file must contain exactly one
// top-level jwt_secret line.
func rotateJWTSecret(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if n := len(jwtSecretLineRE.FindAllIndex(b, -1)); n != 1 {
		return fmt.Errorf("%s: expected exactly one top-level jwt_secret line, found %d", path, n)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	out := jwtSecretLineRE.ReplaceAll(b, []byte(`jwt_secret: "`+hex.EncodeToString(raw)+`"`))
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".rotate-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if err := copyOwner(f, st); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func sessionMigrationApplied(ctx context.Context, db *sql.DB) (bool, error) {
	ok, err := tableExists(ctx, db, "schema_migrations")
	if err != nil || !ok {
		return false, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name=?`, sessionMigrationName).Scan(&n)
	return n > 0, err
}

// clearRetiredTokens is the retired-token migration in a form every schema
// since desired_state was added understands.
func clearRetiredTokens(ctx context.Context, db *sql.DB) error {
	if !columnExists(ctx, db, "machines", "agent_token_hash") {
		return nil
	}
	where := `status='retired'`
	if columnExists(ctx, db, "machines", "desired_state") {
		where = `(desired_state='retired' OR status='retired')`
	}
	_, err := db.ExecContext(ctx, `UPDATE machines SET agent_token_hash=NULL WHERE `+where+` AND agent_token_hash IS NOT NULL`)
	return err
}

// cliRotateJWTSecret: reachd rotate-jwt-secret --config C [--if-session-migration-pending]
func cliRotateJWTSecret(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("rotate-jwt-secret", args)
	onlyPending := fs.Bool("if-session-migration-pending", false, "rotate only if the pre-upgrade JWT invalidation has not run on this DB yet")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *onlyPending {
		cfg, err := LoadConfig(*cfgPath)
		if err != nil {
			return err
		}
		store, err := OpenStore(cfg.DBPath)
		if err != nil {
			return err
		}
		applied, err := sessionMigrationApplied(ctx, store.db)
		_ = store.Close()
		if err != nil {
			return err
		}
		if applied {
			fmt.Println("session migration already applied; jwt_secret unchanged")
			return nil
		}
	}
	if err := rotateJWTSecret(*cfgPath); err != nil {
		return err
	}
	fmt.Println("jwt_secret rotated; every admin must log in again")
	return nil
}
