package main

import (
	"context"
	"net/http"
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
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"pending-box"}`, ""); rec.Code != 200 {
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
			rec := doJSON(t, s, http.MethodPost, "/api/client/register", `{"name":"to-approve"}`, "")
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
			doJSON(t, s, http.MethodPost, "/api/client/register", `{"name":"inv-box","auth_code":"inv-code"}`, "")
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
