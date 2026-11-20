#!/usr/bin/env bash
# Regression for daybreak round 4 P1-01: a Stage 4 (server-only) deploy
# must not build, sign, upload or publish an agent release.
#
# Runs scripts/deploy-from-mac.sh (working-tree copy) from a throwaway clone.
# ssh/scp/go/uv are stand-ins; the remote deploy script that
# deploy-from-mac.sh sends over ssh really runs, in a fake hub home, up to
# the point where it starts scripts/deploy-jason.sh, which is replaced by a
# recorder of the environment it was given.
#
# Usage (from the repo root): bash scripts/test-deploy-from-mac.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-dfm-test.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT

git init -q --bare "$work/origin.git"
git clone -q "$repo" "$work/src"
git -C "$work/src" remote set-url origin "$work/origin.git"
git -C "$work/src" checkout -q -B stage4-test
cp "$repo/scripts/deploy-from-mac.sh" "$work/src/scripts/deploy-from-mac.sh"
git -C "$work/src" -c user.name=test -c user.email=test@invalid commit -qam "test copy" --allow-empty
git -C "$work/src" push -q origin stage4-test

hub="$work/hub"
mkdir -p "$hub"
git clone -q "$work/origin.git" "$hub/reach"
log="$work/calls"
bin="$work/bin"
mkdir -p "$bin"
export TEST_WORK="$work"

cat > "$work/fake-deploy-jason.sh" <<'EOF'
#!/bin/bash
env | grep -E '^REACH_(PUBLISH_AGENT_RELEASE|AGENT_ARTIFACT_DIR|AGENT_VERSION)=' | sort > "$TEST_WORK/remote-env"
EOF

cat > "$bin/ssh" <<'EOF'
#!/bin/bash
echo "ssh $*" >> "$TEST_WORK/calls"
if [ "$2" = "bash -s" ]; then
  shift 3
  body="$(cat)"
  case "$body" in
    *scripts/deploy-jason.sh*)
      body="${body//bash scripts\/deploy-jason.sh/bash $TEST_WORK\/fake-deploy-jason.sh}"
      HOME="$TEST_WORK/hub" XDG_RUNTIME_DIR="$TEST_WORK/hub" bash -c "$body" remote "$@"
      exit $? ;;
  esac
  exit 0
fi
printf '/tmp/reach-agent-artifacts.X\n/tmp/reach-agent-artifacts.X.tar.gz\n'
EOF
for tool in scp go uv; do
  printf '#!/bin/bash\necho "%s $*" >> "$TEST_WORK/calls"\n' "$tool" > "$bin/$tool"
done
printf '#!/bin/bash\nexit 0\n' > "$bin/trash"
command -v flock >/dev/null || printf '#!/bin/bash\nexit 0\n' > "$bin/flock"
chmod +x "$bin"/*

run() { # publish-flag ("" = unset)
  : > "$log"; : > "$work/remote-env"
  (
    cd "$work/src"
    export PATH="$bin:$PATH" REACH_DEPLOY_HOST=fake-hub HOME="$work/home"
    mkdir -p "$HOME"
    if [ -n "$1" ]; then export REACH_PUBLISH_AGENT_RELEASE="$1"; else unset REACH_PUBLISH_AGENT_RELEASE; fi
    bash scripts/deploy-from-mac.sh
  ) > "$work/out" 2>&1
}

fail() { echo "FAIL: $*"; sed 's/^/  /' "$log"; sed 's/^/  > /' "$work/out"; exit 1; }

for flag in "" 0; do
  run "$flag" || fail "deploy-from-mac.sh (flag='$flag') failed"
  if grep -q '^go ' "$log"; then fail "flag='$flag': agent built/signed (go was called)"; fi
  if grep -q '^scp ' "$log"; then fail "flag='$flag': artifacts uploaded"; fi
  grep -qx 'REACH_PUBLISH_AGENT_RELEASE=0' "$work/remote-env" || fail "flag='$flag': deploy-jason.sh not started with publish=0"
  if grep -q 'REACH_AGENT_ARTIFACT_DIR' "$work/remote-env"; then fail "flag='$flag': deploy-jason.sh given an artifact dir"; fi
  grep -q 'Server-only deploy' "$work/out" || fail "flag='$flag': no server-only message"
  echo "ok: REACH_PUBLISH_AGENT_RELEASE='${flag:-unset}' builds and publishes no agent"
done

if run 2; then fail "REACH_PUBLISH_AGENT_RELEASE=2 accepted"; fi
if grep -q 'bash -s' "$log"; then fail "invalid flag still reached the hub"; fi
echo "ok: invalid REACH_PUBLISH_AGENT_RELEASE refused"

if run 1; then fail "publish without a signing key succeeded"; fi
grep -q 'release signing key not found' "$work/out" || fail "publish=1 did not check the key"
if grep -q 'bash -s' "$log"; then fail "publish=1 without key reached the hub"; fi
echo "ok: REACH_PUBLISH_AGENT_RELEASE=1 still requires the signing key"

echo "PASS: deploy-from-mac publication gate"
