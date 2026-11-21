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
//	                 clear retired tokens -> fence -> snapshot ->
//	                 rotate jwt_secret (if the session migration is still
//	                 pending, checked under the same lock)
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
	// Decide and rotate under the same lock: no CLI can run the session
	// migration in between (daybreak round 5 N6).
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	applied, err := sessionMigrationApplied(ctx, store.db)
	_ = store.Close()
	if err != nil {
		return err
	}
	if applied {
		fmt.Println("deploy-prepare: fence and snapshot written; session migration already applied, jwt_secret unchanged")
		return nil
	}
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fencePath == "" || *snapshotPath == "" || *forensicDir == "" {
		return errors.New("--fence, --snapshot and --forensic are required")
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return restoreUnderLock(ctx, cfg.DBPath, *fencePath, *snapshotPath, *forensicDir, time.Now())
}

func restoreUnderLock(ctx context.Context, dbPath, fencePath, snapshotPath, forensicDir string, now time.Time) error {
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
