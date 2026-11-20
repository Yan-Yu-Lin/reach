package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// SSH config generation.
//
// Two representations are served:
//   - /api/admin/ssh-config       legacy text, kept so older reach-mac-agent
//     builds keep working;
//   - /api/admin/ssh-config.json  structured hosts. New mac-agents render
//     their own config from a fixed template and never include raw
//     server-supplied text (daybreak P1-3, Opus P1-3).
//
// Both are built from the same validated SSHHost list. Every desired=active
// machine with a live tunnel row is emitted regardless of observed state so a
// short outage does not delete the alias from the operator's config (perf #4);
// the observed state goes in a comment.

const sshConfigSchema = 1

// SSHHost is one generated SSH alias. All string fields are validated before
// they leave the server; the Mac agent validates them again.
type SSHHost struct {
	Host          string      `json:"host"`
	HostName      string      `json:"hostname"`
	Port          int         `json:"port"`
	User          string      `json:"user"`
	ProxyJump     string      `json:"proxy_jump"`
	HostKeyAlias  string      `json:"host_key_alias"`
	MachineID     string      `json:"machine_id"`
	ObservedState string      `json:"observed_state"`
	Options       [][2]string `json:"options,omitempty"`
}

type SSHConfigDocument struct {
	Schema int       `json:"schema"`
	Hosts  []SSHHost `json:"hosts"`
}

// sshClientOptionAllowlist names the only directives an agent may contribute
// to the operator's config, and the value grammar for each.
var (
	sshAlgoListRE  = regexp.MustCompile(`^[+\-^]?[a-z0-9@.\-]+(,[a-z0-9@.\-]+)*$`)
	sshMachineIDRE = regexp.MustCompile(`^m_[a-z0-9_]{1,40}$`)
)

var sshClientOptionAllowlist = map[string]bool{
	"KexAlgorithms":            true,
	"HostKeyAlgorithms":        true,
	"PubkeyAcceptedAlgorithms": true,
	"PubkeyAcceptedKeyTypes":   true,
	"Ciphers":                  true,
	"MACs":                     true,
}

const maxSSHOptionValueLen = 512

// validSSHClientOptionValue rejects anything that is not a comma-separated
// algorithm list (Opus P1-1). A bogus but well-formed algorithm name would
// still make OpenSSH reject the whole file, which is why the Mac agent also
// runs `ssh -G` on the rendered result before installing it.
func validSSHClientOptionValue(v string) bool {
	return len(v) <= maxSSHOptionValueLen && sshAlgoListRE.MatchString(v)
}

func sshConfigClientOptions(raw string) [][2]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var hb AgentHeartbeat
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		return nil
	}
	return filterSSHClientOptions(hb.Capabilities.SSH.ClientOptions)
}

// sshCompatProfile is the closed set of client options a target may ask the
// operator's ssh to use. These are exactly the legacy-OpenSSH compatibility
// values the official agent emits (cmd/reach-agent/ssh_compat.go). Anything
// else, even a well-formed algorithm name, is dropped, so a compromised
// target cannot inject an unknown algorithm that makes OpenSSH reject the
// operator's whole config (daybreak P1-4).
var sshCompatProfile = map[string]bool{
	"KexAlgorithms=diffie-hellman-group14-sha1":                                      true,
	"KexAlgorithms=diffie-hellman-group-exchange-sha256,diffie-hellman-group14-sha1": true,
	"HostKeyAlgorithms=ssh-rsa":                                                      true,
	"HostKeyAlgorithms=+ssh-rsa":                                                     true,
	"PubkeyAcceptedAlgorithms=+ssh-rsa":                                              true,
	"PubkeyAcceptedKeyTypes=+ssh-rsa":                                                true,
	"Ciphers=aes128-ctr":                                                             true,
	"Ciphers=aes128-ctr,aes192-ctr,aes256-ctr":                                       true,
	"MACs=hmac-sha1":                                                                 true,
}

func filterSSHClientOptions(opts []string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, opt := range opts {
		opt = strings.TrimSpace(opt)
		name, value, ok := strings.Cut(opt, "=")
		if !ok || !sshClientOptionAllowlist[name] || seen[name] || !validSSHClientOptionValue(value) || !sshCompatProfile[opt] {
			continue
		}
		seen[name] = true
		out = append(out, [2]string{name, value})
	}
	return out
}

func sshOptionsSignature(raw string) string {
	opts := sshConfigClientOptions(raw)
	parts := make([]string, 0, len(opts))
	for _, opt := range opts {
		parts = append(parts, opt[0]+"="+opt[1])
	}
	return strings.Join(parts, ";")
}

// SSHHosts returns the validated host list. A row that fails validation is
// skipped and logged instead of failing the whole document, so one bad row
// cannot remove every alias.
func (s *Server) SSHHosts(ctx context.Context) ([]SSHHost, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT m.id,m.slug,m.target_user,m.observed_state,t.remote_port,h.proxyjump_alias,ao.raw_json
		FROM machines m JOIN tunnels t ON t.machine_id=m.id JOIN hubs h ON h.id=t.hub_id
		LEFT JOIN agent_observations ao ON ao.machine_id=m.id
		WHERE m.desired_state='active' AND t.status NOT IN ('disabled','retired','failed') AND t.remote_port>0
		ORDER BY m.slug,t.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := []SSHHost{}
	seen := map[string]bool{}
	for rows.Next() {
		var id, slug string
		var target, observed, pj, rawObservation sql.NullString
		var port int
		if err := rows.Scan(&id, &slug, &target, &observed, &port, &pj, &rawObservation); err != nil {
			return nil, err
		}
		if seen[slug] {
			continue
		}
		h, err := s.buildSSHHost(id, slug, target.String, observed.String, port, pj.String, rawObservation.String)
		if err != nil {
			log.Printf("ssh-config: skipping machine %s: %v", id, err)
			continue
		}
		seen[slug] = true
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (s *Server) buildSSHHost(id, slug, target, observed string, port int, jump, raw string) (SSHHost, error) {
	if err := ValidateSlug(slug); err != nil {
		return SSHHost{}, err
	}
	if !sshMachineIDRE.MatchString(id) {
		return SSHHost{}, fmt.Errorf("invalid machine id")
	}
	if port <= 0 || port > 65535 {
		return SSHHost{}, fmt.Errorf("invalid port %d", port)
	}
	user := target
	if user == "" {
		user = "root"
	}
	if err := ValidateSSHConfigToken("target user", user); err != nil {
		return SSHHost{}, err
	}
	if jump == "" {
		jump = s.cfg.DefaultHub.ProxyJumpAlias
	}
	if err := ValidateSSHConfigToken("proxyjump_alias", jump); err != nil {
		return SSHHost{}, err
	}
	switch observed {
	case ObservedOnline, ObservedDegraded, ObservedOffline, ObservedGone, ObservedUnknown:
	default:
		observed = ObservedUnknown
	}
	return SSHHost{Host: slug, HostName: "localhost", Port: port, User: user, ProxyJump: jump, HostKeyAlias: "reach-" + id, MachineID: id, ObservedState: observed, Options: sshConfigClientOptions(raw)}, nil
}

// RenderSSHConfig renders hosts with the fixed template shared with the Mac
// agent. ForwardAgent/ForwardX11/ClearAllForwardings pin forwarding off for
// Reach aliases even when the operator's global config enables them
// (daybreak P3-2).
func RenderSSHConfig(hosts []SSHHost) string {
	var b strings.Builder
	b.WriteString("# Generated by Reach. Do not edit by hand.\n# Includes every desired-active machine; the observed state is shown in a comment.\n\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "# %s observed=%s\nHost %s\n    HostName %s\n    Port %d\n    User %s\n    ProxyJump %s\n    HostKeyAlias %s\n    StrictHostKeyChecking accept-new\n    ForwardAgent no\n    ForwardX11 no\n    ClearAllForwardings yes\n", h.MachineID, h.ObservedState, h.Host, h.HostName, h.Port, h.User, h.ProxyJump, h.HostKeyAlias)
		for _, opt := range h.Options {
			fmt.Fprintf(&b, "    %s %s\n", opt[0], opt[1])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// GenerateSSHConfig renders the legacy text document. Target-reported
// options are omitted unless legacy_ssh_config_options is set.
func (s *Server) GenerateSSHConfig(ctx context.Context) (string, error) {
	hosts, err := s.SSHHosts(ctx)
	if err != nil {
		return "", err
	}
	if !s.cfg.LegacySSHConfigOptions {
		for i := range hosts {
			hosts[i].Options = nil
		}
	}
	return RenderSSHConfig(hosts), nil
}

func (s *Server) handleSSHConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.GenerateSSHConfig(r.Context())
	if err != nil {
		log.Printf("ssh-config: %v", err)
		writeErr(w, 500, "could not generate ssh config")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(cfg))
}

func (s *Server) handleSSHConfigJSON(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.SSHHosts(r.Context())
	if err != nil {
		log.Printf("ssh-config.json: %v", err)
		writeErr(w, 500, "could not generate ssh config")
		return
	}
	writeJSON(w, 200, SSHConfigDocument{Schema: sshConfigSchema, Hosts: hosts})
}
