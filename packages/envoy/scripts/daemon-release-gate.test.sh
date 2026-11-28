#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.test.sh
#
# Proves daemon-release-gate.sh's verdicts with no network: a fake `gh` answers the stack files, the
# deployment's head and archive, the release tags, the marketplace and the compare calls from files
# this test writes; a fake `docker` answers `buildx imagetools inspect` the way the real one answers
# a single image and an OCI image index (an index's `.Image.Config.Labels` is `null`, and its
# platforms are read one at a time) and `run` from an image's files; and a fake `npm` answers `npm
# pack --dry-run --json`, running the real npm for `npm exec` and where a case asks, so the suite
# needs npm and node. Each records every call, so a case asserts what the gate printed and what
# it asked. The deployment repository is the one `--deployment` names, and each stack's config keys
# carry the project name its own Pulumi.yaml declares; the fake gh answers that repository alone,
# so a gate that read any other would fail every row. Each run's HOME is the case's own, so no row
# reads the machine's dotfiles.
#
# Run one group alone: bash scripts/daemon-release-gate.test.sh <group>
# List the groups: bash scripts/daemon-release-gate.test.sh --groups
# Run every group, one at a time:
#   failed=0; for g in $(bash scripts/daemon-release-gate.test.sh --groups); do bash scripts/daemon-release-gate.test.sh "$g" || failed=1; done; [ "$failed" = 0 ]
# CI queues one pool entry per group. The group runner gives each run its own temporary directory,
# fakes and case roots.
set -euo pipefail

# CPU seconds at main 8e248d6c, run 46127046034 (job 140924069070), longest first:
# versions 19.4, listeners 17.1,
# developer-box-surfaces 16.4, developer-box-records 16.31, developer-box 16.09,
# short-reads 15.58, short-reads-clients 14.99, vm-hub-archive 13.47,
# image-clients 13.44, image-clients-pins 13.39, vm-hub 12.52, daemons 6.5, project-fields 2.1.
# Source: dispatch://INFRA-6359/artifact/case-cpu-at-main-tsv. Split group times sum the cases.
readonly groups=(versions listeners developer-box-surfaces developer-box-records developer-box short-reads short-reads-clients vm-hub-archive image-clients image-clients-pins vm-hub daemons project-fields)
readonly lib="$(dirname "${BASH_SOURCE[0]}")/lib"

is_group() {
  local name
  for name in "${groups[@]}"; do
    [ "$name" != "$1" ] || return 0
  done
  return 1
}

# Check both directions before listing or running a group.
shopt -s nullglob
for file in "$lib"/daemon-release-gate-cases-*.sh; do
  name=${file##*/daemon-release-gate-cases-}
  if ! is_group "${name%.sh}"; then
    printf 'daemon-release-gate.test.sh: unlisted case file: %s\n' "$file" >&2
    exit 2
  fi
done
for name in "${groups[@]}"; do
  file="$lib/daemon-release-gate-cases-$name.sh"
  if [ ! -f "$file" ]; then
    printf 'daemon-release-gate.test.sh: missing case file: %s\n' "$file" >&2
    exit 2
  fi
done

if [ "$#" -eq 1 ] && [ "$1" = --groups ]; then
  printf '%s\n' "${groups[@]}"
  exit 0
fi
if [ "$#" -ne 1 ] || ! is_group "$1"; then
  {
    printf 'usage: bash scripts/daemon-release-gate.test.sh <group|--groups>; groups: %s\n' "${groups[*]}"
    echo 'Run all: failed=0; for g in $(bash scripts/daemon-release-gate.test.sh --groups); do bash scripts/daemon-release-gate.test.sh "$g" || failed=1; done; [ "$failed" = 0 ]'
  } >&2
  exit 2
fi
readonly group=$1

# Its developer-box rows hide a directory with `chmod 000` to show the gate's refusal of what it
# cannot read, and root reads it anyway, so those rows fail as root for no fault of the gate's
# (lib/daemon-release-gate-cases-developer-box-records.sh and -surfaces.sh, "this user cannot
# search the hidden directory").
# Say so once, rather than as a dozen row failures.
if [ "$(id -u)" = 0 ]; then
  echo "daemon-release-gate.test.sh: run it as a user other than root: its rows hide a directory with chmod 000, which root reads anyway" >&2
  exit 1
fi

outputs=$(mktemp -d)
readonly outputs
trap 'rm -rf "$outputs"' EXIT

# now_ms: the clock in milliseconds. A `date` with no %N, which is BSD's, gives whole seconds, so
# a group's time is read in seconds there rather than failing the arithmetic below.
now_ms() {
  local now
  now=$(date +%s%3N)
  [[ $now =~ ^[0-9]+$ ]] || now=$(($(date +%s) * 1000))
  printf '%s\n' "$now"
}

started=$(now_ms)
status=(0 0)
bash "$lib/daemon-release-gate-group.sh" "$group" "$outputs/tally" | tee "$outputs/out" || status=("${PIPESTATUS[@]}")
milliseconds=$(($(now_ms) - started))
[ "${status[1]}" = 0 ] || exit 1
state=${status[0]}

failures=0
tally=$(cat "$outputs/tally" 2>/dev/null || true)
group_cases=$(grep -c '^case: ' "$outputs/out" || true)
group_checks=$(grep -cE '^(PASS|FAIL): ' "$outputs/out" || true)
if [[ ! $tally =~ ^(0|[1-9][0-9]*)$ ]]; then
  # A group that wrote no tally never reached its own last line: its checks are not all in.
  printf 'FAIL: the %s group wrote no count of its failed checks; it ended with exit %s\n' "$group" "$state"
  failures=$((failures + 1))
else
  failures=$tally
  expected=1
  [ "$tally" != 0 ] || expected=0
  if [ "$state" != "$expected" ]; then
    printf 'FAIL: the %s group ended with exit %s after %s failed check(s)\n' "$group" "$state" "$tally"
    failures=$((failures + 1))
  fi
fi
if [ "$group_checks" -eq 0 ]; then
  printf 'FAIL: the %s group ran no check\n' "$group"
  failures=$((failures + 1))
fi
printf 'group %s: %s case(s), %s check(s), %s failed, %s.%ss\n' \
  "$group" "$group_cases" "$group_checks" "${tally:-unknown}" "$((milliseconds / 1000))" "$(((milliseconds % 1000) / 100))"

if [ "$failures" -ne 0 ]; then
  printf '%s check(s) failed\n' "$failures"
  exit 1
fi
echo "all checks passed"
