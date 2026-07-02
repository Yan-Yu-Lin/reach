package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"aead.dev/minisign"
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

func TestAgentAssetForOSArchDarwin(t *testing.T) {
	tests := map[string]string{
		"x86_64":  "reach-agent_darwin_amd64",
		"amd64":   "reach-agent_darwin_amd64",
		"aarch64": "reach-agent_darwin_arm64",
		"arm64":   "reach-agent_darwin_arm64",
	}
	for arch, want := range tests {
		got, err := agentAssetForOSArch("darwin", arch)
		if err != nil {
			t.Fatalf("agentAssetForOSArch(darwin, %q) returned error: %v", arch, err)
		}
		if got != want {
			t.Fatalf("agentAssetForOSArch(darwin, %q)=%q want %q", arch, got, want)
		}
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

func TestParseReleaseManifest(t *testing.T) {
	sum := sha256.Sum256([]byte("agent"))
	manifest := releaseManifest{
		Schema:    1,
		Project:   releaseManifestProject,
		Version:   "1.2.3",
		CreatedAt: "2026-07-03T00:00:00Z",
		Assets: map[string]releaseAsset{
			"reach-agent_linux_amd64": {SHA256: hex.EncodeToString(sum[:]), Size: 5},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	gotManifest, gotAsset, err := parseReleaseManifest(raw, "1.2.3", "reach-agent_linux_amd64")
	if err != nil {
		t.Fatalf("parseReleaseManifest returned error: %v", err)
	}
	if gotManifest.Version != "1.2.3" || gotAsset.Size != 5 {
		t.Fatalf("unexpected manifest=%+v asset=%+v", gotManifest, gotAsset)
	}
}

func TestParseReleaseManifestRejectsWrongVersion(t *testing.T) {
	sum := sha256.Sum256([]byte("agent"))
	manifest := releaseManifest{Schema: 1, Project: releaseManifestProject, Version: "1.2.3", Assets: map[string]releaseAsset{"reach-agent_linux_amd64": {SHA256: hex.EncodeToString(sum[:]), Size: 5}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseReleaseManifest(raw, "1.2.4", "reach-agent_linux_amd64"); err == nil {
		t.Fatalf("expected version mismatch error")
	}
}

func TestVerifyReleaseManifestSignature(t *testing.T) {
	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyText := publicKey.String()
	oldKeys := releaseManifestPublicKeys
	releaseManifestPublicKeys = []string{publicKeyText}
	t.Cleanup(func() { releaseManifestPublicKeys = oldKeys })

	manifest := []byte(`{"schema":1,"project":"reach-agent","version":"1.2.3","assets":{}}`)
	sig := minisign.SignWithComments(privateKey, manifest, "reach-agent test manifest", "untrusted comment: test")
	if err := verifyReleaseManifestSignature(manifest, sig); err != nil {
		t.Fatalf("verifyReleaseManifestSignature returned error: %v", err)
	}
	if err := verifyReleaseManifestSignature([]byte(`{"schema":1}`), sig); err == nil {
		t.Fatalf("expected signature failure for tampered manifest")
	}
}

func TestStripUpdateWorkerOnlyArgs(t *testing.T) {
	got := stripUpdateWorkerOnlyArgs([]string{"--foreground", "--install-env", "/etc/reach/install.env", "--version", "1.2.3", "--install-env=/tmp/nope", "--api-url", "https://example.test"})
	want := []string{"--version", "1.2.3", "--api-url", "https://example.test"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
