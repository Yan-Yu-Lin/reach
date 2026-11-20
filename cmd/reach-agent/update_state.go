package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Durable self-update state machine (astra R2-09, daybreak round 3 P2-E).
//
// The earlier design renamed a flag file to "claim" it; a watcher that found
// the flag missing assumed the update was confirmed, although a confirmer
// might only be holding it and about to fail. Now one JSON state file,
// written atomically and fsynced, says exactly where the update is, and
// every transition happens under an exclusive lock:
//
//	pending      new binary installed, waiting for proof of health
//	confirming   confirm started (floor being raised)
//	confirmed    floor durable; rollback cancelled
//	rolling_back watcher started restoring the backup
//	rolled_back  backup restored
//
// Only "confirmed" cancels a rollback. A crash in "confirming" or
// "rolling_back" is resumed: the watcher (or a re-armed one) rolls back.
// WasActive records whether the machine was meant to be online before the
// update, so the hub cannot lower the confirm gate afterwards.

const (
	updateStateFile = "update-state.json"
	updateLockFile  = "update.lock"

	updPending     = "pending"
	updConfirming  = "confirming"
	updConfirmed   = "confirmed"
	updRollingBack = "rolling_back"
	updRolledBack  = "rolled_back"
)

type updateState struct {
	State             string `json:"state"`
	Backup            string `json:"backup"`
	AgentPath         string `json:"agent_path"`
	InstallMode       string `json:"install_mode,omitempty"`
	ConfigPath        string `json:"config_path,omitempty"`
	CandidateVersion  string `json:"candidate_version"`
	CandidateSequence int64  `json:"candidate_sequence"`
	WasActive         bool   `json:"was_active"`
	InstalledAt       string `json:"installed_at"`
	Deadline          string `json:"deadline"`
	UpdatedAt         string `json:"updated_at"`
}

func (s *updateState) terminal() bool {
	return s == nil || s.State == updConfirmed || s.State == updRolledBack
}

func (s *updateState) deadline() time.Time {
	t, _ := time.Parse(time.RFC3339, s.Deadline)
	return t
}

func readUpdateState(dataDir string) (*updateState, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, updateStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
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

// withUpdateLock runs fn holding the exclusive update lock. The lock is an
// OS file lock, released automatically if the holder dies.
func withUpdateLock(dataDir string, fn func() error) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, updateLockFile), os.O_CREATE|os.O_RDWR, 0o600)
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

// confirmUpdateState: pending/confirming -> confirming -> (floor) ->
// confirmed. If the floor cannot be made durable the state goes back to
// pending and the rollback stays armed. handled=false means there is no
// state file (legacy flag or nothing pending).
func confirmUpdateState(dataDir string) (handled bool, err error) {
	err = withUpdateLock(dataDir, func() error {
		st, err := readUpdateState(dataDir)
		if err != nil {
			return err
		}
		if st == nil {
			return nil
		}
		handled = true
		switch st.State {
		case updConfirmed:
			return nil
		case updRollingBack, updRolledBack:
			return fmt.Errorf("update to %s is being or was rolled back; not confirming", st.CandidateVersion)
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

// rollbackIfUnconfirmed is the watcher's decision, under the lock: unless
// the state is confirmed, restore the backup. It returns true when it
// restored (the caller then restarts the agent). A crashed confirmer
// ("confirming") or rollback ("rolling_back") is rolled back.
func rollbackIfUnconfirmed(dataDir string) (bool, error) {
	restored := false
	err := withUpdateLock(dataDir, func() error {
		st, err := readUpdateState(dataDir)
		if err != nil {
			return err
		}
		if st.terminal() {
			return nil
		}
		st.State = updRollingBack
		if err := writeUpdateState(dataDir, st); err != nil {
			return err
		}
		if err := restoreBinary(st.Backup, st.AgentPath); err != nil {
			return fmt.Errorf("restore %s: %w", st.Backup, err)
		}
		st.State = updRolledBack
		if err := writeUpdateState(dataDir, st); err != nil {
			return err
		}
		restored = true
		return nil
	})
	return restored, err
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
