#!/usr/bin/env bash
# Tests for `tool-fallback-nowhere-controls.sh`'s hold on the base tree: the negative control
# reads a failed install as one with nowhere to go, so it must first refuse a base tree that does
# not hold the action an install runs, whose step fails having installed nothing. A tree with all
# seven passes; a tree without install-elan, as graph/infra-1150-decision-schema is at 8aa92075
# (the base branch of #2804 and #2756), is refused naming it; a tree without three is refused
# naming each.
#
# Each case runs the script from this repository's root, as the tool-fallback job does, with
# BASE_TREE at a tree this harness builds and every install's outcome `failure` in its own time,
# so the hold on the base tree is the one thing that differs between the cases.
#
# Run from anywhere: .github/scripts/tool-fallback-nowhere-controls.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
# One removal, of this harness's own root by its exact path.
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

seven=(install-crane install-changie install-jj setup-bun install-elan install-uv install-pulumi)

# base_tree <name> <action>...: a tree under this harness's root holding those actions alone.
base_tree() {
  local tree="$work/$1" action
  shift
  mkdir -p "$tree/.github/actions"
  for action in "$@"; do
    mkdir -p "$tree/.github/actions/$action"
    printf 'name: %s\nruns:\n  using: composite\n  steps: []\n' "$action" > "$tree/.github/actions/$action/action.yml"
  done
  echo "$tree"
}

# run_controls <tree>: the script over that base tree, every install failed, each mark a second
# after the one before it and the last a second ago; sets `out` and `status`.
run_controls() {
  local now
  now=$(date +%s)
  set +e
  out=$(cd "$script_dir/../.." && env BASE_TREE="$1" \
    CRANE=failure CHANGIE=failure JJ=failure BUN=failure ELAN=failure UV=failure PULUMI_DOCKER=failure PULUMI_CLI=failure \
    CRANE_AT=$((now - 8)) CHANGIE_AT=$((now - 7)) JJ_AT=$((now - 6)) BUN_AT=$((now - 5)) ELAN_AT=$((now - 4)) \
    UV_AT=$((now - 3)) PULUMI_DOCKER_AT=$((now - 2)) PULUMI_CLI_AT=$((now - 1)) \
    .github/scripts/tool-fallback-nowhere-controls.sh 2>&1)
  status=$?
  set -e
}

echo "case: a base tree that holds the seven actions passes (control)"
run_controls "$(base_tree whole "${seven[@]}")"
check "exits 0" "$(is "$status" 0)"
check "says the base tree holds the seven" "$(contains "$out" 'holds the seven actions these installs run')"
check "and reads every install's outcome" "$(contains "$out" 'the install with nowhere to go for pulumi-cli ended in failure')"

echo "case: a base tree without install-elan is refused, though every install failed"
run_controls "$(base_tree no-elan install-crane install-changie install-jj setup-bun install-uv install-pulumi)"
check "exits 1" "$(is "$status" 1)"
check "names the action the tree does not have" "$(contains "$out" 'has no action for install-elan, so the install step that runs it failed before installing anything')"
check "and reads no outcome as a pass" "$(is "$(contains "$out" 'ended in failure')" false)"

echo "case: a base tree without three actions is refused naming each"
run_controls "$(base_tree no-three install-jj setup-bun install-uv install-pulumi)"
check "exits 1" "$(is "$status" 1)"
check "names all three" "$(contains "$out" 'has no action for install-crane install-changie install-elan,')"

summary "tool-fallback-nowhere-controls.sh's hold on the base tree"
