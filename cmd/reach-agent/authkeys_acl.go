package main

import (
	"path/filepath"
	"strings"
)

// authorizedKeysICACLSArgs returns the icacls arguments that give path the
// DACL Windows OpenSSH requires for target:
//   - administrators_authorized_keys: Administrators + SYSTEM only;
//   - a user's authorized_keys: that user + SYSTEM + Administrators.
//
// Inheritance is removed so a broad parent ACL never applies.
// Pure function so it is testable off Windows.
func authorizedKeysICACLSArgs(path, target, user, programData, currentUser string) []string {
	if programData == "" {
		programData = `C:\ProgramData`
	}
	adminFile := filepath.Join(programData, "ssh", "administrators_authorized_keys")
	if strings.EqualFold(filepath.Clean(target), filepath.Clean(adminFile)) {
		return []string{path, "/inheritance:r", "/grant", "*S-1-5-32-544:F", "/grant", "*S-1-5-18:F"}
	}
	if user == "" {
		user = currentUser
	}
	return []string{path, "/inheritance:r", "/grant", user + ":F", "/grant", "*S-1-5-18:F", "/grant", "*S-1-5-32-544:F"}
}
