#!/usr/bin/env bash
# Regressions for the deploy entry points:
# - daybreak round 4 P1-01: a Stage 4 (server-only) deploy must not build,
#   sign, upload or publish an agent release.
# - daybreak round 6 R6-P1-01: only the exact pushed tip of the one release
#   branch deploys -- on the Mac, again on the hub, and when deploy-jason.sh
#   is run directly. No override, no detached-HEAD switch to main.
#
# Runs the working-tree scripts from a throwaway clone whose branch has the
# real release branch name (the scripts have no test override). ssh/scp/go/uv
# are stand-ins; the remote deploy script that deploy-from-mac.sh sends over
# ssh really runs, in a fake hub home, up to the point where it starts
# scripts/deploy-jason.sh, which is replaced by a recorder of its environment.
#
# Usage (from the repo root): bash scripts/test-deploy-from-mac.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-dfm-test.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT

release="$(sed -n 's/^REACH_RELEASE_BRANCH="\(.*\)"$/\1/p' "$repo/scripts/release-gate.sh")"
[ -n "$release" ] || { echo "FAIL: cannot read the release branch name"; exit 1; }

git init -q --bare "$work/origin.git"
git clone -q "$repo" "$work/src"
git -C "$work/src" remote set-url origin "$work/origin.git"
gitc() { git -C "$work/src" -c user.name=test -c user.email=test@invalid "$@"; }
gitc checkout -q -B "$release"
for f in deploy-from-mac.sh deploy-jason.sh release-gate.sh; do
  cp "$repo/scripts/$f" "$work/src/scripts/$f"
done
gitc add scripts
gitc commit -qm "test copy" --allow-empty
gitc push -q origin "$release"

hub="$work/hub"
mkdir -p "$hub" "$work/run"
git clone -q "$work/origin.git" "$hub/reach"
git clone -q -b "$release" "$work/origin.git" "$work/mover"
log="$work/calls"
bin="$work/bin"
mkdir -p "$bin"
export TEST_WORK="$work"

cat > "$work/fake-deploy-jason.sh" <<'EOF'
#!/bin/bash
env | grep -E '^REACH_(PUBLISH_AGENT_RELEASE|AGENT_ARTIFACT_DIR|AGENT_VERSION)=' | sort > "$TEST_WORK/remote-env"
EOF

# TEST_MOVE_TIP=1: someone pushes to the release branch after the Mac checked
# it and before the hub fetches.
cat > "$bin/ssh" <<'EOF'
#!/bin/bash
echo "ssh $*" >> "$TEST_WORK/calls"
if [ "$2" = "bash -s" ]; then
  shift 3
  body="$(cat)"
  if [ "${TEST_MOVE_TIP:-0}" = 1 ]; then
    git -C "$TEST_WORK/mover" -c user.name=t -c user.email=t@invalid commit -qm moved --allow-empty
    git -C "$TEST_WORK/mover" push -q origin HEAD
  fi
  case "$body" in
    *scripts/deploy-jason.sh*)
      body="${body//bash scripts\/deploy-jason.sh/bash $TEST_WORK\/fake-deploy-jason.sh}"
      HOME="$TEST_WORK/hub" XDG_RUNTIME_DIR="$TEST_WORK/run" bash -c "$body" remote "$@"
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
  [ "$(git -C "$hub/reach" rev-parse HEAD)" = "$(git -C "$work/src" rev-parse HEAD)" ] || fail "hub not at the release tip"
  echo "ok: REACH_PUBLISH_AGENT_RELEASE='${flag:-unset}' builds and publishes no agent"
done

if run 2; then fail "REACH_PUBLISH_AGENT_RELEASE=2 accepted"; fi
if grep -q 'bash -s' "$log"; then fail "invalid flag still reached the hub"; fi
echo "ok: invalid REACH_PUBLISH_AGENT_RELEASE refused"

if run 1; then fail "publish without a signing key succeeded"; fi
grep -q 'release signing key not found' "$work/out" || fail "publish=1 did not check the key"
if grep -q 'bash -s' "$log"; then fail "publish=1 without key reached the hub"; fi
echo "ok: REACH_PUBLISH_AGENT_RELEASE=1 still requires the signing key"

# Only the release branch may deploy; REACH_ALLOW_BRANCH no longer exists.
gitc checkout -q -b some-feature
if REACH_ALLOW_BRANCH=some-feature run ""; then fail "deploy from a non-release branch accepted"; fi
grep -q "deploy only from $release" "$work/out" || fail "non-release branch not explained"
if grep -q 'bash -s' "$log"; then fail "non-release branch reached the hub"; fi
echo "ok: only $release deploys (REACH_ALLOW_BRANCH ignored)"

# Another branch under release/ is refused too.
gitc checkout -q -b release/other
if run ""; then fail "another release/* branch accepted"; fi
grep -q "deploy only from $release" "$work/out" || fail "release/other not explained"
echo "ok: other release/* branches refused"

# Detached HEAD is refused.
gitc checkout -q --detach "$release"
if run ""; then fail "detached HEAD accepted"; fi
grep -q 'not detached HEAD' "$work/out" || fail "detached HEAD not explained"
echo "ok: detached HEAD refused"

# An older commit of the release branch (an ancestor of the pushed tip) is refused.
gitc checkout -q "$release"
old="$(git -C "$work/src" rev-parse HEAD)"
gitc commit -qm "newer release commit" --allow-empty
gitc push -q origin "$release"
gitc reset -q --hard "$old"
if run ""; then fail "stale ancestor of the release tip accepted"; fi
grep -q 'is not the tip of origin' "$work/out" || fail "stale ancestor not explained"
if grep -q 'bash -s' "$log"; then fail "stale ancestor reached the hub"; fi
echo "ok: older release commit refused (exact tip required)"

# The hub re-checks: if the tip moves after the Mac check, the hub refuses
# the commit it was sent and never starts deploy-jason.sh.
gitc reset -q --hard "origin/$release"
git -C "$work/mover" pull -q --ff-only
if TEST_MOVE_TIP=1 run ""; then fail "hub accepted a commit that is no longer the release tip"; fi
grep -q 'hub sees origin/' "$work/out" || fail "hub tip check not explained"
[ ! -s "$work/remote-env" ] || fail "deploy-jason.sh started after the hub tip check failed"
echo "ok: the hub requires the exact release tip too"

# deploy-jason.sh run directly on the hub applies the same gate and never
# moves a detached HEAD to main.
direct() {
  (cd "$hub/reach" && PATH="$bin:$PATH" HOME="$hub" XDG_RUNTIME_DIR="$work/run" bash scripts/deploy-jason.sh) > "$work/out" 2>&1
}
git -C "$hub/reach" fetch -q origin
git -C "$hub/reach" checkout -q --detach "origin/$release~1"
before="$(git -C "$hub/reach" rev-parse HEAD)"
if direct; then fail "direct deploy-jason.sh accepted a stale release commit"; fi
grep -q 'is not the tip of origin' "$work/out" || fail "direct stale commit not explained"
[ "$(git -C "$hub/reach" rev-parse HEAD)" = "$before" ] || fail "direct deploy-jason.sh moved HEAD"
if git -C "$hub/reach" symbolic-ref -q HEAD >/dev/null; then fail "direct deploy-jason.sh switched to a branch"; fi
echo "ok: direct deploy-jason.sh refuses a stale commit and leaves a detached HEAD alone"

git -C "$hub/reach" checkout -q -b behind "origin/$release"
git -C "$work/mover" pull -q --ff-only
git -C "$work/mover" -c user.name=t -c user.email=t@invalid commit -qm "tip moves again" --allow-empty
git -C "$work/mover" push -q origin HEAD
if direct; then fail "direct deploy-jason.sh accepted a checkout behind the release tip"; fi
grep -q 'is not the tip of origin' "$work/out" || fail "direct behind-tip not explained"
echo "ok: direct deploy-jason.sh requires the exact release tip"

# The gate itself passes at the exact tip.
git -C "$hub/reach" checkout -q --detach "origin/$release"
(cd "$hub/reach" && . scripts/release-gate.sh && require_release_head "[t]") > "$work/out" 2>&1 || fail "gate refused the exact tip"
echo "ok: the gate accepts the exact tip"
echo "PASS: deploy-from-mac publication and release gate"
