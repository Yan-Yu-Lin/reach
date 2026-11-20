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

// ServerOptions bounds the carrier (daybreak P2-4). Zero values select the
// defaults below.
type ServerOptions struct {
	// PathPrefix, when set, is the only accepted URL path prefix (the shared
	// /ws/tunnel/<secret>/ route). Requests for any other path get 404
	// before the backend is dialled.
	PathPrefix string
	// MaxConns is the global concurrent connection limit.
	MaxConns int
	// MaxConnsPerIP limits concurrent connections per client IP (taken from
	// X-Real-IP when the peer is loopback, i.e. nginx).
	MaxConnsPerIP int
	// IdleTimeout closes a carried connection with no traffic in either
	// direction for this long.
	IdleTimeout time.Duration
	// HandshakeTimeout bounds the HTTP upgrade.
	HandshakeTimeout time.Duration
}

const (
	defaultMaxConns         = 64
	defaultMaxConnsPerIP    = 8
	defaultIdleTimeout      = 10 * time.Minute
	defaultHandshakeTimeout = 10 * time.Second
	// maxFramePayload bounds a single frame. Frames are streamed in chunks,
	// so this is a protocol sanity limit, not a buffer size.
	maxFramePayload = 16 * 1024 * 1024
	streamChunk     = 32 * 1024
)

func (o ServerOptions) withDefaults() ServerOptions {
	if o.MaxConns <= 0 {
		o.MaxConns = defaultMaxConns
	}
	if o.MaxConnsPerIP <= 0 {
		o.MaxConnsPerIP = defaultMaxConnsPerIP
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = defaultHandshakeTimeout
	}
	if o.PathPrefix != "" {
		o.PathPrefix = "/" + strings.Trim(o.PathPrefix, "/") + "/"
	}
	return o
}

type connLimiter struct {
	mu    sync.Mutex
	total int
	perIP map[string]int
	max   int
	maxIP int
}

func (l *connLimiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.max || l.perIP[ip] >= l.maxIP {
		return false
	}
	l.total++
	l.perIP[ip]++
	return true
}

func (l *connLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	return host
}

// Serve accepts WebSocket connections and pipes each one to targetAddr.
func Serve(ctx context.Context, listenAddr, targetAddr string, logger *log.Logger) error {
	return ServeWithOptions(ctx, listenAddr, targetAddr, logger, ServerOptions{})
}

// NewHandler returns the carrier HTTP handler (exported for tests).
func NewHandler(targetAddr string, logger *log.Logger, opts ServerOptions) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	opts = opts.withDefaults()
	limiter := &connLimiter{perIP: map[string]int{}, max: opts.MaxConns, maxIP: opts.MaxConnsPerIP}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.PathPrefix != "" && !strings.HasPrefix(r.URL.Path, opts.PathPrefix) {
			http.NotFound(w, r)
			return
		}
		// Validate the upgrade before touching the backend.
		if !headerHasToken(r.Header.Get("Connection"), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
			http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
			return
		}
		ip := remoteIP(r)
		if !limiter.acquire(ip) {
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
		defer limiter.release(ip)
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
		logger.Printf("carrier connected remote=%s target=%s path=%s", ip, targetAddr, redactPath(r.URL.Path))
		idle := &idleConn{Conn: conn, timeout: opts.IdleTimeout}
		idleTarget := &idleConn{Conn: target, timeout: opts.IdleTimeout, peer: idle}
		idle.peer = idleTarget
		idle.touch()
		if err := serverPump(r.Context(), br, idle, idleTarget); err != nil && !errors.Is(err, io.EOF) {
			logger.Printf("carrier closed remote=%s err=%v", ip, err)
		}
	})
}

// ServeWithOptions is Serve with explicit limits.
func ServeWithOptions(ctx context.Context, listenAddr, targetAddr string, logger *log.Logger, opts ServerOptions) error {
	if logger == nil {
		logger = log.Default()
	}
	opts = opts.withDefaults()
	srv := &http.Server{Addr: listenAddr, Handler: NewHandler(targetAddr, logger, opts), ReadHeaderTimeout: opts.HandshakeTimeout, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Printf("reach websocket carrier listening on %s -> %s (max %d conns, %d per IP, exact path %v)", listenAddr, targetAddr, opts.MaxConns, opts.MaxConnsPerIP, opts.PathPrefix != "")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// idleConn closes both sides when neither has carried traffic for timeout.
// Any read or write on either side extends both deadlines.
type idleConn struct {
	net.Conn
	timeout time.Duration
	peer    *idleConn
}

func (c *idleConn) touch() {
	d := time.Now().Add(c.timeout)
	_ = c.Conn.SetDeadline(d)
	if c.peer != nil {
		_ = c.peer.Conn.SetDeadline(d)
	}
}

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *idleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
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

// serverPump is the carrier side of pump. Client frames are streamed into
// the backend in fixed-size chunks, so a peer that declares a 16 MiB frame
// and sends it slowly costs 32 KiB, not 16 MiB (daybreak P2-4). Each chunk
// read refreshes the idle deadline.
func serverPump(ctx context.Context, br *bufio.Reader, ws *idleConn, target *idleConn) error {
	done := make(chan error, 2)
	var writeMu sync.Mutex
	go func() {
		buf := make([]byte, streamChunk)
		for {
			n, err := target.Read(buf)
			if n > 0 {
				writeMu.Lock()
				werr := writeFrame(ws, 2, buf[:n], false)
				writeMu.Unlock()
				if werr != nil {
					done <- werr
					return
				}
			}
			if err != nil {
				writeMu.Lock()
				_ = writeFrame(ws, 8, []byte{}, false)
				writeMu.Unlock()
				done <- err
				return
			}
		}
	}()
	go func() {
		chunk := make([]byte, streamChunk)
		for {
			op, length, maskKey, err := readFrameHeader(br, true)
			if err != nil {
				done <- err
				return
			}
			ws.touch()
			switch op {
			case 1, 2, 0:
				var off uint64
				for off < length {
					n := uint64(len(chunk))
					if length-off < n {
						n = length - off
					}
					if _, err := io.ReadFull(br, chunk[:n]); err != nil {
						done <- err
						return
					}
					for i := uint64(0); i < n; i++ {
						chunk[i] ^= maskKey[(off+i)%4]
					}
					ws.touch()
					if _, err := target.Write(chunk[:n]); err != nil {
						done <- err
						return
					}
					off += n
				}
			case 8:
				done <- io.EOF
				return
			case 9, 10:
				if length > 125 {
					done <- fmt.Errorf("control frame too large")
					return
				}
				payload := make([]byte, length)
				if _, err := io.ReadFull(br, payload); err != nil {
					done <- err
					return
				}
				for i := range payload {
					payload[i] ^= maskKey[i%4]
				}
				if op == 9 {
					writeMu.Lock()
					_ = writeFrame(ws, 10, payload, false)
					writeMu.Unlock()
				}
			default:
				done <- fmt.Errorf("unsupported websocket opcode %d", op)
				return
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

// readFrameHeader reads a frame header and returns opcode, payload length
// and mask key without reading the payload.
func readFrameHeader(r *bufio.Reader, expectMasked bool) (byte, uint64, [4]byte, error) {
	var maskKey [4]byte
	b1, err := r.ReadByte()
	if err != nil {
		return 0, 0, maskKey, err
	}
	b2, err := r.ReadByte()
	if err != nil {
		return 0, 0, maskKey, err
	}
	opcode := b1 & 0x0f
	masked := b2&0x80 != 0
	if expectMasked && !masked {
		return 0, 0, maskKey, fmt.Errorf("expected masked websocket frame")
	}
	l := uint64(b2 & 0x7f)
	switch l {
	case 126:
		var x uint16
		if err := binary.Read(r, binary.BigEndian, &x); err != nil {
			return 0, 0, maskKey, err
		}
		l = uint64(x)
	case 127:
		if err := binary.Read(r, binary.BigEndian, &l); err != nil {
			return 0, 0, maskKey, err
		}
	}
	if l > maxFramePayload {
		return 0, 0, maskKey, fmt.Errorf("websocket frame too large: %d", l)
	}
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, 0, maskKey, err
		}
	}
	return opcode, l, maskKey, nil
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
