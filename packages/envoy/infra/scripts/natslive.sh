#!/usr/bin/env bash
# packages/envoy/infra/scripts/natslive.sh
#
# envoy-infra-go's live tests: what each of core, seed and gcp adds under the natslive tag, run
# against real nats-server, Postgres and container images, the three packages at once. The one
# command envoy-infra-go's "Live tests of envoy-infra against nats-server (natslive)" step runs.
#
# One go test per package, never one union -run over the three: a name an untagged test of one
# package shares with a natslive test of another would then run a second time with nothing to
# say so. Each package's -run comes from .github/scripts/go-tagged-tests.sh, and every one of
# the three is read in the first loop, before any run starts, so an empty or refused list ends
# the script under -e with no test running, rather than leaving runs already started with nobody
# to wait for them.
#
# The three run at once because each waits on real servers rather than computing: alone, core
# kept 0.44 of one core busy and gcp 0.66 of one, of the runner's four, and in series they took
# 239, 1 and 400 s (run 45950904157, job 140346269806). Each has servers, ports and temporary
# directories of its own; core starts a Postgres container of its own (testpg) and the three share
# the runner's Docker daemon; none of them opens a database of the job, which carries no service.
#
# Each run's output goes to a file of its own and is printed whole under its own heading as that
# run ends (wait -n -p), so two runs never interleave their lines and a package that ends early
# is read early. Waiting is by process id, one run at a time, because `wait` with no argument
# returns 0 whatever the runs exited with, and would pass the job with a package red. The script
# exits 1 naming each package that failed.
#
# -timeout 25m, not go test's 10 m default: gcp's live tests install helm, nats-server, the
# config reloader and the exporter first, minutes on a cold cache, and gcp takes about 430 s. And
# not more: each go test's -timeout must fire before the runner cancels the job at its 32
# minutes, so that a hung test prints the dump that names it.
#
# .github/scripts/go-test-step-expansion.py's AUDITED_SCRIPTS holds a person's reading of this
# file, pinned by its SHA-256, and .github/scripts/ci_job_reads.py pins the step to
# `scripts/natslive.sh`. It runs from the module whatever the caller's directory, takes no
# argument and exits 2 on one.
set -euo pipefail

if [ "$#" -ne 0 ]; then
	echo "usage: $0" >&2
	exit 2
fi
cd "$(dirname "$0")/.."
# A directory of this run's own for the three outputs, never a fixed name in a shared /tmp.
out=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/natslive.XXXXXX")

declare -A pattern=() running=()
for pkg in core seed gcp; do
	pattern[$pkg]=$(../../../.github/scripts/go-tagged-tests.sh natslive "./$pkg")
done
for pkg in core seed gcp; do
	go test -count=1 -timeout 25m -tags natslive -run "${pattern[$pkg]}" "./$pkg" >"$out/$pkg.txt" 2>&1 &
	running[$!]=$pkg
done
failed=()
while [ ${#running[@]} -gt 0 ]; do
	status=0
	wait -n -p ended "${!running[@]}" || status=$?
	pkg=${running[$ended]}
	unset "running[$ended]"
	echo "::group::$pkg (exit $status)"
	cat "$out/$pkg.txt"
	echo "::endgroup::"
	[ "$status" -eq 0 ] || failed+=("$pkg (exit $status)")
done
if [ ${#failed[@]} -ne 0 ]; then
	echo "::error::the natslive tests failed in: ${failed[*]}"
	exit 1
fi
