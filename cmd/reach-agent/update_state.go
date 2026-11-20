package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Durable self-update state machine (astra R2-09, R3-06, R3-07; daybreak
// round 3 P2-E, round 4 P2-04).
//
// One JSON state file, written atomically and fsynced, says where the
// update is; every check and transition happens under one OS file lock:
//
//	pending         new binary installed, waiting for proof of health
//	confirming      confirm started (floor being raised)
//	confirmed       floor durable; rollback cancelled
//	rolling_back    watcher started restoring the backup
//	restart_pending backup restored, agent not yet restarted on it
//	rolled_back     backup restored and the agent restarted
//
// Each update has an Attempt ID and records the sha256 of the candidate
// and of the backup. Recovery acts only on the attempt that matches what
// is on disk:
//
//   - confirming with the floor already at CandidateSequence: the confirm
//     commit was done, finish it as confirmed (never roll back past a
//     durable floor) (R3-06)
//   - rolling_back with AgentPath already equal to the backup: finish as
//     restart_pending; with AgentPath equal to neither the candidate nor
//     the backup: a later update replaced it, the stale state is retired
//     without touching the binary (R3-07)
//   - a legacy flag file (older agent) always takes precedence over an
//     old terminal state (R3-07)
//
// The state file and lock live in DataDir, which must be a real directory
// owned by this user and not writable by group/others; the state file is
// opened without following symlinks (daybreak P2-04).

const (
	updateStateFile = "update-state.json"
	updateLockFile  = "update.lock"

	updPending        = "pending"
	updConfirming     = "confirming"
	updConfirmed      = "confirmed"
	updRollingBack    = "rolling_back"
	updRestartPending = "restart_pending"
	updRolledBack     = "rolled_back"
)

type updateState struct {
	State             string `json:"state"`
	Attempt           string `json:"attempt,omitempty"`
	Backup            string `json:"backup"`
	BackupSHA256      string `json:"backup_sha256,omitempty"`
	AgentPath         string `json:"agent_path"`
	InstallMode       string `json:"install_mode,omitempty"`
	ConfigPath        string `json:"config_path,omitempty"`
	CandidateVersion  string `json:"candidate_version"`
	CandidateSHA256   string `json:"candidate_sha256,omitempty"`
	CandidateSequence int64  `json:"candidate_sequence"`
	// FloorBefore is the anti-rollback floor when the update started.
	FloorBefore int64  `json:"floor_before"`
	WasActive   bool   `json:"was_active"`
	InstalledAt string `json:"installed_at"`
	Deadline    string `json:"deadline"`
	UpdatedAt   string `json:"updated_at"`
}

// terminal: nothing left to do for this attempt. restart_pending is not
// terminal: the agent still has to be restarted on the restored binary.
func (s *updateState) terminal() bool {
	return s == nil || s.State == updConfirmed || s.State == updRolledBack
}

func (s *updateState) deadline() time.Time {
	t, _ := time.Parse(time.RFC3339, s.Deadline)
	return t
}

// checkDataDir refuses a DataDir that another user could have prepared:
// it must be a real directory (not a symlink), owned by us, and not
// group/other-writable (daybreak round 4 P2-04).
func checkDataDir(dataDir string) error {
	st, err := os.Lstat(dataDir)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("%s is not a real directory", dataDir)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %o)", dataDir, st.Mode().Perm())
	}
	return checkOwnedByMe(st)
}

func readUpdateState(dataDir string) (*updateState, error) {
	f, err := openNoFollow(filepath.Join(dataDir, updateStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", updateStateFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	var st updateState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", updateStateFile, err)
	}
	return &st, nil
}

// writeUpdateState replaces the state file durably: temp file fsynced,
// renamed, directory fsynced.
func writeUpdateState(dataDir string, st *updateState) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, updateStateFile)
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dataDir)
}

// withUpdateLock runs fn holding the exclusive update lock, after checking
// DataDir. The lock is an OS file lock, released if the holder dies.
func withUpdateLock(dataDir string, fn func() error) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := checkDataDir(dataDir); err != nil {
		return fmt.Errorf("update data dir: %w", err)
	}
	f, err := openLockNoFollow(filepath.Join(dataDir, updateLockFile))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return err
	}
	defer unlockFile(f)
	return fn()
}

func fileMatches(path, sha string) bool {
	if sha == "" {
		return false
	}
	got, err := fileSHA256Hex(path)
	return err == nil && got == sha
}

// confirmUpdateState: pending/confirming -> confirming -> (floor) ->
// confirmed. If the floor cannot be made durable the state goes back to
// pending and the rollback stays armed. handled=false means there is no
// active state for this protocol (no file, or a terminal one).
func confirmUpdateState(dataDir string) (handled bool, err error) {
	err = withUpdateLock(dataDir, func() error {
		st, err := readUpdateState(dataDir)
		if err != nil {
			return err
		}
		if st.terminal() {
			return nil
		}
		handled = true
		switch st.State {
		case updRollingBack, updRestartPending:
			return fmt.Errorf("update to %s is being rolled back; not confirming", st.CandidateVersion)
		}
		if st.CandidateSHA256 != "" && !fileMatches(st.AgentPath, st.CandidateSHA256) {
			return fmt.Errorf("installed binary is not the %s candidate; not confirming", st.CandidateVersion)
		}
		st.State = updConfirming
		if err := writeUpdateState(dataDir, st); err != nil {
			return err
		}
		if st.CandidateSequence > 0 {
			if err := raiseReleaseFloor(dataDir, st.CandidateSequence); err != nil {
				st.State = updPending
				if werr := writeUpdateState(dataDir, st); werr != nil {
					return fmt.Errorf("record release floor: %v; additionally could not reset state: %w", err, werr)
				}
				return fmt.Errorf("record release floor: %w", err)
			}
		}
		st.State = updConfirmed
		return writeUpdateState(dataDir, st)
	})
	return handled, err
}

// rollbackIfUnconfirmed is the watcher's decision, under the lock. It
// returns true when the agent must be restarted on the restored binary
// (the caller restarts it and then calls markRestarted).
func rollbackIfUnconfirmed(dataDir string) (bool, error) {
	needRestart := false
	err := withUpdateLock(dataDir, func() error {
		st, err := readUpdateState(dataDir)
		if err != nil {
			return err
		}
		if st.terminal() {
			return nil
		}
		switch st.State {
		case updRestartPending:
			needRestart = true
			return nil
		case updConfirming:
			// A crash after the floor became durable: the confirm commit
			// happened, finish it instead of rolling back (R3-06).
			if floor, err := readReleaseFloor(dataDir); err == nil && st.CandidateSequence > 0 && floor >= st.CandidateSequence && floor > st.FloorBefore {
				st.State = updConfirmed
				return writeUpdateState(dataDir, st)
			}
		case updRollingBack:
			if fileMatches(st.AgentPath, st.BackupSHA256) {
				// The restore completed before a crash.
				st.State = updRestartPending
				needRestart = true
				return writeUpdateState(dataDir, st)
			}
		}
		// Only restore over the binary this attempt installed. If the path
		// now holds something else (a later update by an older agent), this
		// state is stale: retire it, leave the binary alone (R3-07).
		if st.CandidateSHA256 != "" && !fileMatches(st.AgentPath, st.CandidateSHA256) {
			st.State = updRolledBack
			return writeUpdateState(dataDir, st)
		}
		if st.BackupSHA256 != "" && !fileMatches(st.Backup, st.BackupSHA256) {
			return fmt.Errorf("backup %s does not match the recorded hash; not restoring", st.Backup)
		}
		st.State = updRollingBack
		if err := writeUpdateState(dataDir, st); err != nil {
			return err
		}
		if err := restoreBinary(st.Backup, st.AgentPath); err != nil {
			return fmt.Errorf("restore %s: %w", st.Backup, err)
		}
		st.State = updRestartPending
		if err := writeUpdateState(dataDir, st); err != nil {
			return err
		}
		needRestart = true
		return nil
	})
	return needRestart, err
}

// markRestarted ends a rollback once the agent runs the restored binary.
func markRestarted(dataDir string) error {
	return withUpdateLock(dataDir, func() error {
		st, err := readUpdateState(dataDir)
		if err != nil || st == nil || st.State != updRestartPending {
			return err
		}
		st.State = updRolledBack
		return writeUpdateState(dataDir, st)
	})
}

// updatePending reports an update that still awaits confirmation (state
// file or legacy flag).
func updatePending(dataDir string) bool {
	if _, err := os.Stat(filepath.Join(dataDir, "update-confirm-required")); err == nil {
		return true
	}
	st, err := readUpdateState(dataDir)
	if err != nil {
		return true // unreadable: assume pending, never raise the floor
	}
	return !st.terminal()
}
