package safehttp

import (
	"errors"
	"io"
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
