package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testPubkey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"

func registerAndApprove(t *testing.T, server *Server, cfg Config, body string) (id, secret string) {
	t.Helper()
	rec := doJSON(t, server, http.MethodPost, "/api/client/register", body, "")
	if rec.Code != 200 {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		RequestID    string `json:"request_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	admin, err := SignJWT(cfg.JWTSecret, "u", "tester", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/requests/"+out.RequestID+"/approve", "", admin); rec.Code != 200 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	return out.RequestID, out.ClientSecret
}

func setupTokenFor(t *testing.T, server *Server, id, secret string) string {
	t.Helper()
	rec := doJSON(t, server, http.MethodPost, "/api/client/register/"+id+"/poll", `{"client_secret":"`+secret+`"}`, "")
	var out struct {
		SetupToken string `json:"setup_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SetupToken == "" {
		t.Fatalf("no setup token: %s", rec.Body.String())
	}
	return out.SetupToken
}

func provisionBody(id, token string, fields map[string]any) string {
	m := map[string]any{"request_id": id, "setup_token": token, "pubkey": testPubkey}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// PoC from Opus P1-2: approve without a slug, then provision as jason-public.
func TestProvisionSlugMustMatchApproval(t *testing.T) {
	server, _, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"Friend laptop"}`)
	tok := setupTokenFor(t, server, id, secret)
	rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "jason-public"}), "")
	if rec.Code == 200 {
		t.Fatal("provision accepted a slug different from the approved one")
	}
	rec = doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "friend-laptop"}), "")
	if rec.Code != 200 {
		t.Fatalf("provision with approved slug = %d %s", rec.Code, rec.Body.String())
	}
}

// PoC from daybreak P2-1: approved safeuser/user/22222, provision with empty
// values to get root/system/22.
func TestProvisionCannotClearApprovedFields(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"safe-box","target_user":"safeuser","mode":"user-sshd","local_port":22222,"metadata":{"source_ip":"125.229.144.147"}}`)
	var observed, metaJSON string
	_ = store.db.QueryRow(`SELECT observed_ip,metadata_json FROM requests WHERE id=?`, id).Scan(&observed, &metaJSON)
	if strings.Contains(metaJSON, "125.229.144.147") || observed == "125.229.144.147" {
		t.Fatalf("client metadata overrode server source_ip: %s / %s", observed, metaJSON)
	}
	tok := setupTokenFor(t, server, id, secret)
	for _, fields := range []map[string]any{
		{"slug": "safe-box", "target_user": "", "mode": "user-sshd", "local_port": 22222},
		{"slug": "safe-box", "target_user": "safeuser", "mode": "", "local_port": 22222},
		{"slug": "safe-box", "target_user": "safeuser", "mode": "user-sshd", "local_port": 0},
		{"slug": "safe-box", "target_user": "root", "mode": "user-sshd", "local_port": 22222},
	} {
		if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, fields), ""); rec.Code == 200 {
			t.Fatalf("provision accepted %v", fields)
		}
	}
	rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "safe-box", "target_user": "safeuser", "mode": "user-sshd", "local_port": 22222}), "")
	if rec.Code != 200 {
		t.Fatalf("matching provision = %d %s", rec.Code, rec.Body.String())
	}
}

func TestProvisionFailsClosedOnCorruptMetadata(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"corrupt-box"}`)
	tok := setupTokenFor(t, server, id, secret)
	if _, err := store.db.Exec(`UPDATE requests SET metadata_json='{not json' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "corrupt-box"}), ""); rec.Code == 200 {
		t.Fatal("provision succeeded with undecodable approved metadata")
	}
}

func TestReservedSlugsRejected(t *testing.T) {
	server, _, _ := newReliabilityTestServer(t)
	for _, name := range []string{"jason-ts", "jason", "home-public", "localhost", "reach-hub"} {
		rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"`+name+`"}`, "")
		if rec.Code != 400 {
			t.Fatalf("reserved %q register = %d", name, rec.Code)
		}
		server.registerL = newRateLimiter(1000, time.Minute)
	}
	if !SlugReserved("pi", nil) || !SlugReserved("my-box", []string{"my-*"}) || SlugReserved("omarchy", nil) {
		t.Fatal("configurable reserved patterns misbehave")
	}
}

// Daybreak P2-2 PoC: two keys in one pubkey field.
func TestProvisionRejectsMultiLineKey(t *testing.T) {
	server, _, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"multi-key"}`)
	tok := setupTokenFor(t, server, id, secret)
	m := map[string]any{"request_id": id, "setup_token": tok, "slug": "multi-key", "pubkey": testPubkey + "\n" + testPubkey}
	b, _ := json.Marshal(m)
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", string(b), ""); rec.Code == 200 {
		t.Fatal("multi-line public key accepted")
	}
}

func TestRequestsListExposesObservedIPAndFilter(t *testing.T) {
	server, _, cfg := newReliabilityTestServer(t)
	registerAndApprove(t, server, cfg, `{"name":"listed-box"}`)
	server.registerL = newRateLimiter(1000, time.Minute)
	_ = doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"pending-box"}`, "")
	admin, _ := SignJWT(cfg.JWTSecret, "u", "tester", "owner", time.Hour)
	rec := doJSON(t, server, http.MethodGet, "/api/admin/requests?status=pending", "", admin)
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0]["slug"] != "pending-box" || rows[0]["observed_ip"] == nil {
		t.Fatalf("pending filter rows = %#v", rows)
	}
}
