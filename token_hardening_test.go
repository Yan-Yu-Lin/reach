package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doJSON(t *testing.T, server *Server, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func registerPending(t *testing.T, server *Server) (string, string) {
	t.Helper()
	rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"poll-box"}`, "")
	if rec.Code != 200 {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		RequestID    string `json:"request_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.RequestID, out.ClientSecret
}

func TestNewTokensAreStoredAsSHA256(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	id, _ := registerPending(t, server)
	var hash string
	if err := store.db.QueryRow(`SELECT client_secret_hash FROM requests WHERE id=?`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, tokenHashPrefix) {
		t.Fatalf("client secret stored as %q, want sha256", hash[:12])
	}
}

// PoC from daybreak P1-1 / Opus P0-1: a stranger polls their own (or an
// expired/denied/provisioned) request with wrong secrets. None of those
// requests may reach Argon2.
func TestPollDoesNotRunArgonForSHATokensOrTerminalStates(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.argon = newArgonGate(2, time.Second)
	id, secret := registerPending(t, server)
	for i := 0; i < badAuthPerIPObjectPerMinute; i++ {
		rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"wrong"}`, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong secret poll = %d", rec.Code)
		}
	}
	server.badAuthL = newRateLimiter(1000, time.Minute)
	rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"`+secret+`"}`, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("right secret poll = %d %s", rec.Code, rec.Body.String())
	}
	// A legacy Argon2 row in a terminal state must also be answered cheaply.
	legacy, err := HashSecret("legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"denied", "provisioned", "expired"} {
		if _, err := store.db.Exec(`UPDATE requests SET status=?, client_secret_hash=? WHERE id=?`, st, legacy, id); err != nil {
			t.Fatal(err)
		}
		server.pollReqL = newRateLimiter(1000, time.Minute)
		rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"wrong"}`, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"`+st+`"`) || strings.Contains(rec.Body.String(), "setup_token\"") {
			t.Fatalf("%s poll = %d %s", st, rec.Code, rec.Body.String())
		}
	}
	// An expired pending request also short-circuits before hashing.
	if _, err := store.db.Exec(`UPDATE requests SET status='pending', expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"wrong"}`, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"expired"`) {
		t.Fatalf("expired poll = %d %s", rec.Code, rec.Body.String())
	}
	if n := server.argon.runs.Load(); n != 0 {
		t.Fatalf("poll ran Argon2 %d times", n)
	}
}

func pollFrom(t *testing.T, server *Server, ip, id, secret string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/client/register/"+id+"/poll", strings.NewReader(`{"client_secret":"`+secret+`"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", ip)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec.Code
}

func TestPollRateLimits(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	id, secret := registerPending(t, server)
	// Authenticated polls are limited per request.
	limited := false
	for i := 0; i < pollPerRequestPerMinute+5; i++ {
		if pollFrom(t, server, "198.51.100.1", id, secret) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("per-request poll limit never triggered")
	}
	// Per-IP limit across many request IDs.
	limited = false
	for i := 0; i < pollPerIPPerMinute+5; i++ {
		if pollFrom(t, server, "198.51.100.2", "r_other"+string(rune('a'+i%26)), "x") == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("per-IP poll limit never triggered")
	}
}

// Daybreak P2-5: someone who knows a request ID but not its secret must not
// exhaust the real installer's quota.
func TestWrongSecretsDoNotLockOutLegitimatePoller(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	id, secret := registerPending(t, server)
	blocked := false
	for i := 0; i < 50; i++ {
		if pollFrom(t, server, "203.0.113.66", id, "wrong") == http.StatusTooManyRequests {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("attacker was never throttled")
	}
	if code := pollFrom(t, server, "198.51.100.10", id, secret); code != 200 {
		t.Fatalf("legitimate poll after attack = %d", code)
	}
}

func TestWrongAgentTokensDoNotLockOutAgent(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES ('m_rl','rl-box','rl-box','offline','active','offline',1,?,?)`, HashToken("at_rl"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	body := `{"machine_id":"m_rl","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{},"tunnel":{},"persistence":{},"keys":{}}}`
	hb := func(ip, tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/client/agent/heartbeat", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("X-Real-IP", ip)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < agentPerMachinePerMinute+20; i++ {
		hb("203.0.113.66", "at_wrong")
	}
	if code := hb("198.51.100.20", "at_rl"); code != 200 {
		t.Fatalf("real agent after wrong-token flood = %d", code)
	}
	for i := 0; i < agentPerMachinePerMinute+5; i++ {
		if hb("198.51.100.20", "at_rl") == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("authenticated per-machine heartbeat limit never triggered")
}

func TestLegacyArgonClientSecretMigratesOnSuccess(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.argon = newArgonGate(2, time.Second)
	id, _ := registerPending(t, server)
	legacy, err := HashSecret("cs_legacysecret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE requests SET client_secret_hash=? WHERE id=?`, legacy, id); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"cs_legacysecret"}`, "")
	if rec.Code != 200 {
		t.Fatalf("legacy poll = %d %s", rec.Code, rec.Body.String())
	}
	var hash string
	_ = store.db.QueryRow(`SELECT client_secret_hash FROM requests WHERE id=?`, id).Scan(&hash)
	if hash != HashToken("cs_legacysecret") {
		t.Fatalf("legacy hash was not migrated: %q", hash[:12])
	}
}

func TestArgonGateBoundsConcurrency(t *testing.T) {
	gate := newArgonGate(1, 50*time.Millisecond)
	hash, err := HashSecret("pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = gate.VerifySecret(context.Background(), "pw", hash)
	if !errors.Is(err, errArgonBusy) {
		t.Fatalf("saturated gate returned %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("saturated gate did not time out promptly")
	}
	gate.release()
	ok, err := gate.VerifySecret(context.Background(), "pw", hash)
	if err != nil || !ok {
		t.Fatalf("free gate verify = %v %v", ok, err)
	}
}

func TestHeartbeatMigratesLegacyAgentTokenAndRejectsWrongToken(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.argon = newArgonGate(2, time.Second)
	legacy, err := HashSecret("at_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES ('m_mig','mig-box','mig-box','offline','active','offline',1,?,?)`, legacy, nowUTC()); err != nil {
		t.Fatal(err)
	}
	body := `{"machine_id":"m_mig","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{"state":"healthy"},"tunnel":{"state":"stopped"},"persistence":{},"keys":{}}}`
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_legacy"); rec.Code != 200 {
		t.Fatalf("legacy heartbeat = %d %s", rec.Code, rec.Body.String())
	}
	var hash string
	_ = store.db.QueryRow(`SELECT agent_token_hash FROM machines WHERE id='m_mig'`).Scan(&hash)
	if hash != HashToken("at_legacy") {
		t.Fatal("agent token hash was not migrated to sha256")
	}
	runs := server.argon.runs.Load()
	for i := 0; i < 5; i++ {
		if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token heartbeat = %d", rec.Code)
		}
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/agent/heartbeat", body, "at_legacy"); rec.Code != 200 {
		t.Fatalf("migrated heartbeat = %d", rec.Code)
	}
	if server.argon.runs.Load() != runs {
		t.Fatal("heartbeats after migration still ran Argon2")
	}
}

func TestLoginUnknownUserStillRunsArgon(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	server.argon = newArgonGate(2, 5*time.Second)
	rec := doJSON(t, server, http.MethodPost, "/api/admin/login", `{"username":"nobody","password":"x"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown login = %d", rec.Code)
	}
	if server.argon.runs.Load() != 1 {
		t.Fatalf("unknown user login ran Argon2 %d times, want 1", server.argon.runs.Load())
	}
}
