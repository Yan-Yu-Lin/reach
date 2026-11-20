package wscarrier

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T) (string, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 100)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String(), func() int { return len(accepted) }
}

func rawUpgrade(t *testing.T, addr, path string) (net.Conn, int) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", path)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, resp.StatusCode
}

// Daybreak P2-4: the carrier accepted any path and dialled sshd first.
func TestCarrierEnforcesExactPathBeforeDial(t *testing.T) {
	target, accepted := echoServer(t)
	srv := httptest.NewServer(NewHandler(target, log.New(io.Discard, "", 0), ServerOptions{PathPrefix: "/ws/tunnel/secret"}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, p := range []string{"/", "/ws/tunnel/guess/", "/ws/tunnel/secretx/", "/other", "/ws/tunnel/secret/attacker-chosen-suffix"} {
		c, code := rawUpgrade(t, addr, p)
		c.Close()
		if code != http.StatusNotFound {
			t.Fatalf("path %s = %d, want 404", p, code)
		}
	}
	if accepted() != 0 {
		t.Fatal("carrier dialled the backend for a wrong path")
	}
	c, code := rawUpgrade(t, addr, "/ws/tunnel/secret/")
	defer c.Close()
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("correct path = %d", code)
	}
}

func TestCarrierLimitsConnectionsPerIP(t *testing.T) {
	target, _ := echoServer(t)
	srv := httptest.NewServer(NewHandler(target, log.New(io.Discard, "", 0), ServerOptions{MaxConnsPerIP: 2, MaxConns: 10}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		c, code := rawUpgrade(t, addr, "/")
		open = append(open, c)
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("conn %d = %d", i, code)
		}
	}
	c, code := rawUpgrade(t, addr, "/")
	open = append(open, c)
	if code != http.StatusTooManyRequests {
		t.Fatalf("third conn = %d, want 429", code)
	}
}

// Daybreak P2-4 PoC: declare a 16 MiB frame and trickle half of it. Memory
// must stay bounded by the streaming chunk, not the declared length.
func TestCarrierStreamsLargeFramesWithoutBuffering(t *testing.T) {
	target, _ := echoServer(t)
	srv := httptest.NewServer(NewHandler(target, log.New(io.Discard, "", 0), ServerOptions{}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var conns []net.Conn
	for i := 0; i < 6; i++ {
		c, code := rawUpgrade(t, addr, "/")
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("upgrade = %d", code)
		}
		conns = append(conns, c)
		hdr := []byte{0x82, 0x80 | 127}
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], 16*1024*1024)
		hdr = append(hdr, l[:]...)
		hdr = append(hdr, 0, 0, 0, 0)
		_, _ = c.Write(hdr)
		_, _ = c.Write(make([]byte, 256*1024))
	}
	time.Sleep(300 * time.Millisecond)
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	for _, c := range conns {
		c.Close()
	}
	if grow := int64(after.HeapInuse) - int64(before.HeapInuse); grow > 16<<20 {
		t.Fatalf("heap grew %d MiB for 6 slow 16 MiB frames", grow>>20)
	}
}

func TestCarrierClosesIdleConnections(t *testing.T) {
	target, _ := echoServer(t)
	srv := httptest.NewServer(NewHandler(target, log.New(io.Discard, "", 0), ServerOptions{IdleTimeout: 200 * time.Millisecond}))
	defer srv.Close()
	c, code := rawUpgrade(t, strings.TrimPrefix(srv.URL, "http://"), "/")
	defer c.Close()
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", code)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			return // closed by the carrier
		}
	}
}

// The exact path the agent dials (carrierURL) must be accepted by a
// carrier configured with the same prefix, for both config spellings.
func TestAgentPathMatchesExactCarrierPath(t *testing.T) {
	target, _ := echoServer(t)
	for _, prefix := range []string{"ws/tunnel/secret", "/ws/tunnel/secret/"} {
		u, err := carrierURL("wss://tunnels.example.com", prefix)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(NewHandler(target, log.New(io.Discard, "", 0), ServerOptions{PathPrefix: prefix}))
		c, code := rawUpgrade(t, strings.TrimPrefix(srv.URL, "http://"), u.Path)
		c.Close()
		srv.Close()
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("prefix %q: agent path %q got %d", prefix, u.Path, code)
		}
	}
}
