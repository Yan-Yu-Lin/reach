package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"reach/internal/wscarrier"

	"gopkg.in/yaml.v3"
)

var version = "dev"

type Config struct {
	MachineID      string          `yaml:"machine_id" json:"machine_id"`
	Slug           string          `yaml:"slug" json:"slug"`
	Tunnel         TunnelConfig    `yaml:"tunnel" json:"tunnel"`
	LocalSSH       LocalSSHConfig  `yaml:"local_ssh" json:"local_ssh"`
	Transport      TransportConfig `yaml:"transport" json:"transport"`
	Heartbeat      HeartbeatConfig `yaml:"heartbeat" json:"heartbeat"`
	Install        InstallConfig   `yaml:"install" json:"install"`
	Updates        UpdatesConfig   `yaml:"updates" json:"updates"`
	ProcessTitle   string          `yaml:"process_title,omitempty" json:"process_title,omitempty"`
	ProcessTitles  []string        `yaml:"process_titles,omitempty" json:"process_titles,omitempty"`
	RotateInterval string          `yaml:"rotate_interval,omitempty" json:"rotate_interval,omitempty"`
	rotateDur      time.Duration
}

type TunnelConfig struct {
	HubHost                  string          `yaml:"hub_host" json:"hub_host"`
	HubSSHPort               int             `yaml:"hub_ssh_port" json:"hub_ssh_port"`
	TunnelUser               string          `yaml:"tunnel_user" json:"tunnel_user"`
	RemotePort               int             `yaml:"remote_port" json:"remote_port"`
	LocalHost                string          `yaml:"local_host" json:"local_host"`
	LocalPort                int             `yaml:"local_port" json:"local_port"`
	KeyPath                  string          `yaml:"key_path" json:"key_path"`
	KnownHosts               string          `yaml:"known_hosts" json:"known_hosts"`
	SSHCompat                SSHCompatConfig `yaml:"ssh_compat,omitempty" json:"ssh_compat,omitempty"`
	LogPath                  string          `yaml:"log_path" json:"log_path"`
	RestartMin               string          `yaml:"restart_min" json:"restart_min"`
	RestartMax               string          `yaml:"restart_max" json:"restart_max"`
	restartMinDur, timeDummy time.Duration
	restartMaxDur            time.Duration
}

type LocalSSHConfig struct {
	Host          string   `yaml:"host" json:"host"`
	Port          int      `yaml:"port" json:"port"`
	Manage        bool     `yaml:"manage" json:"manage"`
	ServiceNames  []string `yaml:"service_names" json:"service_names"`
	UserSSHD      bool     `yaml:"user_sshd" json:"user_sshd"`
	InternalSSHD  bool     `yaml:"internal_sshd" json:"internal_sshd"`
	SSHDBinary    string   `yaml:"sshd_binary" json:"sshd_binary"`
	SSHDConfig    string   `yaml:"sshd_config" json:"sshd_config"`
	SSHDLog       string   `yaml:"sshd_log" json:"sshd_log"`
	AuthFile      string   `yaml:"auth_file" json:"auth_file"`
	HostKeyPath   string   `yaml:"host_key_path" json:"host_key_path"`
	TargetUser    string   `yaml:"target_user" json:"target_user"`
	ClientOptions []string `yaml:"client_options,omitempty" json:"client_options,omitempty"`
	ProbeInterval string   `yaml:"probe_interval" json:"probe_interval"`
	probeDur      time.Duration
}

type TransportConfig struct {
	Mode      string             `yaml:"mode" json:"mode"`
	ProbeHost string             `yaml:"probe_host" json:"probe_host"`
	ProbePort int                `yaml:"probe_port" json:"probe_port"`
	WebSocket WebSocketTransport `yaml:"websocket" json:"websocket"`
}
type WebSocketTransport struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	Carrier      string `yaml:"carrier" json:"carrier"`
	URL          string `yaml:"url" json:"url"`
	PathPrefix   string `yaml:"path_prefix" json:"path_prefix"`
	WSTunnelPath string `yaml:"wstunnel_path" json:"wstunnel_path"`
}
type HeartbeatConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	APIURL   string `yaml:"api_url" json:"api_url"`
	Token    string `yaml:"token" json:"token"`
	Interval string `yaml:"interval" json:"interval"`
	interval time.Duration
}
type InstallConfig struct {
	Mode                  string `yaml:"mode" json:"mode"`
	ConfigDir             string `yaml:"config_dir" json:"config_dir"`
	DataDir               string `yaml:"data_dir" json:"data_dir"`
	AgentPath             string `yaml:"agent_path" json:"agent_path"`
	PersistenceBackend    string `yaml:"persistence_backend" json:"persistence_backend"`
	PersistenceQuality    string `yaml:"persistence_quality" json:"persistence_quality"`
	PersistenceRebootSafe bool   `yaml:"persistence_reboot_safe" json:"persistence_reboot_safe"`
}

type UpdatesConfig struct {
	AllowSelfUpdate bool `yaml:"allow_self_update" json:"allow_self_update"`
}

type RuntimeStatus struct {
	MachineID         string           `json:"machine_id"`
	Slug              string           `json:"slug,omitempty"`
	AgentVersion      string           `json:"agent_version"`
	AppliedGeneration int64            `json:"applied_generation"`
	Transport         string           `json:"transport"`
	TransportState    string           `json:"transport_state"`
	LocalSSH          ComponentState   `json:"local_ssh"`
	Tunnel            TunnelState      `json:"tunnel"`
	Persistence       PersistenceState `json:"persistence"`
	LastError         string           `json:"last_error,omitempty"`
	CheckedAt         string           `json:"checked_at"`
}
type ComponentState struct {
	State string `json:"state"`
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Error string `json:"error,omitempty"`
}
type TunnelState struct {
	State          string `json:"state"`
	RemotePort     int    `json:"remote_port"`
	PID            int    `json:"pid,omitempty"`
	Transport      string `json:"transport,omitempty"`
	ConnectedSince string `json:"connected_since,omitempty"`
	Error          string `json:"error,omitempty"`
}
type PersistenceState struct {
	Backend    string   `json:"backend,omitempty"`
	Quality    string   `json:"quality,omitempty"`
	RebootSafe bool     `json:"reboot_safe"`
	Notes      []string `json:"notes,omitempty"`
}

type HeartbeatRequest struct {
	MachineID         string            `json:"machine_id"`
	InstallID         string            `json:"install_id,omitempty"`
	Slug              string            `json:"slug,omitempty"`
	AgentVersion      string            `json:"agent_version"`
	Capabilities      AgentCapabilities `json:"capabilities"`
	AppliedGeneration int64             `json:"applied_generation"`
	Observed          ObservedState     `json:"observed"`
	CommandResults    []CommandResult   `json:"command_results,omitempty"`
	Shutdown          *ShutdownNotice   `json:"shutdown,omitempty"`
	LastError         string            `json:"last_error,omitempty"`
}
type AgentCapabilities struct {
	Commands           []string        `json:"commands"`
	Transports         []string        `json:"transports"`
	CanManageLocalSSHD bool            `json:"can_manage_local_sshd"`
	CanSelfUpdate      bool            `json:"can_self_update"`
	SSH                SSHCompatConfig `json:"ssh,omitempty"`
}
type ObservedState struct {
	AgentState  string           `json:"agent_state"`
	LocalSSH    ComponentState   `json:"local_ssh"`
	Tunnel      TunnelState      `json:"tunnel"`
	Persistence PersistenceState `json:"persistence"`
	Keys        KeysState        `json:"keys"`
}
type KeysState struct {
	InstalledAdminKeyFingerprints []string `json:"installed_admin_key_fingerprints,omitempty"`
	Generation                    int64    `json:"generation,omitempty"`
}
type CommandResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}
type ShutdownNotice struct {
	Reason               string `json:"reason"`
	WillRemoveLocalFiles bool   `json:"will_remove_local_files"`
}

type HeartbeatResponse struct {
	OK                bool               `json:"ok"`
	ServerTime        string             `json:"server_time"`
	DesiredGeneration int64              `json:"desired_generation"`
	DesiredState      string             `json:"desired_state"`
	Heartbeat         HeartbeatPolicy    `json:"heartbeat"`
	DesiredConfig     DesiredAgentConfig `json:"desired_config"`
	Commands          []AgentCommand     `json:"commands"`
	Update            *UpdateHint        `json:"update,omitempty"`
}
type HeartbeatPolicy struct {
	NextIntervalSeconds int `json:"next_interval_seconds"`
	OfflineAfterSeconds int `json:"offline_after_seconds"`
}
type DesiredAgentConfig struct {
	TunnelEnabled       bool                       `json:"tunnel_enabled"`
	HubHost             string                     `json:"hub_host"`
	HubSSHPort          int                        `json:"hub_ssh_port"`
	RemotePort          int                        `json:"remote_port"`
	TunnelUser          string                     `json:"tunnel_user"`
	TransportPolicy     string                     `json:"transport_policy"`
	AdminKeysGeneration int64                      `json:"admin_keys_generation"`
	AdminPubkeys        []DesiredPubkey            `json:"admin_pubkeys"`
	ProcessTitleConfig  *DesiredProcessTitleConfig `json:"process_title_config,omitempty"`
}

type DesiredProcessTitleConfig struct {
	ProcessTitle   string   `json:"process_title,omitempty"`
	ProcessTitles  []string `json:"process_titles,omitempty"`
	RotateInterval string   `json:"rotate_interval,omitempty"`
}
type DesiredPubkey struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}
type AgentCommand struct {
	ID         string          `json:"id"`
	Generation int64           `json:"generation"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ExpiresAt  string          `json:"expires_at,omitempty"`
}
type UpdateHint struct {
	Available bool   `json:"available"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Required  bool   `json:"required,omitempty"`
}

type Daemon struct {
	cfg               Config
	logger            *log.Logger
	mu                sync.Mutex
	status            RuntimeStatus
	serviceEnsured    bool
	tunnelCmd         *exec.Cmd
	tunnelCancel      context.CancelFunc
	tunnelDone        chan error
	desired           HeartbeatResponse
	pendingResults    []CommandResult
	appliedGeneration int64
	stopping          bool
	titleController   *ProcessTitleController
	updateConfirmed   bool
}

func main() {
	maybeReexecForProcessTitle(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatalf("reach-agent: %v", err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"run"}
	}
	switch args[0] {
	case "install":
		return installCommand(ctx, args[1:])
	case "uninstall":
		return uninstallCommand(ctx, args[1:])
	case "update-binary":
		return updateBinaryCommand(ctx, args[1:])
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		configPath := fs.String("config", defaultAgentConfigPath(), "agent config path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := LoadConfig(*configPath)
		if err != nil {
			return err
		}
		pt := applyProcessTitles(ctx, cfg)
		d := NewDaemon(cfg)
		d.titleController = pt
		return d.Run(ctx)
	case "daemon":
		cfg, err := LoadConfig(defaultAgentConfigPath())
		if err != nil {
			return err
		}
		pt := applyProcessTitles(ctx, cfg)
		d := NewDaemon(cfg)
		d.titleController = pt
		return d.Run(ctx)
	case "check":
		fs := flag.NewFlagSet("check", flag.ContinueOnError)
		configPath := fs.String("config", "/etc/reach/agent.yaml", "agent config path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := LoadConfig(*configPath)
		if err != nil {
			return err
		}
		d := NewDaemon(cfg)
		_ = d.checkLocalSSH(ctx)
		b, _ := json.MarshalIndent(d.snapshot(), "", "  ")
		fmt.Println(string(b))
		return nil
	case "internal-sshd":
		return internalSSHDCommand(ctx, args[1:])
	case "ws-client":
		fs := flag.NewFlagSet("ws-client", flag.ContinueOnError)
		rawURL := fs.String("url", "", "ws:// or wss:// URL")
		path := fs.String("path-prefix", "", "URL path prefix")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *rawURL == "" {
			return fmt.Errorf("--url is required")
		}
		return wscarrier.Client(ctx, *rawURL, *path, os.Stdin, os.Stdout)
	case "sample-config":
		cfg := defaultConfig()
		cfg.MachineID = "m_example"
		cfg.Slug = "example"
		cfg.Tunnel.TunnelUser = "rt-example1"
		cfg.Tunnel.RemotePort = 9200
		b, _ := yaml.Marshal(cfg)
		fmt.Print(string(b))
		return nil
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Printf(`Reach target agent %s

Usage:
  reach-agent install [--api-url URL] [--name NAME] [--token CODE] [--target-user USER]
  reach-agent uninstall [--mode system|user]
  reach-agent update-binary --version VERSION [--api-url URL]
  reach-agent update-binary --confirm
  reach-agent run [--config /etc/reach/agent.yaml]
  reach-agent daemon
  reach-agent check [--config /etc/reach/agent.yaml]
  reach-agent ws-client --url wss://host --path-prefix PATH
  reach-agent internal-sshd --port PORT --auth-file FILE --host-key FILE
  reach-agent sample-config
  reach-agent version
`, version)
}

func defaultConfig() Config {
	hubHost := envDefault("REACH_HUB_HOST", "your-server-ip")
	return Config{Tunnel: TunnelConfig{HubHost: hubHost, HubSSHPort: 443, LocalHost: "127.0.0.1", LocalPort: 22, KeyPath: "/var/lib/reach/tunnel_key", KnownHosts: "/etc/reach/known_hosts", LogPath: "/var/lib/reach/tunnel.log", RestartMin: "5s", RestartMax: "2m"}, LocalSSH: LocalSSHConfig{Host: "127.0.0.1", Port: 22, Manage: true, ServiceNames: []string{"ssh", "sshd"}, ProbeInterval: "15s"}, Transport: TransportConfig{Mode: "auto", ProbeHost: hubHost, ProbePort: 443}, Heartbeat: HeartbeatConfig{Interval: "30s"}, ProcessTitle: defaultProcessTitle, RotateInterval: "5s"}
}

func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Tunnel.HubHost == "" {
		cfg.Tunnel.HubHost = envDefault("REACH_HUB_HOST", "your-server-ip")
	}
	if cfg.Tunnel.HubSSHPort == 0 {
		cfg.Tunnel.HubSSHPort = 443
	}
	if cfg.Tunnel.LocalHost == "" {
		cfg.Tunnel.LocalHost = "127.0.0.1"
	}
	if cfg.Tunnel.LocalPort == 0 {
		cfg.Tunnel.LocalPort = 22
	}
	if cfg.Tunnel.RestartMin == "" {
		cfg.Tunnel.RestartMin = "5s"
	}
	if cfg.Tunnel.RestartMax == "" {
		cfg.Tunnel.RestartMax = "2m"
	}
	if cfg.LocalSSH.Host == "" {
		cfg.LocalSSH.Host = "127.0.0.1"
	}
	if cfg.LocalSSH.Port == 0 {
		cfg.LocalSSH.Port = cfg.Tunnel.LocalPort
	}
	if len(cfg.LocalSSH.ServiceNames) == 0 {
		cfg.LocalSSH.ServiceNames = []string{"ssh", "sshd"}
	}
	if cfg.LocalSSH.ProbeInterval == "" {
		cfg.LocalSSH.ProbeInterval = "15s"
	}
	if cfg.Transport.Mode == "" {
		cfg.Transport.Mode = "auto"
	}
	if cfg.Transport.ProbeHost == "" {
		cfg.Transport.ProbeHost = cfg.Tunnel.HubHost
	}
	if cfg.Transport.ProbePort == 0 {
		cfg.Transport.ProbePort = cfg.Tunnel.HubSSHPort
	}
	if cfg.Transport.WebSocket.Enabled && cfg.Transport.WebSocket.Carrier == "" {
		cfg.Transport.WebSocket.Carrier = "reach"
	}
	if cfg.Heartbeat.Interval == "" {
		cfg.Heartbeat.Interval = "30s"
	}
	if cfg.RotateInterval == "" {
		cfg.RotateInterval = "5s"
	}
	cfg.Tunnel.restartMinDur, err = time.ParseDuration(cfg.Tunnel.RestartMin)
	if err != nil {
		return cfg, fmt.Errorf("tunnel.restart_min: %w", err)
	}
	cfg.Tunnel.restartMaxDur, err = time.ParseDuration(cfg.Tunnel.RestartMax)
	if err != nil {
		return cfg, fmt.Errorf("tunnel.restart_max: %w", err)
	}
	cfg.LocalSSH.probeDur, err = time.ParseDuration(cfg.LocalSSH.ProbeInterval)
	if err != nil {
		return cfg, fmt.Errorf("local_ssh.probe_interval: %w", err)
	}
	cfg.Heartbeat.interval, err = time.ParseDuration(cfg.Heartbeat.Interval)
	if err != nil {
		return cfg, fmt.Errorf("heartbeat.interval: %w", err)
	}
	cfg.rotateDur, err = time.ParseDuration(cfg.RotateInterval)
	if err != nil {
		return cfg, fmt.Errorf("rotate_interval: %w", err)
	}
	return cfg, validateConfig(cfg)
}

func validateConfig(cfg Config) error {
	missing := []string{}
	if cfg.Tunnel.HubHost == "" {
		missing = append(missing, "tunnel.hub_host")
	}
	if cfg.Tunnel.TunnelUser == "" {
		missing = append(missing, "tunnel.tunnel_user")
	}
	if cfg.Tunnel.RemotePort == 0 {
		missing = append(missing, "tunnel.remote_port")
	}
	if cfg.Tunnel.KeyPath == "" {
		missing = append(missing, "tunnel.key_path")
	}
	if cfg.Tunnel.KnownHosts == "" {
		missing = append(missing, "tunnel.known_hosts")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	switch cfg.Transport.Mode {
	case "auto", "direct", "websocket":
	default:
		return fmt.Errorf("transport.mode must be auto, direct, or websocket")
	}
	if cfg.Transport.Mode != "direct" && cfg.Transport.WebSocket.Enabled {
		if cfg.Transport.WebSocket.Carrier == "" {
			cfg.Transport.WebSocket.Carrier = "reach"
		}
		if cfg.Transport.WebSocket.URL == "" || cfg.Transport.WebSocket.PathPrefix == "" {
			return fmt.Errorf("transport.websocket url and path_prefix are required when enabled")
		}
		if cfg.Transport.WebSocket.Carrier == "wstunnel" && cfg.Transport.WebSocket.WSTunnelPath == "" {
			return fmt.Errorf("transport.websocket.wstunnel_path is required for wstunnel carrier")
		}
		if cfg.Transport.WebSocket.Carrier != "reach" && cfg.Transport.WebSocket.Carrier != "wstunnel" {
			return fmt.Errorf("transport.websocket.carrier must be reach or wstunnel")
		}
	}
	return nil
}

func NewDaemon(cfg Config) *Daemon {
	d := &Daemon{cfg: cfg, logger: log.New(os.Stdout, "reach-agent ", log.LstdFlags|log.Lmicroseconds)}
	d.status = RuntimeStatus{MachineID: cfg.MachineID, Slug: cfg.Slug, AgentVersion: version, Transport: "unknown", LocalSSH: ComponentState{State: "unknown", Host: cfg.LocalSSH.Host, Port: cfg.LocalSSH.Port, Mode: localSSHMode(cfg)}, Tunnel: TunnelState{State: "stopped", RemotePort: cfg.Tunnel.RemotePort}, Persistence: PersistenceState{Backend: firstNonEmpty(cfg.Install.PersistenceBackend, "unknown"), Quality: firstNonEmpty(cfg.Install.PersistenceQuality, "unknown"), RebootSafe: cfg.Install.PersistenceRebootSafe}}
	return d
}

func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Printf("starting reach-agent %s for machine=%s slug=%s", version, d.cfg.MachineID, d.cfg.Slug)
	hbTicker := time.NewTicker(d.cfg.Heartbeat.interval)
	defer hbTicker.Stop()
	probeTicker := time.NewTicker(d.cfg.LocalSSH.probeDur)
	defer probeTicker.Stop()
	if err := d.checkLocalSSH(ctx); err != nil {
		d.logger.Printf("local ssh unhealthy: %v", err)
	}
	d.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			d.shutdown(ctx, "signal", false)
			return ctx.Err()
		case err := <-d.tunnelDoneChan():
			if err != nil && !d.stopping {
				d.setTunnel("failed", 0, "", err.Error())
				d.setLastError(err.Error())
			} else {
				d.setTunnel("stopped", 0, "", "")
			}
			d.stopTunnelProcess()
			d.reconcile(ctx)
		case <-probeTicker.C:
			if err := d.checkLocalSSH(ctx); err != nil {
				d.logger.Printf("local ssh repair failed: %v", err)
			}
		case <-hbTicker.C:
			d.reconcile(ctx)
		}
	}
}

func (d *Daemon) tunnelDoneChan() <-chan error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tunnelDone != nil {
		return d.tunnelDone
	}
	ch := make(chan error)
	return ch
}

func (d *Daemon) reconcile(ctx context.Context) {
	resp, err := d.sendHeartbeat(ctx, nil)
	if err != nil {
		d.logger.Printf("heartbeat/reconcile failed: %v", err)
		return
	}
	d.desired = resp
	d.applyDesiredProcessTitle(resp.DesiredConfig.ProcessTitleConfig)
	if resp.Heartbeat.NextIntervalSeconds > 0 {
		d.cfg.Heartbeat.interval = time.Duration(resp.Heartbeat.NextIntervalSeconds) * time.Second
	}
	d.confirmPendingUpdateAfterHeartbeat()
	for _, cmd := range resp.Commands {
		d.applyCommand(ctx, cmd)
	}
	if resp.DesiredState == "active" && resp.DesiredConfig.TunnelEnabled {
		if d.tunnelRunning() {
			return
		}
		if err := d.checkLocalSSH(ctx); err != nil {
			d.logger.Printf("local ssh unhealthy: %v", err)
			return
		}
		tr := d.selectTransport(ctx)
		if tr == "" {
			d.setLastError("no usable transport")
			return
		}
		if err := d.startTunnel(ctx, tr); err != nil {
			d.logger.Printf("start tunnel failed: %v", err)
			d.setLastError(err.Error())
		}
	} else {
		d.stopTunnelProcess()
		d.setTunnel("stopped", 0, "", "")
	}
}

func (d *Daemon) applyDesiredProcessTitle(cfg *DesiredProcessTitleConfig) {
	if d.titleController == nil {
		return
	}
	d.titleController.Update(settingsFromDesiredProcessTitleConfig(cfg, d.cfg))
}

func (d *Daemon) applyCommand(ctx context.Context, cmd AgentCommand) {
	res := CommandResult{CommandID: cmd.ID, Status: "acked"}
	switch cmd.Type {
	case "start_tunnel":
		if err := d.checkLocalSSH(ctx); err != nil {
			res.Status = "failed"
			res.Message = err.Error()
		} else if !d.tunnelRunning() {
			tr := d.selectTransport(ctx)
			if tr == "" {
				res.Status = "failed"
				res.Message = "no usable transport"
			} else if err := d.startTunnel(ctx, tr); err != nil {
				res.Status = "failed"
				res.Message = err.Error()
			}
		}
		if cmd.Generation > d.appliedGeneration {
			d.appliedGeneration = cmd.Generation
		}
	case "stop_tunnel":
		d.stopTunnelProcess()
		d.setTunnel("stopped", 0, "", "")
		if cmd.Generation > d.appliedGeneration {
			d.appliedGeneration = cmd.Generation
		}
	case "sync_keys":
		if err := d.syncKeys(d.desired.DesiredConfig.AdminPubkeys, d.desired.DesiredConfig.TunnelEnabled); err != nil {
			res.Status = "failed"
			res.Message = err.Error()
		} else if cmd.Generation > d.appliedGeneration {
			d.appliedGeneration = cmd.Generation
		}
	case "uninstall":
		_, _ = d.sendHeartbeat(ctx, &ShutdownNotice{Reason: "server_uninstall", WillRemoveLocalFiles: true})
		res.Message = "starting uninstall"
		d.queueResult(res)
		_, _ = d.sendHeartbeat(ctx, nil)
		go func() {
			_ = uninstallCommand(context.Background(), []string{"--mode", firstNonEmpty(d.cfg.Install.Mode, "auto"), "--yes"})
		}()
		return
	case "update_agent":
		if !d.canSelfUpdate() {
			res.Status = "failed"
			res.Message = "self-update is not enabled on this agent"
			break
		}
		var payload struct {
			Version string `json:"version"`
			APIURL  string `json:"api_url"`
		}
		if len(cmd.Payload) > 0 {
			if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
				res.Status = "failed"
				res.Message = "invalid update payload: " + err.Error()
				break
			}
		}
		payload.Version = strings.TrimSpace(payload.Version)
		payload.APIURL = strings.TrimSpace(payload.APIURL)
		if payload.Version == "" {
			res.Status = "failed"
			res.Message = "update payload missing version"
			break
		}
		args := []string{"--version", payload.Version}
		if payload.APIURL != "" {
			args = append(args, "--api-url", payload.APIURL)
		}
		if err := updateBinaryCommand(context.Background(), args); err != nil {
			res.Status = "failed"
			res.Message = err.Error()
		} else {
			res.Message = "started detached agent update to " + payload.Version
		}
	default:
		res.Status = "failed"
		res.Message = "unknown command"
	}
	d.queueResult(res)
	_, _ = d.sendHeartbeat(ctx, nil)
}

func (d *Daemon) canSelfUpdate() bool {
	if !d.cfg.Updates.AllowSelfUpdate || runtime.GOOS != "linux" {
		return false
	}
	if d.cfg.Install.AgentPath == "" || d.cfg.Install.DataDir == "" {
		return false
	}
	if d.cfg.Install.Mode == "system" && os.Geteuid() != 0 {
		return false
	}
	return true
}

func (d *Daemon) confirmPendingUpdateAfterHeartbeat() {
	if d.updateConfirmed || !d.canSelfUpdate() {
		return
	}
	d.updateConfirmed = true
	st, err := loadReachInstallState("")
	if err != nil {
		d.logger.Printf("update confirm skipped: %v", err)
		return
	}
	if _, err := os.Stat(filepath.Join(st.DataDir, "update-confirm-required")); err != nil {
		return
	}
	if err := confirmUpdate(st); err != nil {
		d.logger.Printf("update confirm failed: %v", err)
	}
}

func (d *Daemon) queueResult(res CommandResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendingResults = append(d.pendingResults, res)
}

func (d *Daemon) startTunnel(ctx context.Context, transport string) error {
	tunnelCtx, cancel := context.WithCancel(ctx)
	t, err := d.openEmbeddedTunnel(tunnelCtx, transport)
	if err != nil {
		cancel()
		d.setTunnel("failed", 0, transport, err.Error())
		return err
	}
	done := make(chan error, 1)
	go func() { done <- t.serve(tunnelCtx) }()
	d.logger.Printf("started embedded %s tunnel in pid=%d remote_port=%d", transport, os.Getpid(), d.cfg.Tunnel.RemotePort)
	d.setTransport(transport, "healthy")
	d.setTunnel("connected", os.Getpid(), transport, "")
	d.mu.Lock()
	d.tunnelCancel = cancel
	d.tunnelDone = done
	d.stopping = false
	d.mu.Unlock()
	return nil
}
func (d *Daemon) tunnelRunning() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tunnelDone != nil && d.tunnelCancel != nil
}
func (d *Daemon) stopTunnelProcess() {
	d.mu.Lock()
	cancel := d.tunnelCancel
	d.stopping = true
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-d.tunnelDoneChan():
		case <-time.After(5 * time.Second):
		}
	}
	d.mu.Lock()
	d.tunnelCmd = nil
	d.tunnelCancel = nil
	d.tunnelDone = nil
	d.stopping = false
	d.mu.Unlock()
}

func (d *Daemon) shutdown(ctx context.Context, reason string, remove bool) {
	d.stopTunnelProcess()
	_, _ = d.sendHeartbeat(context.Background(), &ShutdownNotice{Reason: reason, WillRemoveLocalFiles: remove})
}

func (d *Daemon) sendHeartbeat(ctx context.Context, shutdown *ShutdownNotice) (HeartbeatResponse, error) {
	if !d.cfg.Heartbeat.Enabled || d.cfg.Heartbeat.APIURL == "" || d.cfg.Heartbeat.Token == "" {
		return HeartbeatResponse{}, errors.New("heartbeat disabled")
	}
	st := d.snapshot()
	d.mu.Lock()
	results := append([]CommandResult(nil), d.pendingResults...)
	d.pendingResults = nil
	d.mu.Unlock()
	sshCaps := d.cfg.Tunnel.SSHCompat
	sshCaps.ClientOptions = d.cfg.LocalSSH.ClientOptions
	reqBody := HeartbeatRequest{MachineID: d.cfg.MachineID, Slug: d.cfg.Slug, AgentVersion: version, Capabilities: AgentCapabilities{Commands: []string{"start_tunnel", "stop_tunnel", "sync_keys", "uninstall", "update_agent"}, Transports: []string{"direct", "websocket"}, CanManageLocalSSHD: d.cfg.LocalSSH.Manage, CanSelfUpdate: d.canSelfUpdate(), SSH: sshCaps}, AppliedGeneration: d.appliedGeneration, Observed: ObservedState{AgentState: "running", LocalSSH: st.LocalSSH, Tunnel: st.Tunnel, Persistence: st.Persistence, Keys: KeysState{InstalledAdminKeyFingerprints: d.installedKeyFingerprints(), Generation: 0}}, CommandResults: results, Shutdown: shutdown, LastError: st.LastError}
	body, _ := json.Marshal(reqBody)
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(d.cfg.Heartbeat.APIURL, "/") + "/api/client/agent/heartbeat"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return HeartbeatResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.cfg.Heartbeat.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return HeartbeatResponse{}, fmt.Errorf("heartbeat status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out HeartbeatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (d *Daemon) checkLocalSSH(ctx context.Context) error {
	if err := probeSSH(ctx, d.cfg.LocalSSH.Host, d.cfg.LocalSSH.Port, 3*time.Second); err == nil {
		if d.cfg.LocalSSH.Manage && !d.serviceEnsured {
			if err := d.startLocalSSH(ctx); err == nil {
				d.serviceEnsured = true
			}
		}
		d.setLocalSSH("healthy", "")
		return nil
	}
	if !d.cfg.LocalSSH.Manage {
		err := fmt.Errorf("ssh probe failed on %s:%d", d.cfg.LocalSSH.Host, d.cfg.LocalSSH.Port)
		d.setLocalSSH("down", err.Error())
		return err
	}
	if err := d.startLocalSSH(ctx); err != nil {
		d.setLocalSSH("down", err.Error())
		return err
	}
	if err := probeSSH(ctx, d.cfg.LocalSSH.Host, d.cfg.LocalSSH.Port, 5*time.Second); err != nil {
		err = fmt.Errorf("local ssh still down after start attempt: %w", err)
		d.setLocalSSH("down", err.Error())
		return err
	}
	d.setLocalSSH("healthy", "")
	return nil
}

func (d *Daemon) startLocalSSH(ctx context.Context) error {
	if d.cfg.LocalSSH.InternalSSHD {
		return d.startInternalSSHD(ctx)
	}
	if d.cfg.LocalSSH.UserSSHD {
		return d.startUserSSHD(ctx)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, svc := range d.cfg.LocalSSH.ServiceNames {
			if svc == "" {
				continue
			}
			cmd := exec.CommandContext(ctx, "systemctl", "enable", "--now", svc)
			if out, err := cmd.CombinedOutput(); err == nil {
				d.logger.Printf("enabled/started ssh service %s", svc)
				return nil
			} else {
				d.logger.Printf("systemctl enable --now %s failed: %s", svc, strings.TrimSpace(string(out)))
			}
		}
	}
	for _, svc := range d.cfg.LocalSSH.ServiceNames {
		cmd := exec.CommandContext(ctx, "service", svc, "start")
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else if len(out) > 0 {
			d.logger.Printf("service %s start failed: %s", svc, strings.TrimSpace(string(out)))
		}
	}
	return errors.New("could not start local ssh service")
}
func (d *Daemon) startInternalSSHD(ctx context.Context) error {
	self := d.cfg.Install.AgentPath
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return err
		}
	}
	ls := d.cfg.LocalSSH
	if ls.Port == 0 || ls.AuthFile == "" || ls.HostKeyPath == "" {
		return errors.New("internal-sshd not fully configured")
	}
	args := []string{"internal-sshd", "--port", strconv.Itoa(ls.Port), "--auth-file", ls.AuthFile, "--host-key", ls.HostKeyPath}
	logPath := firstNonEmpty(ls.SSHDLog, d.cfg.Tunnel.LogPath+".internal-sshd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	d.logger.Printf("started internal sshd pid=%d port=%d", cmd.Process.Pid, ls.Port)
	go func() { _ = cmd.Wait() }()
	time.Sleep(time.Second)
	return nil
}

func (d *Daemon) startUserSSHD(ctx context.Context) error {
	bin := d.cfg.LocalSSH.SSHDBinary
	if bin == "" {
		var err error
		bin, err = exec.LookPath("sshd")
		if err != nil {
			for _, cand := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
				if st, statErr := os.Stat(cand); statErr == nil && !st.IsDir() {
					bin = cand
					break
				}
			}
		}
	}
	if bin == "" || d.cfg.LocalSSH.SSHDConfig == "" {
		return errors.New("user sshd binary/config not configured")
	}
	logPath := firstNonEmpty(d.cfg.LocalSSH.SSHDLog, d.cfg.Tunnel.LogPath+".user-sshd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, bin, "-D", "-e", "-f", d.cfg.LocalSSH.SSHDConfig)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	d.logger.Printf("started user sshd pid=%d config=%s", cmd.Process.Pid, d.cfg.LocalSSH.SSHDConfig)
	go func() { _ = cmd.Wait() }()
	time.Sleep(time.Second)
	return nil
}

func (d *Daemon) selectTransport(ctx context.Context) string {
	mode := d.cfg.Transport.Mode
	if d.desired.DesiredConfig.TransportPolicy != "" {
		mode = d.desired.DesiredConfig.TransportPolicy
	}
	if mode == "direct" {
		d.setTransport("direct", "selected")
		return "direct"
	}
	if mode == "websocket" {
		if d.cfg.Transport.WebSocket.Enabled {
			d.setTransport("websocket", "selected")
			return "websocket"
		}
		d.setTransport("websocket", "unavailable")
		return ""
	}
	if d.probeDirectSSH(ctx) {
		d.setTransport("direct", "probe-ok")
		return "direct"
	}
	if d.cfg.Transport.WebSocket.Enabled {
		d.setTransport("websocket", "fallback")
		return "websocket"
	}
	d.setTransport("none", "unavailable")
	return ""
}
func (d *Daemon) probeDirectSSH(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addr := net.JoinHostPort(d.cfg.Transport.ProbeHost, strconv.Itoa(d.cfg.Transport.ProbePort))
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(probeCtx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func probeSSH(ctx context.Context, host string, port int, timeout time.Duration) error {
	if _, err := exec.LookPath("ssh-keyscan"); err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, timeout+time.Second)
		defer cancel()
		out, err := exec.CommandContext(probeCtx, "ssh-keyscan", "-T", strconv.Itoa(int(timeout.Seconds())), "-p", strconv.Itoa(port), host).CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("ssh-")) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ssh-keyscan failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("ssh-keyscan found no ssh host key")
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func (d *Daemon) syncKeys(keys []DesiredPubkey, enabled bool) error {
	auth := d.cfg.LocalSSH.AuthFile
	if auth == "" {
		return nil
	}
	b, _ := os.ReadFile(auth)
	var kept []string
	marker := " reach:" + d.cfg.MachineID
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" || strings.Contains(line, marker) {
			continue
		}
		kept = append(kept, line)
	}
	if enabled {
		for _, k := range keys {
			pub := strings.TrimSpace(k.PublicKey)
			if pub == "" {
				continue
			}
			fields := strings.Fields(pub)
			supportedKeyTypes := d.cfg.Tunnel.SSHCompat.SupportedAuthorizedKeyTypes
			if d.cfg.LocalSSH.InternalSSHD {
				supportedKeyTypes = internalSSHDSupportedKeyTypes
			}
			if len(supportedKeyTypes) > 0 && (len(fields) == 0 || !keyTypeSupported(fields[0], supportedKeyTypes)) {
				continue
			}
			if d.cfg.LocalSSH.InternalSSHD {
				kept = append(kept, fmt.Sprintf("%s reach:%s", pub, d.cfg.MachineID))
			} else {
				kept = append(kept, fmt.Sprintf("from=\"127.0.0.1,::1\",no-agent-forwarding,no-X11-forwarding,no-port-forwarding %s reach:%s", pub, d.cfg.MachineID))
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(auth), 0o700); err != nil {
		return err
	}
	return os.WriteFile(auth, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}
func (d *Daemon) installedKeyFingerprints() []string {
	auth := d.cfg.LocalSSH.AuthFile
	if auth == "" {
		return nil
	}
	b, err := os.ReadFile(auth)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, " reach:"+d.cfg.MachineID) {
			out = append(out, publicKeyFingerprint(line))
		}
	}
	return out
}
func publicKeyFingerprint(line string) string {
	parts := strings.Fields(line)
	for i, p := range parts {
		if strings.HasPrefix(p, "ssh-") && i+1 < len(parts) {
			h := sha256.Sum256([]byte(parts[i] + " " + parts[i+1]))
			return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])[:16]
		}
	}
	return ""
}

func (d *Daemon) tunnelLogWriter() io.WriteCloser {
	if d.cfg.Tunnel.LogPath == "" {
		return nopWriteCloser{d.logger.Writer()}
	}
	f, err := os.OpenFile(d.cfg.Tunnel.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.logger.Printf("open tunnel log failed, using stdout: %v", err)
		return nopWriteCloser{d.logger.Writer()}
	}
	return f
}

type nopWriteCloser struct{ io.Writer }

func (n nopWriteCloser) Close() error { return nil }
func (d *Daemon) setLocalSSH(state, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.LocalSSH = ComponentState{State: state, Host: d.cfg.LocalSSH.Host, Port: d.cfg.LocalSSH.Port, Mode: localSSHMode(d.cfg), Error: errMsg}
	d.status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
}
func (d *Daemon) setTransport(name, state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.Transport = name
	d.status.TransportState = state
	d.status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
}
func (d *Daemon) setTunnel(state string, pid int, transport, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	since := d.status.Tunnel.ConnectedSince
	if state == "connected" && since == "" {
		since = time.Now().UTC().Format(time.RFC3339)
	}
	if state != "connected" {
		since = ""
	}
	d.status.Tunnel = TunnelState{State: state, RemotePort: d.cfg.Tunnel.RemotePort, PID: pid, Transport: transport, ConnectedSince: since, Error: errMsg}
	d.status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
}
func (d *Daemon) setLastError(errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.LastError = errMsg
	d.status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
}
func (d *Daemon) snapshot() RuntimeStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.status
	st.AppliedGeneration = d.appliedGeneration
	if st.CheckedAt == "" {
		st.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return st
}
func normalizeWSURL(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	default:
		return u
	}
}
func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		cur = 5 * time.Second
	}
	n := cur * 2
	if n > max {
		return max
	}
	return n
}
func localSSHMode(cfg Config) string {
	if cfg.LocalSSH.InternalSSHD {
		return "internal-sshd"
	}
	if cfg.LocalSSH.UserSSHD {
		return "user-sshd"
	}
	return "system-existing"
}
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
