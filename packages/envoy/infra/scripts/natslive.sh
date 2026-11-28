#!/usr/bin/env bash
# packages/envoy/infra/scripts/natslive.sh
#
# What each package the arguments name adds under the natslive tag, run at once: the one command
# envoy-infra-go's "Live tests of envoy-infra against nats-server (natslive)" step runs.
#
#   natslive.sh PACKAGE...   one go test -tags natslive per package, each with its own -run
#
# Each package's -run is the list .github/scripts/go-tagged-tests.sh prints for it, and every list
# is read before any run starts, so an empty or refused list ends the script with no test
# running. Each run's output goes to a file in a directory of the script's own, removed on exit,
# and is printed whole under its own heading as that run ends. It waits by process id, one run at
# a time, and exits 1 naming each package whose run failed, 2 with no argument, and 0 otherwise.
# Its -timeout fires before envoy-infra-go's job limit, so a hung test prints its dump.
# gotestsum keeps one test2json file per package run in $RUNNER_TEMP/go-test-events/ for the
# job's artifact. Locally the events go in the same temporary directory as the text logs.
#
# .github/scripts/audited_scripts.py's AUDITED_SCRIPTS holds a person's reading of this file,
# pinned by its SHA-256, and .github/scripts/ci_job_reads.py pins the step to this script and the
# packages it passes. Why the packages run at once, with the measurements:
# .github/ci-notes/ci.md#the-envoy-infra-module-its-own-workspace-and-its-live-tests
set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "usage: $0 PACKAGE..." >&2
	exit 2
fi
if ! command -v gotestsum >/dev/null; then
	echo 'natslive.sh: gotestsum is not on PATH; use .github/actions/install-gotestsum or run GOWORK=off go -C .github/actions/install-gotestsum build -o <dir>/gotestsum gotest.tools/gotestsum' >&2
	exit 127
fi
cd "$(dirname "$0")/.."
out=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/natslive.XXXXXX")
trap 'rm -rf "$out"' EXIT
events_dir="$out"
if [ -n "${RUNNER_TEMP:-}" ]; then
	events_dir="$RUNNER_TEMP/go-test-events"
	mkdir -p "$events_dir"
fi

declare -A pattern=() running=()
for pkg in "$@"; do
	pattern[$pkg]=$(../../../.github/scripts/go-tagged-tests.sh natslive "$pkg")
done
events=()
packages=("$@")
for pkg in "${packages[@]}"; do
	slug=${pkg#./}
	slug=${slug//[^a-zA-Z0-9._-]/_}
	event_file=$(mktemp --suffix=.jsonl "$events_dir/natslive-$slug-XXXXXX")
	events+=("$event_file")
done
for i in "${!packages[@]}"; do
	pkg=${packages[$i]}
	gotestsum --jsonfile "${events[$i]}" --format standard-quiet --raw-command -- \
		go test -json -count=1 -timeout 25m -tags natslive -run "${pattern[$pkg]}" "$pkg" >"$out/$i.txt" 2>&1 &
	running[$!]=$i
done
failed=()
while [ ${#running[@]} -gt 0 ]; do
	status=0
	wait -n -p ended "${!running[@]}" || status=$?
	i=${running[$ended]}
	pkg=${packages[$i]}
	unset "running[$ended]"
	echo "::group::$pkg (exit $status)"
	cat "$out/$i.txt"
	echo "::endgroup::"
	[ "$status" -eq 0 ] || failed+=("$pkg (exit $status)")
done
if [ ${#failed[@]} -ne 0 ]; then
	echo "::error::the natslive tests failed in: ${failed[*]}"
	exit 1
fi
