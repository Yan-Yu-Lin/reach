package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func newReliabilityTestServer(t *testing.T) (*Server, *Store, Config) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := DefaultConfig()
	cfg.ProvisioningEnabled = false
	cfg.InitialAdmin.PasswordHash, err = HashSecret("test-password")
	if err != nil {
		t.Fatal(err)
	}
	cfg.JWTSecret = "test-jwt-secret-with-sufficient-length"
	if err := store.Migrate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	prov := NewProvisioner(store, cfg)
	return NewServer(cfg, store, prov), store, cfg
}

func insertTestMachine(t *testing.T, store *Store, id, slug string) {
	t.Helper()
	_, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,created_at) VALUES (?,?,?,'offline','active','offline',?)`, id, slug, slug, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseReadinessCheck(t *testing.T) {
	_, store, _ := newReliabilityTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseReadinessRequiresMigratedSchema(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.CheckReady(ctx); err == nil || !strings.Contains(err.Error(), "schema is incomplete") {
		t.Fatalf("CheckReady error = %v, want incomplete schema", err)
	}
}

func TestPendingCommandsRetryLeaseAndConcurrentClaims(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_claim", "claim-box")
	now := time.Now().UTC()
	fresh := now.Add(-commandRetryLease / 2).Format(time.RFC3339)
	stale := now.Add(-commandRetryLease - time.Second).Format(time.RFC3339)
	expired := now.Add(-time.Second).Format(time.RFC3339)
	for i := 0; i < 19; i++ {
		id := fmt.Sprintf("cmd_%02d", i)
		if _, err := store.db.Exec(`INSERT INTO agent_commands (id,machine_id,generation,type,status,created_at) VALUES (?,'m_claim',1,'sync_keys','pending',?)`, id, now.Add(time.Duration(i)*time.Millisecond).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`INSERT INTO agent_commands (id,machine_id,generation,type,status,created_at,sent_at) VALUES ('cmd_fresh','m_claim',1,'sync_keys','sent',?,?),('cmd_stale','m_claim',1,'sync_keys','sent',?,?)`, nowUTC(), fresh, nowUTC(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO agent_commands (id,machine_id,generation,type,status,created_at,expires_at) VALUES ('cmd_expired','m_claim',1,'sync_keys','pending',?,?)`, nowUTC(), expired); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([][]AgentCommandDelivery, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = server.pendingCommands(context.Background(), "m_claim")
		}(i)
	}
	close(start)
	wg.Wait()
	seen := map[string]bool{}
	var ids []string
	for i, result := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if len(result) != 10 {
			t.Fatalf("claim %d returned %d commands, want 10", i, len(result))
		}
		for _, command := range result {
			if seen[command.ID] {
				t.Fatalf("command %s returned by concurrent claims", command.ID)
			}
			seen[command.ID] = true
			ids = append(ids, command.ID)
		}
	}
	if seen["cmd_fresh"] || seen["cmd_expired"] {
		t.Fatalf("fresh-sent or expired command was claimed: %v", ids)
	}
	sort.Strings(ids)
	if !seen["cmd_stale"] {
		t.Fatalf("stale command was not retried: %v", ids)
	}
}

func TestEventPublishFailureUsesCursorlessResync(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	ch, _, _, cancel, err := server.events.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := store.db.Exec(`DROP TABLE events`); err != nil {
		t.Fatal(err)
	}
	ev := server.events.Publish(context.Background(), "machine.changed", "m_one", map[string]any{"ok": true})
	if ev.ID != "" || ev.Type != "machine.resync_required" {
		t.Fatalf("publish failure returned resumable event: %#v", ev)
	}
	got, ok := <-ch
	if !ok || got.ID != "" || got.Type != "machine.resync_required" {
		t.Fatalf("subscriber did not receive cursorless resync: %#v open=%v", got, ok)
	}
	if _, open := <-ch; open {
		t.Fatal("subscriber remained open after persistence failure")
	}
	var out bytes.Buffer
	if err := writeSSE(&out, got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "id:\nevent: machine.resync_required") {
		t.Fatalf("resync did not clear SSE cursor:\n%s", out.String())
	}
}

func TestEventSubscriberOverflowRequestsResyncAndCloses(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	ch, _, _, cancel, err := server.events.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for i := 0; i <= eventSubscriberBuffer; i++ {
		server.events.Publish(context.Background(), "machine.changed", "m_one", map[string]any{"i": i})
	}
	got, ok := <-ch
	if !ok || got.Type != "machine.resync_required" || got.Data["reason"] != "subscriber_overflow" {
		t.Fatalf("overflow did not signal resync: %#v open=%v", got, ok)
	}
	if _, open := <-ch; open {
		t.Fatal("overflowed subscriber remained open")
	}
}

func TestPendingCommandsWorksWithOneDatabaseConnection(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	insertTestMachine(t, store, "m_one", "one-box")
	if _, err := store.db.Exec(`INSERT INTO agent_commands (id,machine_id,generation,type,status,created_at) VALUES ('cmd_one','m_one',1,'sync_keys','pending',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	commands, err := server.pendingCommands(ctx, "m_one")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].ID != "cmd_one" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
	var status string
	if err := store.db.QueryRow(`SELECT status FROM agent_commands WHERE id='cmd_one'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("command status = %q, want sent", status)
	}
}

func TestAdminEventsNoCursorIsLiveOnlyAndCursorReplays(t *testing.T) {
	server, _, cfg := newReliabilityTestServer(t)
	server.events.Publish(context.Background(), "machine.old", "m_old", map[string]any{"old": true})
	token, err := SignJWT(cfg.JWTSecret, "u_test", "tester", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	readThroughHello := func(lastID string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event status = %d", resp.StatusCode)
		}
		var out strings.Builder
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			out.WriteString(line)
			out.WriteByte('\n')
			if line == "event: hello" {
				if scanner.Scan() {
					out.WriteString(scanner.Text())
					out.WriteByte('\n')
				}
				return out.String()
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	withoutCursor := readThroughHello("")
	if strings.Contains(withoutCursor, "event: machine.old") {
		t.Fatalf("no-cursor stream replayed history:\n%s", withoutCursor)
	}
	withCursor := readThroughHello("0")
	if !strings.Contains(withCursor, "event: machine.old") {
		t.Fatalf("cursor stream did not replay history:\n%s", withCursor)
	}
	if strings.Contains(withCursor, "id: 0\nevent: hello") {
		t.Fatalf("hello reset durable event cursor:\n%s", withCursor)
	}
}

func TestEventReplayOverflowRequiresResnapshot(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < eventReplayLimit+1; i++ {
		if _, err := tx.Exec(`INSERT INTO events (type,payload_json,created_at) VALUES ('test.page','{}',?)`, nowUTC()); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token, err := SignJWT(cfg.JWTSecret, "u_test", "tester", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Last-Event-ID", "0")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "event: machine.resync_required") || !strings.Contains(out.String(), "replay_overflow") {
		t.Fatalf("overflow response did not request resnapshot:\n%s", out.String())
	}
	if strings.Contains(out.String(), "event: test.page") {
		t.Fatalf("overflow response partially replayed events:\n%s", out.String())
	}
}

func TestMachineDetailLoadsOnlyRequestedMachine(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_target", "target-box")
	insertTestMachine(t, store, "m_broken", "broken-box")
	if _, err := store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_target','m_target','primary','rt-target1',9201,'ssh-ed25519 test','offline',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO agent_observations (machine_id,applied_generation,heartbeat_at,raw_json) VALUES ('m_target',1,?,'{"secret":"not-for-dashboard"}')`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO agent_observations (machine_id,applied_generation,heartbeat_at) VALUES ('m_broken',NULL,?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.prov.ListMachines(context.Background()); err == nil {
		t.Fatal("test fixture should make the full fleet scan fail")
	}

	detail, err := server.prov.GetMachineDetail(context.Background(), "m_target")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Machine.ID != "m_target" || len(detail.Tunnels) != 1 || detail.Agent == nil {
		t.Fatalf("unexpected targeted detail: %#v", detail)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "raw_json") || strings.Contains(string(encoded), "not-for-dashboard") {
		t.Fatalf("raw observation leaked in dashboard response: %s", encoded)
	}
}

func TestFullListUsesNewestHubObservation(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_hub", "hub-box")
	if _, err := store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_old','m_hub','primary','rt-hubold1',9201,'ssh-ed25519 old','offline',?),('t_new','m_hub','primary','rt-hubnew1',9202,'ssh-ed25519 new','offline',?)`, nowUTC(), nowUTC()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	newest := nowUTC()
	if _, err := store.db.Exec(`INSERT INTO hub_observations (tunnel_id,machine_id,probe_state,last_probe_at) VALUES ('t_old','m_hub','unreachable',?),('t_new','m_hub','reachable',?)`, old, newest); err != nil {
		t.Fatal(err)
	}
	list, err := server.prov.ListMachines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].HubObservation == nil || list[0].HubObservation.TunnelID != "t_new" || list[0].HubObservation.ProbeState != "reachable" {
		t.Fatalf("full list did not choose newest hub observation: %#v", list)
	}
}

func TestMaintenanceCleansExpiredBlacklistAndOldEvents(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	now := time.Now().UTC()
	expired := now.Add(-time.Hour).Format(time.RFC3339)
	active := now.Add(time.Hour).Format(time.RFC3339)
	oldEvent := now.Add(-eventRetention - time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO jwt_blacklist (token_hash,expires_at,created_at) VALUES ('expired',?,?),('active',?,?)`, expired, expired, active, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO events (type,payload_json,created_at) VALUES ('old','{}',?),('new','{}',?)`, oldEvent, nowUTC()); err != nil {
		t.Fatal(err)
	}

	blacklisted, err := server.isJWTBlacklisted(context.Background(), "not-present")
	if err != nil {
		t.Fatal(err)
	}
	if blacklisted {
		t.Fatal("unexpected blacklist match")
	}
	var before int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM jwt_blacklist WHERE token_hash='expired'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatal("authentication path mutated blacklist state")
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var expiredCount, activeCount, oldCount, newCount int
	queries := []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM jwt_blacklist WHERE token_hash='expired'`, &expiredCount},
		{`SELECT COUNT(*) FROM jwt_blacklist WHERE token_hash='active'`, &activeCount},
		{`SELECT COUNT(*) FROM events WHERE type='old'`, &oldCount},
		{`SELECT COUNT(*) FROM events WHERE type='new'`, &newCount},
	}
	for _, q := range queries {
		if err := store.db.QueryRow(q.query).Scan(q.dest); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
	}
	if expiredCount != 0 || activeCount != 1 || oldCount != 0 || newCount != 1 {
		t.Fatalf("unexpected cleanup counts: expired=%d active=%d old=%d new=%d", expiredCount, activeCount, oldCount, newCount)
	}
}
