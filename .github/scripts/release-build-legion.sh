#!/usr/bin/env bash
# The release archives of the Go `legion`: one per target .github/go-release-targets.json lists,
# each holding that target's static binary and its THIRD_PARTY_NOTICES, packed into
# assets/legion-<os>-<arch>.tar.gz for the publish job to upload.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-build-legion.sh
#
# No arguments. It reads six variables of the step's environment: $VERSION, the version this run
# releases, $PI_LEGION_VERSION and $PI_ENVOY_VERSION, the plugin pair this binary links, and
# $LEGION_CONTRACT_ORDER, the stamp of this commit's contract entries in main's merge order
# (scripts/daemon-contract-order.sh --stamp), which the step's `env:` carries from the version,
# plugin_versions and contract_order steps, and $GITHUB_SHA and $GITHUB_WORKSPACE, which the
# runner sets. It reads .github/go-release-targets.json and writes out/ and assets/ of the
# checkout, so the working directory is the repository root.
#
# The stamp is linked in as api.contractOrder, from which the daemon numbers each entry by its
# rank, so a released daemon gives each entry the number every older build of main gave it. It
# refuses to build without one: an unstamped binary numbers its entries by id instead, and the
# stamp of a commit of no entry is `v1:`, never an empty value.
#
# Run from the repository root: .github/scripts/release-build-legion.sh
# CI runs it in release.yaml's legion_build job, at the step "Build legion".
set -euo pipefail

: "${LEGION_CONTRACT_ORDER:?is unset or empty: set it to the stamp scripts/daemon-contract-order.sh --stamp prints for this commit}"

mkdir -p out assets
for target in $(jq -r '.legion[]' .github/go-release-targets.json); do
  os=${target%/*}
  arch=${target#*/}
  dir="legion-$os-$arch"
  mkdir -p "out/$dir"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go -C packages/daemon build -buildvcs=false \
    -ldflags "-X main.revision=$GITHUB_SHA -X main.release=v$VERSION -X main.piLegionVersion=$PI_LEGION_VERSION -X main.piEnvoyVersion=$PI_ENVOY_VERSION -X github.com/sjawhar/legion/daemon/internal/api.contractOrder=$LEGION_CONTRACT_ORDER" \
    -o "$GITHUB_WORKSPACE/out/$dir/legion" ./cmd/legion
  (cd packages/daemon && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    ../../scripts/go-third-party-notices.sh "$GITHUB_WORKSPACE/out/$dir/THIRD_PARTY_NOTICES" ./cmd/legion)
  tar czf "assets/$dir.tar.gz" -C out "$dir"
done
