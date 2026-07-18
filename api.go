package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg       Config
	store     *Store
	prov      *Provisioner
	mux       *http.ServeMux
	events    *eventBroker
	registerL *rateLimiter
	loginL    *rateLimiter
}

func NewServer(cfg Config, store *Store, prov *Provisioner) *Server {
	s := &Server{cfg: cfg, store: store, prov: prov, mux: http.NewServeMux(), events: newEventBroker(store), registerL: newPersistentRateLimiter(store.db, "client_register", 3, time.Minute), loginL: newPersistentRateLimiter(store.db, "admin_login", 8, time.Minute)}
	if prov != nil {
		prov.publishEvent = func(typ, machineID string, data map[string]any) {
			s.publishEvent(context.Background(), typ, machineID, data)
		}
		prov.pruneEvents = s.events.PruneBefore
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if s.allowedCORSOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	}
}

func (s *Server) allowedCORSOrigin(origin string) bool {
	if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
		return true
	}
	if origin == originFromURL(s.cfg.DefaultHub.APIURL) {
		return true
	}
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.TrimRight(allowed, "/") == origin {
			return true
		}
	}
	return false
}

func originFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/client/register", s.method("POST", s.handleClientRegister))
	s.mux.HandleFunc("/api/client/register/", s.method("POST", s.handleClientPoll))
	s.mux.HandleFunc("/api/client/provision", s.method("POST", s.handleClientProvision))
	s.mux.HandleFunc("/api/client/agent/heartbeat", s.method("POST", s.handleAgentHeartbeat))
	s.mux.HandleFunc("/api/client/agent/uninstall-intent", s.method("POST", s.handleAgentUninstallIntent))
	s.mux.HandleFunc("/api/client/agent/uninstall-complete", s.method("POST", s.handleAgentUninstallComplete))
	s.mux.HandleFunc("/api/admin/login", s.method("POST", s.handleAdminLogin))
	s.mux.HandleFunc("/api/admin/logout", s.admin(s.method("POST", s.handleAdminLogout)))
	s.mux.HandleFunc("/api/admin/service-tokens", s.admin(s.method("POST", s.handleCreateServiceToken)))
	s.mux.HandleFunc("/api/admin/requests", s.admin(s.method("GET", s.handleAdminRequests)))
	s.mux.HandleFunc("/api/admin/requests/", s.admin(s.handleAdminRequestAction))
	s.mux.HandleFunc("/api/admin/machines", s.admin(s.method("GET", s.handleAdminMachines)))
	s.mux.HandleFunc("/api/admin/machines/", s.admin(s.handleAdminMachineAction))
	s.mux.HandleFunc("/api/admin/ssh-config", s.admin(s.method("GET", s.handleSSHConfig)))
	s.mux.HandleFunc("/api/admin/events", s.admin(s.method("GET", s.handleAdminEvents)))
	s.mux.HandleFunc("/api/admin/health", s.admin(s.method("GET", s.handleHealth)))
}

type handlerFunc func(http.ResponseWriter, *http.Request)

func (s *Server) method(method string, h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

func (s *Server) admin(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		claims, kind, err := s.authenticateAdminToken(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		if !adminRoleAllowed(claims.Role, r.URL.Path) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), ctxClaimsKey{}, claims)
		ctx = context.WithValue(ctx, ctxTokenKey{}, token)
		ctx = context.WithValue(ctx, ctxAuthKindKey{}, kind)
		h(w, r.WithContext(ctx))
	}
}

type ctxClaimsKey struct{}
type ctxTokenKey struct{}
type ctxAuthKindKey struct{}

func claimsFrom(ctx context.Context) *Claims { c, _ := ctx.Value(ctxClaimsKey{}).(*Claims); return c }
func tokenFrom(ctx context.Context) string   { t, _ := ctx.Value(ctxTokenKey{}).(string); return t }
func authKindFrom(ctx context.Context) string {
	k, _ := ctx.Value(ctxAuthKindKey{}).(string)
	return k
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
func writeClientAuthErr(w http.ResponseWriter) {
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) publishEvent(ctx context.Context, typ, machineID string, data map[string]any) {
	if s.events != nil {
		base := context.Background()
		if ctx != nil {
			base = context.WithoutCancel(ctx)
		}
		publishCtx, cancel := context.WithTimeout(base, eventPublishTimeout)
		defer cancel()
		published := s.events.Publish(publishCtx, typ, machineID, data)
		if published.Type == "machine.resync_required" && typ != published.Type {
			reason, _ := published.Data["reason"].(string)
			log.Printf("event publish failed: type=%s machine_id=%s reason=%s", typ, machineID, reason)
		}
	}
}

func (s *Server) handleClientRegister(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.registerL.allow(ip) {
		writeErr(w, 429, "rate limit exceeded")
		return
	}
	var req struct {
		Name       string         `json:"name"`
		AuthCode   string         `json:"auth_code"`
		TargetUser string         `json:"target_user"`
		Slug       string         `json:"slug"`
		Mode       string         `json:"mode"`
		Distro     string         `json:"distro"`
		Arch       string         `json:"arch"`
		Transport  string         `json:"transport"`
		LocalPort  int            `json:"local_port"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, 400, "name is required")
		return
	}
	if len(name) > 64 {
		writeErr(w, 400, "name is too long")
		return
	}
	for field, value := range map[string]string{"target_user": req.TargetUser, "slug": req.Slug, "mode": req.Mode, "distro": req.Distro, "arch": req.Arch, "transport": req.Transport} {
		if len(strings.TrimSpace(value)) > 256 {
			writeErr(w, 400, field+" is too long")
			return
		}
	}
	if err := validateMetadataLimits(req.Metadata); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	clientSecret, err := RandomToken("cs", 24)
	if err != nil {
		writeErr(w, 500, "could not generate client secret")
		return
	}
	clientHash, err := HashSecret(clientSecret)
	if err != nil {
		writeErr(w, 500, "could not hash client secret")
		return
	}
	requestID, err := RandomID("r")
	if err != nil {
		writeErr(w, 500, "could not generate request id")
		return
	}
	now := time.Now().UTC()
	expires := now.Add(s.cfg.RequestTTL).Format(time.RFC3339)
	status := "pending"
	var setupToken string
	var setupHash, setupExpires, approvedAt, inviteHash any
	meta := map[string]any{"name": name, "target_user": req.TargetUser, "slug": req.Slug, "mode": req.Mode, "local_port": req.LocalPort, "distro": req.Distro, "arch": req.Arch, "transport": req.Transport, "source_ip": ip}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	metaJSON, _ := json.Marshal(meta)
	if len(metaJSON) > 4096 {
		writeErr(w, 400, "metadata is too large")
		return
	}
	if req.AuthCode != "" {
		ok, kind, matchedHash := s.validateClientAuth(r.Context(), req.AuthCode)
		if !ok {
			writeClientAuthErr(w)
			return
		}
		status = "approved"
		setupToken, err = RandomToken("st", 24)
		if err != nil {
			writeErr(w, 500, "could not generate setup token")
			return
		}
		h, err := HashSecret(setupToken)
		if err != nil {
			writeErr(w, 500, "could not hash setup token")
			return
		}
		setupHash = h
		setupExpires = now.Add(s.cfg.SetupTokenTTL).Format(time.RFC3339)
		approvedAt = now.Format(time.RFC3339)
		if kind == "friend" {
			inviteHash = matchedHash
		}
	}
	_, err = s.store.db.ExecContext(r.Context(), `INSERT INTO requests (id,name,client_secret_hash,status,invite_code_hash,setup_token_hash,setup_token_expires_at,metadata_json,created_at,expires_at,approved_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, requestID, name, clientHash, status, inviteHash, setupHash, setupExpires, string(metaJSON), now.Format(time.RFC3339), expires, approvedAt)
	if err != nil {
		log.Printf("register insert: %v", err)
		writeErr(w, 500, "registration failed")
		return
	}
	s.publishEvent(r.Context(), "request.created", "", map[string]any{"request_id": requestID, "name": name, "status": status})
	if status == "pending" {
		s.notifyPendingRequest(requestID, name, meta)
	}
	resp := map[string]any{"request_id": requestID, "client_secret": clientSecret, "status": status, "expires_at": expires}
	if setupToken != "" {
		resp["setup_token"] = setupToken
		resp["setup_token_expires_at"] = setupExpires
	}
	writeJSON(w, 200, resp)
}

func (s *Server) validateClientAuth(ctx context.Context, code string) (bool, string, string) {
	if s.cfg.GodCodeHash != "" && VerifySecret(code, s.cfg.GodCodeHash) {
		return true, "god", ""
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT code_hash,type,expires_at,used_by FROM invites WHERE used_by IS NULL`)
	if err != nil {
		return false, "", ""
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var hash, typ, exp string
		var used sql.NullString
		if rows.Scan(&hash, &typ, &exp, &used) != nil {
			continue
		}
		et, err := time.Parse(time.RFC3339, exp)
		if err != nil || now.After(et) {
			continue
		}
		if VerifySecret(code, hash) {
			return true, typ, hash
		}
	}
	return false, "", ""
}

func (s *Server) handleClientPoll(w http.ResponseWriter, r *http.Request) {
	id, ok := pathAction(r.URL.Path, "/api/client/register/", "/poll")
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	var req struct {
		ClientSecret string `json:"client_secret"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	var status, secretHash, expiresAt string
	var setupHash, setupExpires sql.NullString
	err := s.store.db.QueryRowContext(r.Context(), `SELECT status,client_secret_hash,expires_at,setup_token_hash,setup_token_expires_at FROM requests WHERE id=?`, id).Scan(&status, &secretHash, &expiresAt, &setupHash, &setupExpires)
	if errors.Is(err, sql.ErrNoRows) {
		writeClientAuthErr(w)
		return
	}
	if err != nil {
		writeErr(w, 500, "poll failed")
		return
	}
	if !VerifySecret(req.ClientSecret, secretHash) {
		writeClientAuthErr(w)
		return
	}
	if t, err := time.Parse(time.RFC3339, expiresAt); err == nil && time.Now().UTC().After(t) && status == "pending" {
		status = "expired"
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='expired' WHERE id=?`, id)
		s.publishEvent(r.Context(), "request.expired", "", map[string]any{"request_id": id})
	}
	resp := map[string]any{"request_id": id, "status": status}
	if status == "approved" {
		if setupHash.Valid && setupExpires.Valid {
			resp["setup_token_expires_at"] = setupExpires.String
		} else {
			setupToken, err := RandomToken("st", 24)
			if err != nil {
				writeErr(w, 500, "could not generate setup token")
				return
			}
			h, err := HashSecret(setupToken)
			if err != nil {
				writeErr(w, 500, "could not hash setup token")
				return
			}
			exp := time.Now().UTC().Add(s.cfg.SetupTokenTTL).Format(time.RFC3339)
			if _, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET setup_token_hash=?, setup_token_expires_at=? WHERE id=? AND status='approved' AND setup_token_hash IS NULL`, h, exp, id); err != nil {
				writeErr(w, 500, "could not persist setup token")
				return
			}
			resp["setup_token"] = setupToken
			resp["setup_token_expires_at"] = exp
		}
	} else if setupExpires.Valid {
		resp["setup_token_expires_at"] = setupExpires.String
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleClientProvision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupToken  string `json:"setup_token"`
		RequestID   string `json:"request_id"`
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		TargetUser  string `json:"target_user"`
		Pubkey      string `json:"pubkey"`
		Mode        string `json:"mode"`
		Persistence string `json:"persistence"`
		Distro      string `json:"distro"`
		Arch        string `json:"arch"`
		LocalPort   int    `json:"local_port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if len(strings.TrimSpace(req.DisplayName)) > 256 {
		writeErr(w, 400, "display_name is too long")
		return
	}
	if req.RequestID == "" || req.SetupToken == "" || req.Pubkey == "" {
		writeErr(w, 400, "request_id, setup_token and pubkey are required")
		return
	}
	if err := s.validateApprovedMetadata(r.Context(), req.RequestID, map[string]any{"target_user": req.TargetUser, "slug": req.Slug, "mode": req.Mode, "local_port": req.LocalPort, "distro": req.Distro, "arch": req.Arch}); err != nil {
		writeClientAuthErr(w)
		return
	}
	requestID, name, err := s.consumeSetupToken(r.Context(), req.RequestID, req.SetupToken)
	if err != nil {
		writeClientAuthErr(w)
		return
	}
	if req.Slug == "" {
		req.Slug = NormalizeSlug(name)
	}
	if err := ValidateSlug(req.Slug); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	owner, err := s.firstAdminID(r.Context())
	if err != nil {
		writeErr(w, 500, "no admin user configured")
		return
	}
	res, err := s.prov.CreateMachine(r.Context(), CreateMachineInput{Slug: req.Slug, DisplayName: firstNonEmpty(req.DisplayName, name), TargetUser: req.TargetUser, Pubkey: req.Pubkey, Mode: req.Mode, LocalPort: req.LocalPort, Persistence: req.Persistence, Distro: req.Distro, Arch: req.Arch, OwnerUserID: owner})
	if err != nil {
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='approved' WHERE id=? AND status='provisioning'`, requestID)
		writeErr(w, 500, err.Error())
		return
	}
	_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='provisioned' WHERE id=?`, requestID)
	_, _ = s.store.db.ExecContext(r.Context(), `UPDATE invites SET used_by=? WHERE code_hash=(SELECT invite_code_hash FROM requests WHERE id=? AND invite_code_hash IS NOT NULL)`, res.Machine.ID, requestID)
	keys, err := s.activeAccessKeys(r.Context())
	if err != nil {
		writeErr(w, 500, "could not load admin public keys")
		return
	}
	hostKeys := s.cfg.HubHostKeys
	if hostKeys == nil {
		hostKeys = []string{}
	}
	resp := map[string]any{"machine": res.Machine, "tunnel": res.Tunnel, "hub": res.Hub, "agent_token": res.AgentToken, "admin_pubkeys": keys, "hub_host_keys": hostKeys, "jason_host_keys": hostKeys}
	if s.cfg.WebSocketTunnel.Enabled {
		carrier := s.cfg.WebSocketTunnel.Carrier
		if carrier == "" {
			carrier = "reach"
		}
		resp["websocket_tunnel"] = map[string]any{"enabled": true, "carrier": carrier, "url": s.cfg.WebSocketTunnel.URL, "path_prefix": s.cfg.WebSocketTunnel.PathPrefix, "binaries": s.cfg.WebSocketTunnel.Binaries}
	}
	s.publishEvent(r.Context(), "request.provisioned", res.Machine.ID, map[string]any{"request_id": requestID, "machine_id": res.Machine.ID, "slug": res.Machine.Slug})
	s.publishEvent(r.Context(), "machine.created", res.Machine.ID, map[string]any{"machine_id": res.Machine.ID, "slug": res.Machine.Slug})
	s.publishEvent(r.Context(), "machine.desired_changed", res.Machine.ID, map[string]any{"machine_id": res.Machine.ID, "desired_state": DesiredActive, "generation": res.Machine.DesiredGeneration})
	writeJSON(w, 200, resp)
}

func (s *Server) validateApprovedMetadata(ctx context.Context, requestID string, got map[string]any) error {
	var raw sql.NullString
	if err := s.store.db.QueryRowContext(ctx, `SELECT metadata_json FROM requests WHERE id=?`, requestID).Scan(&raw); err != nil {
		return nil
	}
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var want map[string]any
	if json.Unmarshal([]byte(raw.String), &want) != nil {
		return nil
	}
	for _, key := range []string{"target_user", "slug", "mode", "distro", "arch"} {
		wv, wok := want[key].(string)
		gv, _ := got[key].(string)
		if wok && strings.TrimSpace(wv) != "" && strings.TrimSpace(gv) != "" && strings.TrimSpace(wv) != strings.TrimSpace(gv) {
			return fmt.Errorf("provision %s does not match approved request", key)
		}
	}
	if wv, ok := want["local_port"].(float64); ok && int(wv) != 0 {
		if gv, _ := got["local_port"].(int); gv != 0 && gv != int(wv) {
			return fmt.Errorf("provision local_port does not match approved request")
		}
	}
	return nil
}

func (s *Server) consumeSetupToken(ctx context.Context, requestID, token string) (string, string, error) {
	var id, name, status string
	var hash, exp sql.NullString
	err := s.store.db.QueryRowContext(ctx, `SELECT id,name,status,setup_token_hash,setup_token_expires_at FROM requests WHERE id=?`, requestID).Scan(&id, &name, &status, &hash, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("request not found")
	}
	if err != nil {
		return "", "", err
	}
	if status != "approved" || !hash.Valid || !VerifySecret(token, hash.String) {
		return "", "", errors.New("invalid setup token")
	}
	if exp.Valid {
		if t, err := time.Parse(time.RFC3339, exp.String); err == nil && time.Now().UTC().After(t) {
			return "", "", errors.New("setup token expired")
		}
	}
	res, err := s.store.db.ExecContext(ctx, `UPDATE requests SET status='provisioning', setup_token_hash=NULL WHERE id=? AND status='approved' AND setup_token_hash=?`, requestID, hash.String)
	if err != nil {
		return "", "", err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", "", errors.New("setup token already used")
	}
	return id, name, nil
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	requested := eventCursor(r)
	if requested.Present && !requested.Valid {
		_ = writeSSE(w, ReachEvent{Type: "machine.resync_required", Data: map[string]any{"reason": "invalid_cursor"}, CreatedAt: nowUTC()})
		flusher.Flush()
		return
	}
	ch, _, _, replay, reason, cancel, err := s.events.SubscribeSince(r.Context(), requested.ID, requested.Present, eventReplayLimit)
	if err != nil {
		_ = writeSSE(w, ReachEvent{Type: "machine.resync_required", Data: map[string]any{"reason": "replay_failed"}, CreatedAt: nowUTC()})
		flusher.Flush()
		return
	}
	defer cancel()
	if reason != "" {
		_ = writeSSE(w, ReachEvent{Type: "machine.resync_required", Data: map[string]any{"reason": reason}, CreatedAt: nowUTC()})
		flusher.Flush()
		return
	}
	cursor := requested.ID
	for _, ev := range replay {
		if err := writeSSE(w, ev); err != nil {
			return
		}
		if id, err := strconv.ParseInt(ev.ID, 10, 64); err == nil {
			cursor = id
		}
	}
	_ = writeSSE(w, ReachEvent{Type: "hello", Data: map[string]any{"ok": true}, CreatedAt: nowUTC()})
	flusher.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			id, err := strconv.ParseInt(ev.ID, 10, 64)
			if err == nil && id <= cursor {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
			if ev.Type == "machine.resync_required" {
				return
			}
			if err == nil {
				cursor = id
			}
		case <-tick.C:
			if err := writeSSEComment(w, "keepalive"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	token, hb, raw, ok := s.authAgentHeartbeat(w, r)
	if !ok {
		_ = token
		return
	}
	m, tunnels, err := s.prov.getMachineAndTunnels(r.Context(), hb.MachineID)
	if err != nil {
		writeClientAuthErr(w)
		return
	}
	if err := s.verifyAgentToken(r.Context(), hb.MachineID, token, true); err != nil {
		writeClientAuthErr(w)
		return
	}
	hb = normalizeHeartbeat(hb)
	s.storeCommandResults(r.Context(), hb)
	now := nowUTC()
	var previousRaw sql.NullString
	_ = s.store.db.QueryRowContext(r.Context(), `SELECT raw_json FROM agent_observations WHERE machine_id=?`, hb.MachineID).Scan(&previousRaw)
	previousSSHOptions := sshOptionsSignature(previousRaw.String)
	pid := nullableInt(hb.Observed.Tunnel.PID)
	fps, _ := json.Marshal(hb.Observed.Keys.InstalledAdminKeyFingerprints)
	_, err = s.store.db.ExecContext(r.Context(), `INSERT INTO agent_observations (machine_id,agent_version,install_id,applied_generation,heartbeat_at,agent_state,transport,transport_state,local_ssh_state,local_ssh_error,tunnel_state,tunnel_pid,tunnel_error,persistence_backend,persistence_quality,persistence_reboot_safe,authorized_key_fingerprints,last_error,raw_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(machine_id) DO UPDATE SET agent_version=excluded.agent_version,install_id=excluded.install_id,applied_generation=excluded.applied_generation,heartbeat_at=excluded.heartbeat_at,agent_state=excluded.agent_state,transport=excluded.transport,transport_state=excluded.transport_state,local_ssh_state=excluded.local_ssh_state,local_ssh_error=excluded.local_ssh_error,tunnel_state=excluded.tunnel_state,tunnel_pid=excluded.tunnel_pid,tunnel_error=excluded.tunnel_error,persistence_backend=excluded.persistence_backend,persistence_quality=excluded.persistence_quality,persistence_reboot_safe=excluded.persistence_reboot_safe,authorized_key_fingerprints=excluded.authorized_key_fingerprints,last_error=excluded.last_error,raw_json=excluded.raw_json`, hb.MachineID, hb.AgentVersion, hb.InstallID, hb.AppliedGeneration, now, hb.Observed.AgentState, hb.Observed.Tunnel.Transport, "", hb.Observed.LocalSSH.State, nullable(hb.Observed.LocalSSH.Error), hb.Observed.Tunnel.State, pid, nullable(hb.Observed.Tunnel.Error), hb.Observed.Persistence.Backend, hb.Observed.Persistence.Quality, boolInt(hb.Observed.Persistence.RebootSafe), string(fps), nullable(hb.LastError), string(raw))
	if err != nil {
		writeErr(w, 500, "could not store heartbeat")
		return
	}
	_, _ = s.store.db.ExecContext(r.Context(), `INSERT INTO agent_status (machine_id,agent_version,transport,transport_state,local_ssh_state,local_ssh_error,tunnel_state,tunnel_pid,tunnel_error,last_error,raw_json,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(machine_id) DO UPDATE SET agent_version=excluded.agent_version,transport=excluded.transport,transport_state=excluded.transport_state,local_ssh_state=excluded.local_ssh_state,local_ssh_error=excluded.local_ssh_error,tunnel_state=excluded.tunnel_state,tunnel_pid=excluded.tunnel_pid,tunnel_error=excluded.tunnel_error,last_error=excluded.last_error,raw_json=excluded.raw_json,updated_at=excluded.updated_at`, hb.MachineID, hb.AgentVersion, hb.Observed.Tunnel.Transport, "", hb.Observed.LocalSSH.State, nullable(hb.Observed.LocalSSH.Error), hb.Observed.Tunnel.State, pid, nullable(hb.Observed.Tunnel.Error), nullable(hb.LastError), string(raw), now)
	if hb.Observed.Tunnel.State == "connected" || hb.Observed.Tunnel.State == "running" {
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE tunnels SET status='online', last_seen_at=? WHERE machine_id=? AND status NOT IN ('disabled','retired','failed')`, now, hb.MachineID)
	}
	old := m.ObservedState
	_, _, _, err = s.prov.recomputeObserved(r.Context(), hb.MachineID)
	if err != nil {
		writeErr(w, 500, "could not recompute machine state")
		return
	}
	m, err = s.prov.GetMachine(r.Context(), hb.MachineID)
	if err != nil {
		writeErr(w, 500, "could not load machine state")
		return
	}
	if old != m.ObservedState {
		s.publishEvent(r.Context(), "machine.observed_changed", m.ID, map[string]any{"machine_id": m.ID, "observed_state": m.ObservedState, "status": m.Status})
		s.publishEvent(r.Context(), "machine."+m.ObservedState, m.ID, map[string]any{"machine_id": m.ID, "slug": m.Slug})
	}
	if previousSSHOptions != sshOptionsSignature(string(raw)) {
		s.publishEvent(r.Context(), "ssh_config.changed", m.ID, map[string]any{"reason": "ssh_compat_changed", "machine_id": m.ID})
	}
	s.maybeQueueAutoAgentUpdate(r.Context(), m, hb)
	cmds, err := s.pendingCommands(r.Context(), m.ID)
	if err != nil {
		writeErr(w, 500, "could not load pending commands")
		return
	}
	resp := HeartbeatResponse{OK: true, ServerTime: now, DesiredGeneration: m.DesiredGeneration, DesiredState: m.DesiredState, Heartbeat: HeartbeatPolicy{NextIntervalSeconds: int(s.cfg.HealthInterval.Seconds()), OfflineAfterSeconds: int(s.cfg.OfflineAfter.Seconds())}, DesiredConfig: s.desiredConfig(r.Context(), m, tunnels), Commands: cmds, Update: agentUpdateHintFor(s.cfg, hb.AgentVersion)}
	writeJSON(w, 200, resp)
	s.publishEvent(r.Context(), "agent.heartbeat", m.ID, map[string]any{"machine_id": m.ID, "heartbeat_at": now})
}

func normalizeHeartbeat(hb AgentHeartbeat) AgentHeartbeat {
	if hb.Observed.LocalSSH.State == "" && hb.LocalSSH.State != "" {
		hb.Observed.LocalSSH = hb.LocalSSH
	}
	if hb.Observed.Tunnel.State == "" && hb.Tunnel.State != "" {
		hb.Observed.Tunnel = hb.Tunnel
		hb.Observed.Tunnel.Transport = hb.Transport
	}
	if hb.Observed.AgentState == "" {
		hb.Observed.AgentState = "running"
	}
	return hb
}

func (s *Server) authAgentHeartbeat(w http.ResponseWriter, r *http.Request) (string, AgentHeartbeat, []byte, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeClientAuthErr(w)
		return "", AgentHeartbeat{}, nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "invalid body")
		return "", AgentHeartbeat{}, nil, false
	}
	var hb AgentHeartbeat
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hb); err != nil {
		writeErr(w, 400, "invalid json")
		return "", hb, raw, false
	}
	if hb.MachineID == "" {
		writeErr(w, 400, "machine_id is required")
		return "", hb, raw, false
	}
	return strings.TrimPrefix(auth, "Bearer "), hb, raw, true
}

func (s *Server) verifyAgentToken(ctx context.Context, machineID, token string, allowRetired bool) error {
	q := `SELECT agent_token_hash FROM machines WHERE id=?`
	if !allowRetired {
		q += ` AND desired_state NOT IN ('retired')`
	}
	var hash sql.NullString
	err := s.store.db.QueryRowContext(ctx, q, machineID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("machine not found")
	}
	if err != nil {
		return err
	}
	if !hash.Valid || !VerifySecret(token, hash.String) {
		return errors.New("invalid token")
	}
	return nil
}

func (s *Server) storeCommandResults(ctx context.Context, hb AgentHeartbeat) {
	for _, res := range hb.CommandResults {
		if res.CommandID == "" {
			continue
		}
		status := "acked"
		if res.Status == "failed" {
			status = "failed"
		}
		_, _ = s.store.db.ExecContext(ctx, `UPDATE agent_commands SET status=?, acked_at=?, last_error=? WHERE id=? AND machine_id=?`, status, nowUTC(), nullable(res.Message), res.CommandID, hb.MachineID)
		s.publishEvent(ctx, "agent.command_"+status, hb.MachineID, map[string]any{"machine_id": hb.MachineID, "command_id": res.CommandID, "status": status, "message": res.Message})
	}
}

const commandRetryLease = 30 * time.Second

func (s *Server) pendingCommands(ctx context.Context, machineID string) ([]AgentCommandDelivery, error) {
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	retryBefore := now.Add(-commandRetryLease).Format(time.RFC3339)
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE agent_commands SET status='sent',sent_at=?
		WHERE id IN (
			SELECT id FROM agent_commands
			WHERE machine_id=? AND (status='pending' OR (status='sent' AND (sent_at IS NULL OR sent_at<=?)))
			AND (expires_at IS NULL OR expires_at>?)
			ORDER BY created_at,id LIMIT 10
		) AND (status='pending' OR (status='sent' AND (sent_at IS NULL OR sent_at<=?)))
		RETURNING id,generation,type,payload_json,expires_at,created_at`, nowText, machineID, retryBefore, nowText, retryBefore)
	if err != nil {
		return nil, err
	}
	var out []AgentCommandDelivery
	var createdByID = map[string]string{}
	for rows.Next() {
		var c AgentCommandDelivery
		var payload, exp sql.NullString
		var created string
		if err := rows.Scan(&c.ID, &c.Generation, &c.Type, &payload, &exp, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if payload.Valid {
			c.Payload = json.RawMessage(payload.String)
		}
		c.ExpiresAt = exp.String
		createdByID[c.ID] = created
		out = append(out, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return out, nil
	}
	sort.Slice(out, func(i, j int) bool {
		if createdByID[out[i].ID] == createdByID[out[j].ID] {
			return out[i].ID < out[j].ID
		}
		return createdByID[out[i].ID] < createdByID[out[j].ID]
	})
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) desiredConfig(ctx context.Context, m Machine, tunnels []Tunnel) DesiredAgentConfig {
	cfg := DesiredAgentConfig{TunnelEnabled: m.DesiredState == DesiredActive, TransportPolicy: "auto", AdminKeysGeneration: 1, AdminPubkeys: s.desiredPubkeys(ctx), ProcessTitleConfig: m.ProcessTitleConfig}
	if len(tunnels) > 0 {
		t := tunnels[0]
		cfg.RemotePort = t.RemotePort
		cfg.TunnelUser = t.UnixUser
		var h Hub
		if err := s.store.db.QueryRowContext(ctx, `SELECT id,name,public_host,ssh_port,proxyjump_alias,api_url,port_range_start,port_range_end,status,created_at FROM hubs WHERE id=?`, t.HubID).Scan(&h.ID, &h.Name, &h.PublicHost, &h.SSHPort, &h.ProxyJumpAlias, &h.APIURL, &h.PortStart, &h.PortEnd, &h.Status, &h.CreatedAt); err == nil {
			cfg.HubHost = h.PublicHost
			cfg.HubSSHPort = h.SSHPort
		}
	}
	return cfg
}

func (s *Server) desiredPubkeys(ctx context.Context) []DesiredPubkey {
	rows, err := s.store.db.QueryContext(ctx, `SELECT id,label,public_key FROM access_keys WHERE revoked_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DesiredPubkey
	for rows.Next() {
		var id, label, key string
		if rows.Scan(&id, &label, &key) == nil {
			out = append(out, DesiredPubkey{ID: id, Label: label, Fingerprint: Fingerprint(key), PublicKey: key})
		}
	}
	return out
}

func (s *Server) handleAgentUninstallIntent(w http.ResponseWriter, r *http.Request) {
	s.handleAgentUninstall(w, r, false)
}
func (s *Server) handleAgentUninstallComplete(w http.ResponseWriter, r *http.Request) {
	s.handleAgentUninstall(w, r, true)
}
func (s *Server) handleAgentUninstall(w http.ResponseWriter, r *http.Request, complete bool) {
	var req struct {
		MachineID string `json:"machine_id"`
		Reason    string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeClientAuthErr(w)
		return
	}
	if err := s.verifyAgentToken(r.Context(), req.MachineID, strings.TrimPrefix(auth, "Bearer "), true); err != nil {
		writeClientAuthErr(w)
		return
	}
	if complete {
		_ = s.prov.RetireMachineNow(r.Context(), req.MachineID, "agent-uninstall")
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE machines SET cleanup_state='complete' WHERE id=?`, req.MachineID)
		s.publishEvent(r.Context(), "machine.retired", req.MachineID, map[string]any{"machine_id": req.MachineID})
	} else {
		_ = s.prov.RemoveMachine(r.Context(), req.MachineID, "agent-uninstall")
		s.publishEvent(r.Context(), "machine.retiring", req.MachineID, map[string]any{"machine_id": req.MachineID, "reason": req.Reason})
	}
	s.publishEvent(r.Context(), "ssh_config.changed", req.MachineID, map[string]any{"machine_id": req.MachineID})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginL.allow(clientIP(r)) {
		writeErr(w, 429, "rate limit exceeded")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	var id, username, hash, role string
	err := s.store.db.QueryRowContext(r.Context(), `SELECT id,username,password_hash,role FROM users WHERE username=?`, req.Username).Scan(&id, &username, &hash, &role)
	if err != nil || !VerifySecret(req.Password, hash) {
		writeErr(w, 401, "invalid username or password")
		return
	}
	tok, err := SignJWT(s.cfg.JWTSecret, id, username, role, s.cfg.AdminJWTTTL)
	if err != nil {
		writeErr(w, 500, "could not sign token")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires_in": int64(s.cfg.AdminJWTTTL / time.Second)})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r.Context())
	claims := claimsFrom(r.Context())
	if tok == "" || claims == nil {
		writeErr(w, 400, "missing token")
		return
	}
	var err error
	if authKindFrom(r.Context()) == "service" {
		err = s.revokeServiceToken(r.Context(), tok)
	} else if claims.ExpiresAt != nil {
		err = s.blacklistJWT(r.Context(), tok, claims.ExpiresAt.Time)
	} else {
		writeErr(w, 400, "missing token expiry")
		return
	}
	if err != nil {
		writeErr(w, 500, "logout failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleCreateServiceToken(w http.ResponseWriter, r *http.Request) {
	if authKindFrom(r.Context()) != "jwt" {
		writeErr(w, 403, "service tokens must be created with an interactive admin session")
		return
	}
	claims := claimsFrom(r.Context())
	var req struct {
		Label string `json:"label"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "reach-mac-agent"
	}
	if len(label) > 64 {
		writeErr(w, 400, "label is too long")
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "mac-agent"
	}
	if role != "mac-agent" {
		writeErr(w, 400, "unsupported service token role")
		return
	}
	id, err := RandomID("stkn")
	if err != nil {
		writeErr(w, 500, "could not generate token id")
		return
	}
	tok, err := RandomToken("sat", 32)
	if err != nil {
		writeErr(w, 500, "could not generate token")
		return
	}
	now := nowUTC()
	_, err = s.store.db.ExecContext(r.Context(), `INSERT INTO service_tokens (id,label,token_hash,role,created_by,created_at) VALUES (?,?,?,?,?,?)`, id, label, TokenHash(tok), role, claims.Username, now)
	if err != nil {
		writeErr(w, 500, "could not persist token")
		return
	}
	s.store.Audit(r.Context(), "service_token.create", claims.Username, "", "id="+id+" label="+label+" role="+role)
	writeJSON(w, 200, map[string]any{"id": id, "label": label, "role": role, "token": tok})
}

func (s *Server) handleAdminRequests(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT id,name,status,metadata_json,created_at,expires_at,approved_at,denied_at,setup_token_expires_at FROM requests ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		writeErr(w, 500, "query failed")
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, name, status, created, expires string
		var meta, approved, denied, setupExp sql.NullString
		_ = rows.Scan(&id, &name, &status, &meta, &created, &expires, &approved, &denied, &setupExp)
		var mv any
		if meta.Valid {
			_ = json.Unmarshal([]byte(meta.String), &mv)
		}
		out = append(out, map[string]any{"id": id, "name": name, "status": status, "metadata": mv, "created_at": created, "expires_at": expires, "approved_at": approved.String, "denied_at": denied.String, "setup_token_expires_at": setupExp.String})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAdminRequestAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitAction(r.URL.Path, "/api/admin/requests/")
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	if r.Method != "POST" {
		writeErr(w, 405, "method not allowed")
		return
	}
	claims := claimsFrom(r.Context())
	now := nowUTC()
	switch action {
	case "approve":
		res, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='approved', setup_token_hash=NULL, setup_token_expires_at=NULL, approved_at=? WHERE id=? AND status='pending'`, now, id)
		if err != nil {
			writeErr(w, 500, "approve failed")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			writeErr(w, 404, "pending request not found")
			return
		}
		s.store.Audit(r.Context(), "request.approve", claims.Username, "", "request="+id)
		s.publishEvent(r.Context(), "request.approved", "", map[string]any{"request_id": id})
		writeJSON(w, 200, map[string]any{"request_id": id, "status": "approved"})
	case "deny":
		res, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='denied', denied_at=? WHERE id=? AND status='pending'`, now, id)
		if err != nil {
			writeErr(w, 500, "deny failed")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			writeErr(w, 404, "pending request not found")
			return
		}
		s.store.Audit(r.Context(), "request.deny", claims.Username, "", "request="+id)
		s.publishEvent(r.Context(), "request.denied", "", map[string]any{"request_id": id})
		writeJSON(w, 200, map[string]any{"request_id": id, "status": "denied"})
	default:
		writeErr(w, 404, "not found")
	}
}

func (s *Server) handleAdminMachines(w http.ResponseWriter, r *http.Request) {
	ms, err := s.prov.ListMachines(r.Context())
	if err != nil {
		writeErr(w, 500, "query failed")
		return
	}
	writeJSON(w, 200, ms)
}

func (s *Server) handleAdminMachineAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitAction(r.URL.Path, "/api/admin/machines/")
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	claims := claimsFrom(r.Context())
	if action == "" && r.Method == "GET" {
		item, err := s.prov.GetMachineDetail(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "machine not found")
			return
		}
		if err != nil {
			writeErr(w, 500, "query failed")
			return
		}
		writeJSON(w, 200, item)
		return
	}
	if r.Method != "POST" {
		writeErr(w, 405, "method not allowed")
		return
	}
	if action == "process-title" {
		s.handleAdminMachineProcessTitle(w, r, id, claims.Username)
		return
	}
	if action == "update-policy" {
		s.handleAdminMachineUpdatePolicy(w, r, id, claims.Username)
		return
	}
	if action == "update-agent" {
		s.handleAdminMachineUpdateAgent(w, r, id, claims.Username)
		return
	}
	var desired, ev string
	switch action {
	case "disable":
		desired = DesiredDisabled
		ev = "machine.disabled"
	case "enable":
		desired = DesiredActive
		ev = "machine.enabled"
	case "remove":
		desired = DesiredRetiring
		ev = "machine.retiring"
	case "retire-now":
		desired = DesiredRetired
		ev = "machine.retired"
	default:
		writeErr(w, 404, "not found")
		return
	}
	m, err := s.prov.SetDesiredState(r.Context(), id, desired, claims.Username)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.publishEvent(r.Context(), "machine.desired_changed", m.ID, map[string]any{"machine_id": m.ID, "desired_state": m.DesiredState, "generation": m.DesiredGeneration})
	s.publishEvent(r.Context(), ev, m.ID, map[string]any{"machine_id": m.ID, "slug": m.Slug})
	s.publishEvent(r.Context(), "ssh_config.changed", m.ID, map[string]any{"reason": action, "machine_id": m.ID})
	writeJSON(w, 200, map[string]any{"status": m.Status, "desired_state": m.DesiredState})
}

func (s *Server) handleAdminMachineUpdateAgent(w http.ResponseWriter, r *http.Request, id, actor string) {
	m, err := s.prov.GetMachine(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "machine not found")
		return
	}
	if err := s.queueAgentUpdate(r.Context(), m, actor, "manual"); err != nil {
		if errors.Is(err, errNoAgentUpdateAvailable) {
			writeErr(w, 400, "no latest agent version configured")
			return
		}
		if errors.Is(err, errUpdateAlreadyQueued) {
			writeErr(w, 409, "agent update already queued")
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminMachineUpdatePolicy(w http.ResponseWriter, r *http.Request, id, actor string) {
	var req struct {
		UpdatePolicy string `json:"update_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	policy, err := normalizeUpdatePolicy(req.UpdatePolicy)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	m, err := s.prov.SetUpdatePolicy(r.Context(), id, policy, actor)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.publishEvent(r.Context(), "machine.update_policy_changed", m.ID, map[string]any{"machine_id": m.ID, "slug": m.Slug, "update_policy": m.UpdatePolicy})
	writeJSON(w, 200, map[string]any{"machine": m})
}

func (s *Server) handleAdminMachineProcessTitle(w http.ResponseWriter, r *http.Request, id, actor string) {
	var req struct {
		ProcessTitleConfig *ProcessTitleConfig `json:"process_title_config"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	m, err := s.prov.SetProcessTitleConfig(r.Context(), id, req.ProcessTitleConfig, actor)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.publishEvent(r.Context(), "machine.desired_changed", m.ID, map[string]any{"machine_id": m.ID, "desired_state": m.DesiredState, "generation": m.DesiredGeneration, "reason": "process_title"})
	s.publishEvent(r.Context(), "machine.process_title_changed", m.ID, map[string]any{"machine_id": m.ID, "slug": m.Slug})
	writeJSON(w, 200, map[string]any{"machine": m})
}

func (s *Server) handleSSHConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.GenerateSSHConfig(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(cfg))
}

func (s *Server) GenerateSSHConfig(ctx context.Context) (string, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT m.id,m.slug,m.target_user,t.remote_port,h.proxyjump_alias,ao.raw_json FROM machines m JOIN tunnels t ON t.machine_id=m.id JOIN hubs h ON h.id=t.hub_id LEFT JOIN agent_observations ao ON ao.machine_id=m.id WHERE m.desired_state='active' AND m.observed_state IN ('online','degraded') AND t.status NOT IN ('disabled','retired','failed') ORDER BY m.slug`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("# Generated by Reach. Do not edit by hand.\n# Includes desired-active machines whose observed state is online/degraded.\n\n")
	for rows.Next() {
		var id, slug string
		var target, pj, rawObservation sql.NullString
		var port int
		if err := rows.Scan(&id, &slug, &target, &port, &pj, &rawObservation); err != nil {
			return "", err
		}
		if err := ValidateSlug(slug); err != nil {
			return "", err
		}
		user := target.String
		if user == "" {
			user = "root"
		}
		if err := ValidateSSHConfigToken("target user", user); err != nil {
			return "", err
		}
		jump := pj.String
		if jump == "" {
			jump = s.cfg.DefaultHub.ProxyJumpAlias
		}
		if err := ValidateSSHConfigToken("proxyjump_alias", jump); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "Host %s\n    HostName localhost\n    Port %d\n    User %s\n    ProxyJump %s\n    HostKeyAlias reach-%s\n    StrictHostKeyChecking accept-new\n", slug, port, user, jump, id)
		for _, opt := range sshConfigClientOptions(rawObservation.String) {
			fmt.Fprintf(&b, "    %s %s\n", opt[0], opt[1])
		}
		b.WriteString("\n")
	}
	return b.String(), rows.Err()
}

func sshOptionsSignature(raw string) string {
	opts := sshConfigClientOptions(raw)
	parts := make([]string, 0, len(opts))
	for _, opt := range opts {
		parts = append(parts, opt[0]+"="+opt[1])
	}
	return strings.Join(parts, ";")
}

func sshConfigClientOptions(raw string) [][2]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var hb AgentHeartbeat
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		return nil
	}
	allowed := map[string]bool{
		"KexAlgorithms":            true,
		"HostKeyAlgorithms":        true,
		"PubkeyAcceptedAlgorithms": true,
		"PubkeyAcceptedKeyTypes":   true,
		"Ciphers":                  true,
		"MACs":                     true,
	}
	var out [][2]string
	for _, opt := range hb.Capabilities.SSH.ClientOptions {
		name, value, ok := strings.Cut(strings.TrimSpace(opt), "=")
		if !ok || !allowed[name] || value == "" || strings.ContainsAny(value, " \t\r\n") {
			continue
		}
		out = append(out, [2]string{name, value})
	}
	return out
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := s.prov.CachedHealth(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rep)
}

func splitAction(path, prefix string) (id, action string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if len(parts) == 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}
func pathAction(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix), "/")
	return id, id != ""
}
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func (s *Server) firstAdminID(ctx context.Context) (string, error) {
	var id string
	err := s.store.db.QueryRowContext(ctx, `SELECT id FROM users ORDER BY created_at LIMIT 1`).Scan(&id)
	return id, err
}
func (s *Server) activeAccessKeys(ctx context.Context) ([]map[string]string, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT label,public_key,kind FROM access_keys WHERE revoked_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var label, key, kind string
		if err := rows.Scan(&label, &key, &kind); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"label": label, "public_key": key, "kind": kind})
	}
	return out, rows.Err()
}

func validateMetadataLimits(v any) error {
	if v == nil {
		return nil
	}
	if err := validateMetadataValue(v, 0); err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if len(b) > 4096 {
		return fmt.Errorf("metadata is too large")
	}
	return nil
}

func validateMetadataValue(v any, depth int) error {
	if depth > 4 {
		return fmt.Errorf("metadata is too deeply nested")
	}
	switch x := v.(type) {
	case string:
		if len(x) > 256 {
			return fmt.Errorf("metadata string is too long")
		}
	case map[string]any:
		if len(x) > 32 {
			return fmt.Errorf("metadata has too many keys")
		}
		for k, val := range x {
			if len(k) > 64 {
				return fmt.Errorf("metadata key is too long")
			}
			if err := validateMetadataValue(val, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if len(x) > 32 {
			return fmt.Errorf("metadata array is too long")
		}
		for _, val := range x {
			if err := validateMetadataValue(val, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) authenticateAdminToken(ctx context.Context, token string) (*Claims, string, error) {
	claims, err := ParseJWT(s.cfg.JWTSecret, token)
	if err == nil {
		blacklisted, lookupErr := s.isJWTBlacklisted(ctx, token)
		if lookupErr != nil {
			return nil, "", lookupErr
		}
		if blacklisted {
			return nil, "", errors.New("blacklisted token")
		}
		return claims, "jwt", nil
	}
	claims, err = s.parseServiceToken(ctx, token)
	if err != nil {
		return nil, "", err
	}
	return claims, "service", nil
}

func adminRoleAllowed(role, path string) bool {
	if role == "owner" || role == "admin" {
		return true
	}
	if role == "mac-agent" {
		return path == "/api/admin/ssh-config" || path == "/api/admin/events" || path == "/api/admin/logout"
	}
	return false
}

func (s *Server) parseServiceToken(ctx context.Context, token string) (*Claims, error) {
	var id, label, role string
	err := s.store.db.QueryRowContext(ctx, `SELECT id,label,role FROM service_tokens WHERE token_hash=? AND revoked_at IS NULL`, TokenHash(token)).Scan(&id, &label, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invalid service token")
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.store.db.ExecContext(ctx, `UPDATE service_tokens SET last_used_at=? WHERE id=?`, nowUTC(), id)
	return &Claims{UserID: id, Username: "service:" + label, Role: role}, nil
}

func (s *Server) revokeServiceToken(ctx context.Context, token string) error {
	_, err := s.store.db.ExecContext(ctx, `UPDATE service_tokens SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`, nowUTC(), TokenHash(token))
	return err
}

func (s *Server) blacklistJWT(ctx context.Context, token string, expiresAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.store.db.ExecContext(ctx, `INSERT OR REPLACE INTO jwt_blacklist (token_hash,expires_at,created_at) VALUES (?,?,?)`, TokenHash(token), expiresAt.UTC().Format(time.RFC3339), now.Format(time.RFC3339))
	return err
}

func (s *Server) isJWTBlacklisted(ctx context.Context, token string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var found string
	err := s.store.db.QueryRowContext(ctx, `SELECT token_hash FROM jwt_blacklist WHERE token_hash=? AND expires_at>?`, TokenHash(token), now).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.ListenAddr, Handler: s, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("reachd listening on %s", s.cfg.ListenAddr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
