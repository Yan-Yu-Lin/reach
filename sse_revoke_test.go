package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openStream connects to /api/admin/events and returns a channel of raw
// lines plus a done channel closed when the server ends the stream.
func openStream(t *testing.T, ts *httptest.Server, bearer string) (<-chan string, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/admin/events", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	lines := make(chan string, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if l == "event: hello" {
				<-lines // hello's data line
				return lines, done
			}
		case <-deadline:
			t.Fatal("no hello")
		}
	}
}

func expectStreamClosedWithoutData(t *testing.T, lines <-chan string, done <-chan struct{}) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data:") || strings.HasPrefix(l, "event:") {
				t.Fatalf("revoked stream still received: %q", l)
			}
		case <-done:
			// Drain anything buffered before close.
			for {
				select {
				case l := <-lines:
					if strings.HasPrefix(l, "data:") || strings.HasPrefix(l, "event:") {
						t.Fatalf("revoked stream still received: %q", l)
					}
				default:
					return
				}
			}
		case <-timeout:
			t.Fatal("revoked stream was not closed")
		}
	}
}

// Astra F01 (TestAstraReviewRevokedSSEStillReceives): after revoke-all the
// existing stream must not receive any further event.
func TestRevokeAllClosesExistingEventStream(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	tok, _ := adminTestToken(t, store, cfg)
	other, _ := adminTestToken(t, store, cfg)
	lines, done := openStream(t, ts, other)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/sessions/revoke-all", "", tok); rec.Code != 200 {
		t.Fatalf("revoke-all = %d", rec.Code)
	}
	server.events.PublishLive("agent.heartbeat", "m_review", map[string]any{"machine_id": "m_review", "heartbeat_at": nowUTC(), "observed_state": "online", "status": "online"})
	server.publishEvent(context.Background(), "machine.desired_changed", "m_review", map[string]any{"machine_id": "m_review"})
	expectStreamClosedWithoutData(t, lines, done)
}

// Even without a revocation broadcast (e.g. another reachd process, or a
// token that simply expired), every event is preceded by a re-check.
func TestEventStreamRechecksTokenBeforeEachEvent(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	server.revocations = nil
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	tok, _ := adminTestToken(t, store, cfg)
	lines, done := openStream(t, ts, tok)
	if _, err := store.db.Exec(`UPDATE users SET session_version=session_version+1`); err != nil {
		t.Fatal(err)
	}
	server.events.PublishLive("agent.heartbeat", "m_review", map[string]any{"machine_id": "m_review"})
	expectStreamClosedWithoutData(t, lines, done)
}

func TestRevokedServiceTokenStreamCloses(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	if _, err := store.db.Exec(`INSERT INTO service_tokens (id,label,token_hash,role,created_at) VALUES ('stkn_sse','mac',?,'mac-agent',?)`, TokenHash("sat_sse"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	lines, done := openStream(t, ts, "sat_sse")
	tok, _ := adminTestToken(t, store, cfg)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/sessions/revoke-all", `{"include_service_tokens":true}`, tok); rec.Code != 200 {
		t.Fatalf("revoke-all = %d", rec.Code)
	}
	server.events.PublishLive("agent.heartbeat", "m_review", map[string]any{"machine_id": "m_review"})
	expectStreamClosedWithoutData(t, lines, done)
}

func TestLogoutClosesOwnStreamButNotOthers(t *testing.T) {
	server, store, cfg := newReliabilityTestServer(t)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	a, _ := adminTestToken(t, store, cfg)
	time.Sleep(1100 * time.Millisecond) // distinct iat so the two JWTs differ
	b, _ := adminTestToken(t, store, cfg)
	if a == b {
		t.Fatal("fixture produced identical tokens")
	}
	linesA, doneA := openStream(t, ts, a)
	linesB, _ := openStream(t, ts, b)
	if rec := doJSON(t, server, http.MethodPost, "/api/admin/logout", "", a); rec.Code != 200 {
		t.Fatalf("logout = %d", rec.Code)
	}
	server.events.PublishLive("agent.heartbeat", "m_x", map[string]any{"machine_id": "m_x"})
	expectStreamClosedWithoutData(t, linesA, doneA)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case l := <-linesB:
			if l == "event: agent.heartbeat" {
				return
			}
		case <-timeout:
			t.Fatal("unrevoked stream stopped receiving events")
		}
	}
}
