// Package safehttp holds the HTTP clients Reach agents use instead of
// http.DefaultClient (daybreak round 3 P2-B).
//
//   - API: control-plane calls (register, poll, provision, heartbeat,
//     uninstall, admin API). Redirects are refused: a 307/308 keeps the
//     method, body and (same-host) Authorization header, so following one
//     can deliver secrets to an endpoint the operator never configured.
//   - Download: release artifacts. Redirects are followed (CDNs), but each
//     hop is revalidated: HTTPS never downgrades to HTTP, no userinfo, and
//     at most 5 hops.
//
// Both have a total timeout; callers still bound response bodies.
package safehttp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRedirect is returned for any redirect on an API client.
var ErrRedirect = errors.New("redirect refused for Reach API request")

// NoRedirect is a CheckRedirect that refuses all redirects.
func NoRedirect(req *http.Request, via []*http.Request) error {
	return fmt.Errorf("%w (to %s)", ErrRedirect, req.URL.Redacted())
}

// CheckDownloadRedirect allows at most 5 hops and never from https to a
// non-https URL (plain http only between loopback hosts, for tests and
// local setups).
func CheckDownloadRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL.User != nil {
		return errors.New("redirect URL contains credentials")
	}
	prev := via[len(via)-1].URL
	switch req.URL.Scheme {
	case "https":
		return nil
	case "http":
		if prev.Scheme == "http" && IsLoopbackHost(prev.Hostname()) && IsLoopbackHost(req.URL.Hostname()) {
			return nil
		}
		return fmt.Errorf("redirect from %s to insecure %s refused", prev.Redacted(), req.URL.Redacted())
	}
	return fmt.Errorf("redirect to unsupported scheme %q refused", req.URL.Scheme)
}

// IsLoopbackHost reports localhost or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// API returns a client for control-plane requests. timeout 0 means no
// client-level timeout (streams); use contexts then.
func API(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: NoRedirect}
}

// Download returns a client for artifact downloads.
func Download(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: CheckDownloadRedirect}
}

// ValidateURL checks an initial URL the same way redirects are checked:
// https, or http only to a loopback host; no userinfo.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil {
		return errors.New("URL contains credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("URL must use https (plain http only for loopback): %s", u.Redacted())
}
