//go:build linux

package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPaddedProcessArgv0(t *testing.T) {
	got := paddedProcessArgv0("Reach-Agent", 32)
	if len(got) != 32 {
		t.Fatalf("len = %d, want 32", len(got))
	}
	if !strings.HasPrefix(got, "Reach-Agent") {
		t.Fatalf("missing prefix: %q", got)
	}
}

func TestWriteProcessTitleBufferBoundsAndUTF8(t *testing.T) {
	buf := []byte(strings.Repeat("x", 7))
	written := writeProcessTitleBuffer(buf, "你好世界")
	if written != "你好" {
		t.Fatalf("written = %q, want %q", written, "你好")
	}
	if !utf8.Valid(buf[:len(written)]) {
		t.Fatalf("buffer prefix is not valid UTF-8: %q", string(buf))
	}
	if got := string(buf[:6]); got != "你好" {
		t.Fatalf("buffer prefix = %q, want %q", got, "你好")
	}
	if buf[6] != 0 {
		t.Fatalf("byte after title = %d, want 0", buf[6])
	}
}
