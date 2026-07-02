//go:build !linux

package main

func maybeReexecForProcessTitle(args []string) {}
func setProcessTitle(title string)             {}
