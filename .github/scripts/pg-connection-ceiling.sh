#!/usr/bin/env bash
# Raise a CI Postgres service's connection ceiling to what the Dispatch processes a lane starts
# may open, and restart it so the setting takes.
#
# The number is derived from the code rather than written here: store.ConnectionsPerProcess is the
# most connections one Dispatch server process opens, and a lane that starts N of them against one
# server needs N times that, beside the slots Postgres keeps for superusers and a little headroom
# for the test's own clients. The ceiling has climbed 105, 108 and 112 as pools were added, so a
# literal in this file would be right until the next pool and silent afterwards: the suite would
# fail as 53300 refusals inside a room, a lease or a socket (see
# packages/envoy/cmd/dispatch/connection_ceiling_test.go, which refuses such a server by name).
#
# Usage: pg-connection-ceiling.sh <container> <processes>
#   container  the service container id (${{ job.services.postgres.id }})
#   processes  how many Dispatch processes the lane starts against it
set -euo pipefail

container=${1:?the service container id}
processes=${2:?how many Dispatch processes the lane starts}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
per_process=$(cd -- "$repo_root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections)
reserved=$(docker exec "$container" psql -U postgres -Atc "select current_setting('superuser_reserved_connections')")
# The test's own clients: each package run holds a pool of its own beside the processes, and the
# harness opens short-lived connections for its reads. Twenty is what those have needed.
headroom=20
want=$((processes * per_process + reserved + headroom))

echo "pg-connection-ceiling: ${processes} process(es) x ${per_process} connections + ${reserved} reserved + ${headroom} headroom = ${want}"
docker exec "$container" psql -U postgres -c "alter system set max_connections = ${want}"
docker restart "$container" >/dev/null
for _ in $(seq 1 30); do
  if docker exec "$container" pg_isready -U postgres >/dev/null 2>&1; then
    allowed=$(docker exec "$container" psql -U postgres -Atc "show max_connections")
    echo "pg-connection-ceiling: the service allows ${allowed}"
    [ "$allowed" -ge "$want" ] || {
      echo "pg-connection-ceiling: the service allows ${allowed}, want at least ${want}" >&2
      exit 1
    }
    exit 0
  fi
  sleep 1
done
echo "pg-connection-ceiling: the service did not come back after its restart" >&2
exit 1
