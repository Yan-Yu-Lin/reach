package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func carrierArchive(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	payload := []byte("carrier-binary")
	if err := tw.WriteHeader(&tar.Header{Name: "wstunnel", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

// Astra R4-08 / daybreak round 5 N5: the carrier URL from the hub cannot
// authorise its own origin; only the configured API origin is allowed, and
// no request is sent to anything else.
func TestCarrierDownloadOnlyFromConfiguredAPI(t *testing.T) {
	archive := carrierArchive(t)
	sum := sha256.Sum256(archive)
	var foreignHits, apiHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		_, _ = io.Copy(w, bytes.NewReader(archive))
	}))
	defer foreign.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		_, _ = io.Copy(w, bytes.NewReader(archive))
	}))
	defer api.Close()
	dest := filepath.Join(t.TempDir(), "wstunnel")
	ws := &websocketTunnelConfig{Binaries: map[string]websocketBinary{runtimeArchKey(): {URL: foreign.URL + "/c.tgz", SHA256: hex.EncodeToString(sum[:])}}}
	if err := installWSTunnel(context.Background(), api.URL, dest, ws); err == nil {
		t.Fatal("carrier downloaded from a foreign origin")
	}
	if foreignHits.Load() != 0 {
		t.Fatal("a request reached the foreign origin")
	}
	ws.Binaries[runtimeArchKey()] = websocketBinary{URL: api.URL + "/c.tgz", SHA256: hex.EncodeToString(sum[:])}
	if err := installWSTunnel(context.Background(), api.URL, dest, ws); err != nil {
		t.Fatalf("same-origin carrier refused: %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "carrier-binary" || apiHits.Load() != 1 {
		t.Fatalf("got %q hits=%d", b, apiHits.Load())
	}
}
