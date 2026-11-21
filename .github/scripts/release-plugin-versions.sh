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
# The runner sets $GITHUB_OUTPUT for every step.
#
# Run from the repository root, with the two lists and $GITHUB_OUTPUT set:
#   .github/scripts/release-plugin-versions.sh
# CI runs it in release.yaml's legion_build job, at the step "Resolve plugin versions for the smoke
# test and worker image".
set -euo pipefail

# released_of <version output>: what this step's own dispatch says the plugin releases
# in this run, which is its version, or, when it skips, nothing. release-push.sh
# pair-version <tag prefix> turns that into the version npm carries for the plugin:
# the version it is given, or, given nothing, the version of the plugin's newest tag,
# which is what npm carries. A newest tag whose name is not <tag prefix>X.Y.Z, and no
# such tag at all, print nothing, which the guards below refuse. Each pair-version
# call is written where its version is resolved rather than inside a function, so
# .github/scripts/check-download-retry.sh reads both as runs this step waits for
# (dispatch://INFRA-1545, round 37).
released_of() {
  if [ "$(sed -n 's/^skip=//p' <<<"$1")" != true ]; then
    sed -n 's/^version=//p' <<<"$1"
  fi
}
# The plugins' own build jobs write their "No release published" summary lines; with
# GITHUB_STEP_SUMMARY empty these two calls write none into this job's summary.
# shellcheck disable=SC2086 # $PI_ENVOY_VERSION_ARGS is the argument list, split on purpose
envoy=$(GITHUB_STEP_SUMMARY='' .github/scripts/release-push.sh version pi-envoy-v $PI_ENVOY_VERSION_ARGS)
# shellcheck disable=SC2086 # $PI_LEGION_VERSION_ARGS is the argument list, split on purpose
legion=$(GITHUB_STEP_SUMMARY='' .github/scripts/release-push.sh version pi-legion-v $PI_LEGION_VERSION_ARGS)
envoy_version=$(.github/scripts/release-push.sh pair-version pi-envoy-v "$(released_of "$envoy")")
legion_version=$(.github/scripts/release-push.sh pair-version pi-legion-v "$(released_of "$legion")")
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
