package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
)

// pinnedHostKeyAlgorithms returns the host key algorithms that are actually
// pinned for targetHostPort in knownHostsPath. The embedded SSH client uses
// this list as HostKeyAlgorithms so the server cannot negotiate an unpinned
// host key type and then fail later in known_hosts validation.
func pinnedHostKeyAlgorithms(knownHostsPath, targetHostPort string) ([]string, error) {
	f, err := os.Open(knownHostsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return pinnedHostKeyAlgorithmsFromKnownHosts(f, knownHostsPath, targetHostPort)
}

func pinnedHostKeyAlgorithmsFromKnownHosts(r interface{ Read([]byte) (int, error) }, filename, targetHostPort string) ([]string, error) {
	targetHost, targetPort, err := net.SplitHostPort(targetHostPort)
	if err != nil {
		return nil, fmt.Errorf("target host must include port: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		marker, hostsField, keyFields, ok := splitKnownHostsLine(line)
		if !ok {
			return nil, fmt.Errorf("%s:%d: invalid known_hosts line", filename, lineNum)
		}
		if marker == "@revoked" || marker == "@cert-authority" {
			continue
		}
		matched, err := knownHostsFieldMatches(hostsField, targetHost, targetPort)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", filename, lineNum, err)
		}
		if !matched {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(keyFields)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: parse host key: %w", filename, lineNum, err)
		}
		alg := key.Type()
		if !seen[alg] {
			seen[alg] = true
			out = append(out, alg)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pinned host keys for %s in %s", targetHostPort, filename)
	}
	return out, nil
}

func splitKnownHostsLine(line []byte) (marker, hostsField string, keyFields []byte, ok bool) {
	fields := bytes.Fields(line)
	if len(fields) < 3 {
		return "", "", nil, false
	}
	idx := 0
	if bytes.HasPrefix(fields[0], []byte("@")) {
		marker = string(fields[0])
		idx = 1
	}
	if len(fields) < idx+3 {
		return "", "", nil, false
	}
	hostsField = string(fields[idx])
	keyFields = bytes.Join(fields[idx+1:], []byte(" "))
	return marker, hostsField, keyFields, true
}

func knownHostsFieldMatches(hostsField, targetHost, targetPort string) (bool, error) {
	matched := false
	for _, raw := range strings.Split(hostsField, ",") {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		negate := strings.HasPrefix(pattern, "!")
		if negate {
			pattern = strings.TrimPrefix(pattern, "!")
		}
		if strings.HasPrefix(pattern, "|") {
			// Hashed known_hosts entries are still enforced by knownhosts.New, but
			// we cannot safely extract host-specific algorithms from them here.
			continue
		}
		ok, err := knownHostsPatternMatches(pattern, targetHost, targetPort)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if negate {
			return false, nil
		}
		matched = true
	}
	return matched, nil
}

func knownHostsPatternMatches(pattern, targetHost, targetPort string) (bool, error) {
	patternHost, patternPort, err := splitKnownHostsPattern(pattern)
	if err != nil {
		return false, err
	}
	if patternPort != targetPort {
		return false, nil
	}
	if patternHost == targetHost {
		return true, nil
	}
	ok, err := path.Match(patternHost, targetHost)
	if err != nil {
		return false, fmt.Errorf("invalid host pattern %q: %w", pattern, err)
	}
	return ok, nil
}

func splitKnownHostsPattern(pattern string) (host, port string, err error) {
	if strings.HasPrefix(pattern, "[") {
		host, port, err = net.SplitHostPort(pattern)
		return host, port, err
	}
	if h, p, splitErr := net.SplitHostPort(pattern); splitErr == nil {
		return h, p, nil
	}
	return pattern, "22", nil
}

func intersectAlgorithms(requested, pinned []string) []string {
	pinnedSet := make(map[string]bool, len(pinned))
	for _, alg := range pinned {
		pinnedSet[alg] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, alg := range requested {
		if pinnedSet[alg] && !seen[alg] {
			seen[alg] = true
			out = append(out, alg)
		}
	}
	return out
}
