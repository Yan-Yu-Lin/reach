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
	admin, err := adminTestToken(t, server.store, cfg)
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
	admin, _ := adminTestToken(t, server.store, cfg)
	rec := doJSON(t, server, http.MethodGet, "/api/admin/requests?status=pending", "", admin)
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0]["slug"] != "pending-box" || rows[0]["observed_ip"] == nil {
		t.Fatalf("pending filter rows = %#v", rows)
	}
}

func TestDuplicateSlugRejectedBeforeTokenIsSpent(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_taken", "taken-box")
	rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"taken-box"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("register duplicate = %d %s", rec.Code, rec.Body.String())
	}
}

// Daybreak P2-3: omitted fields are not wildcards. The registration bound
// the defaults (root/system/22); provision cannot pick other values.
func TestOmittedFieldsAreBoundToDefaults(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"bare-box"}`)
	var meta string
	_ = store.db.QueryRow(`SELECT metadata_json FROM requests WHERE id=?`, id).Scan(&meta)
	for _, want := range []string{`"target_user":"root"`, `"mode":"system"`, `"local_port":22`} {
		if !strings.Contains(meta, want) {
			t.Fatalf("registration did not bind %s: %s", want, meta)
		}
	}
	tok := setupTokenFor(t, server, id, secret)
	for _, f := range []map[string]any{
		{"slug": "bare-box", "local_port": 2222},
		{"slug": "bare-box", "mode": "user-sshd"},
		{"slug": "bare-box", "target_user": "alice"},
	} {
		if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, f), ""); rec.Code == 200 {
			t.Fatalf("provision accepted %v after approval of defaults", f)
		}
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "bare-box"}), ""); rec.Code != 200 {
		t.Fatalf("provision with the bound defaults = %d %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyRequestWithoutBindingMustReregister(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"legacy-req"}`)
	tok := setupTokenFor(t, server, id, secret)
	if _, err := store.db.Exec(`UPDATE requests SET metadata_json='{"name":"legacy-req","slug":"legacy-req"}' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(id, tok, map[string]any{"slug": "legacy-req"}), ""); rec.Code != http.StatusGone {
		t.Fatalf("legacy request provision = %d", rec.Code)
	}
}

// Daybreak P2-4 PoC: an expired pending request was approved and then
// minted a fresh setup token.
func TestExpiredRequestCannotBeApprovedOrPolled(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"stale-box"}`, "")
	var out struct {
		RequestID    string `json:"request_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE requests SET expires_at=? WHERE id=?`, past, out.RequestID); err != nil {
		t.Fatal(err)
	}
	admin, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/requests/"+out.RequestID+"/approve", "", admin); rec.Code == 200 {
		t.Fatal("expired request approved")
	}
	// Approved before expiry, polled after: no new setup token.
	server.registerL = newRateLimiter(100, time.Minute)
	id2, secret2 := registerAndApprove(t, server, cfg, `{"name":"late-poll"}`)
	if _, err := store.db.Exec(`UPDATE requests SET expires_at=? WHERE id=?`, past, id2); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, server, http.MethodPost, "/api/client/register/"+id2+"/poll", `{"client_secret":"`+secret2+`"}`, "")
	if strings.Contains(rec.Body.String(), `"setup_token"`) || !strings.Contains(rec.Body.String(), `"expired"`) {
		t.Fatalf("poll after request expiry = %s", rec.Body.String())
	}
}

func TestSetupTokenExpiryClampedToRequest(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	id, secret := registerAndApprove(t, server, cfg, `{"name":"clamp-box"}`)
	soon := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE requests SET expires_at=? WHERE id=?`, soon, id); err != nil {
		t.Fatal(err)
	}
	_ = setupTokenFor(t, server, id, secret)
	var exp string
	_ = store.db.QueryRow(`SELECT setup_token_expires_at FROM requests WHERE id=?`, id).Scan(&exp)
	if exp > soon {
		t.Fatalf("setup token expires %s after request %s", exp, soon)
	}
}

// Daybreak P2-6: two live requests may not hold the same slug; denying or
// expiring one releases it.
func TestSlugReservedAcrossLiveRequests(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	server.registerL = newRateLimiter(100, time.Minute)
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"twin-box"}`, ""); rec.Code != 200 {
		t.Fatalf("first register %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"twin-box"}`, ""); rec.Code != http.StatusConflict {
		t.Fatalf("second live request for the same slug = %d, want 409", rec.Code)
	}
	var id string
	_ = store.db.QueryRow(`SELECT id FROM requests WHERE slug='twin-box'`).Scan(&id)
	admin, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/requests/"+id+"/deny", "", admin); rec.Code != 200 {
		t.Fatalf("deny %d", rec.Code)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"twin-box"}`, ""); rec.Code != 200 {
		t.Fatalf("register after deny = %d", rec.Code)
	}
	// An abandoned (expired but unmarked) request does not block the slug.
	if _, err := store.db.Exec(`UPDATE requests SET expires_at=? WHERE slug='twin-box' AND status='pending'`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"twin-box"}`, ""); rec.Code != 200 {
		t.Fatalf("register after expiry = %d", rec.Code)
	}
}
