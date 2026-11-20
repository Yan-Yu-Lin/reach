package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Astra R2-08 with the real 8c74086 reach-agent (set REACH_OLD_AGENT_BIN):
// its install must stop at registration with the upgrade message, not after
// an approval at provision.
func TestRealOldAgentGetsUpgradeRequired(t *testing.T) {
	oldAgent := os.Getenv("REACH_OLD_AGENT_BIN")
	if oldAgent == "" {
		t.Skip("set REACH_OLD_AGENT_BIN to an 8c74086 reach-agent binary")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.ProvisioningEnabled = false
	cfg.GodCodeHash, _ = HashSecret("compat-god-code")
	cfg.InitialAdmin.PasswordHash, _ = HashSecret("compat-admin-password")
	cfg.JWTSecret = "compat-jwt-secret-with-sufficient-length"
	if err := store.Migrate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(cfg, store, NewProvisioner(store, cfg)))
	defer ts.Close()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	out, err := exec.Command(oldAgent, "install", "--api-url", ts.URL, "--name", "old-agent-compat", "--token", "compat-god-code", "--target-user", u.Username, "--mode", "user", "--config-dir", filepath.Join(root, "config"), "--data-dir", filepath.Join(root, "data"), "--agent-path", oldAgent, "--transport", "direct", "--yes").CombinedOutput()
	if err == nil {
		t.Fatalf("old agent installed: %s", out)
	}
	if !strings.Contains(string(out), "426") || !strings.Contains(string(out), "too old") {
		t.Fatalf("old agent failed for another reason: %v\n%s", err, out)
	}
	var n int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	if n != 0 {
		t.Fatalf("old agent left %d requests", n)
	}
}
