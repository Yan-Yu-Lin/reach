//go:build !darwin

package main

import "os"

func syncFileDurable(file *os.File) error {
	return file.Sync()
}
