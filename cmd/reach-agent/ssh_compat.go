package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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
	userKeyTypes := supportedKeygenTypes(ctx)
	if len(userKeyTypes) == 0 {
		// RSA is the only safe assumption for ancient OpenSSH authorized_keys and
		// host keys when no local ssh-keygen is available to probe support. Tunnel
		// keys are independent: reach-agent's embedded Go SSH client can always use
		// Ed25519, and the hub validates keys with x/crypto/ssh.
		userKeyTypes = []string{"ssh-rsa"}
	}
	userKeyType := chooseKeyType(userKeyTypes)
	compat := SSHCompatConfig{
		ClientVersion:               v.Raw,
		TunnelKeyType:               "ssh-ed25519",
		UserSSHDHostKeyType:         userKeyType,
		SupportedAuthorizedKeyTypes: userKeyTypes,
	}
	if needsLegacyClientOptions(v, userKeyType) {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}

	privatePEM, publicKey, err := generateSSHKeyMaterial(keyType, comment)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath, privatePEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath+".pub", publicKey, 0o644); err != nil {
		return err
	}
	return nil
}

func generateSSHKeyMaterial(keyType, comment string) (privatePEM, publicKey []byte, err error) {
	var signer ssh.Signer
	switch keyType {
	case "ssh-ed25519":
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, comment)
		if err != nil {
			return nil, nil, err
		}
		privatePEM = pem.EncodeToMemory(block)
		signer, err = ssh.NewSignerFromKey(priv)
		if err != nil {
			return nil, nil, err
		}
	case "ecdsa-sha2-nistp256":
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			return nil, nil, err
		}
		privatePEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		signer, err = ssh.NewSignerFromKey(priv)
		if err != nil {
			return nil, nil, err
		}
	case "ssh-rsa", "rsa":
		priv, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return nil, nil, err
		}
		privatePEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
		signer, err = ssh.NewSignerFromKey(priv)
		if err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("unsupported ssh key type %q", keyType)
	}
	if len(privatePEM) == 0 {
		return nil, nil, fmt.Errorf("encode %s private key failed", keyType)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		pub += " " + comment
	}
	return privatePEM, []byte(pub + "\n"), nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, perm)
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

func ensurePublicKeyForPrivateKey(privatePath, comment string) error {
	b, err := os.ReadFile(privatePath)
	if err != nil {
		return err
	}
	signer, err := parseTunnelPrivateKey(b)
	if err != nil {
		return err
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		pub += " " + comment
	}
	return writeFileAtomic(privatePath+".pub", []byte(pub+"\n"), 0o644)
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
