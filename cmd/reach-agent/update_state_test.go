package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Legacy flag files from an older agent still confirm and raise the floor.
func TestConfirmRaisesFloorFromLegacyFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "update-confirm-required"), []byte("backup=x\ncandidate_version=0.2.0\ncandidate_sequence=1790000042\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if f, _ := readReleaseFloor(dir); f != 1790000042 {
		t.Fatalf("floor after confirm = %d", f)
	}
}

func pendingUpdateFixture(t *testing.T) (dir, agent string) {
	t.Helper()
	dir = t.TempDir()
	agent = filepath.Join(dir, "agent")
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &updateState{State: updPending, Backup: backup, AgentPath: agent, CandidateVersion: "0.2.0", CandidateSequence: 1_790_000_000, WasActive: true, Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	return dir, agent
}

func agentBytes(t *testing.T, agent string) string {
	t.Helper()
	b, err := os.ReadFile(agent)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConfirmRaisesFloorFromState(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if f, _ := readReleaseFloor(dir); f != 1_790_000_000 {
		t.Fatalf("floor after confirm = %d", f)
	}
	if restored, err := rollbackIfUnconfirmed(dir); err != nil || restored {
		t.Fatalf("confirmed update rolled back: %v %v", restored, err)
	}
	if agentBytes(t, agent) != "candidate" {
		t.Fatal("binary changed")
	}
}

// Astra P2-03: a floor that cannot be recorded must not cancel rollback.
func TestConfirmKeepsRollbackArmedWhenFloorFails(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	if err := os.WriteFile(filepath.Join(dir, releaseFloorFile), []byte("corrupt-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err == nil {
		t.Fatal("confirm succeeded although the floor could not be recorded")
	}
	if st, _ := readUpdateState(dir); st == nil || st.State != updPending {
		t.Fatalf("state after failed confirm = %+v", st)
	}
	if restored, err := rollbackIfUnconfirmed(dir); err != nil || !restored || agentBytes(t, agent) != "old" {
		t.Fatalf("rollback after failed confirm: restored=%v err=%v", restored, err)
	}
}

// Astra R2-09 (first PoC): the watcher's deadline lands while a confirm
// holds the update, and the confirm then fails. The watcher must not read
// "being confirmed" as confirmed: it waits for the lock, sees the state is
// still pending, and rolls back.
func TestWatcherDoesNotTrustAnInProgressConfirm(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	if err := os.WriteFile(filepath.Join(dir, releaseFloorFile), []byte("corrupt-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	inConfirm := make(chan struct{})
	release := make(chan struct{})
	confirmErr := make(chan error, 1)
	go func() {
		// The confirmer holds the lock the whole time, as confirmUpdateState does.
		var err error
		_ = withUpdateLock(dir, func() error {
			close(inConfirm)
			<-release
			st, _ := readUpdateState(dir)
			st.State = updConfirming
			_ = writeUpdateState(dir, st)
			if err = raiseReleaseFloor(dir, st.CandidateSequence); err != nil {
				st.State = updPending
				_ = writeUpdateState(dir, st)
			}
			return nil
		})
		confirmErr <- err
	}()
	<-inConfirm
	watcherDone := make(chan error, 1)
	go func() {
		watcherDone <- runUpdateRollbackWatcher(context.Background(), updateBinaryOptions{RollbackAfter: time.Millisecond, dataDir: dir, agentPath: agent})
	}()
	select {
	case err := <-watcherDone:
		t.Fatalf("watcher decided while confirm held the update (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-confirmErr; err == nil {
		t.Fatal("confirm with a corrupt floor succeeded")
	}
	<-watcherDone // restart fails in the test (no service); the restore is what matters
	if agentBytes(t, agent) != "old" {
		t.Fatal("failed confirm left the candidate without a rollback")
	}
}

// Astra R2-09 (second PoC): a confirmer crashed mid-confirm. The durable
// state says "confirming", not confirmed; it is rolled back, and a late
// confirm cannot resurrect it.
func TestCrashedConfirmIsRolledBack(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.State = updConfirming
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	restored, err := rollbackIfUnconfirmed(dir)
	if err != nil || !restored || agentBytes(t, agent) != "old" {
		t.Fatalf("crashed confirm not rolled back: restored=%v err=%v", restored, err)
	}
	if st, _ := readUpdateState(dir); st.State != updRolledBack {
		t.Fatalf("state = %s", st.State)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err == nil {
		t.Fatal("confirm after rollback succeeded")
	}
	if f, _ := readReleaseFloor(dir); f != 0 {
		t.Fatalf("floor raised for a rolled-back update: %d", f)
	}
}

// Startup recovery: the watcher died and the deadline passed.
func TestStartupRollsBackOrphanedUpdate(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	resumeInterruptedUpdate(context.Background(), reachInstallState{DataDir: dir, AgentPath: agent}, t.Logf)
	if agentBytes(t, agent) != "old" {
		t.Fatal("orphaned update not rolled back at startup")
	}
}
