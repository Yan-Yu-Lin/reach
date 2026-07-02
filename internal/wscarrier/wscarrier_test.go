package wscarrier

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestCarrierEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	wsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wsAddr := wsLn.Addr().String()
	_ = wsLn.Close()
	go func() {
		_ = Serve(ctx, wsAddr, echoLn.Addr().String(), log.New(io.Discard, "", 0))
	}()
	time.Sleep(100 * time.Millisecond)
	inR, inW := io.Pipe()
	var out bytes.Buffer
	errCh := make(chan error, 1)
	go func() { errCh <- Client(ctx, "ws://"+wsAddr, "/secret", inR, &out) }()
	msg := []byte("hello over websocket")
	if _, err := inW.Write(msg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && out.Len() < len(msg) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = inW.Close()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		cancel()
	}
	if got := out.String(); got != string(msg) {
		t.Fatalf("echo mismatch: got %q want %q", got, string(msg))
	}
}
