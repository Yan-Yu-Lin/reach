//go:build !windows

package main

import "os"

func hasReparseAttr(os.FileInfo) bool { return false }
