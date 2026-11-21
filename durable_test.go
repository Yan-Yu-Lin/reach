package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// failAudit makes every audit_log insert fail, as in the astra R2-02 and
// daybreak round 3 P1-A PoCs.
func failAudit(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'injected audit failure'); END;`); err != nil {
		t.Fatal(err)
	}
}

func failJournal(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_journal BEFORE INSERT ON durable_actions BEGIN SELECT RAISE(ABORT,'injected journal failure'); END;`); err != nil {
		t.Fatal(err)
	}
}

type durableCase struct {
	path, body string
	// effect reports whether the mutation itself was committed.
	effect func(t *testing.T, store *Store) bool
}

func durableCases() map[string]durableCase {
	desired := func(want string) func(*testing.T, *Store) bool {
		return func(t *testing.T, store *Store) bool {
			var d string
			_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_d'`).Scan(&d)
			return d == want
		}
	}
	return map[string]durableCase{
		"disable": {"/api/admin/machines/m_d/disable", "", desired(DesiredDisabled)},
		"remove":  {"/api/admin/machines/m_d/remove", "", desired(DesiredRetiring)},
		"revoke-all": {"/api/admin/sessions/revoke-all", "", func(t *testing.T, store *Store) bool {
			var sv int
			_ = store.db.QueryRow(`SELECT MAX(session_version) FROM users`).Scan(&sv)
			return sv > 5 // fixture sets 5
		}},
		"update-policy": {"/api/admin/machines/m_d/update-policy", `{"update_policy":"auto"}`, func(t *testing.T, store *Store) bool {
			var p string
			_ = store.db.QueryRow(`SELECT update_policy FROM machines WHERE id='m_d'`).Scan(&p)
			return p == "auto"
		}},
		"process-title": {"/api/admin/machines/m_d/process-title", `{"process_title_config":{"process_title":"x"}}`, func(t *testing.T, store *Store) bool {
			var c string
			_ = store.db.QueryRow(`SELECT COALESCE(process_title_config_json,'') FROM machines WHERE id='m_d'`).Scan(&c)
			return c != ""
		}},
		"service-token": {"/api/admin/service-tokens", `{"label":"x"}`, func(t *testing.T, store *Store) bool {
			var n int
			_ = store.db.QueryRow(`SELECT COUNT(*) FROM service_tokens`).Scan(&n)
			return n > 0
		}},
		"logout": {"/api/admin/logout", "", func(t *testing.T, store *Store) bool {
			var n int
			_ = store.db.QueryRow(`SELECT COUNT(*) FROM jwt_blacklist`).Scan(&n)
			return n > 0
		}},
	}
}

// Astra R2-02 / daybreak P1-A: with audit inserts failing, every durable
// action still succeeds AND still blocks an automatic snapshot restore.
func TestDeployFenceDetectsDurableActionsWhenAuditFails(t *testing.T) {
	for name, c := range durableCases() {
		t.Run(name, func(t *testing.T) {
			server, store, cfg := newReliabilityTestServer(t)
			server.prov.osOps = &fakeTunnelOS{}
			insertRetireFixture(t, server, "m_d", "d-box", "rt-dbox1111", 9250)
			if _, err := store.db.Exec(`UPDATE users SET session_version=5`); err != nil {
				t.Fatal(err)
			}
			admin, _ := adminTestToken(t, store, cfg)
			f := fenceNow(t, server)
			if _, err := store.db.Exec(`UPDATE machines SET created_at='2026-01-01T00:00:00Z'`); err != nil {
				t.Fatal(err)
			}
			failAudit(t, store)
			rec := doJSON(t, server, http.MethodPost, c.path, c.body, admin)
			if rec.Code != 200 {
				t.Fatalf("%s returned %d: %s", name, rec.Code, rec.Body.String())
			}
			if !c.effect(t, store) {
				t.Fatalf("%s: mutation not committed", name)
			}
			w := durable(t, server, f)
			if !strings.Contains(strings.Join(w, "\n"), "journal #") {
				t.Fatalf("%s committed but the fence reports no journal entry: %v", name, w)
			}
		})
	}
}

// If the journal row cannot be written, a DB-only durable action must not
// happen at all (no unrecorded mutation) and the caller gets a 5xx.
func TestDurableActionRollsBackWhenJournalFails(t *testing.T) {
	for name, c := range durableCases() {
		t.Run(name, func(t *testing.T) {
			server, store, cfg := newReliabilityTestServer(t)
			server.prov.osOps = &fakeTunnelOS{}
			insertRetireFixture(t, server, "m_d", "d-box", "rt-dbox1111", 9250)
			if _, err := store.db.Exec(`UPDATE users SET session_version=5`); err != nil {
				t.Fatal(err)
			}
			admin, _ := adminTestToken(t, store, cfg)
			failJournal(t, store)
			rec := doJSON(t, server, http.MethodPost, c.path, c.body, admin)
			if rec.Code < 500 {
				t.Fatalf("%s returned %d with a failing journal", name, rec.Code)
			}
			if c.effect(t, store) {
				t.Fatalf("%s: mutation committed without its journal row", name)
			}
		})
	}
}

// recordingOS checks that the intent row exists before any OS side effect.
type recordingOS struct {
	fakeTunnelOS
	store   *Store
	mu      sync.Mutex
	sawRows []int
	enabled map[string]bool
}

func (r *recordingOS) note() {
	var n int
	_ = r.store.db.QueryRow(`SELECT COUNT(*) FROM durable_actions WHERE state='intent'`).Scan(&n)
	r.mu.Lock()
	r.sawRows = append(r.sawRows, n)
	r.mu.Unlock()
}
func (r *recordingOS) EnableAuth(ctx context.Context, u string) error { r.note(); return nil }
func (r *recordingOS) DisableAuth(ctx context.Context, u string) error {
	r.note()
	return r.fakeTunnelOS.DisableAuth(ctx, u)
}
func (r *recordingOS) Remove(ctx context.Context, u string) error {
	r.note()
	return r.fakeTunnelOS.Remove(ctx, u)
}

func TestOSActionsCommitIntentFirst(t *testing.T) {
	for _, desired := range []string{DesiredActive, DesiredDisabled, DesiredRetired} {
		t.Run(desired, func(t *testing.T) {
			server, store, _ := newReliabilityTestServer(t)
			ros := &recordingOS{store: store}
			server.prov.osOps = ros
			insertRetireFixture(t, server, "m_i", "i-box", "rt-ibox1111", 9251)
			if desired == DesiredActive {
				if _, err := store.db.Exec(`UPDATE machines SET desired_state='disabled',status='disabled'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := server.prov.SetDesiredState(context.Background(), "m_i", desired, "t"); err != nil {
				t.Fatal(err)
			}
			if len(ros.sawRows) == 0 {
				t.Fatal("no OS call observed")
			}
			for _, n := range ros.sawRows {
				if n < 1 {
					t.Fatalf("OS side effect ran before its intent was committed (%v)", ros.sawRows)
				}
			}
			var state string
			_ = store.db.QueryRow(`SELECT state FROM durable_actions ORDER BY id DESC LIMIT 1`).Scan(&state)
			if state != durableDone {
				t.Fatalf("journal state after success = %q", state)
			}
		})
	}
	t.Run("journal failure blocks the OS step", func(t *testing.T) {
		server, store, _ := newReliabilityTestServer(t)
		ros := &recordingOS{store: store}
		server.prov.osOps = ros
		insertRetireFixture(t, server, "m_j", "j-box", "rt-jbox1111", 9252)
		failJournal(t, store)
		if _, err := server.prov.SetDesiredState(context.Background(), "m_j", DesiredDisabled, "t"); err == nil {
			t.Fatal("disable succeeded without a journal intent")
		}
		if len(ros.sawRows) != 0 {
			t.Fatal("OS side effect ran although the intent could not be recorded")
		}
	})
}

// An intent that never settled (crash between OS step and DB record) must
// block the restore like a finished action.
func TestDeployFenceBlocksOnOpenIntent(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	f := fenceNow(t, server)
	if _, err := recordDurable(context.Background(), store.db, "machine.desired.active", "m_x", "t", durableIntent); err != nil {
		t.Fatal(err)
	}
	w := durable(t, server, f)
	if !strings.Contains(strings.Join(w, "\n"), "(intent)") {
		t.Fatalf("open intent not reported: %v", w)
	}
}

// A fence recorded on the pre-deploy schema (no journal yet) still works:
// the new binary creates the table and every row in it is after the fence.
func TestDeployFenceOnPreJournalSchema(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`DROP TABLE durable_actions`); err != nil {
		t.Fatal(err)
	}
	f, err := recordDeployFence(context.Background(), store.db, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if f.JournalMaxID != 0 || !f.JournalPresent {
		t.Fatalf("fence = %+v", f)
	}
	if w := durable(t, server, f); len(w) != 0 {
		t.Fatalf("empty pre-journal DB reported writes: %v", w)
	}
	if _, err := store.db.Exec(`CREATE TABLE durable_actions (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, machine_id TEXT, actor TEXT, state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := recordDurable(context.Background(), store.db, "sessions.revoke_all", "", "t", durableDone); err != nil {
		t.Fatal(err)
	}
	if w := durable(t, server, f); len(w) == 0 {
		t.Fatal("journal row after a pre-journal fence not reported")
	}
	// Old fence files (no journal fields) fail closed.
	if _, err := durableWritesSince(context.Background(), store.db, deployFence{RecordedAt: f.RecordedAt}); err == nil {
		t.Fatal("fence without journal information was accepted")
	}
}

// Daybreak round 4 P2-01: consuming a setup token is journalled in the
// same transaction; if the journal fails, the token stays unconsumed.
func TestSetupTokenConsumptionIsJournalled(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"mode":"system","local_port":22,"name":"journal-box"}`)
	rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"`+secret+`"}`, "")
	var out struct {
		SetupToken string `json:"setup_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SetupToken == "" {
		t.Fatalf("no setup token: %s", rec.Body.String())
	}
	f := fenceNow(t, server)
	failJournal(t, store)
	if _, _, err := server.consumeSetupToken(context.Background(), id, out.SetupToken); err == nil {
		t.Fatal("token consumed although the journal failed")
	}
	var status string
	_ = store.db.QueryRow(`SELECT status FROM requests WHERE id=?`, id).Scan(&status)
	if status != "approved" {
		t.Fatalf("status after failed consume = %s", status)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_journal`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.consumeSetupToken(context.Background(), id, out.SetupToken); err != nil {
		t.Fatal(err)
	}
	if w := durable(t, server, f); !strings.Contains(strings.Join(w, "\n"), "request.provision.start") {
		t.Fatalf("consumption not journalled: %v", w)
	}
}

// Astra R3-02 / daybreak round 4 P2-05: a manual update-agent is a durable
// operator decision; with the journal failing it is not queued at all.
func TestManualUpdateIsJournalled(t *testing.T) {
	for _, fault := range []string{"audit", "journal"} {
		t.Run(fault, func(t *testing.T) {
			s, store, cfg := newReliabilityTestServer(t)
			insertRetireFixture(t, s, "m_manual", "manual-box", "rt-manual11", 9254)
			s.cfg.AgentUpdates.Enabled = true
			s.cfg.AgentUpdates.LatestVersion = "99.0.0"
			admin, _ := adminTestToken(t, store, cfg)
			f := fenceNow(t, s)
			if fault == "audit" {
				failAudit(t, store)
			} else {
				failJournal(t, store)
			}
			rec := doJSON(t, s, http.MethodPost, "/api/admin/machines/m_manual/update-agent", "", admin)
			var commands int
			_ = store.db.QueryRow(`SELECT COUNT(*) FROM agent_commands WHERE machine_id='m_manual' AND type='update_agent'`).Scan(&commands)
			if fault == "audit" {
				if rec.Code != 200 || commands != 1 {
					t.Fatalf("manual update = %d, %d commands", rec.Code, commands)
				}
				if w := durable(t, s, f); !strings.Contains(strings.Join(w, "\n"), "agent_update.queue") {
					t.Fatalf("manual update not journalled: %v", w)
				}
			} else if rec.Code == 200 || commands != 0 {
				t.Fatalf("journal failure: HTTP %d, %d commands queued", rec.Code, commands)
			}
		})
	}
}

// Daybreak round 4 P2-04: one open update command per machine, also under
// concurrent requests.
func TestOnlyOneOpenUpdatePerMachine(t *testing.T) {
	s, store, cfg := newReliabilityTestServer(t)
	insertRetireFixture(t, s, "m_one", "one-box", "rt-onebox11", 9255)
	s.cfg.AgentUpdates.Enabled = true
	s.cfg.AgentUpdates.LatestVersion = "99.0.0"
	admin, _ := adminTestToken(t, store, cfg)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doJSON(t, s, http.MethodPost, "/api/admin/machines/m_one/update-agent", "", admin)
		}()
	}
	wg.Wait()
	var n int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM agent_commands WHERE machine_id='m_one' AND type='update_agent' AND status='pending'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d open update commands", n)
	}
}

// Daybreak round 4 P3-01: no-op changes write no journal row, and old rows
// are pruned by maintenance.
func TestJournalSkipsNoOpsAndIsPruned(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_n", "n-box", "rt-nbox1111", 9256)
	admin, _ := adminTestToken(t, store, cfg)
	count := func() int {
		var n int
		_ = store.db.QueryRow(`SELECT COUNT(*) FROM durable_actions`).Scan(&n)
		return n
	}
	doJSON(t, server, http.MethodPost, "/api/admin/machines/m_n/update-policy", `{"update_policy":"auto"}`, admin)
	before := count()
	for i := 0; i < 5; i++ {
		if rec := doJSON(t, server, http.MethodPost, "/api/admin/machines/m_n/update-policy", `{"update_policy":"auto"}`, admin); rec.Code != 200 {
			t.Fatalf("repeat policy = %d", rec.Code)
		}
	}
	if count() != before {
		t.Fatalf("no-op policy updates wrote %d journal rows", count()-before)
	}
	if err := server.revokeServiceToken(context.Background(), "sat_does_not_exist"); err != nil {
		t.Fatal(err)
	}
	if count() != before {
		t.Fatal("revoking an unknown token wrote a journal row")
	}
	if _, err := store.db.Exec(`UPDATE durable_actions SET created_at='2020-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("%d old journal rows left", count())
	}
}

// Daybreak round 5 N4: whether an update is durable comes from its origin,
// not from the actor's name. An admin called "system" (legacy DB row) is
// still journalled; the auto policy never is; "system" cannot be
// configured as an admin name.
func TestUpdateOriginIsTyped(t *testing.T) {
	s, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, s, "m_o", "o-box", "rt-obox1111", 9258)
	s.cfg.AgentUpdates.Enabled = true
	s.cfg.AgentUpdates.LatestVersion = "99.0.0"
	m, _ := s.prov.GetMachine(context.Background(), "m_o")
	f := fenceNow(t, s)
	if err := s.queueAgentUpdate(context.Background(), m, "system", updateManual); err != nil {
		t.Fatal(err)
	}
	if w := durable(t, s, f); !strings.Contains(strings.Join(w, "\n"), "journal #") {
		t.Fatalf("manual update by an admin named system not journalled: %v", w)
	}
	if _, err := store.db.Exec(`UPDATE agent_commands SET status='acked'`); err != nil {
		t.Fatal(err)
	}
	f2 := fenceNow(t, s)
	if _, err := store.db.Exec(`UPDATE machines SET created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	if err := s.queueAgentUpdate(context.Background(), m, "anyone", updateAutoPolicy); err != nil {
		t.Fatal(err)
	}
	if w := durable(t, s, f2); len(w) != 0 {
		t.Fatalf("auto-policy update blocks rollback: %v", w)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("initial_admin:\n  username: system\n"), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("reserved admin username accepted")
	}
}
