package main

import (
	"context"
	"net/http"
	"testing"
)

// Astra P2-01: a heartbeat must not turn disable_failed/cleanup_failed back
// into a plain disabled/retiring status.
func TestHeartbeatKeepsCleanupFailureStatus(t *testing.T) {
	for _, action := range []string{DesiredDisabled, DesiredRetiring, DesiredRetired} {
		t.Run(action, func(t *testing.T) {
			server, store, _ := newReliabilityTestServer(t)
			server.prov.osOps = &fakeTunnelOS{failDisable: true, failRemove: true}
			insertRetireFixture(t, server, "m_review", "review-box", "rt-review11", 9241)
			if _, err := server.prov.SetDesiredState(context.Background(), "m_review", action, "review"); err == nil {
				t.Fatal("expected cleanup failure")
			}
			var before string
			_ = store.db.QueryRow(`SELECT status FROM machines WHERE id='m_review'`).Scan(&before)
			rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", `{"machine_id":"m_review","observed":{"tunnel":{"state":"connected"},"local_ssh":{"state":"healthy"}}}`, "at_m_review")
			if rec.Code != 200 {
				t.Fatalf("heartbeat %d %s", rec.Code, rec.Body.String())
			}
			var after, cleanup string
			_ = store.db.QueryRow(`SELECT status,cleanup_state FROM machines WHERE id='m_review'`).Scan(&after, &cleanup)
			if after != before || after != cleanup {
				t.Fatalf("status %s -> %s (cleanup_state %s)", before, after, cleanup)
			}
		})
	}
}

func TestEffectiveStatusPrecedence(t *testing.T) {
	cases := []struct{ desired, cleanup, observed, want string }{
		{DesiredActive, "", ObservedOnline, ObservedOnline},
		{DesiredDisabled, "", ObservedOnline, DesiredDisabled},
		{DesiredDisabled, DisableFailed, ObservedOnline, DisableFailed},
		{DesiredRetiring, CleanupFailed, ObservedGone, CleanupFailed},
		{DesiredRetiring, CleanupTimedOut, ObservedGone, DesiredRetiring},
		{DesiredRetired, CleanupComplete, ObservedGone, DesiredRetired},
	}
	for _, c := range cases {
		if got := effectiveStatus(c.desired, c.cleanup, c.observed); got != c.want {
			t.Fatalf("%+v -> %s", c, got)
		}
	}
}

// Astra P2-02: after a failed admin remove, the agent's uninstall-intent must
// retry the revoke and report failure, not return 200.
func TestUninstallIntentRetriesPriorDisableFailure(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	fake := &fakeTunnelOS{failDisable: true}
	server.prov.osOps = fake
	insertRetireFixture(t, server, "m_retry", "retry-box", "rt-retry111", 9243)
	admin, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/machines/m_retry/remove", "", admin); rec.Code != http.StatusConflict {
		t.Fatalf("remove %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/uninstall-intent", `{"machine_id":"m_retry","reason":"retry"}`, "at_m_retry"); rec.Code != http.StatusConflict {
		t.Fatalf("uninstall-intent with failing OS = %d, want 409", rec.Code)
	}
	fake.failDisable = false
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/uninstall-intent", `{"machine_id":"m_retry","reason":"retry"}`, "at_m_retry"); rec.Code != 200 {
		t.Fatalf("uninstall-intent after fix = %d", rec.Code)
	}
	var status, cleanup string
	_ = store.db.QueryRow(`SELECT status,cleanup_state FROM machines WHERE id='m_retry'`).Scan(&status, &cleanup)
	if status != DesiredRetiring || cleanup != CleanupPending {
		t.Fatalf("after successful retry status=%s cleanup=%s", status, cleanup)
	}
}
