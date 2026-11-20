package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fenceAfter records a fence one second in the past so writes in the same
// second as the fence count as "after" it (timestamps are second precision).
func fenceNow(t *testing.T, server *Server) deployFence {
	t.Helper()
	f, err := recordDeployFence(context.Background(), server.store.db, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func durable(t *testing.T, server *Server, f deployFence) []string {
	t.Helper()
	w, err := durableWritesSince(context.Background(), server.store.db, f)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Discardable traffic (heartbeats, lazy token migration, health probes,
// maintenance, pending registrations) must not block an automatic rollback.
func TestDeployFenceIgnoresDiscardableWrites(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{}
	legacy, _ := HashSecret("at_legacy")
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at,desired_changed_at,cleanup_state) VALUES ('m_f','f-box','f-box','offline','active','offline',1,?,?,?,NULL)`, legacy, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,created_at,desired_changed_at,cleanup_state) VALUES ('m_old','old-box','old-box','retiring','retiring','gone','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','pending')`); err != nil {
		t.Fatal(err)
	}
	f := fenceNow(t, server)
	body := `{"machine_id":"m_f","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{"state":"healthy"},"tunnel":{"state":"connected"},"persistence":{},"keys":{}}}`
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_legacy"); rec.Code != 200 {
		t.Fatalf("heartbeat %d", rec.Code)
	}
	server.prov.listListeners = func(context.Context) (map[int]bool, error) { return map[int]bool{}, nil }
	if _, err := server.prov.RunHealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"mode":"system","local_port":22,"name":"pending-box"}`, ""); rec.Code != 200 {
		t.Fatalf("register %d", rec.Code)
	}
	if w := durable(t, server, f); len(w) != 0 {
		t.Fatalf("discardable writes reported as durable: %v", w)
	}
}

// Every operator/enrollment decision must be detected.
func TestDeployFenceDetectsDurableWrites(t *testing.T) {
	type tc struct {
		want string
		act  func(t *testing.T, server *Server, admin string)
	}
	cases := map[string]tc{
		"disable": {"machine.desired.disabled", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/machines/m_d/disable", "", admin)
		}},
		"remove": {"machine.desired.retiring", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/machines/m_d/remove", "", admin)
		}},
		"failed disable": {"machine.disable_failed", func(t *testing.T, s *Server, admin string) {
			s.prov.osOps = &fakeTunnelOS{failDisable: true}
			doJSON(t, s, http.MethodPost, "/api/admin/machines/m_d/disable", "", admin)
		}},
		"approve": {"request.approve", func(t *testing.T, s *Server, admin string) {
			s.registerL = newRateLimiter(100, time.Minute)
			rec := doJSON(t, s, http.MethodPost, "/api/client/register", `{"mode":"system","local_port":22,"name":"to-approve"}`, "")
			var id string
			_ = s.store.db.QueryRow(`SELECT id FROM requests ORDER BY created_at DESC LIMIT 1`).Scan(&id)
			_ = rec
			doJSON(t, s, http.MethodPost, "/api/admin/requests/"+id+"/approve", "", admin)
		}},
		"service token": {"service_token.create", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/service-tokens", `{"label":"x"}`, admin)
		}},
		"revoke-all": {"sessions.revoke_all", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/sessions/revoke-all", "", admin)
		}},
		"logout": {"session logged out", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/logout", "", admin)
		}},
		"update policy": {"machine.update_policy.update", func(t *testing.T, s *Server, admin string) {
			doJSON(t, s, http.MethodPost, "/api/admin/machines/m_d/update-policy", `{"update_policy":"auto"}`, admin)
		}},
		"invite claim": {"invite claimed", func(t *testing.T, s *Server, admin string) {
			hash, _ := HashSecret("inv-code")
			if _, err := s.store.db.Exec(`INSERT INTO invites (code_hash,type,expires_at,created_at) VALUES (?,'friend',?,'2026-01-01T00:00:00Z')`, hash, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			doJSON(t, s, http.MethodPost, "/api/client/register", `{"mode":"system","local_port":22,"name":"inv-box","auth_code":"inv-code"}`, "")
		}},
		"god-code provision": {"machine.create", func(t *testing.T, s *Server, admin string) {
			_, _ = s.prov.CreateMachine(context.Background(), CreateMachineInput{Slug: "new-box", Pubkey: testPubkey})
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			server, store, cfg := newReliabilityTestServer(t)
			server.prov.osOps = &fakeTunnelOS{}
			insertRetireFixture(t, server, "m_d", "d-box", "rt-dbox1111", 9250)
			admin, _ := adminTestToken(t, store, cfg)
			f := fenceNow(t, server)
			// Back-date the fixture so only the action under test is new.
			if _, err := store.db.Exec(`UPDATE machines SET created_at='2026-01-01T00:00:00Z'`); err != nil {
				t.Fatal(err)
			}
			if w := durable(t, server, f); len(w) != 0 {
				t.Fatalf("fixture already durable: %v", w)
			}
			c.act(t, server, admin)
			w := durable(t, server, f)
			if !strings.Contains(strings.Join(w, "\n"), c.want) {
				t.Fatalf("%s not detected (want %q): %v", name, c.want, w)
			}
		})
	}
}

// Daybreak round 4 P1-02: fence first, then snapshot, under the exclusive
// writers lock. A write that a writer managed to make before the lock is in
// both (restore refused, which is safe); while the lock is held no writer
// can start.
func TestRecordFenceThenSnapshot(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_s", "s-box", "rt-sbox1111", 9251)
	dir := t.TempDir()
	fence := filepath.Join(dir, "fence.json")
	snap := filepath.Join(dir, "snap.db")
	if err := recordFenceAndSnapshot(context.Background(), store.path, fence, snap); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(fence)
	var f deployFence
	if err := json.Unmarshal(b, &f); err != nil || !f.JournalPresent {
		t.Fatalf("fence %s %v", b, err)
	}
	// The snapshot contains everything up to the fence.
	s2, err := OpenStore(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n int
	_ = s2.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE id='m_s'`).Scan(&n)
	if n != 1 {
		t.Fatal("snapshot misses data written before the fence")
	}
	// A durable action after the fence is detected.
	if _, err := server.prov.SetDesiredState(context.Background(), "m_s", DesiredDisabled, "t"); err != nil {
		t.Fatal(err)
	}
	if w := durable(t, server, f); len(w) == 0 {
		t.Fatal("action after the fence not detected")
	}
	// A running writer (shared lock) blocks the record step.
	release, err := acquireWritersShared(store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := recordFenceAndSnapshot(context.Background(), store.path, filepath.Join(dir, "f2.json"), filepath.Join(dir, "s2.db")); err == nil {
		t.Fatal("fence recorded while a writer held the lock")
	}
}
