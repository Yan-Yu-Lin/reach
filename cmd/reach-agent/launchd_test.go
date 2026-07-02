package main

import (
	"strings"
	"testing"
)

func TestLaunchdPlistEscapesAndIncludesArgs(t *testing.T) {
	plist := launchdPlist("dev.arthurlin.reach-agent", "/tmp/reach&agent", "/custom/path/agent.yaml", "/tmp/reach-data", "/tmp/reach-data/agent.log")
	for _, want := range []string{
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/tmp/reach&amp;agent</string>",
		"<string>run</string>",
		"<string>--config</string>",
		"<string>/custom/path/agent.yaml</string>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}
