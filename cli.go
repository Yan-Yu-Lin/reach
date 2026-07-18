package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"reach/internal/wscarrier"
)

func runCLI(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return serveCommand(ctx, args)
	}
	cmd := args[0]
	switch cmd {
	case "serve":
		return serveCommand(ctx, args[1:])
	case "create":
		return cliCreate(ctx, args[1:])
	case "list":
		return cliList(ctx, args[1:])
	case "disable":
		return cliDisable(ctx, args[1:])
	case "remove":
		return cliRemove(ctx, args[1:])
	case "ssh-config":
		return cliSSHConfig(ctx, args[1:])
	case "health":
		return cliHealth(ctx, args[1:])
	case "db-check":
		return cliDBCheck(ctx, args[1:])
	case "hash-secret":
		return cliHashSecret(args[1:])
	case "ws-server":
		return cliWSServer(ctx, args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cliWSServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ws-server", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9401", "listen address")
	target := fs.String("target", "127.0.0.1:22", "target TCP address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return wscarrier.Serve(ctx, *listen, *target, nil)
}

func baseFS(name string, args []string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfg := fs.String("config", "/etc/reach/config.yaml", "config path")
	return fs, cfg
}

func initApp(ctx context.Context, configPath string) (Config, *Store, *Provisioner, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return cfg, nil, nil, err
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return cfg, nil, nil, err
	}
	if err := store.Migrate(ctx, cfg); err != nil {
		_ = store.Close()
		return cfg, nil, nil, err
	}
	return cfg, store, NewProvisioner(store, cfg), nil
}

func serveCommand(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("serve", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if cfg.JWTSecret == "" {
		return fmt.Errorf("jwt_secret is required for API server")
	}
	server := NewServer(cfg, store, prov)
	prov.StartHealthLoop(ctx)
	prov.StartMaintenanceLoop(ctx)
	return server.Run(ctx)
}

func cliCreate(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("create", args)
	slug := fs.String("slug", "", "machine slug")
	pubkeyPath := fs.String("pubkey", "", "tunnel public key path")
	expires := fs.String("expires", "", "expiry duration (e.g. 7d, 12h) or RFC3339")
	targetUser := fs.String("target-user", "", "target login user")
	display := fs.String("display-name", "", "display name")
	noProvision := fs.Bool("no-provision", false, "skip system user/sshd writes (dev only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" || *pubkeyPath == "" {
		return fmt.Errorf("--slug and --pubkey are required")
	}
	cfg, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if *noProvision {
		cfg.ProvisioningEnabled = false
		prov.cfg = cfg
	}
	b, err := os.ReadFile(*pubkeyPath)
	if err != nil {
		return err
	}
	exp, err := parseExpiry(*expires, cfg.DefaultExpiry)
	if err != nil {
		return err
	}
	owner, err := firstUserID(ctx, store)
	if err != nil {
		return err
	}
	res, err := prov.CreateMachine(ctx, CreateMachineInput{Slug: *slug, DisplayName: *display, TargetUser: *targetUser, Pubkey: strings.TrimSpace(string(b)), OwnerUserID: owner, ExpiresAt: exp})
	if err != nil {
		return err
	}
	fmt.Printf("created %s port=%d unix_user=%s machine_id=%s\n", res.Machine.Slug, res.Tunnel.RemotePort, res.Tunnel.UnixUser, res.Machine.ID)
	return nil
}

func cliList(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("list", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ms, err := prov.ListMachines(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		for _, t := range m.Tunnels {
			fmt.Printf("%s\t%s\t%s\t%s\t%d\t%s\n", m.Machine.Slug, m.Machine.ID, m.Machine.Status, t.UnixUser, t.RemotePort, t.Status)
		}
	}
	return nil
}

func cliDisable(ctx context.Context, args []string) error {
	return cliMachineAction(ctx, "disable", args)
}
func cliRemove(ctx context.Context, args []string) error {
	return cliMachineAction(ctx, "remove", args)
}

func cliMachineAction(ctx context.Context, action string, args []string) error {
	fs, cfgPath := baseFS(action, args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: %s [--config path] <slug-or-id>", action)
	}
	_, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if action == "disable" {
		err = prov.DisableMachine(ctx, fs.Arg(0), "cli")
	} else {
		err = prov.RemoveMachine(ctx, fs.Arg(0), "cli")
	}
	if err != nil {
		return err
	}
	fmt.Println(action + "d " + fs.Arg(0))
	return nil
}

func cliSSHConfig(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("ssh-config", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	out, err := NewServer(cfg, store, prov).GenerateSSHConfig(ctx)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cliHealth(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("health", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, store, prov, err := initApp(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer store.Close()
	rep, err := prov.RunHealthCheck(ctx)
	if err != nil {
		return err
	}
	for _, r := range rep {
		fmt.Printf("%s port=%v ok=%v %s\n", r["machine_id"], r["port"], r["ok"], r["detail"])
	}
	return nil
}

func cliDBCheck(ctx context.Context, args []string) error {
	fs, cfgPath := baseFS("db-check", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: reachd db-check [--config path]")
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.CheckReady(checkCtx); err != nil {
		return err
	}
	fmt.Println("database ready")
	return nil
}

func cliHashSecret(args []string) error {
	fs := flag.NewFlagSet("hash-secret", flag.ContinueOnError)
	stdin := fs.Bool("stdin", true, "read secret from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: printf 'secret' | reachd hash-secret")
	}
	if !*stdin {
		return fmt.Errorf("hash-secret only supports stdin to avoid leaking secrets in process listings")
	}
	b, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return err
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if secret == "" {
		return fmt.Errorf("empty secret on stdin")
	}
	h, err := HashSecret(secret)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func parseExpiry(s string, def time.Duration) (string, error) {
	if s == "" {
		if def == 0 {
			return "", nil
		}
		return time.Now().UTC().Add(def).Format(time.RFC3339), nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return "", err
		}
		return time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(d).Format(time.RFC3339), nil
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return "", err
	}
	return s, nil
}

func firstUserID(ctx context.Context, store *Store) (string, error) {
	var id string
	err := store.db.QueryRowContext(ctx, `SELECT id FROM users ORDER BY created_at LIMIT 1`).Scan(&id)
	return id, err
}

func usage() {
	fmt.Println(`Reach reverse SSH tunnel manager

Usage:
  reachd serve [--config /etc/reach/config.yaml]
  reachctl create --slug testbox --pubkey /tmp/key.pub [--expires 7d]
  reachctl list
  reachctl disable <slug-or-id>
  reachctl remove <slug-or-id>
  reachctl ssh-config
  reachctl health
  reachd db-check [--config /etc/reach/config.yaml]
  printf 'secret' | reachd hash-secret`)
}
