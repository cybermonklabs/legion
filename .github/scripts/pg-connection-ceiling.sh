#!/usr/bin/env bash
# Raise a CI Postgres service's max_connections to what a Dispatch test lane needs, and restart it
# so the setting takes, unless the server already allows what the lane needs.
#
# A lane's Postgres is raised because a server that allows fewer connections than the suite opens
# does not fail where the size is wrong. It fails later, as a 53300 refusal (`remaining connection
# slots are reserved for roles with the SUPERUSER attribute`) inside a room, a lease or a socket,
# and nothing in the failure names the cause. What a lane's server must allow is
# testlimits.ServerConnections (packages/envoy/internal/dispatch/testlimits) beyond its own
# superuser_reserved_connections. packages/envoy/cmd/dispatch/connection_ceiling_test.go refuses,
# by name, a test start on a server too small for the Dispatch processes then running.
#
# Usage: pg-connection-ceiling.sh <container>
#   container  the service container id (${{ job.services.postgres.id }})
#
# It holds no number of its own: internal/dispatch/store/cmd/connections prints
# testlimits.ServerConnections, and this script adds only the server's own
# superuser_reserved_connections.
#
# A server started with `-c max_connections=<n>` on its command line already allows n, and a
# command-line setting outranks `alter system`, so the alter and the restart would change nothing
# there. The script therefore reads `show max_connections` first and, where the server already
# allows the whole requirement, prints that and ends. It compares against the requirement whole -
# the suite's connections plus the server's reserved slots - never against the suite's connections
# alone, so a server at exactly that number is still raised. An answer it cannot read as a number
# fails the step rather than skipping it (dispatch://INFRA-1075; that lane's A8 says no step of its
# jobs runs `alter system`).
set -euo pipefail

container=${1:?the service container id}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
server=$(cd -- "$repo_root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections)
reserved=$(docker exec "$container" psql -U postgres -Atc "select current_setting('superuser_reserved_connections')")
want=$((server + reserved))

echo "pg-connection-ceiling: ${server} connections the suite needs (testlimits.ServerConnections) + ${reserved} reserved for superusers = ${want}"

allowed=$(docker exec "$container" psql -U postgres -Atc "show max_connections")
case $allowed in
  '' | *[!0-9]*)
    echo "pg-connection-ceiling: the service answered \"${allowed}\" for max_connections, which is no number" >&2
    exit 1
    ;;
esac
if [ "$allowed" -ge "$want" ]; then
  echo "pg-connection-ceiling: the service already allows ${allowed}, so nothing is altered or restarted"
  exit 0
fi
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
