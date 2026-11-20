package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syncGoodKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func syncKeysFixture(t *testing.T) (*Daemon, string) {
	t.Helper()
	sshDir := filepath.Join(t.TempDir(), "home", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(sshDir, "authorized_keys")
	if err := os.WriteFile(auth, []byte("ssh-ed25519 AAAAuser user@laptop\n"+syncGoodKey+" reach:m_review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MachineID: "m_review"}
	cfg.LocalSSH.AuthFile = auth
	cfg.LocalSSH.InternalSSHD = true
	return NewDaemon(cfg), auth
}

// Astra R2-11: a desired list whose only key is invalid must not erase the
// last working Reach key, and must be reported as an error.
func TestSyncKeysKeepsFileWhenNoDesiredKeyIsValid(t *testing.T) {
	d, auth := syncKeysFixture(t)
	before, _ := os.ReadFile(auth)
	bad := syncGoodKey + " first\n" + syncGoodKey + " second"
	if err := d.syncKeys([]DesiredPubkey{{PublicKey: bad}}, true); !errors.Is(err, errNoValidDesiredKeys) {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(auth)
	if string(after) != string(before) {
		t.Fatalf("authorized_keys changed:\n%s", after)
	}
}

// An explicit disable and an empty desired list are real revocations.
func TestSyncKeysRevocationsStillRemoveReachKeys(t *testing.T) {
	for name, call := range map[string]func(*Daemon) error{
		"disable":   func(d *Daemon) error { return d.syncKeys([]DesiredPubkey{{PublicKey: syncGoodKey}}, false) },
		"empty set": func(d *Daemon) error { return d.syncKeys(nil, true) },
	} {
		t.Run(name, func(t *testing.T) {
			d, auth := syncKeysFixture(t)
			if err := call(d); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(auth)
			if strings.Contains(string(b), "reach:m_review") || !strings.Contains(string(b), "user@laptop") {
				t.Fatalf("after %s: %q", name, b)
			}
		})
	}
}

// A mix of valid and invalid keys installs the valid ones.
func TestSyncKeysInstallsValidSubset(t *testing.T) {
	d, auth := syncKeysFixture(t)
	if err := d.syncKeys([]DesiredPubkey{{PublicKey: "garbage"}, {PublicKey: syncGoodKey + " admin@mac"}}, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(auth)
	if strings.Count(string(b), "reach:m_review") != 1 {
		t.Fatalf("%q", b)
	}
}

// Astra R3-08: a desired list whose only entries are blank is not a
// revocation; the installed key stays and the sync reports an error.
func TestSyncKeysBlankDesiredKeysKeepFile(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		d, auth := syncKeysFixture(t)
		before, _ := os.ReadFile(auth)
		if err := d.syncKeys([]DesiredPubkey{{PublicKey: blank}}, true); !errors.Is(err, errNoValidDesiredKeys) {
			t.Fatalf("blank %q: err = %v", blank, err)
		}
		if after, _ := os.ReadFile(auth); string(after) != string(before) {
			t.Fatalf("blank %q changed authorized_keys", blank)
		}
	}
}
