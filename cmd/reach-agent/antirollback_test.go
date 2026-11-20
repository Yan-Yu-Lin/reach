package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Daybreak P2-6: a compromised hub offers an older, validly signed release.
func TestReleaseFloorRefusesDowngrade(t *testing.T) {
	dir := t.TempDir()
	newer := releaseManifest{Version: "0.2.0", Sequence: 1_790_000_000}
	older := releaseManifest{Version: "0.1.0", Sequence: 1_780_000_000}
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
	if f, _ := readReleaseFloor(dir); f != 1_790_000_000 {
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

// Astra P2-04: legacy created_at floors and explicit sequences share one
// ordering space; a small counter is rejected outright instead of being
// silently compared against a timestamp.
func TestSequenceSpaceIsUnixSeconds(t *testing.T) {
	dir := t.TempDir()
	legacy := releaseManifest{Version: "0.1.0", CreatedAt: "2026-07-01T00:00:00Z"}
	if err := raiseReleaseFloor(dir, manifestSequence(legacy)); err != nil {
		t.Fatal(err)
	}
	newer := releaseManifest{Version: "0.2.0", Sequence: 1_795_000_000, CreatedAt: "2026-11-21T00:00:00Z"}
	if err := checkReleaseFloor(dir, newer, false); err != nil {
		t.Fatalf("newer release with timestamp sequence refused: %v", err)
	}
	if manifestSequence(releaseManifest{Version: "x", Sequence: 1}) != 0 {
		t.Fatal("small counter accepted as a sequence")
	}
	if err := checkReleaseFloor(dir, releaseManifest{Version: "x", Sequence: 1}, false); err == nil {
		t.Fatal("small counter passed the floor check")
	}
}

// Daybreak P2-1: a fresh install (no floor yet) seeds the floor from its
// own embedded release on start, so an older signed release is refused.
func TestFreshInstallSeedsFloorFromEmbeddedRelease(t *testing.T) {
	dir := t.TempDir()
	old := releaseSequence
	releaseSequence = "1795000000"
	defer func() { releaseSequence = old }()
	cfg := defaultConfig()
	cfg.Install.DataDir = dir
	d := NewDaemon(cfg)
	d.seedReleaseFloor()
	if f, _ := readReleaseFloor(dir); f != 1795000000 {
		t.Fatalf("floor after first start = %d", f)
	}
	if err := checkReleaseFloor(dir, releaseManifest{Version: "old", Sequence: 1790000000}, false); err == nil {
		t.Fatal("older signed release accepted on a fresh install")
	}
	// While an update awaits confirmation the floor is not raised, so the
	// rollback to the previous binary stays possible.
	dir2 := t.TempDir()
	if err := writeUpdateState(dir2, &updateState{State: updPending}); err != nil {
		t.Fatal(err)
	}
	cfg.Install.DataDir = dir2
	NewDaemon(cfg).seedReleaseFloor()
	if f, _ := readReleaseFloor(dir2); f != 0 {
		t.Fatalf("floor raised during pending update: %d", f)
	}
	// Dev builds (no embedded sequence) do nothing.
	releaseSequence = ""
	dir3 := t.TempDir()
	cfg.Install.DataDir = dir3
	NewDaemon(cfg).seedReleaseFloor()
	if f, _ := readReleaseFloor(dir3); f != 0 {
		t.Fatalf("dev build seeded floor %d", f)
	}
}

// Daybreak P2-2: --candidate is copied privately; later changes to the
// original path do not affect what is verified and executed.
func TestCandidateIsCopiedPrivately(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "cand")
	if err := os.WriteFile(src, []byte("signed bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	cp, cleanup, err := privateCandidateCopy(src)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(src, []byte("swapped evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cp)
	if string(b) != "signed bytes" {
		t.Fatalf("private copy changed: %q", b)
	}
	st, _ := os.Stat(filepath.Dir(cp))
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("private dir mode %v", st.Mode().Perm())
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(src, link); err == nil {
		if _, _, err := privateCandidateCopy(link); err == nil {
			t.Fatal("symlink candidate accepted")
		}
	}
}

// Daybreak P3-3: a hub-chosen tiny rotate interval is clamped.
func TestProcessTitleIntervalClamped(t *testing.T) {
	got := normalizeProcessTitleSettings([]string{"a", "b"}, time.Nanosecond)
	if got.Interval < time.Second {
		t.Fatalf("interval %s not clamped", got.Interval)
	}
}
