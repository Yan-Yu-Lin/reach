package main

import (
	"errors"
	"io"
)

// ioReadFull reads until buf is full or EOF and returns the byte count.
func ioReadFull(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, nil
	}
	return n, err
}
