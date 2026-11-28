#!/usr/bin/env bash
# The settings every CI Postgres service starts with, and their read-back from the running server.
# This file is the source every service's `command:` line is held to.
#
#   .github/scripts/postgres-service-settings.sh --command
#   .github/scripts/postgres-service-settings.sh <container id> <service id>
#
# --command prints the server's command line, the value each Postgres service's `command:` in
# .github/workflows must carry. .github/scripts/check-workflow-files.sh (rule 7) runs this form and
# refuses a Postgres service whose `command:` is any other line; it refuses the lines that differ
# and writes none of them, so a change to a setting edits this file and each Postgres service's
# `command:` with it (16 services in 14 files at this writing; the check names each one it
# refuses). The second form reads each setting back from the server in the container and fails
# unless each is what the command line asks for. Every job with a Postgres service runs it in its
# step "Read the service's settings back", and check-workflow-files.sh refuses a job with no such
# step for each of its Postgres services.
#
# Why a command line: the runner gives the service's `command:` to `docker create` as the
# arguments after the image (docs.github.com, "Communicating with Docker service containers",
# "Customizing service container entrypoints and commands", since 2026-04-02), so the server
# starts with these settings and no step has to change them. Before that, a step wrote them with
# `alter system`, a reload applied three of them, and max_connections needed a restart of the
# service (0.4 to 3.2 s from the alter to the read-back in eleven jobs of the merge groups of
# 2026-11-25: runs 45708557702, 45711252287, 45712646870 and 45714810875). A setting on the
# server's command line also outranks `alter system`, so nothing a test writes to
# postgresql.auto.conf changes it.
#
# Why the read-back: a `command:` the runner did not pass through would leave the server on the
# image's defaults rather than failing, and the tests would then wait on the disk and run out of
# connections with nothing that names the cause. A setting the server refuses never reaches this
# script: postgres exits, and the job fails in "Initialize containers".
#
# What each setting is for:
#
#   fsync, full_page_writes, synchronous_commit = off. Nothing a CI service holds outlives its
#     job, so a crash, the one thing these guard against, would lose nothing a later step reads.
#     PostgreSQL's guide lists all three for such a site
#     (https://www.postgresql.org/docs/current/non-durability.html), and the test Postgres of a
#     devbox (packages/daemon/docker/legion-testpg) and the e2e harness's
#     (packages/dispatch/e2e/harness-postgres.ts) run so. With them on, the tests wait on the
#     disk: the daemon's tests clone each database from a template with `strategy file_copy`
#     (packages/daemon/internal/testpg/template.go), which forces a checkpoint before and after
#     the copy and another at the drop, and with fsync on each checkpoint waits for the disk.
#     daemon-go-race's command at 2b43effb in one 4-CPU pod with this image beside it
#     (dispatch://INFRA-1075): internal/daemon 586.6 s with them on, 261.7 s and 281.3 s off.
#   max_connections = 1000. The Dispatch lanes need testlimits.ServerConnections plus the
#     superuser reserve of 3 (988 + 3 = 991 at this writing), which
#     .github/scripts/pg-connection-ceiling.sh checks against the server in each such job. That
#     sum bounds what eight Dispatch processes may open; they open far fewer: cmd/dispatch with
#     eight at once peaked at 182 and 184 client backends in one pod (and 109 and 133 with four).
#     The rest is room for a job whose `go test -p` runs several package binaries against one
#     service at once, each with pools of its own; the largest peaks the samplers read in the merge
#     groups of 2026-11-25 were 121 client backends (envoy-go-race-api) and 111 (daemon-go-race).
#     A connection slot costs the server shared memory, not a process: the read-back prints
#     shared_memory_size so the cost of this number is in every job's log.
#   max_prepared_transactions = 10. packages/envoy/internal/dispatch/events'
#     TestAnObservationFindsAnEventWriterThatPreparesDuringItsRead prepares a transaction, and it
#     skips on a server that allows none, which is Postgres's default. One test prepares one
#     transaction at a time, so 10 is room enough.
#
# The read-back also holds the container's /dev/shm to 1 GiB, the service's `--shm-size=1g`.
# Docker gives a container 64 MiB, and Postgres keeps its dynamic shared memory there (parallel
# query workers' segments, dynamic_shared_memory_type = posix), so a segment that does not fit
# fails its query with "could not resize shared memory segment" rather than slowing it.
# --shm-size is a `docker create` option, not a server setting, so the read-back asks docker.
#
# The health check in the same `options:` probes every second until the server first answers
# (--health-start-interval 1s, within a --health-start-period of 60 s) and every 10 s after that.
# The runner's "Initialize containers" waits for the first healthy probe, so the probe's interval
# is a wait every job pays: in dispatch-memory job 139546656768 the server took connections 0.95 s
# after `docker start`, but the first probe of a 10 s interval ran at 10 s and the runner read
# "healthy" at 14.7 s. The probe asks over TCP (-h 127.0.0.1): the image's entrypoint first runs
# a server on the Unix socket alone to create the database, then stops it and starts the real
# one, and a probe over the socket can answer from the first.
#
# What it prints: one line, `<service id>: <container id>: fsync=off ... shm_size=1073741824
# (shared_memory_size=<n>MB)`, the values as the server and docker reported them, and before it
# an ::error:: for each one that differs, naming the service, the setting, what it read and what
# it wants. It reads all of them before it exits 1, so one red step names every setting that is
# off. A read that docker cannot make ends it at once with an ::error:: naming the service and
# the container.
#
# Run from anywhere: it reads nothing of the checkout. Its test is
# .github/scripts/postgres-service-settings.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

# The server's command line, setting by setting, as `show` reports each one.
settings=(fsync=off full_page_writes=off synchronous_commit=off max_connections=1000 max_prepared_transactions=10)
# The service's --shm-size=1g, as docker inspect reports HostConfig.ShmSize.
shm_size=1073741824

usage() {
  echo "::error::postgres-service-settings.sh takes --command, or two arguments, the service's" \
    "container id and its service id: .github/scripts/postgres-service-settings.sh <container id>" \
    "<service id>. It was given: $*"
  exit 1
}

if [ "${1:-}" = --command ]; then
  [ "$#" -eq 1 ] || usage "$@"
  line=postgres
  for setting in "${settings[@]}"; do
    line+=" -c $setting"
  done
  echo "$line"
  exit 0
fi

[ "$#" -eq 2 ] || usage "$@"
id=$1
label=$2
[ -n "$label" ] || usage "$@"
if [ -z "$id" ]; then
  echo "::error::$label: the container id is empty, which is what the runner gives a step for" \
    "a service id the job does not declare"
  exit 1
fi

# One docker exec for every setting: psql prints one line per -c, in order.
shows=()
for want in "${settings[@]}"; do
  shows+=(-c "show ${want%%=*}")
done
shows+=(-c "show shared_memory_size")
if ! values=$(docker exec "$id" psql -U postgres -XAt -v ON_ERROR_STOP=1 "${shows[@]}"); then
  echo "::error::$label: docker exec into $id could not read the server's settings"
  exit 1
fi
mapfile -t read_values <<<"$values"
if [ "${#read_values[@]}" -ne $((${#settings[@]} + 1)) ]; then
  echo "::error::$label: the server in $id answered ${#read_values[@]} lines for" \
    "$((${#settings[@]} + 1)) settings: $values"
  exit 1
fi
if ! shm=$(docker inspect --format '{{.HostConfig.ShmSize}}' "$id"); then
  echo "::error::$label: docker inspect could not read the shared memory size of $id"
  exit 1
fi

failed=0
read_back=
for index in "${!settings[@]}"; do
  want=${settings[$index]}
  setting=${want%%=*}
  value=${read_values[$index]}
  read_back+=" $setting=$value"
  if [ "$value" != "${want#*=}" ]; then
    echo "::error::the Postgres service $label reports $setting=$value, not ${want#*=}"
    failed=1
  fi
done
read_back+=" shm_size=$shm"
if [ "$shm" != "$shm_size" ]; then
  echo "::error::the Postgres service $label has a /dev/shm of $shm bytes, not $shm_size (--shm-size=1g)"
  failed=1
fi
echo "$label: $id:$read_back (shared_memory_size=${read_values[${#settings[@]}]})"
exit "$failed"
