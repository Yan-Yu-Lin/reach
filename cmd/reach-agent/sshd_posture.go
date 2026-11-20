package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Reach needs SSH only on loopback. Daybreak P1-5: system installs used to
// install and enable the OS sshd, which normally listens on every interface
// and so added a LAN/public port 22 that bypasses Reach's disable/retire.
// Installs no longer enable a host-wide sshd; when no sshd answers on
// loopback, the agent's own internal-sshd (loopback only) is used. If an
// existing sshd also listens beyond loopback, the installer warns.

// sshdWideListeners returns non-loopback addresses on which something
// listens on port 22, using ss (Linux) or lsof (macOS). Best effort: nil on
// unknown platforms or tool failure.
func sshdWideListeners(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		out, err = exec.CommandContext(ctx, "ss", "-Htln", "sport", "=", ":22").Output()
	case "darwin":
		out, err = exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:22", "-sTCP:LISTEN").Output()
	default:
		return nil
	}
	if err != nil {
		return nil
	}
	return parseWideListeners(out)
}

// parseWideListeners extracts listen addresses on :22 that are not loopback
// from ss -Htln or lsof output.
func parseWideListeners(out []byte) []string {
	var wide []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		for _, f := range strings.Fields(sc.Text()) {
			if !strings.HasSuffix(f, ":22") {
				continue
			}
			host := strings.TrimSuffix(f, ":22")
			host = strings.Trim(host, "[]")
			switch {
			case host == "127.0.0.1", host == "::1", host == "localhost", strings.HasPrefix(host, "127."):
				continue
			}
			if !seen[f] {
				seen[f] = true
				wide = append(wide, f)
			}
		}
	}
	return wide
}

func warnIfSSHDWide(ctx context.Context) {
	if wide := sshdWideListeners(ctx); len(wide) > 0 {
		printfWarn("[reach warning] the existing sshd also listens on %s, not only loopback. Reach only needs 127.0.0.1:22; restrict ListenAddress or firewall port 22 if this machine should not accept SSH from the network.\n", strings.Join(wide, ", "))
	}
}

// printfWarn is a seam for tests.
var printfWarn = func(format string, a ...any) { fmt.Printf(format, a...) }
