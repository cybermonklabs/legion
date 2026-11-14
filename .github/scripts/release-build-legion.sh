#!/usr/bin/env bash
# The release archives of the Go `legion`: one per target .github/go-release-targets.json lists,
# each holding that target's static binary and its THIRD_PARTY_NOTICES, packed into
# assets/<target>.tar.gz for the publish job to upload.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-build-legion.sh
#
# No arguments. It reads four variables of the step's environment: $VERSION, the version this run
# releases, and $PI_LEGION_VERSION and $PI_ENVOY_VERSION, the plugin pair this binary links, which
# the step's `env:` carries from the version and plugin_versions steps, and $GITHUB_SHA and
# $GITHUB_WORKSPACE, which the runner sets. It reads .github/go-release-targets.json and writes
# out/ and assets/ of the checkout, so the working directory is the repository root.
#
# Run from the repository root: .github/scripts/release-build-legion.sh
# CI runs it in release.yaml's legion_build job, at the step "Build legion".
set -euo pipefail

mkdir -p out assets
for target in $(jq -r '.legion[]' .github/go-release-targets.json); do
  os=${target%/*}
  arch=${target#*/}
  dir="legion-$os-$arch"
  mkdir -p "out/$dir"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go -C packages/daemon build -buildvcs=false \
    -ldflags "-X main.revision=$GITHUB_SHA -X main.release=v$VERSION -X main.piLegionVersion=$PI_LEGION_VERSION -X main.piEnvoyVersion=$PI_ENVOY_VERSION" \
    -o "$GITHUB_WORKSPACE/out/$dir/legion" ./cmd/legion
  (cd packages/daemon && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    ../../scripts/go-third-party-notices.sh "$GITHUB_WORKSPACE/out/$dir/THIRD_PARTY_NOTICES" ./cmd/legion)
  tar czf "assets/$dir.tar.gz" -C out "$dir"
done
