package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHeartbeatRetainsResultsUntilSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"command_id":"cmd_one"`)) {
			t.Errorf("heartbeat missing pending command result: %s", body)
		}
		if calls.Add(1) <= 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"heartbeat":{},"desired_config":{},"commands":[]}`)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = true
	cfg.Heartbeat.APIURL = server.URL
	cfg.Heartbeat.Token = "token"
	d := NewDaemon(cfg)
	d.logger = log.New(io.Discard, "", 0)
	d.queueResult(CommandResult{CommandID: "cmd_one", Status: "acked"})
	if _, err := d.sendHeartbeat(context.Background(), nil); err == nil {
		t.Fatal("first heartbeat unexpectedly succeeded")
	}
	if len(d.pendingResults) != 1 {
		t.Fatalf("pending results after failure = %v", d.pendingResults)
	}
	if _, err := d.sendHeartbeat(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(d.pendingResults) != 0 {
		t.Fatalf("pending results after success = %v", d.pendingResults)
	}
}

func TestCommandLedgerBoundsRetention(t *testing.T) {
	cfg := defaultConfig()
	cfg.Install.DataDir = t.TempDir()
	d := NewDaemon(cfg)
	d.logger = log.New(io.Discard, "", 0)
	for i := 0; i < commandLedgerLimit+10; i++ {
		result := CommandResult{CommandID: fmt.Sprintf("cmd_%03d", i), Status: "acked"}
		if err := d.rememberCommandResult(result); err != nil {
			t.Fatal(err)
		}
	}

	restarted := NewDaemon(cfg)
	if len(restarted.completedCommands) != commandLedgerLimit || len(restarted.completedOrder) != commandLedgerLimit {
		t.Fatalf("loaded ledger size = %d/%d, want %d", len(restarted.completedCommands), len(restarted.completedOrder), commandLedgerLimit)
	}
	if _, retained := restarted.completedCommands["cmd_000"]; retained {
		t.Fatal("oldest command was not pruned")
	}
	if _, retained := restarted.completedCommands[fmt.Sprintf("cmd_%03d", commandLedgerLimit+9)]; !retained {
		t.Fatal("newest command was pruned")
	}
}

func TestExecutingCommandIsNotReexecutedAfterRestart(t *testing.T) {
	cfg := defaultConfig()
	cfg.Install.DataDir = t.TempDir()
	d := NewDaemon(cfg)
	if err := d.rememberCommandResult(CommandResult{CommandID: "cmd_interrupted", Status: "executing"}); err != nil {
		t.Fatal(err)
	}

	restarted := NewDaemon(cfg)
	restarted.logger = log.New(io.Discard, "", 0)
	restarted.applyCommand(context.Background(), AgentCommand{ID: "cmd_interrupted", Type: "unknown"})
	if len(restarted.pendingResults) != 1 {
		t.Fatalf("pending results = %#v", restarted.pendingResults)
	}
	got := restarted.pendingResults[0]
	if got.Status != "failed" || !strings.Contains(got.Message, "interrupted") {
		t.Fatalf("interrupted command result = %#v", got)
	}
}

func TestCommandLedgerPreventsReexecutionAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	cfg := defaultConfig()
	cfg.Install.DataDir = dataDir
	d := NewDaemon(cfg)
	d.logger = log.New(io.Discard, "", 0)
	result := CommandResult{CommandID: "cmd_danger", Status: "acked", Message: "done"}
	if err := d.rememberCommandResult(result); err != nil {
		t.Fatal(err)
	}

	restarted := NewDaemon(cfg)
	restarted.logger = log.New(io.Discard, "", 0)
	loaded, ok := restarted.completedCommands["cmd_danger"]
	if !ok || loaded.Message != "done" {
		t.Fatalf("ledger did not survive restart: %#v", restarted.completedCommands)
	}
	restarted.applyCommand(context.Background(), AgentCommand{ID: "cmd_danger", Type: "unknown"})
	if len(restarted.pendingResults) != 1 || restarted.pendingResults[0].Status != "acked" || restarted.pendingResults[0].Message != "done" {
		t.Fatalf("duplicate command was re-executed: %#v", restarted.pendingResults)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "command-ledger.json")); err != nil {
		t.Fatal(err)
	}
}
