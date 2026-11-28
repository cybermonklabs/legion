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
#       Inputs. BASELINE_SHA or BASELINE_BINARY must be set; LEGION_WORKSPACE and
#       BASELINE_EXTRA_ENV are optional. The database and the bearer follow them.
#
#       BASELINE_SHA, the commit the baseline server is built from (any revision this repository
#       resolves to a commit): the release this branch lands after. It has no default. Unset or
#       empty, with BASELINE_BINARY unset too, the run is refused by name before anything is read
#       out of the checkout or made. A default of the merge base with main would be older than any
#       sibling branch this one is stacked on, so a check built on it could pass without being
#       able to fail; only the caller knows which release the branch lands after.
#
#       LEGION_WORKSPACE names the checkout the commit is read from, whose
#       scripts/e2e/lib/held-port.py also holds each server's port; unset, the checkout this file
#       is part of is read instead.
#
#       BASELINE_BINARY, the path of a baseline server already built (a `./cmd/dispatch` binary).
#       Set, it is the server booted, and nothing is built for it: no `git archive` and no
#       `go build`, so a caller that runs several checks against one baseline builds it once, not
#       once a run. No tree is read then, so BASELINE_SHA is not read either, and a checkout whose
#       commit cannot be resolved is no refusal. The harness cannot tell which commit a binary
#       came from, so it names the binary and never a commit, and it stamps none on it: the binary
#       is booted exactly as its caller built it and carries whatever stamp that build gave it,
#       which may be none. The build paragraph below says what a missing stamp costs. A path that
#       is not an executable file is refused by name before anything is made; a relative one is
#       taken from the directory baseline_start is called in.
#
#       BASELINE_EXTRA_ENV, NAME=value pairs each server gets on top of what the harness gives it.
#       baseline_boot clears every exported variable before it starts a server, so a setting the
#       caller needs there (a test hook, say) reaches it this way or not at all. One pair per
#       line, so that a value may hold spaces, though never a newline; a line that is empty or all
#       whitespace is skipped. NAME is everything before the line's first `=` and must be a shell
#       identifier ([A-Za-z_][A-Za-z0-9_]*); the value is everything after it, kept as written. A
#       line that is not NAME=value with such a NAME is refused, naming its number and its NAME
#       but never its value, which may be a secret. A NAME the harness sets itself is refused by
#       name: DATABASE_URL, DISPATCH_AGENT_TOKEN, DISPATCH_DEV_SIGNIN,
#       DISPATCH_DEV_SIGNIN_SHARED_KEY, DISPATCH_IDENTITY, DISPATCH_INSECURE_COOKIE,
#       DISPATCH_LISTEN_HOST, DISPATCH_NATS_DISABLED, DISPATCH_WEB_DIST, ENVOY_URL, HOME, PATH,
#       TMPDIR, XDG_CACHE_HOME, XDG_CONFIG_HOME and XDG_DATA_HOME, which baseline_boot exports, and
#       DISPATCH_LISTEN_FD, DISPATCH_PORT and DISPATCH_SERVER_URL, which held-port.py sets. The
#       harness sets each one on purpose, and a pair that silently replaced one would change what
#       the check proves: a DATABASE_URL pair would leave the check reading a different database
#       than the one the servers wrote, the failure the paragraph on DATABASE_URL below guards
#       against. Every server baseline_boot starts gets the pairs, this branch's server under
#       --selftest too: both serve one database, and whatever made the caller need the setting
#       holds for both.
#
#       The database is the run's when the run gives one. `c-decisions-converted` and `c-derived`
#       each drop and create an empty database of their own and hand it to their driver as
#       DATABASE_URL, so with DATABASE_URL set this harness boots on that database, creates none,
#       and drops none: the run that made it owns it. Creating a second one here would either
#       collide with the run's or quietly leave the check reading a different database than the
#       one the servers wrote. With DATABASE_URL unset, as under --selftest, the harness creates a
#       database named for this run alone on the server LEGION_TEST_PG_DSN names, connecting to
#       the database that DSN names only to create and drop its own, and drops it again in
#       baseline_stop.
#
#       The agent bearer is the run's when the run gives one. The two checks pass
#       DISPATCH_TOKEN=e2e-token and boot every server with DISPATCH_AGENT_TOKEN set to that bearer,
#       so with DISPATCH_TOKEN set both servers accept exactly it; unset, the harness makes a random
#       one. A driver calls baseline_start before lib.sh's read_bearer, which unsets DISPATCH_TOKEN.
#
#       Without BASELINE_BINARY, it takes the commit's tree with `git archive` into a directory of
#       its own under TMPDIR — a read of the repository: no worktree, no checkout, no write of any
#       kind — and builds `./cmd/dispatch` from that tree's packages/envoy, stamped with that
#       commit through the flag the image build uses (`-ldflags "-X main.buildCommit=<commit>"`,
#       packages/envoy/docker/Dockerfile). The stamp is the commit the server registers in
#       server_processes as the one it serves, and a plain `go build` leaves it empty. Dispatch
#       takes a fleet as agreeing on a commit only when every live process serves the same
#       non-empty one (servedByEvery, packages/envoy/internal/dispatch/startup/started.go), so
#       while an unstamped build is live no build announcement is written, and a reader that
#       groups live processes by commit cannot tell two unstamped builds apart. It boots that
#       binary, or BASELINE_BINARY's, on the database. It returns once the server has reported
#       that it is listening, and leaves the caller
#           baseline_database_url   the database the servers serve, which this branch's server then
#                                   takes as its own DATABASE_URL
#           baseline_url            the baseline server's address, http://127.0.0.1:<port>
#           baseline_token          the agent bearer both servers accept, for a driver that writes
#                                   the old release's rows through its API. It is printed nowhere.
#           baseline_commit         the commit the baseline was built from and stamped with, the
#                                   one BASELINE_SHA named, in full; empty when BASELINE_BINARY
#                                   was booted
#           baseline_binary         the absolute path of the binary booted as the baseline
#       It prints, one per line on stdout, `baseline commit <commit>` (or `baseline binary <path>`)
#       and then the first two, so that a run's log names its baseline in full even when
#       BASELINE_SHA named it by a branch or a short hash.
#
#   baseline_stop_server
#       Stops the baseline server and KEEPS the database. This is the one a caller calls before it
#       boots this branch's server over that database: the database outlives the server that made
#       it, and goes only when the caller asks.
#
#   baseline_stop
#       Stops the baseline server, drops the database when this harness made it, and removes the
#       work directory. This is the one a caller's cleanup trap calls, so that a failed run leaves
#       no server and no build directory behind, and no database of its own making.
#
# Both stops are safe to call twice and safe to call when `baseline_start` refused or failed part
# way: each one stops and removes what this run actually made, and nothing else — never a
# database the run handed in.
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
#   `bash e2e/acceptance/lib-baseline.sh --selftest`, from packages/dispatch, takes
#   baseline_start's inputs above, of which BASELINE_SHA (or BASELINE_BINARY) and a database
#   (LEGION_TEST_PG_DSN, or the run's DATABASE_URL) are required. With neither BASELINE_SHA nor
#   BASELINE_BINARY set it is refused at once, with status 2, and starts nothing. Before it starts
#   any server it proves that refusal: baseline_start, run in a subshell with neither set, must
#   exit 2, and the first line it prints on stderr must name BASELINE_SHA. Then it proves the
#   harness end to end: it starts the baseline server, reads the version of every migration that
#   server applied out of the `schema_migrations` table, stops that server while keeping the
#   database, builds and boots this branch's own server over the same database, and reads the
#   versions again. A baseline that applied none fails: it did not migrate its own database. Then
#   the two sets of versions are compared. A version the baseline's set holds and the branch's
#   lacks fails the run, which names every such version: this branch would be missing a migration
#   the previous release already has, and since both sets are read from one database, whose
#   schema_migrations a migration only adds to, a server deleted it. Otherwise the run passes and
#   names each version the branch's set adds, on a line of its own, and how many there are: the
#   migrations this branch applied over the old schema, so that a reader sees the upgrade once a
#   child's migration lands. A branch that added none says so and passes: that is where a branch
#   no migration has landed on yet stands.
#
#   The sets are compared, never their high-water marks, the highest version in each. Dispatch
#   applies each migration by its own version alone (applyMigration, in
#   packages/envoy/internal/dispatch/store/store.go, asks whether that one version is in
#   schema_migrations), so a migration of this branch dated before the baseline's newest is applied
#   without moving the mark, and a comparison of marks reads that upgrade as no migration landed.
#   That happens whenever the baseline holds a migration dated after one this branch adds, as when
#   this branch is stacked on a merge whose newest migration was written after this branch's. The
#   comparison is baseline_compare_versions, defined above the selftest so that a script can source
#   this file and feed it two lists of versions without booting a server.
#
#   The baseline's line names the commit it was built from, or the binary BASELINE_BINARY gave.
#   This branch's server is built whatever the baseline was, from the checkout's working copy, and
#   stamped as the baseline build is, with the commit `git rev-parse HEAD` names there, so that
#   each server registers the commit it came from; a HEAD that names no commit fails the run, and
#   the stamp names that commit even when the working copy holds edits it does not. Building and
#   stamping this server needs go and git even with BASELINE_BINARY set, and it gets
#   BASELINE_EXTRA_ENV's pairs as the baseline did. It prints one `PASS:` or `FAIL:` line per
#   assertion, exits 2 on a missing or bad input, and exits 1 on the first failure, as the drivers
#   in this directory do.

set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# The caller's usage stands; this one is for the --selftest run, which has no caller.
if ! declare -F usage >/dev/null; then
  usage() {
    cat >&2 <<'USAGE'
Usage: {BASELINE_SHA=<commit> | BASELINE_BINARY=<path>} [BASELINE_EXTRA_ENV=$'NAME=value\n...'] \
         LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<db>?sslmode=disable \
         bash e2e/acceptance/lib-baseline.sh --selftest
       (BASELINE_SHA is the commit of the release this branch lands after, and has no default:
       only the caller knows which release that is. BASELINE_BINARY, set instead, boots that
       prebuilt server and builds none. BASELINE_EXTRA_ENV is one NAME=value per line, given to
       every server. LEGION_WORKSPACE names the checkout the commit is read from; unset, this
       file's own. Sourced instead, it gives baseline_start, baseline_stop_server and baseline_stop)
USAGE
  }
fi

# What this run made, each empty until it exists, so that every stop below knows what to undo.
baseline_workspace=""
baseline_commit=""
# The server booted as the baseline: BASELINE_BINARY's, or the one built from baseline_commit.
baseline_binary=""
# BASELINE_EXTRA_ENV's NAME=value pairs, each checked, which baseline_boot gives every server.
baseline_extra_env=()
baseline_work=""
baseline_database=""
baseline_database_url=""
# 1 when this harness created the database and so drops it; empty when the run handed one in.
baseline_database_owned=""
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

# The names BASELINE_EXTRA_ENV may not set: every variable baseline_boot exports itself, and the
# three held-port.py sets after it. The head comment lists the same names, for the caller who reads
# only it, and says why each is refused; a name baseline_boot comes to export belongs in both.
readonly -a baseline_harness_names=(
  DATABASE_URL DISPATCH_AGENT_TOKEN DISPATCH_DEV_SIGNIN DISPATCH_DEV_SIGNIN_SHARED_KEY
  DISPATCH_IDENTITY DISPATCH_INSECURE_COOKIE DISPATCH_LISTEN_FD DISPATCH_LISTEN_HOST
  DISPATCH_NATS_DISABLED DISPATCH_PORT DISPATCH_SERVER_URL DISPATCH_WEB_DIST ENVOY_URL HOME PATH
  TMPDIR XDG_CACHE_HOME XDG_CONFIG_HOME XDG_DATA_HOME
)

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

# baseline_applied_versions URL: prints the version of every migration applied to that database,
# out of its schema_migrations table, one per line and sorted as text (LC_ALL=C sort), the order
# baseline_compare_versions takes them in; nothing for a database no migration has touched.
baseline_applied_versions() {
  baseline_psql "$1" -tAc 'select version from schema_migrations' | LC_ALL=C sort ||
    baseline_fail "could not read schema_migrations"
}

# baseline_compare_versions BASELINE BRANCH: compares two sets of applied migration versions, each
# a list of versions one per line, sorted as text as baseline_applied_versions prints it: BASELINE,
# read once the baseline server migrated the database, and BRANCH, read once this branch's server
# booted over it. When BASELINE holds versions BRANCH lacks, it prints how many and then each one on
# a line of its own, and returns 1: this branch would be missing a migration the previous release
# already has, and since both lists are read from one database, whose schema_migrations a migration
# only adds to, a server deleted it. Otherwise it prints how many versions BRANCH adds and then each
# one, the migrations this branch applied over the old schema, or says that it adds none, and
# returns 0. A comparison comm cannot make returns 1 too, and says so.
#
# The sets are compared, never their high-water marks. Dispatch applies each migration by its own
# version alone: applyMigration, in packages/envoy/internal/dispatch/store/store.go, asks whether
# that one version is in schema_migrations and applies the migration when it is not, whatever newer
# versions are there. A migration of this branch dated before the baseline's newest is then applied
# without moving the mark, and a comparison of marks would read that upgrade as no migration landed.
#
# comm compares lines as text and reads each list in order, so a list sorted any other way,
# numerically say, would be misread; comm reports a list it finds out of order and fails. Each list
# reaches comm through printf '%s', never '%s\n', so that an empty list is no line at all rather
# than one empty line: comm reads a last line with no newline after it as a line.
baseline_compare_versions() {
  local missing added
  local -a versions
  missing="$(LC_ALL=C comm -23 <(printf '%s' "$1") <(printf '%s' "$2"))" || {
    printf 'comm could not compare the two lists of versions, which must be sorted as text\n'
    return 1
  }
  if [[ -n "$missing" ]]; then
    mapfile -t versions < <(printf '%s' "$missing")
    printf "baseline versions this branch's database lacks: %d\n" "${#versions[@]}"
    printf '  %s\n' "${versions[@]}"
    return 1
  fi
  added="$(LC_ALL=C comm -13 <(printf '%s' "$1") <(printf '%s' "$2"))" || {
    printf 'comm could not compare the two lists of versions, which must be sorted as text\n'
    return 1
  }
  if [[ -z "$added" ]]; then
    printf 'this branch added no migration over the baseline\n'
    return 0
  fi
  mapfile -t versions < <(printf '%s' "$added")
  printf 'versions this branch added over the baseline: %d\n' "${#versions[@]}"
  printf '  %s\n' "${versions[@]}"
}

# baseline_require_sha_or_binary: refuses a run that names no baseline, with BASELINE_SHA and
# BASELINE_BINARY both unset or empty. Nothing stands in for them, for the reason the head comment
# gives under BASELINE_SHA. baseline_start calls it first, before it reads the checkout or makes
# anything, and the --selftest run below calls it before anything else too.
baseline_require_sha_or_binary() {
  [[ -n "${BASELINE_SHA:-}" || -n "${BASELINE_BINARY:-}" ]] ||
    baseline_refuse "BASELINE_SHA is unset: set it to the commit of the release this branch" \
      "lands after (any revision this repository resolves to one), or set BASELINE_BINARY to a" \
      "server already built from it. It has no default: only the caller knows that release"
}

# baseline_read_extra_env: checks BASELINE_EXTRA_ENV line by line and keeps its pairs in
# baseline_extra_env, for baseline_boot to give every server. baseline_start calls it before it
# makes anything: baseline_boot clears every exported variable, BASELINE_EXTRA_ENV among them when
# the caller exported it, so its value is read here or never. A refusal names the line by its number
# and its NAME, never by its value, which may be a secret, as the bearer is printed nowhere.
baseline_read_extra_env() {
  local line name reserved number=0
  baseline_extra_env=()
  [[ -n "${BASELINE_EXTRA_ENV:-}" ]] || return 0
  while IFS= read -r line; do
    number=$((number + 1))
    [[ "$line" =~ [^[:space:]] ]] || continue
    [[ "$line" == *=* ]] ||
      baseline_refuse "BASELINE_EXTRA_ENV line $number holds no '=':" \
        "each line is one NAME=value pair"
    name="${line%%=*}"
    [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] ||
      baseline_refuse "BASELINE_EXTRA_ENV line $number names '$name'," \
        "which is not a shell identifier ([A-Za-z_][A-Za-z0-9_]*)"
    for reserved in "${baseline_harness_names[@]}"; do
      [[ "$name" != "$reserved" ]] ||
        baseline_refuse "BASELINE_EXTRA_ENV line $number names $name," \
          "which the harness sets itself for every server it boots"
    done
    baseline_extra_env+=("$line")
  done <<<"$BASELINE_EXTRA_ENV"
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
# for the real NATS. Every exported variable goes, and the server gets only what is named here and
# the caller's BASELINE_EXTRA_ENV pairs, which baseline_start read and checked before this could
# clear that variable too. ENVOY_URL names port 1, which nothing in a container can listen on, so
# every delivery this server attempts is refused at once and none reaches a real listener; this
# harness proves a migration, not a delivery.
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
    # shellcheck disable=SC2031 # this shell's own PATH, which lib.sh's subshell never changed
    local variable saved_path="$PATH"
    local -a exported
    mapfile -t exported < <(compgen -e)
    for variable in "${exported[@]}"; do
      unset "$variable" 2>/dev/null || true
    done
    # shellcheck disable=SC2031 # the PATH saved above, which lib.sh's subshell never changed
    export PATH="$saved_path"
    # shellcheck disable=SC2031 # the server's HOME, set here; lib.sh's subshell sets its own
    export HOME=/nonexistent
    export XDG_CACHE_HOME=/nonexistent
    export XDG_CONFIG_HOME=/nonexistent
    export XDG_DATA_HOME=/nonexistent
    # shellcheck disable=SC2030 # the server's TMPDIR is this run's own work directory; the shell
    # that started it keeps the caller's, which the line in baseline_start below reads
    # shellcheck disable=SC2031 # the server's TMPDIR, set here; lib.sh's subshell sets its own
    export TMPDIR="$baseline_work"
    # shellcheck disable=SC2030 # the server's DATABASE_URL is the database it serves; the shell
    # that started it keeps the run's, which baseline_start below reads before any server boots
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
    # DISPATCH_LISTEN_FD. The caller's pairs go into the environment through env and never through
    # a variable of this shell, so that a NAME this shell uses too (binary, held, an array) reaches
    # the server as the caller wrote it and changes nothing here. None can replace one of the
    # harness's own names, which baseline_read_extra_env refused.
    exec env "${baseline_extra_env[@]}" python3 "$held" exec --fd-env DISPATCH_LISTEN_FD \
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

# baseline_drop_database: drops the database when this harness made it. FORCE ends a connection
# the stopped server has not closed yet, which would otherwise leave the database behind. A
# database the run handed in is the run's to drop: this forgets it and touches nothing.
baseline_drop_database() {
  local refusal
  if [[ -z "$baseline_database_owned" ]]; then
    baseline_database=""
    baseline_database_url=""
    return 0
  fi
  [[ -n "$baseline_database" ]] || return 0
  refusal="$(baseline_psql "$LEGION_TEST_PG_DSN" -q \
    -c "drop database if exists \"$baseline_database\" with (force)" 2>&1)" ||
    printf 'lib-baseline.sh: could not drop the database %s: %s\n' "$baseline_database" "$refusal" >&2
  baseline_database=""
  baseline_database_url=""
  baseline_database_owned=""
}

# baseline_stop: stops the baseline server, drops the database when this harness made it, and
# removes the work directory.
baseline_stop() {
  baseline_stop_server
  baseline_drop_database
  [[ -z "$baseline_work" ]] || rm -rf "$baseline_work"
  baseline_work=""
  baseline_log=""
  baseline_token=""
  baseline_signin_key=""
}

# baseline_start: the baseline server of BASELINE_SHA or of BASELINE_BINARY, serving the run's
# database or one of its own. The head comment above is its contract.
baseline_start() {
  # Read before anything else: a driver that calls lib.sh's read_bearer after this unsets
  # DISPATCH_TOKEN, and the run's DATABASE_URL must be the one every server here is booted on.
  # shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
  local given_database_url="${DATABASE_URL:-}" given_token="${DISPATCH_TOKEN:-}"
  local given_binary="${BASELINE_BINARY:-}" sha="${BASELINE_SHA:-}"
  # Refused before anything is read out of the checkout or made: a run that names no baseline has
  # nothing to build or boot, and none is guessed for it.
  baseline_require_sha_or_binary
  [[ -n "$given_database_url" ]] || require_env LEGION_TEST_PG_DSN
  # A binary given is booted as it is, with whatever stamp its caller's build gave it: nothing
  # reads the repository and nothing is built.
  if [[ -n "$given_binary" ]]; then
    require_commands env openssl psql python3 sleep
  else
    require_commands env git go openssl psql python3 sleep tar
  fi
  baseline_read_extra_env
  # The checkout comes before the commit: BASELINE_SHA is resolved in it.
  baseline_workspace="${LEGION_WORKSPACE:-}"
  if [[ -z "$baseline_workspace" ]]; then
    baseline_workspace="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)" ||
      baseline_refuse "could not find the checkout this file is part of; set LEGION_WORKSPACE"
  fi
  if [[ -n "$given_binary" ]]; then
    [[ -f "$given_binary" && -x "$given_binary" ]] ||
      baseline_refuse "BASELINE_BINARY '$given_binary' is not an executable file"
    # Absolute, so that held-port.py, which executes it, never looks a bare name up on PATH, and a
    # caller that changes directory before a boot still boots this file.
    case "$given_binary" in
      /*) baseline_binary="$given_binary" ;;
      *) baseline_binary="$PWD/$given_binary" ;;
    esac
    baseline_commit=""
  else
    git -C "$baseline_workspace" rev-parse --git-dir >/dev/null 2>&1 ||
      baseline_refuse "$baseline_workspace holds no git repository to read the commit out of"
    baseline_commit="$(git -C "$baseline_workspace" rev-parse --verify --quiet "${sha}^{commit}")" ||
      baseline_refuse "BASELINE_SHA '$sha' names no commit of $baseline_workspace"
  fi
  if [[ -n "$given_database_url" ]]; then
    # The run's own database, which it dropped and created empty: boot on it, make no other.
    baseline_database=""
    baseline_database_url="$given_database_url"
  else
    # The name is this run's alone: a second run, of this check or the other one, gets a
    # database of its own and neither run drops the other's.
    baseline_database="dispatch_baseline_$(date +%s)_$$"
    baseline_database_url="$(baseline_dsn_for "$baseline_database")"
  fi

  # shellcheck disable=SC2031 # this shell's own TMPDIR, which the subshell above never changed
  baseline_work="$(mktemp -d "${TMPDIR:-/tmp}/dispatch-baseline.XXXXXXXX")" ||
    baseline_fail "could not make a work directory under ${TMPDIR:-/tmp}"
  mkdir -p "$baseline_work/tree" "$baseline_work/bin" "$baseline_work/logs" "$baseline_work/web-dist" ||
    baseline_fail "could not make this run's directories under $baseline_work"
  if [[ -z "$given_binary" ]]; then
    # The commit's tree is taken out of the repository rather than checked out, so the working
    # copy is never touched and no worktree is added.
    git -C "$baseline_workspace" archive "$baseline_commit" | tar -x -C "$baseline_work/tree" ||
      baseline_fail "could not take the tree of $baseline_commit out of $baseline_workspace"
    # Stamped with its own commit, as the image build stamps it: unstamped, the server registers an
    # empty commit, which no fleet agrees on and no reader can tell from another unstamped build's.
    go -C "$baseline_work/tree/packages/envoy" build -ldflags "-X main.buildCommit=$baseline_commit" \
      -o "$baseline_work/bin/dispatch-baseline" ./cmd/dispatch ||
      baseline_fail "the ./cmd/dispatch of $baseline_commit did not build"
    baseline_binary="$baseline_work/bin/dispatch-baseline"
  fi

  if [[ -z "$given_database_url" ]]; then
    baseline_psql "$LEGION_TEST_PG_DSN" -q -c "create database \"$baseline_database\"" >/dev/null ||
      baseline_fail "could not create the database $baseline_database on the server LEGION_TEST_PG_DSN names"
    baseline_database_owned=1
  fi
  # The run's bearer when it gives one, since both checks boot every server with
  # DISPATCH_AGENT_TOKEN set to the DISPATCH_TOKEN they pass; a random one otherwise.
  baseline_token="$given_token"
  if [[ -z "$baseline_token" ]]; then
    baseline_token="$(openssl rand -hex 24)" || baseline_fail "could not make an agent bearer"
  fi
  baseline_signin_key="$(openssl rand -hex 32)" || baseline_fail "could not make a dev sign-in key"

  baseline_boot baseline-server "$baseline_binary" "$baseline_database_url"
  baseline_pid="$baseline_boot_pid"
  baseline_url="$baseline_boot_url"
  if [[ -n "$baseline_commit" ]]; then
    printf 'baseline commit %s\n' "$baseline_commit"
  else
    printf 'baseline binary %s\n' "$baseline_binary"
  fi
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
# A run that names no baseline is refused at once, as baseline_start refuses it, before the
# refusal is proved below and before anything else is checked or made.
baseline_require_sha_or_binary
# Every input is baseline_start's own, which refuses a missing or bad one by name. This run builds
# this branch's server itself whatever baseline_start boots, and stamps it with the commit HEAD
# names, so it needs git and go even when BASELINE_BINARY spares baseline_start its own build. It
# compares the two sets of versions with sort and comm.
require_commands comm git go sort

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# The refusal of a run that names no baseline, proved before any server starts: baseline_start, in
# a subshell with BASELINE_SHA and BASELINE_BINARY unset, must exit 2, and the first line it prints
# on stderr must name BASELINE_SHA. Only that line is read: the usage text after it names
# BASELINE_SHA whatever was refused. The subshell drops DATABASE_URL and LEGION_TEST_PG_DSN as well,
# so that it could make nothing even if the refusal ever moved below baseline_start's first check:
# lib.sh's require_env would then refuse first, naming LEGION_TEST_PG_DSN, and this step would fail
# naming that line.
refusal_status=0
refusal="$(
  unset BASELINE_SHA BASELINE_BINARY DATABASE_URL LEGION_TEST_PG_DSN
  baseline_start 2>&1 >/dev/null
)" || refusal_status=$?
refusal_line="${refusal%%$'\n'*}"
[[ "$refusal_status" -eq 2 ]] ||
  fail refusal "baseline_start with neither BASELINE_SHA nor BASELINE_BINARY exits 2" \
    "it exited $refusal_status, saying first: $refusal_line"
[[ "$refusal_line" == *BASELINE_SHA* ]] ||
  fail refusal "baseline_start with neither BASELINE_SHA nor BASELINE_BINARY names BASELINE_SHA" \
    "the first line it printed on stderr was: $refusal_line"
pass refusal \
  "baseline_start with neither BASELINE_SHA nor BASELINE_BINARY exits 2 and names BASELINE_SHA"

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

# The set of versions the baseline applied, which the set this branch's server leaves is compared
# with below: the sets and never their high-water marks, for the reason baseline_compare_versions
# gives.
baseline_versions="$(baseline_applied_versions "$baseline_database_url")" ||
  fail baseline "read the versions in the baseline database's schema_migrations" "psql refused it"
mapfile -t baseline_version_list < <(printf '%s' "$baseline_versions")
baseline_count="${#baseline_version_list[@]}"
printf 'baseline schema_migrations holds %d versions\n' "$baseline_count"
[[ "$baseline_count" -gt 0 ]] ||
  fail baseline "the baseline server migrated its own database" "schema_migrations is empty"
# The baseline is named by what was booted: the commit it was built from, which BASELINE_SHA named,
# or the binary BASELINE_BINARY gave, whose commit nothing here can vouch for.
if [[ -n "$baseline_commit" ]]; then
  baseline_booted="the server built from $baseline_commit"
else
  baseline_booted="the prebuilt server $baseline_binary"
fi
pass baseline "$baseline_booted migrated its own database: $baseline_count migrations applied"

# The server goes and the database stays: what this branch's server migrates next is the schema the
# old release wrote.
baseline_stop_server

# Stamped with the commit HEAD names, as the baseline build is with its own: unstamped, this server
# would register an empty commit, which no fleet agrees on and no reader can tell from another
# unstamped build's (the head comment says why).
branch_commit="$(git -C "$baseline_workspace" rev-parse HEAD)" ||
  fail upgrade "read the commit this branch's server is stamped with" \
    "git rev-parse HEAD failed in $baseline_workspace"
go -C "$baseline_workspace/packages/envoy" build -ldflags "-X main.buildCommit=$branch_commit" \
  -o "$baseline_work/bin/dispatch-branch" ./cmd/dispatch ||
  fail upgrade "build this branch's ./cmd/dispatch" "go build exited non-zero"
baseline_boot branch-server "$baseline_work/bin/dispatch-branch" "$baseline_database_url"
branch_pid="$baseline_boot_pid"
pass upgrade "this branch's server booted over the baseline's database at $baseline_boot_url"

branch_versions="$(baseline_applied_versions "$baseline_database_url")" ||
  fail upgrade "read the versions in the branch's schema_migrations" "psql refused it"
mapfile -t branch_version_list < <(printf '%s' "$branch_versions")
printf 'branch schema_migrations holds %d versions\n' "${#branch_version_list[@]}"

# A baseline version the branch's set lacks fails, named. Otherwise the lines name each version
# this branch added, so that the upgrade shows the moment a child's migration lands, or say that it
# added none, which is where a branch no migration has landed on stands, and the run passes.
comparison="$(baseline_compare_versions "$baseline_versions" "$branch_versions")" ||
  fail upgrade "this branch's database holds every migration the baseline applied" "$comparison"
printf '%s\n' "$comparison"
pass upgrade "this branch's database holds every migration the baseline applied"
