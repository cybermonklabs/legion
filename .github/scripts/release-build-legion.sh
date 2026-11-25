#!/usr/bin/env bash
# The release archives of the Go `legion`: one per target .github/go-release-targets.json lists,
# each holding that target's static binary and its THIRD_PARTY_NOTICES, packed into
# assets/legion-<os>-<arch>.tar.gz for the publish job to upload.
#
# This script is where the release says with which environment and command legion is built for a
# target. Its modes answer those questions from the same functions its build runs, so a reader
# asks it rather than keeping a copy. Which targets it builds is release-push.sh legion-targets,
# beside this script, which the build runs and a reader runs itself:
#
#   .github/scripts/release-build-legion.sh
#   .github/scripts/release-build-legion.sh list <os>/<arch> <go list flag>...
#   .github/scripts/release-build-legion.sh env <os>/<arch> <go env argument>...
#   .github/scripts/release-build-legion.sh modules <os>/<arch>
#
# No argument: the build. It reads six variables of the step's environment: $VERSION, the
# version this run releases, $PI_LEGION_VERSION and $PI_ENVOY_VERSION, the plugin pair this
# binary links, and $LEGION_CONTRACT_ORDER, the stamp of this commit's contract entries in main's
# merge order (scripts/daemon-contract-order.sh --stamp), which the step's `env:` carries from the
# version, plugin_versions and contract_order steps, and $GITHUB_SHA and $GITHUB_WORKSPACE, which
# the runner sets. It reads the targets and writes out/ and assets/ of the checkout, and stops,
# building nothing, when release-push.sh legion-targets refuses the list.
#
# The stamp is linked in as api.contractOrder, from which the daemon numbers each entry by its
# rank, so a released daemon gives each entry the number every older build of main gave it. The
# build refuses to start without one, before it reads the targets: an unstamped binary numbers its
# entries by id instead, and the stamp of a commit of no entry is `v1:`, never an empty value.
#
# list: `go list <go list flag>... ./cmd/legion` in packages/daemon, with the environment the build
# gives <os>/<arch>: what that target's binary is built from.
# env: `go env <go env argument>...` in packages/daemon with that environment, such as GOWORK, the
# go.work that target's build reads.
# modules: `go list -m` in packages/daemon with that environment, the go.mod of each main module:
# in workspace mode each module the go.work uses, whose require lines all feed the one version
# selection the binary is built with, whether or not a package of it is linked.
# The modes read none of the build's six variables, $LEGION_CONTRACT_ORDER among them, and write
# nothing. A mode the script does not know, `list`, `env` or `modules` without a target, and
# `modules` with more than its target exit 2 with the usage.
#
# Run from the repository root.
# CI runs the build in release.yaml's legion_build job, at the step "Build legion", and
# .github/scripts/check-legion-release-paths.sh runs its modes in ci.yaml's lint job.
set -euo pipefail

usage='usage: release-build-legion.sh [list <os>/<arch> <go list flag>... | env <os>/<arch> <go env argument>... | modules <os>/<arch>]'

# for_target <os>/<arch> <command>...: run <command> with the environment the build gives that
# target: no cgo, so the binary is static, and the target's GOOS and GOARCH.
for_target() {
  local target=$1
  shift
  CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" "$@"
}

build() {
  local list target dir
  local -a each
  # The stamp, here and not at the top: the modes, which lint runs, have none and need none.
  : "${LEGION_CONTRACT_ORDER:?is unset or empty: set it to the stamp scripts/daemon-contract-order.sh --stamp prints for this commit}"
  # release-push.sh's failure is the assignment's, so the build stops on a list it refuses.
  list=$("$(dirname -- "${BASH_SOURCE[0]}")/release-push.sh" legion-targets)
  mkdir -p out assets
  mapfile -t each <<<"$list"
  for target in "${each[@]}"; do
    dir="legion-${target%/*}-${target#*/}"
    mkdir -p "out/$dir"
    for_target "$target" go -C packages/daemon build -buildvcs=false \
      -ldflags "-X main.revision=$GITHUB_SHA -X main.release=v$VERSION -X main.piLegionVersion=$PI_LEGION_VERSION -X main.piEnvoyVersion=$PI_ENVOY_VERSION -X github.com/sjawhar/legion/daemon/internal/api.contractOrder=$LEGION_CONTRACT_ORDER" \
      -o "$GITHUB_WORKSPACE/out/$dir/legion" ./cmd/legion
    (cd packages/daemon && for_target "$target" \
      ../../scripts/go-third-party-notices.sh "$GITHUB_WORKSPACE/out/$dir/THIRD_PARTY_NOTICES" ./cmd/legion)
    tar czf "assets/$dir.tar.gz" -C out "$dir"
  done
}

refuse() {
  echo "::error::release-build-legion.sh: $1; $usage" >&2
  exit 2
}

if [ $# -eq 0 ]; then
  build
  exit 0
fi
case $1 in
  list | env | modules)
    [ $# -ge 2 ] || refuse "$1 needs a target"
    mode=$1 target=$2
    shift 2
    if [ "$mode" = list ]; then
      for_target "$target" go -C packages/daemon list "$@" ./cmd/legion
    elif [ "$mode" = env ]; then
      for_target "$target" go -C packages/daemon env "$@"
    else
      [ $# -eq 0 ] || refuse "modules takes a target alone"
      for_target "$target" go -C packages/daemon list -m -f '{{.GoMod}}'
    fi
    ;;
  *) refuse "unknown mode $1" ;;
esac
