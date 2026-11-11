#!/usr/bin/env bash
# The Oh My Pi plugin pair this run's legion links: for each plugin, the version npm carries once
# this run is through, which is the version that plugin's build job releases or, when it releases
# nothing, the version of its newest tag. It writes the two as `pi_legion_version` and
# `pi_envoy_version` on $GITHUB_OUTPUT, and fails when either cannot be resolved.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-plugin-versions.sh
#
# No arguments. It reads two keys of release.yaml's workflow `env:`, $PI_ENVOY_VERSION_ARGS and
# $PI_LEGION_VERSION_ARGS, each the argument list of that plugin's own version step, and writes to
# $GITHUB_OUTPUT. It runs release-push.sh by its path in this checkout, so the working directory is
# the repository root.
#
# Run from the repository root: .github/scripts/release-plugin-versions.sh
# CI runs it in release.yaml's legion_build job, at the step "Resolve plugin versions for the smoke
# test and worker image".
set -euo pipefail

# pair_member <version output> <tag prefix>: the version npm carries for the plugin in
# this run. This step's own dispatch is which that is: the version the plugin releases,
# or, when it skips, nothing, and release-push.sh pair-version then reads the version of
# its newest tag, which is what npm carries. A newest tag whose name is not <tag
# prefix>X.Y.Z, and no such tag at all, print nothing, which the guards below refuse.
pair_member() {
  local released=''
  if [ "$(sed -n 's/^skip=//p' <<<"$1")" != true ]; then
    released=$(sed -n 's/^version=//p' <<<"$1")
  fi
  .github/scripts/release-push.sh pair-version "$2" "$released"
}
# The plugins' own build jobs write their "No release published" summary lines; with
# GITHUB_STEP_SUMMARY empty these two calls write none into this job's summary.
# shellcheck disable=SC2086 # $PI_ENVOY_VERSION_ARGS is the argument list, split on purpose
envoy=$(GITHUB_STEP_SUMMARY='' .github/scripts/release-push.sh version pi-envoy-v $PI_ENVOY_VERSION_ARGS)
# shellcheck disable=SC2086 # $PI_LEGION_VERSION_ARGS is the argument list, split on purpose
legion=$(GITHUB_STEP_SUMMARY='' .github/scripts/release-push.sh version pi-legion-v $PI_LEGION_VERSION_ARGS)
envoy_version=$(pair_member "$envoy" pi-envoy-v)
legion_version=$(pair_member "$legion" pi-legion-v)
if [ -z "$legion_version" ]; then
  echo "::error::Could not resolve @sjawhar/pi-legion version"
  exit 1
fi
if [ -z "$envoy_version" ]; then
  echo "::error::Could not resolve @sjawhar/pi-envoy version"
  exit 1
fi
{
  echo "pi_legion_version=$legion_version"
  echo "pi_envoy_version=$envoy_version"
} >> "$GITHUB_OUTPUT"
