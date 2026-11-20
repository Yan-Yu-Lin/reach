package main

import (
	"context"
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
