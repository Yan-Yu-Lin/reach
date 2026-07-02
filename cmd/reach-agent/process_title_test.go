package main

import (
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncateUTF8Bytes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "ascii", in: "Reach-Agent", max: 5, want: "Reach"},
		{name: "keeps full rune", in: "你好世界", max: 6, want: "你好"},
		{name: "drops partial rune", in: "你好世界", max: 7, want: "你好"},
		{name: "emoji", in: "Go 🚀 now", max: 6, want: "Go "},
		{name: "zero max", in: "Reach", max: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateUTF8Bytes(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("truncateUTF8Bytes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if len(got) > tt.max {
				t.Fatalf("result length %d > max %d", len(got), tt.max)
			}
		})
	}
}

func TestNormalizeProcessTitleSettings(t *testing.T) {
	got := normalizeProcessTitleSettings([]string{"", "  hello  ", "a\x00b"}, 0)
	if len(got.Titles) != 2 || got.Titles[0] != "hello" || got.Titles[1] != "a b" {
		t.Fatalf("unexpected titles: %#v", got.Titles)
	}
	if got.Interval != 5*time.Second {
		t.Fatalf("interval = %v, want 5s", got.Interval)
	}

	got = normalizeProcessTitleSettings(nil, time.Second)
	if len(got.Titles) != 1 || got.Titles[0] != defaultProcessTitle {
		t.Fatalf("default titles = %#v", got.Titles)
	}
}
