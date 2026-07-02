package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	gliderssh "github.com/gliderlabs/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestLoadAuthorizedKeysBareAndOptions(t *testing.T) {
	_, priv1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, priv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer1, err := cryptossh.NewSignerFromKey(priv1)
	if err != nil {
		t.Fatal(err)
	}
	signer2, err := cryptossh.NewSignerFromKey(priv2)
	if err != nil {
		t.Fatal(err)
	}
	bare := string(cryptossh.MarshalAuthorizedKey(signer1.PublicKey()))
	withOptions := "from=\"127.0.0.1,::1\",no-agent-forwarding,no-X11-forwarding,no-port-forwarding " + stringsTrimNewline(string(cryptossh.MarshalAuthorizedKey(signer2.PublicKey()))) + " reach:test\n"

	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, []byte("# comment\n\n"+bare+withOptions), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}
	if !publicKeyAuthorized(signer1.PublicKey(), keys) {
		t.Fatal("bare key was not authorized")
	}
	if !publicKeyAuthorized(signer2.PublicKey(), keys) {
		t.Fatal("options-prefixed key was not authorized")
	}
}

func TestPublicKeyAuthorizedRejectsStranger(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, strangerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := cryptossh.NewSignerFromKey(strangerPriv)
	if err != nil {
		t.Fatal(err)
	}
	if publicKeyAuthorized(stranger.PublicKey(), []gliderssh.PublicKey{signer.PublicKey()}) {
		t.Fatal("stranger key was authorized")
	}
}

func TestEnsureInternalHostKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_ed25519_key")
	if err := ensureInternalHostKey(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cryptossh.ParsePrivateKey(first); err != nil {
		t.Fatalf("generated host key did not parse: %v", err)
	}
	if err := ensureInternalHostKey(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("host key changed on second ensure")
	}
}

func stringsTrimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
