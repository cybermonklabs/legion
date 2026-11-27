#!/usr/bin/env bash
# The address of a Postgres service container on the runner's docker network, for a job whose
# tests would otherwise reach it through the port map on localhost.
#
#   .github/scripts/postgres-service-address.sh <container id>
#
# It prints `address=<ip>`, the line a step writes to $GITHUB_OUTPUT, and nothing else; the caller
# adds the port (ci-job-dispatch-memory.yaml's URL ends `:5432`). It
# fails, naming the container, when docker reports no address or more than one: a job that cannot
# read one address fails here rather than running its tests against a URL with no host in it.
#
# Why not localhost:5432, which the runner's port map serves: that map is a userland proxy
# (docker-proxy), a process that copies every byte of every connection in both directions, and a
# lane whose tests talk to Postgres all run long pays for it twice over - in the proxy's own CPU
# and in the system time of both ends. Measured on the dispatch-memory suite, the same tests, the
# same runner image and the same service, three runs each way (scratch run 46055613107,
# dispatch://INFRA-5962):
#
#   localhost:5432      tests 539.2 / 547.8 / 548.8 s, machine 1,898 / 1,936 / 1,958 CPU-s,
#                       docker-proxy 131.2 / 134.1 / 130.8 CPU-s (6.8-6.9 % of the machine)
#   the container       tests 457.0 / 460.6 / 475.8 s, machine 1,695 / 1,706 / 1,769 CPU-s,
#                       docker-proxy 0
#
# The medians are 547.8 s and 460.6 s, and the two sets do not overlap: the slowest run past the
# proxy was faster than the fastest run through it. The servers' own CPU and the server's fall
# too, since both ends stop copying through a third process.
#
# The address is read at run time from the container the runner made, as this job already reads
# that container for its settings (postgres-service-settings.sh) and its connection ceiling
# (pg-connection-ceiling.sh), so nothing here depends on a network name or a subnet staying what
# it is today. A runner whose services stop being reachable this way fails the job at this script
# or at the first connection, and never silently: the tests skip only when
# DISPATCH_TEST_DATABASE_URL is unset, and this step leaves it set or fails.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: postgres-service-address.sh <container id>" >&2
  exit 2
fi
container="$1"

mapfile -t addresses < <(docker inspect -f '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' "$container" | grep -v '^$')
if [ "${#addresses[@]}" -ne 1 ]; then
  echo "postgres-service-address.sh: docker reports ${#addresses[@]} addresses for the service container ${container} (${addresses[*]:-none}); want one" >&2
  exit 1
fi
printf 'address=%s\n' "${addresses[0]}"
