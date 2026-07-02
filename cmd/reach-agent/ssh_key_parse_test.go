package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseTunnelPrivateKeyExplicitECDSA(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "explicit-p256.pem")
	cmd := exec.Command("openssl", "ecparam", "-name", "prime256v1", "-genkey", "-param_enc", "explicit", "-out", keyPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("openssl cannot generate explicit EC key: %v: %s", err, out)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := parseTunnelPrivateKey(key)
	if err != nil {
		t.Fatalf("parse explicit EC key: %v", err)
	}
	if got, want := signer.PublicKey().Type(), "ecdsa-sha2-nistp256"; got != want {
		t.Fatalf("public key type = %q, want %q", got, want)
	}
}
