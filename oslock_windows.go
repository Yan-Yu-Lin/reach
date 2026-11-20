//go:build windows

package main

import "os"

// reachd runs on Linux; Windows builds (development only) have no
// cross-process lock.
func tryFlock(*os.File, bool) (bool, error) { return true, nil }
func flockBlocking(*os.File) error          { return nil }
func unflock(*os.File) error                { return nil }
