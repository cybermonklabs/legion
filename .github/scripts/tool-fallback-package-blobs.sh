#!/usr/bin/env bash
# The package's copy of every mirrored row, read before the fault: each row's blob is asked for
# with no credential at all, at the address .github/scripts/ci-tools.sh builds, and a row the
# registry answers anything but 200 for fails the job, because a pass recorded for a second source
# that is not there is a pass for nothing.
#
# Who runs it: the step "Check the package holds the blob of every mirrored row" of
# .github/workflows/ci-job-tool-fallback.yaml's tool-fallback job;
# .github/ci-notes/ci-job-tool-fallback.md#why-the-packages-blob-is-read-for-every-mirrored-row says
# why a cold row is a refusal here where the filler leaves one green. It takes no argument. That
# job checks
# this repository out in its first step, above every step below it, so this script is in the
# checkout when the step runs.
#
# Run from the repository root: .github/scripts/tool-fallback-package-blobs.sh
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the step "Check the package
# holds the blob of every mirrored row".
set -euo pipefail

# shellcheck source=.github/scripts/ci-tools.sh
. .github/scripts/ci-tools.sh
# ci_tools_registry takes no argument; this step deliberately has no positional input.
# shellcheck disable=SC2119
registry=$(ci_tools_registry)
token=$(ci_tools_ghcr_token)
# ghcr-mirror is ci_tools_rows' explicit optional source selector.
# shellcheck disable=SC2119
rows=$(ci_tools_rows ghcr-mirror)
cold=""
for row in $rows; do
  url=$(ci_tools_blob_url "$row")
  # The HEAD every reader of the package makes (ci_tools_registry_head): the bearer on
  # standard input, in no process's command line, and with the first request alone, so
  # a redirect to the store that holds the blob is followed with no token.
  status=$(ci_tools_registry_head "$token" /dev/null "$url")
  echo "$row: $registry answers $status for $url"
  [ "$status" = 200 ] || cold="$cold $row"
done
if [ -n "$cold" ]; then
  echo "::error::the package answered no 200 for the blob of$cold (each answer is above), so this job would record a pass for a second source that is not there; the ci-images job this one needs fills it with .github/scripts/mirror-ci-tools.sh, where a cold row stays green on purpose" >&2
  exit 1
fi
