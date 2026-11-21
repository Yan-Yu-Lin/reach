package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIRefusesRedirects(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect followed to %s", r.URL)
	}))
	defer sink.Close()
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c := API(0)
	c.Transport = src.Client().Transport
	req, _ := http.NewRequest(http.MethodPost, src.URL, strings.NewReader("secret"))
	req.Header.Set("Authorization", "Bearer secret")
	_, err := c.Do(req)
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadRedirectPolicy(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	defer plain.Close()
	// https -> http (downgrade) refused.
	tlsSrc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tlsSrc.Close()
	c := Download(0)
	c.Transport = tlsSrc.Client().Transport
	if _, err := c.Get(tlsSrc.URL); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("https->http redirect: err = %v", err)
	}
	// https -> https followed.
	tlsDst := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer tlsDst.Close()
	tlsHop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlsDst.URL+"/y", http.StatusFound)
	}))
	defer tlsHop.Close()
	c = Download(0)
	c.Transport = tlsHop.Client().Transport
	resp, err := c.Get(tlsHop.URL)
	if err != nil {
		t.Fatalf("https->https redirect refused: %v", err)
	}
	resp.Body.Close()
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{"https://x.example/a", "http://127.0.0.1:9/a", "http://localhost/a", "http://[::1]:1/"} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://x.example/a", "http://localhost.evil.example/", "https://u:p@x.example/", "ftp://x/", "file:///etc/passwd"} {
		if err := ValidateURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Daybreak round 4 P2-03: a download may not leave its origin, even by an
// HTTPS redirect, and never reaches a private address.
func TestDownloaderStaysOnOrigin(t *testing.T) {
	marker := "SECRET-INTERNAL-MARKER"
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, marker)
	}))
	defer other.Close()
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer src.Close()
	origin, _ := OriginOf(src.URL)
	d := Downloader{Origins: []string{origin}}
	c := d.Client()
	c.Transport.(*http.Transport).TLSClientConfig = src.Client().Transport.(*http.Transport).TLSClientConfig
	resp, err := c.Get(src.URL)
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("cross-origin redirect followed: %s", b)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatal("other origin's body leaked into the error")
	}
}

func TestDownloaderRefusesPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "fe80::1", "0.0.0.0"} {
		if !disallowedIP(net.ParseIP(ip)) {
			t.Errorf("%s allowed", ip)
		}
	}
	if disallowedIP(net.ParseIP("1.1.1.1")) {
		t.Error("public address refused")
	}
	d := Downloader{Origins: []string{"https://metadata.internal"}}
	c := d.Client()
	tr := c.Transport.(*http.Transport)
	if _, err := tr.DialContext(context.Background(), "tcp", "10.0.0.1:443"); err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("dial to a private IP: %v", err)
	}
}

func TestDownloaderCheck(t *testing.T) {
	d := Downloader{Origins: []string{"https://hub.example"}}
	for _, ok := range []string{"https://hub.example/downloads/x", "https://HUB.example/y"} {
		if err := d.Check(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://evil.example/x", "http://hub.example/x", "https://u@hub.example/x", "https://hub.example:8443/x"} {
		if err := d.Check(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Daybreak round 5 N5 / astra R4-09: only plain global unicast is allowed.
func TestSpecialPurposeAddressesRefused(t *testing.T) {
	for _, ip := range []string{
		"100.64.0.1", "100.100.100.200", "100.127.255.254", // RFC 6598 (tailnets)
		"64:ff9b::a00:1", "2002:a00:1::1", "::ffff:10.0.0.1", // NAT64, 6to4, mapped
		"198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", // benchmark, docs
		"240.0.0.1", "0.1.2.3", "fc00::1", "ff02::1", "192.0.0.8",
	} {
		if !disallowedIP(net.ParseIP(ip)) {
			t.Errorf("%s allowed", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "125.229.144.147", "2606:4700:4700::1111"} {
		if disallowedIP(net.ParseIP(ip)) {
			t.Errorf("%s refused", ip)
		}
	}
}
