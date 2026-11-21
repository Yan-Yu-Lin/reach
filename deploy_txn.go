package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Locked deploy transactions (astra R4-01, daybreak round 5 N6).
//
// The writers lock is a flock held by one process; a shell cannot hold it
// across child commands. So each critical section is ONE reachd process
// that takes the lock exclusively and does every step itself:
//
//	deploy-prepare   (reachd verifiably stopped)
//	                 provisioning freeze check -> clear retired tokens ->
//	                 fence -> snapshot -> rotate jwt_secret (always)
//	deploy-restore   (reachd verifiably stopped)
//	                 forensic copy -> fence check -> snapshot restore
//
// While either runs, reachd and every CLI writer (which hold the lock
// shared) cannot start, and the command fails if one is running. A writer
// that slips in after deploy-prepare and before the new reachd starts is
// above the fence, so a later restore refuses; after deploy-restore, a
// writer works on the restored DB and is not overwritten.

// fenceMaxAge: older fences may depend on journal rows that maintenance
// has pruned (journalRetention), so they are refused (daybreak N9).
const fenceMaxAge = 30 * 24 * time.Hour

func cliDeployPrepare(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("deploy-prepare", args)
	fencePath := fs.String("fence", "", "fence file to write")
	snapshotPath := fs.String("snapshot", "", "snapshot file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fencePath == "" || *snapshotPath == "" {
		return errors.New("--fence and --snapshot are required")
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	lock, err := acquireWritersExclusive(cfg.DBPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := recordFenceAndSnapshotLocked(ctx, cfg.DBPath, *fencePath, *snapshotPath); err != nil {
		return err
	}
	// Rotate jwt_secret on every deploy, unconditionally, while reachd is
	// stopped and the lock is held (daybreak round 6 R6-P1-03). Whether
	// the DB already carries the session migration says nothing about
	// the secret: a newer CLI run against the live DB can create the
	// marker with the secret unchanged, and a rollback to the old binary
	// would then accept every pre-deploy JWT again. config.yaml is not
	// part of a rollback, so the old binary verifies with the new secret.
	// Cost: every admin logs in again after each deploy.
	if err := rotateJWTSecret(*cfgPath); err != nil {
		return fmt.Errorf("rotate jwt_secret: %w", err)
	}
	fmt.Println("deploy-prepare: fence and snapshot written; jwt_secret rotated (every admin must log in again)")
	return nil
}

// errRestoreRefused: the restore was not done; the live DB is untouched.
var errRestoreRefused = errors.New("snapshot restore refused")

func cliDeployRestore(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("deploy-restore", args)
	fencePath := fs.String("fence", "", "fence recorded by deploy-prepare")
	snapshotPath := fs.String("snapshot", "", "snapshot written by deploy-prepare")
	forensicDir := fs.String("forensic", "", "directory to create for the failed database files")
	h := handoffFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fencePath == "" || *snapshotPath == "" || *forensicDir == "" {
		return errors.New("--fence, --snapshot and --forensic are required")
	}
	if err := h.validate(); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return restoreUnderLockWithHandoff(ctx, cfg.DBPath, *fencePath, *snapshotPath, *forensicDir, time.Now(), h)
}

// binaryHandoff: what to put at the reachd binary path before the writers
// lock is released (astra R5-01). Exactly one of rollback/remove is set.
type binaryHandoff struct {
	target   *string
	rollback *string
	remove   *bool
}

func handoffFlags(fs interface {
	String(string, string, string) *string
	Bool(string, bool, string) *bool
}) binaryHandoff {
	return binaryHandoff{
		target:   fs.String("binary-target", "", "reachd binary path to hand back before unlocking"),
		rollback: fs.String("rollback-binary", "", "previous reachd binary to install at --binary-target"),
		remove:   fs.Bool("remove-binary", false, "fresh install: move the new binary away instead"),
	}
}

func (h binaryHandoff) set() bool { return h.target != nil && *h.target != "" }

func (h binaryHandoff) validate() error {
	if !h.set() {
		if (h.rollback != nil && *h.rollback != "") || (h.remove != nil && *h.remove) {
			return errors.New("--rollback-binary/--remove-binary need --binary-target")
		}
		return nil
	}
	if (*h.rollback == "") == !*h.remove {
		return errors.New("give exactly one of --rollback-binary or --remove-binary")
	}
	return nil
}

// apply puts the previous binary (or nothing) at the target: a verified
// copy, fsync, atomic rename, hash check. Runs while the caller holds the
// writers lock, so no process can start the new binary on the restored
// database in between.
func (h binaryHandoff) apply() error {
	target := *h.target
	if *h.remove {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := os.Rename(target, target+".failed."+time.Now().UTC().Format("20060102T150405Z")); err != nil {
			return fmt.Errorf("move new binary away: %w", err)
		}
		return syncDirPath(filepath.Dir(target))
	}
	cand := target + ".rollback-candidate"
	_ = os.Remove(cand)
	if err := copyFileExact(*h.rollback, cand, 0o755); err != nil {
		return fmt.Errorf("stage previous binary: %w", err)
	}
	if err := os.Chmod(cand, 0o755); err != nil {
		return err
	}
	if err := os.Rename(cand, target); err != nil {
		return fmt.Errorf("install previous binary: %w", err)
	}
	if err := syncDirPath(filepath.Dir(target)); err != nil {
		return err
	}
	if same, err := sameContent(*h.rollback, target); err != nil || !same {
		return fmt.Errorf("installed binary does not match the rollback copy: %v", err)
	}
	return nil
}

// cliDeployHandoff: reachd deploy-handoff --config C --binary-target T
// (--rollback-binary R | --remove-binary). Binary handoff alone, under the
// writers lock, for rollbacks that did not touch the database.
func cliDeployHandoff(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("deploy-handoff", args)
	h := handoffFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !h.set() {
		return errors.New("--binary-target is required")
	}
	if err := h.validate(); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	lock, err := acquireWritersExclusive(cfg.DBPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := h.apply(); err != nil {
		return err
	}
	fmt.Println("deploy-handoff: previous binary in place")
	return nil
}

func restoreUnderLockWithHandoff(ctx context.Context, dbPath, fencePath, snapshotPath, forensicDir string, now time.Time, h binaryHandoff) error {
	return restoreUnderLockHook(ctx, dbPath, fencePath, snapshotPath, forensicDir, now, func() error {
		if !h.set() {
			return nil
		}
		if err := h.apply(); err != nil {
			return fmt.Errorf("database restored, but the binary handoff failed: %w", err)
		}
		fmt.Println("deploy-restore: previous binary in place")
		return nil
	})
}

func restoreUnderLock(ctx context.Context, dbPath, fencePath, snapshotPath, forensicDir string, now time.Time) error {
	return restoreUnderLockHook(ctx, dbPath, fencePath, snapshotPath, forensicDir, now, nil)
}

// restoreUnderLockHook: afterRestore runs after the DB restore, still
// holding the writers lock.
func restoreUnderLockHook(ctx context.Context, dbPath, fencePath, snapshotPath, forensicDir string, now time.Time, afterRestore func() error) error {
	dbPath, err := canonicalDBPath(dbPath)
	if err != nil {
		return err
	}
	lock, err := acquireWritersExclusive(dbPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errRestoreRefused, err)
	}
	defer lock.Close()
	testPauseWithLock()

	// 1. Preserve the failed set (main, WAL, SHM) before anything else.
	if err := preserveFailedDatabase(dbPath, forensicDir); err != nil {
		return fmt.Errorf("%w: forensic copy: %v", errRestoreRefused, err)
	}
	// 2. Fence check on the live DB.
	b, err := os.ReadFile(fencePath)
	if err != nil {
		return fmt.Errorf("%w: %v", errRestoreRefused, err)
	}
	var f deployFence
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%w: fence: %v", errRestoreRefused, err)
	}
	if t, err := time.Parse(time.RFC3339, f.RecordedAt); err != nil || now.Sub(t) > fenceMaxAge {
		return fmt.Errorf("%w: fence is missing a time or older than %s", errRestoreRefused, fenceMaxAge)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errRestoreRefused, err)
	}
	writes, err := durableWritesSince(ctx, store.db, f)
	// Checkpoint so the main file holds everything, then close: no
	// handle of ours stays on the file we replace.
	_, _ = store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	_ = store.Close()
	if err != nil {
		return fmt.Errorf("%w: fence check: %v", errRestoreRefused, err)
	}
	if len(writes) > 0 {
		for _, w := range writes {
			fmt.Fprintln(os.Stderr, "  "+w)
		}
		return fmt.Errorf("%w: %d durable action(s) after the fence", errRestoreRefused, len(writes))
	}
	// 3. Restore: verified copy of the snapshot, remove sidecars, rename.
	cand := dbPath + ".rollback-candidate"
	if err := copyFileExact(snapshotPath, cand, 0o600); err != nil {
		return fmt.Errorf("%w: %v", errRestoreRefused, err)
	}
	if err := quickCheck(ctx, cand); err != nil {
		_ = os.Remove(cand)
		return fmt.Errorf("%w: snapshot quick_check: %v", errRestoreRefused, err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w (live DB untouched)", sfx, err)
		}
	}
	if err := os.Rename(cand, dbPath); err != nil {
		return fmt.Errorf("rename snapshot into place: %w", err)
	}
	if err := syncDirPath(filepath.Dir(dbPath)); err != nil {
		return err
	}
	same, err := sameContent(snapshotPath, dbPath)
	if err != nil || !same {
		return fmt.Errorf("restored database does not match the snapshot: %v", err)
	}
	fmt.Println("deploy-restore: pre-deploy snapshot restored")
	if afterRestore != nil {
		return afterRestore()
	}
	return nil
}

func preserveFailedDatabase(dbPath, dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	stage := dir + ".staging"
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Lstat(f); errors.Is(err, os.ErrNotExist) {
			continue
		}
		dst := filepath.Join(stage, filepath.Base(f))
		if err := copyFileExact(f, dst, 0o600); err != nil {
			return err
		}
		if same, err := sameContent(f, dst); err != nil || !same {
			return fmt.Errorf("copy of %s does not match", f)
		}
	}
	if st, err := os.Stat(filepath.Join(stage, filepath.Base(dbPath))); err != nil || st.Size() == 0 {
		return fmt.Errorf("failed main database was not preserved")
	}
	if err := os.Rename(stage, dir); err != nil {
		return err
	}
	return syncDirPath(filepath.Dir(dir))
}

func copyFileExact(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sameContent(a, b string) (bool, error) {
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func quickCheck(ctx context.Context, path string) error {
	db, err := openPlainSQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("%s", res)
	}
	return nil
}

// testPauseWithLock lets the container rollback test hold a locked restore
// still while it runs a CLI writer (scenario L). Only active when
// REACH_TEST_LOCK_PAUSE names a directory; production never sets it.
func testPauseWithLock() {
	dir := os.Getenv("REACH_TEST_LOCK_PAUSE")
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "holding"), nil, 0o600)
	for i := 0; i < 600; i++ {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
