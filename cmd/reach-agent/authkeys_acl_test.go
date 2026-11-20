package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Astra P1-02: the replacement must get its DACL before it holds content
// and before it replaces authorized_keys. Runs on any OS; the Windows icacls
// call itself is not exercised here (no Windows host in CI).
func TestReplaceAppliesACLToTempBeforeContentAndRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var aclPath string
	applied := func(p string) error {
		aclPath = p
		if p == target {
			t.Fatal("ACL applied to the live file, not the replacement")
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("ACL target missing: %v", err)
		}
		if st.Size() != 0 {
			t.Fatalf("content written before the ACL was set (%d bytes)", st.Size())
		}
		if b, _ := os.ReadFile(target); string(b) != "old\n" {
			t.Fatal("target replaced before ACL was set")
		}
		return nil
	}
	if err := replaceWithACL(target, []byte("new\n"), applied, true, nil); err != nil {
		t.Fatal(err)
	}
	if aclPath == "" || filepath.Dir(aclPath) != dir {
		t.Fatalf("ACL not applied to a sibling temp file: %q", aclPath)
	}
	if b, _ := os.ReadFile(target); string(b) != "new\n" {
		t.Fatalf("target = %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestReplaceStrictACLFailureLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := func(string) error { return errors.New("icacls: access denied") }
	if err := replaceWithACL(target, []byte("new\n"), fail, true, nil); err == nil {
		t.Fatal("strict ACL failure ignored")
	}
	if b, _ := os.ReadFile(target); string(b) != "old\n" {
		t.Fatal("target replaced despite ACL failure")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp file left: %v", entries)
	}
	var warned bool
	if err := replaceWithACL(target, []byte("new\n"), fail, false, func(string, ...any) { warned = true }); err != nil {
		t.Fatal(err)
	}
	if !warned {
		t.Fatal("best-effort ACL failure not reported")
	}
}

func TestAuthorizedKeysICACLSArgs(t *testing.T) {
	pd := `C:\ProgramData`
	admin := filepath.Join(pd, "ssh", "administrators_authorized_keys")
	got := authorizedKeysICACLSArgs("tmp1", admin, "alice", pd, "me")
	want := []string{"tmp1", "/inheritance:r", "/grant", "*S-1-5-32-544:F", "/grant", "*S-1-5-18:F"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admin file args = %v", got)
	}
	got = authorizedKeysICACLSArgs("tmp2", filepath.Join(`C:\Users\alice`, ".ssh", "authorized_keys"), "alice", pd, "me")
	if got[0] != "tmp2" || !strings.Contains(strings.Join(got, " "), "/grant alice:F") || got[1] != "/inheritance:r" {
		t.Fatalf("user file args = %v", got)
	}
	got = authorizedKeysICACLSArgs("tmp3", "C:/x/authorized_keys", "", pd, "me")
	if !strings.Contains(strings.Join(got, " "), "/grant me:F") {
		t.Fatalf("empty user should fall back to current user: %v", got)
	}
}
