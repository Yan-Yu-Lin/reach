package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// PoC from daybreak P1-4: two concurrent registrations with one invite both
// came back approved and both provisioned.
func TestInviteIsClaimedAtomically(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	server.registerL = newRateLimiter(1000, time.Minute)
	hash, err := HashSecret("invite-code-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO invites (code_hash,type,expires_at,created_at) VALUES (?,'friend',?,?)`, hash, time.Now().UTC().Add(time.Hour).Format(time.RFC3339), nowUTC()); err != nil {
		t.Fatal(err)
	}
	const n = 4
	codes := make([]int, n)
	tokens := make([]string, n)
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"name": "friend-" + string(rune('a'+i)) + "-box", "auth_code": "invite-code-1"})
			rec := doJSON(t, server, http.MethodPost, "/api/client/register", string(body), "")
			codes[i] = rec.Code
			var out struct {
				RequestID  string `json:"request_id"`
				SetupToken string `json:"setup_token"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			tokens[i], ids[i] = out.SetupToken, out.RequestID
		}(i)
	}
	wg.Wait()
	winners := 0
	winner := -1
	for i, c := range codes {
		if c == 200 {
			winners++
			winner = i
		}
	}
	if winners != 1 {
		t.Fatalf("register codes = %v, want exactly one 200", codes)
	}
	slug := "friend-" + string(rune('a'+winner)) + "-box"
	if rec := doJSON(t, server, http.MethodPost, "/api/client/provision", provisionBody(ids[winner], tokens[winner], map[string]any{"slug": slug}), ""); rec.Code != 200 {
		t.Fatalf("winner provision = %d %s", rec.Code, rec.Body.String())
	}
	var used, claimed string
	_ = store.db.QueryRow(`SELECT used_by,claimed_by_request_id FROM invites`).Scan(&used, &claimed)
	if used == "" || claimed != ids[winner] {
		t.Fatalf("invite used_by=%q claimed=%q", used, claimed)
	}
	// A used invite cannot be claimed again.
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"late-box","auth_code":"invite-code-1"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse of consumed invite = %d", rec.Code)
	}
}

func TestExpiredInviteClaimIsReleased(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	hash, _ := HashSecret("invite-code-2")
	if _, err := store.db.Exec(`INSERT INTO invites (code_hash,type,expires_at,created_at) VALUES (?,'friend',?,?)`, hash, time.Now().UTC().Add(time.Hour).Format(time.RFC3339), nowUTC()); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/client/register", `{"name":"slow-box","auth_code":"invite-code-2"}`, ""); rec.Code != 200 {
		t.Fatalf("register = %d", rec.Code)
	}
	if _, err := store.db.Exec(`UPDATE requests SET expires_at=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := server.prov.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var claimed *string
	_ = store.db.QueryRow(`SELECT claimed_by_request_id FROM invites`).Scan(&claimed)
	if claimed != nil {
		t.Fatal("expired request kept its invite claim")
	}
}
