#!/usr/bin/env bash
# The baseline half of an upgrade check, shared by the two checks that start from a database the
# previous release wrote: `c-decisions-converted` and `c-derived`. It boots a Dispatch server
# built from an older commit on a database of this run's own, so that the OLD release's
# `Store.Migrate` writes the schema. The caller then boots this branch's own server over that
# same database, and this branch's `Store.Migrate` runs over the old schema, which is the upgrade
# both checks read.
#
# Sourced, as the drivers here source lib.sh — which this file sources in turn, for `require_env`
# and `require_commands`. It sets `set -euo pipefail`, as every driver here does. It reads the
# caller's `usage` when the caller has one and defines its own otherwise, for its --selftest run
# below. The three functions it gives the caller:
#
#   baseline_start
#       Inputs: BASELINE_SHA, the commit the baseline server is built from (any revision this
#       repository resolves to a commit), and LEGION_TEST_PG_DSN, a PostgreSQL server this run may
#       create a database on. The database LEGION_TEST_PG_DSN itself names is only connected to,
#       to create and to drop this run's own database, and nothing in it is ever written.
#       LEGION_WORKSPACE names the checkout the commit is read from; unset, the checkout this file
#       is part of is read instead.
#       It takes the commit's tree with `git archive` into a directory of its own under TMPDIR —
#       a read of the repository: no worktree, no checkout, no write of any kind — builds
#       `./cmd/dispatch` from that tree's packages/envoy, creates a database named for this run
#       alone, and boots the binary on it. It returns once the server has reported that it is
#       listening, and leaves the caller
#           baseline_database_url   the database it made, which this branch's server then takes as
#                                   its own DATABASE_URL
#           baseline_url            the baseline server's address, http://127.0.0.1:<port>
#           baseline_token          the agent bearer that server accepts, for a driver that writes
#                                   the old release's rows through its API. It is printed nowhere.
#       It prints the first two on stdout, one per line.
#
#   baseline_stop_server
#       Stops the baseline server and KEEPS the database. This is the one a caller calls before it
#       boots this branch's server over that database: the database outlives the server that made
#       it, and goes only when the caller asks.
#
#   baseline_stop
#       Stops the baseline server, drops the database and removes the work directory. This is the
#       one a caller's cleanup trap calls, so that a failed run leaves no database, no server and
#       no build directory behind.
#
# Both stops are safe to call twice and safe to call when `baseline_start` refused or failed part
# way: each one stops and removes what this run actually made, and nothing else.
#
# A missing or bad input is refused on stderr with status 2, as lib.sh's own refusals are; a step
# of the harness that cannot do its job — the archive, the build, the database, a server that
# never listens — prints what failed with the tail of that server's log and exits 1.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.
#
# --selftest
#
#   `BASELINE_SHA=<commit> bash e2e/acceptance/lib-baseline.sh --selftest`, from packages/dispatch,
#   proves the harness end to end: it starts the baseline server, prints the high-water mark of
#   the `schema_migrations` table that server migrated, stops that server while keeping the
#   database, builds and boots this branch's own server over the same database, prints the mark
#   again, and passes when the baseline mark is at or below the branch mark. Equal is the expected
#   state of a branch no migration has landed on yet and passes; the run says which of the two it
#   is, so that a reader sees the upgrade once a child's migration lands. A baseline mark above the
#   branch mark fails: this branch would be missing a migration the previous release already has.
#   It prints one `PASS:` or `FAIL:` line per assertion, exits 2 on a missing input, and exits 1 on
#   the first failure, as the drivers in this directory do.

set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# The caller's usage stands; this one is for the --selftest run, which has no caller.
if ! declare -F usage >/dev/null; then
  usage() {
    cat >&2 <<'USAGE'
Usage: BASELINE_SHA=<commit> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<db>?sslmode=disable \
         bash e2e/acceptance/lib-baseline.sh --selftest
       (LEGION_WORKSPACE names the checkout BASELINE_SHA is read from; unset, this file's own.
       Sourced instead, it gives baseline_start, baseline_stop_server and baseline_stop)
USAGE
  }
fi

# What this run made, each empty until it exists, so that every stop below knows what to undo.
baseline_workspace=""
baseline_commit=""
baseline_work=""
baseline_database=""
baseline_database_url=""
baseline_url=""
baseline_token=""
baseline_signin_key=""
baseline_pid=""
# The log of the server baseline_boot started last, whose tail a failure prints.
baseline_log=""
# What baseline_boot leaves its caller.
baseline_boot_pid=""
baseline_boot_url=""

# How long a server gets to report that it is listening. Its first boot migrates a whole schema
# from empty, which is the longest this wait ever has to cover.
readonly baseline_listen_limit=180

# baseline_refuse WORDS…: a missing or bad input. It prints WORDS on stderr and exits the calling
# script with 2, as lib.sh's require_env does.
baseline_refuse() {
  printf 'lib-baseline.sh: %s\n' "$*" >&2
  usage
  exit 2
}

# baseline_fail WORDS…: a step of the harness that could not do its job. It prints WORDS and the
# last lines of the server log, when there is one, and exits the calling script with 1. The server
# names its own refusal there, and a failure that does not say which one is unactionable.
baseline_fail() {
  printf 'lib-baseline.sh: %s\n' "$*" >&2
  if [[ -n "$baseline_log" && -s "$baseline_log" ]]; then
    printf 'lib-baseline.sh: the last lines of %s:\n' "$baseline_log" >&2
    tail -n 20 "$baseline_log" >&2
  fi
  exit 1
}

# baseline_psql URL ARGS…: psql against URL, stopping on the first error. PGHOSTADDR is dropped:
# libpq reads it as the address to connect to and it would replace the host the URL names, while a
# server, which never reads it, connects to the host this harness admitted.
baseline_psql() {
  local url="$1"
  shift
  (
    unset PGHOSTADDR
    exec psql "$url" -X -v ON_ERROR_STOP=1 "$@"
  )
}

# baseline_dsn_for NAME: prints LEGION_TEST_PG_DSN with its database name replaced by NAME, so that
# this run's own database is reached on the server the caller named and with its credentials.
baseline_dsn_for() {
  local name="$1" rest="$LEGION_TEST_PG_DSN" query=""
  case "$rest" in
    postgres://*/* | postgresql://*/*) ;;
    *) baseline_refuse "LEGION_TEST_PG_DSN must be a postgres:// or postgresql:// URL naming a database" ;;
  esac
  case "$rest" in
    *\?*)
      query="?${rest#*\?}"
      rest="${rest%%\?*}"
      ;;
  esac
  printf '%s/%s%s\n' "${rest%/*}" "$name" "$query"
}

# baseline_high_water URL: prints the high-water mark of that database's schema_migrations table,
# the version of the last migration applied, and 0 for a database no migration has touched.
baseline_high_water() {
  baseline_psql "$1" -tAc 'select coalesce(max(version), 0) from schema_migrations' ||
    baseline_fail "could not read schema_migrations"
}

# baseline_boot NAME BINARY DATABASE_URL: starts BINARY against DATABASE_URL, logging to
# $baseline_work/logs/NAME.log, and waits for the server's own
#     msg="dispatch: listening" addr=127.0.0.1:<port>
# line. It leaves baseline_boot_pid, the server's pid, and baseline_boot_url, its address.
#
# The port is one the kernel chose and scripts/e2e/lib/held-port.py holds from that choice until
# the server serves on it (DISPATCH_LISTEN_FD), so two runs at once cannot collide and no other
# process can take the port in between. held-port.py executes the server in its own process, so
# the pid is the server's and a signal to it reaches the server.
#
# The server is a test fixture and inherits nothing from the shell that started it: a Legion pane
# exports the real Dispatch and the real Envoy listener, and an operator's shell can hold an nkey
# for the real NATS. Every exported variable goes, and the server gets only what is named here.
# ENVOY_URL names port 1, which nothing in a container can listen on, so every delivery this server
# attempts is refused at once and none reaches a real listener; this harness proves a migration,
# not a delivery.
#
# Identity is the production cookie, minted at the dev sign-in route. Dev sign-in signs it with a
# key held for this run alone, so nothing reaches a persistent data directory — without it the
# server writes a signing key under HOME, which names nothing here. Every server this harness
# boots gets that one key, because they serve one database: a process holding a key of its own is
# refused registration by the one already live.
baseline_boot() {
  local name="$1" binary="$2" database_url="$3"
  local held="$baseline_workspace/scripts/e2e/lib/held-port.py"
  [[ -r "$held" ]] || baseline_fail "$held is missing: it holds the port each server here serves on"
  baseline_log="$baseline_work/logs/$name.log"
  : >"$baseline_log" || baseline_fail "could not write $baseline_log"
  (
    set +x
    local variable saved_path="$PATH"
    local -a exported
    mapfile -t exported < <(compgen -e)
    for variable in "${exported[@]}"; do
      unset "$variable" 2>/dev/null || true
    done
    export PATH="$saved_path"
    export HOME=/nonexistent
    export XDG_CACHE_HOME=/nonexistent
    export XDG_CONFIG_HOME=/nonexistent
    export XDG_DATA_HOME=/nonexistent
    # shellcheck disable=SC2030 # the server's TMPDIR is this run's own work directory; the shell
    # that started it keeps the caller's, which the line in baseline_start below reads
    export TMPDIR="$baseline_work"
    export DATABASE_URL="$database_url"
    export DISPATCH_AGENT_TOKEN="$baseline_token"
    export DISPATCH_DEV_SIGNIN=1
    export DISPATCH_DEV_SIGNIN_SHARED_KEY="$baseline_signin_key"
    export DISPATCH_IDENTITY=cookie
    export DISPATCH_INSECURE_COOKIE=1
    export DISPATCH_LISTEN_HOST=127.0.0.1
    export DISPATCH_NATS_DISABLED=1
    export DISPATCH_WEB_DIST="$baseline_work/web-dist"
    export ENVOY_URL=http://127.0.0.1:1
    # held-port.py writes the port it holds in for {port}, and hands the socket over as
    # DISPATCH_LISTEN_FD.
    exec python3 "$held" exec --fd-env DISPATCH_LISTEN_FD \
      --env 'DISPATCH_PORT={port}' --env 'DISPATCH_SERVER_URL=http://127.0.0.1:{port}' \
      -- "$binary"
  ) >>"$baseline_log" 2>&1 &
  baseline_boot_pid=$!
  baseline_boot_url=""
  local waited=0 line=""
  while [[ "$waited" -lt "$baseline_listen_limit" ]]; do
    line="$(grep -m 1 'dispatch: listening' "$baseline_log" 2>/dev/null)" || line=""
    [[ -z "$line" ]] || break
    kill -0 "$baseline_boot_pid" 2>/dev/null ||
      baseline_fail "the $name server exited before it listened"
    sleep 1
    waited=$((waited + 1))
  done
  [[ -n "$line" ]] ||
    baseline_fail "the $name server did not report that it was listening within ${baseline_listen_limit}s"
  [[ "$line" =~ addr=([^[:space:]]+) ]] ||
    baseline_fail "the $name server's listening line names no address: $line"
  baseline_boot_url="http://${BASH_REMATCH[1]}"
}

# baseline_stop_pid PID: ends the process PID, with SIGKILL for one that does not go on SIGTERM
# within ten seconds. It is best effort, as the rig's own stop is (scripts/e2e/lib/rig.sh): a
# failed stop must never hide the check that failed. The trailing wait reaps the process, which is
# this shell's own child.
baseline_stop_pid() {
  local pid="${1:-}" waited=0
  [[ -n "$pid" ]] || return 0
  case "$pid" in
    *[!0-9]*) return 0 ;;
  esac
  kill -TERM "$pid" 2>/dev/null || return 0
  while [[ "$waited" -lt 50 ]]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
    waited=$((waited + 1))
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# baseline_stop_server: stops the baseline server and keeps its database, for a caller about to
# boot this branch's own server over that database.
baseline_stop_server() {
  baseline_stop_pid "$baseline_pid"
  baseline_pid=""
  baseline_url=""
}

# baseline_drop_database: drops this run's own database. FORCE ends a connection the stopped
# server has not closed yet, which would otherwise leave the database behind.
baseline_drop_database() {
  local refusal
  [[ -n "$baseline_database" ]] || return 0
  refusal="$(baseline_psql "$LEGION_TEST_PG_DSN" -q \
    -c "drop database if exists \"$baseline_database\" with (force)" 2>&1)" ||
    printf 'lib-baseline.sh: could not drop the database %s: %s\n' "$baseline_database" "$refusal" >&2
  baseline_database=""
  baseline_database_url=""
}

# baseline_stop: stops the baseline server, drops its database and removes the work directory.
baseline_stop() {
  baseline_stop_server
  baseline_drop_database
  [[ -z "$baseline_work" ]] || rm -rf "$baseline_work"
  baseline_work=""
  baseline_log=""
  baseline_token=""
  baseline_signin_key=""
}

# baseline_start: the baseline server of BASELINE_SHA, serving a database of this run's own. The
# head comment above is its contract.
baseline_start() {
  require_env BASELINE_SHA LEGION_TEST_PG_DSN
  require_commands git go openssl psql python3 sleep tar
  baseline_workspace="${LEGION_WORKSPACE:-}"
  if [[ -z "$baseline_workspace" ]]; then
    baseline_workspace="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)" ||
      baseline_refuse "could not find the checkout this file is part of; set LEGION_WORKSPACE"
  fi
  git -C "$baseline_workspace" rev-parse --git-dir >/dev/null 2>&1 ||
    baseline_refuse "$baseline_workspace holds no git repository to read BASELINE_SHA out of"
  baseline_commit="$(git -C "$baseline_workspace" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" ||
    baseline_refuse "BASELINE_SHA '$BASELINE_SHA' names no commit of $baseline_workspace"
  # The name is this run's alone: a second run, of this check or the other one, gets a database of
  # its own and neither run drops the other's.
  baseline_database="dispatch_baseline_$(date +%s)_$$"
  baseline_database_url="$(baseline_dsn_for "$baseline_database")"

  # shellcheck disable=SC2031 # this shell's own TMPDIR, which the subshell above never changed
  baseline_work="$(mktemp -d "${TMPDIR:-/tmp}/dispatch-baseline.XXXXXXXX")" ||
    baseline_fail "could not make a work directory under ${TMPDIR:-/tmp}"
  mkdir -p "$baseline_work/tree" "$baseline_work/bin" "$baseline_work/logs" "$baseline_work/web-dist" ||
    baseline_fail "could not make this run's directories under $baseline_work"
  # The commit's tree is taken out of the repository rather than checked out, so the working copy
  # is never touched and no worktree is added.
  git -C "$baseline_workspace" archive "$baseline_commit" | tar -x -C "$baseline_work/tree" ||
    baseline_fail "could not take the tree of $baseline_commit out of $baseline_workspace"
  go -C "$baseline_work/tree/packages/envoy" build -o "$baseline_work/bin/dispatch-baseline" ./cmd/dispatch ||
    baseline_fail "the ./cmd/dispatch of $baseline_commit did not build"

  baseline_psql "$LEGION_TEST_PG_DSN" -q -c "create database \"$baseline_database\"" >/dev/null ||
    baseline_fail "could not create the database $baseline_database on the server LEGION_TEST_PG_DSN names"
  baseline_token="$(openssl rand -hex 24)" || baseline_fail "could not make an agent bearer"
  baseline_signin_key="$(openssl rand -hex 32)" || baseline_fail "could not make a dev sign-in key"

  baseline_boot baseline-server "$baseline_work/bin/dispatch-baseline" "$baseline_database_url"
  baseline_pid="$baseline_boot_pid"
  baseline_url="$baseline_boot_url"
  printf 'baseline database %s\n' "$baseline_database_url"
  printf 'baseline server %s\n' "$baseline_url"
}

# Sourced, this file ends here. Everything below is the --selftest run.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

[[ "${1:-}" == "--selftest" && $# -eq 1 ]] || {
  printf 'lib-baseline.sh: --selftest is the one argument this script takes\n' >&2
  usage
  exit 2
}
# Both inputs are baseline_start's own, which refuses a missing one by name.

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

branch_pid=""
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  baseline_stop_pid "$branch_pid"
  branch_pid=""
  baseline_stop
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

baseline_start

baseline_mark="$(baseline_high_water "$baseline_database_url")" ||
  fail baseline "read the baseline database's schema_migrations high-water mark" "psql refused it"
printf 'baseline schema_migrations high-water mark: %s\n' "$baseline_mark"
[[ "$baseline_mark" =~ ^[0-9]+$ ]] ||
  fail baseline "the baseline high-water mark is a number" "got '$baseline_mark'"
[[ "$baseline_mark" -gt 0 ]] ||
  fail baseline "the baseline server migrated its own database" "schema_migrations is still at 0"
pass baseline "the server of $BASELINE_SHA migrated its own database to $baseline_mark"

# The server goes and the database stays: what this branch's server migrates next is the schema the
# old release wrote.
baseline_stop_server

go -C "$baseline_workspace/packages/envoy" build -o "$baseline_work/bin/dispatch-branch" ./cmd/dispatch ||
  fail upgrade "build this branch's ./cmd/dispatch" "go build exited non-zero"
baseline_boot branch-server "$baseline_work/bin/dispatch-branch" "$baseline_database_url"
branch_pid="$baseline_boot_pid"
pass upgrade "this branch's server booted over the baseline's database at $baseline_boot_url"

branch_mark="$(baseline_high_water "$baseline_database_url")" ||
  fail upgrade "read the branch's schema_migrations high-water mark" "psql refused it"
printf 'branch schema_migrations high-water mark: %s\n' "$branch_mark"
[[ "$branch_mark" =~ ^[0-9]+$ ]] ||
  fail upgrade "the branch high-water mark is a number" "got '$branch_mark'"

[[ "$baseline_mark" -le "$branch_mark" ]] ||
  fail upgrade "the baseline mark is at or below the branch mark" \
    "baseline $baseline_mark is above branch $branch_mark: this branch is missing a migration the baseline has"
# Equal is where a branch no migration has landed on stands, and it passes; the line says which of
# the two it is, so that the upgrade shows the moment a child's migration lands. A version is the
# migration's own timestamp rather than a count, so the migrations this branch applied over the old
# schema are counted and not taken as the difference of the two marks.
if [[ "$baseline_mark" -eq "$branch_mark" ]]; then
  relation="the two marks are equal at $branch_mark: no migration of this branch has landed yet"
else
  landed="$(baseline_psql "$baseline_database_url" -tAc \
    "select count(*) from schema_migrations where version > $baseline_mark")" ||
    fail upgrade "count the migrations this branch applied over the baseline's schema" "psql refused it"
  relation="the branch is ahead by $landed migrations: baseline $baseline_mark, branch $branch_mark"
fi
printf '%s\n' "$relation"
pass upgrade "baseline $baseline_mark is at or below branch $branch_mark"
