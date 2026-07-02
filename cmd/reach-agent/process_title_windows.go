//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

func defaultWindowsAgentConfigPath() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	candidates := []string{filepath.Join(programData, "Reach", "agent.yaml")}
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "Reach", "agent.yaml"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return candidates[0]
}

func isDiscoverableWindowsAgentConfig(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	candidates := []string{defaultWindowsAgentConfigPath()}
	if v := os.Getenv("REACH_AGENT_CONFIG"); v != "" {
		candidates = append(candidates, v)
	}
	for _, c := range candidates {
		cAbs, err := filepath.Abs(c)
		if err == nil && strings.EqualFold(cAbs, abs) {
			return true
		}
	}
	return false
}
