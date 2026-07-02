//go:build windows

package main

import "syscall"

const (
	windowsCreateNewProcessGroup = 0x00000200
	windowsCreateNoWindow        = 0x08000000
)

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windowsCreateNewProcessGroup | windowsCreateNoWindow,
		HideWindow:    true,
	}
}
