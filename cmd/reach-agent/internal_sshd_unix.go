//go:build !windows

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
	gliderssh "github.com/gliderlabs/ssh"
)

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

func setPTYWindow(f *os.File, w gliderssh.Window) error {
	if w.Width <= 0 || w.Height <= 0 {
		return nil
	}
	return pty.Setsize(f, &pty.Winsize{Rows: uint16(w.Height), Cols: uint16(w.Width)})
}
