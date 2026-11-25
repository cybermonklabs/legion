#!/usr/bin/env bash
# What .github/actions/install-skill-cuts-omp left on this runner, read against the
# skill-cuts-omp row of .github/ci-tools.yaml: the Oh My Pi it unpacked answers the row's
# version, and the native addon that binary extracted, in the directory the action exported as
# SKILL_CUTS_NATIVES_DIR, is one of the two the skill-cuts runner opens, by its SHA-256
# (PINNED_ADDON_DIGESTS of packages/pi-legion/scripts/skill-cuts/natives.ts, read here rather than
# written again). The action held the archive to the row's digest before it unpacked anything,
# so this reads what came out of it.
# With `--expect-refused <host>` it also reads which source served those bytes: the fault proxy
# must have refused a request to that host since the probe before the install marked its log,
# the 504 that sent the install to the ghcr copy (`ci-tools-proof.sh refusals`). A version and a
# digest cannot say it, because the release asset and the copy are one archive by design.
#
# Who runs it: the steps "Read the skill-cuts Oh My Pi the release asset served" (before the
# fault, no argument) and "Read the skill-cuts Oh My Pi the copy served" (under the fault,
# `--expect-refused github.com`) of .github/workflows/ci-job-tool-fallback.yaml's tool-fallback
# job; .github/ci-notes/ci-job-tool-fallback.md#why-the-skill-cuts-omp-row-is-installed-twice
# says why the row is installed twice. That job checks this repository out in its first step,
# so this script, natives.ts and the rig it runs are in the checkout when either step runs.
#
# Run from the repository root: SKILL_CUTS_NATIVES_DIR=<dir> .github/scripts/tool-fallback-skill-cuts-omp.sh [--expect-refused <host>]
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the two steps named above.
set -euo pipefail

proof=.github/scripts/ci-tools-proof.sh
natives_ts=packages/pi-legion/scripts/skill-cuts/natives.ts
refused=""
case "$#" in
  0) ;;
  2)
    [ "$1" = --expect-refused ] || { echo "::error::tool-fallback-skill-cuts-omp: '$1' is no option of this script; it takes --expect-refused <host> or nothing" >&2; exit 1; }
    refused=$2
    ;;
  *) echo "::error::tool-fallback-skill-cuts-omp: this script takes --expect-refused <host> or nothing, and this call gave $# arguments" >&2; exit 1 ;;
esac
dir="${SKILL_CUTS_NATIVES_DIR:-}"
[ -n "$dir" ] || { echo "::error::tool-fallback-skill-cuts-omp: SKILL_CUTS_NATIVES_DIR is not set, so the install wrote no natives directory for this step to read" >&2; exit 1; }
wrong=""
omp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/skill-cuts-omp/bin/omp"
printed=$(HOME="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/skill-cuts-omp/home" "$omp" --version 2>&1 || true)
"$proof" version-answers skill-cuts-omp "$printed" || wrong="$wrong the version"
# The two digests the runner opens, as natives.ts writes them: one line each, `<digest>: "<variant>",`.
pinned=$(sed -nE 's/^[[:space:]]*"?([0-9a-f]{64})"?: "(modern|baseline)",$/\1/p' "$natives_ts")
[ "$(printf '%s\n' "$pinned" | grep -c .)" -eq 2 ] ||
  { echo "::error::tool-fallback-skill-cuts-omp: $natives_ts holds $(printf '%s\n' "$pinned" | grep -c .) pinned addon digests where PINNED_ADDON_DIGESTS holds two, so this step has nothing to hold the addon to" >&2; exit 1; }
found=0
for addon in "$dir"/18.18.6/pi_natives.linux-x64-*.node; do
  [ -f "$addon" ] || continue
  found=$((found + 1))
  digest=$(sha256sum -- "$addon")
  digest=${digest%% *}
  echo "$addon has sha256 $digest"
  printf '%s\n' "$pinned" | grep -qx -- "$digest" || wrong="$wrong $(basename "$addon")"
done
[ "$found" -gt 0 ] || wrong="$wrong the addon (none at $dir/18.18.6)"
if [ -n "$wrong" ]; then
  echo "::error::the skill-cuts Oh My Pi this install left is not what its row and natives.ts pin:$wrong; the pinned addon digests are $(printf '%s\n' "$pinned" | paste -sd ' ')" >&2
  exit 1
fi
echo "the skill-cuts Oh My Pi answers its row's version and its addon is one natives.ts opens"
[ -z "$refused" ] || "$proof" refusals "$refused"
