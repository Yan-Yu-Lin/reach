package main

import (
	"database/sql"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	UpdatePolicyManual   = "manual"
	UpdatePolicyAuto     = "auto"
	UpdatePolicyDisabled = "disabled"
)

var (
	errNoAgentUpdateAvailable = errors.New("no agent update version configured")
	errUpdateAlreadyQueued    = errors.New("agent update already queued")
)

type agentUpdateRelease struct {
	Version string `json:"version"`
	APIURL  string `json:"api_url,omitempty"`
}

func normalizeUpdatePolicy(policy string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", UpdatePolicyManual:
		return UpdatePolicyManual, nil
	case UpdatePolicyAuto:
		return UpdatePolicyAuto, nil
	case UpdatePolicyDisabled:
		return UpdatePolicyDisabled, nil
	default:
		return "", fmt.Errorf("invalid update_policy %q", policy)
	}
}

func latestAgentUpdate(cfg Config) (agentUpdateRelease, bool) {
	if !cfg.AgentUpdates.Enabled {
		return agentUpdateRelease{}, false
	}
	rel := agentUpdateRelease{Version: strings.TrimSpace(cfg.AgentUpdates.LatestVersion), APIURL: strings.TrimSpace(cfg.AgentUpdates.APIURL)}
	if rel.Version == "" && cfg.AgentUpdates.ManifestPath != "" {
		if cached, ok := readManifestCached(cfg.AgentUpdates.ManifestPath); ok {
			rel = cached
		}
	}
	if rel.Version == "" {
		return agentUpdateRelease{}, false
	}
	if rel.APIURL == "" {
		rel.APIURL = strings.TrimSpace(cfg.DefaultHub.APIURL)
	}
	return rel, true
}

// manifestCache avoids re-reading latest.json on every heartbeat and for
// every machine in /machines (perf #7). The file is re-read when its mtime or
// size changes, and at most every manifestCacheTTL otherwise.
var manifestCache struct {
	sync.Mutex
	path    string
	mod     time.Time
	size    int64
	checked time.Time
	rel     agentUpdateRelease
	ok      bool
}

const manifestCacheTTL = 10 * time.Second

func readManifestCached(path string) (agentUpdateRelease, bool) {
	manifestCache.Lock()
	defer manifestCache.Unlock()
	now := time.Now()
	if manifestCache.path == path && now.Sub(manifestCache.checked) < manifestCacheTTL {
		return manifestCache.rel, manifestCache.ok
	}
	st, err := os.Stat(path)
	if err != nil {
		manifestCache.path, manifestCache.checked, manifestCache.ok = path, now, false
		manifestCache.rel = agentUpdateRelease{}
		return agentUpdateRelease{}, false
	}
	if manifestCache.path == path && st.ModTime().Equal(manifestCache.mod) && st.Size() == manifestCache.size {
		manifestCache.checked = now
		return manifestCache.rel, manifestCache.ok
	}
	var rel agentUpdateRelease
	ok := false
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &rel) == nil {
		rel.Version = strings.TrimSpace(rel.Version)
		rel.APIURL = strings.TrimSpace(rel.APIURL)
		ok = true
	}
	manifestCache.path, manifestCache.mod, manifestCache.size, manifestCache.checked = path, st.ModTime(), st.Size(), now
	manifestCache.rel, manifestCache.ok = rel, ok
	return rel, ok
}

func agentUpdateHintFor(cfg Config, current string) *AgentUpdateHint {
	rel, ok := latestAgentUpdate(cfg)
	if !ok {
		return nil
	}
	current = strings.TrimSpace(current)
	base := strings.TrimRight(rel.APIURL, "/") + "/downloads/reach-agent/v" + rel.Version
	return &AgentUpdateHint{Available: current != "" && current != rel.Version, Current: current, Latest: rel.Version, URL: base}
}

func updateAgentPayload(rel agentUpdateRelease) map[string]any {
	payload := map[string]any{"version": rel.Version}
	if rel.APIURL != "" {
		payload["api_url"] = rel.APIURL
	}
	return payload
}

func (s *Server) queueAgentUpdate(ctx context.Context, m Machine, actor, reason string) error {
	if m.UpdatePolicy == UpdatePolicyDisabled {
		return fmt.Errorf("agent updates are disabled for this machine")
	}
	rel, ok := latestAgentUpdate(s.cfg)
	if !ok {
		return errNoAgentUpdateAvailable
	}
	if actor == "" {
		actor = "system"
	}
	// One open update per machine, checked and inserted in one transaction
	// (daybreak round 4 P2-04). A manual update is an operator decision and
	// is journalled with it (astra R3-02, daybreak P2-05); the automatic
	// policy queue stays discardable (it is re-queued by the next heartbeat).
	err := s.store.inTx(ctx, func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_commands WHERE machine_id=? AND type='update_agent' AND status IN ('pending','sent') AND (expires_at IS NULL OR expires_at>?)`, m.ID, nowUTC()).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return errUpdateAlreadyQueued
		}
		if err := queueCommandTx(ctx, tx, m.ID, m.DesiredGeneration, "update_agent", updateAgentPayload(rel), 24*time.Hour); err != nil {
			return err
		}
		if actor != "system" {
			if _, err := recordDurable(ctx, tx, "machine.agent_update.queue", m.ID, actor, durableDone); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.store.Audit(ctx, "machine.agent_update.queue", actor, m.ID, "version="+rel.Version+" reason="+reason)
	s.publishEvent(ctx, "agent.update_queued", m.ID, map[string]any{"machine_id": m.ID, "version": rel.Version, "reason": reason})
	return nil
}

func (s *Server) hasOpenUpdateCommand(ctx context.Context, machineID string) bool {
	var id string
	err := s.store.db.QueryRowContext(ctx, `SELECT id FROM agent_commands WHERE machine_id=? AND type='update_agent' AND status IN ('pending','sent') AND (expires_at IS NULL OR expires_at>?) ORDER BY created_at DESC LIMIT 1`, machineID, nowUTC()).Scan(&id)
	return err == nil
}

func (s *Server) hasRecentUpdateCommand(ctx context.Context, machineID string, since time.Time) bool {
	var id string
	err := s.store.db.QueryRowContext(ctx, `SELECT id FROM agent_commands WHERE machine_id=? AND type='update_agent' AND created_at>? ORDER BY created_at DESC LIMIT 1`, machineID, since.UTC().Format(time.RFC3339)).Scan(&id)
	return err == nil
}

func (s *Server) maybeQueueAutoAgentUpdate(ctx context.Context, m Machine, hb AgentHeartbeat) {
	if m.UpdatePolicy != UpdatePolicyAuto || m.DesiredState == DesiredRetired || m.DesiredState == DesiredRetiring {
		return
	}
	if !hb.Capabilities.CanSelfUpdate {
		return
	}
	rel, ok := latestAgentUpdate(s.cfg)
	if !ok || strings.TrimSpace(hb.AgentVersion) == rel.Version {
		return
	}
	if s.hasRecentUpdateCommand(ctx, m.ID, time.Now().UTC().Add(-6*time.Hour)) {
		return
	}
	if err := s.queueAgentUpdate(ctx, m, "system", "auto_policy"); err != nil && !errors.Is(err, errUpdateAlreadyQueued) {
		log.Printf("queue auto agent update for %s: %v", m.ID, err)
	}
}
