package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func goodHost() sshHost {
	return sshHost{Host: "jack", HostName: "localhost", Port: 9210, User: "arthur", ProxyJump: "jason-public", HostKeyAlias: "reach-m_abc123", ObservedState: "online"}
}

func TestLegacyParserRejectsExecutableDirectives(t *testing.T) {
	// PoC from daybreak P1-3: a compromised hub returns Match exec /
	// ProxyCommand / LocalCommand / Include in the raw ssh-config text.
	for _, evil := range []string{
		"Match exec \"touch /tmp/pwned\"\n",
		"Host jack\n    ProxyCommand sh -c 'touch /tmp/pwned'\n",
		"Host jack\n    LocalCommand touch /tmp/pwned\n    PermitLocalCommand yes\n",
		"Include /tmp/evil.conf\n",
		"Host jack\n    KnownHostsCommand /bin/sh\n",
		"Host jack\n    ForwardAgent yes\n",
		"Host jack\n    IdentityFile ~/.ssh/id_ed25519\n",
	} {
		if _, err := parseLegacySSHConfig([]byte(evil)); err == nil {
			t.Fatalf("legacy parser accepted %q", evil)
		}
	}
}

func TestValidateHostRejectsBadValues(t *testing.T) {
	cases := map[string]func(*sshHost){
		"reserved jason-ts":     func(h *sshHost) { h.Host = "jason-ts" },
		"reserved jason-public": func(h *sshHost) { h.Host = "jason-public" },
		"reserved suffix":       func(h *sshHost) { h.Host = "home-public" },
		"remote hostname":       func(h *sshHost) { h.HostName = "evil.example" },
		"proxyjump token":       func(h *sshHost) { h.ProxyJump = "%h" },
		"option name":           func(h *sshHost) { h.Options = [][2]string{{"ProxyCommand", "sh"}} },
		"option value newline":  func(h *sshHost) { h.Options = [][2]string{{"Ciphers", "aes128-ctr\nProxyCommand"}} },
		"option value percent":  func(h *sshHost) { h.Options = [][2]string{{"Ciphers", "aes128-ctr%0a"}} },
		"option value hash":     func(h *sshHost) { h.Options = [][2]string{{"HostKeyAlgorithms", "ssh-rsa#x"}} },
		"host key alias":        func(h *sshHost) { h.HostKeyAlias = "jason" },
		"user dash":             func(h *sshHost) { h.User = "-oProxyCommand=x" },
	}
	for name, mutate := range cases {
		h := goodHost()
		mutate(&h)
		if err := validateHost(h); err == nil {
			t.Fatalf("%s: validateHost accepted %#v", name, h)
		}
	}
	if err := validateHost(goodHost()); err != nil {
		t.Fatal(err)
	}
}

func TestRenderedConfigUsesFixedTemplate(t *testing.T) {
	out := renderSSHConfig([]sshHost{goodHost()})
	for _, want := range []string{"Host jack\n", "ForwardAgent no", "ClearAllForwardings yes", "HostKeyAlias reach-m_abc123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}

func requireSSH(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh not installed")
	}
	return p
}

// PoC from Opus P1-1 / daybreak P1-4: one host carries a well-formed but
// unknown cipher. ssh -G rejects the candidate; the agent must still install
// the fleet (without the bad per-host options) rather than freeze every
// later update.
func TestBadHostOptionsDoNotFreezeFleet(t *testing.T) {
	sshBin := requireSSH(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "reach-tunnels.conf")
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{goodHost()}); err != nil {
		t.Fatal(err)
	}
	bad := goodHost()
	bad.Host = "evil-box"
	bad.HostKeyAlias = "reach-m_evil01"
	bad.Options = [][2]string{{"Ciphers", "bogus-cipher-xyz"}}
	if err := validateHost(bad); err != nil {
		t.Fatalf("grammar should accept well-formed name: %v", err)
	}
	added := goodHost()
	added.Host = "new-box"
	added.HostKeyAlias = "reach-m_new001"
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{goodHost(), bad, added}); err != nil {
		t.Fatalf("one bad host froze the fleet: %v", err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "Host new-box") || strings.Contains(string(b), "bogus-cipher") {
		t.Fatalf("unexpected config:\n%s", b)
	}
	if len(droppedOptions) != 1 || droppedOptions[0] != "evil-box" {
		t.Fatalf("dropped options reported for %v", droppedOptions)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".reach-validate-*")); len(matches) != 0 {
		t.Fatalf("validation temp file left behind: %v", matches)
	}
}

func TestInstallKeepsPreviousFileWhenConfigStillInvalid(t *testing.T) {
	sshBin := requireSSH(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "reach-tunnels.conf")
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{goodHost()}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(out)
	// A fake ssh that rejects everything stands in for an invalid file
	// that stripping options cannot fix.
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho bad >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := goodHost()
	h.Port = 9999
	if err := installSSHConfig(context.Background(), fake, out, []sshHost{h}); err == nil {
		t.Fatal("install succeeded although ssh -G rejected it")
	}
	if after, _ := os.ReadFile(out); string(after) != string(before) {
		t.Fatal("previous config replaced after validation failure")
	}
}

func TestSyncUsesJSONEndpointAndNeverWritesServerText(t *testing.T) {
	sshBin := requireSSH(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/ssh-config.json":
			_, _ = io.WriteString(w, `{"schema":1,"hosts":[
				{"host":"jack","hostname":"localhost","port":9210,"user":"arthur","proxy_jump":"jason-public","host_key_alias":"reach-m_abc123","machine_id":"m_abc123","observed_state":"offline"},
				{"host":"jason-ts","hostname":"localhost","port":9211,"user":"root","proxy_jump":"jason-public","host_key_alias":"reach-m_evil01","machine_id":"m_evil01","observed_state":"online"}]}`)
		case "/api/admin/ssh-config":
			t.Error("legacy endpoint used although JSON is available")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "reach.conf")
	agent := Agent{cfg: Config{APIURL: server.URL, Token: "t", OutFile: out, SSHBinary: sshBin}, log: log.New(io.Discard, "", 0)}
	if err := agent.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "Host jack\n") || strings.Contains(string(b), "jason-ts") {
		t.Fatalf("unexpected rendered config:\n%s", b)
	}
}

func TestSyncFallsBackToStrictLegacyParser(t *testing.T) {
	sshBin := requireSSH(t)
	legacy := "# Generated by Reach\n\nHost jack\n    HostName localhost\n    Port 9210\n    User arthur\n    ProxyJump jason-public\n    HostKeyAlias reach-m_abc123\n    StrictHostKeyChecking accept-new\n    KexAlgorithms diffie-hellman-group14-sha1\n"
	evil := legacy + "Match exec \"touch /tmp/reach-pwned\"\n"
	body := legacy
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/ssh-config" {
			_, _ = io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "reach.conf")
	agent := Agent{cfg: Config{APIURL: server.URL, Token: "t", OutFile: out, SSHBinary: sshBin}, log: log.New(io.Discard, "", 0)}
	if err := agent.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(out)
	body = evil
	err := agent.Sync(context.Background())
	if err == nil || !errors.Is(err, errLegacyDirective) {
		t.Fatalf("evil legacy config error = %v", err)
	}
	after, _ := os.ReadFile(out)
	if string(after) != string(good) || strings.Contains(string(after), "Match") {
		t.Fatalf("evil config reached disk:\n%s", after)
	}
}

// Astra P2-10: the Mac must reject exactly what the server reserves, plus a
// host that jumps through itself.
func TestMacRejectsEveryServerReservedAlias(t *testing.T) {
	for _, name := range []string{"jason", "jason-ts", "home-public", "pi", "pi-lab", "localhost", "localhost-dev", "reach", "reach-hub", "hub", "hub-node", "github", "gitlab-ci", "bitbucket-work"} {
		h := goodHost()
		h.Host = name
		if err := validateHost(h); err == nil {
			t.Fatalf("reserved alias %q accepted", name)
		}
	}
	h := goodHost()
	h.Host, h.ProxyJump = "jumpbox", "jumpbox"
	if err := validateHost(h); err == nil {
		t.Fatal("host that proxy-jumps through itself accepted")
	}
	h = goodHost()
	h.Host = "my-box"
	if err := validateHost(h, "my-*"); err == nil {
		t.Fatal("locally reserved alias accepted")
	}
	if err := validateHost(goodHost()); err != nil {
		t.Fatal(err)
	}
}

// Daybreak round 3 P3-C: a bad option on one host must not strip the valid
// legacy options of another host.
func TestBadOptionsOnlyAffectTheirHost(t *testing.T) {
	sshBin := requireSSH(t)
	out := filepath.Join(t.TempDir(), "reach-tunnels.conf")
	legacy := goodHost()
	legacy.Host = "old-box"
	legacy.HostKeyAlias = "reach-m_old001"
	legacy.Options = [][2]string{{"KexAlgorithms", "+diffie-hellman-group14-sha1"}}
	bad := goodHost()
	bad.Host = "evil-box"
	bad.HostKeyAlias = "reach-m_evil01"
	bad.Options = [][2]string{{"Ciphers", "bogus-cipher-xyz"}}
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{goodHost(), legacy, bad}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "diffie-hellman-group14-sha1") {
		t.Fatalf("valid legacy option of another host was dropped:\n%s", b)
	}
	if strings.Contains(string(b), "bogus-cipher") {
		t.Fatalf("bad option installed:\n%s", b)
	}
	if len(droppedOptions) != 1 || droppedOptions[0] != "evil-box" {
		t.Fatalf("droppedOptions = %v", droppedOptions)
	}
}

// Daybreak round 3 P3-B: aliases the hub reserves (reserved_slugs, its
// ProxyJump alias) are honoured by the Mac without local configuration.
func TestSyncHonoursServerReservedHosts(t *testing.T) {
	sshBin := requireSSH(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/ssh-config.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"schema":1,"reserved_hosts":["office-*"],"hosts":[
			{"host":"jack","hostname":"localhost","port":9210,"user":"arthur","proxy_jump":"jason-public","host_key_alias":"reach-m_abc123","machine_id":"m_abc123","observed_state":"offline"},
			{"host":"office-nas","hostname":"localhost","port":9211,"user":"root","proxy_jump":"jason-public","host_key_alias":"reach-m_off001","machine_id":"m_off001","observed_state":"online"}]}`)
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "reach.conf")
	agent := Agent{cfg: Config{APIURL: server.URL, Token: "t", OutFile: out, SSHBinary: sshBin}, log: log.New(io.Discard, "", 0)}
	if err := agent.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "Host jack\n") || strings.Contains(string(b), "office-nas") {
		t.Fatalf("server-reserved alias written:\n%s", b)
	}
}
