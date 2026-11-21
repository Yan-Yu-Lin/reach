//go:build windows

package main

import "os"

// reachd runs on Linux; Windows builds (development only) have no
// cross-process lock.
func tryFlock(*os.File, bool) (bool, error) { return true, nil }
func flockBlocking(*os.File) error          { return nil }
func unflock(*os.File) error                { return nil }

func copyOwner(*os.File, os.FileInfo) error { return nil }

func openLockFileSafe(dir, name string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(dir+string(os.PathSeparator)+name, os.O_CREATE|os.O_RDWR, 0o600)
}

func checkPrivateRegularFile(path string) (os.FileInfo, error) { return os.Lstat(path) }
