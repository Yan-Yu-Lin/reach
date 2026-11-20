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

// PoC from Opus P1-1: a well-formed but unknown cipher name makes OpenSSH
// reject the whole config. ssh -G must catch it and the previous file must
// survive.
func TestInstallKeepsPreviousFileWhenSSHRejectsConfig(t *testing.T) {
	sshBin := requireSSH(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "reach-tunnels.conf")
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{goodHost()}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(out)
	bad := goodHost()
	bad.Options = [][2]string{{"Ciphers", "bogus-cipher-xyz"}}
	if err := validateHost(bad); err != nil {
		t.Fatalf("grammar should accept well-formed name: %v", err)
	}
	if err := installSSHConfig(context.Background(), sshBin, out, []sshHost{bad}); err == nil {
		t.Fatal("ssh -G accepted bogus cipher")
	}
	after, _ := os.ReadFile(out)
	if string(after) != string(before) {
		t.Fatal("previous config was replaced after validation failure")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".reach-validate-*")); len(matches) != 0 {
		t.Fatalf("validation temp file left behind: %v", matches)
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
