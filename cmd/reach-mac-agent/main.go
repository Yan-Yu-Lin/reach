package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const version = "0.1.0-alpha"

type Config struct {
	APIURL         string `yaml:"api_url"`
	Token          string `yaml:"token"`
	TokenFile      string `yaml:"token_file"`
	OutFile        string `yaml:"out_file"`
	SSHConfigFile  string `yaml:"ssh_config_file"`
	InstallInclude bool   `yaml:"install_include"`
}

type Event struct {
	ID   string
	Type string
	Data string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatalf("reach-mac-agent: %v", err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"run"}
	}
	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		cfgPath := fs.String("config", defaultConfigPath(), "config path")
		once := fs.Bool("once", false, "sync once and exit")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		ag := &Agent{cfg: cfg, log: log.New(os.Stdout, "reach-mac-agent ", log.LstdFlags|log.Lmicroseconds)}
		ag.ensureServiceToken(ctx)
		if *once {
			return ag.Sync(ctx)
		}
		return ag.Run(ctx)
	case "sample-config":
		cfg := defaultConfig()
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
	fmt.Printf(`Reach Mac agent %s

Usage:
  reach-mac-agent run [--config ~/.config/reach/mac-agent.yaml] [--once]
  reach-mac-agent sample-config
  reach-mac-agent version
`, version)
}

const (
	maxSSHConfigBytes = 2 << 20
	maxSSEEventBytes  = 2 << 20
	eventSyncAttempts = 3
)

var errSSEEventTooLarge = errors.New("SSE event exceeds size limit")

var errEventResync = errors.New("event resync required")

type Agent struct {
	cfg          Config
	log          *log.Logger
	syncOverride func(context.Context) error
	cursorWriter func(string) error
	streamFn     func(context.Context, *string) error
	backoffHook  func(time.Duration)
}

func (a Agent) syncOnce(ctx context.Context) error {
	if a.syncOverride != nil {
		return a.syncOverride(ctx)
	}
	return a.Sync(ctx)
}

func (a Agent) Run(ctx context.Context) error {
	backoff := time.Second
	lastID := a.readLastEventID()
	for ctx.Err() == nil {
		if err := a.syncOnce(ctx); err != nil {
			a.log.Printf("reconnect sync failed: %v", err)
		} else {
			stream := a.streamFn
			if stream == nil {
				stream = a.stream
			}
			startedAt := time.Now()
			err := stream(ctx, &lastID)
			if ctx.Err() != nil {
				break
			}
			a.log.Printf("event stream ended: %v", err)
			if time.Since(startedAt) >= 10*time.Second {
				backoff = time.Second
			}
		}
		if a.backoffHook != nil {
			a.backoffHook(backoff)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

func (a Agent) stream(ctx context.Context, lastID *string) error {
	cursorless := lastID == nil || *lastID == ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.APIURL, "/")+"/api/admin/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.token())
	if lastID != nil && *lastID != "" {
		req.Header.Set("Last-Event-ID", *lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("events returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	a.log.Printf("connected to Reach event stream")
	reconciledAtBoundary := false
	return parseSSE(ctx, resp.Body, func(ev Event) error {
		syncRequired := false
		switch ev.Type {
		case "hello":
			if cursorless && !reconciledAtBoundary {
				syncRequired = true
				reconciledAtBoundary = true
			}
		case "machine.resync_required":
			a.log.Printf("server requested event resync; resetting cursor before reconnect")
			if lastID != nil {
				if err := a.persistLastEventID(""); err != nil {
					a.log.Printf("could not persist cleared event cursor; continuing safely: %v", err)
				}
				*lastID = ""
			}
			return errEventResync
		case "ssh_config.changed", "machine.created", "machine.desired_changed", "machine.observed_changed", "machine.health_changed", "machine.online", "machine.degraded", "machine.offline", "machine.gone", "machine.disabled", "machine.enabled", "machine.retiring", "machine.retired":
			a.log.Printf("event %s id=%s; syncing SSH config", ev.Type, ev.ID)
			syncRequired = true
		default:
			a.log.Printf("event %s id=%s", ev.Type, ev.ID)
		}
		if syncRequired {
			if err := a.syncWithRetry(ctx, ev.Type); err != nil {
				return err
			}
		}
		if lastID != nil && ev.ID != "" && ev.ID != "0" {
			if err := a.persistLastEventID(ev.ID); err != nil {
				a.log.Printf("could not persist event cursor %s; continuing safely: %v", ev.ID, err)
			}
			*lastID = ev.ID
		}
		return nil
	})
}

func (a Agent) syncWithRetry(ctx context.Context, eventType string) error {
	var err error
	for attempt := 0; attempt < eventSyncAttempts; attempt++ {
		if err = a.syncOnce(ctx); err == nil {
			return nil
		}
		if attempt == eventSyncAttempts-1 {
			break
		}
		delay := time.Duration(1<<attempt) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("sync for event %s after %d attempts: %w", eventType, eventSyncAttempts, err)
}

func parseSSE(ctx context.Context, r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
	var ev Event
	var data []string
	eventBytes := 0
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Text()
		if len(line) > maxSSEEventBytes-eventBytes-1 {
			return errSSEEventTooLarge
		}
		eventBytes += len(line) + 1
		if line == "" {
			if ev.Type != "" || len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				if ev.Type == "" {
					ev.Type = "message"
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = Event{}
			data = nil
			eventBytes = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "id":
			ev.ID = v
		case "event":
			ev.Type = v
		case "data":
			data = append(data, v)
		}
	}
	return sc.Err()
}

func (a Agent) Sync(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.APIURL, "/")+"/api/admin/ssh-config", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body, maxSSHConfigBytes)
	if err != nil {
		return fmt.Errorf("read ssh-config response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ssh-config returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := writeAtomic(a.cfg.OutFile, body, 0o600); err != nil {
		return err
	}
	if a.cfg.InstallInclude {
		if err := ensureInclude(a.cfg.SSHConfigFile, a.cfg.OutFile); err != nil {
			return err
		}
	}
	a.log.Printf("synced %s", a.cfg.OutFile)
	return nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func (a *Agent) ensureServiceToken(ctx context.Context) {
	tok := a.token()
	if tok == "" || strings.HasPrefix(tok, "sat_") || a.cfg.TokenFile == "" {
		return
	}
	host, _ := os.Hostname()
	label := "reach-mac-agent"
	if host != "" {
		label += ":" + host
	}
	if len(label) > 64 {
		label = label[:64]
	}
	body, _ := json.Marshal(map[string]string{"label": label, "role": "mac-agent"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.APIURL, "/")+"/api/admin/service-tokens", bytes.NewReader(body))
	if err != nil {
		a.log.Printf("could not prepare service token request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.log.Printf("could not request service token: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		a.log.Printf("service token exchange skipped: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || out.Token == "" {
		a.log.Printf("service token exchange returned invalid response: %v", err)
		return
	}
	if err := writeAtomic(a.cfg.TokenFile, []byte(out.Token+"\n"), 0o600); err != nil {
		a.log.Printf("could not persist service token: %v", err)
		return
	}
	a.cfg.Token = ""
	a.log.Printf("exchanged expiring admin JWT for non-expiring mac-agent service token")
}

func (a Agent) token() string {
	if a.cfg.TokenFile != "" {
		if b, err := os.ReadFile(expandHome(a.cfg.TokenFile)); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if a.cfg.Token != "" {
		return a.cfg.Token
	}
	return ""
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		APIURL:         envDefault("REACH_API_URL", "https://tunnels.your-domain.example"),
		TokenFile:      filepath.Join(home, ".config/reach/admin-token"),
		OutFile:        filepath.Join(home, ".ssh/reach-tunnels.conf"),
		SSHConfigFile:  filepath.Join(home, ".ssh/config"),
		InstallInclude: true,
	}
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config/reach/mac-agent.yaml")
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	path = expandHome(path)
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
	cfg.TokenFile = expandHome(cfg.TokenFile)
	cfg.OutFile = expandHome(cfg.OutFile)
	cfg.SSHConfigFile = expandHome(cfg.SSHConfigFile)
	if cfg.APIURL == "" {
		cfg.APIURL = envDefault("REACH_API_URL", "https://tunnels.your-domain.example")
	}
	if cfg.OutFile == "" || cfg.SSHConfigFile == "" {
		return cfg, fmt.Errorf("out_file and ssh_config_file are required")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		return cfg, fmt.Errorf("token or token_file is required")
	}
	return cfg, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	path = expandHome(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := durableSync(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (a Agent) stateDir() string {
	return filepath.Join(filepath.Dir(expandHome(a.cfg.OutFile)), ".reach-state")
}

func (a Agent) readLastEventID() string {
	b, err := os.ReadFile(filepath.Join(a.stateDir(), "last-event-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a Agent) persistLastEventID(id string) error {
	if a.cursorWriter != nil {
		return a.cursorWriter(id)
	}
	return writeAtomic(filepath.Join(a.stateDir(), "last-event-id"), []byte(id+"\n"), 0o600)
}

func ensureInclude(configPath, includePath string) error {
	configPath = expandHome(configPath)
	includePath = expandHome(includePath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	line := "Include " + includePath
	b, _ := os.ReadFile(configPath)
	var out bytes.Buffer
	found := false
	for _, existing := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(existing)
		if strings.HasPrefix(trimmed, "Include ") {
			inc := strings.TrimSpace(strings.TrimPrefix(trimmed, "Include "))
			if expandHome(inc) == includePath || inc == "~/.ssh/reach-tunnels.conf" {
				if !found {
					out.WriteString(line + "\n")
					found = true
				}
				continue
			}
		}
		out.WriteString(existing + "\n")
	}
	if found {
		return writeAtomic(configPath, bytes.TrimRight(out.Bytes(), "\n"), 0o600)
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString("\n" + line + "\n")
	return writeAtomic(configPath, out.Bytes(), 0o600)
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
