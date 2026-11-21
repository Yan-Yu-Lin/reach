# shellcheck shell=bash
# The one branch Reach deploys from (daybreak round 6 R6-P1-01).
#
# Sourced by deploy-from-mac.sh and deploy-jason.sh. There is deliberately no
# override: tests use a throwaway repository whose branch has this same name.
# A later release changes this constant in a commit on the new release branch.
REACH_RELEASE_BRANCH="release/reach-2026-11"

# fetch_release_tip: fetch exactly refs/heads/$REACH_RELEASE_BRANCH from origin
# (no tags, no other branches) and print the commit it points at.
fetch_release_tip() {
  git fetch --quiet --no-tags origin \
    "+refs/heads/${REACH_RELEASE_BRANCH}:refs/remotes/origin/${REACH_RELEASE_BRANCH}" >&2 || return 1
  git rev-parse --verify --quiet "refs/remotes/origin/${REACH_RELEASE_BRANCH}^{commit}"
}

# require_release_head PREFIX: HEAD must be exactly the release tip on origin
# right now. An older commit of the branch (an ancestor) is refused too.
require_release_head() {
  local prefix="$1" head tip
  head="$(git rev-parse --verify --quiet 'HEAD^{commit}')" || {
    echo "${prefix} ERROR: no HEAD commit" >&2; return 1; }
  tip="$(fetch_release_tip)" || {
    echo "${prefix} ERROR: cannot fetch origin/${REACH_RELEASE_BRANCH}" >&2; return 1; }
  if [ "$head" != "$tip" ]; then
    echo "${prefix} ERROR: HEAD ${head} is not the tip of origin/${REACH_RELEASE_BRANCH} (${tip}); deploy only the pushed release tip" >&2
    return 1
  fi
}
