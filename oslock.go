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

func lockDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "locks")
}

func openLockFile(dbPath, name string) (*os.File, error) {
	if !lockNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid lock name %q", name)
	}
	dir := lockDir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
