//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
)

func copyFileNoFollow(src, dst string, mode os.FileMode) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || hasReparseAttr(st) {
		return fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, 128<<20)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
