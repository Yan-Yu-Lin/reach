package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Astra P2-08: an expired command's outcome must not depend on whether
// maintenance ran before the late result arrived.
func TestExpiredCommandResultIndependentOfMaintenanceOrder(t *testing.T) {
	for _, maintenanceFirst := range []bool{false, true} {
		server, store, _ := newReliabilityTestServer(t)
		insertRetireFixture(t, server, "m_cmdreview", "cmdreview", "rt-cmdrev11", 9244)
		if err := server.prov.QueueCommand(context.Background(), "m_cmdreview", 1, "stop_tunnel", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
		var id string
		_ = store.db.QueryRow(`SELECT id FROM agent_commands WHERE machine_id='m_cmdreview'`).Scan(&id)
		if _, err := store.db.Exec(`UPDATE agent_commands SET status='sent',expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
		if maintenanceFirst {
			if err := server.prov.expireCommands(context.Background(), nowUTC()); err != nil {
				t.Fatal(err)
			}
		}
		body := `{"machine_id":"m_cmdreview","command_results":[{"command_id":"` + id + `","status":"acked"}]}`
		if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_m_cmdreview"); rec.Code != 200 {
			t.Fatalf("heartbeat %d", rec.Code)
		}
		var status string
		_ = store.db.QueryRow(`SELECT status FROM agent_commands WHERE id=?`, id).Scan(&status)
		if status == "acked" {
			t.Fatalf("maintenanceFirst=%v: expired command was acked", maintenanceFirst)
		}
		var events int
		_ = store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type LIKE 'agent.command_%'`).Scan(&events)
		if events != 0 {
			t.Fatalf("late result published %d events", events)
		}
	}
}

// Astra P2-09: an expired setup token is terminal, not "busy".
func TestExpiredSetupTokenIsTerminal(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"expired-setup"}`)
	token := setupTokenFor(t, server, id, secret)
	if _, err := store.db.Exec(`UPDATE requests SET setup_token_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, token, map[string]any{"slug": "expired-setup"}), "")
	if rec.Code != http.StatusGone || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("expired token = %d Retry-After=%q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

func TestUsedSetupTokenIsUnauthorizedNotBusy(t *testing.T) {
	server, _, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"used-setup"}`)
	token := setupTokenFor(t, server, id, secret)
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, token, map[string]any{"slug": "used-setup"}), ""); rec.Code != 200 {
		t.Fatalf("first provision %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, token, map[string]any{"slug": "used-setup"}), ""); rec.Code == http.StatusServiceUnavailable || rec.Code == 200 {
		t.Fatalf("reused token = %d", rec.Code)
	}
}
