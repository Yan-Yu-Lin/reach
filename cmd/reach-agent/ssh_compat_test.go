package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParseOpenSSHVersionAndLegacyOptions(t *testing.T) {
	v := parseOpenSSHVersion("OpenSSH_5.3")
	if !v.Recognized || v.HasED25519 || !v.LikelyOld53 {
		t.Fatalf("unexpected parsed version: %+v", v)
	}
	opts := clientOptionsForServerBanner("SSH-2.0-OpenSSH_5.3", "")
	if len(opts) == 0 {
		t.Fatal("expected legacy client options for OpenSSH 5.3 server")
	}
	want := "HostKeyAlgorithms=ssh-rsa"
	for _, got := range opts {
		if got == want {
			return
		}
	}
	t.Fatalf("missing %q in %#v", want, opts)
}

func TestModernOpenSSHPrefersED25519(t *testing.T) {
	v := parseOpenSSHVersion("OpenSSH_10.2p1, LibreSSL 3.3.6")
	if !v.Recognized || !v.HasED25519 || v.LikelyOld53 {
		t.Fatalf("unexpected parsed version: %+v", v)
	}
	if got := chooseKeyType([]string{"ssh-rsa", "ssh-ed25519"}); got != "ssh-ed25519" {
		t.Fatalf("chooseKeyType = %q", got)
	}
}

func TestTunnelKeyTypeIndependentOfLocalKeygenSupport(t *testing.T) {
	compat := detectSSHCompatibility(context.Background())
	if compat.TunnelKeyType != "ssh-ed25519" {
		t.Fatalf("TunnelKeyType = %q, want ssh-ed25519", compat.TunnelKeyType)
	}
}

func TestGenerateSSHKeyPureGoSupportedTypes(t *testing.T) {
	for _, tc := range []struct {
		keyType string
		wantPub string
	}{
		{"ssh-ed25519", "ssh-ed25519"},
		{"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp256"},
		{"ssh-rsa", "ssh-rsa"},
	} {
		t.Run(tc.keyType, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := generateSSHKey(context.Background(), path, tc.keyType, "reach:test"); err != nil {
				t.Fatal(err)
			}
			priv, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseTunnelPrivateKey(priv); err != nil {
				t.Fatalf("private key did not parse: %v", err)
			}
			pub, err := os.ReadFile(path + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(string(pub))[0]; got != tc.wantPub {
				t.Fatalf("public key type = %q, want %q", got, tc.wantPub)
			}
			if _, _, _, _, err := ssh.ParseAuthorizedKey(pub); err != nil {
				t.Fatalf("public key did not parse: %v", err)
			}
		})
	}
}

func TestEnsureTunnelKeyDefaultsToEd25519AndRepairsPublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel_key")
	if err := ensureTunnelKey(context.Background(), path, "box", SSHCompatConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := publicKeyType(path + ".pub"); got != "ssh-ed25519" {
		t.Fatalf("new tunnel key type = %q, want ssh-ed25519", got)
	}
	if err := os.Remove(path + ".pub"); err != nil {
		t.Fatal(err)
	}
	if err := ensureTunnelKey(context.Background(), path, "box", SSHCompatConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := publicKeyType(path + ".pub"); got != "ssh-ed25519" {
		t.Fatalf("repaired tunnel public key type = %q, want ssh-ed25519", got)
	}
}
