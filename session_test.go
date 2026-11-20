package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRevokeAllSessionsInvalidatesExistingJWTs(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	tok, _ := adminTestToken(t, store, cfg)
	other, _ := adminTestToken(t, store, cfg)
	if _, err := store.db.Exec(`INSERT INTO service_tokens (id,label,token_hash,role,created_at) VALUES ('stkn_r','mac',?,'mac-agent',?)`, TokenHash("sat_r"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodGet, "/api/admin/machines", "", other); rec.Code != 200 {
		t.Fatalf("before revoke = %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/sessions/revoke-all", `{"include_service_tokens":true}`, tok); rec.Code != 200 {
		t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body.String())
	}
	for _, bearer := range []string{tok, other, "sat_r"} {
		if rec := doJSON(t, server, http.MethodGet, "/api/admin/ssh-config", "", bearer); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token still valid after revoke-all: %d", rec.Code)
		}
	}
	fresh, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodGet, "/api/admin/machines", "", fresh); rec.Code != 200 {
		t.Fatalf("new login after revoke = %d", rec.Code)
	}
}

func TestServiceTokenCannotRevokeSessions(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`INSERT INTO service_tokens (id,label,token_hash,role,created_at) VALUES ('stkn_s','mac',?,'mac-agent',?)`, TokenHash("sat_s"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/sessions/revoke-all", "", "sat_s"); rec.Code == 200 {
		t.Fatal("mac-agent token revoked sessions")
	}
}

func TestAdminJWTTTLIsCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("admin_jwt_ttl: 8760h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminJWTTTL != maxAdminJWTTTL {
		t.Fatalf("ttl = %s", cfg.AdminJWTTTL)
	}
	if DefaultConfig().AdminJWTTTL != 7*24*time.Hour {
		t.Fatal("default TTL changed unexpectedly")
	}
}

func TestCORSRejectsArbitraryLocalhost(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	server.cfg.DefaultHub.APIURL = "https://tunnels.example.com"
	server.cfg.AllowedOrigins = []string{"http://localhost:3000"}
	check := func(origin string) bool {
		req := httptest.NewRequest(http.MethodOptions, "/api/admin/machines", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec.Header().Get("Access-Control-Allow-Origin") == origin
	}
	if check("http://localhost:8080") || check("http://127.0.0.1:5173") || check("https://evil.example") {
		t.Fatal("unlisted origin allowed")
	}
	if !check("https://tunnels.example.com") || !check("http://localhost:3000") {
		t.Fatal("configured origin rejected")
	}
}
