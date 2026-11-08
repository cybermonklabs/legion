#!/usr/bin/env bash
# What the Dispatch browser suite's lanes get from the image they run in, read once per job
# before the suite starts, and what the provider that makes those lanes costs.
#
# Who runs it, and why it is a script: the "Read the lanes' PostgreSQL and time its provider"
# step of .github/workflows/dispatch-browser.yaml's shard job and the step of that name in its
# alone job. The two jobs repeat their step lists for the reason that file's head gives - a
# composite action carries neither `timeout-minutes` nor `services:` - but that reason is a
# composite action's and not a script's: a step that calls a script keeps its own
# `timeout-minutes`. So this body is written once, both steps call it, and a fix here is a fix
# for both jobs. It stood as two copies, word for word apart from the lane count, until a
# review read them together (dispatch://INFRA-1443).
#
#   .github/scripts/dispatch-browser-postgres.sh <lane count>
#
# <lane count> is the number of lanes the calling job's browser step runs, its
# DISPATCH_E2E_WORKERS: four on a shard leg, one on an alone leg. It is the one thing the two
# callers differ in, and it decides how many data directories are copied below, so the copy
# time printed is that job's own copy time and not some other shape's.
#
# What it prints, in order, and why each line is wanted:
#
#   id          The user the calling job's steps run as, uid, gid and groups. This script is a
#               child process of the step, so its id is the step's. `initdb` refuses to run as
#               root and refuses a uid with no passwd entry, so the lanes' clusters stand or
#               fall on this line, and it is the one reading that says the container's
#               `--user 1001` met a passwd entry in the image that was actually pulled. It was
#               printed by a step of its own until the image began asserting its own uid at
#               build time (.github/docker/browser.Dockerfile); the per-run reading stays here,
#               where the thing that needs it is.
#   version()   The lane server's own answer, from the same binaries the lanes start. It names
#               the PostgreSQL the rows ran against, which no other record of the run holds:
#               e2e/run.ts removes its run directory when it ends, so a lane's own cluster is
#               gone by the time anyone reads the job.
#   vector <v>  The version of the `vector` extension that server loaded, for the same reason:
#               harness-postgres.ts's migration 0072 needs an extension built against the
#               server it runs, and a mismatch is legible here rather than in a migration
#               failure four minutes later.
#   the times   `initdb`, the template database with its extension, the lane copies and the sum
#               of the three. harness-postgres.ts runs `initdb` once per run and copies the data
#               directory per lane, so these are what the lanes pay before the first row runs,
#               and the figure the provider records for itself can be held against them.
#
# It runs the same sequence the lanes' provider runs (packages/dispatch/e2e/harness-postgres.ts),
# with this image's PostgreSQL 18 on port 55432, which no lane uses: the lanes take ports the OS
# chooses. It calls `initdb`, `pg_ctl` and `psql` by name: .github/docker/browser.Dockerfile
# appends the version directory to PATH and asserts all three are found at build time, so this
# script writes no version directory of its own and a PostgreSQL major moving in that file moves
# it here with no edit. The two browser steps still export DISPATCH_E2E_PG_BIN, which is the
# harness's binaries provider's own input - a directory, not a PATH lookup - and is no part of
# what this script reads; it could not read it in any case, since that export happens inside a
# later step's `run:`.
#
# Everything it writes goes under a directory of its own in $RUNNER_TEMP and is removed again,
# so the checkout stays clean and nothing is left for the suite to find.
#
# Run from anywhere: it reads nothing of the checkout and writes only inside that directory.
# CI runs it in the shard and alone jobs of dispatch-browser.yaml.
set -euo pipefail
shopt -s inherit_errexit

usage() {
  echo "::error::dispatch-browser-postgres.sh takes one argument, the number of lanes the" \
    "calling job runs: .github/scripts/dispatch-browser-postgres.sh <lane count>." \
    "It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
lanes=$1
case "$lanes" in '' | *[!0-9]* | 0) usage "$@" ;; esac

# The user every step of the calling job runs as, this script's own process included.
id

work="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/pg-reading"
rm -rf "$work"
mkdir -p "$work/template"
start=$(date +%s.%N)
initdb -D "$work/template/pgdata" -U postgres --auth=trust --no-sync -E UTF8 > /dev/null
after_initdb=$(date +%s.%N)
pg_ctl -D "$work/template/pgdata" -l "$work/template/postgres.log" \
  -o "-p 55432 -h 127.0.0.1 -k $work/template" -w start > /dev/null
url="postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable"
psql "$url" -v ON_ERROR_STOP=1 -c 'create database dispatch_e2e' > /dev/null
lane="postgres://postgres@127.0.0.1:55432/dispatch_e2e?sslmode=disable"
psql "$lane" -v ON_ERROR_STOP=1 -c 'create extension vector' > /dev/null
psql "$lane" -XAt -v ON_ERROR_STOP=1 -c 'select version()'
psql "$lane" -XAt -v ON_ERROR_STOP=1 \
  -c "select 'vector ' || extversion from pg_extension where extname = 'vector'"
pg_ctl -D "$work/template/pgdata" -m fast -w stop > /dev/null
after_template=$(date +%s.%N)
# One copy per lane the caller named, as the provider makes one per Playwright worker.
lane_number=0
while [ "$lane_number" -lt "$lanes" ]; do
  mkdir -p "$work/lane-$lane_number"
  cp -a "$work/template/pgdata" "$work/lane-$lane_number/pgdata"
  lane_number=$((lane_number + 1))
done
done_at=$(date +%s.%N)
# The labels are padded to one width rather than written out per lane count, so the four lines
# line up whether the caller asked for one lane or four; the copy line carries the count it
# was asked for, so a reader never has to know which job printed it.
awk -v a="$start" -v b="$after_initdb" -v c="$after_template" -v d="$done_at" -v n="$lanes" 'BEGIN {
  printf "%-22s%.2f s\n", "initdb:", b - a
  printf "%-22s%.2f s\n", "template db + vector:", c - b
  printf "%-22s%.2f s\n", n " lane cop" (n == 1 ? "y:" : "ies:"), d - c
  printf "%-22s%.2f s\n", "the whole provider:", d - a
}'
rm -rf "$work"
