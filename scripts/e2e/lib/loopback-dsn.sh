# shellcheck shell=bash
# The helpers a proof that writes a PostgreSQL server reads its caller's DSN with: which host
# libpq reads from that DSN, whether the run can have a database of its own on that host, and
# the start of a command that is handed that database and nothing else of the DSN. None of them
# assigns a variable of the caller's the connection string, which carries the DSN's password.
# lib/loopback-dsn.py answers each, and this file addresses it beside itself, so it needs nothing
# of its caller's. A fourth helper, empty_public_schema, empties the database a run was given, so
# that a server migrates it from nothing. lib/rig.sh sources this file, and so does
# scripts/e2e/envoy-session-delivery.sh, which sources no rig: that proof keeps an
# await_port_file and a keep_port of its own, written for its own pid map.
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
# Eleven call sites read a DSN with this guard before they start anything, because the server must
# be this machine's. Eight are proofs of their own: plan-pointer-drive.sh,
# verification-results.sh, dispatch-claimant-delivery.sh, clean-session-stop-tmux.sh,
# envoy-session-states.sh, envoy-session-delivery.sh, dispatch-session-settings.sh and
# stage1-skeleton.sh. The ninth is lib/two-builds.sh, the harness the two two-builds proofs
# share, which reads DATABASE_URL once for dispatch-two-builds.sh and
# dispatch-search-two-builds.sh. The tenth is lib/live-daemon-proof.sh, whose prerequisites check
# reads LEGION_TEST_PG_DSN for each proof that makes that run, which is every entry script that
# sources it. The eleventh call site is busy-relaunch-delivery.sh's negative control, which reads
# LEGION_TEST_PG_DSN with this guard before it builds the base commit's tree, through the copy
# lib/live-daemon-proof.sh sources. Eight of the eleven call sites call this function.
# envoy-session-delivery.sh, dispatch-session-settings.sh and stage1-skeleton.sh read their DSN
# with require_run_database alone, which begins with this guard's own check. Every proof that
# writes a server its caller's DSN names reads that DSN with this guard before it starts anything.
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
# ten proofs those nine call sites serve reaches that server with psql, which is libpq. Eight of
# them give every server they start a database of the run's own through exec_run_database below,
# in the connection string the helper rebuilds and libpq reads back inside it.
#
# The other two read the DSN a second time, with pgx. lib/two-builds.sh:284, which starts both
# servers of dispatch-two-builds.sh and of dispatch-search-two-builds.sh, gives Dispatch the
# caller's raw DSN, and Dispatch opens its pool from that text with pgxpool.ParseConfig
# (packages/envoy/internal/dispatch/store/store.go:58). Two facts keep those two safe. The
# pinned pgx reads these shapes as libpq does, because pgx v5.11.0's own pgconn/parse_url.go is
# written to match libpq's URI parser and says so in its first comment. And both run Dispatch
# with DISPATCH_DEV_SIGNIN=1, whose boot reads the host again with pgx and refuses one that is
# not loopback (loopbackDatabase, packages/envoy/cmd/dispatch/main.go:1270-1284).
#
# Nothing enforces that pin. The pin is `github.com/jackc/pgx/v5 v5.11.0` in
# packages/envoy/go.mod:31 and packages/daemon/go.mod:13, with its checksum in each go.sum.
# .github/dependabot.yml covers the github-actions ecosystem alone, so no automation raises pgx,
# and no CI job runs `go mod tidy`, `go mod verify` or any other check that reads which pgx
# version is in use. dispatch://INFRA-2338 tracks that gap. A raise is a deliberate edit of those
# files, and this comment is the one text that says what such a raise moves:
# scripts/e2e/README.md and packages/daemon/internal/testpg/testdb/testdb_test.go point here
# rather than write it again. Whoever raises pgx moves the safety of those two proofs, and of
# the rewrite check the next paragraph names, and must drive this guard's own cases
# (lib/loopback-dsn.test.ts) and that check's own cases
# (packages/daemon/internal/testpg/testdb/testdb_test.go) again.
#
# The tenth call site's binary rewrites that DSN itself, and this guard reaches no further than
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

# require_run_database VAR NAME DSN DATABASE WHAT [--uri | --libpq-env] runs every check
# exec_run_database below runs before it hands a command the run's own database DATABASE on the
# server DSN names in that form, assigns VAR the empty string, and returns 0. For a DSN the helper
# refuses it assigns VAR the helper's own refusal line and returns 1, the contract
# require_loopback_dsn above has, so the caller chooses how the run ends:
#
#   require_run_database refusal NAME "$dsn" "$database" "this run creates and drops a database
#     there" || fail "$refusal"
#
# NAME is the environment variable the caller read DSN from, and it starts the refusal. WHAT is
# what the run does on that server, in a few words. The form is the one the caller's
# exec_run_database hands the database over in, and a caller that hands it over in two forms
# checks both.
#
# This check refuses every DSN the guard refuses, and a value the run's command cannot be handed
# as well. envoy-session-delivery.sh and dispatch-session-settings.sh therefore call it in their
# setup step as their one read of the DSN, before the builds, the services and the `create
# database`, so a refused input ends the run where the run's other refusals are. The database's
# name is only the shell's pid and random bytes, so each has it that early. The other callers
# name a database per run or per attempt, and call this where that name is made.
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
# whose value the command cannot be handed, and no other part of the DSN.
require_run_database() {
  _loopback_dsn "$1" "$2" "$3" "$5" --dbname "$4" "${@:6}" --check
}

# exec_run_database NAME DSN DATABASE WHAT FORM -- COMMAND... runs COMMAND in place of the shell
# that calls it, as `exec` does, handed the run's own database DATABASE on the server DSN names and
# nothing else of DSN. FORM is one of
#
#   [--uri] --env VAR       VAR holds the connection string, or with --uri the connection URI, in
#                           COMMAND's environment, which is otherwise this shell's without NAME
#   [--uri] --env-only VAR  VAR alone is COMMAND's environment, as under `env -i`, and a COMMAND
#                           of `env K=V… PROGRAM` names the rest
#   --libpq-env             each keyword is the variable libpq reads it from (PGHOST, PGPORT,
#                           PGUSER, PGPASSWORD, PGDATABASE and the rest), for psql, which then
#                           takes no connection string; PGSERVICE goes, as NAME does
#
# lib/loopback-dsn.py's docstring says what each form hands over and why NAME goes. It runs every
# check require_run_database runs first, and for a DSN it refuses COMMAND never runs: the helper
# prints the one refusal line on stderr and exits 1 in the shell's place. No variable of the
# caller's ever holds the connection string, so the caller's xtrace never prints it and no other
# command the caller runs is handed it.
#
# Since it replaces the shell, a caller runs it as the last command of a subshell of its own: in
# the background, where COMMAND keeps the subshell's pid, so `$!`, or the <NAME>_pid lib/rig.sh's
# start_process sets, names COMMAND itself and a signal to that pid reaches it,
#
#   exec_run_database NAME "$dsn" "$database" "$what" --env DATABASE_URL -- "$server" &
#   server_pid=$!
#
# or in `( … )` for a command the caller waits for:
#
#   (exec_run_database NAME "$dsn" "$database" "$what" --libpq-env -- psql -X -c 'select 1')
#
# Assignments before it reach COMMAND, as they reach any command (`K=V exec_run_database …`).
# Called in the script's own shell, it replaces nothing: it says so on stderr and returns 2.
#
# The DSN goes to the helper in that subshell's own environment, as NAME, and never in an argument
# list (_loopback_dsn below says why).
exec_run_database() {
  if [ "$BASHPID" = "$$" ]; then
    echo "exec_run_database runs its command in place of the shell that calls it, and this is the script's own shell: call it in ( … ) or with &" >&2
    return 2
  fi
  export "$1=$2"
  exec python3 "${BASH_SOURCE[0]%/*}/loopback-dsn.py" "$1" "$4" --dbname "$3" "${@:5}"
}

# empty_public_schema URL empties the public schema of the database URL names, so that a server
# migrates it from nothing, and returns psql's status, so the caller ends the run in its own words:
#
#   empty_public_schema "$database_url" || fail "could not empty the database's public schema"
#
# Every table, view, routine and type of the public schema goes, and the extensions it holds stay.
# `vector` is installed by the cluster's master role (0072_embeddings_core.up.sql says so, and
# takes it as given with `create extension if not exists`), which the role a run connects as need
# not be, so dropping the schema would leave a database no build can migrate. A Legion pod's test
# server is one such case: every database made from its template1 holds `vector`, and its login
# may not create it again. One caller runs it, lib/two-builds.sh's two_builds_reset_database:
# verification-results.sh and dispatch-claimant-delivery.sh make a database of their own instead
# (exec_run_database above), which `create database` copies from template1 with its extensions.
empty_public_schema() {
  local reset_sql="do \$\$
declare object record;
begin
  for object in select table_name, table_type from information_schema.tables where table_schema = 'public' loop
    if object.table_type = 'VIEW' then
      execute format('drop view if exists public.%I cascade', object.table_name);
    else
      execute format('drop table if exists public.%I cascade', object.table_name);
    end if;
  end loop;
  for object in
    select p.oid::regprocedure as signature from pg_proc p join pg_namespace n on n.oid = p.pronamespace
    where n.nspname = 'public' and not exists (select 1 from pg_depend d where d.objid = p.oid and d.deptype = 'e')
  loop
    execute format('drop routine if exists %s cascade', object.signature);
  end loop;
  for object in
    select t.oid::regtype as name from pg_type t join pg_namespace n on n.oid = t.typnamespace
    where n.nspname = 'public' and t.typtype in ('e', 'c', 'd')
      and not exists (select 1 from pg_depend d where d.objid = t.oid and d.deptype = 'e')
      and not exists (select 1 from pg_class c where c.oid = t.typrelid and c.relkind <> 'c')
  loop
    execute format('drop type if exists %s cascade', object.name);
  end loop;
end
\$\$;"
  psql "$1" -X -q -v ON_ERROR_STOP=1 -c 'set client_min_messages = warning' \
    -c "$reset_sql" >/dev/null
}

# _loopback_dsn VAR NAME DSN WHAT [--dbname DATABASE [--uri | --libpq-env] --check] assigns VAR
# what lib/loopback-dsn.py printed for the DSN, which is the host, nothing, or the one refusal
# line, and returns that helper's own status. require_loopback_dsn and require_run_database share
# this one call to it, and neither form it runs prints a connection string. The locals' names are
# ones no caller passes: printf -v would otherwise assign the local.
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
