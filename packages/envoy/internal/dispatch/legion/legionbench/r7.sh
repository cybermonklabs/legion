#!/usr/bin/env bash
# R7's Job (dispatch://INFRA-5577, check k27; spec Design, "How R7's hold is measured"), whole, in
# one pod: a Postgres 18 of its own; the server built at this checkout's commit, stamped; a database
# it migrates, which legionbench loads at INFRA's size with ten times the run volume and with
# -first-read (no issue_facts row, no build in issue_facts_computed); and TestFarmChangeLockHolds
# (legionbench, farm tag), which boots the server on it, so it makes its first whole read of
# INFRA's facts, and drives the delivery intake beside that read and for 5 minutes after it while it
# samples each hold of the change log's lock and each wait for it. Three arms, each over a first
# whole read (before each arm after the first the consumer's tables are emptied again):
#
#   stated    the Design's rates, held to R7's bounds;
#   recorded  one and a half times those rates, recorded and not held;
#   sleep     k27's negative control, at the Design's rates: the server built from this commit with
#             pg_sleep(0.15) in the drain's statement (delivery's drainOneBatch), run once
#             delivery_changes_drain has taken the lock and moved rows, and before the statement's
#             transaction commits (the script edits the line in a worktree of its own and checks
#             the edit took). Each batch the drain moves then holds the lock 150 ms longer, and the
#             drain still takes it 40 times or more, so only R7's 100 ms bound on the drain's p99
#             hold can fail it. The arm is judged as a control: it passes when its drain misses
#             that bound, which prints a RESULT line, and fails, with a FAIL line, when the drain's
#             p99 hold is 100 ms or less or the drain took the lock fewer than 40 times, since the
#             measurement then cannot see the hold it bounds.
#
# r7.yaml runs it on the build farm. Every measurement prints a RESULT line and every miss a FAIL
# line; the script exits 1 when any arm failed.
#
# Environment:
#   R7_WORK          scratch for everything the run writes (required)
#   R7_SOURCE        the checkout of this head (the repository this script is in)
#   R7_SCALE (1), R7_SEED (5577), R7_NOW (the current UTC midnight): the load's -scale, -seed, -now
#   R7_AFTER (5m), R7_READ_TIMEOUT (30m): how long the drive runs after the read, and how long the
#                    read may take
#   R7_ARMS ("stated recorded sleep"): the arms to run, in order
#   R7_PGPORT (55432), R7_SHARED_BUFFERS (2GB), PGBIN (the newest /usr/lib/postgresql/*/bin)
set -euo pipefail

: "${R7_WORK:?R7_WORK must name a scratch directory for the run}"
here=$(cd "$(dirname "$0")" && pwd)
source_dir=${R7_SOURCE:-$(cd "$here/../../../../../.." && pwd)}
work=$R7_WORK
port=${R7_PGPORT:-55432}
now=${R7_NOW:-$(date -u +%Y-%m-%dT00:00:00Z)}
commit=$(git -C "$source_dir" rev-parse HEAD)
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/sock" "$work/run"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }
mark "r7.sh: source $source_dir at $commit, scale ${R7_SCALE:-1}, now $now, node ${NODE_NAME:-unknown}"
echo "RESULT r7 head=$commit node=${NODE_NAME:-unknown} scale=${R7_SCALE:-1} ten_times=1 after=${R7_AFTER:-5m} arms=[${R7_ARMS:-stated recorded sleep}]"

server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then kill -TERM "$server_pid" 2>/dev/null || true; fi
  "$PGBIN/pg_ctl" --pgdata "$work/pg" --wait --mode fast stop >/dev/null 2>&1 || true
}
trap cleanup EXIT
"$PGBIN/initdb" --pgdata "$work/pg" --username postgres --auth=trust --encoding=UTF8 --no-sync >"$work/initdb.log"
"$PGBIN/pg_ctl" --pgdata "$work/pg" --log "$work/pg-server.log" --wait --timeout 300 --options "-p $port -k $work/sock \
  -c listen_addresses=127.0.0.1 -c max_connections=400 -c shared_buffers=${R7_SHARED_BUFFERS:-2GB} \
  -c fsync=off -c full_page_writes=off -c synchronous_commit=off -c wal_level=minimal -c max_wal_senders=0 \
  -c max_wal_size=8GB -c huge_pages=off -c dynamic_shared_memory_type=mmap -c jit=off" start
admin_url="postgres://postgres@127.0.0.1:$port/postgres?sslmode=disable"
url="postgres://postgres@127.0.0.1:$port/r7?sslmode=disable"
psql "$admin_url" -Atc 'select version()'
psql "$admin_url" -Atc 'create database r7'

# The server, the loader and the measurement's test binary, from this checkout; and the sleep
# control's server, from a worktree of it whose drain sleeps 150 ms under the lock.
cd "$source_dir/packages/envoy"
go build -ldflags "-X main.buildCommit=$commit" -o "$work/bin/server" ./cmd/dispatch
go build -o "$work/bin/legionbench" ./internal/dispatch/legion/legionbench/cmd
go test -c -tags farm -o "$work/bin/legionbench.test" ./internal/dispatch/legion/legionbench
case " ${R7_ARMS:-stated recorded sleep} " in
  *" sleep "*)
    git -C "$source_dir" worktree add --detach "$work/sleep-src" "$commit" >/dev/null
    changes=$work/sleep-src/packages/envoy/internal/dispatch/delivery/changes.go
    # The drain's statement becomes the drain followed by a sleep in the same statement, so in its
    # transaction. The sleep is a lateral call on the drain's answer, so it runs after the drain
    # took the lock, and it sleeps only where the drain moved rows: a drain that finds none pending
    # takes no lock, and its callers' passes are not slowed for nothing.
    before=$(grep -cF '`select delivery_changes_drain($1)`, drainBatch)' "$changes" || true)
    sed -i 's/`select delivery_changes_drain(\$1)`, drainBatch)/`select drained.moved from (select delivery_changes_drain($1) as moved offset 0) drained cross join lateral pg_sleep(case when drained.moved > 0 then 0.15 else 0 end)`, drainBatch)/' "$changes"
    slept='`select drained.moved from (select delivery_changes_drain($1) as moved offset 0) drained cross join lateral pg_sleep(case when drained.moved > 0 then 0.15 else 0 end)`, drainBatch)'
    after=$(grep -cF "$slept" "$changes" || true)
    if [ "$before" != 1 ] || [ "$after" != 1 ]; then
      echo "FAIL r7 sleep: the edit of the drain's statement did not take (lines of the drain before: $before, of the drain with its sleep after: $after)"
      exit 1
    fi
    echo "RESULT r7 sleep edit delivery/changes.go:$(grep -nF "$slept" "$changes" | tr -d '\t')"
    (cd "$work/sleep-src/packages/envoy" && go build -ldflags "-X main.buildCommit=$commit" -o "$work/bin/server-sleep" ./cmd/dispatch)
    ;;
esac
mark "built the server, the sleep control's server, legionbench and its test binary at $commit"

# The migration: the server booted once on the empty database. It refuses to start without an
# agent token, and nothing sends one: a value drawn for this run is enough.
agent_token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
(cd "$work/run" && exec env -i PATH="$PATH" HOME="$work/run" DATABASE_URL="$url" DISPATCH_AGENT_TOKEN="$agent_token" \
  DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_PORT=58111 DISPATCH_WEB_DIST="$work/run" DISPATCH_NATS_DISABLED=1 "$work/bin/server" >"$work/server-migrate.log" 2>&1) &
server_pid=$!
started=$SECONDS
until curl -fsS http://127.0.0.1:58111/healthz >/dev/null 2>&1; do
  if ! kill -0 "$server_pid" 2>/dev/null || [ $((SECONDS - started)) -gt 900 ]; then
    echo "FAIL r7 the migrating server did not serve; the end of its log:"
    tail -n 40 "$work/server-migrate.log"
    exit 1
  fi
  sleep 1
done
kill -TERM "$server_pid"
wait "$server_pid" || true
server_pid=""
mark "migrated by the server"

# The load. The daemon's address is under .invalid, which no name server answers (RFC 2606): the
# sampler's read of it fails each minute and adds no observation to the loaded history.
"$work/bin/legionbench" load -database "$url" -state-url "http://legion-daemon.invalid:13370" -ten-times -first-read \
  -scale "${R7_SCALE:-1}" -seed "${R7_SEED:-5577}" -now "$now"
psql "$url" -Atc "select 'RESULT r7 size database=' || pg_size_pretty(pg_database_size(current_database()))"
mark "loaded"

status=0
first=1
for arm in ${R7_ARMS:-stated recorded sleep}; do
  server=$work/bin/server rate=1 gate=$arm
  case "$arm" in
    stated) ;;
    recorded) rate=1.5 ;;
    sleep) server=$work/bin/server-sleep ;;
    *)
      echo "FAIL r7 no arm is named $arm"
      status=1
      continue
      ;;
  esac
  if [ "$first" != 1 ]; then
    # Each arm measures a first whole read: the consumer's rows, filings, cursors, record and
    # outbox emptied by `truncate`, which fires no row trigger and so logs no change of its own.
    psql "$url" -Atc 'truncate issue_facts, issue_filings, issue_facts_cursor, issue_facts_computed, delivery_changes_pending'
  fi
  first=0
  mark "arm $arm: server $server, rate $rate"
  rc=0
  LEGIONBENCH_DATABASE_URL="$url" LEGIONBENCH_SERVER="$server" LEGIONBENCH_WORK="$work/run" LEGIONBENCH_ARM="$arm" \
    LEGIONBENCH_GATE="$gate" LEGIONBENCH_RATE="$rate" LEGIONBENCH_AFTER="${R7_AFTER:-5m}" \
    LEGIONBENCH_READ_TIMEOUT="${R7_READ_TIMEOUT:-30m}" \
    "$work/bin/legionbench.test" -test.run '^TestFarmChangeLockHolds$' -test.v -test.count=1 -test.timeout 3h || rc=$?
  mark "arm $arm exits $rc"
  if [ "$arm" = sleep ]; then
    # The sleep gate passes the test only when the drain missed R7's hold bound over at least 40
    # takes, so the control's verdict is the test's exit; a test that stopped before it judged the
    # holds (a failed setup, which prints no FAIL line of its own) is a control that did not fail.
    if [ "$rc" -eq 0 ]; then
      echo "RESULT r7 sleep control failed on the hold as it must (the arm's lines above say by how much)"
    else
      echo "FAIL r7 sleep control did not fail on the drain's hold alone (the test exits $rc; the arm's lines above say why)"
    fi
  fi
  if [ "$rc" -ne 0 ]; then status=1; fi
done
mark "r7.sh exits $status"
exit "$status"
