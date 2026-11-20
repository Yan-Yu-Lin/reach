package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Daybreak P2-6: a compromised hub offers an older, validly signed release.
func TestReleaseFloorRefusesDowngrade(t *testing.T) {
	dir := t.TempDir()
	newer := releaseManifest{Version: "0.2.0", Sequence: 2000}
	older := releaseManifest{Version: "0.1.0", Sequence: 1000}
	if err := checkReleaseFloor(dir, older, false); err != nil {
		t.Fatalf("fresh install refused: %v", err)
	}
	if err := raiseReleaseFloor(dir, manifestSequence(newer)); err != nil {
		t.Fatal(err)
	}
	if err := checkReleaseFloor(dir, older, false); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade accepted: %v", err)
	}
	if err := checkReleaseFloor(dir, older, true); err != nil {
		t.Fatalf("local break-glass refused: %v", err)
	}
	if err := checkReleaseFloor(dir, newer, false); err != nil {
		t.Fatalf("same release refused: %v", err)
	}
	// The floor never moves backwards.
	if err := raiseReleaseFloor(dir, 10); err != nil {
		t.Fatal(err)
	}
	if f, _ := readReleaseFloor(dir); f != 2000 {
		t.Fatalf("floor = %d", f)
	}
}

func TestLegacyManifestUsesCreatedAt(t *testing.T) {
	m := releaseManifest{Version: "0.1.0", CreatedAt: "2026-07-01T00:00:00Z"}
	if manifestSequence(m) <= 0 {
		t.Fatal("legacy manifest has no sequence")
	}
	if err := checkReleaseFloor(t.TempDir(), releaseManifest{Version: "x"}, false); err == nil {
		t.Fatal("manifest without sequence or created_at accepted")
	}
}

func TestConfirmRaisesFloorFromFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "update-confirm-required"), []byte("backup=x\ncandidate_version=0.2.0\ncandidate_sequence=4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := confirmUpdate(reachInstallState{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if f, _ := readReleaseFloor(dir); f != 4242 {
		t.Fatalf("floor after confirm = %d", f)
	}
}
