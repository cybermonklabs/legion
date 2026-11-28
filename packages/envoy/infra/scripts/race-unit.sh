#!/usr/bin/env bash
# packages/envoy/infra/scripts/race-unit.sh
#
# The race build of envoy-infra's unit tests: the one command envoy-infra-build's unit step runs
# and ci-caches.yaml's envoy-infra fill compiles into the build cache that step restores.
#
#   race-unit.sh run       go test -json -count=1 -race over PACKAGES (envoy-infra-build's unit step)
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
# 2 on any other; it exits 1 when go list fails or keeps no package. Compile exits with go test's
# status. Run exits with gotestsum's status: go test's own, 128+N for a signal it forwarded on
# SIGINT, or 3 for a fault of its own. With no gotestsum on PATH, run exits 127 before testing.
# gotestsum keeps test2json events in $RUNNER_TEMP/go-test-events/race-unit-*.jsonl for the job's
# artifact, or in a temporary file removed on exit.
set -euo pipefail

EXCLUDE='/internal/textcheck$'

if [ "$#" -ne 1 ]; then
	echo "usage: $0 run|compile" >&2
	exit 2
fi
case "$1" in
run)
	if ! command -v gotestsum >/dev/null; then
		echo 'race-unit.sh: gotestsum is not on PATH; use .github/actions/install-gotestsum or run GOWORK=off go -C .github/actions/install-gotestsum build -o <dir>/gotestsum gotest.tools/gotestsum' >&2
		exit 127
	fi
	;;
compile) ;;
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

if [ -n "${RUNNER_TEMP:-}" ]; then
	mkdir -p "$RUNNER_TEMP/go-test-events"
	events=$(mktemp --suffix=.jsonl "$RUNNER_TEMP/go-test-events/race-unit-XXXXXX")
else
	events=$(mktemp --suffix=.jsonl -t race-unit-XXXXXX)
	trap 'rm -f "$events"' EXIT
fi
gotestsum --jsonfile "$events" --format standard-quiet --raw-command -- \
	go test -json -count=1 -race "${packages[@]}"
