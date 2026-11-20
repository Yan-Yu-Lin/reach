package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// blockingOS lets a test pause DisableAuth mid-transition and records the
// resulting auth state of each tunnel user, like the real authorized_keys.
type blockingOS struct {
	fakeTunnelOS
	mu           sync.Mutex
	enabled      map[string]bool
	disableEnter chan struct{}
	disableGo    chan struct{}
}

func newBlockingOS() *blockingOS {
	return &blockingOS{enabled: map[string]bool{}, disableEnter: make(chan struct{}, 4), disableGo: make(chan struct{})}
}

func (b *blockingOS) DisableAuth(_ context.Context, u string) error {
	b.disableEnter <- struct{}{}
	<-b.disableGo
	b.mu.Lock()
	b.enabled[u] = false
	b.mu.Unlock()
	return nil
}

func (b *blockingOS) EnableAuth(_ context.Context, u string) error {
	b.mu.Lock()
	b.enabled[u] = true
	b.mu.Unlock()
	return nil
}

func (b *blockingOS) auth(u string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled[u]
}

// Daybreak P1-1 PoC: uninstall-intent's DisableAuth is paused, admin enable
// runs meanwhile. Before the fix the DB ended "retiring" with auth enabled.
// With per-machine serialisation the enable waits; the final DB state and
// the hub auth state must agree.
func TestEnableRacingUninstallKeepsDBAndAuthConsistent(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	bos := newBlockingOS()
	server.prov.osOps = bos
	insertRetireFixture(t, server, "m_race", "race-box", "rt-racebox1", 9260)
	if _, err := store.db.Exec(`UPDATE machines SET desired_state='disabled',status='disabled'`); err != nil {
		t.Fatal(err)
	}
	admin, _ := adminTestToken(t, store, cfg)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		doJSON(t, server, http.MethodPost, "/api/client/agent/uninstall-intent", `{"machine_id":"m_race","reason":"race"}`, "at_m_race")
	}()
	<-bos.disableEnter // uninstall is inside DisableAuth
	enableDone := make(chan int, 1)
	go func() {
		defer wg.Done()
		enableDone <- doJSON(t, server, http.MethodPost, "/api/admin/machines/m_race/enable", "", admin).Code
	}()
	select {
	case <-enableDone:
		t.Fatal("enable ran while another transition was mid-flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(bos.disableGo)
	wg.Wait()
	var desired string
	_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_race'`).Scan(&desired)
	authOn := bos.auth("rt-racebox1")
	if (desired == DesiredActive) != authOn {
		t.Fatalf("DB desired_state=%s but hub auth enabled=%v", desired, authOn)
	}
}

func TestEnableRacingDisableKeepsDBAndAuthConsistent(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	bos := newBlockingOS()
	server.prov.osOps = bos
	insertRetireFixture(t, server, "m_race2", "race2-box", "rt-racebox2", 9261)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = server.prov.SetDesiredState(context.Background(), "m_race2", DesiredDisabled, "a")
	}()
	<-bos.disableEnter
	go func() {
		defer wg.Done()
		_, _ = server.prov.SetDesiredState(context.Background(), "m_race2", DesiredActive, "b")
	}()
	time.Sleep(100 * time.Millisecond)
	close(bos.disableGo)
	wg.Wait()
	var desired string
	_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_race2'`).Scan(&desired)
	if (desired == DesiredActive) != bos.auth("rt-racebox2") {
		t.Fatalf("DB desired_state=%s but hub auth enabled=%v", desired, bos.auth("rt-racebox2"))
	}
}

// The OS enable succeeded but recording it failed: access must be revoked
// again (compensation), and the call must report failure.
type enableThenBreakDB struct {
	fakeTunnelOS
	store   *Store
	enabled map[string]bool
	mu      sync.Mutex
}

func (e *enableThenBreakDB) EnableAuth(_ context.Context, u string) error {
	e.mu.Lock()
	e.enabled[u] = true
	e.mu.Unlock()
	// Simulate a concurrent writer bumping the generation (or a lost DB
	// write): the CAS that records the enable will not match.
	_, err := e.store.db.Exec(`UPDATE machines SET desired_generation=desired_generation+100`)
	return err
}

func (e *enableThenBreakDB) DisableAuth(_ context.Context, u string) error {
	e.mu.Lock()
	e.enabled[u] = false
	e.mu.Unlock()
	return nil
}

func TestEnableCompensatesWhenRecordFails(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	eos := &enableThenBreakDB{store: store, enabled: map[string]bool{}}
	server.prov.osOps = eos
	insertRetireFixture(t, server, "m_comp", "comp-box", "rt-compbox1", 9262)
	if _, err := store.db.Exec(`UPDATE machines SET desired_state='disabled',status='disabled'`); err != nil {
		t.Fatal(err)
	}
	_, err := server.prov.SetDesiredState(context.Background(), "m_comp", DesiredActive, "a")
	if !errors.Is(err, errConcurrentTransition) {
		t.Fatalf("enable error = %v", err)
	}
	if eos.enabled["rt-compbox1"] {
		t.Fatal("hub auth left enabled although the enable was not recorded")
	}
	var desired string
	_ = store.db.QueryRow(`SELECT desired_state FROM machines WHERE id='m_comp'`).Scan(&desired)
	if desired != DesiredDisabled {
		t.Fatalf("desired_state = %s", desired)
	}
}

// pauseRemoveOS pauses retire inside Remove.
type pauseRemoveOS struct {
	fakeTunnelOS
	entered chan struct{}
	proceed chan struct{}
}

func (o *pauseRemoveOS) Remove(ctx context.Context, u string) error {
	o.entered <- struct{}{}
	<-o.proceed
	return o.fakeTunnelOS.Remove(ctx, u)
}

// Astra R2-04: a process-title change during a paused retire must not let
// the retire commit retired tunnels under a machine that stays active.
// The title change waits for the machine lock; either way, machine and
// tunnel rows must agree.
func TestProcessTitleCannotSplitRetire(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_pt", "pt-box", "rt-ptbox111", 9280)
	ops := &pauseRemoveOS{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	server.prov.osOps = ops
	retireErr := make(chan error, 1)
	go func() {
		_, err := server.prov.SetDesiredState(context.Background(), "m_pt", DesiredRetired, "retirer")
		retireErr <- err
	}()
	<-ops.entered
	titleErr := make(chan error, 1)
	go func() {
		_, err := server.prov.SetProcessTitleConfig(context.Background(), "m_pt", &ProcessTitleConfig{ProcessTitle: "x"}, "editor")
		titleErr <- err
	}()
	select {
	case err := <-titleErr:
		t.Fatalf("process-title ran while retire held the machine (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(ops.proceed)
	if err := <-retireErr; err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := <-titleErr; err == nil {
		t.Fatal("process-title on a retired machine succeeded")
	}
	assertMachineTunnelAgree(t, store, "m_pt")
}

// Cross-process writers do not share the in-process lock: if the
// generation changes under a retire (e.g. reachd CLI), the finalize must
// roll back as a whole.
type bumpOnRemoveOS struct {
	fakeTunnelOS
	store *Store
}

func (o *bumpOnRemoveOS) Remove(ctx context.Context, u string) error {
	_, err := o.store.db.Exec(`UPDATE machines SET desired_generation=desired_generation+1`)
	if err != nil {
		return err
	}
	return o.fakeTunnelOS.Remove(ctx, u)
}

func TestRetireFinalizeIsAtomicAgainstOtherWriters(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_x", "x-box", "rt-xbox1111", 9281)
	server.prov.osOps = &bumpOnRemoveOS{store: store}
	_, err := server.prov.SetDesiredState(context.Background(), "m_x", DesiredRetired, "retirer")
	if !errors.Is(err, errConcurrentTransition) {
		t.Fatalf("retire error = %v", err)
	}
	assertMachineTunnelAgree(t, store, "m_x")
}

func assertMachineTunnelAgree(t *testing.T, store *Store, id string) {
	t.Helper()
	var desired, mstatus string
	var token sql.NullString
	if err := store.db.QueryRow(`SELECT desired_state,status,agent_token_hash FROM machines WHERE id=?`, id).Scan(&desired, &mstatus, &token); err != nil {
		t.Fatal(err)
	}
	var tstatus string
	var port int
	if err := store.db.QueryRow(`SELECT status,remote_port FROM tunnels WHERE machine_id=?`, id).Scan(&tstatus, &port); err != nil {
		t.Fatal(err)
	}
	machineRetired := desired == DesiredRetired && mstatus == "retired" && !token.Valid
	tunnelRetired := tstatus == "retired" && port < 0
	if machineRetired != tunnelRetired {
		t.Fatalf("split state: desired=%s machine=%s token=%v tunnel=%s port=%d", desired, mstatus, token.Valid, tstatus, port)
	}
}

// Astra R2 note: a failed compensation must be visible, not only logged.
type compFailOS struct {
	fakeTunnelOS
	calls int
}

func (o *compFailOS) EnableAuth(context.Context, string) error {
	o.calls++
	if o.calls == 2 {
		return errors.New("second enable failed")
	}
	return nil
}
func (o *compFailOS) DisableAuth(context.Context, string) error {
	return errors.New("compensation failed")
}

func TestFailedCompensationIsRecorded(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertRetireFixture(t, server, "m_c", "c-box", "rt-cbox1111", 9282)
	if _, err := store.db.Exec(`INSERT INTO tunnels(id,machine_id,hub_id,unix_user,original_unix_user,remote_port,original_remote_port,tunnel_pubkey,status,created_at) VALUES ('t_c2','m_c','primary','rt-cbox2222','rt-cbox2222',9283,9283,'ssh-ed25519 x','disabled',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE machines SET desired_state='disabled',status='disabled' WHERE id='m_c'`); err != nil {
		t.Fatal(err)
	}
	server.prov.osOps = &compFailOS{}
	if _, err := server.prov.SetDesiredState(context.Background(), "m_c", DesiredActive, "t"); !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("enable error = %v", err)
	}
	var status string
	_ = store.db.QueryRow(`SELECT status FROM machines WHERE id='m_c'`).Scan(&status)
	if status != DisableFailed {
		t.Fatalf("status after failed compensation = %s", status)
	}
}
