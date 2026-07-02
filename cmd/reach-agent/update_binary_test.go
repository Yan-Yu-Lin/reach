package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAgentAssetForArch(t *testing.T) {
	tests := map[string]string{
		"x86_64":  "reach-agent_linux_amd64",
		"amd64":   "reach-agent_linux_amd64",
		"aarch64": "reach-agent_linux_arm64",
		"arm64":   "reach-agent_linux_arm64",
		"armv7l":  "reach-agent_linux_armv7",
		"armv6l":  "reach-agent_linux_armv6",
		"i686":    "reach-agent_linux_386",
	}
	for arch, want := range tests {
		got, err := agentAssetForArch(arch)
		if err != nil {
			t.Fatalf("agentAssetForArch(%q) returned error: %v", arch, err)
		}
		if got != want {
			t.Fatalf("agentAssetForArch(%q)=%q want %q", arch, got, want)
		}
	}
	if _, err := agentAssetForArch("mips64"); err == nil {
		t.Fatalf("expected unsupported arch error")
	}
}

func TestChecksumForAsset(t *testing.T) {
	sum := sha256.Sum256([]byte("agent"))
	hexSum := hex.EncodeToString(sum[:])
	raw := []byte("deadbeef  other\n" + hexSum + "  reach-agent_linux_amd64\n")
	got, err := checksumForAsset(raw, "reach-agent_linux_amd64")
	if err != nil {
		t.Fatalf("checksumForAsset returned error: %v", err)
	}
	if got != hexSum {
		t.Fatalf("checksumForAsset=%q want %q", got, hexSum)
	}
}

func TestChecksumForAssetStarName(t *testing.T) {
	hexSum := strings.Repeat("a", 64)
	got, err := checksumForAsset([]byte(hexSum+" *reach-agent_linux_arm64\n"), "reach-agent_linux_arm64")
	if err != nil {
		t.Fatalf("checksumForAsset returned error: %v", err)
	}
	if got != hexSum {
		t.Fatalf("checksumForAsset=%q want %q", got, hexSum)
	}
}

func TestChecksumForAssetRejectsInvalidHash(t *testing.T) {
	if _, err := checksumForAsset([]byte("nothex  reach-agent_linux_amd64\n"), "reach-agent_linux_amd64"); err == nil {
		t.Fatalf("expected invalid hash error")
	}
}

func TestStripUpdateWorkerOnlyArgs(t *testing.T) {
	got := stripUpdateWorkerOnlyArgs([]string{"--foreground", "--install-env", "/etc/reach/install.env", "--version", "1.2.3", "--install-env=/tmp/nope", "--api-url", "https://example.test"})
	want := []string{"--version", "1.2.3", "--api-url", "https://example.test"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
