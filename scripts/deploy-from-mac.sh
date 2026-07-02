#!/usr/bin/env bash
set -Eeuo pipefail

# Full deploy from a workstation to a configured hub.
# Set REACH_DEPLOY_HOST to the SSH alias for your hub.
# Builds dashboard locally (needs Node 22+), uploads it, and runs deploy on the hub.

cd "$(dirname "$0")/.."

export CGO_ENABLED=0
DEPLOY_HOST="${REACH_DEPLOY_HOST:-reach-hub}"

echo "[mac] building dashboard..."
cd dashboard
npx nuxt generate 2>&1 | tail -1
cd ..

echo "[mac] uploading dashboard to ${DEPLOY_HOST}..."
tar czf /tmp/reach-dashboard-deploy.tar.gz -C dashboard/.output/public .
scp /tmp/reach-dashboard-deploy.tar.gz "${DEPLOY_HOST}:/tmp/"
ssh "${DEPLOY_HOST}" "sudo tar xzf /tmp/reach-dashboard-deploy.tar.gz -C /opt/reach-dashboard/ && rm /tmp/reach-dashboard-deploy.tar.gz"
rm /tmp/reach-dashboard-deploy.tar.gz

echo "[mac] running deploy on ${DEPLOY_HOST}..."
ssh "${DEPLOY_HOST}" "cd ~/reach && git pull --ff-only && CGO_ENABLED=0 bash scripts/deploy-jason.sh"

echo "[mac] done."
