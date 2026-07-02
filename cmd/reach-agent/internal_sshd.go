package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	gliderssh "github.com/gliderlabs/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

type internalSSHDOptions struct {
	Port        int
	AuthFile    string
	HostKeyPath string
	Shell       string
}

var internalSSHDSupportedKeyTypes = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ssh-rsa"}

func internalSSHDCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("internal-sshd", flag.ContinueOnError)
	port := fs.Int("port", 0, "loopback TCP port to bind")
	authFile := fs.String("auth-file", "", "authorized_keys path")
	hostKey := fs.String("host-key", "", "server host private key path")
	shell := fs.String("shell", "", "login shell to exec")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected internal-sshd arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *port < 1024 || *port > 65535 {
		return fmt.Errorf("--port must be in range 1024-65535")
	}
	if strings.TrimSpace(*authFile) == "" {
		return fmt.Errorf("--auth-file is required")
	}
	if strings.TrimSpace(*hostKey) == "" {
		return fmt.Errorf("--host-key is required")
	}
	for _, path := range []string{*authFile, *hostKey} {
		if st, err := os.Stat(path); err != nil {
			return err
		} else if st.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
	}
	return runInternalSSHD(ctx, internalSSHDOptions{Port: *port, AuthFile: *authFile, HostKeyPath: *hostKey, Shell: *shell})
}

func runInternalSSHD(ctx context.Context, opt internalSSHDOptions) error {
	signer, err := loadInternalHostKey(opt.HostKeyPath)
	if err != nil {
		return err
	}
	authorized, err := loadAuthorizedKeys(opt.AuthFile)
	if err != nil {
		return err
	}
	if len(authorized) == 0 {
		return fmt.Errorf("no authorized public keys in %s", opt.AuthFile)
	}

	srv := &gliderssh.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", opt.Port),
		PublicKeyHandler: func(_ gliderssh.Context, key gliderssh.PublicKey) bool {
			// Reload on each auth attempt so later sync_keys updates take effect
			// without needing a restart. The file is tiny in Reach's single-user mode.
			authorized, err := loadAuthorizedKeys(opt.AuthFile)
			if err != nil {
				log.Printf("internal-sshd load authorized_keys failed: %v", err)
				return false
			}
			return publicKeyAuthorized(key, authorized)
		},
		Handler: func(s gliderssh.Session) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("internal-sshd session panic: %v", r)
					_ = s.Exit(1)
				}
			}()
			handleShellSession(s, opt.Shell)
		},
	}
	srv.AddHostKey(signer)

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("internal-sshd shutdown goroutine panic: %v", r)
			}
		}()
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("internal-sshd listening on %s", srv.Addr)
	err = srv.Serve(ln)
	if errors.Is(err, gliderssh.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func loadInternalHostKey(path string) (gliderssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := cryptossh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse internal sshd host key %s: %w", path, err)
	}
	return signer, nil
}

func loadAuthorizedKeys(path string) ([]gliderssh.PublicKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var keys []gliderssh.PublicKey
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := gliderssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		keys = append(keys, key)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func publicKeyAuthorized(key gliderssh.PublicKey, authorized []gliderssh.PublicKey) bool {
	for _, ak := range authorized {
		if gliderssh.KeysEqual(key, ak) {
			return true
		}
	}
	return false
}

func handleShellSession(s gliderssh.Session, shellOverride string) {
	shell := resolveShell(shellOverride)
	cmd := exec.CommandContext(s.Context(), shell)
	if raw := strings.TrimSpace(s.RawCommand()); raw != "" {
		cmd = exec.CommandContext(s.Context(), shell, "-c", raw)
	}
	cmd.Env = append(minimalShellEnv(shell), s.Environ()...)

	ptyReq, winCh, isPty := s.Pty()
	if isPty {
		if ptyReq.Term != "" {
			cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		}
		f, err := pty.Start(cmd)
		if err != nil {
			log.Printf("internal-sshd pty start failed: %v", err)
			_ = s.Exit(1)
			return
		}
		defer f.Close()
		_ = setPTYWindow(f, ptyReq.Window)
		go func() {
			defer recoverLog("internal-sshd pty resize goroutine")
			for w := range winCh {
				_ = setPTYWindow(f, w)
			}
		}()
		go func() {
			defer recoverLog("internal-sshd stdin copy goroutine")
			_, _ = io.Copy(f, s)
		}()
		_, _ = io.Copy(s, f)
		_ = cmd.Wait()
		_ = s.Exit(commandExitCode(cmd))
		return
	}

	cmd.Stdin = s
	cmd.Stdout = s
	cmd.Stderr = s.Stderr()
	_ = cmd.Run()
	_ = s.Exit(commandExitCode(cmd))
}

func recoverLog(context string) {
	if r := recover(); r != nil {
		log.Printf("%s panic: %v", context, r)
	}
}

func setPTYWindow(f *os.File, w gliderssh.Window) error {
	if w.Width <= 0 || w.Height <= 0 {
		return nil
	}
	return pty.Setsize(f, &pty.Winsize{Rows: uint16(w.Height), Cols: uint16(w.Width)})
}

func resolveShell(shellOverride string) string {
	for _, cand := range []string{shellOverride, os.Getenv("SHELL"), "/bin/bash", "/bin/sh"} {
		cand = strings.TrimSpace(cand)
		if cand == "" {
			continue
		}
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return cand
		}
	}
	return "/bin/sh"
}

func minimalShellEnv(shell string) []string {
	env := []string{"SHELL=" + shell}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		env = append(env, "HOME="+home)
	}
	if user := currentUsername(); user != "" {
		env = append(env, "USER="+user, "LOGNAME="+user)
	}
	if path := os.Getenv("PATH"); strings.TrimSpace(path) != "" {
		env = append(env, "PATH="+path)
	} else {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

func commandExitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 1
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	return 1
}

func ensureInternalHostKey(path string) error {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "reach-internal-sshd")
	if err != nil {
		return err
	}
	data := pem.EncodeToMemory(block)
	if len(data) == 0 {
		return fmt.Errorf("encode internal sshd host key failed")
	}
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}
