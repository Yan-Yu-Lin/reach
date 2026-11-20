package main

import (
	"context"
	"database/sql"
	"errors"
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
	id, secret := registerAndApprove(t, server, cfg, `{"mode":"system","local_port":22,"name":"expired-setup"}`)
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
	id, secret := registerAndApprove(t, server, cfg, `{"mode":"system","local_port":22,"name":"used-setup"}`)
	token := setupTokenFor(t, server, id, secret)
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, token, map[string]any{"slug": "used-setup"}), ""); rec.Code != 200 {
		t.Fatalf("first provision %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, token, map[string]any{"slug": "used-setup"}), ""); rec.Code == http.StatusServiceUnavailable || rec.Code == 200 {
		t.Fatalf("reused token = %d", rec.Code)
	}
}

// Daybreak P2-7: a reachd crash mid-provision must not leave the request
// (and its invite claim) stuck in "provisioning" forever, and a half-made
// machine must be cleaned up (retired) or kept as a cleanup_failed tombstone.
func TestStuckProvisioningIsRecovered(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	hash, _ := HashSecret("inv-stuck")
	if _, err := store.db.Exec(`INSERT INTO invites (code_hash,type,expires_at,created_at) VALUES (?,'friend',?,?)`, hash, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), nowUTC()); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"mode":"system","local_port":22,"name":"stuck-box","auth_code":"inv-stuck"}`, "")
	if rec.Code != 200 {
		t.Fatalf("register %d", rec.Code)
	}
	var id string
	_ = store.db.QueryRow(`SELECT id FROM requests`).Scan(&id)
	// Simulate the crash: consumed token, machine row in provisioning state.
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	insertTestMachine(t, store, "m_stuck", "stuck-box")
	if _, err := store.db.Exec(`UPDATE machines SET status='provisioning' WHERE id='m_stuck'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE requests SET status='provisioning', setup_token_hash=NULL, provisioning_started_at=?, machine_id='m_stuck' WHERE id=?`, old, id); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reqStatus, mStatus string
	var claimed *string
	_ = store.db.QueryRow(`SELECT status FROM requests WHERE id=?`, id).Scan(&reqStatus)
	_ = store.db.QueryRow(`SELECT status FROM machines WHERE id='m_stuck'`).Scan(&mStatus)
	_ = store.db.QueryRow(`SELECT claimed_by_request_id FROM invites`).Scan(&claimed)
	// The half-made machine is retired through the verified cleanup path
	// (round 3 P2-A), not just labelled failed.
	if reqStatus != "expired" || mStatus != "retired" || claimed != nil {
		t.Fatalf("after recovery: request=%s machine=%s invite claim=%v", reqStatus, mStatus, claimed)
	}
	// A provision still inside its lease is left alone.
	if _, err := store.db.Exec(`UPDATE requests SET status='provisioning', provisioning_started_at=? WHERE id=?`, nowUTC(), id); err != nil {
		t.Fatal(err)
	}
	_ = server.prov.ExpireOnce(context.Background())
	_ = store.db.QueryRow(`SELECT status FROM requests WHERE id=?`, id).Scan(&reqStatus)
	if reqStatus != "provisioning" {
		t.Fatalf("in-lease provision was recovered: %s", reqStatus)
	}
}

// Astra R2-10 / daybreak round 3 P2-A: the crash may happen after the
// machine went offline but before the request was finalized, with OS auth
// in place and a valid agent token. Recovery must revoke the token, remove
// the hub account, and retire the machine and tunnel together.
func TestRecoveryRevokesHalfFinishedProvision(t *testing.T) {
	for _, mstatus := range []string{"provisioning", "offline", "failed"} {
		t.Run(mstatus, func(t *testing.T) {
			server, store, _ := newReliabilityTestServer(t)
			fos := &fakeTunnelOS{residue: map[string][]string{"rt-orphan11": {"/etc/ssh/sshd_config.d/90-rt-orphan11.conf"}}}
			server.prov.osOps = fos
			insertRetireFixture(t, server, "m_orphan", "orphan-box", "rt-orphan11", 9281)
			if _, err := store.db.Exec(`UPDATE machines SET status=? WHERE id='m_orphan'`, mstatus); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
			if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at,machine_id) VALUES ('r_orphan','orphan-box','sha256:x','provisioning',?,?,'orphan-box',?,'m_orphan')`, old, future, old); err != nil {
				t.Fatal(err)
			}
			if err := server.prov.recoverStuckProvisioning(context.Background(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			var reqStatus, mStat, desired, tStatus string
			var token sql.NullString
			_ = store.db.QueryRow(`SELECT status FROM requests WHERE id='r_orphan'`).Scan(&reqStatus)
			_ = store.db.QueryRow(`SELECT status,desired_state,agent_token_hash FROM machines WHERE id='m_orphan'`).Scan(&mStat, &desired, &token)
			_ = store.db.QueryRow(`SELECT status FROM tunnels WHERE machine_id='m_orphan'`).Scan(&tStatus)
			if reqStatus != "expired" || token.Valid || desired != DesiredRetired || tStatus != "retired" {
				t.Fatalf("request=%s machine=%s desired=%s tunnel=%s token=%v", reqStatus, mStat, desired, tStatus, token.Valid)
			}
			if len(fos.removed) == 0 {
				t.Fatal("hub account was not removed")
			}
		})
	}
}

// If the hub cleanup fails the machine stays a cleanup_failed tombstone
// with its token revoked, and maintenance retries until it succeeds.
func TestRecoveryCleanupFailureIsRetried(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	fos := &fakeTunnelOS{failRemove: true}
	server.prov.osOps = fos
	insertRetireFixture(t, server, "m_o2", "o2-box", "rt-orphan22", 9282)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at,machine_id) VALUES ('r_o2','o2-box','sha256:x','provisioning',?,?,'o2-box',?,'m_o2')`, old, future, old); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mStat, tStatus string
	var token sql.NullString
	_ = store.db.QueryRow(`SELECT status,agent_token_hash FROM machines WHERE id='m_o2'`).Scan(&mStat, &token)
	_ = store.db.QueryRow(`SELECT status FROM tunnels WHERE machine_id='m_o2'`).Scan(&tStatus)
	if mStat != CleanupFailed || token.Valid || tStatus != "cleanup_failed" {
		t.Fatalf("after failed cleanup: machine=%s tunnel=%s token=%v", mStat, tStatus, token.Valid)
	}
	fos.mu.Lock()
	fos.failRemove = false
	fos.mu.Unlock()
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = store.db.QueryRow(`SELECT status FROM machines WHERE id='m_o2'`).Scan(&mStat)
	if mStat != "retired" {
		t.Fatalf("maintenance did not retry the cleanup: %s", mStat)
	}
}

// The normal finalize must not succeed (or hand out the agent token) if
// recovery reclaimed the request while CreateMachine ran.
func TestProvisionFinalizeLosesToRecovery(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at) VALUES ('r_l','l-box','sha256:x','expired',?,?,'l-box',?)`, nowUTC(), future, nowUTC()); err != nil {
		t.Fatal(err)
	}
	_, err := server.prov.CreateMachine(context.Background(), CreateMachineInput{RequestID: "r_l", Slug: "l-box", Pubkey: testPubkey})
	if !errors.Is(err, errProvisioningLeaseLost) {
		t.Fatalf("CreateMachine on a reclaimed request: %v", err)
	}
	var n int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE slug='l-box'`).Scan(&n)
	if n != 0 {
		t.Fatal("machine created for a reclaimed request")
	}
}

// Astra R3-05: the retire intent of a recovered provision fails once. The
// request carries the durable cleanup_pending marker, so the next run
// retires the machine anyway.
func TestRecoveryRetriesWhenRetireIntentFails(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	ops := &fakeTunnelOS{}
	server.prov.osOps = ops
	insertRetireFixture(t, server, "m_retrygap", "retrygap-box", "rt-retrygap", 9292)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at,machine_id,provision_phase) VALUES ('r_retrygap','retrygap-box','sha256:x','provisioning',?,?,'retrygap-box',?,'m_retrygap','os_created')`, old, future, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_retire_intent BEFORE INSERT ON durable_actions WHEN NEW.kind='machine.desired.retired' BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.recoverStuckProvisioning(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var phase string
	_ = store.db.QueryRow(`SELECT provision_phase FROM requests WHERE id='r_retrygap'`).Scan(&phase)
	if phase != phaseCleanupPending {
		t.Fatalf("phase after failed retire = %s", phase)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_retire_intent`); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.recoverStuckProvisioning(context.Background(), time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var desired, tstatus string
	_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_retrygap'`).Scan(&desired)
	_ = store.db.QueryRow(`SELECT status FROM tunnels WHERE machine_id='m_retrygap'`).Scan(&tstatus)
	_ = store.db.QueryRow(`SELECT provision_phase FROM requests WHERE id='r_retrygap'`).Scan(&phase)
	if desired != DesiredRetired || tstatus != "retired" || phase != phaseCleaned || len(ops.removed) == 0 {
		t.Fatalf("after retry: desired=%s tunnel=%s phase=%s removed=%v", desired, tstatus, phase, ops.removed)
	}
}

// Daybreak round 4 P2-02 window 1: the request was committed as provisioned
// but the agent never got the response (no heartbeat). After the response
// lease the machine is revoked and retired; a delivered machine is not.
func TestUndeliveredProvisionIsCleanedUp(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	ops := &fakeTunnelOS{}
	server.prov.osOps = ops
	insertRetireFixture(t, server, "m_lost", "lost-box", "rt-lostbox1", 9293)
	insertRetireFixture(t, server, "m_ok", "ok-box", "rt-okbox111", 9294)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, r := range [][3]string{{"r_lost", "m_lost", "response_pending"}, {"r_ok", "m_ok", "delivered"}} {
		if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at,machine_id,provision_phase) VALUES (?,?,'sha256:x','provisioned',?,?,?,?,?,?)`, r[0], r[0], old, future, r[0], old, r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.prov.recoverStuckProvisioning(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var lostDesired, okDesired string
	var lostToken, okToken sql.NullString
	_ = store.db.QueryRow(`SELECT desired_state,agent_token_hash FROM machines WHERE id='m_lost'`).Scan(&lostDesired, &lostToken)
	_ = store.db.QueryRow(`SELECT desired_state,agent_token_hash FROM machines WHERE id='m_ok'`).Scan(&okDesired, &okToken)
	if lostDesired != DesiredRetired || lostToken.Valid {
		t.Fatalf("undelivered machine: desired=%s token=%v", lostDesired, lostToken.Valid)
	}
	if okDesired != DesiredActive || !okToken.Valid {
		t.Fatalf("delivered machine touched: desired=%s token=%v", okDesired, okToken.Valid)
	}
}

// The first heartbeat marks the provision delivered.
func TestHeartbeatMarksProvisionDelivered(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_hb", "hb-box", "rt-hbbox111", 9295)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO requests(id,name,client_secret_hash,status,created_at,expires_at,slug,provisioning_started_at,machine_id,provision_phase) VALUES ('r_hb','hb','sha256:x','provisioned',?,?,'hb-box',?,'m_hb','response_pending')`, nowUTC(), future, nowUTC()); err != nil {
		t.Fatal(err)
	}
	body := `{"machine_id":"m_hb","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{"state":"healthy"},"tunnel":{"state":"connected"},"persistence":{},"keys":{}}}`
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_m_hb"); rec.Code != 200 {
		t.Fatalf("heartbeat %d %s", rec.Code, rec.Body.String())
	}
	var phase string
	_ = store.db.QueryRow(`SELECT provision_phase FROM requests WHERE id='r_hb'`).Scan(&phase)
	if phase != phaseDelivered {
		t.Fatalf("phase = %s", phase)
	}
}
