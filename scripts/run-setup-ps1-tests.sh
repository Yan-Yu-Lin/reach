#!/usr/bin/env bash
# Runs scripts/test-setup-ps1-policy.ps1 with PowerShell 7 in a throwaway
# Ubuntu container (native architecture; the official PowerShell image has
# no arm64 build). Windows cmdlets are mocked by the test.
# Usage: bash scripts/run-setup-ps1-tests.sh [repo-dir]
set -Eeuo pipefail
repo="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
ver=7.4.6
docker run --rm -v "$repo:/src:ro" ubuntu:24.04 bash -c "
set -e
apt-get -qq update >/dev/null && apt-get -qq install -y curl ca-certificates libicu74 >/dev/null
case \$(uname -m) in aarch64) a=arm64 ;; *) a=x64 ;; esac
curl -fsSL -o /tmp/ps.tgz https://github.com/PowerShell/PowerShell/releases/download/v$ver/powershell-$ver-linux-\$a.tar.gz
mkdir -p /opt/pwsh && tar xzf /tmp/ps.tgz -C /opt/pwsh && chmod +x /opt/pwsh/pwsh
/opt/pwsh/pwsh -NoProfile -NonInteractive -File /src/scripts/test-setup-ps1-policy.ps1 < /dev/null
"
