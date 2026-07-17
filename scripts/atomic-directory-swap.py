#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///

"""Atomically exchange a staged directory with a live directory on Linux."""

from __future__ import annotations

import ctypes
import os
import sys
from pathlib import Path

AT_FDCWD = -100
RENAME_EXCHANGE = 2


def fail(message: str) -> None:
    raise SystemExit(f"atomic-directory-swap: {message}")


def main() -> None:
    if len(sys.argv) != 3:
        fail("usage: atomic-directory-swap.py STAGED LIVE")

    staged = Path(sys.argv[1])
    live = Path(sys.argv[2])
    if not staged.is_absolute() or not live.is_absolute():
        fail("both paths must be absolute")
    if staged.is_symlink() or not staged.is_dir():
        fail(f"staged path is not a real directory: {staged}")
    if live.is_symlink():
        fail(f"live path must not be a symlink: {live}")

    if not live.exists():
        os.rename(staged, live)
        return
    if not live.is_dir():
        fail(f"live path is not a directory: {live}")
    if staged.stat().st_dev != live.stat().st_dev:
        fail("staged and live directories must be on the same filesystem")

    libc = ctypes.CDLL(None, use_errno=True)
    renameat2 = getattr(libc, "renameat2", None)
    if renameat2 is None:
        fail("libc does not provide renameat2; refusing a non-atomic swap")
    renameat2.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    renameat2.restype = ctypes.c_int

    result = renameat2(
        AT_FDCWD,
        os.fsencode(staged),
        AT_FDCWD,
        os.fsencode(live),
        RENAME_EXCHANGE,
    )
    if result != 0:
        error = ctypes.get_errno()
        fail(f"renameat2 failed: {os.strerror(error)}")


if __name__ == "__main__":
    main()
