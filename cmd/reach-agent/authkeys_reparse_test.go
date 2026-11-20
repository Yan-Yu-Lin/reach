package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Daybreak P2-9: the Windows writer must refuse a junction/symlink in any
// user-controlled component. The link predicate is injected so the walk is
// tested on every OS; real symlinks are used where the OS allows.
func TestCheckNoLinksRefusesUserControlledLinks(t *testing.T) {
	path := filepath.Join("C:", "Users", "alice", ".ssh", "authorized_keys")
	for _, linked := range []string{
		path,
		filepath.Dir(path),
		filepath.Dir(filepath.Dir(path)),
	} {
		isLink := func(p string) (bool, error) { return p == linked, nil }
		if err := checkNoLinks(path, 3, isLink); err == nil {
			t.Fatalf("link at %s not refused", linked)
		}
	}
	// Above the user-controlled part (e.g. C:\Users) is not checked.
	isLink := func(p string) (bool, error) { return p == filepath.Join("C:", "Users"), nil }
	if err := checkNoLinks(path, 3, isLink); err != nil {
		t.Fatalf("trusted parent link refused: %v", err)
	}
	notExist := func(string) (bool, error) { return false, os.ErrNotExist }
	if err := checkNoLinks(path, 3, notExist); err != nil {
		t.Fatalf("missing components refused: %v", err)
	}
}

func TestCheckNoLinksWithRealSymlink(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	other := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(home, ".ssh")); err != nil {
		t.Skip("symlinks unavailable")
	}
	err := checkNoLinks(filepath.Join(home, ".ssh", "authorized_keys"), 3, isLinkOrReparse)
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("symlinked .ssh not refused: %v", err)
	}
}

func TestReplaceRunsBeforeRenameCheck(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := replaceWithACL(target, []byte("new\n"), nil, true, nil, func() error { return os.ErrPermission })
	if err == nil {
		t.Fatal("beforeRename failure ignored")
	}
	if b, _ := os.ReadFile(target); string(b) != "old\n" {
		t.Fatal("target replaced although the pre-rename check failed")
	}
}
