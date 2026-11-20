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
	"sync"
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
	// Unauthenticated/agent endpoints get in-memory limiters: a persistent
	// (SQLite) limiter would turn every rejected request into a DB write.
	pollIPL      *rateLimiter
	pollReqL     *rateLimiter
	agentIPL     *rateLimiter
	agentMachL   *rateLimiter
	uninstallIPL *rateLimiter
	// Failed attempts are counted per (IP, object), so someone who knows a
	// request or machine ID but not its secret cannot use up the quota of
	// the real installer or agent (daybreak P2-5). Per-object quotas apply
	// only after successful authentication.
	badAuthL      *rateLimiter
	uninstallMach *rateLimiter
	argon         *argonGate
	// revocations wakes open SSE streams so they re-authenticate at once
	// after logout, revoke-all or service-token revocation (astra F01).
	revocations *revocationNotifier
	streams     *streamLimiter
}

const (
	pollPerIPPerMinute           = 60
	pollPerRequestPerMinute      = 30
	agentPerIPPerMinute          = 240
	agentPerMachinePerMinute     = 60
	uninstallPerIPPerMinute      = 20
	maxHeartbeatCommandResults   = 32
	maxEventStreams              = 64
	maxEventStreamsPerIP         = 8
	maxEventStreamsPerCred       = 8
	badAuthPerIPObjectPerMinute  = 10
	uninstallPerMachinePerMinute = 10
	maxHeartbeatBodyBytes        = 128 << 10
	maxCommandResultMessage      = 1024
)

func NewServer(cfg Config, store *Store, prov *Provisioner) *Server {
	s := &Server{cfg: cfg, store: store, prov: prov, mux: http.NewServeMux(), events: newEventBroker(store),
		registerL:     newPersistentRateLimiter(store.db, "client_register", 3, time.Minute),
		loginL:        newPersistentRateLimiter(store.db, "admin_login", 8, time.Minute),
		pollIPL:       newRateLimiter(pollPerIPPerMinute, time.Minute),
		pollReqL:      newRateLimiter(pollPerRequestPerMinute, time.Minute),
		agentIPL:      newRateLimiter(agentPerIPPerMinute, time.Minute),
		agentMachL:    newRateLimiter(agentPerMachinePerMinute, time.Minute),
		uninstallIPL:  newRateLimiter(uninstallPerIPPerMinute, time.Minute),
		badAuthL:      newRateLimiter(badAuthPerIPObjectPerMinute, time.Minute),
		uninstallMach: newRateLimiter(uninstallPerMachinePerMinute, time.Minute),
		argon:         defaultArgonGate,
		revocations:   newRevocationNotifier(),
		streams:       newStreamLimiter(maxEventStreams, maxEventStreamsPerIP, maxEventStreamsPerCred),
	}
	if prov != nil {
		prov.argon = s.argon
	}
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

// allowedCORSOrigin allows only the configured public origin and explicit
// allowed_origins. Local dev servers (http://localhost:3000) must be listed in
// allowed_origins; they are no longer allowed implicitly (Opus P3-2).
func (s *Server) allowedCORSOrigin(origin string) bool {
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
	s.mux.HandleFunc("/api/admin/sessions/revoke-all", s.admin(s.method("POST", s.handleRevokeAllSessions)))
	s.mux.HandleFunc("/api/admin/requests", s.admin(s.method("GET", s.handleAdminRequests)))
	s.mux.HandleFunc("/api/admin/requests/", s.admin(s.handleAdminRequestAction))
	s.mux.HandleFunc("/api/admin/machines", s.admin(s.method("GET", s.handleAdminMachines)))
	s.mux.HandleFunc("/api/admin/machines/", s.admin(s.handleAdminMachineAction))
	s.mux.HandleFunc("/api/admin/ssh-config", s.admin(s.method("GET", s.handleSSHConfig)))
	s.mux.HandleFunc("/api/admin/ssh-config.json", s.admin(s.method("GET", s.handleSSHConfigJSON)))
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
func writeRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// writeAuthBackendErr maps an Argon2 gate failure to a retryable 503 instead
// of pretending the credentials were wrong.
func writeAuthBackendErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errArgonBusy) || errors.Is(err, context.DeadlineExceeded) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "authentication busy; retry later")
		return true
	}
	log.Printf("authentication backend error: %v", err)
	writeErr(w, http.StatusInternalServerError, "authentication failed")
	return true
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
	clientHash := HashToken(clientSecret)
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
	// The slug is fixed at registration so the operator approves the alias
	// that will actually be created (Opus P1-2).
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = NormalizeSlug(name)
	}
	if err := ValidateSlug(slug); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.checkSlugAllowed(slug); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if used, err := s.slugInUse(r.Context(), slug); err != nil {
		writeErr(w, 500, "registration failed")
		return
	} else if used {
		writeErr(w, 409, fmt.Sprintf("machine name %q is already in use; choose another name", slug))
		return
	}
	if err := ValidateTargetUser(strings.TrimSpace(req.TargetUser)); err != nil {
		writeErr(w, 400, "invalid target_user")
		return
	}
	// Free-form client metadata first; server-derived and bound fields last so
	// a client cannot forge source_ip or the approved parameters (P2-4/P2-1).
	meta := map[string]any{}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	// Resolve defaults now so the operator approves concrete values, not
	// blanks that provision could later fill with root/system/22
	// (daybreak P2-3).
	bound := canonicalBinding(slug, req.TargetUser, req.Mode, req.LocalPort)
	if bound.LocalPort < 1 || bound.LocalPort > 65535 {
		writeErr(w, 400, "invalid local_port")
		return
	}
	for k, v := range map[string]any{"name": name, "target_user": bound.TargetUser, "slug": slug, "mode": bound.Mode, "local_port": bound.LocalPort, "binding": bindingVersion, "distro": req.Distro, "arch": req.Arch, "transport": req.Transport, "source_ip": ip} {
		meta[k] = v
	}
	metaJSON, _ := json.Marshal(meta)
	if len(metaJSON) > 4096 {
		writeErr(w, 400, "metadata is too large")
		return
	}
	if req.AuthCode != "" {
		ok, kind, matchedHash, authErr := s.validateClientAuth(r.Context(), req.AuthCode)
		if writeAuthBackendErr(w, authErr) {
			return
		}
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
		setupHash = HashToken(setupToken)
		setupExpires = clampExpiry(now.Add(s.cfg.SetupTokenTTL), expires).Format(time.RFC3339)
		approvedAt = now.Format(time.RFC3339)
		if kind != "god" && matchedHash != "" {
			inviteHash = matchedHash
		}
	}
	if err := s.insertRequestClaimingInvite(r.Context(), requestID, name, slug, clientHash, status, inviteHash, setupHash, setupExpires, string(metaJSON), ip, now, expires, approvedAt); err != nil {
		if errors.Is(err, errInviteAlreadyClaimed) {
			writeClientAuthErr(w)
			return
		}
		if errors.Is(err, errSlugReserved) {
			writeErr(w, 409, fmt.Sprintf("machine name %q is already requested by another pending registration; choose another name", slug))
			return
		}
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

// validateClientAuth checks a god code or invite code. Both are human-handled
// secrets stored as Argon2id, so every check runs under the Argon2 gate.
var (
	errInviteAlreadyClaimed = errors.New("invite already claimed")
	errSlugReserved         = errors.New("slug reserved by another live request")
)

// insertRequestClaimingInvite creates the request and, for invite codes,
// claims the invite in the same transaction. The conditional UPDATE makes the
// claim atomic: of N concurrent registrations with one invite exactly one
// gets RowsAffected=1 (daybreak P1-4).
func (s *Server) insertRequestClaimingInvite(ctx context.Context, requestID, name, slug, clientHash, status string, inviteHash, setupHash, setupExpires any, metaJSON, ip string, now time.Time, expires string, approvedAt any) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if inviteHash != nil {
		res, err := tx.ExecContext(ctx, `UPDATE invites SET claimed_by_request_id=?, claimed_at=? WHERE code_hash=? AND used_by IS NULL AND claimed_by_request_id IS NULL AND expires_at>?`, requestID, now.Format(time.RFC3339), inviteHash, now.Format(time.RFC3339))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errInviteAlreadyClaimed
		}
	}
	// Release reservations held by requests that have quietly expired, so
	// an abandoned request does not block its slug until maintenance runs.
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET status='expired', setup_token_hash=NULL WHERE slug=? AND status IN ('pending','approved') AND expires_at<=?`, slug, now.Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests (id,name,client_secret_hash,status,invite_code_hash,setup_token_hash,setup_token_expires_at,metadata_json,observed_ip,slug,created_at,expires_at,approved_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, requestID, name, clientHash, status, inviteHash, setupHash, setupExpires, metaJSON, ip, slug, now.Format(time.RFC3339), expires, approvedAt); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: requests.slug") {
			return errSlugReserved
		}
		return err
	}
	return tx.Commit()
}

func (s *Server) validateClientAuth(ctx context.Context, code string) (bool, string, string, error) {
	if s.cfg.GodCodeHash != "" {
		ok, err := s.argon.VerifySecret(ctx, code, s.cfg.GodCodeHash)
		if err != nil {
			return false, "", "", err
		}
		if ok {
			return true, "god", "", nil
		}
	}
	type invite struct{ hash, typ string }
	var candidates []invite
	rows, err := s.store.db.QueryContext(ctx, `SELECT code_hash,type,expires_at FROM invites WHERE used_by IS NULL AND claimed_by_request_id IS NULL`)
	if err != nil {
		return false, "", "", nil
	}
	now := time.Now().UTC()
	for rows.Next() {
		var hash, typ, exp string
		if rows.Scan(&hash, &typ, &exp) != nil {
			continue
		}
		et, err := time.Parse(time.RFC3339, exp)
		if err != nil || now.After(et) {
			continue
		}
		candidates = append(candidates, invite{hash, typ})
	}
	_ = rows.Close()
	for _, c := range candidates {
		ok, err := s.argon.VerifySecret(ctx, code, c.hash)
		if err != nil {
			return false, "", "", err
		}
		if ok {
			return true, c.typ, c.hash, nil
		}
	}
	return false, "", "", nil
}

func (s *Server) handleClientPoll(w http.ResponseWriter, r *http.Request) {
	id, ok := pathAction(r.URL.Path, "/api/client/register/", "/poll")
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	// Rate limits come before any DB or hash work (daybreak P1-1, Opus P0-1).
	ip := clientIP(r)
	if !s.pollIPL.allow(ip) || s.badAuthL.blocked("poll|"+ip+"|"+id) {
		writeRateLimited(w)
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
	// Cheap state checks first. Terminal states carry no secret material, so
	// they are answered without verifying the client secret at all.
	// The request TTL bounds approved requests too: an approval does not
	// extend it (daybreak P2-4). Unparseable expiry fails closed.
	if status == "pending" || status == "approved" {
		if t, err := time.Parse(time.RFC3339, expiresAt); err != nil || time.Now().UTC().After(t) {
			if res, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='expired', setup_token_hash=NULL WHERE id=? AND status IN ('pending','approved')`, id); err == nil {
				if n, _ := res.RowsAffected(); n == 1 {
					s.publishEvent(r.Context(), "request.expired", "", map[string]any{"request_id": id})
				}
			}
			status = "expired"
		}
	}
	switch status {
	case "pending", "approved":
	default:
		writeJSON(w, 200, map[string]any{"request_id": id, "status": status})
		return
	}
	ok, upgraded, err := s.argon.VerifyStoredToken(r.Context(), req.ClientSecret, secretHash)
	if writeAuthBackendErr(w, err) {
		return
	}
	if !ok {
		s.badAuthL.allow("poll|" + ip + "|" + id)
		writeClientAuthErr(w)
		return
	}
	if !s.pollReqL.allow(id) {
		writeRateLimited(w)
		return
	}
	if upgraded != "" {
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET client_secret_hash=? WHERE id=? AND client_secret_hash=?`, upgraded, id, secretHash)
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
			exp := clampExpiry(time.Now().UTC().Add(s.cfg.SetupTokenTTL), expiresAt).Format(time.RFC3339)
			res, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET setup_token_hash=?, setup_token_expires_at=? WHERE id=? AND status='approved' AND setup_token_hash IS NULL`, HashToken(setupToken), exp, id)
			if err != nil {
				writeErr(w, 500, "could not persist setup token")
				return
			}
			// A concurrent poll may have minted the token first; only the
			// request that actually stored its hash may reveal a token.
			if n, _ := res.RowsAffected(); n != 1 {
				resp["setup_token_expires_at"] = exp
				writeJSON(w, 200, resp)
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
	bound, err := s.approvedBinding(r.Context(), req.RequestID)
	if errors.Is(err, errLegacyBinding) {
		writeErr(w, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		writeClientAuthErr(w)
		return
	}
	if req.Slug == "" {
		req.Slug = bound.Slug
	}
	if err := bound.check(req.Slug, req.TargetUser, req.Mode, req.LocalPort); err != nil {
		log.Printf("provision %s rejected: %v", req.RequestID, err)
		writeErr(w, 403, "provision parameters do not match the approved request")
		return
	}
	if err := s.checkSlugAllowed(req.Slug); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := ValidateSSHPublicKey(req.Pubkey); err != nil {
		writeErr(w, 400, "invalid tunnel public key")
		return
	}
	if used, err := s.slugInUse(r.Context(), req.Slug); err != nil || used {
		writeErr(w, 409, "slug already in use")
		return
	}
	requestID, name, err := s.consumeSetupToken(r.Context(), req.RequestID, req.SetupToken)
	if err != nil {
		// Only a saturated Argon2 gate is retryable; terminal token states
		// and database errors are not "busy" (astra P2-09).
		switch {
		case errors.Is(err, errArgonBusy):
			writeAuthBackendErr(w, err)
		case errors.Is(err, errSetupTokenExpired):
			writeErr(w, http.StatusGone, "setup token expired; rerun setup to request a new one")
		case errors.Is(err, errInvalidSetupToken), errors.Is(err, sql.ErrNoRows):
			writeClientAuthErr(w)
		default:
			log.Printf("consume setup token for %s: %v", req.RequestID, err)
			writeErr(w, 500, "provisioning failed")
		}
		return
	}
	_ = name
	owner, err := s.firstAdminID(r.Context())
	if err != nil {
		writeErr(w, 500, "no admin user configured")
		return
	}
	res, err := s.prov.CreateMachine(r.Context(), CreateMachineInput{RequestID: requestID, Slug: req.Slug, DisplayName: firstNonEmpty(req.DisplayName, name), TargetUser: req.TargetUser, Pubkey: req.Pubkey, Mode: req.Mode, LocalPort: req.LocalPort, Persistence: req.Persistence, Distro: req.Distro, Arch: req.Arch, OwnerUserID: owner})
	if err != nil {
		// Back to approved without a setup token: the agent must poll for
		// a new one, so a retry cannot reuse the consumed token.
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='approved', provisioning_started_at=NULL, machine_id=NULL WHERE id=? AND status='provisioning'`, requestID)
		// Details (sshd -t output, paths) stay in the server log (Opus P3-3).
		log.Printf("provision %s failed: %v", requestID, err)
		if isUniqueSlugErr(err) {
			writeErr(w, 409, "slug already in use")
			return
		}
		writeErr(w, 500, "provisioning failed; ask the operator to check the reachd log")
		return
	}
	_, _ = s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='provisioned', machine_id=? WHERE id=?`, res.Machine.ID, requestID)
	// Consume only the claim this request holds; never overwrite used_by.
	if _, err := s.store.db.ExecContext(r.Context(), `UPDATE invites SET used_by=?, consumed_at=? WHERE claimed_by_request_id=? AND used_by IS NULL`, res.Machine.ID, nowUTC(), requestID); err != nil {
		log.Printf("consume invite for %s: %v", requestID, err)
	}
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

// approvedBinding is the immutable set of parameters an operator approved.
type approvedBinding struct {
	Slug       string
	TargetUser string
	Mode       string
	LocalPort  int
}

// bindingVersion marks requests whose metadata carries a complete,
// default-resolved binding. Requests without it (created before this
// release, at most request_ttl old) must re-register.
const bindingVersion = 2

var errLegacyBinding = errors.New("request predates complete approval binding; rerun setup to register again")

// canonicalBinding applies the same defaults provisioning uses, so an
// omitted field is bound to a concrete value at registration.
func canonicalBinding(slug, targetUser, mode string, localPort int) approvedBinding {
	b := approvedBinding{Slug: strings.TrimSpace(slug), TargetUser: strings.TrimSpace(targetUser), Mode: strings.TrimSpace(mode), LocalPort: localPort}
	if b.TargetUser == "" {
		b.TargetUser = "root"
	}
	if b.Mode == "" {
		b.Mode = "system"
	}
	if b.LocalPort == 0 {
		b.LocalPort = 22
	}
	return b
}

// approvedBinding loads the binding for a request. Any lookup or decode
// failure is an error: provisioning fails closed (Opus P2-4 / daybreak P2-1).
func (s *Server) approvedBinding(ctx context.Context, requestID string) (approvedBinding, error) {
	var name string
	var raw sql.NullString
	if err := s.store.db.QueryRowContext(ctx, `SELECT name,metadata_json FROM requests WHERE id=?`, requestID).Scan(&name, &raw); err != nil {
		return approvedBinding{}, err
	}
	want := map[string]any{}
	if raw.Valid && raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &want); err != nil {
			return approvedBinding{}, fmt.Errorf("decode approved metadata: %w", err)
		}
	}
	if v, _ := want["binding"].(float64); int(v) != bindingVersion {
		return approvedBinding{}, errLegacyBinding
	}
	str := func(k string) string { v, _ := want[k].(string); return strings.TrimSpace(v) }
	port, _ := want["local_port"].(float64)
	b := approvedBinding{Slug: str("slug"), TargetUser: str("target_user"), Mode: str("mode"), LocalPort: int(port)}
	if b.Slug == "" || b.TargetUser == "" || b.Mode == "" || b.LocalPort == 0 {
		return approvedBinding{}, fmt.Errorf("approved binding is incomplete")
	}
	return b, nil
}

// check requires the provision values, after the same defaulting as at
// registration, to equal every bound value exactly. Nothing is a wildcard.
func (b approvedBinding) check(slug, targetUser, mode string, localPort int) error {
	got := canonicalBinding(slug, targetUser, mode, localPort)
	if got.Slug != b.Slug {
		return fmt.Errorf("slug %q differs from approved %q", got.Slug, b.Slug)
	}
	if got.TargetUser != b.TargetUser {
		return fmt.Errorf("target_user differs from approved value")
	}
	if got.Mode != b.Mode {
		return fmt.Errorf("mode differs from approved value")
	}
	if got.LocalPort != b.LocalPort {
		return fmt.Errorf("local_port differs from approved value")
	}
	return nil
}

// checkSlugAllowed rejects reserved aliases and the hub's own ProxyJump alias.
func (s *Server) checkSlugAllowed(slug string) error {
	if SlugReserved(slug, s.cfg.ReservedSlugs) || strings.EqualFold(slug, s.cfg.DefaultHub.ProxyJumpAlias) {
		return fmt.Errorf("slug %q is reserved; choose another machine name", slug)
	}
	return nil
}

// slugInUse reports whether a non-retired machine already holds slug.
// Checked at registration and before the setup token is consumed, so a
// collision does not burn the token.
func (s *Server) slugInUse(ctx context.Context, slug string) (bool, error) {
	var id string
	err := s.store.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE slug=?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// clampExpiry returns t, or the request expiry if that is earlier.
func clampExpiry(t time.Time, requestExpires string) time.Time {
	if r, err := time.Parse(time.RFC3339, requestExpires); err == nil && r.Before(t) {
		return r
	}
	return t
}

func isUniqueSlugErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: machines.slug")
}

var (
	errInvalidSetupToken = errors.New("invalid setup token")
	errSetupTokenExpired = errors.New("setup token expired")
)

func (s *Server) consumeSetupToken(ctx context.Context, requestID, token string) (string, string, error) {
	var id, name, status, reqExpires string
	var hash, exp sql.NullString
	err := s.store.db.QueryRowContext(ctx, `SELECT id,name,status,setup_token_hash,setup_token_expires_at,expires_at FROM requests WHERE id=?`, requestID).Scan(&id, &name, &status, &hash, &exp, &reqExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("request not found")
	}
	if err != nil {
		return "", "", err
	}
	if status != "approved" || !hash.Valid {
		return "", "", errInvalidSetupToken
	}
	ok, _, verr := s.argon.VerifyStoredToken(ctx, token, hash.String)
	if verr != nil {
		return "", "", verr
	}
	if !ok {
		return "", "", errInvalidSetupToken
	}
	if exp.Valid {
		if t, err := time.Parse(time.RFC3339, exp.String); err != nil || time.Now().UTC().After(t) {
			return "", "", errSetupTokenExpired
		}
	}
	if t, err := time.Parse(time.RFC3339, reqExpires); err != nil || time.Now().UTC().After(t) {
		return "", "", errSetupTokenExpired
	}
	res, err := s.store.db.ExecContext(ctx, `UPDATE requests SET status='provisioning', setup_token_hash=NULL, provisioning_started_at=? WHERE id=? AND status='approved' AND setup_token_hash=?`, nowUTC(), requestID, hash.String)
	if err != nil {
		return "", "", err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", "", errInvalidSetupToken
	}
	return id, name, nil
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	// Bound open streams globally, per client IP and per credential
	// (daybreak P2-12). A mac-agent service token gets one stream: a new
	// connection replaces the old one instead of piling up.
	claims := claimsFrom(r.Context())
	credKey := ""
	if claims != nil {
		credKey = authKindFrom(r.Context()) + "|" + claims.UserID
	}
	single := claims != nil && claims.Role == "mac-agent"
	streamCtx, release, ok := s.streams.acquire(r.Context(), clientIP(r), credKey, single)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "too many event streams")
		return
	}
	defer release()
	r = r.WithContext(streamCtx)
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
	ch, _, _, replay, reason, cancel, err := s.events.SubscribeSince(r.Context(), requested.ID, requested.Present, eventReplayMaxEvents)
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
	token := tokenFrom(r.Context())
	revoked := s.revocations.current()
	// stillAuthorized re-validates the bearer token (signature, expiry,
	// blacklist, session_version, service-token revocation) before anything
	// is written, so a revoked session never receives another byte.
	stillAuthorized := func() bool {
		claims, _, err := s.authenticateAdminToken(r.Context(), token)
		return err == nil && claims != nil && adminRoleAllowed(claims.Role, r.URL.Path)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-revoked:
			if !stillAuthorized() {
				return
			}
			revoked = s.revocations.current()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			id, err := strconv.ParseInt(ev.ID, 10, 64)
			if err == nil && id <= cursor {
				continue
			}
			if !stillAuthorized() {
				return
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
			if !stillAuthorized() {
				return
			}
			if err := writeSSEComment(w, "keepalive"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.agentIPL.allow(ip) {
		writeRateLimited(w)
		return
	}
	token, hb, raw, ok := s.authAgentHeartbeat(w, r)
	if !ok {
		return
	}
	badKey := "hb|" + ip + "|" + hb.MachineID
	if s.badAuthL.blocked(badKey) {
		writeRateLimited(w)
		return
	}
	if err := s.verifyAgentToken(r.Context(), hb.MachineID, token, true); err != nil {
		if !errors.Is(err, errAgentTokenInvalid) && writeAuthBackendErr(w, err) {
			return
		}
		s.badAuthL.allow(badKey)
		writeClientAuthErr(w)
		return
	}
	if !s.agentMachL.allow(hb.MachineID) {
		writeRateLimited(w)
		return
	}
	m, tunnels, err := s.prov.getMachineAndTunnels(r.Context(), hb.MachineID)
	if err != nil {
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
	// agent_status was a write-only duplicate of agent_observations (perf #3).
	if hb.Observed.Tunnel.State == "connected" || hb.Observed.Tunnel.State == "running" {
		// Only write when the status flips or last_seen_at is getting stale.
		staleBefore := time.Now().UTC().Add(-tunnelSeenWriteInterval).Format(time.RFC3339)
		_, _ = s.store.db.ExecContext(r.Context(), `UPDATE tunnels SET status='online', last_seen_at=? WHERE machine_id=? AND status NOT IN ('disabled','retired','failed','cleanup_failed') AND (status<>'online' OR last_seen_at IS NULL OR last_seen_at<?)`, now, hb.MachineID, staleBefore)
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
		s.publishEvent(r.Context(), "machine.observed_changed", m.ID, map[string]any{"machine_id": m.ID, "observed_state": m.ObservedState, "status": m.Status, "heartbeat_at": now, "event_at": now})
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
	// Live-only: never stored or replayed (perf #3, docs/EVENTS.md).
	if s.events != nil {
		s.events.PublishLive("agent.heartbeat", m.ID, map[string]any{"machine_id": m.ID, "heartbeat_at": now, "observed_state": m.ObservedState, "status": m.Status})
	}
}

const (
	tunnelSeenWriteInterval   = 5 * time.Minute
	serviceTokenTouchInterval = time.Minute
)

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
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxHeartbeatBodyBytes+1))
	if err != nil {
		writeErr(w, 400, "invalid body")
		return "", AgentHeartbeat{}, nil, false
	}
	if len(raw) > maxHeartbeatBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "heartbeat too large")
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

var errAgentTokenInvalid = errors.New("invalid token")

// verifyAgentToken checks a per-machine agent bearer token. Legacy Argon2id
// hashes are verified under the Argon2 gate and replaced with a SHA-256 hash
// on first success (lazy migration). Retired machines have their token hash
// cleared, so they always fail here.
func (s *Server) verifyAgentToken(ctx context.Context, machineID, token string, allowRetired bool) error {
	q := `SELECT agent_token_hash FROM machines WHERE id=?`
	if !allowRetired {
		q += ` AND desired_state NOT IN ('retired')`
	}
	var hash sql.NullString
	err := s.store.db.QueryRowContext(ctx, q, machineID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return errAgentTokenInvalid
	}
	if err != nil {
		return err
	}
	if !hash.Valid {
		return errAgentTokenInvalid
	}
	ok, upgraded, err := s.argon.VerifyStoredToken(ctx, token, hash.String)
	if err != nil {
		return err
	}
	if !ok {
		return errAgentTokenInvalid
	}
	if upgraded != "" {
		_, _ = s.store.db.ExecContext(ctx, `UPDATE machines SET agent_token_hash=? WHERE id=? AND agent_token_hash=?`, upgraded, machineID, hash.String)
	}
	return nil
}

// storeCommandResults records agent results. Only a real transition of a
// command this machine owns (pending/sent -> acked/failed) is stored and
// published, so a stale or forged token cannot inject events (daybreak P2-5).
func (s *Server) storeCommandResults(ctx context.Context, hb AgentHeartbeat) {
	for i, res := range hb.CommandResults {
		if i >= maxHeartbeatCommandResults {
			break
		}
		if res.CommandID == "" || len(res.CommandID) > 64 {
			continue
		}
		status := "acked"
		if res.Status == "failed" {
			status = "failed"
		}
		msg := truncateUTF8(res.Message, maxCommandResultMessage)
		// expires_at is a completion deadline: once it has passed the command
		// is terminal whether or not maintenance has marked it yet, so the
		// outcome no longer depends on goroutine order (astra P2-08).
		now := nowUTC()
		r, err := s.store.db.ExecContext(ctx, `UPDATE agent_commands SET status=?, acked_at=?, last_error=? WHERE id=? AND machine_id=? AND status IN ('pending','sent') AND (expires_at IS NULL OR expires_at>?)`, status, now, nullable(msg), res.CommandID, hb.MachineID, now)
		if err != nil {
			continue
		}
		if n, _ := r.RowsAffected(); n != 1 {
			continue
		}
		s.publishEvent(ctx, "agent.command_"+status, hb.MachineID, map[string]any{"machine_id": hb.MachineID, "command_id": res.CommandID, "status": status, "message": msg})
	}
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return truncateProcessTitleUTF8Bytes(s, max)
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
	ip := clientIP(r)
	badKey := "un|" + ip + "|" + req.MachineID
	if !s.uninstallIPL.allow(ip) || s.badAuthL.blocked(badKey) {
		writeRateLimited(w)
		return
	}
	if err := s.verifyAgentToken(r.Context(), req.MachineID, strings.TrimPrefix(auth, "Bearer "), true); err != nil {
		if !errors.Is(err, errAgentTokenInvalid) && writeAuthBackendErr(w, err) {
			return
		}
		s.badAuthL.allow(badKey)
		writeClientAuthErr(w)
		return
	}
	// Separate bucket from heartbeats so heartbeats cannot starve uninstall.
	if !s.uninstallMach.allow(req.MachineID) {
		writeRateLimited(w)
		return
	}
	// cleanup_state='complete' is only written by retire() after the hub
	// side is verified clean; a failure is reported, not hidden (P1-2).
	var opErr error
	if complete {
		opErr = s.prov.RetireMachineNow(r.Context(), req.MachineID, "agent-uninstall")
		if opErr == nil {
			s.publishEvent(r.Context(), "machine.retired", req.MachineID, map[string]any{"machine_id": req.MachineID})
		}
	} else {
		// Idempotent only once revoke actually succeeded: a machine already
		// retiring with cleanup pending/timed_out needs nothing; a previous
		// disable_failed/cleanup_failed must be retried (astra P2-02).
		m, err := s.prov.GetMachine(r.Context(), req.MachineID)
		if err != nil {
			opErr = err
		} else if m.CleanupState != nil && *m.CleanupState == CleanupFailed {
			// Hub cleanup already started and failed: retry the full retire.
			opErr = s.prov.RetireMachineNow(r.Context(), req.MachineID, "agent-uninstall")
		} else if m.DesiredState != DesiredRetiring || (m.CleanupState != nil && *m.CleanupState == DisableFailed) {
			opErr = s.prov.RemoveMachine(r.Context(), req.MachineID, "agent-uninstall")
		}
		if opErr == nil {
			s.publishEvent(r.Context(), "machine.retiring", req.MachineID, map[string]any{"machine_id": req.MachineID, "reason": truncateUTF8(req.Reason, 128)})
		}
	}
	s.publishEvent(r.Context(), "ssh_config.changed", req.MachineID, map[string]any{"machine_id": req.MachineID})
	if opErr != nil {
		log.Printf("agent uninstall for %s: %v", req.MachineID, opErr)
		writeErr(w, http.StatusConflict, "hub-side cleanup failed; operator action required")
		return
	}
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
	var sessionVersion int64
	err := s.store.db.QueryRowContext(r.Context(), `SELECT id,username,password_hash,role,session_version FROM users WHERE username=?`, req.Username).Scan(&id, &username, &hash, &role, &sessionVersion)
	known := err == nil
	if !known {
		// Unknown account: still pay for one Argon2 run so response timing
		// does not reveal which usernames exist (Opus P3-1).
		hash = dummyArgonHash()
	}
	ok, verr := s.argon.VerifySecret(r.Context(), req.Password, hash)
	if writeAuthBackendErr(w, verr) {
		return
	}
	if !known || !ok {
		writeErr(w, 401, "invalid username or password")
		return
	}
	tok, err := SignJWTVersion(s.cfg.JWTSecret, id, username, role, sessionVersion, s.cfg.AdminJWTTTL)
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
	s.revocations.notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleRevokeAllSessions invalidates every dashboard JWT of the caller's
// account (including the current one). With include_service_tokens it also
// revokes all service tokens, which disconnects reach-mac-agent until it is
// given a new token.
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if authKindFrom(r.Context()) != "jwt" {
		writeErr(w, 403, "sessions can only be revoked from an interactive admin session")
		return
	}
	claims := claimsFrom(r.Context())
	var req struct {
		IncludeServiceTokens bool `json:"include_service_tokens"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, 400, "invalid json")
			return
		}
	}
	if _, err := s.store.db.ExecContext(r.Context(), `UPDATE users SET session_version=session_version+1 WHERE id=?`, claims.UserID); err != nil {
		writeErr(w, 500, "revoke failed")
		return
	}
	revokedServices := int64(0)
	if req.IncludeServiceTokens {
		res, err := s.store.db.ExecContext(r.Context(), `UPDATE service_tokens SET revoked_at=? WHERE revoked_at IS NULL`, nowUTC())
		if err != nil {
			writeErr(w, 500, "revoke failed")
			return
		}
		revokedServices, _ = res.RowsAffected()
	}
	s.revocations.notify()
	s.store.Audit(r.Context(), "sessions.revoke_all", claims.Username, "", fmt.Sprintf("service_tokens=%d", revokedServices))
	writeJSON(w, 200, map[string]any{"ok": true, "revoked_service_tokens": revokedServices})
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
	q := `SELECT id,name,status,metadata_json,observed_ip,created_at,expires_at,approved_at,denied_at,setup_token_expires_at FROM requests`
	var args []any
	switch st := r.URL.Query().Get("status"); st {
	case "pending", "approved", "denied", "expired", "provisioned", "provisioning":
		q += ` WHERE status=?`
		args = append(args, st)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := s.store.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		writeErr(w, 500, "query failed")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, status, created, expires string
		var meta, observedIP, approved, denied, setupExp sql.NullString
		if err := rows.Scan(&id, &name, &status, &meta, &observedIP, &created, &expires, &approved, &denied, &setupExp); err != nil {
			writeErr(w, 500, "query failed")
			return
		}
		mv := map[string]any{}
		if meta.Valid {
			_ = json.Unmarshal([]byte(meta.String), &mv)
		}
		slug, _ := mv["slug"].(string)
		if slug == "" {
			slug = NormalizeSlug(name)
		}
		item := map[string]any{"id": id, "name": name, "status": status, "metadata": mv, "observed_ip": observedIP.String, "slug": slug, "target_user": mv["target_user"], "mode": mv["mode"], "local_port": mv["local_port"], "created_at": created, "expires_at": expires, "approved_at": approved.String, "denied_at": denied.String, "setup_token_expires_at": setupExp.String}
		if observedIP.String == "" {
			// Older rows only have the (forgeable) metadata copy.
			item["observed_ip"] = nil
		}
		out = append(out, item)
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
		// An expired request cannot be approved even before maintenance
		// marks it (daybreak P2-4).
		res, err := s.store.db.ExecContext(r.Context(), `UPDATE requests SET status='approved', setup_token_hash=NULL, setup_token_expires_at=NULL, approved_at=? WHERE id=? AND status='pending' AND expires_at>?`, now, id, now)
		if err != nil {
			writeErr(w, 500, "approve failed")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			writeErr(w, 404, "pending, unexpired request not found")
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
	include := r.URL.Query().Get("include_retired")
	ms, err := s.prov.ListMachines(r.Context(), include == "1" || include == "true")
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
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "machine not found")
			return
		}
		if errors.Is(err, ErrCleanupFailed) {
			s.publishEvent(r.Context(), "ssh_config.changed", id, map[string]any{"reason": action, "machine_id": id})
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("machine %s %s: %v", id, action, err)
		writeErr(w, 500, "action failed")
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
		var current int64
		if err := s.store.db.QueryRowContext(ctx, `SELECT session_version FROM users WHERE id=?`, claims.UserID).Scan(&current); err != nil {
			return nil, "", errors.New("unknown user")
		}
		if claims.SessionVersion != current {
			return nil, "", errors.New("session revoked")
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
		return path == "/api/admin/ssh-config" || path == "/api/admin/ssh-config.json" || path == "/api/admin/events" || path == "/api/admin/logout"
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
	// Throttled: at most one write per token per minute (perf #6). The
	// conditional WHERE makes concurrent requests a no-op.
	now := time.Now().UTC()
	_, _ = s.store.db.ExecContext(ctx, `UPDATE service_tokens SET last_used_at=? WHERE id=? AND (last_used_at IS NULL OR last_used_at<?)`, now.Format(time.RFC3339), id, now.Add(-serviceTokenTouchInterval).Format(time.RFC3339))
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
	// No WriteTimeout: /api/admin/events is a long-lived SSE stream.
	srv := &http.Server{Addr: s.cfg.ListenAddr, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
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

// revocationNotifier broadcasts "some credential was revoked" to every open
// event stream. Each stream then re-checks its own token; streams whose token
// is still valid keep running. A closed channel is the broadcast; notify
// swaps in a fresh one.
type revocationNotifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newRevocationNotifier() *revocationNotifier {
	return &revocationNotifier{ch: make(chan struct{})}
}

// current returns the channel that the next notify will close. A nil
// notifier yields a nil channel, which never fires.
func (n *revocationNotifier) current() <-chan struct{} {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *revocationNotifier) notify() {
	if n == nil {
		return
	}
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}
