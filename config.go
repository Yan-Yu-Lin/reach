package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr          string                `yaml:"listen_addr"`
	DBPath              string                `yaml:"db_path"`
	AllowedOrigins      []string              `yaml:"allowed_origins"`
	GodCodeHash         string                `yaml:"god_code_hash"`
	JWTSecret           string                `yaml:"jwt_secret"`
	AdminJWTTTL         time.Duration         `yaml:"admin_jwt_ttl"`
	LogLevel            string                `yaml:"log_level"`
	RequestTTL          time.Duration         `yaml:"request_ttl"`
	SetupTokenTTL       time.Duration         `yaml:"setup_token_ttl"`
	HealthInterval      time.Duration         `yaml:"health_interval"`
	MaintenanceInterval time.Duration         `yaml:"maintenance_interval"`
	OfflineAfter        time.Duration         `yaml:"offline_after"`
	DefaultExpiry       time.Duration         `yaml:"default_expiry"`
	SSHDConfigDir       string                `yaml:"sshd_config_dir"`
	TunnelHomeBase      string                `yaml:"tunnel_home_base"`
	SSHServiceName      string                `yaml:"ssh_service_name"`
	HubHostKeys         []string              `yaml:"hub_host_keys"`
	DeprecatedHostKeys  []string              `yaml:"jason_host_keys"`
	WebSocketTunnel     WebSocketTunnelConfig `yaml:"websocket_tunnel"`
	InitialAdmin        InitialAdmin          `yaml:"initial_admin"`
	DefaultHub          HubConfig             `yaml:"default_hub"`
	ProvisioningEnabled bool                  `yaml:"provisioning_enabled"`
}

type InitialAdmin struct {
	Username     string            `yaml:"username"`
	PasswordHash string            `yaml:"password_hash"`
	PublicKeys   []InitialKey      `yaml:"public_keys"`
	Labels       map[string]string `yaml:"labels"`
}

type InitialKey struct {
	Label     string `yaml:"label"`
	PublicKey string `yaml:"public_key"`
}

type HubConfig struct {
	ID             string `yaml:"id"`
	Name           string `yaml:"name"`
	PublicHost     string `yaml:"public_host"`
	SSHPort        int    `yaml:"ssh_port"`
	ProxyJumpAlias string `yaml:"proxyjump_alias"`
	APIURL         string `yaml:"api_url"`
	PortStart      int    `yaml:"port_range_start"`
	PortEnd        int    `yaml:"port_range_end"`
}

type WebSocketTunnelConfig struct {
	Enabled    bool                       `yaml:"enabled"`
	Carrier    string                     `yaml:"carrier"`
	URL        string                     `yaml:"url"`
	PathPrefix string                     `yaml:"path_prefix"`
	Binaries   map[string]WebSocketBinary `yaml:"binaries"`
}

type WebSocketBinary struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func DefaultConfig() Config {
	hubHost := envDefault("REACH_HUB_HOST", "your-server-ip")
	apiURL := envDefault("REACH_API_URL", "https://tunnels.your-domain.example")
	return Config{
		ListenAddr:          "127.0.0.1:9300",
		DBPath:              "/var/lib/reach/reach.db",
		LogLevel:            "info",
		AdminJWTTTL:         365 * 24 * time.Hour,
		RequestTTL:          30 * time.Minute,
		SetupTokenTTL:       15 * time.Minute,
		HealthInterval:      45 * time.Second,
		MaintenanceInterval: 10 * time.Minute,
		OfflineAfter:        5 * time.Minute,
		DefaultExpiry:       0,
		SSHDConfigDir:       "/etc/ssh/sshd_config.d",
		TunnelHomeBase:      "/var/lib",
		SSHServiceName:      "ssh",
		ProvisioningEnabled: true,
		DefaultHub: HubConfig{
			ID:             envDefault("REACH_HUB_ID", "primary"),
			Name:           envDefault("REACH_HUB_NAME", "Primary Hub"),
			PublicHost:     hubHost,
			SSHPort:        443,
			ProxyJumpAlias: envDefault("REACH_PROXYJUMP_ALIAS", "reach-hub"),
			APIURL:         apiURL,
			PortStart:      9200,
			PortEnd:        9499,
		},
		InitialAdmin: InitialAdmin{Username: envDefault("REACH_INITIAL_ADMIN_USERNAME", "admin")},
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		path = "/etc/reach/config.yaml"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:9300"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/var/lib/reach/reach.db"
	}
	if cfg.SSHDConfigDir == "" {
		cfg.SSHDConfigDir = "/etc/ssh/sshd_config.d"
	}
	if cfg.TunnelHomeBase == "" {
		cfg.TunnelHomeBase = "/var/lib"
	}
	if cfg.SSHServiceName == "" {
		cfg.SSHServiceName = "ssh"
	}
	if cfg.AdminJWTTTL == 0 {
		cfg.AdminJWTTTL = 365 * 24 * time.Hour
	}
	if cfg.AdminJWTTTL < 0 {
		return cfg, fmt.Errorf("admin_jwt_ttl must not be negative")
	}
	if cfg.RequestTTL == 0 {
		cfg.RequestTTL = 30 * time.Minute
	}
	if cfg.SetupTokenTTL == 0 {
		cfg.SetupTokenTTL = 15 * time.Minute
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = 45 * time.Second
	}
	if cfg.MaintenanceInterval == 0 {
		cfg.MaintenanceInterval = 10 * time.Minute
	}
	if cfg.OfflineAfter == 0 {
		cfg.OfflineAfter = 5 * time.Minute
	}
	// DefaultExpiry 0 means never expire. Only set expiry if explicitly configured.
	if cfg.DefaultHub.ID == "" {
		cfg.DefaultHub = DefaultConfig().DefaultHub
	}
	if cfg.DefaultHub.PortStart == 0 || cfg.DefaultHub.PortEnd == 0 {
		cfg.DefaultHub.PortStart = 9200
		cfg.DefaultHub.PortEnd = 9499
	}
	if cfg.DefaultHub.SSHPort == 0 {
		cfg.DefaultHub.SSHPort = 443
	}
	if cfg.InitialAdmin.Username == "" {
		cfg.InitialAdmin.Username = envDefault("REACH_INITIAL_ADMIN_USERNAME", "admin")
	}
	if len(cfg.HubHostKeys) == 0 && len(cfg.DeprecatedHostKeys) > 0 {
		cfg.HubHostKeys = cfg.DeprecatedHostKeys
	}
	if cfg.WebSocketTunnel.Enabled {
		if cfg.WebSocketTunnel.URL == "" {
			return cfg, fmt.Errorf("websocket_tunnel.url is required when enabled")
		}
		if cfg.WebSocketTunnel.PathPrefix == "" {
			return cfg, fmt.Errorf("websocket_tunnel.path_prefix is required when enabled")
		}
		if cfg.WebSocketTunnel.Carrier == "" {
			cfg.WebSocketTunnel.Carrier = "reach"
		}
		if cfg.WebSocketTunnel.Carrier == "wstunnel" && len(cfg.WebSocketTunnel.Binaries) == 0 {
			return cfg, fmt.Errorf("websocket_tunnel.binaries is required when carrier is wstunnel")
		}
		if cfg.WebSocketTunnel.Carrier != "reach" && cfg.WebSocketTunnel.Carrier != "wstunnel" {
			return cfg, fmt.Errorf("websocket_tunnel.carrier must be reach or wstunnel")
		}
	}
	if !strings.HasPrefix(cfg.ListenAddr, "127.0.0.1:") && !strings.HasPrefix(cfg.ListenAddr, "localhost:") {
		return cfg, fmt.Errorf("listen_addr must bind localhost only, got %q", cfg.ListenAddr)
	}
	return cfg, nil
}
