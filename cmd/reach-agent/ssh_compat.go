package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type SSHCompatConfig struct {
	ClientVersion               string   `yaml:"client_version,omitempty" json:"client_version,omitempty"`
	TunnelKeyType               string   `yaml:"tunnel_key_type,omitempty" json:"tunnel_key_type,omitempty"`
	UserSSHDHostKeyType         string   `yaml:"user_sshd_host_key_type,omitempty" json:"user_sshd_host_key_type,omitempty"`
	TunnelOptions               []string `yaml:"tunnel_options,omitempty" json:"tunnel_options,omitempty"`
	ClientOptions               []string `yaml:"client_options,omitempty" json:"client_options,omitempty"`
	SupportedAuthorizedKeyTypes []string `yaml:"supported_authorized_key_types,omitempty" json:"supported_authorized_key_types,omitempty"`
}

type sshVersionInfo struct {
	Raw         string
	Major       int
	Minor       int
	Patch       int
	Recognized  bool
	HasED25519  bool
	HasECDSA    bool
	LikelyOld53 bool
}

func detectSSHCompatibility(ctx context.Context) SSHCompatConfig {
	v := localSSHVersion(ctx)
	keyTypes := supportedKeygenTypes(ctx)
	if len(keyTypes) == 0 {
		// RSA is the only safe assumption for ancient OpenSSH. If ssh-keygen is
		// present but probing failed because of unsupported flags, the real key
		// generation step will still return a useful error.
		keyTypes = []string{"ssh-rsa"}
	}
	keyType := chooseKeyType(keyTypes)
	compat := SSHCompatConfig{
		ClientVersion:               v.Raw,
		TunnelKeyType:               keyType,
		UserSSHDHostKeyType:         keyType,
		SupportedAuthorizedKeyTypes: keyTypes,
	}
	if needsLegacyClientOptions(v, keyType) {
		compat.TunnelOptions = []string{
			"KexAlgorithms=diffie-hellman-group-exchange-sha256,diffie-hellman-group14-sha1",
			"HostKeyAlgorithms=ssh-rsa",
			"Ciphers=aes128-ctr,aes192-ctr,aes256-ctr",
			"MACs=hmac-sha1",
		}
	}
	return compat
}

func localSSHVersion(ctx context.Context) sshVersionInfo {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-V").CombinedOutput()
	if err != nil && len(out) == 0 {
		return sshVersionInfo{}
	}
	return parseOpenSSHVersion(strings.TrimSpace(string(out)))
}

var opensshVersionRE = regexp.MustCompile(`OpenSSH_([0-9]+)\.([0-9]+)(?:p([0-9]+))?`)

func parseOpenSSHVersion(raw string) sshVersionInfo {
	v := sshVersionInfo{Raw: raw}
	m := opensshVersionRE.FindStringSubmatch(raw)
	if len(m) == 0 {
		return v
	}
	v.Recognized = true
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if len(m) > 3 && m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	v.HasED25519 = versionAtLeast(v, 6, 5)
	v.HasECDSA = versionAtLeast(v, 5, 7)
	v.LikelyOld53 = v.Major < 6 || (v.Major == 6 && v.Minor <= 2)
	return v
}

func versionAtLeast(v sshVersionInfo, major, minor int) bool {
	if !v.Recognized {
		return false
	}
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

func supportedKeygenTypes(ctx context.Context) []string {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return nil
	}
	candidates := []struct {
		keyType string
		args    []string
	}{
		{"ssh-ed25519", []string{"-t", "ed25519"}},
		{"ecdsa-sha2-nistp256", []string{"-t", "ecdsa", "-b", "256"}},
		{"ssh-rsa", []string{"-t", "rsa", "-b", "4096"}},
	}
	var out []string
	for _, c := range candidates {
		if probeKeygenType(ctx, c.args...) {
			out = append(out, c.keyType)
		}
	}
	return out
}

func probeKeygenType(ctx context.Context, args ...string) bool {
	dir, err := os.MkdirTemp("", "reach-keyprobe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	keyPath := filepath.Join(dir, "key")
	cmdArgs := append([]string{"-q"}, args...)
	cmdArgs = append(cmdArgs, "-N", "", "-f", keyPath, "-C", "reach-probe")
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(probeCtx, "ssh-keygen", cmdArgs...).Run() == nil
}

func chooseKeyType(types []string) string {
	for _, want := range []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ssh-rsa"} {
		for _, got := range types {
			if got == want {
				return got
			}
		}
	}
	return "ssh-rsa"
}

func needsLegacyClientOptions(v sshVersionInfo, keyType string) bool {
	if keyType == "ssh-rsa" && (!v.Recognized || !v.HasED25519) {
		return true
	}
	return v.Recognized && v.LikelyOld53
}

func clientOptionsForServerBanner(banner string, hostKeyType string) []string {
	v := parseOpenSSHVersion(banner)
	if (v.Recognized && v.LikelyOld53) || hostKeyType == "ssh-rsa" {
		return []string{
			"KexAlgorithms=diffie-hellman-group14-sha1",
			"HostKeyAlgorithms=ssh-rsa",
			"PubkeyAcceptedAlgorithms=+ssh-rsa",
			"Ciphers=aes128-ctr",
			"MACs=hmac-sha1",
		}
	}
	return nil
}

func generateSSHKey(ctx context.Context, keyPath, keyType, comment string) error {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	args := []string{"-q"}
	switch keyType {
	case "ssh-ed25519":
		args = append(args, "-t", "ed25519")
	case "ecdsa-sha2-nistp256":
		args = append(args, "-t", "ecdsa", "-b", "256")
	case "ssh-rsa", "rsa":
		args = append(args, "-t", "rsa", "-b", "4096")
	default:
		return fmt.Errorf("unsupported ssh key type %q", keyType)
	}
	args = append(args, "-N", "", "-f", keyPath, "-C", comment)
	if out, err := exec.CommandContext(ctx, "ssh-keygen", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen %s failed: %w: %s", keyType, err, strings.TrimSpace(string(out)))
	}
	_ = os.Chmod(keyPath, 0o600)
	_ = os.Chmod(keyPath+".pub", 0o644)
	return nil
}

func publicKeyType(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func keyTypeSupported(keyType string, supported []string) bool {
	for _, s := range supported {
		if s == keyType {
			return true
		}
	}
	return false
}

func sshServerBanner(ctx context.Context, host string, port int, timeout time.Duration) string {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}
