//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
)

// Windows has no target-user symlink attack of the same shape; the risk is
// the security descriptor. Every rewrite goes through replaceWithACL, which
// sets the final DACL on the temp file before content is written and before
// it replaces authorized_keys (astra P1-02). Not yet exercised on a real
// Windows host; see docs/review/2026-11-21-hardening-backend.md.
func readAuthorizedKeysFile(authFile string) ([]byte, error) {
	f, err := os.Open(authFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, maxAuthorizedKeysBytes+1)
	n, err := ioReadFull(f, b)
	if err != nil {
		return nil, err
	}
	if n > maxAuthorizedKeysBytes {
		return nil, fmt.Errorf("refusing %s: larger than %d bytes", authFile, maxAuthorizedKeysBytes)
	}
	return b[:n], nil
}

func writeAuthorizedKeysFile(authFile string, acct keyFileAccount, content []byte) error {
	return replaceWithACL(authFile, content, func(path string) error {
		return setAuthorizedKeysACL(path, authFile, acct.User)
	}, acct.Strict, func(format string, a ...any) { fmt.Printf(format, a...) })
}
