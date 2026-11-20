//go:build windows

package main

import (
	"os"
	"syscall"
)

func hasReparseAttr(st os.FileInfo) bool {
	if d, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}
