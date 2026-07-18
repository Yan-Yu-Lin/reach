package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamRetriesSyncBeforePersistingCursor(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 42\nevent: ssh_config.changed\ndata: {}\n\n")
	}))
	defer server.Close()
	agent := Agent{
		cfg: Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(t.TempDir(), "reach.conf")},
		log: log.New(io.Discard, "", 0),
		syncOverride: func(context.Context) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary")
			}
			return nil
		},
	}
	lastID := "41"
	if err := agent.stream(context.Background(), &lastID); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || lastID != "42" || agent.readLastEventID() != "42" {
		t.Fatalf("attempts=%d memory=%q disk=%q", attempts.Load(), lastID, agent.readLastEventID())
	}
}

func TestHelloReconcilesAfterSubscriptionBoundary(t *testing.T) {
	var syncs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: hello\ndata: {\"ok\":true}\n\n")
	}))
	defer server.Close()
	agent := Agent{
		cfg:          Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(t.TempDir(), "reach.conf")},
		log:          log.New(io.Discard, "", 0),
		syncOverride: func(context.Context) error { syncs.Add(1); return nil },
	}
	lastID := ""
	if err := agent.stream(context.Background(), &lastID); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 1 {
		t.Fatalf("hello boundary reconciliations = %d, want 1", syncs.Load())
	}
}

func TestCursorWriteFailureAdvancesMemoryWithoutLivelock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 42\nevent: agent.heartbeat\ndata: {}\n\n")
	}))
	defer server.Close()
	want := errors.New("disk full")
	agent := Agent{
		cfg:          Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(t.TempDir(), "reach.conf")},
		log:          log.New(io.Discard, "", 0),
		cursorWriter: func(string) error { return want },
	}
	lastID := "41"
	err := agent.stream(context.Background(), &lastID)
	if err != nil || lastID != "42" || agent.readLastEventID() != "" {
		t.Fatalf("error=%v memory cursor=%q disk cursor=%q", err, lastID, agent.readLastEventID())
	}
}

func TestStreamPersistsSyncEventCursorOnlyAfterSuccessfulSync(t *testing.T) {
	var syncCalls atomic.Int32
	var streamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/events":
			streamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id: 42\nevent: ssh_config.changed\ndata: {}\n\n")
		case "/api/admin/ssh-config":
			if syncCalls.Add(1) <= eventSyncAttempts {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "Host test\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	agent := Agent{cfg: Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(dir, "reach.conf")}, log: log.New(io.Discard, "", 0)}
	lastID := "41"
	if err := agent.stream(context.Background(), &lastID); err == nil {
		t.Fatal("stream succeeded despite sync failure")
	}
	if lastID != "41" || agent.readLastEventID() != "" {
		t.Fatalf("cursor advanced after failed sync: memory=%q disk=%q", lastID, agent.readLastEventID())
	}
	if err := agent.stream(context.Background(), &lastID); err != nil {
		t.Fatal(err)
	}
	if lastID != "42" || agent.readLastEventID() != "42" {
		t.Fatalf("cursor not persisted after successful sync: memory=%q disk=%q", lastID, agent.readLastEventID())
	}
	if streamCalls.Load() != 2 {
		t.Fatalf("stream calls = %d", streamCalls.Load())
	}
}

func TestRunDoesNotOpenStreamWhenPreSyncFails(t *testing.T) {
	var streams atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/events" {
			streams.Add(1)
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	agent := Agent{cfg: Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(t.TempDir(), "reach.conf")}, log: log.New(io.Discard, "", 0)}
	err := agent.Run(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if streams.Load() != 0 {
		t.Fatalf("opened %d event streams after failed pre-sync", streams.Load())
	}
}

func TestSyncTimesOutWhenResponseBodyStalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	agent := Agent{
		cfg:         Config{APIURL: server.URL, Token: "token", OutFile: filepath.Join(t.TempDir(), "reach.conf")},
		log:         log.New(io.Discard, "", 0),
		syncTimeout: 100 * time.Millisecond,
	}
	started := time.Now()
	err := agent.Sync(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("Sync timeout took too long: %s", time.Since(started))
	}
}

func TestSyncRejectsOversizedAndTruncatedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(http.ResponseWriter)
	}{
		{name: "oversized", body: func(w http.ResponseWriter) { _, _ = io.CopyN(w, zeroReader{}, maxSSHConfigBytes+1) }},
		{name: "truncated", body: func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "short")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.body(w) }))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "reach.conf")
			agent := Agent{cfg: Config{APIURL: server.URL, Token: "token", OutFile: out}, log: log.New(io.Discard, "", 0)}
			if err := agent.Sync(context.Background()); err == nil {
				t.Fatal("Sync accepted invalid response")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid response wrote config: %v", err)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestParseSSEAcceptsEventAboveDefaultScannerLimit(t *testing.T) {
	payload := strings.Repeat("x", 70*1024)
	var got string
	err := parseSSE(context.Background(), strings.NewReader("id: 1\nevent: test\ndata: "+payload+"\n\n"), func(ev Event) error {
		got = ev.Data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != payload {
		t.Fatalf("payload length = %d, want %d", len(got), len(payload))
	}
}

func TestParseSSERejectsAggregateEventAboveLimit(t *testing.T) {
	line := "data: " + strings.Repeat("x", 64*1024) + "\n"
	var body strings.Builder
	for body.Len() <= maxSSEEventBytes {
		body.WriteString(line)
	}
	err := parseSSE(context.Background(), strings.NewReader(body.String()), func(Event) error {
		t.Fatal("oversized aggregate event reached callback")
		return nil
	})
	if !errors.Is(err, errSSEEventTooLarge) {
		t.Fatalf("parseSSE error = %v, want %v", err, errSSEEventTooLarge)
	}
}

func TestRunBackoffGrowsAcrossRepeatedStreamFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	agent := Agent{
		cfg:          Config{OutFile: filepath.Join(t.TempDir(), "reach.conf")},
		log:          log.New(io.Discard, "", 0),
		syncOverride: func(context.Context) error { return nil },
		streamFn:     func(context.Context, *string) error { return errors.New("events unavailable") },
		backoffHook: func(delay time.Duration) {
			delays = append(delays, delay)
			if len(delays) == 3 {
				cancel()
			}
		},
	}
	_ = agent.Run(ctx)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v", delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
	}
}

func TestParseSSEStopsOnCallbackError(t *testing.T) {
	want := errors.New("stop")
	err := parseSSE(context.Background(), bytes.NewBufferString("id: 1\nevent: test\ndata: {}\n\nid: 2\nevent: test\ndata: {}\n\n"), func(Event) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("parseSSE error = %v", err)
	}
}
