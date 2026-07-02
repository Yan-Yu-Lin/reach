package main

import "testing"

func TestSSHConfigClientOptionsFromHeartbeat(t *testing.T) {
	raw := `{"capabilities":{"ssh":{"client_options":["KexAlgorithms=diffie-hellman-group14-sha1","HostKeyAlgorithms=ssh-rsa","BadOption=x","Ciphers=aes128-ctr bad"]}}}`
	opts := sshConfigClientOptions(raw)
	if len(opts) != 2 {
		t.Fatalf("expected 2 safe options, got %#v", opts)
	}
	if opts[0] != [2]string{"KexAlgorithms", "diffie-hellman-group14-sha1"} || opts[1] != [2]string{"HostKeyAlgorithms", "ssh-rsa"} {
		t.Fatalf("unexpected options: %#v", opts)
	}
}
