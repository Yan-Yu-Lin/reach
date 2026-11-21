package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Cross-process locks (astra R3-01, daybreak round 4 P1-02).
//
// The in-process machine mutex only serialises one Provisioner. reachd, the
// reachd CLI and maintenance in another instance all write the same SQLite
// DB and the same hub OS state (tunnel users, authorized_keys, sshd
// drop-ins). Lock files next to the DB give every process that opens it the
// same locks:
//
//	<dbdir>/locks/machine-<id>.lock  exclusive for a machine's whole
//	                                 lifecycle step, DB read through OS
//	                                 phase to the final DB write
//	<dbdir>/locks/writers.lock       shared by every process that may
//	                                 mutate (reachd serve, CLI create/
//	                                 disable/remove); exclusive for the
//	                                 deploy while it snapshots and fences
//
// flock locks are released by the kernel when the holder dies, so a crash
// never leaves a stale lock.

var lockNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

// canonicalDBPath resolves dbPath to the absolute path of the real file
// (symlinks resolved), so every process that opens the same database uses
// the same SQLite file AND the same lock directory (astra R4-03). A path
// whose final component is a symlink is refused: it is an alias that one
// process could resolve and another not.
func canonicalDBPath(dbPath string) (string, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	if st, err := os.Lstat(abs); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("database path %s is a symlink; configure the real file path", abs)
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

func lockDir(dbPath string) string {
	if c, err := canonicalDBPath(dbPath); err == nil {
		dbPath = c
	}
	return filepath.Join(filepath.Dir(dbPath), "locks")
}

func openLockFile(dbPath, name string) (*os.File, error) {
	if !lockNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid lock name %q", name)
	}
	if _, err := canonicalDBPath(dbPath); err != nil {
		return nil, err
	}
	return openLockFileSafe(lockDir(dbPath), name+".lock")
}

// errWritersLocked: a deployment holds the writers lock.
var errWritersLocked = errors.New("a Reach deployment is in progress (writers lock held); try again after it finishes")

// acquireWritersShared takes the writers lock in shared mode without
// waiting. reachd holds it for its lifetime; CLI mutations for their run.
func acquireWritersShared(dbPath string) (func(), error) {
	f, err := openLockFile(dbPath, "writers")
	if err != nil {
		return nil, err
	}
	ok, err := tryFlock(f, false)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !ok {
		_ = f.Close()
		return nil, errWritersLocked
	}
	return func() { _ = unflock(f); _ = f.Close() }, nil
}

// acquireWritersExclusive takes the writers lock exclusively without
// waiting; it fails while reachd or any CLI writer runs.
func acquireWritersExclusive(dbPath string) (*os.File, error) {
	f, err := openLockFile(dbPath, "writers")
	if err != nil {
		return nil, err
	}
	ok, err := tryFlock(f, true)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !ok {
		_ = f.Close()
		return nil, errors.New("another process holds the Reach writers lock (reachd or a CLI writer still running)")
	}
	return f, nil
}

// lockMachineOS takes the cross-process lock of one machine (blocking).
func lockMachineOS(dbPath, machineID string) (func(), error) {
	f, err := openLockFile(dbPath, "machine-"+machineID)
	if err != nil {
		return nil, err
	}
	if err := flockBlocking(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = unflock(f); _ = f.Close() }, nil
}
