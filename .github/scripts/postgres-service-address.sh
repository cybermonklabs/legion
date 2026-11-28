#!/usr/bin/env bash
# The address of a Postgres service container on the runner's docker network, for a job whose
# tests reach it there rather than at localhost through a published port.
#
#   .github/scripts/postgres-service-address.sh <container id>
#
# It prints `address=<ip>`, the line a step writes to $GITHUB_OUTPUT, and nothing else; the caller
# adds the port (ci-job-dispatch-memory.yaml's URL puts `:5432` after it). It fails with an
# ::error:: on stderr, naming the container, when docker inspect fails or reports no address or
# more than one: a job that cannot read one address fails here rather than running its tests
# against a URL with no host in it.
#
# Why not localhost:5432: docker serves a published port (`ports:`) on localhost through a
# userland proxy (docker-proxy), a process that copies every byte of every connection in both
# directions, and a lane whose tests talk to Postgres all run long pays for it in the proxy's CPU
# and in the suite's time. So ci-job-dispatch-memory.yaml publishes no port. Measured on the
# dispatch-memory suite, the same tests, the same runner image and the same service, three runs
# each way, one job a row (scratch run 46055613107, dispatch://INFRA-5962):
#
#   reached at      job           tests  machine  docker-proxy   Postgres  servers
#   localhost:5432  140697245037  539.2    1,898  131.2 (6.9 %)     281.5  1,024.8
#   localhost:5432  140697244536  548.8    1,958  134.1 (6.8 %)     288.8  1,084.1
#   localhost:5432  140697244899  547.8    1,936  130.8 (6.8 %)     286.1  1,064.3
#   the container   140697244920  457.0    1,695  none              258.9    992.6
#   the container   140697244914  475.8    1,769  none              267.7  1,036.3
#   the container   140697245137  460.6    1,706  none              263.3    994.9
#
# tests is go test's own package line for ./cmd/dispatch, in seconds; the other columns are CPU-s:
# the machine's, docker-proxy's (with its share of the machine's; none: not in that run's list of
# processes), Postgres's, and the Dispatch servers', the test binary's child processes (336 to 339
# a run). The medians of tests are 547.8 s and 460.6 s, and the two sets do not overlap: the
# slowest run past the proxy, 475.8 s, was faster than the fastest run through it, 539.2 s.
# Postgres's own CPU falls in every run, from 281.5-288.8 to 258.9-267.7 CPU-s; the Dispatch
# servers' falls from 1,024.8-1,084.1 to 992.6-1,036.3 CPU-s, two ranges that overlap.
#
# The address is read at run time from the container the runner made, as this job already reads
# that container for its settings (postgres-service-settings.sh) and its connection ceiling
# (pg-connection-ceiling.sh), so nothing here depends on a network name or a subnet staying what
# it is today. A runner whose services stop being reachable this way fails the job at this script
# or at the first connection, and never skips the tests: they skip only when
# DISPATCH_TEST_DATABASE_URL is unset or empty, the test step sets it to a URL that is never
# empty, and a failure of this script stops the job before that step runs.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: postgres-service-address.sh <container id>" >&2
  exit 2
fi
container="$1"

# One line for each network that gave the container an address.
template='{{range .NetworkSettings.Networks}}{{if .IPAddress}}{{println .IPAddress}}{{end}}{{end}}'
if ! out=$(docker inspect -f "$template" "$container"); then
  echo "::error::postgres-service-address.sh: docker inspect failed for the service container ${container}" >&2
  exit 1
fi
# A here-string of "" reads as one empty line, so an empty answer is split into no lines at all.
addresses=()
if [ -n "$out" ]; then
  mapfile -t addresses <<<"$out"
fi
if [ "${#addresses[@]}" -ne 1 ]; then
  echo "::error::postgres-service-address.sh: docker reports ${#addresses[@]} addresses for the service container ${container} (${addresses[*]:-none}); want one" >&2
  exit 1
fi
printf 'address=%s\n' "${addresses[0]}"
