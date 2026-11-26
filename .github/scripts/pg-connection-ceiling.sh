#!/usr/bin/env bash
# Check that a CI Postgres service allows the connections a Dispatch test lane needs.
#
# A lane's server must allow enough because a server that allows fewer connections than the suite
# opens does not fail where the size is wrong. It fails later, as a 53300 refusal (`remaining
# connection slots are reserved for roles with the SUPERUSER attribute`) inside a room, a lease or
# a socket, and nothing in the failure names the cause. What a lane's server must allow is
# testlimits.ServerConnections (packages/envoy/internal/dispatch/testlimits) beyond its own
# superuser_reserved_connections. packages/envoy/cmd/dispatch/connection_ceiling_test.go refuses,
# by name, a test start on a server too small for the Dispatch processes then running.
#
# Usage: pg-connection-ceiling.sh <container>
#   container  the service container id (${{ job.services.postgres.id }})
#
# It holds no number of its own: internal/dispatch/store/cmd/connections prints
# testlimits.ServerConnections, and this script adds only the server's own
# superuser_reserved_connections. The server's max_connections is on its command line, which
# .github/scripts/postgres-service-settings.sh writes for every CI Postgres service, so this
# script changes nothing: it reads `show max_connections` and fails, naming both numbers, when
# the server allows less than the lane needs. A command-line setting outranks `alter system`, so
# an alter and a restart here could not raise it; the fix for that failure is the command line.
set -euo pipefail

container=${1:?the service container id}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
server=$(cd -- "$repo_root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections)
reserved=$(docker exec "$container" psql -U postgres -Atc "select current_setting('superuser_reserved_connections')")
want=$((server + reserved))
allowed=$(docker exec "$container" psql -U postgres -Atc "show max_connections")
case $allowed in
  '' | *[!0-9]*)
    echo "::error::pg-connection-ceiling: the service answered '${allowed}' for max_connections, which is no number"
    exit 1
    ;;
esac

echo "pg-connection-ceiling: ${server} connections the suite needs (testlimits.ServerConnections) + ${reserved} reserved for superusers = ${want}; the service allows ${allowed}"
[ "$allowed" -ge "$want" ] || {
  echo "::error::pg-connection-ceiling: the service allows ${allowed} connections, and this lane needs ${want}. Raise max_connections in .github/scripts/postgres-service-settings.sh to at least ${want}, and every Postgres service's command: line with it (check-workflow-files.sh names each one)."
  exit 1
}
