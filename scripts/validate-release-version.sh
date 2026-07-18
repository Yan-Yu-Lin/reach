#!/usr/bin/env bash
set -Eeuo pipefail

version="${1:-}"
case "$version" in
  ''|[!0-9A-Za-z]*|*[!0-9A-Za-z._+-]*)
    echo "unsafe release version: $version" >&2
    exit 1
    ;;
esac
if [ "${#version}" -gt 128 ]; then
  echo "release version exceeds 128 characters" >&2
  exit 1
fi
