#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///

"""Delete older deployment backups matching explicit path patterns."""

from __future__ import annotations

import argparse
import glob
import shutil
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--retain", type=int, required=True)
    parser.add_argument("patterns", nargs="+")
    args = parser.parse_args()
    if args.retain < 1:
        raise SystemExit("--retain must be at least 1")

    for pattern in args.patterns:
        if not pattern.startswith("/") or ".." in Path(pattern).parts:
            raise SystemExit(f"unsafe backup pattern: {pattern}")
        matches = sorted((Path(path) for path in glob.glob(pattern)), reverse=True)
        for path in matches[args.retain :]:
            if path.is_symlink() or path.is_file():
                path.unlink()
            elif path.is_dir():
                shutil.rmtree(path)


if __name__ == "__main__":
    main()
