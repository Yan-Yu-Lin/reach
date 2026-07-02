package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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
		if b, err := os.ReadFile(cfg.AgentUpdates.ManifestPath); err == nil {
			_ = json.Unmarshal(b, &rel)
			rel.Version = strings.TrimSpace(rel.Version)
			rel.APIURL = strings.TrimSpace(rel.APIURL)
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
	if s.hasOpenUpdateCommand(ctx, m.ID) {
		return errUpdateAlreadyQueued
	}
	if err := s.prov.QueueCommand(ctx, m.ID, m.DesiredGeneration, "update_agent", updateAgentPayload(rel), 24*time.Hour); err != nil {
		return err
	}
	if actor == "" {
		actor = "system"
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
