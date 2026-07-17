package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

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
			if syncCalls.Add(1) == 1 {
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

func TestParseSSEStopsOnCallbackError(t *testing.T) {
	want := errors.New("stop")
	err := parseSSE(context.Background(), bytes.NewBufferString("id: 1\nevent: test\ndata: {}\n\nid: 2\nevent: test\ndata: {}\n\n"), func(Event) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("parseSSE error = %v", err)
	}
}
