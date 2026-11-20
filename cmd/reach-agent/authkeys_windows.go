//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
)

// Windows has no target-user symlink attack of the same shape (ACLs are set
// explicitly by applyAuthorizedKeysPermissions), so plain file I/O is kept.
func readAuthorizedKeysFile(authFile string) ([]byte, error) {
	b, err := os.ReadFile(authFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func writeAuthorizedKeysFile(authFile, _ string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(authFile), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(authFile, content, 0o600)
}
