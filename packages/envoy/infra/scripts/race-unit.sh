#!/usr/bin/env bash
# packages/envoy/infra/scripts/race-unit.sh
#
# The race build of envoy-infra's unit tests: the one command envoy-infra-build's unit step runs
# and ci-caches.yaml's envoy-infra fill compiles into the build cache that step restores.
#
#   race-unit.sh run       go test -count=1 -race over PACKAGES (envoy-infra-build's unit step)
#   race-unit.sh compile   go test -c -o /dev/null -race over PACKAGES (the fill's arm64 leg)
#
# PACKAGES is every package of the module, from `go list ./...`, less those EXCLUDE matches: the
# text checks, whose tests the text-check job runs. Both modes share PACKAGES and -race, and only
# the flags of a run against those of a compile differ, so the objects the fill compiles are the
# objects the step links. A fill built from another list or other flags holds objects no step
# reads, and nothing fails: the step compiles them from nothing on every run.
# .github/scripts/ci_job_reads.py pins the step to `scripts/race-unit.sh run`, under
# .github/scripts/go_test_events.py, and refuses an
# EXCLUDE other than its INFRA_PLAIN_PATTERN, which its coverage model of the step is built from;
# .github/scripts/ci_workflow_rules.py's check_race_fill_scripts holds the fill to
# `scripts/race-unit.sh compile` and to no race build of its own.
#
# A run goes through .github/scripts/go_test_events.py (so it needs python3), which adds -json,
# keeps every event and prints the log go test prints without it: under a runner in its step's
# file (or the TEST_EVENTS_FILE the writer that runs this script names, as envoy-infra-build's
# unit step, "Unit test envoy-infra", does), and outside one in a file of its own under TMPDIR,
# which nothing uploads there, so it is removed when the run ends. A compile writes no events.
#
# It runs from the module whatever the caller's directory, takes exactly one argument, and exits
# 2 on any other; it exits 1 when go list fails or keeps no package, and otherwise with go test's
# own status.
set -euo pipefail

EXCLUDE='/internal/textcheck$'

if [ "$#" -ne 1 ]; then
	echo "usage: $0 run|compile" >&2
	exit 2
fi
case "$1" in
run | compile) ;;
*)
	echo "usage: $0 run|compile" >&2
	exit 2
	;;
esac

cd "$(dirname "$0")/.."
listed=$(go list ./...)
mapfile -t packages < <(grep -Ev "$EXCLUDE" <<<"$listed" || true)
if [ "${#packages[@]}" -eq 0 ]; then
	echo "race-unit.sh: go list ./... kept no package once EXCLUDE ($EXCLUDE) was taken out" >&2
	exit 1
fi
if [ "$1" = compile ]; then
	exec go test -c -o /dev/null -race "${packages[@]}"
fi
if [ -n "${RUNNER_TEMP:-}${TEST_EVENTS_FILE:-}" ]; then
	exec ../../../.github/scripts/go_test_events.py go test -count=1 -race "${packages[@]}"
fi
TEST_EVENTS_FILE=$(mktemp "${TMPDIR:-/tmp}/race-unit-events.XXXXXX")
export TEST_EVENTS_FILE
trap 'rm -f "$TEST_EVENTS_FILE"' EXIT
../../../.github/scripts/go_test_events.py go test -count=1 -race "${packages[@]}"
