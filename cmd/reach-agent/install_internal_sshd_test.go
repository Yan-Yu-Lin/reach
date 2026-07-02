package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestInstallAdminKeysBareForInternalSSHD(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(signer.PublicKey())))
	authFile := filepath.Join(t.TempDir(), "internal-sshd", "authorized_keys")
	prov := provisionResponse{Machine: machineInfo{ID: "m_test"}, AdminPubkeys: []adminPubkey{{PublicKey: pub}}}
	if err := installAdminKeys(authFile, "", prov, false, internalSSHDSupportedKeyTypes, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(b))
	want := pub + " reach:m_test"
	if got != want {
		t.Fatalf("authorized_keys line mismatch\ngot:  %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "from=") || strings.Contains(got, "no-port-forwarding") {
		t.Fatalf("internal-sshd key line should be bare, got %q", got)
	}
}
