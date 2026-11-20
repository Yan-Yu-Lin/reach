package main

import (
	"net/http"
	"net/url"
	"time"

	"reach/internal/safehttp"
)

// Control-plane requests never follow redirects (daybreak round 3 P2-B).
// Downloads use a safehttp.Downloader per call: same origin only, every hop
// rechecked, no private addresses (daybreak round 4 P2-03).
var apiClient = safehttp.API(60 * time.Second)

// downloadClientFor builds the client for one download; tests replace it.
var downloadClientFor = func(d safehttp.Downloader) *http.Client { return d.Client() }

// sameOrigin compares scheme://host[:port] of two URLs.
func sameOrigin(a, b string) bool {
	oa, errA := safehttp.OriginOf(a)
	ob, errB := safehttp.OriginOf(b)
	return errA == nil && errB == nil && oa == ob
}

// redactURL keeps scheme, host and path; drops query and userinfo.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
