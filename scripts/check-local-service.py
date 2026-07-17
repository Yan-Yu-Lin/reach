#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///

"""Verify that a local TCP service accepts a connection."""

from __future__ import annotations

import socket
import sys


def split_address(address: str) -> tuple[str, int]:
    if address.startswith("["):
        host, separator, port = address[1:].partition("]:")
    else:
        host, separator, port = address.rpartition(":")
    if not separator or not host or not port.isdigit():
        raise ValueError(f"invalid listen address: {address}")
    return host, int(port)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("check-local-service: usage: check-local-service.py HOST:PORT")
    try:
        host, port = split_address(sys.argv[1])
        with socket.create_connection((host, port), timeout=2):
            pass
    except (OSError, ValueError) as error:
        raise SystemExit(f"check-local-service: {error}") from error


if __name__ == "__main__":
    main()
