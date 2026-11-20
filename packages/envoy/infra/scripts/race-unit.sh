#!/usr/bin/env bash
# packages/envoy/infra/scripts/race-unit.sh
#
# The race build of envoy-infra's unit tests: the one command envoy-infra-go's unit step runs and
# ci-caches.yaml's envoy-infra fill compiles into the build cache that step restores.
#
#   race-unit.sh run       go test -count=1 -race over PACKAGES (envoy-infra-go's unit step)
#   race-unit.sh compile   go test -c -o /dev/null -race over PACKAGES (the fill's arm64 leg)
#
# PACKAGES is every package of the module, from `go list ./...`, less those EXCLUDE matches: the
# text checks, whose tests the text-check job runs. Both modes share PACKAGES and -race, and only
# the flags of a run against those of a compile differ, so the objects the fill compiles are the
# objects the step links. A fill built from another list or other flags holds objects no step
# reads, and nothing fails: the step compiles them from nothing on every run.
# .github/scripts/ci_job_reads.py pins the step to `scripts/race-unit.sh run` and refuses an
# EXCLUDE other than its INFRA_PLAIN_PATTERN, which its coverage model of the step is built from;
# .github/scripts/ci_workflow_rules.py's check_race_fill_scripts holds the fill to
# `scripts/race-unit.sh compile` and to no race build of its own.
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
run) flags=(-count=1) ;;
compile) flags=(-c -o /dev/null) ;;
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
exec go test "${flags[@]}" -race "${packages[@]}"
