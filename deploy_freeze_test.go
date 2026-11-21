package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepareArgs(dir, cfgPath string) []string {
	return []string{"--config", cfgPath, "--fence", filepath.Join(dir, "f.json"), "--snapshot", filepath.Join(dir, "s.db")}
}

func writeTestConfig(t *testing.T, dir string, store *Store, cfg Config) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("db_path: "+store.path+"\njwt_secret: \""+cfg.JWTSecret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Daybreak round 6 R6-P1-02 PoC: the early preflight passed, then a
// public client registered before reachd was stopped. deploy-prepare must
// refuse (no fence, no snapshot, secret unchanged), not snapshot it.
func TestDeployPrepareRefusesRegistrationAfterPreflight(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, store, cfg)
	if err := checkProvisioningFrozen(context.Background(), store.db); err != nil {
		t.Fatalf("clean DB not frozen: %v", err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"mode":"system","local_port":22,"name":"late-box"}`, ""); rec.Code != 200 {
		t.Fatalf("register %d", rec.Code)
	}
	before, _ := os.ReadFile(cfgPath)
	err := cliDeployPrepare(context.Background(), prepareArgs(dir, cfgPath))
	if !errors.Is(err, errNotFrozen) || !strings.Contains(err.Error(), "status=pending") {
		t.Fatalf("deploy-prepare with a pending request: %v", err)
	}
	for _, f := range []string{"f.json", "s.db"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s written although the freeze check failed", f)
		}
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Fatal("jwt_secret rotated although the freeze check failed")
	}
}

// Every non-terminal state refuses, including phases this binary does not
// know (fail closed), and machines still being provisioned.
func TestFreezeCheckFailsClosed(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	ctx := context.Background()
	ins := func(id, status string, phase any) {
		t.Helper()
		if _, err := store.db.Exec(`INSERT INTO requests (id,name,client_secret_hash,status,created_at,expires_at,provision_phase) VALUES (?,?,?,?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',?)`, id, id, "x", status, phase); err != nil {
			t.Fatal(err)
		}
	}
	// Terminal states pass, including legacy rows without a phase.
	ins("r_ok1", "provisioned", phaseDelivered)
	ins("r_ok2", "expired", phaseCleaned)
	ins("r_ok3", "denied", nil)
	ins("r_ok4", "provisioned", nil)
	ins("r_ok5", "expired", nil)
	if err := checkProvisioningFrozen(ctx, store.db); err != nil {
		t.Fatalf("terminal states refused: %v", err)
	}
	cases := []struct {
		status string
		phase  any
	}{
		{"pending", nil}, {"approved", nil}, {"provisioning", phaseClaimed},
		{"provisioning", phaseOSCreated}, {"provisioned", phaseResponsePending},
		{"expired", phaseCleanupPending}, {"provisioned", "future_phase"},
		{"some_new_status", nil},
	}
	for i, c := range cases {
		id := "r_bad" + string(rune('a'+i))
		ins(id, c.status, c.phase)
		if err := checkProvisioningFrozen(ctx, store.db); !errors.Is(err, errNotFrozen) {
			t.Fatalf("%s/%v passed the freeze check: %v", c.status, c.phase, err)
		}
		if _, err := store.db.Exec(`DELETE FROM requests WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	insertRetireFixture(t, server, "m_p", "p-box", "rt-pbox1111", 9261)
	if _, err := store.db.Exec(`UPDATE machines SET status='provisioning' WHERE id='m_p'`); err != nil {
		t.Fatal(err)
	}
	if err := checkProvisioningFrozen(ctx, store.db); !errors.Is(err, errNotFrozen) {
		t.Fatalf("machine in provisioning passed: %v", err)
	}
}

// The pre-provision_phase schema (production 8c74086) is checked too.
func TestFreezeCheckOnLegacySchema(t *testing.T) {
	db, err := openPlainSQLite(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := checkProvisioningFrozen(ctx, db); err != nil {
		t.Fatalf("empty DB: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE requests (id TEXT PRIMARY KEY, status TEXT DEFAULT 'pending'); INSERT INTO requests VALUES ('a','provisioned'),('b','expired')`); err != nil {
		t.Fatal(err)
	}
	if err := checkProvisioningFrozen(ctx, db); err != nil {
		t.Fatalf("legacy terminal rows: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO requests VALUES ('c','approved')`); err != nil {
		t.Fatal(err)
	}
	if err := checkProvisioningFrozen(ctx, db); !errors.Is(err, errNotFrozen) {
		t.Fatalf("legacy approved request passed: %v", err)
	}
}
