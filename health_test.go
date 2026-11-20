package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSSListeners(t *testing.T) {
	out := "LISTEN 0 128 127.0.0.1:9208 0.0.0.0:*\nLISTEN 0 4096 [::1]:9300 [::]:*\nLISTEN 0 128 *:22 *:*\n"
	got := parseSSListeners(out)
	for _, p := range []int{9208, 9300, 22} {
		if !got[p] {
			t.Fatalf("missing port %d in %v", p, got)
		}
	}
	if got[0] || len(got) != 3 {
		t.Fatalf("unexpected ports %v", got)
	}
}

// perf #5/#6: one listener scan per round and no DB writes when nothing
// changed.
func TestHealthCheckBatchesAndSkipsUnchangedWrites(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	for i, id := range []string{"m_h1", "m_h2", "m_h3"} {
		insertTestMachine(t, store, id, "health-"+id[2:])
		if _, err := store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES (?,?,'primary',?,?,'k','offline',?)`, "t_"+id, id, "rt-health0"+string(rune('1'+i)), 9220+i, nowUTC()); err != nil {
			t.Fatal(err)
		}
	}
	scans := 0
	server.prov.listListeners = func(context.Context) (map[int]bool, error) {
		scans++
		return map[int]bool{}, nil
	}
	if _, err := server.prov.RunHealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("listener scans per round = %d, want 1", scans)
	}
	// Rewind every observation to a sentinel one minute ago: inside the
	// refresh window, but a different string from anything a rewrite this
	// second would produce. Comparing against the first round's own
	// timestamp could not see a rewrite in the same second (astra P3-01).
	sentinel := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE hub_observations SET last_probe_at=?`, sentinel); err != nil {
		t.Fatal(err)
	}
	if _, err := server.prov.RunHealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	var obsWrites int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM hub_observations WHERE last_probe_at<>?`, sentinel).Scan(&obsWrites)
	if obsWrites != 0 {
		t.Fatalf("unchanged round rewrote %d observations", obsWrites)
	}
	// Past the refresh window the row must be refreshed.
	stale := time.Now().UTC().Add(-2 * dormantRefresh).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE hub_observations SET last_probe_at=? WHERE tunnel_id='t_m_h1'`, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := server.prov.RunHealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	var refreshed string
	_ = store.db.QueryRow(`SELECT last_probe_at FROM hub_observations WHERE tunnel_id='t_m_h1'`).Scan(&refreshed)
	if refreshed == stale {
		t.Fatal("stale observation was not refreshed")
	}
	var events int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='machine.health_changed'`).Scan(&events)
	if events != 3 {
		t.Fatalf("health events = %d, want 3 (first round only)", events)
	}
}

func TestServiceTokenLastUsedIsThrottled(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	if _, err := store.db.Exec(`INSERT INTO service_tokens (id,label,token_hash,role,created_at) VALUES ('stkn_t','mac',?,'mac-agent',?)`, TokenHash("sat_t"), nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.parseServiceToken(context.Background(), "sat_t"); err != nil {
		t.Fatal(err)
	}
	// Mark the row with a sentinel inside the throttle window; further uses
	// must not overwrite it.
	sentinel := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE service_tokens SET last_used_at=?`, sentinel); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _ = server.parseServiceToken(context.Background(), "sat_t")
	}
	var got string
	_ = store.db.QueryRow(`SELECT last_used_at FROM service_tokens`).Scan(&got)
	if got != sentinel {
		t.Fatalf("last_used_at rewritten within throttle window: %s", got)
	}
	stale := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	_, _ = store.db.Exec(`UPDATE service_tokens SET last_used_at=?`, stale)
	_, _ = server.parseServiceToken(context.Background(), "sat_t")
	_ = store.db.QueryRow(`SELECT last_used_at FROM service_tokens`).Scan(&got)
	if got == stale {
		t.Fatal("stale last_used_at was not refreshed")
	}
}

func TestManifestIsCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latest.json")
	if err := os.WriteFile(path, []byte(`{"version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, ok := readManifestCached(path)
	if !ok || rel.Version != "1.0.0" {
		t.Fatalf("first read = %#v %v", rel, ok)
	}
	// Removing the file is not seen until the TTL passes: proof the
	// heartbeat path does not touch the disk each time.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rel, ok := readManifestCached(path); !ok || rel.Version != "1.0.0" {
		t.Fatalf("cached read = %#v %v", rel, ok)
	}
}

// Astra dashboard R2-03: health events carry the server's probe time so a
// replayed event is not mistaken for a fresh probe.
func TestHealthChangedEventCarriesServerProbeTime(t *testing.T) {
	server, store, _ := newReliabilityTestServer(t)
	insertTestMachine(t, store, "m_hp", "hp-box")
	if _, err := store.db.Exec(`INSERT INTO tunnels (id,machine_id,hub_id,unix_user,remote_port,tunnel_pubkey,status,created_at) VALUES ('t_hp','m_hp','primary','rt-hpbox111',9270,'k','offline',?)`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	server.prov.listListeners = func(context.Context) (map[int]bool, error) { return map[int]bool{}, nil }
	if _, err := server.prov.RunHealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	var payload, stored string
	_ = store.db.QueryRow(`SELECT payload_json FROM events WHERE type='machine.health_changed' ORDER BY id DESC LIMIT 1`).Scan(&payload)
	_ = store.db.QueryRow(`SELECT last_probe_at FROM hub_observations WHERE tunnel_id='t_hp'`).Scan(&stored)
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatal(err)
	}
	if data["last_probe_at"] != stored || stored == "" {
		t.Fatalf("event last_probe_at=%v, stored=%s", data["last_probe_at"], stored)
	}
	for _, k := range []string{"machine_id", "observed_state", "status", "probe_state", "event_at"} {
		if _, ok := data[k]; !ok {
			t.Fatalf("event lost field %s: %v", k, data)
		}
	}
	// The same event replayed later still reports the original probe time.
	ts := httptest.NewServer(server)
	defer ts.Close()
	tok, _ := adminTestToken(t, store, server.cfg)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Last-Event-ID", "0")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") && strings.Contains(sc.Text(), "probe_state") {
			if !strings.Contains(sc.Text(), `"last_probe_at":"`+stored+`"`) {
				t.Fatalf("replayed event lacks probe time: %s", sc.Text())
			}
			return
		}
	}
	t.Fatal("health event not replayed")
}
