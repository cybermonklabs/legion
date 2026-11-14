#!/usr/bin/env bash
# Raise a CI Postgres service's max_connections to what a Dispatch test lane needs, and restart it
# so the setting takes.
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
set -euo pipefail

container=${1:?the service container id}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
server=$(cd -- "$repo_root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections)
reserved=$(docker exec "$container" psql -U postgres -Atc "select current_setting('superuser_reserved_connections')")
want=$((server + reserved))

echo "pg-connection-ceiling: ${server} connections the suite needs (testlimits.ServerConnections) + ${reserved} reserved for superusers = ${want}"
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
