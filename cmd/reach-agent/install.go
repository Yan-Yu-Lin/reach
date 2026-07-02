package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type installOptions struct {
	APIURL      string
	Name        string
	Token       string
	TargetUser  string
	Transport   string
	Yes         bool
	InstallMode string
	ConfigDir   string
	DataDir     string
	AgentPath   string
}

type registerResponse struct {
	Status              string `json:"status"`
	RequestID           string `json:"request_id"`
	ClientSecret        string `json:"client_secret"`
	SetupToken          string `json:"setup_token"`
	SetupTokenExpiresAt string `json:"setup_token_expires_at"`
}

type provisionResponse struct {
	Machine        machineInfo            `json:"machine"`
	Tunnel         tunnelInfo             `json:"tunnel"`
	Hub            hubInfo                `json:"hub"`
	AgentToken     string                 `json:"agent_token"`
	AdminPubkeys   []adminPubkey          `json:"admin_pubkeys"`
	HubHostKeys    []string               `json:"hub_host_keys"`
	LegacyHostKeys []string               `json:"jason_host_keys"`
	WebSocket      *websocketTunnelConfig `json:"websocket_tunnel"`
}

type machineInfo struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ExpiresAt string `json:"expires_at"`
}

type tunnelInfo struct {
	RemotePort int    `json:"remote_port"`
	UnixUser   string `json:"unix_user"`
}

type hubInfo struct {
	PublicHost string `json:"public_host"`
	SSHPort    int    `json:"ssh_port"`
	APIURL     string `json:"api_url"`
}

type adminPubkey struct {
	PublicKey string `json:"public_key"`
}

type websocketTunnelConfig struct {
	Enabled    bool                       `json:"enabled"`
	Carrier    string                     `json:"carrier"`
	URL        string                     `json:"url"`
	PathPrefix string                     `json:"path_prefix"`
	Binaries   map[string]websocketBinary `json:"binaries"`
}

type websocketBinary struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type localSSHPlan struct {
	Mode          string
	LocalPort     int
	AuthFile      string
	UserSSHD      bool
	InternalSSHD  bool
	SSHDBinary    string
	SSHDConfig    string
	SSHDLog       string
	HostKeyPath   string
	HostKeyType   string
	ClientOptions []string
}

func installCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	opt := installOptions{}
	fs.StringVar(&opt.APIURL, "api-url", envDefault("REACH_API_URL", "https://tunnels.your-domain.example"), "Reach API URL")
	fs.StringVar(&opt.Name, "name", "", "machine name / SSH alias")
	fs.StringVar(&opt.Token, "token", "", "god/invite code; empty requests approval")
	fs.StringVar(&opt.Token, "god-code", "", "alias for --token")
	fs.StringVar(&opt.TargetUser, "target-user", "", "local user the operator should SSH as")
	fs.StringVar(&opt.Transport, "transport", envDefault("REACH_TRANSPORT", "auto"), "auto, direct, or websocket")
	fs.StringVar(&opt.InstallMode, "mode", "auto", "install mode: auto, system, or user")
	fs.BoolVar(&opt.Yes, "yes", false, "non-interactive defaults")
	fs.BoolVar(&opt.Yes, "y", false, "non-interactive defaults")
	fs.StringVar(&opt.ConfigDir, "config-dir", "", "config directory")
	fs.StringVar(&opt.DataDir, "data-dir", "", "data directory")
	fs.StringVar(&opt.AgentPath, "agent-path", "", "path to reach-agent binary for service ExecStart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("reach-agent install currently supports Linux only")
	}
	if opt.InstallMode == "auto" {
		if os.Geteuid() == 0 {
			opt.InstallMode = "system"
		} else {
			opt.InstallMode = "user"
		}
	}
	switch opt.InstallMode {
	case "system":
		if os.Geteuid() != 0 {
			return fmt.Errorf("system install needs root; use --mode user or rerun via sudo")
		}
	case "user":
	default:
		return fmt.Errorf("--mode must be auto, system, or user")
	}
	applyModeDefaults(&opt)
	switch opt.Transport {
	case "auto", "direct", "websocket":
	default:
		return fmt.Errorf("--transport must be auto, direct, or websocket")
	}
	if opt.Name == "" {
		opt.Name = hostnameDefault()
	}
	if opt.TargetUser == "" {
		opt.TargetUser = defaultTargetUser()
	}
	if !opt.Yes {
		var err error
		opt.Name, err = promptDefault("Machine name / SSH alias", opt.Name, false)
		if err != nil {
			return err
		}
		opt.TargetUser, err = promptDefault("Local user the operator should SSH as", opt.TargetUser, false)
		if err != nil {
			return err
		}
		if opt.Token == "" {
			opt.Token, err = promptDefault("Reach auth code (leave empty to request operator approval)", "", true)
			if err != nil {
				return err
			}
		}
	}
	if err := validateTargetUserLocal(opt.TargetUser); err != nil {
		return err
	}
	targetHome, err := userHome(opt.TargetUser)
	if err != nil {
		return err
	}
	if opt.InstallMode == "user" && opt.TargetUser != currentUsername() {
		return fmt.Errorf("user-mode install can only configure the current user (%s), not %s", currentUsername(), opt.TargetUser)
	}
	if err := os.MkdirAll(opt.ConfigDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(opt.DataDir, 0o700); err != nil {
		return err
	}
	sshCompat := detectSSHCompatibility(ctx)
	fmt.Printf("[reach] local SSH: %s; tunnel key=%s\n", firstNonEmpty(sshCompat.ClientVersion, "unknown"), sshCompat.TunnelKeyType)
	plan, err := prepareLocalSSH(ctx, opt, targetHome, sshCompat)
	if err != nil {
		return err
	}

	fmt.Printf("[reach] registering %s with %s\n", opt.Name, opt.APIURL)
	reg, err := registerAndWait(ctx, opt)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(opt.DataDir, "tunnel_key")
	if err := ensureTunnelKey(ctx, keyPath, opt.Name, sshCompat); err != nil {
		return err
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}

	prov, err := provision(ctx, opt, reg, strings.TrimSpace(string(pub)), plan)
	if err != nil {
		return err
	}
	if prov.AgentToken == "" {
		return fmt.Errorf("provision response did not include agent_token; update reachd first")
	}
	hostKeys := prov.HubHostKeys
	if len(hostKeys) == 0 {
		hostKeys = prov.LegacyHostKeys
	}
	if err := writeKnownHosts(filepath.Join(opt.ConfigDir, "known_hosts"), hostKeys, sshCompat); err != nil {
		return err
	}
	supportedAdminKeyTypes := sshCompat.SupportedAuthorizedKeyTypes
	if plan.InternalSSHD {
		supportedAdminKeyTypes = internalSSHDSupportedKeyTypes
	}
	if err := installAdminKeys(plan.AuthFile, opt.TargetUser, prov, opt.InstallMode == "system" && plan.Mode != "user-sshd", supportedAdminKeyTypes, plan.InternalSSHD); err != nil {
		return err
	}
	wsPath := ""
	if prov.WebSocket != nil && prov.WebSocket.Enabled && opt.Transport != "direct" {
		carrier := prov.WebSocket.Carrier
		if carrier == "" || carrier == "reach" {
			wsPath = opt.AgentPath
			if st, err := os.Stat(wsPath); err != nil || st.IsDir() {
				if exe, exeErr := os.Executable(); exeErr == nil {
					wsPath = exe
				}
			}
		} else if carrier == "wstunnel" {
			wsPath = filepath.Join(opt.DataDir, "wstunnel")
			if err := installWSTunnel(ctx, wsPath, prov.WebSocket); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported websocket carrier %q", carrier)
		}
	}
	cfgPath := filepath.Join(opt.ConfigDir, "agent.yaml")
	cfg := buildAgentConfig(opt, prov, keyPath, filepath.Join(opt.ConfigDir, "known_hosts"), wsPath, plan, sshCompat)
	if err := writeYAML0600(cfgPath, cfg); err != nil {
		return err
	}
	if err := installPersistence(ctx, opt, cfgPath); err != nil {
		return err
	}
	writeInstallEnv(opt, prov, cfgPath, plan)
	localSummary := plan.Mode
	if plan.InternalSSHD {
		localSummary += " (shell only; no sftp; loopback-only)"
	}
	fmt.Printf("\n✅ Reach agent installed.\n\n  Machine ID: %s\n  SSH alias:  %s\n  Hub port:   %d\n  Transport:  %s\n  Local SSH:  %s\n\nThe operator can now run: ssh %s\n", prov.Machine.ID, prov.Machine.Slug, prov.Tunnel.RemotePort, opt.Transport, localSummary, prov.Machine.Slug)
	return nil
}

func applyModeDefaults(opt *installOptions) {
	if opt.InstallMode == "system" {
		if opt.ConfigDir == "" {
			opt.ConfigDir = "/etc/reach"
		}
		if opt.DataDir == "" {
			opt.DataDir = "/var/lib/reach"
		}
		if opt.AgentPath == "" {
			opt.AgentPath = "/opt/reach/reach-agent"
		}
		return
	}
	home, _ := os.UserHomeDir()
	if opt.ConfigDir == "" {
		opt.ConfigDir = filepath.Join(home, ".config", "reach")
	}
	if opt.DataDir == "" {
		opt.DataDir = filepath.Join(home, ".local", "share", "reach")
	}
	if opt.AgentPath == "" {
		opt.AgentPath = filepath.Join(home, ".local", "bin", "reach-agent")
	}
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func hostnameDefault() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return normalizeSlug(h)
	}
	return "linux"
}

func defaultTargetUser() string {
	if os.Geteuid() == 0 {
		if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
			return u
		}
	}
	if u := currentUsername(); u != "" {
		return u
	}
	return "root"
}

func currentUsername() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if out, err := exec.Command("id", "-un").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func promptDefault(label, def string, secret bool) (string, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("interactive input requires /dev/tty; pass flags or --yes")
	}
	defer f.Close()
	if def != "" {
		fmt.Fprintf(f, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(f, "%s: ", label)
	}
	if secret {
		_ = exec.Command("sh", "-c", "stty -echo < /dev/tty").Run()
		defer exec.Command("sh", "-c", "stty echo < /dev/tty").Run()
	}
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if secret {
		fmt.Fprintln(f)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = def
	}
	return line, nil
}

func validateTargetUserLocal(s string) error {
	if s == "" || len(s) > 128 || strings.HasPrefix(s, "-") || strings.Contains(s, ":") || strings.ContainsAny(s, "\x00\r\n\t ") {
		return fmt.Errorf("invalid target user %q", s)
	}
	return nil
}

func userHome(user string) (string, error) {
	out, err := exec.Command("getent", "passwd", user).Output()
	if err != nil {
		if user == currentUsername() {
			if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
				return home, nil
			}
		}
		return "", fmt.Errorf("user %q does not exist according to getent passwd", user)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(parts) < 6 || parts[5] == "" {
		return "", fmt.Errorf("could not determine home for user %q", user)
	}
	return parts[5], nil
}

func prepareLocalSSH(ctx context.Context, opt installOptions, targetHome string, compat SSHCompatConfig) (localSSHPlan, error) {
	if opt.InstallMode == "system" {
		if err := ensureSystemSSHD(ctx); err != nil {
			return localSSHPlan{}, err
		}
		if err := probeSSH(ctx, "127.0.0.1", 22, 5*time.Second); err != nil {
			return localSSHPlan{}, fmt.Errorf("local sshd did not answer on 127.0.0.1:22 after start/enable: %w", err)
		}
		banner := sshServerBanner(ctx, "127.0.0.1", 22, 2*time.Second)
		return localSSHPlan{Mode: "system-existing", LocalPort: 22, AuthFile: filepath.Join(targetHome, ".ssh", "authorized_keys"), ClientOptions: clientOptionsForServerBanner(banner, "")}, nil
	}
	if err := probeSSH(ctx, "127.0.0.1", 22, 3*time.Second); err == nil {
		fmt.Println("[reach] using existing system sshd on 127.0.0.1:22 (user-mode install cannot manage it if it stops)")
		banner := sshServerBanner(ctx, "127.0.0.1", 22, 2*time.Second)
		return localSSHPlan{Mode: "system-existing", LocalPort: 22, AuthFile: filepath.Join(targetHome, ".ssh", "authorized_keys"), ClientOptions: clientOptionsForServerBanner(banner, "")}, nil
	}
	bin := findSSHD()
	if bin == "" {
		return prepareInternalSSHD(ctx, opt, compat)
	}
	port, err := findFreePort(22220, 22320)
	if err != nil {
		return localSSHPlan{}, err
	}
	dir := filepath.Join(opt.DataDir, "user-sshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return localSSHPlan{}, err
	}
	hostKeyType := compat.UserSSHDHostKeyType
	if hostKeyType == "" {
		hostKeyType = "ssh-rsa"
	}
	hostKey := filepath.Join(dir, "ssh_host_"+keyTypeFileStem(hostKeyType)+"_key")
	if _, err := os.Stat(hostKey); errors.Is(err, os.ErrNotExist) {
		if err := generateSSHKey(ctx, hostKey, hostKeyType, "reach-user-sshd:"+normalizeSlug(opt.Name)); err != nil {
			return localSSHPlan{}, fmt.Errorf("generate user sshd host key failed: %w", err)
		}
	}
	authFile := filepath.Join(dir, "authorized_keys")
	cfgPath := filepath.Join(dir, "sshd_config")
	logPath := filepath.Join(dir, "sshd.log")
	config := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PasswordAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
UsePAM no
PermitRootLogin no
AllowTcpForwarding no
X11Forwarding no
PermitTunnel no
PrintLastLog no
PrintMotd no
PermitUserEnvironment no
StrictModes no
Subsystem sftp internal-sftp
LogLevel VERBOSE
`, port, hostKey, filepath.Join(dir, "sshd.pid"), authFile)
	if err := os.WriteFile(cfgPath, []byte(config), 0o600); err != nil {
		return localSSHPlan{}, err
	}
	fmt.Printf("[reach] no system sshd detected; using private user sshd on 127.0.0.1:%d\n", port)
	return localSSHPlan{Mode: "user-sshd", LocalPort: port, AuthFile: authFile, UserSSHD: true, SSHDBinary: bin, SSHDConfig: cfgPath, SSHDLog: logPath, HostKeyType: hostKeyType, ClientOptions: clientOptionsForServerBanner(compat.ClientVersion, hostKeyType)}, nil
}

func prepareInternalSSHD(_ context.Context, opt installOptions, _ SSHCompatConfig) (localSSHPlan, error) {
	port, err := findFreePort(22220, 22320)
	if err != nil {
		return localSSHPlan{}, err
	}
	dir := filepath.Join(opt.DataDir, "internal-sshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return localSSHPlan{}, err
	}
	hostKey := filepath.Join(dir, "host_ed25519_key")
	if err := ensureInternalHostKey(hostKey); err != nil {
		return localSSHPlan{}, fmt.Errorf("generate internal sshd host key failed: %w", err)
	}
	authFile := filepath.Join(dir, "authorized_keys")
	logPath := filepath.Join(dir, "sshd.log")
	fmt.Printf("[reach] no sshd and no sudo; using reach-agent internal ssh server on 127.0.0.1:%d (shell only; no sftp; loopback-only)\n", port)
	return localSSHPlan{Mode: "internal-sshd", LocalPort: port, AuthFile: authFile, InternalSSHD: true, HostKeyPath: hostKey, SSHDLog: logPath}, nil
}

func findSSHD() string {
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func findFreePort(start, end int) (int, error) {
	for p := start; p <= end; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			_ = ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free user sshd port in %d-%d", start, end)
}

func registerAndWait(ctx context.Context, opt installOptions) (registerResponse, error) {
	payload := map[string]string{"name": opt.Name, "auth_code": opt.Token, "target_user": opt.TargetUser}
	attempt := 0
	for ctx.Err() == nil {
		attempt++
		var reg registerResponse
		if err := postJSON(ctx, opt.APIURL+"/api/client/register", payload, &reg, ""); err != nil {
			return reg, err
		}
		if reg.Status != "pending" {
			return reg, nil
		}
		if attempt == 1 {
			fmt.Printf("[reach] approval requested. Waiting for an operator to approve %q in the dashboard", opt.Name)
		} else {
			fmt.Printf("\n[reach] previous approval request expired; created a fresh request and continuing to wait")
		}
		poll := map[string]string{"client_secret": reg.ClientSecret}
		for ctx.Err() == nil {
			time.Sleep(3 * time.Second)
			fmt.Print(".")
			if err := postJSON(ctx, opt.APIURL+"/api/client/register/"+reg.RequestID+"/poll", poll, &reg, ""); err != nil {
				return reg, err
			}
			switch reg.Status {
			case "approved":
				fmt.Println(" approved")
				return reg, nil
			case "pending":
				continue
			case "denied":
				return reg, fmt.Errorf("Reach request was denied")
			case "expired":
				goto nextRequest
			default:
				return reg, fmt.Errorf("unexpected registration status %q", reg.Status)
			}
		}
	nextRequest:
		continue
	}
	return registerResponse{}, ctx.Err()
}

func provision(ctx context.Context, opt installOptions, reg registerResponse, pubkey string, plan localSSHPlan) (provisionResponse, error) {
	var prov provisionResponse
	if reg.SetupToken == "" {
		return prov, fmt.Errorf("registration did not return setup_token")
	}
	payload := map[string]any{
		"request_id":   reg.RequestID,
		"setup_token":  reg.SetupToken,
		"slug":         normalizeSlug(opt.Name),
		"display_name": opt.Name,
		"target_user":  opt.TargetUser,
		"pubkey":       pubkey,
		"mode":         plan.Mode,
		"local_port":   plan.LocalPort,
		"persistence":  "reach-agent-" + opt.InstallMode,
		"distro":       distroString(),
		"arch":         runtime.GOARCH,
	}
	if err := postJSON(ctx, opt.APIURL+"/api/client/provision", payload, &prov, ""); err != nil {
		return prov, err
	}
	return prov, nil
}

func postJSON(ctx context.Context, url string, payload any, out any, bearer string) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
}

func ensureTunnelKey(ctx context.Context, keyPath, name string, compat SSHCompatConfig) error {
	keyType := compat.TunnelKeyType
	if keyType == "" {
		keyType = "ssh-rsa"
	}
	if _, err := os.Stat(keyPath); err == nil {
		pubType := publicKeyType(keyPath + ".pub")
		if pubType == "" || keyTypeSupported(pubType, compat.SupportedAuthorizedKeyTypes) {
			return nil
		}
		backup := fmt.Sprintf("%s.unsupported.%d", keyPath, time.Now().Unix())
		_ = os.Rename(keyPath, backup)
		_ = os.Rename(keyPath+".pub", backup+".pub")
		fmt.Printf("[reach warning] existing tunnel key type %s is not supported by this ssh; moved it to %s\n", pubType, backup)
	}
	return generateSSHKey(ctx, keyPath, keyType, "reach:"+normalizeSlug(name))
}

func keyTypeFileStem(keyType string) string {
	switch keyType {
	case "ssh-ed25519":
		return "ed25519"
	case "ecdsa-sha2-nistp256":
		return "ecdsa"
	default:
		return "rsa"
	}
}

func ensureSystemSSHD(ctx context.Context) error {
	if err := startSSHServices(ctx); err == nil {
		return nil
	}
	if _, err := exec.LookPath("apt-get"); err == nil {
		cmd := exec.CommandContext(ctx, "apt-get", "update")
		_ = cmd.Run()
		cmd = exec.CommandContext(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "openssh-server")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("install openssh-server failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return startSSHServices(ctx)
	}
	return startSSHServices(ctx)
}

func startSSHServices(ctx context.Context) error {
	services := []string{"ssh", "sshd"}
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, svc := range services {
			cmd := exec.CommandContext(ctx, "systemctl", "enable", "--now", svc)
			if out, err := cmd.CombinedOutput(); err == nil {
				return nil
			} else if len(out) > 0 {
				fmt.Printf("[reach warning] systemctl enable --now %s failed: %s\n", svc, strings.TrimSpace(string(out)))
			}
		}
	}
	for _, svc := range services {
		cmd := exec.CommandContext(ctx, "service", svc, "start")
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else if len(out) > 0 {
			fmt.Printf("[reach warning] service %s start failed: %s\n", svc, strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("could not start ssh/sshd service")
}

func writeKnownHosts(path string, keys []string, compat SSHCompatConfig) error {
	if len(keys) == 0 {
		return fmt.Errorf("API returned no hub_host_keys; refusing to install without host key pinning")
	}
	if requiresKnownHostType(compat, "ssh-rsa") && !knownHostsContainType(keys, "ssh-rsa") {
		return fmt.Errorf("local ssh requires ssh-rsa host keys, but API did not return a hub ssh-rsa host key; update reachd hub_host_keys")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0o644)
}

func requiresKnownHostType(compat SSHCompatConfig, keyType string) bool {
	for _, opt := range compat.TunnelOptions {
		if strings.HasPrefix(opt, "HostKeyAlgorithms=") && strings.Contains(opt, keyType) {
			return true
		}
	}
	return false
}

func knownHostsContainType(keys []string, keyType string) bool {
	for _, k := range keys {
		fields := strings.Fields(k)
		for _, f := range fields {
			if f == keyType {
				return true
			}
		}
	}
	return false
}

func installAdminKeys(authFile, user string, prov provisionResponse, chownFiles bool, supportedKeyTypes []string, bareKeys bool) error {
	if len(prov.AdminPubkeys) == 0 {
		return fmt.Errorf("API returned no admin public keys")
	}
	sshDir := filepath.Dir(authFile)
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	marker := "reach:" + prov.Machine.ID
	existing := ""
	if b, err := os.ReadFile(authFile); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" || strings.Contains(line, " "+marker) {
				continue
			}
			existing += line + "\n"
		}
	}
	var b strings.Builder
	b.WriteString(existing)
	installed := 0
	for _, k := range prov.AdminPubkeys {
		pub := strings.TrimSpace(k.PublicKey)
		if pub == "" {
			continue
		}
		fields := strings.Fields(pub)
		if len(fields) == 0 || !keyTypeSupported(fields[0], supportedKeyTypes) {
			fmt.Printf("[reach warning] skipping admin key unsupported by local sshd: %s\n", firstField(pub))
			continue
		}
		if bareKeys {
			// internal-sshd binds loopback-only and enforces restrictions at the
			// server layer; options only add parser-compatibility risk.
			fmt.Fprintf(&b, "%s %s\n", pub, marker)
		} else {
			// Keep target-side authorized_keys options conservative. Reach targets may
			// run old OpenSSH releases (Ubuntu 18.04 ships OpenSSH 7.6), and unknown
			// authorized_keys options cause the entire key line to be ignored. In
			// particular, expiry-time is not safe to emit here. Expiry is enforced on
			// the hub tunnel account; target keys are loopback-only.
			fmt.Fprintf(&b, "from=\"127.0.0.1,::1\",no-agent-forwarding,no-X11-forwarding,no-port-forwarding %s %s\n", pub, marker)
		}
		installed++
	}
	if installed == 0 {
		serverName := "local sshd"
		advice := "Add an ssh-rsa admin key for OpenSSH 5.x targets"
		if bareKeys {
			serverName = "internal sshd"
			advice = "Add an ed25519, ECDSA P-256, or RSA admin key"
		}
		return fmt.Errorf("none of the admin public keys are supported by this %s; supported=%s. %s", serverName, strings.Join(supportedKeyTypes, ","), advice)
	}
	if err := os.WriteFile(authFile, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if chownFiles {
		_ = exec.Command("chown", "-R", user+":"+user, sshDir).Run()
	}
	return nil
}

func installWSTunnel(ctx context.Context, dest string, ws *websocketTunnelConfig) error {
	key := runtimeArchKey()
	bin, ok := ws.Binaries[key]
	if !ok {
		return fmt.Errorf("no wstunnel binary for %s", key)
	}
	fmt.Printf("[reach] installing wstunnel carrier for %s\n", key)
	data, err := download(ctx, bin.URL)
	if err != nil {
		return err
	}
	if bin.SHA256 != "" {
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != bin.SHA256 {
			return fmt.Errorf("wstunnel checksum mismatch: got %s expected %s", got, bin.SHA256)
		}
	}
	wst, err := extractTarGzFile(data, "wstunnel")
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, wst, 0o755); err != nil {
		return err
	}
	return nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("download %s returned %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func extractTarGzFile(data []byte, base string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == base && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 64<<20))
		}
	}
	return nil, fmt.Errorf("archive did not contain %s", base)
}

func runtimeArchKey() string {
	switch runtime.GOARCH {
	case "amd64":
		return "linux_amd64"
	case "arm64":
		return "linux_arm64"
	case "386":
		return "linux_386"
	case "arm":
		return "linux_armv7"
	default:
		return "linux_" + runtime.GOARCH
	}
}

func buildAgentConfig(opt installOptions, prov provisionResponse, keyPath, knownHosts, wsPath string, plan localSSHPlan, compat SSHCompatConfig) Config {
	cfg := defaultConfig()
	cfg.MachineID = prov.Machine.ID
	cfg.Slug = prov.Machine.Slug
	cfg.Tunnel.HubHost = prov.Hub.PublicHost
	cfg.Tunnel.HubSSHPort = prov.Hub.SSHPort
	cfg.Tunnel.TunnelUser = prov.Tunnel.UnixUser
	cfg.Tunnel.RemotePort = prov.Tunnel.RemotePort
	cfg.Tunnel.LocalHost = "127.0.0.1"
	cfg.Tunnel.LocalPort = plan.LocalPort
	cfg.Tunnel.KeyPath = keyPath
	cfg.Tunnel.KnownHosts = knownHosts
	cfg.Tunnel.SSHCompat = compat
	cfg.Tunnel.LogPath = filepath.Join(opt.DataDir, "tunnel.log")
	cfg.LocalSSH.Host = "127.0.0.1"
	cfg.LocalSSH.Port = plan.LocalPort
	cfg.LocalSSH.Manage = opt.InstallMode == "system" || plan.UserSSHD || plan.InternalSSHD
	cfg.LocalSSH.UserSSHD = plan.UserSSHD
	cfg.LocalSSH.InternalSSHD = plan.InternalSSHD
	cfg.LocalSSH.SSHDBinary = plan.SSHDBinary
	cfg.LocalSSH.SSHDConfig = plan.SSHDConfig
	cfg.LocalSSH.SSHDLog = plan.SSHDLog
	cfg.LocalSSH.AuthFile = plan.AuthFile
	cfg.LocalSSH.HostKeyPath = plan.HostKeyPath
	cfg.LocalSSH.TargetUser = opt.TargetUser
	cfg.LocalSSH.ClientOptions = plan.ClientOptions
	cfg.Install.Mode = opt.InstallMode
	cfg.Install.ConfigDir = opt.ConfigDir
	cfg.Install.DataDir = opt.DataDir
	cfg.Install.AgentPath = opt.AgentPath
	cfg.Install.PersistenceBackend = "reach-agent-" + opt.InstallMode
	cfg.Install.PersistenceQuality = "unknown"
	cfg.Install.PersistenceRebootSafe = opt.InstallMode == "system"
	cfg.Updates.AllowSelfUpdate = true
	cfg.Transport.Mode = opt.Transport
	cfg.Transport.ProbeHost = prov.Hub.PublicHost
	cfg.Transport.ProbePort = prov.Hub.SSHPort
	if prov.WebSocket != nil && prov.WebSocket.Enabled && wsPath != "" {
		cfg.Transport.WebSocket.Enabled = true
		cfg.Transport.WebSocket.Carrier = firstNonEmpty(prov.WebSocket.Carrier, "reach")
		cfg.Transport.WebSocket.URL = prov.WebSocket.URL
		cfg.Transport.WebSocket.PathPrefix = prov.WebSocket.PathPrefix
		cfg.Transport.WebSocket.WSTunnelPath = wsPath
	}
	cfg.Heartbeat.Enabled = true
	cfg.Heartbeat.APIURL = opt.APIURL
	cfg.Heartbeat.Token = prov.AgentToken
	return cfg
}

func writeYAML0600(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return nil
}

func installPersistence(ctx context.Context, opt installOptions, cfgPath string) error {
	if opt.InstallMode == "system" {
		if err := installSystemdService(ctx, opt.AgentPath, cfgPath); err == nil {
			return nil
		} else {
			fmt.Printf("[reach warning] systemd service unavailable: %v\n", err)
		}
		if err := installCron(ctx, opt.AgentPath, cfgPath, opt.DataDir); err == nil {
			return nil
		} else {
			fmt.Printf("[reach warning] root crontab persistence unavailable: %v\n", err)
		}
		fmt.Printf("[reach warning] no reboot-safe system persistence available; starting a detached agent for this boot only.\n")
		return startDetached(opt.AgentPath, cfgPath, filepath.Join(opt.DataDir, "agent.log"), filepath.Join(opt.DataDir, "reach-agent.pid"))
	}
	if err := installUserSystemd(ctx, opt.AgentPath, cfgPath); err == nil {
		return nil
	} else {
		fmt.Printf("[reach warning] systemd --user unavailable: %v\n", err)
	}
	if err := installCron(ctx, opt.AgentPath, cfgPath, opt.DataDir); err == nil {
		return nil
	} else {
		fmt.Printf("[reach warning] crontab persistence unavailable: %v\n", err)
	}
	fmt.Printf("[reach warning] no reboot-safe user persistence available; starting a detached agent for this login only.\n")
	return startDetached(opt.AgentPath, cfgPath, filepath.Join(opt.DataDir, "agent.log"), filepath.Join(opt.DataDir, "reach-agent.pid"))
}

func installSystemdService(ctx context.Context, agentPath, cfgPath string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl is required for reach-agent system service")
	}
	unit := `[Unit]
Description=Reach target agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + agentExecStart(agentPath, cfgPath) + `
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile("/etc/systemd/system/reach-agent.service", []byte(unit), 0o644); err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", "reach-tunnel.service").Run()
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "reach-agent.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now reach-agent failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installUserSystemd(ctx context.Context, agentPath, cfgPath string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "--user", "list-units").Run(); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unit := `[Unit]
Description=Reach target agent
After=default.target

[Service]
Type=simple
ExecStart=` + agentExecStart(agentPath, cfgPath) + `
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`
	if err := os.WriteFile(filepath.Join(unitDir, "reach-agent.service"), []byte(unit), 0o644); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", "reach-agent.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user enable --now failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := exec.LookPath("loginctl"); err == nil {
		if out, err := exec.CommandContext(ctx, "loginctl", "enable-linger", currentUsername()).CombinedOutput(); err != nil {
			fmt.Printf("[reach warning] could not enable linger; user service may not start before login: %s\n", strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func installCron(ctx context.Context, agentPath, cfgPath, dataDir string) error {
	if _, err := exec.LookPath("crontab"); err != nil {
		return err
	}
	logPath := filepath.Join(dataDir, "agent.log")
	pidPath := filepath.Join(dataDir, "reach-agent.pid")
	line := fmt.Sprintf("@reboot sleep 20; %s >> %s 2>&1", agentShellCommand(agentPath, cfgPath), shellQuote(logPath))
	current := ""
	if out, err := exec.CommandContext(ctx, "crontab", "-l").Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, "reach-agent") {
				continue
			}
			if strings.TrimSpace(l) != "" {
				current += l + "\n"
			}
		}
	}
	cmd := exec.CommandContext(ctx, "crontab", "-")
	cmd.Stdin = strings.NewReader(current + line + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crontab install failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return startDetached(agentPath, cfgPath, logPath, pidPath)
}

func startDetached(agentPath, cfgPath, logPath, pidPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if pidPath != "" {
		if pid, ok := readPID(pidPath); ok && processAlive(pid) {
			fmt.Printf("[reach] reach-agent already running pid=%d\n", pid)
			return nil
		}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd := exec.Command(agentPath, agentCommandArgs(cfgPath)...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if pidPath != "" {
		_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600)
	}
	fmt.Printf("[reach] started detached reach-agent pid=%d log=%s\n", cmd.Process.Pid, logPath)
	return cmd.Process.Release()
}

func readPID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func agentCommandArgs(cfgPath string) []string {
	if isDiscoverableAgentConfig(cfgPath) {
		return nil
	}
	return []string{"run", "--config", cfgPath}
}

func agentCommand(agentPath, cfgPath string) string {
	if isDiscoverableAgentConfig(cfgPath) {
		return agentPath
	}
	return agentPath + " run --config " + cfgPath
}

func agentExecStart(agentPath, cfgPath string) string {
	return agentCommand(agentPath, cfgPath)
}

func agentShellCommand(agentPath, cfgPath string) string {
	if isDiscoverableAgentConfig(cfgPath) {
		return shellQuote(agentPath)
	}
	return shellQuote(agentPath) + " run --config " + shellQuote(cfgPath)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func writeInstallEnv(opt installOptions, prov provisionResponse, cfgPath string, plan localSSHPlan) {
	content := fmt.Sprintf("MACHINE_ID='%s'\nSLUG='%s'\nREMOTE_PORT='%d'\nTUNNEL_USER='%s'\nTRANSPORT='%s'\nINSTALL_MODE='%s'\nSERVICE_NAME='reach-agent.service'\nCONFIG_DIR='%s'\nDATA_DIR='%s'\nAGENT_CONFIG='%s'\nAGENT_PATH='%s'\nPID_FILE='%s'\nTARGET_AUTH_FILE='%s'\nLOCAL_SSH_MODE='%s'\nLOCAL_FORWARD_PORT='%d'\n", prov.Machine.ID, prov.Machine.Slug, prov.Tunnel.RemotePort, prov.Tunnel.UnixUser, opt.Transport, opt.InstallMode, opt.ConfigDir, opt.DataDir, cfgPath, opt.AgentPath, filepath.Join(opt.DataDir, "reach-agent.pid"), plan.AuthFile, plan.Mode, plan.LocalPort)
	_ = os.WriteFile(filepath.Join(opt.ConfigDir, "install.env"), []byte(content), 0o600)
}

func uninstallCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	mode := fs.String("mode", "auto", "install mode: auto, system, or user")
	configDir := fs.String("config-dir", "", "config directory")
	dataDir := fs.String("data-dir", "", "data directory")
	agentPath := fs.String("agent-path", "", "agent binary path")
	yes := fs.Bool("yes", false, "do not prompt")
	fs.BoolVar(yes, "y", false, "do not prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opt := installOptions{InstallMode: *mode, ConfigDir: *configDir, DataDir: *dataDir, AgentPath: *agentPath}
	if opt.InstallMode == "auto" {
		if os.Geteuid() == 0 {
			opt.InstallMode = "system"
		} else {
			opt.InstallMode = "user"
		}
	}
	applyModeDefaults(&opt)
	env := readInstallEnv(filepath.Join(opt.ConfigDir, "install.env"))
	for k, v := range env {
		switch k {
		case "CONFIG_DIR":
			opt.ConfigDir = v
		case "DATA_DIR":
			opt.DataDir = v
		case "AGENT_PATH":
			opt.AgentPath = v
		}
	}
	if opt.InstallMode == "system" && os.Geteuid() != 0 {
		return fmt.Errorf("system uninstall needs root")
	}
	if !*yes {
		ans, err := promptDefault("Remove local Reach files and services?", "N", false)
		if err != nil {
			return err
		}
		if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
			return fmt.Errorf("cancelled")
		}
	}
	fmt.Printf("[reach] uninstalling Reach %s install\n", opt.InstallMode)
	sendUninstallIntent(ctx, opt, env, "user_uninstall", false)
	stopPersistence(ctx, opt)
	removeAuthorizedKeyMarker(env)
	removePID(env["PID_FILE"])
	sendUninstallIntent(ctx, opt, env, "user_uninstall_complete", true)
	if opt.DataDir != "" {
		_ = os.RemoveAll(opt.DataDir)
	}
	if opt.ConfigDir != "" {
		_ = os.RemoveAll(opt.ConfigDir)
	}
	if opt.AgentPath != "" && strings.HasSuffix(filepath.Base(opt.AgentPath), "reach-agent") {
		_ = os.Remove(opt.AgentPath)
	}
	fmt.Println("[reach] uninstall complete")
	return nil
}

func sendUninstallIntent(ctx context.Context, opt installOptions, env map[string]string, reason string, complete bool) {
	machineID := env["MACHINE_ID"]
	if machineID == "" {
		return
	}
	cfgPath := env["AGENT_CONFIG"]
	if cfgPath == "" {
		cfgPath = filepath.Join(opt.ConfigDir, "agent.yaml")
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil || cfg.Heartbeat.APIURL == "" || cfg.Heartbeat.Token == "" {
		return
	}
	path := "/api/client/agent/uninstall-intent"
	if complete {
		path = "/api/client/agent/uninstall-complete"
	}
	payload := map[string]string{"machine_id": machineID, "reason": reason}
	_ = postJSON(ctx, strings.TrimRight(cfg.Heartbeat.APIURL, "/")+path, payload, nil, cfg.Heartbeat.Token)
}

func readInstallEnv(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(strings.TrimSpace(v), "'")
	}
	return out
}

func stopPersistence(ctx context.Context, opt installOptions) {
	if opt.InstallMode == "system" {
		_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", "reach-agent.service").Run()
		_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", "reach-tunnel.service").Run()
		_ = os.Remove("/etc/systemd/system/reach-agent.service")
		_ = os.Remove("/etc/systemd/system/reach-tunnel.service")
		_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	} else {
		_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", "reach-agent.service").Run()
		home, _ := os.UserHomeDir()
		_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", "reach-agent.service"))
		_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
		removeReachCrontab(ctx)
	}
	_ = exec.CommandContext(ctx, "pkill", "-f", "reach-agent run --config").Run()
	_ = exec.CommandContext(ctx, "pkill", "-f", "user-sshd/sshd_config").Run()
}

func removeReachCrontab(ctx context.Context) {
	if _, err := exec.LookPath("crontab"); err != nil {
		return
	}
	out, err := exec.CommandContext(ctx, "crontab", "-l").Output()
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "reach-agent") {
			continue
		}
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	cmd := exec.CommandContext(ctx, "crontab", "-")
	cmd.Stdin = strings.NewReader(strings.Join(kept, "\n") + "\n")
	_ = cmd.Run()
}

func removeAuthorizedKeyMarker(env map[string]string) {
	machineID := env["MACHINE_ID"]
	authFile := env["TARGET_AUTH_FILE"]
	if machineID == "" || authFile == "" {
		return
	}
	b, err := os.ReadFile(authFile)
	if err != nil {
		return
	}
	marker := " reach:" + machineID
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, marker) {
			continue
		}
		if line != "" {
			kept = append(kept, line)
		}
	}
	_ = os.WriteFile(authFile, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

func removePID(path string) {
	if path == "" {
		return
	}
	if pid, ok := readPID(path); ok && processAlive(pid) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(path)
}

func distroString() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			vals[k] = strings.Trim(v, "\"")
		}
	}
	return strings.TrimSpace(vals["ID"] + " " + vals["VERSION_ID"])
}

var nonSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if len(s) < 3 {
		s = "box-" + s
	}
	if len(s) < 3 {
		s = "box-linux"
	}
	return s
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}
