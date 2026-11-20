package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Daybreak round 4 P1-03: rotating jwt_secret keeps pre-upgrade JWTs dead
// even when the DB (and its session_version bump) is rolled back.
func TestRotateJWTSecretKillsOldTokensAcrossRollback(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	old, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodGet, "/api/admin/machines", "", old); rec.Code != 200 {
		t.Fatalf("baseline %d", rec.Code)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen_addr: 127.0.0.1:9300\njwt_secret: \""+cfg.JWTSecret+"\"\ndb_path: /x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := rotateJWTSecret(path); err != nil {
		t.Fatal(err)
	}
	newCfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if newCfg.JWTSecret == cfg.JWTSecret || len(newCfg.JWTSecret) < 32 || newCfg.DBPath != "/x" {
		t.Fatalf("rotated config: %+v", newCfg.JWTSecret)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", st.Mode())
	}
	// Same DB (as after a snapshot restore), new secret: the old JWT fails.
	server2 := NewServer(newCfg, store, server.prov)
	if rec := doJSON(t, server2, http.MethodGet, "/api/admin/machines", "", old); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-rotation JWT with the new secret = %d", rec.Code)
	}
}

func TestRotateJWTSecretRefusesAmbiguousConfig(t *testing.T) {
	for _, body := range []string{"listen_addr: x\n", "jwt_secret: a\njwt_secret: b\n"} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		_ = os.WriteFile(path, []byte(body), 0o600)
		if err := rotateJWTSecret(path); err == nil {
			t.Fatalf("rotated %q", body)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Fatal("file changed on refusal")
		}
	}
}

// Only while the session migration is pending.
func TestRotateOnlyIfSessionMigrationPending(t *testing.T) {
	_, store, cfg := newReliabilityTestServer(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "db_path: " + store.path + "\njwt_secret: \"" + cfg.JWTSecret + "\"\n"
	_ = os.WriteFile(path, []byte(body), 0o600)
	if err := cliRotateJWTSecret(context.Background(), []string{"--config", path, "--if-session-migration-pending"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Fatal("rotated although the migration already ran")
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := cliRotateJWTSecret(context.Background(), []string{"--config", path, "--if-session-migration-pending"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), cfg.JWTSecret) {
		t.Fatal("not rotated although the migration is pending")
	}
}

// Retired tokens are cleared before the snapshot is taken.
func TestFenceRecordClearsRetiredTokensBeforeSnapshot(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_r", "r-box", "rt-rbox1111", 9252)
	if _, err := store.db.Exec(`UPDATE machines SET desired_state='retired', status='retired' WHERE id='m_r'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	snap := filepath.Join(dir, "s.db")
	if err := recordFenceAndSnapshot(context.Background(), store.path, filepath.Join(dir, "f.json"), snap); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n int
	_ = s2.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE agent_token_hash IS NOT NULL AND (desired_state='retired' OR status='retired')`).Scan(&n)
	if n != 0 {
		t.Fatal("snapshot still holds a retired machine's token")
	}
}
