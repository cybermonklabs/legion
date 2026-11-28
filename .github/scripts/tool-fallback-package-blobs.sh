#!/usr/bin/env bash
# The package's copy of every mirrored row of one machine, read before the fault: each row's blob
# is asked for with no credential at all, at the address .github/scripts/ci-tools.sh builds, and a
# row the registry answers anything but 200 for fails the job, because a pass recorded for a
# second source that is not there is a pass for nothing.
#
# Who runs it, and what it takes:
#
#   .github/scripts/tool-fallback-package-blobs.sh <x86_64|aarch64>
#
# The steps "Check the package holds the blob of every x86_64 mirrored row" and "Check the package
# holds the blob of every aarch64 mirrored row" of .github/workflows/ci-job-tool-fallback.yaml's
# tool-fallback job run it, one per machine, as the filler runs one per machine in the ci-images
# job: the argument names the machine whose ghcr-mirror rows it reads, with ci_tools_mirror_rows,
# the one reading of that split both sides make, which refuses a row built for neither machine by
# name and a machine no row is built for, so every ghcr-mirror row is checked by exactly one of
# the two steps or both fail (.github/ci-budgets.md, "The ci-images job's ceiling and the tool
# copy", says why the reads are split).
# .github/ci-notes/ci-job-tool-fallback.md#why-the-packages-blob-is-read-for-every-mirrored-row says
# why a cold row is a refusal here where the filler leaves one green. That job checks this
# repository out in its first step, above every step below it, so this script is in the checkout
# when the step runs.
#
# Run from the repository root: .github/scripts/tool-fallback-package-blobs.sh x86_64
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the two steps above.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "::error::tool-fallback-package-blobs.sh takes the machine whose rows it reads:" \
    ".github/scripts/tool-fallback-package-blobs.sh <x86_64|aarch64>. It was given: $*" >&2
  exit 1
fi
machine=$1

# shellcheck source=.github/scripts/ci-tools.sh
. .github/scripts/ci-tools.sh
registry=$(ci_tools_registry)
rows=$(ci_tools_mirror_rows "$machine")
token=$(ci_tools_ghcr_token)
cold=""
for row in $rows; do
  url=$(ci_tools_blob_url "$row")
  # The HEAD every reader of the package makes (ci_tools_registry_head): the bearer on
  # standard input, in no process's command line, and with the first request alone; with
  # `--follow` a redirect to the store that holds the blob is read with no token, since
  # an install reads the blob there too. Each of its requests rides the ladder an install's
  # read of the same blob rides, so this script holds no attempts of its own
  # (dispatch://INFRA-1545, R2). It reads the status itself, which is the question here: a 404
  # is this step's answer about a cold row, not a fetch that failed.
  status=$(ci_tools_registry_head --follow "$token" /dev/null "$url")
  echo "$row: $registry answers $status for $url"
  [ "$status" = 200 ] || cold="$cold $row"
done
if [ -n "$cold" ]; then
  echo "::error::the package answered no 200 for the blob of$cold (each answer is above), so this job would record a pass for a second source that is not there; the ci-images job this one needs fills it with .github/scripts/mirror-ci-tools.sh, where a cold row stays green on purpose" >&2
  exit 1
fi
