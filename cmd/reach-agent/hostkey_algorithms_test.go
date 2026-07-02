package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestPinnedHostKeyAlgorithmsForTarget(t *testing.T) {
	target := "203.0.113.10:443"
	knownHosts := strings.Join([]string{
		"[203.0.113.10]:443 " + testHostKeyLine(t, "ssh-ed25519"),
		"[203.0.113.10]:443 " + testHostKeyLine(t, "ssh-rsa"),
		"[example.invalid]:443 " + testHostKeyLine(t, "ecdsa-sha2-nistp256"),
	}, "\n") + "\n"
	got, err := pinnedHostKeyAlgorithmsFromKnownHosts(strings.NewReader(knownHosts), "known_hosts", target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh-ed25519", "ssh-rsa"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("algorithms=%v want %v", got, want)
	}
}

func TestPinnedHostKeyAlgorithmsDefaultPortAndWildcard(t *testing.T) {
	knownHosts := "*.example.test " + testHostKeyLine(t, "ssh-ed25519") + "\n"
	got, err := pinnedHostKeyAlgorithmsFromKnownHosts(strings.NewReader(knownHosts), "known_hosts", "box.example.test:22")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ssh-ed25519" {
		t.Fatalf("algorithms=%v want [ssh-ed25519]", got)
	}
}

func TestPinnedHostKeyAlgorithmsNoMatchFailsClosed(t *testing.T) {
	knownHosts := "[other.example]:443 " + testHostKeyLine(t, "ssh-ed25519") + "\n"
	if _, err := pinnedHostKeyAlgorithmsFromKnownHosts(strings.NewReader(knownHosts), "known_hosts", "203.0.113.10:443"); err == nil {
		t.Fatal("expected no pinned host keys error")
	}
}

func TestKnownHostsNegatedPattern(t *testing.T) {
	knownHosts := "!blocked.example.test,*.example.test " + testHostKeyLine(t, "ssh-ed25519") + "\n"
	if _, err := pinnedHostKeyAlgorithmsFromKnownHosts(strings.NewReader(knownHosts), "known_hosts", "blocked.example.test:22"); err == nil {
		t.Fatal("expected negated host to have no pinned algorithms")
	}
}

func TestIntersectAlgorithms(t *testing.T) {
	got := intersectAlgorithms([]string{"ecdsa-sha2-nistp256", "ssh-rsa", "ssh-rsa", "ssh-ed25519"}, []string{"ssh-ed25519", "ssh-rsa"})
	want := []string{"ssh-rsa", "ssh-ed25519"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("intersectAlgorithms=%v want %v", got, want)
	}
}

func testHostKeyLine(t *testing.T, typ string) string {
	t.Helper()
	var pub any
	switch typ {
	case "ssh-ed25519":
		p, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub = p
	case "ssh-rsa":
		k, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		pub = &k.PublicKey
	case "ecdsa-sha2-nistp256":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub = &k.PublicKey
	default:
		t.Fatalf("unknown key type %s", typ)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}
