package main

import (
	"database/sql"
	"encoding/json"
)

const (
	DesiredActive   = "active"
	DesiredDisabled = "disabled"
	DesiredRetiring = "retiring"
	DesiredRetired  = "retired"

	ObservedUnknown  = "unknown"
	ObservedOnline   = "online"
	ObservedDegraded = "degraded"
	ObservedOffline  = "offline"
	ObservedGone     = "gone"
)

type Machine struct {
	ID                 string              `json:"id"`
	Slug               string              `json:"slug"`
	OriginalSlug       *string             `json:"original_slug,omitempty"`
	DisplayName        *string             `json:"display_name,omitempty"`
	TargetUser         *string             `json:"target_user,omitempty"`
	OwnerUserID        *string             `json:"owner_user_id,omitempty"`
	Status             string              `json:"status"`
	DesiredState       string              `json:"desired_state"`
	ObservedState      string              `json:"observed_state"`
	DesiredGeneration  int64               `json:"desired_generation"`
	UpdatePolicy       string              `json:"update_policy"`
	DesiredChangedAt   *string             `json:"desired_changed_at,omitempty"`
	DesiredChangedBy   *string             `json:"desired_changed_by,omitempty"`
	CleanupState       *string             `json:"cleanup_state,omitempty"`
	Mode               *string             `json:"mode,omitempty"`
	LocalPort          int                 `json:"local_port"`
	Persistence        *string             `json:"persistence,omitempty"`
	Distro             *string             `json:"distro,omitempty"`
	Arch               *string             `json:"arch,omitempty"`
	ProvisionError     *string             `json:"provision_error,omitempty"`
	CreatedAt          string              `json:"created_at"`
	UpdatedAt          *string             `json:"updated_at,omitempty"`
	ExpiresAt          *string             `json:"expires_at,omitempty"`
	DisabledAt         *string             `json:"disabled_at,omitempty"`
	RetiredAt          *string             `json:"retired_at,omitempty"`
	ProcessTitleConfig *ProcessTitleConfig `json:"process_title_config,omitempty"`
}

type Hub struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	PublicHost     string `json:"public_host"`
	SSHPort        int    `json:"ssh_port"`
	ProxyJumpAlias string `json:"proxyjump_alias"`
	APIURL         string `json:"api_url"`
	PortStart      int    `json:"port_range_start"`
	PortEnd        int    `json:"port_range_end"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}

type Tunnel struct {
	ID                 string  `json:"id"`
	MachineID          string  `json:"machine_id"`
	HubID              string  `json:"hub_id"`
	UnixUser           string  `json:"unix_user"`
	OriginalUnixUser   *string `json:"original_unix_user,omitempty"`
	RemotePort         int     `json:"remote_port"`
	OriginalRemotePort *int    `json:"original_remote_port,omitempty"`
	TunnelPubkey       string  `json:"tunnel_pubkey,omitempty"`
	Status             string  `json:"status"`
	LastSeenAt         *string `json:"last_seen_at,omitempty"`
	CreatedAt          string  `json:"created_at"`
	ExpiresAt          *string `json:"expires_at,omitempty"`
}

type Request struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	ClientSecretHash    string          `json:"-"`
	Status              string          `json:"status"`
	InviteCodeHash      *string         `json:"-"`
	SetupTokenHash      *string         `json:"-"`
	SetupTokenExpiresAt *string         `json:"setup_token_expires_at,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	CreatedAt           string          `json:"created_at"`
	ExpiresAt           string          `json:"expires_at"`
	ApprovedAt          *string         `json:"approved_at,omitempty"`
	DeniedAt            *string         `json:"denied_at,omitempty"`
}

type MachineWithTunnels struct {
	Machine        Machine            `json:"machine"`
	Tunnels        []Tunnel           `json:"tunnels"`
	Agent          *AgentObservation  `json:"agent_observation,omitempty"`
	HubObservation *HubObservation    `json:"hub_observation,omitempty"`
	Commands       []AgentCommandInfo `json:"commands,omitempty"`
	Update         *AgentUpdateHint   `json:"update,omitempty"`
}

type AgentObservation struct {
	MachineID                 string          `json:"machine_id"`
	AgentVersion              string          `json:"agent_version,omitempty"`
	InstallID                 string          `json:"install_id,omitempty"`
	AppliedGeneration         int64           `json:"applied_generation"`
	HeartbeatAt               string          `json:"heartbeat_at"`
	AgentState                string          `json:"agent_state,omitempty"`
	Transport                 string          `json:"transport,omitempty"`
	TransportState            string          `json:"transport_state,omitempty"`
	LocalSSHState             string          `json:"local_ssh_state,omitempty"`
	LocalSSHError             string          `json:"local_ssh_error,omitempty"`
	TunnelState               string          `json:"tunnel_state,omitempty"`
	TunnelPID                 *int            `json:"tunnel_pid,omitempty"`
	TunnelError               string          `json:"tunnel_error,omitempty"`
	PersistenceBackend        string          `json:"persistence_backend,omitempty"`
	PersistenceQuality        string          `json:"persistence_quality,omitempty"`
	PersistenceRebootSafe     bool            `json:"persistence_reboot_safe"`
	AuthorizedKeyFingerprints json.RawMessage `json:"authorized_key_fingerprints,omitempty"`
	LastError                 string          `json:"last_error,omitempty"`
}

type HubObservation struct {
	TunnelID      string  `json:"tunnel_id"`
	MachineID     string  `json:"machine_id"`
	ProbeState    string  `json:"probe_state"`
	LastProbeAt   string  `json:"last_probe_at"`
	LastSuccessAt *string `json:"last_success_at,omitempty"`
	ProbeError    string  `json:"probe_error,omitempty"`
}

type AgentCommandInfo struct {
	ID         string          `json:"id"`
	MachineID  string          `json:"machine_id"`
	Generation int64           `json:"generation"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Status     string          `json:"status"`
	CreatedAt  string          `json:"created_at"`
	SentAt     *string         `json:"sent_at,omitempty"`
	AckedAt    *string         `json:"acked_at,omitempty"`
	ExpiresAt  *string         `json:"expires_at,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
}

type AgentComponentStatus struct {
	State string `json:"state"`
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Error string `json:"error,omitempty"`
}

type AgentTunnelStatus struct {
	State          string `json:"state"`
	RemotePort     int    `json:"remote_port"`
	PID            int    `json:"pid,omitempty"`
	Transport      string `json:"transport,omitempty"`
	ConnectedSince string `json:"connected_since,omitempty"`
	Error          string `json:"error,omitempty"`
}

type AgentPersistenceStatus struct {
	Backend    string   `json:"backend,omitempty"`
	Quality    string   `json:"quality,omitempty"`
	RebootSafe bool     `json:"reboot_safe"`
	Notes      []string `json:"notes,omitempty"`
}

type AgentKeysStatus struct {
	InstalledAdminKeyFingerprints []string `json:"installed_admin_key_fingerprints,omitempty"`
	Generation                    int64    `json:"generation,omitempty"`
}

type AgentObservedState struct {
	AgentState  string                 `json:"agent_state,omitempty"`
	LocalSSH    AgentComponentStatus   `json:"local_ssh"`
	Tunnel      AgentTunnelStatus      `json:"tunnel"`
	Persistence AgentPersistenceStatus `json:"persistence"`
	Keys        AgentKeysStatus        `json:"keys"`
}

type AgentCapabilities struct {
	Commands           []string        `json:"commands,omitempty"`
	Transports         []string        `json:"transports,omitempty"`
	CanManageLocalSSHD bool            `json:"can_manage_local_sshd"`
	CanSelfUpdate      bool            `json:"can_self_update"`
	SSH                SSHCompatStatus `json:"ssh,omitempty"`
}

type SSHCompatStatus struct {
	ClientVersion               string   `json:"client_version,omitempty"`
	TunnelKeyType               string   `json:"tunnel_key_type,omitempty"`
	UserSSHDHostKeyType         string   `json:"user_sshd_host_key_type,omitempty"`
	TunnelOptions               []string `json:"tunnel_options,omitempty"`
	ClientOptions               []string `json:"client_options,omitempty"`
	SupportedAuthorizedKeyTypes []string `json:"supported_authorized_key_types,omitempty"`
}

type AgentCommandResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

type AgentShutdown struct {
	Reason               string `json:"reason"`
	WillRemoveLocalFiles bool   `json:"will_remove_local_files"`
}

type AgentHeartbeat struct {
	MachineID         string               `json:"machine_id"`
	InstallID         string               `json:"install_id,omitempty"`
	Slug              string               `json:"slug,omitempty"`
	AgentVersion      string               `json:"agent_version"`
	Capabilities      AgentCapabilities    `json:"capabilities,omitempty"`
	AppliedGeneration int64                `json:"applied_generation"`
	Observed          AgentObservedState   `json:"observed"`
	CommandResults    []AgentCommandResult `json:"command_results,omitempty"`
	Shutdown          *AgentShutdown       `json:"shutdown,omitempty"`
	// Legacy fields accepted during rolling upgrades.
	Transport      string               `json:"transport,omitempty"`
	TransportState string               `json:"transport_state,omitempty"`
	LocalSSH       AgentComponentStatus `json:"local_ssh,omitempty"`
	Tunnel         AgentTunnelStatus    `json:"tunnel,omitempty"`
	LastError      string               `json:"last_error,omitempty"`
	CheckedAt      string               `json:"checked_at,omitempty"`
}

type HeartbeatResponse struct {
	OK                bool                   `json:"ok"`
	ServerTime        string                 `json:"server_time"`
	DesiredGeneration int64                  `json:"desired_generation"`
	DesiredState      string                 `json:"desired_state"`
	Heartbeat         HeartbeatPolicy        `json:"heartbeat"`
	DesiredConfig     DesiredAgentConfig     `json:"desired_config"`
	Commands          []AgentCommandDelivery `json:"commands"`
	Update            *AgentUpdateHint       `json:"update,omitempty"`
}

type HeartbeatPolicy struct {
	NextIntervalSeconds int `json:"next_interval_seconds"`
	OfflineAfterSeconds int `json:"offline_after_seconds"`
}

type DesiredAgentConfig struct {
	TunnelEnabled       bool                `json:"tunnel_enabled"`
	HubHost             string              `json:"hub_host"`
	HubSSHPort          int                 `json:"hub_ssh_port"`
	RemotePort          int                 `json:"remote_port"`
	TunnelUser          string              `json:"tunnel_user"`
	TransportPolicy     string              `json:"transport_policy"`
	AdminKeysGeneration int64               `json:"admin_keys_generation"`
	AdminPubkeys        []DesiredPubkey     `json:"admin_pubkeys"`
	ProcessTitleConfig  *ProcessTitleConfig `json:"process_title_config,omitempty"`
}

type ProcessTitleConfig struct {
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

type AgentCommandDelivery struct {
	ID         string          `json:"id"`
	Generation int64           `json:"generation"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ExpiresAt  string          `json:"expires_at,omitempty"`
}

type AgentUpdateHint struct {
	Available bool   `json:"available"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Required  bool   `json:"required,omitempty"`
}

func stringPtrFromNull(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func intPtrFromNull(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}
