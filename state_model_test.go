package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRecomputeObservedKeepsReachableStaleHeartbeatDegraded(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.InitialAdmin.PasswordHash, err = HashSecret("test-password")
	if err != nil {
		t.Fatal(err)
	}
	cfg.OfflineAfter = 5 * time.Minute
	if err := store.Migrate(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO machines (id,slug,status,desired_state,observed_state,created_at) VALUES ('m_stale','stale-box','online','active','online',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_stale','m_stale','primary','rt-stalebox',9201,'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake test','online',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_observations (machine_id,heartbeat_at,tunnel_state,local_ssh_state) VALUES ('m_stale',?,'connected','healthy')`, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO hub_observations (tunnel_id,machine_id,probe_state,last_probe_at,last_success_at) VALUES ('t_stale','m_stale','reachable',?,?)`, nowUTC(), nowUTC()); err != nil {
		t.Fatal(err)
	}

	p := &Provisioner{store: store, cfg: cfg}
	if _, _, _, err := p.recomputeObserved(ctx, "m_stale"); err != nil {
		t.Fatal(err)
	}
	var observed, status string
	if err := store.db.QueryRowContext(ctx, `SELECT observed_state,status FROM machines WHERE id='m_stale'`).Scan(&observed, &status); err != nil {
		t.Fatal(err)
	}
	if observed != ObservedDegraded || status != ObservedDegraded {
		t.Fatalf("reachable stale heartbeat should be degraded, got observed=%q status=%q", observed, status)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE hub_observations SET probe_state='unreachable', last_probe_at=? WHERE tunnel_id='t_stale'`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := p.recomputeObserved(ctx, "m_stale"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT observed_state,status FROM machines WHERE id='m_stale'`).Scan(&observed, &status); err != nil {
		t.Fatal(err)
	}
	if observed != ObservedGone || status != ObservedGone {
		t.Fatalf("unreachable stale heartbeat should be gone, got observed=%q status=%q", observed, status)
	}
}

func TestRetiredTunnelsReleaseUniqueFields(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.InitialAdmin.PasswordHash, err = HashSecret("test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,created_at) VALUES ('m_old','box--retired-old1','box','retired','retired','gone',?)`, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_old','m_old','primary','rt-abcdefgh',9200,'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake test','retired',?)`, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.releaseRetiredTunnels(ctx); err != nil {
		t.Fatal(err)
	}
	var unixUser, originalUnix string
	var port, originalPort int
	if err := store.db.QueryRowContext(ctx, `SELECT unix_user,original_unix_user,remote_port,original_remote_port FROM tunnels WHERE id='t_old'`).Scan(&unixUser, &originalUnix, &port, &originalPort); err != nil {
		t.Fatal(err)
	}
	if unixUser == originalUnix || port == originalPort || port >= 0 {
		t.Fatalf("retired tunnel did not release unique fields: unix=%q original=%q port=%d original=%d", unixUser, originalUnix, port, originalPort)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,created_at) VALUES ('m_new','box','box','offline','active','offline',?)`, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_new','m_new','primary','rt-abcdefgh',9200,'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake test','offline',?)`, nowUTC()); err != nil {
		t.Fatalf("new tunnel could not reuse retired tunnel unique fields: %v", err)
	}
}
