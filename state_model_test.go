package main

import (
	"context"
	"path/filepath"
	"testing"
)

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
