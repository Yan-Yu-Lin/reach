//go:build !windows

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	osuser "os/user"
	"strconv"
	"strings"

	"syscall"

	"github.com/creack/pty"
	gliderssh "github.com/gliderlabs/ssh"
)

func handleShellSession(s gliderssh.Session, shellOverride, runAs string) {
	shell := resolveShell(shellOverride)
	env := minimalShellEnv(shell)
	var cred *runAsCred
	if runAs != "" {
		c, err := lookupRunAs(runAs)
		if err != nil {
			log.Printf("internal-sshd: cannot run session as %q: %v", runAs, err)
			_, _ = io.WriteString(s.Stderr(), "internal-sshd: session user unavailable\r\n")
			_ = s.Exit(1)
			return
		}
		cred = c
		if c.shell != "" && shellOverride == "" {
			shell = c.shell
		}
		env = []string{"SHELL=" + shell, "HOME=" + c.home, "USER=" + c.name, "LOGNAME=" + c.name, "PATH=/usr/local/bin:/usr/bin:/bin"}
	}
	cmd := exec.CommandContext(s.Context(), shell)
	if raw := strings.TrimSpace(s.RawCommand()); raw != "" {
		cmd = exec.CommandContext(s.Context(), shell, "-c", raw)
	}
	cmd.Env = append(env, filterClientEnv(s.Environ())...)
	if cred != nil {
		// Drop to the target user: a root agent must not give operators a
		// root shell (daybreak P1-5 follow-up).
		cmd.SysProcAttr = runAsSysProcAttr(cred)
		cmd.Dir = cred.home
	}

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

type runAsCred struct {
	name, home, shell string
	uid, gid          uint32
	groups            []uint32
}

func lookupRunAs(name string) (*runAsCred, error) {
	u, err := osuser.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		return nil, fmt.Errorf("refusing to run sessions as root via --run-as")
	}
	c := &runAsCred{name: u.Username, home: u.HomeDir, uid: uint32(uid), gid: uint32(gid)}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				c.groups = append(c.groups, uint32(n))
			}
		}
	}
	c.shell = loginShell(u.Username)
	return c, nil
}

// loginShell reads the account's shell from /etc/passwd (best effort).
func loginShell(name string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[0] == name {
			sh := strings.TrimSpace(f[6])
			if st, err := os.Stat(sh); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return sh
			}
		}
	}
	return ""
}

// filterClientEnv drops client-supplied variables that would let a session
// change who it runs as or what the loader does.
func filterClientEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME", k == "USER", k == "LOGNAME", k == "SHELL", k == "PATH",
			strings.HasPrefix(k, "LD_"), strings.HasPrefix(k, "DYLD_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

func runAsSysProcAttr(c *runAsCred) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: c.uid, Gid: c.gid, Groups: c.groups}}
}
