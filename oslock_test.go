//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordingAccountOS tracks hub accounts like the real OS would: Create adds
// one, Remove deletes it, Residue reports it.
type accountState struct {
	mu       sync.Mutex
	accounts map[string]bool
}

type accountOS struct {
	fakeTunnelOS
	st *accountState
}

func (o *accountOS) Remove(_ context.Context, u string) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	delete(o.st.accounts, u)
	return nil
}
func (o *accountOS) Residue(_ context.Context, u string) ([]string, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	if o.st.accounts[u] {
		return []string{"account"}, nil
	}
	return nil, nil
}

// Astra R3-01: two independent instances (separate Provisioners, like
// reachd and the reachd CLI) share one DB. Instance A pauses inside the OS
// phase of a provision; instance B retires the machine. The retire must
// wait for A's OS phase, and the hub must end with no account for a
// retired machine.
func TestIndependentRetireWaitsForProvisionOSPhase(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.ProvisioningEnabled = false
	cfg.InitialAdmin.PasswordHash, _ = HashSecret("pw")
	if err := store.Migrate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	st := &accountState{accounts: map[string]bool{}}
	creator := NewProvisioner(store, cfg)
	creator.osOps = &accountOS{st: st}
	entered := make(chan string, 1)
	proceed := make(chan struct{})
	creator.createAccount = func(_ context.Context, u string, _ int, _, _ string) error {
		entered <- u
		<-proceed
		st.mu.Lock()
		st.accounts[u] = true
		st.mu.Unlock()
		return nil
	}
	// A second store handle, as another process would open.
	store2, err := OpenStore(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	retiree := NewProvisioner(store2, cfg)
	retiree.osOps = &accountOS{st: st}

	createErr := make(chan error, 1)
	go func() {
		_, err := creator.CreateMachine(context.Background(), CreateMachineInput{Slug: "late-os-box", Pubkey: testPubkey})
		createErr <- err
	}()
	user := <-entered
	var machineID string
	if err := store.db.QueryRow(`SELECT id FROM machines WHERE slug='late-os-box'`).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	retireErr := make(chan error, 1)
	go func() {
		_, err := retiree.SetDesiredState(context.Background(), machineID, DesiredRetired, "independent-cli")
		retireErr <- err
	}()
	select {
	case err := <-retireErr:
		t.Fatalf("retire ran while another instance was in the provision OS phase (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(proceed)
	if err := <-createErr; err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := <-retireErr; err != nil {
		t.Fatalf("retire after provision: %v", err)
	}
	st.mu.Lock()
	left := st.accounts[user]
	st.mu.Unlock()
	var desired, tstatus string
	_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id=?`, machineID).Scan(&desired)
	_ = store.db.QueryRow(`SELECT status FROM tunnels WHERE machine_id=?`, machineID).Scan(&tstatus)
	if desired != DesiredRetired || tstatus != "retired" || left {
		t.Fatalf("desired=%s tunnel=%s hub account left=%v", desired, tstatus, left)
	}
}

// The writers lock: a deployment's exclusive hold makes CLI writers fail
// fast, and a running writer makes the deployment's lock fail.
func TestWritersLock(t *testing.T) {
	db := filepath.Join(t.TempDir(), "reach.db")
	release, err := acquireWritersShared(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWritersExclusive(db); err == nil {
		t.Fatal("exclusive lock taken while a writer runs")
	}
	release()
	f, err := acquireWritersExclusive(db)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := acquireWritersShared(db); err == nil {
		t.Fatal("writer started during a deployment")
	}
}

// The lock is held across processes: a child process holding the machine
// lock blocks this one.
func TestMachineLockAcrossProcesses(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) not installed")
	}
	db := filepath.Join(t.TempDir(), "reach.db")
	if err := os.MkdirAll(lockDir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	lf := filepath.Join(lockDir(db), "machine-m_x.lock")
	if err := os.WriteFile(lf, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("flock", "-x", lf, "sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	unlock, err := lockMachineOS(db, "m_x")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	_ = cmd.Wait()
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("machine lock did not wait for the other process")
	}
}
