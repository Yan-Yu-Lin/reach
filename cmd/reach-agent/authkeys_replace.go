package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxAuthorizedKeysBytes bounds reads; larger files are refused rather than
// truncated (astra P2-05).
const maxAuthorizedKeysBytes = 4 << 20

// keyFileAccount says whose authorized_keys is being rewritten.
type keyFileAccount struct {
	// User is the account that logs in with this file.
	User string
	// Chown: on Unix, chown the new inode to User (root editing another
	// user's file).
	Chown bool
	// Strict: on Windows, a failure to set the DACL aborts the write (system
	// installs). Per-user installs treat it as best effort, as before.
	Strict bool
}

// aclApplier sets the final security descriptor on a not-yet-visible file.
type aclApplier func(path string) error

// replaceWithACL atomically replaces target with content such that the file
// is never visible with a weaker ACL than applyACL sets (astra P1-02):
//
//  1. create an empty temp file next to target (O_EXCL);
//  2. apply the final DACL to the empty temp file;
//  3. write and sync the content;
//  4. rename over target; the security descriptor travels with the file.
//
// Writing content only after the DACL is in place means even a crash
// mid-write leaves no readable key material under a broad inherited ACL.
// If applying the ACL fails and strict is set, target is left untouched.
func replaceWithACL(target string, content []byte, applyACL aclApplier, strict bool, warn func(string, ...any), beforeRename ...func() error) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.reach-tmp-%d", filepath.Base(target), time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if applyACL != nil {
		if err := applyACL(tmp); err != nil {
			if strict {
				return fmt.Errorf("set ACL on new authorized_keys: %w", err)
			}
			if warn != nil {
				warn("[reach warning] could not tighten %s ACL: %v\n", target, err)
			}
		}
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	for _, check := range beforeRename {
		if err := check(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	ok = true
	return nil
}
