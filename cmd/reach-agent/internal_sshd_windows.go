//go:build windows

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	gliderssh "github.com/gliderlabs/ssh"
)

// handleShellSession mirrors Win32-OpenSSH with the default cmd.exe shell:
// an exec request runs as `"cmd.exe" /c "<command>"`, passed verbatim (no
// re-escaping), started in the user's profile directory, with the user's full
// environment. There is no ConPTY support: a pty request runs over plain pipes.
func handleShellSession(s gliderssh.Session, shellOverride, _ string) {
	shell := resolveWindowsShell(shellOverride)
	cmd := exec.Command(shell)
	cmdLine := `"` + shell + `"`
	if raw := s.RawCommand(); strings.TrimSpace(raw) != "" {
		cmdLine += ` /c "` + raw + `"`
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       cmdLine,
		HideWindow:    true,
		CreationFlags: windowsCreateNoWindow,
	}
	cmd.Env = append(os.Environ(), s.Environ()...)
	if home := os.Getenv("USERPROFILE"); home != "" {
		cmd.Dir = home
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = s.Exit(1)
		return
	}
	cmd.Stdout = s
	cmd.Stderr = s.Stderr()
	// A background grandchild that inherited stdout must not keep the session
	// open after the command itself has exited.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		log.Printf("internal-sshd start %q failed: %v", cmdLine, err)
		_, _ = io.WriteString(s.Stderr(), "internal-sshd: "+err.Error()+"\r\n")
		_ = s.Exit(1)
		return
	}
	go func() {
		defer recoverLog("internal-sshd stdin copy goroutine")
		_, _ = io.Copy(stdin, s)
		_ = stdin.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer recoverLog("internal-sshd session watcher")
		select {
		case <-s.Context().Done():
			killWindowsTree(cmd.Process.Pid)
		case <-done:
		}
	}()
	_ = cmd.Wait()
	close(done)
	_ = s.Exit(commandExitCode(cmd))
}

// killWindowsTree stops a session's process and everything it started when the
// client goes away, like Win32-OpenSSH's kill-on-close job object.
func killWindowsTree(pid int) {
	kill := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windowsCreateNoWindow}
	_ = kill.Run()
}

func resolveWindowsShell(shellOverride string) string {
	if s := strings.TrimSpace(shellOverride); s != "" {
		return s
	}
	if c := strings.TrimSpace(os.Getenv("ComSpec")); c != "" {
		return c
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "cmd.exe")
}
