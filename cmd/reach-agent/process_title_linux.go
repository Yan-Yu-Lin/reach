//go:build linux

package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	processTitlePadBytes  = 4096
	processTitleMarkerEnv = "REACH_AGENT_PROCESS_TITLE_PADDED"
	processTitleConfigEnv = "REACH_AGENT_CONFIG"
)

var argvTitle struct {
	mu   sync.Mutex
	base *byte
	cap  int
}

func maybeReexecForProcessTitle(args []string) {
	if os.Getenv(processTitleMarkerEnv) == "1" {
		_ = os.Unsetenv(processTitleMarkerEnv)
		initArgvTitleBuffer(processTitlePadBytes)
		return
	}

	configPath, ok := processTitleReexecConfigPath(args)
	if !ok {
		initArgvTitleBuffer(0)
		return
	}

	exe, err := os.Executable()
	if err != nil || exe == "" {
		if len(os.Args) > 0 && os.Args[0] != "" {
			exe = os.Args[0]
		} else {
			initArgvTitleBuffer(0)
			return
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}

	env := upsertEnv(os.Environ(), processTitleMarkerEnv, "1")
	if configPath != "" && !isDiscoverableAgentConfig(configPath) {
		env = upsertEnv(env, processTitleConfigEnv, configPath)
	}

	argv0 := paddedProcessArgv0(defaultProcessTitle, processTitlePadBytes)
	if err := syscall.Exec(exe, []string{argv0}, env); err != nil {
		log.Printf("reach-agent: process title self-exec skipped: %v", err)
		initArgvTitleBuffer(0)
	}
}

func processTitleReexecConfigPath(args []string) (string, bool) {
	if len(args) == 0 {
		return "", true
	}
	switch args[0] {
	case "daemon":
		return "", len(args) == 1
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(new(strings.Builder))
		configPath := fs.String("config", defaultAgentConfigPath(), "agent config path")
		if err := fs.Parse(args[1:]); err != nil {
			return "", false
		}
		if fs.NArg() != 0 {
			return "", false
		}
		return *configPath, true
	default:
		return "", false
	}
}

func paddedProcessArgv0(title string, size int) string {
	title = cleanProcessTitle(title)
	if title == "" {
		title = defaultProcessTitle
	}
	title = truncateUTF8Bytes(title, size)
	if len(title) >= size {
		return title
	}
	return title + strings.Repeat(" ", size-len(title))
}

func initArgvTitleBuffer(wantCap int) {
	argvTitle.mu.Lock()
	defer argvTitle.mu.Unlock()
	if len(os.Args) == 0 || len(os.Args[0]) == 0 {
		argvTitle.base = nil
		argvTitle.cap = 0
		return
	}
	capBytes := len(os.Args[0])
	if wantCap > 0 && capBytes > wantCap {
		capBytes = wantCap
	}
	argvTitle.base = unsafe.StringData(os.Args[0])
	argvTitle.cap = capBytes
}

func setProcessTitle(title string) {
	title = cleanProcessTitle(title)
	if title == "" {
		title = defaultProcessTitle
	}
	setThreadComm(title)
	writeArgvProcessTitle(title)
}

func setThreadComm(title string) {
	title = truncateUTF8Bytes(title, 15)
	if title == "" {
		title = defaultProcessTitle
	}
	b := append([]byte(title), 0)
	_, _, _ = unix.Syscall(unix.SYS_PRCTL, uintptr(unix.PR_SET_NAME), uintptr(unsafe.Pointer(&b[0])), 0)
}

func writeArgvProcessTitle(title string) {
	argvTitle.mu.Lock()
	defer argvTitle.mu.Unlock()
	if argvTitle.base == nil || argvTitle.cap <= 0 {
		return
	}
	buf := unsafe.Slice(argvTitle.base, argvTitle.cap)
	writeProcessTitleBuffer(buf, title)
}

func writeProcessTitleBuffer(buf []byte, title string) string {
	for i := range buf {
		buf[i] = 0
	}
	if len(buf) == 0 {
		return ""
	}
	title = truncateUTF8Bytes(cleanProcessTitle(title), len(buf))
	copy(buf, title)
	return title
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			next := append([]string(nil), env...)
			next[i] = prefix + value
			return next
		}
	}
	next := append([]string(nil), env...)
	return append(next, prefix+value)
}
