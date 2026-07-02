//go:build !windows

package main

func defaultWindowsAgentConfigPath() string             { return "" }
func isDiscoverableWindowsAgentConfig(path string) bool { return false }
