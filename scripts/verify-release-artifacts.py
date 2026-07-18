#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///

"""Verify a complete Reach agent artifact set against its release metadata."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

ASSET_NAMES = {
    "reach-agent_linux_amd64",
    "reach-agent_linux_arm64",
    "reach-agent_linux_386",
    "reach-agent_linux_armv6",
    "reach-agent_linux_armv7",
    "reach-agent_darwin_amd64",
    "reach-agent_darwin_arm64",
    "reach-agent_windows_amd64.exe",
    "reach-agent_windows_arm64.exe",
}


def fail(message: str) -> None:
    raise SystemExit(f"artifact verification failed: {message}")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def read_checksums(path: Path) -> dict[str, str]:
    checksums: dict[str, str] = {}
    for line_number, line in enumerate(path.read_text().splitlines(), 1):
        fields = line.split()
        if len(fields) != 2:
            fail(f"invalid checksums.txt line {line_number}")
        digest, name = fields
        name = name.removeprefix("*")
        if name in checksums:
            fail(f"duplicate checksum for {name}")
        if len(digest) != 64 or any(character not in "0123456789abcdefABCDEF" for character in digest):
            fail(f"invalid checksum for {name}")
        checksums[name] = digest.lower()
    return checksums


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dir", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()

    directory = Path(args.dir)
    try:
        manifest = json.loads((directory / "manifest.json").read_text())
        checksums = read_checksums(directory / "checksums.txt")
    except (OSError, json.JSONDecodeError) as error:
        fail(str(error))

    if manifest.get("schema") != 1 or manifest.get("project") != "reach-agent":
        fail("unexpected manifest schema or project")
    if manifest.get("version") != args.version:
        fail(f"manifest version is {manifest.get('version')!r}, expected {args.version!r}")
    if manifest.get("git_commit") != args.commit:
        fail(f"manifest commit is {manifest.get('git_commit')!r}, expected {args.commit!r}")

    assets = manifest.get("assets")
    if not isinstance(assets, dict) or set(assets) != ASSET_NAMES:
        fail("manifest asset set is incomplete or contains unexpected entries")
    if set(checksums) != ASSET_NAMES:
        fail("checksums.txt asset set is incomplete or contains unexpected entries")

    for name in sorted(ASSET_NAMES):
        path = directory / name
        try:
            stat = path.stat()
        except OSError as error:
            fail(str(error))
        metadata = assets[name]
        if not isinstance(metadata, dict):
            fail(f"invalid manifest metadata for {name}")
        actual_digest = sha256(path)
        if metadata.get("sha256") != actual_digest:
            fail(f"manifest checksum mismatch for {name}")
        if checksums[name] != actual_digest:
            fail(f"checksums.txt mismatch for {name}")
        if metadata.get("size") != stat.st_size:
            fail(f"manifest size mismatch for {name}")

    print(f"verified reach-agent {args.version} artifacts for {args.commit}")


if __name__ == "__main__":
    main()
