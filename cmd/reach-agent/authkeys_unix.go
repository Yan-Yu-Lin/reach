//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Symlink-safe authorized_keys access (daybreak P2-3).
//
// The system-mode agent runs as root but edits <targetHome>/.ssh/
// authorized_keys, a path the target user controls. Following a symlink
// planted there lets that user aim root writes at any file. So:
//
//   - the parent of the home directory is root-controlled and is resolved
//     normally (it may legitimately contain symlinks, e.g. /var on macOS);
//   - the last userControlledDepth components (home, .ssh, authorized_keys)
//     are opened with openat(O_NOFOLLOW) relative to the previous fd;
//   - the file is replaced by writing a temp file inside the verified .ssh
//     fd and renameat()ing it, which replaces a symlink rather than
//     following it; an existing symlink or non-regular file is refused;
//   - only inodes created here are chowned (no chown -R).
const userControlledDepth = 3

type keyDir struct {
	fd      int
	path    string
	created bool
}

func (d keyDir) Close() { _ = unix.Close(d.fd) }

// openKeyDir opens the directory containing authFile without following
// symlinks in the user-controlled components. When create is set, a missing
// final directory (.ssh) is created with mode 0700.
func openKeyDir(authFile string, create bool) (keyDir, string, error) {
	authFile = filepath.Clean(authFile)
	if !filepath.IsAbs(authFile) {
		return keyDir{}, "", fmt.Errorf("authorized_keys path must be absolute: %s", authFile)
	}
	parts := strings.Split(strings.TrimPrefix(authFile, "/"), "/")
	if len(parts) < userControlledDepth {
		return keyDir{}, "", fmt.Errorf("authorized_keys path too short: %s", authFile)
	}
	trustedParts := parts[:len(parts)-userControlledDepth]
	nofollow := parts[len(parts)-userControlledDepth : len(parts)-1]
	name := parts[len(parts)-1]
	trusted, err := filepath.EvalSymlinks("/" + strings.Join(trustedParts, "/"))
	if err != nil {
		return keyDir{}, "", err
	}
	fd, err := unix.Open(trusted, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return keyDir{}, "", fmt.Errorf("open %s: %w", trusted, err)
	}
	cur := trusted
	created := false
	for i, comp := range nofollow {
		last := i == len(nofollow)-1
		next, err := unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create && last {
			if mkErr := unix.Mkdirat(fd, comp, 0o700); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return keyDir{}, "", fmt.Errorf("create %s/%s: %w", cur, comp, mkErr)
			}
			created = true
			next, err = unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		_ = unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return keyDir{}, "", fmt.Errorf("refusing %s/%s: not a real directory (symlink?)", cur, comp)
			}
			return keyDir{}, "", fmt.Errorf("open %s/%s: %w", cur, comp, err)
		}
		fd = next
		cur = cur + "/" + comp
	}
	return keyDir{fd: fd, path: cur, created: created}, name, nil
}

// readAuthorizedKeysFile returns the current content, or nil if the file does
// not exist. A symlink or non-regular file is an error.
func readAuthorizedKeysFile(authFile string) ([]byte, error) {
	dir, name, err := openKeyDir(authFile, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(dir.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("refusing %s/%s: symlink", dir.path, name)
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), dir.path+"/"+name)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing %s/%s: not a regular file", dir.path, name)
	}
	// Read one byte past the limit: an oversized file is an error, never a
	// silently truncated prefix that a caller would write back (astra P2-05).
	b, err := io.ReadAll(io.LimitReader(f, maxAuthorizedKeysBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxAuthorizedKeysBytes {
		return nil, fmt.Errorf("refusing %s/%s: larger than %d bytes", dir.path, name, maxAuthorizedKeysBytes)
	}
	return b, nil
}

// writeAuthorizedKeysFile atomically replaces authFile. When owner is set
// (root installing for another account) the new file, and a .ssh directory
// created here, are chowned to owner.
func writeAuthorizedKeysFile(authFile string, acct keyFileAccount, content []byte) error {
	owner := ""
	if acct.Chown {
		owner = acct.User
	}
	dir, name, err := openKeyDir(authFile, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(dir.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("refusing to replace %s/%s: not a regular file (symlink?)", dir.path, name)
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	uid, gid := -1, -1
	if owner != "" {
		u, g := lookupUIDGID(owner)
		if u == "" || g == "" {
			return fmt.Errorf("unknown owner %q for %s", owner, authFile)
		}
		uid, _ = strconv.Atoi(u)
		gid, _ = strconv.Atoi(g)
	}
	if dir.created && uid >= 0 {
		if err := unix.Fchown(dir.fd, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", dir.path, err)
		}
	}
	tmpName := fmt.Sprintf(".%s.reach-tmp-%d", name, time.Now().UnixNano())
	fd, err := unix.Openat(dir.fd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir.path, err)
	}
	f := os.NewFile(uintptr(fd), dir.path+"/"+tmpName)
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = unix.Unlinkat(dir.fd, tmpName, 0)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if uid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := unix.Renameat(dir.fd, tmpName, dir.fd, name); err != nil {
		return fmt.Errorf("install %s/%s: %w", dir.path, name, err)
	}
	ok = true
	_ = unix.Fsync(dir.fd)
	return nil
}
