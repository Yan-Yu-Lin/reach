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

// Daybreak round 6 R6-P1-03: a DB that a newer CLI already migrated
// (session marker present, secret unchanged) must still get its secret
// rotated by deploy-prepare. After a rollback (snapshot restored, old
// binary: no session_version check) the pre-deploy JWT must fail.
func TestDeployPrepareRotatesOnPreMigratedDB(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	old, _ := adminTestToken(t, store, cfg)
	var marker int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='2026-11-21-invalidate-pre-session-version-jwts'`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("fixture: session marker %d %v", marker, err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "db_path: " + store.path + "\njwt_secret: \"" + cfg.JWTSecret + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cliDeployPrepare(context.Background(), []string{"--config", cfgPath, "--fence", filepath.Join(dir, "f.json"), "--snapshot", filepath.Join(dir, "s.db")}); err != nil {
		t.Fatal(err)
	}
	newCfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if newCfg.JWTSecret == cfg.JWTSecret {
		t.Fatal("deploy-prepare kept jwt_secret because the DB marker was present")
	}
	// The legacy binary does not check session_version: model it by
	// verifying the old JWT's signature only, with the config it would read.
	if _, err := ParseJWT(newCfg.JWTSecret, old); err == nil {
		t.Fatal("pre-deploy JWT still verifies with the rotated secret")
	}
	if _, err := ParseJWT(cfg.JWTSecret, old); err != nil {
		t.Fatalf("fixture: JWT did not verify with the old secret: %v", err)
	}
	server2 := NewServer(newCfg, store, server.prov)
	if rec := doJSON(t, server2, http.MethodGet, "/api/admin/machines", "", old); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-deploy JWT after deploy-prepare = %d", rec.Code)
	}
}

func TestRotateJWTSecretCommandAlwaysRotates(t *testing.T) {
	_, store, cfg := newReliabilityTestServer(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte("db_path: "+store.path+"\njwt_secret: \""+cfg.JWTSecret+"\"\n"), 0o600)
	if err := cliRotateJWTSecret(context.Background(), []string{"--config", path}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), cfg.JWTSecret) {
		t.Fatal("not rotated")
	}
	if err := cliRotateJWTSecret(context.Background(), []string{"--config", path, "--if-session-migration-pending"}); err == nil {
		t.Fatal("the removed conditional flag is still accepted")
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
