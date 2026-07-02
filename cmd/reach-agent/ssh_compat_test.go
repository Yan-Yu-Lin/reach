package main

import "testing"

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
