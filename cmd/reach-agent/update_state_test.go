package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
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
	st := &updateState{State: updPending, Attempt: "a1", Backup: backup, BackupSHA256: sha256Hex("old"), AgentPath: agent, CandidateVersion: "0.2.0", CandidateSHA256: sha256Hex("candidate"), CandidateSequence: 1_790_000_000, WasActive: true, Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}
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
	if restored, err := rollbackIfUnconfirmed(dir, "a1"); err != nil || restored {
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
	if restored, err := rollbackIfUnconfirmed(dir, "a1"); err != nil || !restored || agentBytes(t, agent) != "old" {
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
		watcherDone <- runUpdateRollbackWatcher(context.Background(), updateBinaryOptions{RollbackAfter: time.Millisecond, dataDir: dir, agentPath: agent, attempt: "a1"})
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
	restored, err := rollbackIfUnconfirmed(dir, "a1")
	if err != nil || !restored || agentBytes(t, agent) != "old" {
		t.Fatalf("crashed confirm not rolled back: restored=%v err=%v", restored, err)
	}
	if st, _ := readUpdateState(dir); st.State != updRestartPending {
		t.Fatalf("state = %s", st.State)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err == nil {
		t.Fatal("confirm during rollback succeeded")
	}
	if err := markRestarted(dir, "a1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := readUpdateState(dir); st.State != updRolledBack {
		t.Fatalf("state after restart = %s", st.State)
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

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Astra R3-06: the floor reached disk, then the process died before the
// confirmed state. Recovery finishes the confirm instead of restoring the
// old binary under a raised floor.
func TestCrashAfterFloorWriteFinishesConfirm(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.State = updConfirming
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := raiseReleaseFloor(dir, st.CandidateSequence); err != nil {
		t.Fatal(err)
	}
	restored, err := rollbackIfUnconfirmed(dir, "a1")
	if err != nil || restored || agentBytes(t, agent) != "candidate" {
		t.Fatalf("rolled back past a durable floor: restored=%v err=%v", restored, err)
	}
	if st, _ := readUpdateState(dir); st.State != updConfirmed {
		t.Fatalf("state = %s", st.State)
	}
}

// Astra R3-07 A: a leftover terminal state must not hide a legacy flag.
func TestLegacyFlagWinsOverTerminalState(t *testing.T) {
	dir := t.TempDir()
	if err := writeUpdateState(dir, &updateState{State: updConfirmed, CandidateVersion: "prior"}); err != nil {
		t.Fatal(err)
	}
	flag := filepath.Join(dir, "update-confirm-required")
	if err := os.WriteFile(flag, []byte("candidate_sequence=1799000004\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(flag); err == nil {
		t.Fatal("legacy flag still pending after confirm")
	}
	if f, _ := readReleaseFloor(dir); f != 1799000004 {
		t.Fatalf("floor = %d", f)
	}
}

// Astra R3-07 B: a stale rolling_back state from an earlier attempt must
// not undo a later update (the binary at AgentPath is neither the recorded
// candidate nor the backup).
func TestStaleRollingBackDoesNotUndoLaterUpdate(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.State = updRollingBack
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("future-candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	resumeInterruptedUpdate(context.Background(), reachInstallState{DataDir: dir, AgentPath: agent}, t.Logf)
	if agentBytes(t, agent) != "future-candidate" {
		t.Fatal("stale rollback state undid a later update")
	}
	if st, _ := readUpdateState(dir); st.State != updRolledBack {
		t.Fatalf("stale state not retired: %s", st.State)
	}
}

// The restore completed but the state still says rolling_back: finish as
// restart_pending without restoring again.
func TestCompletedRestoreIsNotReplayed(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.State = updRollingBack
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	need, err := rollbackIfUnconfirmed(dir, "a1")
	if err != nil || !need {
		t.Fatalf("need restart=%v err=%v", need, err)
	}
	if st, _ := readUpdateState(dir); st.State != updRestartPending {
		t.Fatalf("state = %s", st.State)
	}
}

// Daybreak round 4 P2-04: a symlinked state file is refused, and a
// group-writable DataDir is refused.
func TestUpdateStateRefusesSymlinksAndLooseDataDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"state":"pending","agent_path":"/etc/passwd"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, updateStateFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := readUpdateState(dir); err == nil {
		t.Fatal("symlinked update state was read")
	}
	loose := t.TempDir()
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := withUpdateLock(loose, func() error { return nil }); err == nil {
		t.Fatal("world-writable DataDir accepted")
	}
}

// Daybreak round 4 P2-04: two update workers cannot both start an update.
func TestUpdateStartIsSingleFlight(t *testing.T) {
	dir := t.TempDir()
	if err := writeUpdateState(dir, &updateState{State: updConfirmed}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withUpdateLock(dir, func() error {
				prev, err := readUpdateState(dir)
				if err != nil || !prev.terminal() {
					return os.ErrExist
				}
				return writeUpdateState(dir, &updateState{State: updPending})
			})
			if err == nil {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if started != 1 {
		t.Fatalf("%d workers started an update", started)
	}
}

// Astra R4-05 / daybreak round 5 N2: a watcher left over from attempt A
// does not roll back attempt B.
func TestOldWatcherLeavesNewAttemptAlone(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.Attempt = "attempt-b"
	st.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := runUpdateRollbackWatcher(context.Background(), updateBinaryOptions{RollbackAfter: time.Millisecond, dataDir: dir, agentPath: agent, attempt: "attempt-a"}); err != nil {
		t.Fatal(err)
	}
	if agentBytes(t, agent) != "candidate" {
		t.Fatal("attempt A's watcher rolled back attempt B")
	}
	if st, _ := readUpdateState(dir); st.State != updPending {
		t.Fatalf("state = %s", st.State)
	}
}

// Astra R4-06: the backup is created exclusively; an existing file (from a
// second worker, or planted) is never overwritten or followed.
func TestBackupIsCreatedExclusively(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent")
	_ = os.WriteFile(src, []byte("running"), 0o755)
	dst := filepath.Join(dir, "backup")
	_ = os.WriteFile(dst, []byte("winner-backup"), 0o755)
	if err := copyFileExclusive(src, dst, 0o755); err == nil {
		t.Fatal("existing backup overwritten")
	}
	if b, _ := os.ReadFile(dst); string(b) != "winner-backup" {
		t.Fatal("backup content changed")
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(filepath.Join(dir, "elsewhere"), link)
	if err := copyFileExclusive(src, link, 0o755); err == nil {
		t.Fatal("backup written through a symlink")
	}
}

// Astra R4-07: restart_pending is finished by the process running the
// backup image, compared by image hash, even when the candidate and the
// backup carry the same version string.
func TestRestartPendingFinishedByRunningImage(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	running, err := runningImageSHA256()
	if err != nil {
		t.Skip("cannot hash the running image here")
	}
	st, _ := readUpdateState(dir)
	st.State = updRestartPending
	st.BackupSHA256 = running
	st.CandidateVersion = version // same version string as this binary
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	resumeInterruptedUpdate(context.Background(), reachInstallState{DataDir: dir, AgentPath: agent}, t.Logf)
	if st, _ := readUpdateState(dir); st.State != updRolledBack {
		t.Fatalf("restart_pending not finished by the restored image: %s", st.State)
	}
}

// Astra R4-11: a state without identities is never replayed.
func TestHashlessStateIsNotReplayed(t *testing.T) {
	dir, agent := pendingUpdateFixture(t)
	st, _ := readUpdateState(dir)
	st.State = updRollingBack
	st.CandidateSHA256, st.BackupSHA256 = "", ""
	if err := writeUpdateState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("future-candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := rollbackIfUnconfirmed(dir, "a1"); err != nil {
		t.Fatal(err)
	}
	if agentBytes(t, agent) != "future-candidate" {
		t.Fatal("hashless state replayed a restore")
	}
}

// Daybreak round 5 N10: a symlinked DataDir is refused before any read.
func TestSymlinkedDataDirNotRead(t *testing.T) {
	real := t.TempDir()
	if err := writeUpdateState(real, &updateState{State: updPending, Attempt: "x"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if st, err := readUpdateState(link); err == nil && st != nil {
		t.Fatal("state read through a symlinked DataDir")
	}
}
