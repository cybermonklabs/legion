#!/usr/bin/env bash
# Tests for `ci-tools-proof.sh ladder-report`: the refusal that reads how many records of the
# stand-in curl's log carried CI_TOOLS_PROOF_FULL_LADDER, and holds that count to LADDER_RECORDS
# at the top of that script.
#
# No case reaches the network. Each case writes a curl.log of its own and runs the verb over it,
# so no case starts a server and none reads a database. Every line of such a log is the
# five-field line the stand-in curl of the shorten-ladder step writes, one line for each fetch
# it was handed.
#
# What this file covers is that one refusal, in four readings: a legitimate log passes, a log
# with one record more than the bound is refused, a log whose every record carries the name is
# refused, and a log with no such record still refuses for the reason it refused for before.
# The verb's other refusals stay driven by the `tool-fallback` job of .github/workflows/ci.yaml,
# which runs this verb over the log of its own run: the window the one kept case must measure
# inside, a second name that kept the pinned ladder, and a fetch that was shortened and then
# served the bytes.
#
# Run from anywhere: .github/scripts/ci-tools-proof.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
rig="$script_dir/ci-tools-proof.sh"
work=$(mktemp -d)
# One removal, of this harness's own root by its exact path. Every case directory is made under
# it, so nothing else on this machine's temporary directory is touched.
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# repeat <count> <line>: that line, <count> times, one per line.
repeat() {
  local i
  for ((i = 0; i < $1; i++)); do printf '%s\n' "$2"; done
}

# legitimate_log: the 65 records of a run that must pass, in the order the listing holds them.
# This is the shape merge_group run 42975857631, job 130374882879 (`tool-fallback`, success,
# head 9300f28b1c) read on a runner: 18 refusals of the faulted release host, then the three
# records of the one case that keeps the pinned ladder, then 11 refusals of the dead address,
# then the 33 fetches of the npm copy. Every case below is built from this one log.
legitimate_log() {
  repeat 18 "seconds=0 decision=shortened host=github.com exit=22 full-ladder=none"
  printf '%s\n' \
    "seconds=63 decision=full host=github.com exit=22 full-ladder=crane-head" \
    "seconds=0 decision=full host=ghcr.io exit=0 full-ladder=crane-head" \
    "seconds=1 decision=full host=ghcr.io exit=0 full-ladder=crane-head"
  repeat 11 "seconds=0 decision=shortened host=127.0.0.1 exit=7 full-ladder=none"
  repeat 33 "seconds=1 decision=full host=registry.npmjs.org exit=0 full-ladder=none"
}

# run_report <log>: makes a directory of its own with mktemp under this harness's root, writes
# the log it is handed where the stand-in curl writes one, and runs the verb over it with that
# directory as RUNNER_TEMP. It sets `out` to the combined output and `status` to the exit.
run_report() {
  local case_dir
  case_dir=$(mktemp -d "$work/case.XXXXXX")
  mkdir -p "$case_dir/ci-tools-proof"
  printf '%s\n' "$1" > "$case_dir/ci-tools-proof/curl.log"
  set +e
  out=$(RUNNER_TEMP="$case_dir" "$rig" ladder-report 2>&1)
  status=$?
  set -e
}

# error_count <output>: how many `::error::` lines a run printed, which is how a case says that
# no refusal spoke beside the ones it names.
error_count() { grep -c -- '::error::' <<<"$1" || true; }

closing="every fetch this job shortened is one it arranged to fail, and the one case that kept the pinned ladder measured it inside the window"

log=$(legitimate_log)

echo "case: a legitimate log passes"
run_report "$log"
check "exits 0" "$(is "$status" 0)"
check "prints the count it read beside the bound" \
  "$(contains "$out" "3 of those fetches carried CI_TOOLS_PROOF_FULL_LADDER, and this rig holds that count to 3")"
check "prints the closing sentence" "$(contains "$out" "$closing")"

echo "case: one record more than the bound is refused"
fourth="seconds=0 decision=full host=ghcr.io exit=0 full-ladder=crane-head"
run_report "$log"$'\n'"$fourth"
check "exits 1" "$(is "$status" 1)"
check "names the count it read and the bound" \
  "$(contains "$out" "4 fetches of this job carried CI_TOOLS_PROOF_FULL_LADDER, over the 3 this rig holds that count to, and the names they carried it under are: crane-head")"
check "prints no closing sentence" "$(is "$(contains "$out" "$closing")" false)"
check "no other refusal speaks" "$(is "$(error_count "$out")" 1)"

echo "case: a log whose every record carries the name is refused"
# The log dispatch://INFRA-2850 was filed against. Before the count refusal the verb read one
# name and a measurement inside the window here, passed, and printed a closing sentence that is
# false of this log: every one of these 65 fetches kept the pinned ladder and none was shortened.
run_report "$(repeat 65 "seconds=63 decision=full host=github.com exit=22 full-ladder=crane-head")"
check "exits 1" "$(is "$status" 1)"
check "names the count it read and the bound" \
  "$(contains "$out" "65 fetches of this job carried CI_TOOLS_PROOF_FULL_LADDER, over the 3 this rig holds that count to, and the names they carried it under are: crane-head")"
check "prints no closing sentence" "$(is "$(contains "$out" "$closing")" false)"
check "no other refusal speaks" "$(is "$(error_count "$out")" 1)"

echo "case: a log with no record of that name still refuses for the old reason"
# The legitimate log with one field changed on every record and nothing else. A run where the
# variable reaches no step writes a log of another shape as well, since the stand-in then
# shortens every fetch of a faulted host; this fixture holds the records where they were, so
# that the count is the one thing between it and the case above.
run_report "${log//full-ladder=crane-head/full-ladder=none}"
check "exits 1" "$(is "$status" 1)"
check "the refusal that was there before speaks" \
  "$(contains "$out" "no fetch of this job carried CI_TOOLS_PROOF_FULL_LADDER")"
check "the count refusal stays silent" "$(is "$(contains "$out" "fetches of this job carried")" false)"
# The 63 s record of the kept case now carries no name either, so the verb reads it as a fetch
# that stayed on the pinned ladder and failed. That second refusal is what this fixture must
# produce, and not a defect of it: the one record above the floor is the measurement of the
# ladder, and nothing but the name tells the verb that this job arranged that refusal.
check "the unrecognised-refusal reading names the host" \
  "$(contains "$out" "the fetch of github.com stayed on the pinned ladder")"
check "the unrecognised-refusal reading says what it read" \
  "$(contains "$out" "so this job arranged a refusal whose shape the stand-in curl does not recognise")"
check "no refusal speaks beside those two" "$(is "$(error_count "$out")" 2)"

summary "ci-tools-proof.sh ladder-report"
