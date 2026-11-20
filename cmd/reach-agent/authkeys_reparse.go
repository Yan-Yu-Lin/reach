package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkNoLinks refuses a path whose user-controlled components (the last
// `depth` components, i.e. <home>/.ssh/authorized_keys) are symlinks or, on
// Windows, junctions/reparse points (daybreak P2-9). isLink is injected so
// the check is testable off Windows.
//
// This is a pathname check. It narrows, but does not close, a race where
// the user swaps a component for a junction between the check and the
// write; callers re-check immediately before replacing the file. Full
// handle-relative traversal on Windows is described in
// docs/design/2026-11-21-windows-keyfile-traversal.md.
func checkNoLinks(path string, depth int, isLink func(string) (bool, error)) error {
	path = filepath.Clean(path)
	parts := []string{}
	cur := path
	for i := 0; i < depth; i++ {
		parts = append(parts, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for _, p := range parts {
		link, err := isLink(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if link {
			return fmt.Errorf("refusing %s: %s is a link/junction", path, p)
		}
	}
	return nil
}

// isLinkOrReparse reports whether p is a symlink or (on Windows) any
// reparse point, without following it.
func isLinkOrReparse(p string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return false, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	return hasReparseAttr(st), nil
}

// samePath compares two Windows-style paths case-insensitively.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(strings.TrimPrefix(a, `\\?\`)), filepath.Clean(strings.TrimPrefix(b, `\\?\`)))
}
