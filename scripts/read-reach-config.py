#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["PyYAML>=6,<7"]
# ///

"""Read Reach config values with the same defaults used by the Go server."""

from __future__ import annotations

import argparse
from pathlib import Path

import yaml

DEFAULTS = {
    "db_path": "/var/lib/reach/reach.db",
    "listen_addr": "127.0.0.1:9300",
}


def lookup(data: dict[object, object], key: str) -> object:
    value: object = data
    for component in key.split("."):
        if not isinstance(value, dict):
            return None
        value = value.get(component)
    return value


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="/etc/reach/config.yaml")
    parser.add_argument("key")
    args = parser.parse_args()

    path = Path(args.config)
    data = yaml.safe_load(path.read_text()) if path.exists() else {}
    if data is None:
        data = {}
    if not isinstance(data, dict):
        raise SystemExit("Reach config must be a YAML mapping")
    value = lookup(data, args.key)
    if value in (None, ""):
        if args.key not in DEFAULTS:
            raise SystemExit(0)
        value = DEFAULTS[args.key]
    if not isinstance(value, str):
        raise SystemExit(f"{args.key} must be a string")
    print(value)


if __name__ == "__main__":
    main()
