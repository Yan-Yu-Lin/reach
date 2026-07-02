package wscarrier

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Dial returns a framed net.Conn carried over a WebSocket. Client frames are masked per RFC 6455.
func Dial(ctx context.Context, rawURL, pathPrefix string) (net.Conn, error) {
	u, err := carrierURL(rawURL, pathPrefix)
	if err != nil {
		return nil, err
	}
	conn, br, err := dialWebSocket(ctx, u)
	if err != nil {
		return nil, err
	}
	return &wsConn{Conn: conn, br: br, client: true}, nil
}

// Client connects stdin/stdout to a WebSocket. Client frames are masked per RFC 6455.
func Client(ctx context.Context, rawURL, pathPrefix string, stdin io.Reader, stdout io.Writer) error {
	u, err := carrierURL(rawURL, pathPrefix)
	if err != nil {
		return err
	}
	conn, br, err := dialWebSocket(ctx, u)
	if err != nil {
		return err
	}
	defer conn.Close()
	return pump(ctx, br, conn, stdin, stdout, true)
}

type wsConn struct {
	net.Conn
	br      *bufio.Reader
	client  bool
	writeMu sync.Mutex
	readBuf bytes.Buffer
}

func (c *wsConn) Read(p []byte) (int, error) {
	if c.readBuf.Len() > 0 {
		return c.readBuf.Read(p)
	}
	for {
		op, payload, err := readFrame(c.br, !c.client)
		if err != nil {
			return 0, err
		}
		switch op {
		case 1, 2:
			if len(payload) == 0 {
				continue
			}
			c.readBuf.Write(payload)
			return c.readBuf.Read(p)
		case 8:
			return 0, io.EOF
		case 9:
			c.writeMu.Lock()
			_ = writeFrame(c.Conn, 10, payload, c.client)
			c.writeMu.Unlock()
		case 10:
			continue
		}
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeFrame(c.Conn, 2, p, c.client); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	c.writeMu.Lock()
	_ = writeFrame(c.Conn, 8, []byte{}, c.client)
	c.writeMu.Unlock()
	return c.Conn.Close()
}

// Serve accepts WebSocket connections and pipes each one to targetAddr.
func Serve(ctx context.Context, listenAddr, targetAddr string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return
		}
		defer target.Close()
		conn, br, err := upgrade(w, r)
		if err != nil {
			logger.Printf("websocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		logger.Printf("carrier connected remote=%s target=%s path=%s", r.RemoteAddr, targetAddr, redactPath(r.URL.Path))
		if err := pump(r.Context(), br, conn, target, target, false); err != nil && !errors.Is(err, io.EOF) {
			logger.Printf("carrier closed remote=%s err=%v", r.RemoteAddr, err)
		}
	})
	srv := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Printf("reach websocket carrier listening on %s -> %s", listenAddr, targetAddr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func carrierURL(rawURL, pathPrefix string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("websocket url must use ws:// or wss://, got %q", u.Scheme)
	}
	prefix := strings.TrimSpace(pathPrefix)
	if prefix != "" {
		prefix = "/" + strings.Trim(prefix, "/") + "/"
	}
	base := strings.TrimRight(u.Path, "/")
	if base == "" {
		u.Path = prefix
	} else if prefix != "" && !strings.HasSuffix(base, prefix) {
		u.Path = base + prefix
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

func dialWebSocket(ctx context.Context, u *url.URL) (net.Conn, *bufio.Reader, error) {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		if u.Scheme == "wss" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme == "wss" {
		host := u.Hostname()
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if deadline, ok := ctx.Deadline(); ok {
			_ = tlsConn.SetDeadline(deadline)
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}
	keyBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, keyBytes); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: reach-ws-carrier\r\n\r\n", path, u.Host, key)
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if !strings.Contains(status, " 101 ") && !strings.HasPrefix(status, "HTTP/1.1 101") {
		body, _ := io.ReadAll(io.LimitReader(br, 4096))
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket upgrade failed: %s %s", strings.TrimSpace(status), strings.TrimSpace(string(body)))
	}
	headers := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok {
			headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	}
	want := acceptKey(key)
	if got := headers.Get("Sec-WebSocket-Accept"); got != want {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket accept mismatch")
	}
	return conn, br, nil
}

func upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return nil, nil, fmt.Errorf("not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)
		return nil, nil, fmt.Errorf("bad websocket handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return nil, nil, fmt.Errorf("hijacking unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw.Reader, nil
}

func pump(ctx context.Context, br *bufio.Reader, conn net.Conn, plainIn io.Reader, plainOut io.Writer, client bool) error {
	done := make(chan error, 2)
	var writeMu sync.Mutex
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := plainIn.Read(buf)
			if n > 0 {
				writeMu.Lock()
				werr := writeFrame(conn, 2, buf[:n], client)
				writeMu.Unlock()
				if werr != nil {
					done <- werr
					return
				}
			}
			if err != nil {
				writeMu.Lock()
				_ = writeFrame(conn, 8, []byte{}, client)
				writeMu.Unlock()
				done <- err
				return
			}
		}
	}()
	go func() {
		for {
			op, payload, err := readFrame(br, !client)
			if err != nil {
				done <- err
				return
			}
			switch op {
			case 1, 2:
				if len(payload) > 0 {
					if _, err := plainOut.Write(payload); err != nil {
						done <- err
						return
					}
				}
			case 8:
				done <- io.EOF
				return
			case 9:
				writeMu.Lock()
				_ = writeFrame(conn, 10, payload, client)
				writeMu.Unlock()
			case 10:
				continue
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func writeFrame(w io.Writer, opcode byte, payload []byte, mask bool) error {
	var hdr bytes.Buffer
	hdr.WriteByte(0x80 | (opcode & 0x0f))
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	l := len(payload)
	switch {
	case l < 126:
		hdr.WriteByte(maskBit | byte(l))
	case l <= 0xffff:
		hdr.WriteByte(maskBit | 126)
		_ = binary.Write(&hdr, binary.BigEndian, uint16(l))
	default:
		hdr.WriteByte(maskBit | 127)
		_ = binary.Write(&hdr, binary.BigEndian, uint64(l))
	}
	out := payload
	if mask {
		key := [4]byte{}
		if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
			return err
		}
		hdr.Write(key[:])
		out = make([]byte, len(payload))
		for i, b := range payload {
			out[i] = b ^ key[i%4]
		}
	}
	if _, err := w.Write(hdr.Bytes()); err != nil {
		return err
	}
	_, err := w.Write(out)
	return err
}

func readFrame(r *bufio.Reader, expectMasked bool) (byte, []byte, error) {
	b1, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	b2, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := b1 & 0x0f
	masked := b2&0x80 != 0
	if expectMasked && !masked {
		return 0, nil, fmt.Errorf("expected masked websocket frame")
	}
	l := uint64(b2 & 0x7f)
	switch l {
	case 126:
		var x uint16
		if err := binary.Read(r, binary.BigEndian, &x); err != nil {
			return 0, nil, err
		}
		l = uint64(x)
	case 127:
		if err := binary.Read(r, binary.BigEndian, &l); err != nil {
			return 0, nil, err
		}
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	if l > 16*1024*1024 {
		return 0, nil, fmt.Errorf("websocket frame too large: %d", l)
	}
	payload := make([]byte, int(l))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i, b := range payload {
			payload[i] = b ^ maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func redactPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "/" {
		return path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "ws" && parts[1] == "tunnel" {
		return "/ws/tunnel/<redacted>/"
	}
	return "<redacted>"
}
