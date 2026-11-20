package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTunnelOS simulates hub-side failures without root.
type fakeTunnelOS struct {
	mu          sync.Mutex
	failDisable bool
	failRemove  bool
	residue     map[string][]string
	removed     []string
	disabled    []string
	dropIn      map[int]bool
}

func (f *fakeTunnelOS) DisableAuth(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDisable {
		return errors.New("rename authorized_keys: permission denied")
	}
	f.disabled = append(f.disabled, u)
	return nil
}
func (f *fakeTunnelOS) EnableAuth(context.Context, string) error { return nil }
func (f *fakeTunnelOS) Remove(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRemove {
		return errors.New("remove /etc/ssh/sshd_config.d/90-" + u + ".conf: not a directory")
	}
	f.removed = append(f.removed, u)
	delete(f.residue, u)
	return nil
}
func (f *fakeTunnelOS) Residue(_ context.Context, u string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.residue[u], nil
}
func (f *fakeTunnelOS) DropInPorts() (map[int]bool, error) {
	if f.dropIn == nil {
		return map[int]bool{}, nil
	}
	return f.dropIn, nil
}

func insertRetireFixture(t *testing.T, server *Server, id, slug, user string, port int) {
	t.Helper()
	if _, err := server.store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES (?,?,?,'offline','active','offline',1,?,?)`, id, slug, slug, HashToken("at_"+id), nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,original_unix_user,remote_port,original_remote_port,tunnel_pubkey,status,created_at) VALUES (?,?,'primary',?,?,?,?,'ssh-ed25519 x','offline',?)`, "t_"+id, id, user, user, port, port, nowUTC()); err != nil {
		t.Fatal(err)
	}
}

// PoC from daybreak P1-2: cleanup fails but the old code reported retired and
// released the port.
func TestRetireFailsClosedAndKeepsPortTombstone(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	fake := &fakeTunnelOS{failRemove: true}
	server.prov.osOps = fake
	insertRetireFixture(t, server, "m_old", "old-box", "rt-oldbox11", 9205)
	_, err := server.prov.SetDesiredState(context.Background(), "m_old", DesiredRetired, "test")
	if !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("retire error = %v, want cleanup failure", err)
	}
	var status, cleanup, desired, tunnelStatus string
	var port int
	_ = store.db.QueryRow(`SELECT status,cleanup_state,desired_state FROM machines WHERE id='m_old'`).Scan(&status, &cleanup, &desired)
	_ = store.db.QueryRow(`SELECT status,remote_port FROM tunnels WHERE id='t_m_old'`).Scan(&tunnelStatus, &port)
	if status != CleanupFailed || cleanup != CleanupFailed || desired != DesiredRetiring || tunnelStatus != "cleanup_failed" || port != 9205 {
		t.Fatalf("after failed retire: status=%s cleanup=%s desired=%s tunnel=%s port=%d", status, cleanup, desired, tunnelStatus, port)
	}
	// The allocator must not hand 9205 to a new machine.
	tx, _ := store.db.Begin()
	hub, _ := getHubTx(context.Background(), tx, "primary")
	hub.PortStart, hub.PortEnd = 9205, 9206
	got, err := allocatePortTx(context.Background(), tx, hub, nil)
	_ = tx.Rollback()
	if err != nil || got != 9206 {
		t.Fatalf("allocator returned %d,%v; tombstone port reused", got, err)
	}
	// Retry once the hub is fixed.
	fake.failRemove = false
	m, err := server.prov.SetDesiredState(context.Background(), "m_old", DesiredRetired, "test")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "retired" || m.CleanupState == nil || *m.CleanupState != CleanupComplete {
		t.Fatalf("retry result = %#v", m)
	}
}

func TestRetireRequiresVerifiedResidueFree(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_res", "res-box", "rt-resid111", 9207)
	// Remove reports success but a session survives: Residue must catch it.
	server.prov.osOps = &stickyResidue{fakeTunnelOS: &fakeTunnelOS{}}
	if _, err := server.prov.SetDesiredState(context.Background(), "m_res", DesiredRetired, "test"); !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("retire with residue = %v", err)
	}
}

type stickyResidue struct{ *fakeTunnelOS }

func (s *stickyResidue) Residue(context.Context, string) ([]string, error) {
	return []string{"processes still running"}, nil
}

func TestDisableFailsClosed(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{failDisable: true}
	insertRetireFixture(t, server, "m_dis", "dis-box", "rt-disbox11", 9208)
	admin, _ := SignJWT(cfg.JWTSecret, "u", "tester", "owner", time.Hour)
	rec := doJSON(t, server, http.MethodPost, "/api/admin/machines/m_dis/disable", "", admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disable with failing OS = %d %s", rec.Code, rec.Body.String())
	}
	var status string
	_ = store.db.QueryRow(`SELECT status FROM machines WHERE id='m_dis'`).Scan(&status)
	if status != DisableFailed {
		t.Fatalf("status = %s, want %s", status, DisableFailed)
	}
}

func TestUninstallCompleteOnlyCompletesAfterCleanup(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{failRemove: true}
	insertRetireFixture(t, server, "m_un", "un-box", "rt-unbox111", 9209)
	rec := doJSON(t, server, http.MethodPost, "/api/client/agent/uninstall-complete", `{"machine_id":"m_un","reason":"user"}`, "at_m_un")
	if rec.Code != http.StatusConflict {
		t.Fatalf("uninstall-complete = %d %s", rec.Code, rec.Body.String())
	}
	var cleanup string
	_ = store.db.QueryRow(`SELECT cleanup_state FROM machines WHERE id='m_un'`).Scan(&cleanup)
	if cleanup == CleanupComplete {
		t.Fatal("cleanup_state marked complete although hub cleanup failed")
	}
}

// Daybreak P2-5: once retired, the old agent token is dead.
func TestRetireRevokesAgentToken(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{}
	insertRetireFixture(t, server, "m_tok", "tok-box", "rt-tokbox11", 9210)
	if _, err := server.prov.SetDesiredState(context.Background(), "m_tok", DesiredRetired, "test"); err != nil {
		t.Fatal(err)
	}
	var hash *string
	_ = store.db.QueryRow(`SELECT agent_token_hash FROM machines WHERE id='m_tok'`).Scan(&hash)
	if hash != nil {
		t.Fatal("agent token hash survived retirement")
	}
	body := `{"machine_id":"m_tok","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{},"tunnel":{},"persistence":{},"keys":{}},"command_results":[{"command_id":"x","status":"acked"}]}`
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_m_tok"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("retired heartbeat = %d", rec.Code)
	}
}

func TestMaintenanceExpiresCommandsAndFlagsStuckRetiring(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.prov.osOps = &fakeTunnelOS{}
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,cleanup_state,desired_changed_at,created_at) VALUES ('m_stuck','stuck-box','stuck-box','retiring','retiring','gone','pending',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO agent_commands (id,machine_id,generation,type,status,created_at,expires_at) VALUES ('cmd_old','m_stuck',2,'uninstall','pending',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cmdStatus, cleanup, desired string
	_ = store.db.QueryRow(`SELECT status FROM agent_commands WHERE id='cmd_old'`).Scan(&cmdStatus)
	_ = store.db.QueryRow(`SELECT cleanup_state,desired_state FROM machines WHERE id='m_stuck'`).Scan(&cleanup, &desired)
	if cmdStatus != "expired" || cleanup != CleanupTimedOut || desired != DesiredRetiring {
		t.Fatalf("cmd=%s cleanup=%s desired=%s", cmdStatus, cleanup, desired)
	}
	var events int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='machine.retiring_timed_out'`).Scan(&events)
	if events != 1 {
		t.Fatalf("timed_out events = %d", events)
	}
}

func TestListMachinesHidesRetiredByDefault(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_live", "live-box")
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,created_at) VALUES ('m_dead','dead--retired-x','dead','retired','retired','gone',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	admin, _ := SignJWT(cfg.JWTSecret, "u", "tester", "owner", time.Hour)
	if body := doJSON(t, server, http.MethodGet, "/api/admin/machines", "", admin).Body.String(); strings.Contains(body, "m_dead") || !strings.Contains(body, "m_live") {
		t.Fatalf("default list: %s", body)
	}
	if body := doJSON(t, server, http.MethodGet, "/api/admin/machines?include_retired=1", "", admin).Body.String(); !strings.Contains(body, "m_dead") {
		t.Fatal("include_retired did not return retired machines")
	}
}

func TestAllocatorSkipsDropInPortsAndOtherHubs(t *testing.T) {
	_, store, _ := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`INSERT INTO hubs (id,name,public_host,ssh_port,port_range_start,port_range_end,created_at) VALUES ('jason','old','h',443,9200,9499,?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	insertTestMachine(t, store, "m_other", "other-hub")
	if _, err := store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_other','m_other','jason','rt-otherhub',9310,'k','offline',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	tx, _ := store.db.Begin()
	defer tx.Rollback()
	hub, _ := getHubTx(context.Background(), tx, "primary")
	hub.PortStart, hub.PortEnd = 9200, 9203
	got, err := allocatePortTx(context.Background(), tx, hub, map[int]bool{9200: true, 9201: true})
	if err != nil || got != 9202 {
		t.Fatalf("allocator = %d %v", got, err)
	}
	hub.PortStart, hub.PortEnd = 9310, 9310
	if _, err := allocatePortTx(context.Background(), tx, hub, nil); err == nil {
		t.Fatal("port used by another hub row was reallocated")
	}
}
