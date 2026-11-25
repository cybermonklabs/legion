#!/usr/bin/env bash
# Turn a CI Postgres service's crash safety off (fsync, full_page_writes, synchronous_commit), so
# no commit, checkpoint or database copy waits for the runner's disk. Every job of
# .github/workflows that runs a Postgres service runs this script before its tests, and
# .github/scripts/check-workflow-files.sh refuses one that does not.
#
# Why: nothing a CI service holds outlives its job, so a crash, the one thing these settings guard
# against, would lose nothing a later step reads. PostgreSQL's guide lists all three for a site
# that needs no such guarantee (https://www.postgresql.org/docs/current/non-durability.html), and the
# test Postgres of a devbox (packages/daemon/docker/legion-testpg) and the e2e harness's
# (packages/dispatch/e2e/harness-postgres.ts) already run so. With them on, the tests wait on the
# disk rather than on anything they test. The daemon's tests clone each database from a template
# with `strategy file_copy` (packages/daemon/internal/testpg/template.go), which forces a
# checkpoint before and after the copy and another at the drop, every package's queued on the
# server's one checkpointer, and with fsync on each checkpoint waits for the disk to flush.
# daemon-go-race's command at 2b43effb, in one 4-CPU pod with this image beside it (dispatch://INFRA-1075):
# internal/daemon 586.6 s with crash safety on and 261.7 s and 281.3 s with it off, and internal/intake
# 147.8 s against 86.0 s and 86.4 s; with it on the pod's CPUs were 1.1 to 1.6 of 4 busy.
#
# A service container takes no server arguments, so `alter system` writes the three, as
# pg-connection-ceiling.sh writes max_connections, and a reload applies them: max_connections
# needs that script's restart, and these three need only the reload, synchronous_commit reaching
# every session that starts after it. They stay written through a later restart of the service,
# such as pg-connection-ceiling.sh's. The script reads them back from a new session and fails
# unless each is off.
#
# Usage: pg-crash-safety-off.sh <container>
#   container  the service container id (${{ job.services.postgres.id }})
set -euo pipefail

container=${1:?the service container id}

docker exec "$container" psql -U postgres -v ON_ERROR_STOP=1 -q \
  -c 'alter system set fsync = off' \
  -c 'alter system set full_page_writes = off' \
  -c 'alter system set synchronous_commit = off' \
  -c 'select pg_reload_conf()'
settings=""
for _ in $(seq 1 20); do
  settings=$(docker exec "$container" psql -U postgres -Atc \
    "select current_setting('fsync') || ' ' || current_setting('full_page_writes') || ' ' || current_setting('synchronous_commit')")
  [ "$settings" = "off off off" ] && break
  sleep 0.5
done
echo "pg-crash-safety-off: fsync, full_page_writes, synchronous_commit: $settings"
[ "$settings" = "off off off" ] || {
  echo "::error::pg-crash-safety-off: the service's fsync, full_page_writes and synchronous_commit read '$settings' after the reload, not 'off off off'"
  exit 1
}
