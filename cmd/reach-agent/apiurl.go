package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// validateAPIURL accepts https://<host>[:port][/path] for any valid DNS host
// or IP, and plain http only when the host is exactly a loopback name or
// address. Userinfo, query and fragment are rejected. The agent enforces
// this itself instead of trusting setup.sh (daybreak P2-10): registration
// and provisioning secrets must never cross the network in clear text.
func validateAPIURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("API URL must be scheme://host[:port][/path]")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("API URL has no host")
	}
	if p := u.Port(); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("API URL has an invalid port")
		}
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("API URL must use https:// (plain http only for localhost/127.0.0.1/::1), got %q", raw)
	}
	return fmt.Errorf("API URL must use https://, got scheme %q", u.Scheme)
}
