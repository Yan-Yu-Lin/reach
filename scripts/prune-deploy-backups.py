#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///

"""Delete older deployment backups.

Two modes:

  prune-deploy-backups.py --retain N PATTERN...
      For each pattern separately, keep the N newest matches.

  prune-deploy-backups.py --retain N --group TS_REGEX PATTERN...
      Group every match by the deploy timestamp the regex captures and
      delete whole groups, keeping the N newest timestamps. Used for the
      artefacts of one rollback (fence, snapshot, probe binary, config
      backup, forensic set): they are only useful together, and an old
      fence left without its snapshot (or the other way round) could be
      used for a wrong manual rollback (daybreak round 5 N9).
"""

from __future__ import annotations

import argparse
import glob
import re
import shutil
from pathlib import Path


def remove(path: Path) -> None:
    if path.is_symlink() or path.is_file():
        path.unlink()
    elif path.is_dir():
        shutil.rmtree(path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--retain", type=int, required=True)
    parser.add_argument("--group", help="regex with one group capturing the deploy timestamp")
    parser.add_argument("patterns", nargs="+")
    args = parser.parse_args()
    if args.retain < 1:
        raise SystemExit("--retain must be at least 1")
    for pattern in args.patterns:
        if not pattern.startswith("/") or ".." in Path(pattern).parts:
            raise SystemExit(f"unsafe backup pattern: {pattern}")

    if args.group:
        ts_re = re.compile(args.group)
        groups: dict[str, list[Path]] = {}
        for pattern in args.patterns:
            for path in glob.glob(pattern):
                m = ts_re.search(Path(path).name)
                if m:
                    groups.setdefault(m.group(1), []).append(Path(path))
        for ts in sorted(groups, reverse=True)[args.retain :]:
            for path in groups[ts]:
                remove(path)
        return

    for pattern in args.patterns:
        matches = sorted((Path(path) for path in glob.glob(pattern)), reverse=True)
        for path in matches[args.retain :]:
            remove(path)


if __name__ == "__main__":
    main()
