package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func txnFixture(t *testing.T) (*Server, *Store, string, string, string) {
	t.Helper()
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_t", "t-box", "rt-tbox1111", 9257)
	if _, err := store.db.Exec(`UPDATE machines SET created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fence := filepath.Join(dir, "fence.json")
	snap := filepath.Join(dir, "snap.db")
	if err := recordFenceAndSnapshot(context.Background(), store.path, fence, snap); err != nil {
		t.Fatal(err)
	}
	return server, store, fence, snap, dir
}

// Astra R4-01: deploy-restore refuses while any writer (reachd, a CLI)
// holds the writers lock, and touches nothing.
func TestRestoreRefusedWhileAWriterRuns(t *testing.T) {
	_, store, fence, snap, dir := txnFixture(t)
	release, err := acquireWritersShared(store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = restoreUnderLock(context.Background(), store.path, fence, snap, filepath.Join(dir, "failed"), time.Now())
	if !errors.Is(err, errRestoreRefused) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed")); err == nil {
		t.Fatal("forensic copy taken although the restore was refused")
	}
}

// A durable action after the fence blocks the restore; the live DB keeps it.
func TestRestoreRefusedAfterDurableAction(t *testing.T) {
	server, store, fence, snap, dir := txnFixture(t)
	if _, err := server.prov.SetDesiredState(context.Background(), "m_t", DesiredDisabled, "t"); err != nil {
		t.Fatal(err)
	}
	path := store.path
	_ = store.Close()
	err := restoreUnderLock(context.Background(), path, fence, snap, filepath.Join(dir, "failed"), time.Now())
	if !errors.Is(err, errRestoreRefused) || !strings.Contains(err.Error(), "durable") {
		t.Fatalf("err = %v", err)
	}
	s2, _ := OpenStore(path)
	defer s2.Close()
	var d string
	_ = s2.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_t'`).Scan(&d)
	if d != DesiredDisabled {
		t.Fatalf("live DB lost the disable: %s", d)
	}
}

// Nothing durable: the snapshot is restored and the failed set preserved.
func TestRestoreWithoutDurableActions(t *testing.T) {
	_, store, fence, snap, dir := txnFixture(t)
	if _, err := store.db.Exec(`UPDATE machines SET agent_token_hash='sha256:migrated' WHERE id='m_t'`); err != nil {
		t.Fatal(err)
	}
	path := store.path
	_ = store.Close()
	if err := restoreUnderLock(context.Background(), path, fence, snap, filepath.Join(dir, "failed"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, "failed", filepath.Base(path))); err != nil || st.Size() == 0 {
		t.Fatalf("forensic set: %v", err)
	}
	if same, _ := sameContent(snap, path); !same {
		t.Fatal("live DB is not the snapshot")
	}
}

// Daybreak round 5 N9: a fence older than the journal retention can no
// longer prove anything; it is refused.
func TestRestoreRefusesOldFence(t *testing.T) {
	_, store, fence, snap, dir := txnFixture(t)
	path := store.path
	_ = store.Close()
	err := restoreUnderLock(context.Background(), path, fence, snap, filepath.Join(dir, "failed"), time.Now().Add(fenceMaxAge+time.Hour))
	if !errors.Is(err, errRestoreRefused) {
		t.Fatalf("old fence accepted: %v", err)
	}
	b, _ := os.ReadFile(fence)
	var f deployFence
	_ = json.Unmarshal(b, &f)
	if fenceMaxAge >= journalRetention {
		t.Fatal("fenceMaxAge must stay below journalRetention")
	}
}

// Daybreak round 5 N6: the rotation decision is taken under the same lock
// as fence and snapshot; deploy-prepare refuses while a writer runs, so a
// CLI cannot run the session migration between snapshot and rotation.
func TestDeployPrepareRotatesUnderTheLock(t *testing.T) {
	_, store, cfg := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "db_path: " + store.path + "\njwt_secret: \"" + cfg.JWTSecret + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	release, _ := acquireWritersShared(store.path)
	err := cliDeployPrepare(context.Background(), []string{"--config", cfgPath, "--fence", filepath.Join(dir, "f.json"), "--snapshot", filepath.Join(dir, "s.db")})
	release()
	if err == nil {
		t.Fatal("deploy-prepare ran while a writer held the lock")
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != body {
		t.Fatal("config changed although prepare was refused")
	}
	if err := cliDeployPrepare(context.Background(), []string{"--config", cfgPath, "--fence", filepath.Join(dir, "f.json"), "--snapshot", filepath.Join(dir, "s.db")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); strings.Contains(string(b), cfg.JWTSecret) {
		t.Fatal("jwt_secret not rotated while the session migration is pending")
	}
}

// Astra R4-03 / daybreak N7: a symlinked DB path is refused, and a
// symlinked lock file is refused instead of silently giving another lock.
func TestDatabaseAndLockAliasesRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	alias := filepath.Join(other, "alias.db")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(alias); err == nil {
		t.Fatal("symlinked database path accepted")
	}
	lockdir := lockDir(real)
	if err := os.MkdirAll(lockdir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.lock")
	_ = os.WriteFile(target, nil, 0o600)
	if err := os.Symlink(target, filepath.Join(lockdir, "writers.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWritersShared(real); err == nil {
		t.Fatal("symlinked writers.lock accepted")
	}
}

// Daybreak N7: rotation only rewrites a private regular file.
func TestRotateRefusesSymlinkAndLooseConfig(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.yaml")
	_ = os.WriteFile(real, []byte("jwt_secret: \"x\"\n"), 0o600)
	link := filepath.Join(dir, "config.yaml")
	_ = os.Symlink(real, link)
	if err := rotateJWTSecret(link); err == nil {
		t.Fatal("symlinked config rotated")
	}
	loose := filepath.Join(dir, "loose.yaml")
	_ = os.WriteFile(loose, []byte("jwt_secret: \"x\"\n"), 0o666)
	_ = os.Chmod(loose, 0o666)
	if err := rotateJWTSecret(loose); err == nil {
		t.Fatal("world-writable config rotated")
	}
	hard := filepath.Join(dir, "hard.yaml")
	_ = os.Link(real, hard)
	if err := rotateJWTSecret(real); err == nil {
		t.Fatal("hard-linked config rotated")
	}
}

// Astra R5-01: the previous binary is put back inside the locked restore;
// fresh installs move the new binary away.
func TestRestoreHandsBackTheBinary(t *testing.T) {
	_, store, fence, snap, dir := txnFixture(t)
	path := store.path
	_ = store.Close()
	target := filepath.Join(dir, "reachd")
	rollback := filepath.Join(dir, "reachd.rollback")
	_ = os.WriteFile(target, []byte("new"), 0o755)
	_ = os.WriteFile(rollback, []byte("old"), 0o755)
	rb, remove := rollback, false
	h := binaryHandoff{target: &target, rollback: &rb, remove: &remove}
	if err := restoreUnderLockWithHandoff(context.Background(), path, fence, snap, filepath.Join(dir, "failed"), time.Now(), h); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatalf("binary after restore: %q", b)
	}
	empty, rm := "", true
	h2 := binaryHandoff{target: &target, rollback: &empty, remove: &rm}
	if err := h2.apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("fresh-install binary not moved away")
	}
}
