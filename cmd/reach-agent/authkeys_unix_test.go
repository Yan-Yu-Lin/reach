//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeHome(t *testing.T) (root, authFile string) {
	t.Helper()
	root = t.TempDir()
	home := filepath.Join(root, "home", "alice")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(home, ".ssh", "authorized_keys")
}

// PoC from daybreak P2-3: the target user replaces authorized_keys with a
// symlink to a root-owned file; installAdminKeys used to write through it.
func TestInstallAdminKeysRefusesAuthorizedKeysSymlink(t *testing.T) {
	root, authFile := fakeHome(t)
	sink := filepath.Join(root, "root-owned-sink")
	if err := os.WriteFile(sink, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sink, authFile); err != nil {
		t.Fatal(err)
	}
	prov := provisionResponse{Machine: machineInfo{ID: "m_test"}, AdminPubkeys: []adminPubkey{{PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl op"}}}
	if err := installAdminKeys(authFile, "", prov, false, []string{"ssh-ed25519"}, false); err == nil {
		t.Fatal("installAdminKeys wrote through a symlink")
	}
	b, _ := os.ReadFile(sink)
	if string(b) != "original\n" {
		t.Fatalf("sink modified: %q", b)
	}
}

func TestSyncKeysRefusesSymlinkedSSHDir(t *testing.T) {
	root, authFile := fakeHome(t)
	sshDir := filepath.Dir(authFile)
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sshDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sshDir); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.MachineID = "m_test"
	cfg.LocalSSH.AuthFile = authFile
	d := NewDaemon(cfg)
	err := d.syncKeys([]DesiredPubkey{{PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl op"}}, true)
	if err == nil {
		t.Fatal("syncKeys followed a symlinked .ssh directory")
	}
	if _, err := os.Stat(filepath.Join(target, "authorized_keys")); err == nil {
		t.Fatal("file created through symlinked .ssh")
	}
}

func TestAuthorizedKeysWriterKeepsOtherLinesAndCreatesDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "bob")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(home, ".ssh", "authorized_keys")
	if err := writeAuthorizedKeysFile(authFile, keyFileAccount{}, []byte("ssh-ed25519 AAAA keep\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Dir(authFile))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v %v", st, err)
	}
	b, err := readAuthorizedKeysFile(authFile)
	if err != nil || !strings.Contains(string(b), "keep") {
		t.Fatalf("read back %q %v", b, err)
	}
	if fi, _ := os.Stat(authFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(authFile))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestHeartbeatIntervalIsClampedAndApplied(t *testing.T) {
	d := NewDaemon(defaultConfig())
	d.setHeartbeatInterval(time.Millisecond)
	if d.heartbeatInterval() != minHeartbeatInterval {
		t.Fatalf("interval = %s", d.heartbeatInterval())
	}
	d.setHeartbeatInterval(90 * time.Second)
	if d.heartbeatInterval() != 90*time.Second {
		t.Fatalf("interval = %s", d.heartbeatInterval())
	}
	d.setHeartbeatInterval(24 * time.Hour)
	if d.heartbeatInterval() != maxHeartbeatInterval {
		t.Fatalf("interval = %s", d.heartbeatInterval())
	}
}

// Astra P2-05: an authorized_keys larger than the read limit must fail and
// stay untouched, not be truncated and written back.
func TestOversizeAuthorizedKeysIsRefusedNotTruncated(t *testing.T) {
	_, auth := fakeHome(t)
	suffix := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl unrelated-key\n"
	content := "#" + strings.Repeat("a", maxAuthorizedKeysBytes-2) + "\n" + suffix
	if err := os.WriteFile(auth, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.MachineID = "m_review"
	cfg.LocalSSH.AuthFile = auth
	d := NewDaemon(cfg)
	if err := d.syncKeys(nil, false); err == nil {
		t.Fatal("syncKeys accepted an oversized authorized_keys")
	}
	after, err := os.ReadFile(auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Fatalf("oversized file was modified: %d -> %d bytes", len(content), len(after))
	}
	// Exactly at the limit is still fine.
	if err := os.WriteFile(auth, []byte(strings.Repeat("#", maxAuthorizedKeysBytes-1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAuthorizedKeysFile(auth); err != nil {
		t.Fatalf("file at the limit refused: %v", err)
	}
}
