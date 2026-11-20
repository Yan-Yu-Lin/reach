package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reach/internal/safehttp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aead.dev/minisign"
)

const defaultUpdateRollbackAfter = 5 * time.Minute

const releaseManifestProject = "reach-agent"

var releaseManifestPublicKeys = []string{
	"RWQ+MT09E8yd1V1tf3J3NI3Eb7L9DgMgNrN4SiqdrTs1Y2C61++bpyYY",
}

type releaseManifest struct {
	Schema  int    `json:"schema"`
	Project string `json:"project"`
	Version string `json:"version"`
	// Sequence is a monotonically increasing release number inside the
	// signed manifest. Manifests published before it existed fall back to
	// their signed created_at.
	Sequence  int64                   `json:"sequence,omitempty"`
	GitCommit string                  `json:"git_commit,omitempty"`
	CreatedAt string                  `json:"created_at"`
	Assets    map[string]releaseAsset `json:"assets"`
}

type releaseAsset struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type updateBinaryOptions struct {
	Version       string
	APIURL        string
	CandidatePath string
	InstallEnv    string
	RollbackAfter time.Duration
	Foreground    bool
	Confirm       bool
	// AllowDowngrade is a local break-glass flag. Remote update_agent
	// commands never pass it.
	AllowDowngrade bool
	// WasActive: the machine was meant to be online (tunnel enabled) when
	// the update started. The confirm gate uses this recorded value, not
	// the hub's answer after the update (daybreak round 3 P2-E).
	WasActive bool

	rollbackWatcher bool
	confirmFlag     string
	backupPath      string
	agentPath       string
	installMode     string
	configPath      string
	dataDir         string
	logPath         string
}

type reachInstallState struct {
	EnvPath     string
	AgentPath   string
	DataDir     string
	InstallMode string
	ConfigPath  string
	PIDFile     string
	LogPath     string
}

func updateBinaryCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update-binary", flag.ContinueOnError)
	opt := updateBinaryOptions{}
	rollbackAfter := fs.Duration("rollback-after", defaultUpdateRollbackAfter, "restore the previous binary unless confirmed within this duration")
	fs.StringVar(&opt.Version, "version", envDefault("REACH_AGENT_VERSION", ""), "Reach agent version to install from /downloads/reach-agent/vVERSION")
	fs.StringVar(&opt.APIURL, "api-url", envDefault("REACH_API_URL", ""), "Reach API URL/download host")
	fs.StringVar(&opt.CandidatePath, "candidate", "", "already-downloaded candidate binary to install instead of downloading")
	fs.StringVar(&opt.InstallEnv, "install-env", "", "path to existing install.env")
	fs.BoolVar(&opt.Foreground, "foreground", false, "perform the update in this process instead of spawning a detached worker")
	fs.BoolVar(&opt.Confirm, "confirm", false, "confirm the current manual update and cancel rollback")
	fs.BoolVar(&opt.AllowDowngrade, "allow-downgrade", false, "local break-glass: accept a signed release older than the last confirmed one")
	fs.BoolVar(&opt.WasActive, "was-active", true, "the machine is meant to be online; confirmation then requires a working tunnel and SSH login")
	fs.BoolVar(&opt.rollbackWatcher, "rollback-watcher", false, "internal: run rollback watcher")
	fs.StringVar(&opt.confirmFlag, "confirm-flag", "", "internal: confirmation flag path")
	fs.StringVar(&opt.backupPath, "backup", "", "internal: backup binary path")
	fs.StringVar(&opt.agentPath, "agent-path", "", "internal: installed agent path")
	fs.StringVar(&opt.installMode, "install-mode", "", "internal: install mode")
	fs.StringVar(&opt.configPath, "config", "", "internal: agent config path")
	fs.StringVar(&opt.dataDir, "data-dir", "", "internal: data directory")
	fs.StringVar(&opt.logPath, "log", "", "internal: update log path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opt.RollbackAfter = *rollbackAfter

	if opt.rollbackWatcher {
		return runUpdateRollbackWatcher(ctx, opt)
	}

	st, err := loadReachInstallState(opt.InstallEnv)
	if err != nil {
		return err
	}
	if opt.Confirm {
		return confirmUpdate(st)
	}
	if !opt.Foreground {
		return startDetachedUpdateWorker(st, args)
	}
	return performBinaryUpdate(ctx, st, opt)
}

func loadReachInstallState(explicit string) (reachInstallState, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		candidates = append(candidates, "/etc/reach/install.env")
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidates = append(candidates, filepath.Join(home, ".config", "reach", "install.env"))
		}
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		env := readInstallEnv(p)
		state := reachInstallState{EnvPath: p, AgentPath: env["AGENT_PATH"], DataDir: env["DATA_DIR"], InstallMode: env["INSTALL_MODE"], ConfigPath: env["AGENT_CONFIG"], PIDFile: env["PID_FILE"]}
		if state.InstallMode == "" {
			state.InstallMode = "user"
		}
		if state.ConfigPath == "" {
			state.ConfigPath = filepath.Join(filepath.Dir(p), "agent.yaml")
		}
		if state.DataDir == "" {
			if state.InstallMode == "system" {
				state.DataDir = "/var/lib/reach"
			} else if home, err := os.UserHomeDir(); err == nil && home != "" {
				state.DataDir = filepath.Join(home, ".local", "share", "reach")
			}
		}
		if state.AgentPath == "" {
			if state.InstallMode == "system" {
				state.AgentPath = "/opt/reach/reach-agent"
			} else if home, err := os.UserHomeDir(); err == nil && home != "" {
				state.AgentPath = filepath.Join(home, ".local", "bin", "reach-agent")
			}
		}
		if state.PIDFile == "" && state.DataDir != "" {
			state.PIDFile = filepath.Join(state.DataDir, "reach-agent.pid")
		}
		if state.LogPath == "" && state.DataDir != "" {
			state.LogPath = filepath.Join(state.DataDir, "agent.log")
		}
		if state.AgentPath == "" || state.DataDir == "" || state.ConfigPath == "" {
			return state, fmt.Errorf("existing Reach install is missing AGENT_PATH, DATA_DIR, or AGENT_CONFIG in %s", p)
		}
		if _, err := os.Stat(state.AgentPath); err != nil {
			return state, fmt.Errorf("installed reach-agent not found at %s: %w", state.AgentPath, err)
		}
		if _, err := os.Stat(state.ConfigPath); err != nil {
			return state, fmt.Errorf("agent config not found at %s: %w", state.ConfigPath, err)
		}
		return state, nil
	}
	return reachInstallState{}, errors.New("no existing Reach install found; refusing to register, provision, or uninstall during binary update")
}

func startDetachedUpdateWorker(st reachInstallState, args []string) error {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return fmt.Errorf("could not find current executable for detached update: %w", err)
	}
	id := time.Now().UTC().Format("20060102T150405Z")
	if err := os.MkdirAll(st.DataDir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(st.DataDir, "update-binary-"+id+".log")
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
	workerArgs := []string{"update-binary", "--foreground", "--install-env", st.EnvPath}
	workerArgs = append(workerArgs, stripUpdateWorkerOnlyArgs(args)...)
	cmd := exec.Command(exe, workerArgs...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	fmt.Printf("[reach] started detached binary update pid=%d log=%s\n", pid, logPath)
	fmt.Printf("[reach] if the tunnel reconnects, confirm before rollback with:\n  %s update-binary --confirm\n", st.AgentPath)
	fmt.Printf("[reach] rollback will run automatically if not confirmed within %s.\n", defaultUpdateRollbackAfter)
	return nil
}

func stripUpdateWorkerOnlyArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--foreground", "--confirm", "--rollback-watcher":
			continue
		case "--install-env":
			i++
			continue
		}
		if strings.HasPrefix(a, "--install-env=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func performBinaryUpdate(ctx context.Context, st reachInstallState, opt updateBinaryOptions) error {
	switch runtime.GOOS {
	case "linux", "darwin":
	default:
		return fmt.Errorf("reach-agent update-binary currently supports Linux and macOS only (got %s)", runtime.GOOS)
	}
	if st.InstallMode == "system" && os.Geteuid() != 0 {
		return fmt.Errorf("system Reach install at %s requires root to update; rerun with sudo", st.AgentPath)
	}
	apiURL := opt.APIURL
	if apiURL == "" {
		if cfg, err := LoadConfig(st.ConfigPath); err == nil {
			apiURL = cfg.Heartbeat.APIURL
		}
	}
	if apiURL != "" {
		if err := validateAPIURL(apiURL); err != nil {
			return err
		}
	}
	if apiURL == "" && opt.CandidatePath == "" {
		return fmt.Errorf("--api-url is required when the existing config does not provide heartbeat.api_url")
	}
	fmt.Printf("[reach] existing install: mode=%s agent=%s config=%s data=%s\n", st.InstallMode, st.AgentPath, st.ConfigPath, st.DataDir)

	candidatePath := opt.CandidatePath
	cleanup := func() {}
	if opt.Version == "" {
		return fmt.Errorf("--version is required")
	}
	asset, err := detectAgentAsset()
	if err != nil {
		return err
	}
	var manifest releaseManifest
	if candidatePath == "" {
		path, m, done, err := downloadAgentCandidate(ctx, apiURL, opt.Version, asset, st.DataDir, opt.AllowDowngrade)
		if err != nil {
			return err
		}
		candidatePath, cleanup, manifest = path, done, m
	} else {
		// Copy the caller's file into a private 0700 directory first and
		// verify, preflight and install only that copy, so nobody who can
		// write the original path can swap it between the hash check and
		// execution (daybreak P2-2).
		private, done, err := privateCandidateCopy(candidatePath)
		if err != nil {
			return err
		}
		candidatePath, cleanup = private, done
		m, err := verifyCandidateManifest(ctx, apiURL, opt.Version, asset, candidatePath, st.DataDir, opt.AllowDowngrade)
		if err != nil {
			cleanup()
			return err
		}
		manifest = m
	}
	defer cleanup()

	candidateVersion, err := preflightAgentBinary(ctx, candidatePath)
	if err != nil {
		return err
	}
	candidateVersion = strings.TrimSpace(candidateVersion)
	if candidateVersion != opt.Version {
		return fmt.Errorf("candidate version mismatch: binary reports %q but requested %q", candidateVersion, opt.Version)
	}
	fmt.Printf("[reach] candidate binary preflight OK: version=%s\n", candidateVersion)

	id := time.Now().UTC().Format("20060102T150405Z")
	if err := os.MkdirAll(filepath.Join(st.DataDir, "update-backups"), 0o700); err != nil {
		return err
	}
	backupPath := filepath.Join(st.DataDir, "update-backups", "reach-agent.backup."+id)
	if err := copyFile(st.AgentPath, backupPath, 0o755); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	fmt.Printf("[reach] backed up current binary to %s\n", backupPath)

	if prev, err := readUpdateState(st.DataDir); err != nil {
		return err
	} else if !prev.terminal() {
		return fmt.Errorf("an update to %s is still %s; confirm it or let it roll back first", prev.CandidateVersion, prev.State)
	}
	now := time.Now().UTC()
	ust := &updateState{State: updPending, Backup: backupPath, AgentPath: st.AgentPath, InstallMode: st.InstallMode, ConfigPath: st.ConfigPath, CandidateVersion: candidateVersion, CandidateSequence: manifestSequence(manifest), WasActive: opt.WasActive, InstalledAt: now.Format(time.RFC3339), Deadline: now.Add(opt.RollbackAfter).Format(time.RFC3339)}
	if err := withUpdateLock(st.DataDir, func() error { return writeUpdateState(st.DataDir, ust) }); err != nil {
		return fmt.Errorf("record update state: %w", err)
	}

	if err := atomicInstallBinary(candidatePath, st.AgentPath); err != nil {
		ust.State = updRolledBack
		_ = withUpdateLock(st.DataDir, func() error { return writeUpdateState(st.DataDir, ust) })
		return err
	}
	fmt.Printf("[reach] installed candidate at %s\n", st.AgentPath)

	rollbackLog := filepath.Join(st.DataDir, "update-rollback-"+id+".log")
	if err := startRollbackWatcher(st, rollbackLog, opt.RollbackAfter); err != nil {
		if _, rerr := rollbackIfUnconfirmed(st.DataDir); rerr != nil {
			return fmt.Errorf("start rollback watcher failed (%v) and restoring the backup failed: %w", err, rerr)
		}
		return fmt.Errorf("start rollback watcher failed; restored backup: %w", err)
	}
	fmt.Printf("[reach] rollback watcher armed: %s (timeout %s)\n", rollbackLog, opt.RollbackAfter)

	fmt.Printf("[reach] restarting reach-agent service/process now; this SSH session may drop.\n")
	if err := restartReachAgent(ctx, st); err != nil {
		return fmt.Errorf("restart after update failed; rollback watcher remains armed: %w", err)
	}
	fmt.Printf("[reach] update restart requested. If the tunnel reconnects, confirm before rollback with:\n  %s update-binary --confirm\n", st.AgentPath)
	return nil
}

func detectAgentAsset() (string, error) {
	arch := ""
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		arch = strings.TrimSpace(string(out))
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return agentAssetForOSArch(runtime.GOOS, arch)
}

func agentAssetForArch(arch string) (string, error) {
	return agentAssetForOSArch("linux", arch)
}

func agentAssetForOSArch(goos, arch string) (string, error) {
	normArch := strings.ToLower(strings.TrimSpace(arch))
	switch goos {
	case "linux":
		switch normArch {
		case "x86_64", "amd64":
			return "reach-agent_linux_amd64", nil
		case "aarch64", "arm64":
			return "reach-agent_linux_arm64", nil
		case "armv7l", "armv7", "armv7hl":
			return "reach-agent_linux_armv7", nil
		case "armv6l", "armv6":
			return "reach-agent_linux_armv6", nil
		case "i386", "i686", "386":
			return "reach-agent_linux_386", nil
		}
	case "darwin":
		switch normArch {
		case "x86_64", "amd64":
			return "reach-agent_darwin_amd64", nil
		case "aarch64", "arm64":
			return "reach-agent_darwin_arm64", nil
		}
	}
	return "", fmt.Errorf("unsupported platform %s/%s", goos, arch)
}

func verifyCandidateManifest(ctx context.Context, apiURL, version, asset, candidatePath, dataDir string, allowDowngrade bool) (releaseManifest, error) {
	if apiURL == "" {
		return releaseManifest{}, fmt.Errorf("--api-url is required to verify --candidate")
	}
	baseURL := strings.TrimRight(apiURL, "/") + "/downloads/reach-agent/v" + version
	manifest, releaseAsset, err := downloadAndVerifyReleaseManifest(ctx, baseURL, version, asset)
	if err != nil {
		return releaseManifest{}, err
	}
	if err := checkReleaseFloor(dataDir, manifest, allowDowngrade); err != nil {
		return releaseManifest{}, err
	}
	actual, err := verifyFileSHA256(candidatePath, releaseAsset)
	if err != nil {
		return releaseManifest{}, fmt.Errorf("candidate verification failed for %s: %w", asset, err)
	}
	fmt.Printf("[reach] signed release manifest verified: version=%s created_at=%s\n", manifest.Version, manifest.CreatedAt)
	fmt.Printf("[reach] candidate checksum verified from signed manifest: %s\n", actual)
	return manifest, nil
}

func downloadAgentCandidate(ctx context.Context, apiURL, version, asset, dataDir string, allowDowngrade bool) (string, releaseManifest, func(), error) {
	tmpDir, err := os.MkdirTemp("", "reach-agent-update-*")
	if err != nil {
		return "", releaseManifest{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	baseURL := strings.TrimRight(apiURL, "/") + "/downloads/reach-agent/v" + version
	manifest, releaseAsset, err := downloadAndVerifyReleaseManifest(ctx, baseURL, version, asset)
	if err != nil {
		cleanup()
		return "", releaseManifest{}, nil, err
	}
	if err := checkReleaseFloor(dataDir, manifest, allowDowngrade); err != nil {
		cleanup()
		return "", releaseManifest{}, nil, err
	}
	binURL := baseURL + "/" + asset
	binPath := filepath.Join(tmpDir, "reach-agent")
	fmt.Printf("[reach] signed release manifest verified: version=%s created_at=%s\n", manifest.Version, manifest.CreatedAt)
	fmt.Printf("[reach] downloading %s\n", binURL)
	if err := downloadFile(ctx, binURL, binPath, maxDownloadBytes(releaseAsset.Size)); err != nil {
		cleanup()
		return "", releaseManifest{}, nil, err
	}
	actual, err := verifyFileSHA256(binPath, releaseAsset)
	if err != nil {
		cleanup()
		return "", releaseManifest{}, nil, fmt.Errorf("download verification failed for %s: %w", asset, err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		cleanup()
		return "", releaseManifest{}, nil, err
	}
	fmt.Printf("[reach] checksum verified from signed manifest: %s\n", actual)
	return binPath, manifest, cleanup, nil
}

func downloadAndVerifyReleaseManifest(ctx context.Context, baseURL, version, asset string) (releaseManifest, releaseAsset, error) {
	tmpDir, err := os.MkdirTemp("", "reach-agent-manifest-*")
	if err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	defer os.RemoveAll(tmpDir)
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	sigPath := filepath.Join(tmpDir, "manifest.json.minisig")
	if err := downloadFile(ctx, baseURL+"/manifest.json", manifestPath, 1<<20); err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	if err := downloadFile(ctx, baseURL+"/manifest.json.minisig", sigPath, 16<<10); err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	if err := verifyReleaseManifestSignature(manifestBytes, sigBytes); err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	manifest, assetInfo, err := parseReleaseManifest(manifestBytes, version, asset)
	if err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	return manifest, assetInfo, nil
}

func verifyReleaseManifestSignature(manifestBytes, sigBytes []byte) error {
	var parseErrs []string
	for _, keyText := range releaseManifestPublicKeys {
		keyText = strings.TrimSpace(keyText)
		if keyText == "" {
			continue
		}
		var publicKey minisign.PublicKey
		if err := publicKey.UnmarshalText([]byte(keyText)); err != nil {
			parseErrs = append(parseErrs, err.Error())
			continue
		}
		if minisign.Verify(publicKey, manifestBytes, sigBytes) {
			return nil
		}
	}
	if len(parseErrs) > 0 {
		return fmt.Errorf("release manifest signature verification failed (public key parse errors: %s)", strings.Join(parseErrs, "; "))
	}
	return fmt.Errorf("release manifest signature verification failed")
}

func parseReleaseManifest(manifestBytes []byte, version, asset string) (releaseManifest, releaseAsset, error) {
	var manifest releaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return releaseManifest{}, releaseAsset{}, fmt.Errorf("parse release manifest: %w", err)
	}
	if manifest.Schema != 1 {
		return releaseManifest{}, releaseAsset{}, fmt.Errorf("unsupported release manifest schema %d", manifest.Schema)
	}
	if manifest.Project != releaseManifestProject {
		return releaseManifest{}, releaseAsset{}, fmt.Errorf("release manifest project %q does not match %q", manifest.Project, releaseManifestProject)
	}
	if manifest.Version != version {
		return releaseManifest{}, releaseAsset{}, fmt.Errorf("release manifest version %q does not match requested %q", manifest.Version, version)
	}
	assetInfo, ok := manifest.Assets[asset]
	if !ok {
		return releaseManifest{}, releaseAsset{}, fmt.Errorf("release manifest has no asset %s", asset)
	}
	if err := validateReleaseAsset(asset, assetInfo); err != nil {
		return releaseManifest{}, releaseAsset{}, err
	}
	return manifest, assetInfo, nil
}

func validateReleaseAsset(asset string, releaseAsset releaseAsset) error {
	if len(releaseAsset.SHA256) != sha256.Size*2 {
		return fmt.Errorf("invalid sha256 length for %s", asset)
	}
	if _, err := hex.DecodeString(releaseAsset.SHA256); err != nil {
		return fmt.Errorf("invalid sha256 for %s: %w", asset, err)
	}
	if releaseAsset.Size <= 0 {
		return fmt.Errorf("invalid size for %s: %d", asset, releaseAsset.Size)
	}
	return nil
}

func verifyFileSHA256(path string, releaseAsset releaseAsset) (string, error) {
	if st, err := os.Stat(path); err != nil {
		return "", err
	} else if st.Size() != releaseAsset.Size {
		return "", fmt.Errorf("size mismatch: got %d expected %d", st.Size(), releaseAsset.Size)
	}
	actual, err := fileSHA256Hex(path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(actual, releaseAsset.SHA256) {
		return "", fmt.Errorf("checksum mismatch: got %s expected %s", actual, releaseAsset.SHA256)
	}
	return actual, nil
}

func maxDownloadBytes(size int64) int64 {
	const defaultMax = 128 << 20
	if size <= 0 || size > defaultMax {
		return defaultMax
	}
	return size
}

func downloadFile(ctx context.Context, url, path string, maxBytes int64) error {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := safehttp.ValidateURL(url); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil && st.Size() > maxBytes {
		return fmt.Errorf("download %s exceeded %d bytes", url, maxBytes)
	}
	return f.Sync()
}

func checksumForAsset(checksums []byte, asset string) (string, error) {
	s := bufio.NewScanner(bytes.NewReader(checksums))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) == asset {
			if len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("invalid sha256 length for %s", asset)
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return "", fmt.Errorf("invalid sha256 for %s: %w", asset, err)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum found for %s", asset)
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func preflightAgentBinary(ctx context.Context, path string) (string, error) {
	preflightCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(preflightCtx, path, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("candidate preflight failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("candidate preflight failed: version output was empty")
	}
	return v, nil
}

func atomicInstallBinary(src, dst string) error {
	st, err := os.Stat(dst)
	if err != nil {
		return err
	}
	mode := st.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".new-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	if _, err := io.Copy(tmp, sf); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	return out.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	return d.Sync()
}

func startRollbackWatcher(st reachInstallState, logPath string, after time.Duration) error {
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
	args := []string{"update-binary", "--rollback-watcher", "--agent-path", st.AgentPath, "--install-mode", st.InstallMode, "--config", st.ConfigPath, "--data-dir", st.DataDir, "--rollback-after", after.String(), "--log", logPath}
	cmd := exec.Command(st.AgentPath, args...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runUpdateRollbackWatcher(ctx context.Context, opt updateBinaryOptions) error {
	if opt.RollbackAfter <= 0 {
		opt.RollbackAfter = defaultUpdateRollbackAfter
	}
	if opt.dataDir == "" {
		return fmt.Errorf("rollback watcher missing --data-dir")
	}
	// Legacy watchers were given a flag and backup; finish those the old way.
	if opt.confirmFlag != "" {
		return runLegacyRollbackWatcher(ctx, opt)
	}
	fmt.Printf("[reach] rollback watcher waiting %s for confirmation\n", opt.RollbackAfter)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(opt.RollbackAfter):
	}
	// Decide under the update lock from the durable state: anything but
	// "confirmed" (including a confirm still running or one that crashed)
	// is rolled back (astra R2-09).
	restored, err := rollbackIfUnconfirmed(opt.dataDir)
	if err != nil {
		return err
	}
	if !restored {
		fmt.Printf("[reach] update was confirmed; rollback canceled\n")
		return nil
	}
	st := reachInstallState{AgentPath: opt.agentPath, InstallMode: opt.installMode, ConfigPath: opt.configPath, DataDir: opt.dataDir, PIDFile: filepath.Join(opt.dataDir, "reach-agent.pid"), LogPath: filepath.Join(opt.dataDir, "agent.log")}
	if err := restartReachAgent(ctx, st); err != nil {
		return fmt.Errorf("rollback restored binary but restart failed: %w", err)
	}
	fmt.Printf("[reach] update was not confirmed; rollback complete\n")
	return nil
}

// resumeInterruptedUpdate runs at agent start. If an update is unfinished
// and its deadline passed (the watcher died, or a confirm/rollback
// crashed), roll it back now; if the deadline is still ahead, make sure a
// watcher exists by starting a new one for the remaining time.
func resumeInterruptedUpdate(ctx context.Context, st reachInstallState, logf func(string, ...any)) {
	ust, err := readUpdateState(st.DataDir)
	if err != nil {
		logf("update state unreadable: %v", err)
		return
	}
	if ust.terminal() {
		return
	}
	remaining := time.Until(ust.deadline())
	if remaining > 0 {
		logPath := filepath.Join(st.DataDir, "update-rollback-resume-"+time.Now().UTC().Format("20060102T150405Z")+".log")
		if err := startRollbackWatcher(st, logPath, remaining); err != nil {
			logf("could not re-arm rollback watcher: %v", err)
		}
		return
	}
	restored, err := rollbackIfUnconfirmed(st.DataDir)
	if err != nil {
		logf("interrupted update rollback failed: %v", err)
		return
	}
	if restored {
		logf("rolled back unconfirmed update to %s; restarting", ust.CandidateVersion)
		if err := restartReachAgent(ctx, st); err != nil {
			logf("restart after rollback failed: %v", err)
		}
	}
}

// runLegacyRollbackWatcher handles a watcher started by an older agent
// (flag file protocol), so an in-flight update survives the upgrade.
func runLegacyRollbackWatcher(ctx context.Context, opt updateBinaryOptions) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(opt.RollbackAfter):
	}
	if opt.backupPath == "" || opt.agentPath == "" {
		return fmt.Errorf("rollback watcher missing required paths")
	}
	var restored bool
	err := withUpdateLock(opt.dataDir, func() error {
		if _, err := os.Stat(opt.confirmFlag); err != nil {
			return nil // confirmed (legacy confirm removes the flag under the lock)
		}
		if err := restoreBinary(opt.backupPath, opt.agentPath); err != nil {
			return err
		}
		restored = true
		return os.Remove(opt.confirmFlag)
	})
	if err != nil || !restored {
		return err
	}
	st := reachInstallState{AgentPath: opt.agentPath, InstallMode: opt.installMode, ConfigPath: opt.configPath, DataDir: opt.dataDir, PIDFile: filepath.Join(opt.dataDir, "reach-agent.pid"), LogPath: filepath.Join(opt.dataDir, "agent.log")}
	return restartReachAgent(ctx, st)
}

func restoreBinary(backupPath, agentPath string) error {
	return atomicInstallBinary(backupPath, agentPath)
}

// confirmUpdate cancels the pending rollback and raises the anti-rollback
// floor, through the durable state machine (update_state.go). If the floor
// cannot be recorded the update stays pending and the rollback armed.
func confirmUpdate(st reachInstallState) error {
	handled, err := confirmUpdateState(st.DataDir)
	if err != nil {
		return err
	}
	if handled {
		fmt.Printf("[reach] update confirmed; rollback canceled.\n")
		return nil
	}
	return confirmLegacyFlag(st)
}

// confirmLegacyFlag confirms an update started by an older agent (flag
// file). It holds the update lock, so a legacy watcher cannot act halfway.
func confirmLegacyFlag(st reachInstallState) error {
	flag := filepath.Join(st.DataDir, "update-confirm-required")
	return withUpdateLock(st.DataDir, func() error {
		b, err := os.ReadFile(flag)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("[reach] no pending update confirmation found\n")
			return nil
		}
		if err != nil {
			return err
		}
		if seq := confirmFlagSequence(string(b)); seq > 0 {
			if err := raiseReleaseFloor(st.DataDir, seq); err != nil {
				return fmt.Errorf("record release floor: %w", err)
			}
		}
		if err := os.Remove(flag); err != nil {
			return err
		}
		fmt.Printf("[reach] update confirmed; rollback canceled.\n")
		return syncDir(st.DataDir)
	})
}

func restartLaunchdAgent(ctx context.Context, st reachInstallState) error {
	if _, err := exec.LookPath("launchctl"); err == nil {
		label := launchdLabel
		if st.InstallMode == "system" {
			service := "system/" + label
			if out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", service).CombinedOutput(); err == nil {
				return nil
			} else {
				fmt.Printf("[reach warning] launchctl kickstart failed, trying pid-file fallback: %s\n", strings.TrimSpace(string(out)))
			}
		} else {
			service := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
			if out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", service).CombinedOutput(); err == nil {
				return nil
			} else {
				fmt.Printf("[reach warning] launchctl kickstart failed, trying pid-file fallback: %s\n", strings.TrimSpace(string(out)))
			}
		}
	}
	return restartDetachedAgent(st)
}

func restartReachAgent(ctx context.Context, st reachInstallState) error {
	if runtime.GOOS == "darwin" {
		return restartLaunchdAgent(ctx, st)
	}
	if st.InstallMode == "system" {
		if _, err := exec.LookPath("systemctl"); err == nil {
			cmd := exec.CommandContext(ctx, "systemctl", "restart", "reach-agent.service")
			if out, err := cmd.CombinedOutput(); err == nil {
				return nil
			} else {
				fmt.Printf("[reach warning] systemctl restart failed, trying pid-file fallback: %s\n", strings.TrimSpace(string(out)))
			}
		}
		return restartDetachedAgent(st)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.CommandContext(ctx, "systemctl", "--user", "restart", "reach-agent.service")
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			fmt.Printf("[reach warning] systemctl --user restart failed, trying pid-file fallback: %s\n", strings.TrimSpace(string(out)))
		}
	}
	return restartDetachedAgent(st)
}

func restartDetachedAgent(st reachInstallState) error {
	if st.PIDFile == "" {
		st.PIDFile = filepath.Join(st.DataDir, "reach-agent.pid")
	}
	if st.LogPath == "" {
		st.LogPath = filepath.Join(st.DataDir, "agent.log")
	}
	if pid, ok := readPID(st.PIDFile); ok && processAlive(pid) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(250 * time.Millisecond)
		}
		if processAlive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
	_ = os.Remove(st.PIDFile)
	return startDetached(st.AgentPath, st.ConfigPath, st.LogPath, st.PIDFile)
}

// --- anti-rollback (daybreak P2-6) ---

const releaseFloorFile = "release-floor.json"

type releaseFloor struct {
	Sequence  int64  `json:"sequence"`
	UpdatedAt string `json:"updated_at"`
}

// minReleaseSequence pins the ordering contract (astra P2-04): a release
// sequence is a Unix-seconds value, in the same space as the created_at
// fallback of legacy manifests. reach-release defaults to the build time and
// deploy refuses to publish a sequence not greater than the previous one, so
// counters may advance faster than the clock but never restart small.
const minReleaseSequence int64 = 1_000_000_000

// manifestSequence returns the signed ordering key of a manifest, or 0 if it
// has none usable.
func manifestSequence(m releaseManifest) int64 {
	if m.Sequence != 0 {
		if m.Sequence < minReleaseSequence {
			return 0
		}
		return m.Sequence
	}
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil && t.Unix() >= minReleaseSequence {
		return t.Unix()
	}
	return 0
}

func readReleaseFloor(dataDir string) (int64, error) {
	if dataDir == "" {
		return 0, nil
	}
	b, err := os.ReadFile(filepath.Join(dataDir, releaseFloorFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var f releaseFloor
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, fmt.Errorf("corrupt %s: %w", releaseFloorFile, err)
	}
	return f.Sequence, nil
}

// checkReleaseFloor refuses a signed manifest older than the highest release
// this agent has confirmed. A compromised hub can only offer validly signed
// releases; this stops it from downgrading to one with known bugs.
func checkReleaseFloor(dataDir string, m releaseManifest, allowDowngrade bool) error {
	seq := manifestSequence(m)
	if seq <= 0 {
		return fmt.Errorf("release manifest %s has no usable sequence (must be Unix seconds >= %d) or created_at", m.Version, minReleaseSequence)
	}
	floor, err := readReleaseFloor(dataDir)
	if err != nil {
		return err
	}
	if seq < floor {
		if allowDowngrade {
			fmt.Printf("[reach warning] --allow-downgrade: accepting release %s (sequence %d < floor %d)\n", m.Version, seq, floor)
			return nil
		}
		return fmt.Errorf("refusing downgrade to %s: release sequence %d is older than the last confirmed release (%d); rerun locally with --allow-downgrade if intended", m.Version, seq, floor)
	}
	return nil
}

func raiseReleaseFloor(dataDir string, seq int64) error {
	if dataDir == "" || seq <= 0 {
		return nil
	}
	cur, err := readReleaseFloor(dataDir)
	if err != nil {
		return err
	}
	if seq <= cur {
		return nil
	}
	b, _ := json.Marshal(releaseFloor{Sequence: seq, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dataDir, releaseFloorFile), b, 0o600)
}

func confirmFlagSequence(flag string) int64 {
	for _, line := range strings.Split(flag, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "candidate_sequence="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// embeddedSequence parses the build's release sequence; 0 when unset or
// outside the Unix-seconds space.
func embeddedSequence() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(releaseSequence), 10, 64)
	if err != nil || n < minReleaseSequence {
		return 0
	}
	return n
}

// seedReleaseFloor raises the anti-rollback floor to this binary's own
// release on start, so a fresh install (no confirmed update yet) already
// refuses older signed releases (daybreak P2-1). The floor never moves down,
// and a binary that is running at all has been installed by the operator or
// a verified update, so it is a safe lower bound. Skipped while an update
// awaits confirmation: a rollback to the previous binary must stay possible.
func (d *Daemon) seedReleaseFloor() {
	seq := embeddedSequence()
	dir := d.cfg.Install.DataDir
	if seq == 0 || dir == "" {
		return
	}
	if updatePending(dir) {
		return
	}
	if err := raiseReleaseFloor(dir, seq); err != nil {
		d.logger.Printf("could not seed release floor: %v", err)
	}
}

// privateCandidateCopy copies a regular, non-symlink file into a fresh 0700
// temp directory owned by the current user and returns the copy's path.
func privateCandidateCopy(src string) (string, func(), error) {
	st, err := os.Lstat(src)
	if err != nil {
		return "", nil, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, fmt.Errorf("--candidate %s is not a regular file", src)
	}
	if st.Size() > 128<<20 {
		return "", nil, fmt.Errorf("--candidate %s is too large", src)
	}
	dir, err := os.MkdirTemp("", "reach-agent-candidate-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	dst := filepath.Join(dir, "reach-agent")
	if err := copyFileNoFollow(src, dst, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	return dst, cleanup, nil
}
