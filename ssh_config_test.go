package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

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

// PoC values from Opus P1-1: these used to pass straight through to the Mac.
func TestSSHConfigClientOptionsRejectMalformedValues(t *testing.T) {
	for _, v := range []string{"aes128-ctr%0aProxyCommand", "ssh-rsa#x", "AES128-CTR", "a;b", "a,,b", strings.Repeat("a", 600), "+"} {
		raw, _ := json.Marshal(map[string]any{"capabilities": map[string]any{"ssh": map[string]any{"client_options": []string{"Ciphers=" + v}}}})
		if got := sshConfigClientOptions(string(raw)); len(got) != 0 {
			t.Fatalf("value %q accepted: %#v", v, got)
		}
	}
	for _, v := range []string{"+ssh-rsa", "aes128-ctr,aes256-ctr", "hmac-sha2-256-etm@openssh.com", "^curve25519-sha256"} {
		raw, _ := json.Marshal(map[string]any{"capabilities": map[string]any{"ssh": map[string]any{"client_options": []string{"Ciphers=" + v}}}})
		if got := sshConfigClientOptions(string(raw)); len(got) != 1 {
			t.Fatalf("valid value %q rejected", v)
		}
	}
}

func insertSSHMachine(t *testing.T, server *Server, id, slug, observed, tunnelStatus string, port int) {
	t.Helper()
	if _, err := server.store.db.Exec(`INSERT INTO machines (id,slug,original_slug,target_user,status,desired_state,observed_state,created_at) VALUES (?,?,?,'arthur',?,'active',?,?)`, id, slug, slug, observed, observed, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES (?,?,'primary',?,?,'ssh-ed25519 x',?,?)`, "t"+id, id, "rt-"+strings.Repeat(slug[:1], 8), port, tunnelStatus, nowUTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSSHConfigKeepsActiveMachinesWhileOffline(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	insertSSHMachine(t, server, "m_jack01", "jack", ObservedOffline, "offline", 9210)
	insertSSHMachine(t, server, "m_gone01", "gone-box", ObservedGone, "offline", 9211)
	cfg, err := server.GenerateSSHConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host jack\n", "Host gone-box\n", "observed=offline", "ForwardAgent no"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %q:\n%s", want, cfg)
		}
	}
}

func TestSSHConfigSkipsBadRowInsteadOfFailing(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertSSHMachine(t, server, "m_good01", "good-box", ObservedOnline, "online", 9210)
	insertSSHMachine(t, server, "m_bad001", "bad-box", ObservedOnline, "online", 9211)
	if _, err := store.db.Exec(`UPDATE machines SET target_user='bad user' WHERE id='m_bad001'`); err != nil {
		t.Fatal(err)
	}
	cfg, err := server.GenerateSSHConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "Host good-box") || strings.Contains(cfg, "bad-box") {
		t.Fatalf("unexpected config:\n%s", cfg)
	}
}

func TestSSHConfigJSONEndpointForMacAgentRole(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertSSHMachine(t, server, "m_jack01", "jack", ObservedOnline, "online", 9210)
	if _, err := store.db.Exec(`INSERT INTO service_tokens (id,label,token_hash,role,created_at) VALUES ('stkn_1','mac',?,'mac-agent',?)`, TokenHash("sat_test"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodGet, "/api/admin/ssh-config.json", "", "sat_test")
	if rec.Code != 200 {
		t.Fatalf("json endpoint = %d %s", rec.Code, rec.Body.String())
	}
	var doc SSHConfigDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != 1 || len(doc.Hosts) != 1 || doc.Hosts[0].Host != "jack" || doc.Hosts[0].HostKeyAlias != "reach-m_jack01" || doc.Hosts[0].Port != 9210 {
		t.Fatalf("unexpected doc: %#v", doc)
	}
	if rec := doJSON(t, server, http.MethodGet, "/api/admin/machines", "", "sat_test"); rec.Code != http.StatusForbidden {
		t.Fatalf("mac-agent role reached machines: %d", rec.Code)
	}
}
