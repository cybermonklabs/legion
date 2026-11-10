# shellcheck shell=bash
# The two helpers a proof that writes a PostgreSQL server reads its caller's DSN with: which host
# libpq reads from that DSN, and the connection string that gives the run its own database on
# that host. lib/loopback-dsn.py answers both, and this file addresses it beside itself, so it
# needs nothing of its caller's. lib/rig.sh sources it, and so does
# scripts/e2e/envoy-session-delivery.sh, which sources no rig: that proof keeps an await_port_file
# and a keep_port of its own, written for its own pid map.
# Sourced, never run.

# require_loopback_dsn VAR NAME DSN WHAT assigns VAR the host libpq reads from DSN, which is
# 127.0.0.1, localhost or ::1, and returns 0. For a DSN the guard refuses it assigns VAR the
# guard's own refusal line and returns 1, so each proof ends the run in its own words:
#
#   require_loopback_dsn db_host NAME "$dsn" "this run creates and drops a database there" ||
#     fail "$db_host"
#
# NAME is the environment variable the caller read DSN from, and it starts that line. WHAT is
# what the run does on that server, in a few words.
#
# Nine call sites read a DSN with this guard before they start anything, because the server must
# be this machine's. Seven are proofs of their own: plan-pointer-drive.sh,
# verification-results.sh, dispatch-claimant-delivery.sh, clean-session-stop-tmux.sh,
# envoy-session-states.sh, envoy-session-delivery.sh and dispatch-session-settings.sh. The
# eighth is lib/two-builds.sh, the harness the two two-builds proofs share, which reads
# DATABASE_URL once for dispatch-two-builds.sh and dispatch-search-two-builds.sh. The ninth is
# lib/live-daemon-proof.sh, whose prerequisites check
# reads LEGION_TEST_PG_DSN for each of the seven proofs that make that run: child-adoption.sh,
# ci-red-no-takeover.sh, claim-compaction.sh, handoff-records.sh, merged-root-waits.sh,
# no-required-checks.sh and provider-error-retry.sh. Seven of the nine call this function.
# envoy-session-delivery.sh and dispatch-session-settings.sh read their DSN with
# run_database_url alone, which begins with this guard's own check. Every proof that writes a
# server its caller's DSN names reads that DSN with this guard before it starts anything.
#
# libpq's own parser answers which host the DSN names, because libpq and a URL parser read
# different text as the host. libpq ends the user name at the first `@` and starts no query
# inside it, so it reads the host of `postgres://u@localhost:5432@10.1.2.3/db` as localhost,
# with the port `5432@10.1.2.3`, and the host of `postgres://evil.example?@localhost/db` as
# localhost as well. A WHATWG URL ends the user name at the last `@` and starts the query at the
# first `?`, so it reads the hosts 10.1.2.3 and evil.example, and a run that connects with its
# reading writes another machine. lib/loopback-dsn.py holds the answer, and its own section of
# scripts/e2e/README.md says what it reads and what it refuses.
#
# So the client the run connects with must be this one. A proof that reads the DSN with a second
# parser decides the host twice, and the host it connects to is the second answer. Each of the
# nine proofs those eight call sites serve reaches that server with psql, which is libpq. Five
# of them give every server
# they start the connection string run_database_url below rebuilds, which libpq read back inside
# the helper.
#
# The other four read the DSN a second time, with pgx. lib/two-builds.sh:289, which starts both
# servers of dispatch-two-builds.sh and of dispatch-search-two-builds.sh,
# verification-results.sh:206 and dispatch-claimant-delivery.sh:169 give Dispatch the caller's
# raw DSN, and Dispatch opens its pool from that text with pgxpool.ParseConfig
# (packages/envoy/internal/dispatch/store/store.go:58). Two facts keep those four safe. The
# pinned pgx reads these shapes as libpq does, because pgx v5.11.0's own pgconn/parse_url.go is
# written to match libpq's URI parser and says so in its first comment. And all four run
# Dispatch with DISPATCH_DEV_SIGNIN=1, whose boot reads the host again with pgx and refuses one
# that is not loopback (loopbackDatabase, packages/envoy/cmd/dispatch/main.go:1270-1284).
#
# Nothing enforces that pin. The pin is `github.com/jackc/pgx/v5 v5.11.0` in
# packages/envoy/go.mod:31 and packages/daemon/go.mod:13, with its checksum in each go.sum.
# .github/dependabot.yml covers the github-actions ecosystem alone, so no automation raises pgx,
# and no CI job runs `go mod tidy`, `go mod verify` or any other check that reads which pgx
# version is in use. dispatch://INFRA-2338 tracks that gap. A raise is a deliberate edit of those
# files, and this comment is the one text that says what such a raise moves:
# scripts/e2e/README.md and packages/daemon/internal/testpg/testdb/testdb_test.go point here
# rather than write it again. Whoever raises pgx moves the safety of those four proofs, and of
# the rewrite check the next paragraph names, and must drive this guard's own cases
# (lib/loopback-dsn.test.ts) and that check's own cases
# (packages/daemon/internal/testpg/testdb/testdb_test.go) again.
#
# The ninth call site's binary rewrites that DSN itself, and this guard reaches no further than
# the shell. That rewrite, and the check that keeps it on the host this guard read, are written
# once, on DatabaseURL in packages/daemon/internal/testpg/testdb/testdb.go. The two frames divide
# this way: this guard reads the caller's DSN with libpq in the shell, before anything starts,
# and the binary reads every DSN it builds from that one back with pgx inside itself.
#
# The call runs after the caller's own tool check, because it is python3 that runs the guard.
# The caller clears PGHOSTADDR, PGSERVICE and PGSERVICEFILE before its first connection, in its
# sweep of the environment or in an unset of its own: this guard reads the DSN alone, psql reads
# PGHOSTADDR, and PGSERVICE with PGSERVICEFILE name an entry of ~/.pg_service.conf that can set
# hostaddr. Measured with an exported entry that sets hostaddr=127.0.0.2: psql connected to
# 127.0.0.2, and the guard printed 127.0.0.1 for the same DSN.
require_loopback_dsn() {
  _loopback_dsn "$1" "$2" "$3" "$4"
}

# run_database_url VAR NAME DSN DATABASE WHAT assigns VAR the connection string that names the
# run's own database DATABASE on the server DSN names, so nothing the run starts writes the
# database the caller's DSN itself names, and returns 0. For a DSN the helper refuses it assigns
# VAR the helper's own refusal line and returns 1, the contract require_loopback_dsn above has,
# so the caller chooses how the run ends:
#
#   run_database_url database_url NAME "$dsn" "$database" "this run creates and drops a database
#     there" || fail "$database_url"
#
# NAME is the environment variable the caller read DSN from, and it starts the refusal. WHAT is
# what the run does on that server, in a few words.
#
# This mode refuses every DSN the guard refuses, and a value the rebuilt string cannot carry as
# well. envoy-session-delivery.sh and dispatch-session-settings.sh therefore call it in their
# setup step as their one read of the DSN, before the builds, the services and the `create
# database`, so a refused input ends the run where the run's other refusals are. The database's
# name is only the shell's pid and random bytes, so each has it that early. The three other
# callers name a database per run or per attempt, and call this where that name is made.
#
# lib/loopback-dsn.py does the work: it rebuilds the connection string from the keywords libpq
# reads of DSN, rather than appending a `?dbname=` to the DSN's own text. Its docstring and
# scripts/e2e/README.md's lib/loopback-dsn.py section say what it refuses and why it rebuilds.
# A keyword the DSN sets in its query outranks the same keyword in its path for libpq and for
# pgx, so `?dbname=other` names the database a text join of server, database and parameters
# would leave in place. The rebuilt string carries one dbname, which is DATABASE.
#
# require_loopback_dsn above says why the caller clears PGHOSTADDR, PGSERVICE and PGSERVICEFILE
# first. The refusal line names the host, the hostaddr or the service libpq read, or the keyword
# whose value the string cannot carry, and no other part of the DSN.
run_database_url() {
  _loopback_dsn "$1" "$2" "$3" "$5" --dbname "$4"
}

# _loopback_dsn VAR NAME DSN WHAT [--dbname DATABASE] assigns VAR what lib/loopback-dsn.py
# printed for the DSN, which is the host, the rebuilt connection string or the one refusal line,
# and returns that helper's own status. The two helpers above share this one call to it. The
# locals' names are ones no caller passes: printf -v would otherwise assign the local.
#
# The DSN goes to the helper through the environment of this command substitution's own subshell.
# `env NAME=$DSN python3 …` would put the password in env(1)'s argument list, which
# /proc/<pid>/cmdline holds until it execs. The helper is addressed beside this file, so a proof
# that sources this file needs no variable of its own for the checkout.
_loopback_dsn() {
  local _answer_var=$1 _name=$2 _dsn=$3 _answer _status=0
  shift 3
  _answer=$(export "$_name=$_dsn"; python3 "${BASH_SOURCE[0]%/*}/loopback-dsn.py" \
    "$_name" "$@" 2>&1) || _status=1
  printf -v "$_answer_var" '%s' "$_answer"
  return "$_status"
}
