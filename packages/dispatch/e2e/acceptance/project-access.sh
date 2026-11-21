#!/usr/bin/env bash
# c-private-unseen (private projects, verification v17): a private project is unseen by every
# caller its access list does not let in, on every read path, and seen by every caller it does.
#
# It starts two servers of this checkout, `main` and `twin`, each on a database of its own, with
# a NATS of its own (the bus a private project's record is to be written to, by a writer this
# build does not have), a fake Envoy of its own and one fake GitHub, and seeds them through the
# routes (access-lib.sh):
#   main  verification v16's seed: PUB, public, and PRIV, made private through the visibility
#         route; alice, a full member no list names; bob, a restricted member PRIV's list names;
#         carol, a full member PRIV's list names, who owns both; carol's token that lists PRIV,
#         minted while PRIV is public, carol's token that lists PUB, carol's token that lists no
#         project, and the shared token. Each project holds an issue whose spec is a document with
#         blocks, a comment, an ask, a message, an issue document, a project document, an agent
#         session of its own that the fake Envoy lists, and a repository its issue names as its own
#         `repo`. PUB's issue holds a comment of carol's that names PRIV's issue and its spec, the
#         seed's reference from PUB to PRIV. PRIV also holds ses-priv-worker, a session main's fake
#         Envoy lists, recorded as having acted in PRIV; a broadcast to PRIV's session and a file of
#         its conversation; and a pull request of the deploy repository naming PRIV's issue, with a
#         PR-checks run of it.
#   twin  the same seed with PRIV never written: the same PUB, its comment naming a key the twin
#         never holds, the same people, and carol's three tokens, of which the one that lists PRIV
#         on main lists nothing here. Its fake Envoy lists PUB's session alone: a session whose
#         credential lists PRIV is PRIV's, as hidden as its objects.
# No personal token lists a private project whose key DISPATCH_PRIVATE_PROJECTS names (R18): main's
# switch of PRIV to private revokes carol's PRIV token, so that token is a listed caller only while
# PRIV is public (--all-public), and once PRIV is private the private section holds it revoked and a
# new token naming PRIV refused. No token of alice's lists PRIV: once PRIV is private no credential
# of a full member who cannot see it lists it.
# Then, in sections, each of which runs whatever the ones before found:
#   private     the two commands an operator runs on the deploy (envoy-dispatch
#               backfill-session-projects and scrub-access-events) run against main's database,
#               and PRIV is made private through PATCH /api/v1/projects/PRIV/access. walk, search,
#               sockets, proxy and writes need it, and are reported as not run when the route
#               refuses, as it always does here: this Dispatch names no secrets broker, so the
#               route answers 409 PRIVATE_PROJECTS_NEED_BROKER, and a build that runs no
#               private-hub admission proof answers 409 PRIVATE_PROJECTS_UNAVAILABLE; listed and
#               confined run either way. With --private-by-sql, that refusal is stood in for by the
#               store writes the route makes (PRIV private with its full switch recorded, and every
#               personal token that lists it revoked, its closing state's revocation), so every
#               section below runs, and the run passes when they do. The route checks, in the
#               transaction it refuses, that PRIV is provisioned and that the store is complete
#               (PRIV's census and the markers of the two commands) before it reaches either, so
#               that refusal says the switch's preconditions hold; any other answer of the route
#               fails the run. The stand-in does
#               not append the route's project.access_changed events, events of PRIV, which no
#               unseeing caller reads. Either way the twin's carol PRIV token, listing nothing, is
#               revoked beside main's, so carol's list of her tokens compares alike. Then PRIV's
#               tokens are held to R18: carol's PRIV token answers 401 TOKEN_REVOKED, and a new
#               token naming PRIV is refused 403 PRIVATE_PROJECT_TOKEN.
#               With --all-public, PRIV is left public and listed and confined alone run: every
#               other section prints `not run: <section> (slice A3b, #t-a-36)`, the slice that
#               makes a project private, and the run passes when those two do.
#   listed      bob and carol find each object of PRIV, and carol's PRIV token too while PRIV is
#               public.
#   confined    bob finds nothing of PUB, no person (once PRIV is private) and no agent outside
#               PRIV, and no direct message that is not his own. carol's PRIV token, a full
#               member's, lists PUB beside PRIV while PRIV is public: a full member's list decides
#               only which private projects it sees (R3).
#   walk        for alice, carol's token that lists no project and the shared token ("the unseeing
#               callers"): every GET row of GET /api/v1 whose path names an object, filled once with
#               PRIV's objects and once with ids that do not exist, each kind of object as the row's
#               Probe names it, answers the same status, headers and bytes; a row that names an
#               object and has no Probe fails the run, named. A GET row that names no object is a
#               list, and one whose every wildcard names a node of no project is filled alike on
#               both servers: either answers as the twin answers it.
#               Every GET row filled with PUB's objects answers as the twin answers it, so no read
#               of PUB names PRIV's objects through the reference from PUB to PRIV.
#   search      both legs of search, the keyword leg alone and the two together, answer each
#               unseeing caller what the twin answers.
#   sockets     the document socket answers an unseeing caller for PRIV's spec what it answers
#               for a document that does not exist, lets bob and carol in, and refuses carol's
#               PRIV token alike for PUB's spec and PRIV's, since it admits a person's cookie
#               alone; the live socket sends each unseeing caller, while carol comments on PRIV
#               and then on PUB, the frames the twin sends it.
#   proxy       the dashboard's GitHub proxy answers an unseeing person for PRIV's repository
#               what it answers for a repository the App is not installed on.
#   writes      last, since a write that is not hidden changes what every section above reads:
#               every write row whose path names an object, walked as walk walks the GET rows.
# The Probes are read from the server's own table (`go run ./internal/dispatch/api/cmd/routeprobes`),
# never from a list here. Answers are compared after access-lib.sh's mask, headers and body, and an
# answer to a filled path also with the ids it was filled with masked as the wildcard they fill,
# which is the only text in which a hidden object's answer and a missing one's may differ. Beside
# the twin, each server's `Dispatch-Private-Projects` line is its own (main_header, twin_header).
# This file runs the sections in order; the seed (seed_scenario) and the walk's helpers and tables
# are in project-access-walk.sh beside it, which it sources.
#
# Usage, from packages/dispatch:
#
#   DATABASE_URL=<a loopback PostgreSQL this run may create databases on> \
#     bash e2e/acceptance/project-access.sh [--private-by-sql | --all-public]
#
# It needs no harness: the run starts every process it reads, and reads DATABASE_URL only for the
# server it names, on which it makes a database for each of its servers and drops it at the end.
# It prints `ok <section>` once a section has run to its end, whatever it found, and fails at the
# end (verdict), naming every answer that failed its judgement, a pair that differed or a read that
# broke its own rule, and everything it could not run, with why.
set -euo pipefail

root=$(cd "$(dirname "$0")/../../../.." && pwd)
started=$SECONDS
# The run's start, the earliest instant an answer can hold that the run wrote.
run_epoch=$(date +%s)
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh, which lib/two-builds.sh sources, reads of its caller: start_process and
# await_start log under $evidence/logs, and a timed-out until_true runs timeout_hook, which this
# driver leaves empty.
evidence=''
# shellcheck disable=SC2034 # read by lib/rig.sh's until_true
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=../../../../scripts/e2e/lib/two-builds.sh
. "$root/scripts/e2e/lib/two-builds.sh"
# shellcheck source-path=SCRIPTDIR source=access-lib.sh
. "$(dirname "$0")/access-lib.sh"
# shellcheck source-path=SCRIPTDIR source=project-access-walk.sh
. "$(dirname "$0")/project-access-walk.sh"

private_by_sql=0
all_public=0
case "${1:-}" in
  '') ;;
  --private-by-sql) private_by_sql=1 ;;
  --all-public) all_public=1 ;;
  *) fail "unknown argument '$1': this driver takes --private-by-sql or --all-public, alone" ;;
esac
[ $# -le 1 ] || fail "this driver takes --private-by-sql or --all-public, alone"
# A section a run with every project public leaves out, named as --all-public prints it.
not_run() { echo "not run: $1 (slice A3b, #t-a-36)"; }
# two_builds_setup reads DATABASE_URL, drops every DISPATCH_*, ENVOY_*, NATS_* and PG* setting the
# shell inherited, and arms two-builds' teardown, which access_cleanup below runs last.
two_builds_setup 'project access e2e'
main_pid='' twin_pid='' nats_main_pid='' nats_twin_pid='' envoy_main_pid='' envoy_twin_pid='' fake_github_pid=''
access_cleanup() {
  stop_all "$main_pid" "$twin_pid" "$nats_main_pid" "$nats_twin_pid" "$envoy_main_pid" "$envoy_twin_pid" "$fake_github_pid"
  drop_own_databases
  cleanup "$1"
}
trap 'access_cleanup "$?"' EXIT
trap 'access_cleanup 130' INT
trap 'access_cleanup 143' TERM

# What the run could not do, each with why: every line fails the run at its end.
unrun() { printf '%s\n' "$*" >>"$work/unrun"; }

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL server this run makes its databases on"
for tool in go bun curl psql jq python3 openssl; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
require_loopback_dsn db_host DATABASE_URL "$database_url" "this run creates and drops databases there" || fail "$db_host"
note "database host $db_host (URL not printed)"
pass

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/project-access.XXXXXXXX")
# shellcheck disable=SC2034 # read by lib/rig.sh's start_process and await_start
evidence=$work
mkdir -p "$work/logs" "$work/bin"
: >"$work/unrun"
: >"$work/statuses"
: >"$work/differences"
note "work directory $work"
go -C "$root/packages/envoy" build -o "$work/bin/" ./cmd/dispatch ./internal/testnats/cmd/natsd ||
  fail "this checkout's ./cmd/dispatch or its NATS did not build"
pass

# await_reported NAME VAR PATTERN WHAT waits, bounded, for the service NAME, which bound port 0
# itself, to report the port it got in its log (lib/rig.sh's report_port, PATTERN), and assigns VAR
# that port.
await_reported() {
  local pid_var=${1}_pid
  await_start "$1" "${!pid_var}" 60 "$4 to report its port" report_port "$evidence/logs/$1.log" "$3"
  printf -v "$2" '%s' "$(report_port "$evidence/logs/$1.log" "$3")"
}

begin services
# Every service serves on a port the kernel chose, so no other process can serve on it in its
# place: each fake and each NATS binds port 0 itself and reports what it got (report_port), and each
# Dispatch serves on a socket held from that choice until it serves on it (start_server, given no
# port, through lib/rig.sh's start_held). The fake GitHub, both NATS servers and both fake Envoys
# start at once, and each is awaited before anything is sent to it. FAKE_*_PORT is set empty, since
# a caller under with-harness.sh exports the harness's own.
# shellcheck disable=SC2034 # read through ${!database_var} below
main_database_url='' twin_database_url=''
# Assigned by await_reported below.
github_port='' envoy_port=''
FAKE_GITHUB_PORT='' start_process fake_github bun "$root/packages/dispatch/e2e/fake-github.ts"
for server in main twin; do
  mkdir -p "$work/nats-$server"
  start_process "nats_$server" "$work/bin/natsd" -p -1 -store_dir "$work/nats-$server"
  FAKE_ENVOY_PORT='' start_process "envoy_$server" bun "$root/packages/dispatch/e2e/fake-envoy.ts"
done
await_reported fake_github github_port 'fake github listener on 127\.0\.0\.1:' "the fake GitHub"
# The App is installed on both projects' repositories and on no other. PRIV's holds the commit the
# seed records PRIV-1's check result at: an issue whose own repository is known has its result's
# commit read at GitHub (RESULT_COMMIT_UNKNOWN otherwise).
curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$github_port/__fixture/repos" \
  -d '{"acme/pub-repo":{"contents":"read","installation_id":71},"acme/priv-repo":{"contents":"read","installation_id":72,"commits":["1111111111111111111111111111111111111111"]}}' \
  >/dev/null || fail "the fake GitHub refused its repositories"
agent_token=$(openssl rand -hex 24)
signin_key=$(openssl rand -hex 32)
app_pem=$(openssl genrsa 2048 2>/dev/null)
for server in main twin; do
  nats_pid_var=nats_${server}_pid
  await_start "nats_$server" "${!nats_pid_var}" 30 "the $server NATS to be ready" \
    grep -q 'Server is ready' "$evidence/logs/nats_$server.log"
  nats_port=$(report_port "$evidence/logs/nats_$server.log" 'Listening for client connections on [^ ]*:') ||
    fail "the $server NATS logged no client port; see $evidence/logs/nats_$server.log"
  await_reported "envoy_$server" envoy_port 'fake envoy listener on 127\.0\.0\.1:' "the $server fake Envoy"
  # A session that has acted in no project is judged by the projects its listener record names,
  # its credential's list (access.go's SessionClause): ses-priv-session's names PRIV, so it is
  # PRIV's agent, as hidden as PRIV's objects, and the twin, which never holds PRIV, lists no such
  # session. Main's also holds ses-priv-worker, which the seed records as having acted in PRIV.
  # PUB's session, ses-pub-session, is on both.
  sessions='[{"session_id":"ses-pub-session","title":"Works on PUB","projects":["PUB"],"capabilities":["aside","btw","steer"]}'
  [ "$server" = twin ] ||
    sessions+=',{"session_id":"ses-priv-session","title":"Works on PRIV","projects":["PRIV"],"capabilities":["aside","btw","steer"]}'
  [ "$server" = twin ] ||
    sessions+=',{"session_id":"ses-priv-worker","title":"Worked in PRIV","projects":["PRIV"],"capabilities":["aside","btw","steer"]}'
  curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$envoy_port/__fixture/sessions" \
    -d "$sessions]" >/dev/null || fail "the $server fake Envoy refused its sessions"
  database_var=${server}_database_url
  printf -v "$database_var" '%s' ''
  own_database "$database_var" "$server"
  # The server as start_server starts it, with the bus on and the deployment's own settings for
  # what this check reads: the projects it has provisioned as private, the dev embedder for the
  # meaning leg, the test hooks the route index lists, and the GitHub App the proxy mints its
  # tokens as. DISPATCH_PRIVATE_PROJECTS is the comma-separated keys of the projects a deployment
  # has provisioned as private (cmd/dispatch privateProjectsSetting, which refuses the boot for
  # any entry that is no project key): main provisions PRIV, and the twin, which never makes a
  # project private, sets none. Given no port, start_server sets <server>_port.
  private_projects=()
  [ "$server" = twin ] || private_projects=(DISPATCH_PRIVATE_PROJECTS=PRIV)
  envoy_port=$envoy_port database_url=${!database_var} start_server "$server" "$work/bin/dispatch" '' \
    DISPATCH_NATS_DISABLED= \
    NATS_URLS="nats://127.0.0.1:$nats_port" \
    "${private_projects[@]}" \
    DISPATCH_DEV_EMBEDDER=1 \
    DISPATCH_TEST_HOOKS=1 \
    DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=1s \
    DISPATCH_APP_CLIENT_ID=Iv1.e2efake \
    DISPATCH_APP_PEM_B64="$app_pem" \
    DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$github_port"
  port_var=${server}_port
  build_url[$server]=http://127.0.0.1:${!port_var}
done
await_start main "$main_pid" 120 "main to answer /healthz" curl -fsS --max-time 2 "${build_url[main]}/healthz"
await_start twin "$twin_pid" 120 "the twin to answer /healthz" curl -fsS --max-time 2 "${build_url[twin]}/healthz"
note "main serves ${build_url[main]}, the twin ${build_url[twin]}"
pass

begin seed
seed_scenario
note "main holds PUB and PRIV, the twin PUB alone"
pass

priv_private=0
if [ "$all_public" = 1 ]; then
  not_run private
else
  begin private
  # What an operator runs against the database on a deploy that carries private projects, before a
  # project can be made private: the fill of every event's session project, and the scrub of the
  # stored access events. Until each has written its marker row the switch answers 409
  # PRIVATE_PROJECTS_INCOMPLETE (cmd/dispatch README, "Filling the sessions' projects" and
  # "Scrubbing the access events"). Each reads DATABASE_URL alone.
  for command in backfill-session-projects scrub-access-events; do
    env -i PATH="$PATH" HOME=/nonexistent TMPDIR="$work" DATABASE_URL="$main_database_url" \
      "$work/bin/dispatch" "$command" >"$work/$command.out" 2>&1 ||
      fail "main: envoy-dispatch $command failed: $(tail -n 5 "$work/$command.out")"
    note "main: $(tail -n 1 "$work/$command.out")"
  done
  send main carol PATCH /api/v1/projects/PRIV/access '{"visibility":"private"}'
  if [ "$status" = 200 ]; then
    priv_private=1
    note "PRIV is private"
  else
    refusal="PATCH /api/v1/projects/PRIV/access {visibility: private} answered HTTP $status $(head -c 300 "$work/body")"
    if [ "$private_by_sql" = 0 ]; then
      unrun "PRIV is not private: $refusal; the walk, search, sockets, proxy and writes sections need it and did not run"
      note "PRIV is not private: HTTP $status"
    elif [ "$status" != 409 ] || ! jq -e '.code == "PRIVATE_PROJECTS_NEED_BROKER" or .code == "PRIVATE_PROJECTS_UNAVAILABLE"' "$work/body" >/dev/null 2>&1; then
      # The route reaches either refusal only once PRIV is provisioned and the store complete: any
      # other refusal is a precondition the stand-in would skip.
      fail "--private-by-sql stands in only for 409 PRIVATE_PROJECTS_NEED_BROKER or PRIVATE_PROJECTS_UNAVAILABLE, the refusals of a switch whose preconditions hold: $refusal"
    else
      # The store writes the route makes: its closing state revokes every personal token that
      # lists PRIV, and its commit makes PRIV private and records its full switch.
      psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 \
        -c "update agent_tokens set revoked_at = now() where revoked_at is null and 'PRIV' = any(projects)" \
        -c "update projects set visibility = 'private', full_switch_at = now() where key = 'PRIV'" >/dev/null ||
        fail "main: the stand-in could not make PRIV private"
      priv_private=1
      note "PRIV is private by the store writes the route makes (--private-by-sql): the route answered 409 $(jq -r '.code' "$work/body"), its preconditions held"
    fi
  fi
  if [ "$priv_private" = 1 ]; then
    # The twin's carol PRIV token, which lists nothing there, is revoked as main's is, so carol's
    # list of her tokens compares alike: a revocation names no project.
    send twin carol DELETE "/api/v1/me/agent-tokens/$(recalled twin "carol-priv-token id")"
    expect_status 204 "twin: carol revokes her second token as main's switch revoked hers"
    # R18: no personal token lists PRIV once it is private and DISPATCH_PRIVATE_PROJECTS names it.
    # The token of carol's that listed it answers as a revoked token, and a new one is refused,
    # naming PRIV, which carol sees.
    send main carol-priv-token GET /api/v1/whoami
    problem=''
    [ "$status" = 401 ] && [ "$(jq -r '.code // empty' "$work/body" 2>/dev/null)" = TOKEN_REVOKED ] ||
      problem="main: carol's PRIV token once PRIV is private: HTTP $status, want 401 TOKEN_REVOKED: $(head -c 300 "$work/body")"
    judge "$status" "$problem"
    send main carol POST /api/v1/me/agent-tokens '{"name":"after the switch","projects":["PRIV"]}'
    problem=''
    if [ "$status" != 403 ] || [ "$(jq -r '.code // empty' "$work/body" 2>/dev/null)" != PRIVATE_PROJECT_TOKEN ] || ! grep -q PRIV "$work/body"; then
      problem="main: carol's new token naming PRIV once PRIV is private: HTTP $status, want 403 PRIVATE_PROJECT_TOKEN naming PRIV: $(head -c 300 "$work/body")"
    fi
    judge "$status" "$problem"
    note "carol's PRIV token answers as revoked, and a new token naming PRIV is refused"
  fi
  pass
fi

# The callers PRIV's list lets in, as they stand now: carol's PRIV token only while PRIV is public,
# since the switch revoked it.
listed_now=("${listed_callers[@]}")
if [ "$priv_private" = 1 ]; then
  listed_now=(bob carol)
fi

begin listed
# Each listed caller reads each object of PRIV: 200, and the answer names the object.
found=0
for caller in "${listed_now[@]}"; do
  for path in "${object_reads[@]}"; do
    path=${path//\{P\}/PRIV}
    send main "$caller" GET "$(resolve main "$path")"
    problem=''
    if [ "$status" = 200 ] && grep -q "PRIV" "$work/body"; then
      found=$((found + 1))
    else
      problem="$caller GET $path: HTTP $status, want 200 naming PRIV: $(head -c 300 "$work/body")"
    fi
    judge "$status" "$problem"
  done
done
note "$found of $((${#listed_now[@]} * ${#object_reads[@]})) reads of PRIV's objects found them"
pass

begin confined
# bob: PUB's objects answer as missing, and the lists hold PRIV alone. A restricted member's list
# narrows what that person sees; a full member's list decides only which private projects it
# sees, so carol's PRIV token sees PUB too (below).
: >"$work/fill-empty"
confined_callers=(bob)
for caller in "${confined_callers[@]}"; do
  for path in "${object_reads[@]}"; do
    pub=$(resolve main "${path//\{P\}/PUB}")
    none=${path//\{P\}/ZZNONE}
    none=${none//\{ZZNONE comment\}/${missing[comment]}}
    none=${none//\{ZZNONE ask\}/${missing[ask]}}
    none=${none//\{ZZNONE message\}/${missing[message]}}
    none=${none//\{ZZNONE spec\}/${missing[document]}}
    none=${none//\{ZZNONE notes\}/${missing[document]}}
    none=${none//\{ZZNONE playbook\}/${missing[document]}}
    printf 'object\tPUB\n' >"$work/fill-hidden"
    printf 'object\tZZNONE\n' >"$work/fill-missing"
    compare_missing "$caller" GET "${path//\{P\}/<P>}" "$pub" "$none" "$work/fill-hidden" "$work/fill-missing"
  done
  send main "$caller" GET /api/v1/projects
  keys=$(jq -c '[.[].key]' "$work/body" 2>/dev/null || echo "not a list: $(head -c 200 "$work/body")")
  problem=''
  [ "$keys" = '["PRIV"]' ] || problem="$caller GET /api/v1/projects: lists $keys, want [\"PRIV\"]"
  judge "$status" "$problem"
  # A person of PUB is one of PRIV's people only while PRIV is public (R2: every full member reads a
  # public project, and so is one of its people), so the people a restricted member and a token that
  # lists PRIV read is held to PRIV's own only once PRIV is private. Each list must be answered 200
  # and name what the caller sees, carol and the session with history in PRIV (ses-priv-worker), or
  # the name it must not hold could be missing from an error or an empty list.
  if [ "$priv_private" = 1 ]; then
    send main "$caller" GET /api/v1/users
    problem=''
    if [ "$status" != 200 ]; then
      problem="$caller GET /api/v1/users: HTTP $status, want 200: $(head -c 300 "$work/body")"
    elif ! grep -q "$carol" "$work/body"; then
      problem="$caller GET /api/v1/users: names no $carol, one of PRIV's people, so it shows nothing: $(head -c 300 "$work/body")"
    elif grep -q "$alice" "$work/body"; then
      problem="$caller GET /api/v1/users: names $alice, who is outside PRIV"
    fi
    judge "$status" "$problem"
  else
    note "$caller GET /api/v1/users not compared: PRIV is public, so every full member is one of its people (R2)"
  fi
  send main "$caller" GET /api/v1/agents
  problem=''
  if [ "$status" != 200 ]; then
    problem="$caller GET /api/v1/agents: HTTP $status, want 200: $(head -c 300 "$work/body")"
  elif ! grep -q ses-priv-worker "$work/body"; then
    problem="$caller GET /api/v1/agents: lists no ses-priv-worker, the session with history in PRIV, which $caller sees, so it shows nothing: $(head -c 300 "$work/body")"
  elif grep -q ses-pub-session "$work/body"; then
    problem="$caller GET /api/v1/agents: lists ses-pub-session, an agent of PUB"
  fi
  judge "$status" "$problem"
  # PUB's session is answered as one nobody holds (404 SESSION_NOT_FOUND), or its messages are
  # read without carol's direct message to it.
  send main "$caller" GET /api/v1/agents/ses-pub-session/messages
  problem=''
  case "$status" in
    200)
      if grep -q "$(recalled main "PUB direct message")" "$work/body"; then
        problem="$caller GET /api/v1/agents/ses-pub-session/messages: holds carol's direct message to it"
      fi
      ;;
    404)
      if [ "$(jq -r '.code // empty' "$work/body" 2>/dev/null)" != SESSION_NOT_FOUND ]; then
        problem="$caller GET /api/v1/agents/ses-pub-session/messages: HTTP 404, want its code SESSION_NOT_FOUND: $(head -c 300 "$work/body")"
      fi
      ;;
    *) problem="$caller GET /api/v1/agents/ses-pub-session/messages: HTTP $status, want 200 or 404 SESSION_NOT_FOUND: $(head -c 300 "$work/body")" ;;
  esac
  judge "$status" "$problem"
done
note "bob read PUB's objects, the projects, the people, the agents and a direct message"
# carol's PRIV token, a full member's, sees PUB, which it does not list, beside PRIV. Once PRIV is
# private the switch has revoked it (the private section holds it to 401).
if [ "$priv_private" = 0 ]; then
  send main carol-priv-token GET /api/v1/projects
  keys=$(jq -c '[.[].key]' "$work/body" 2>/dev/null || echo "not a list: $(head -c 200 "$work/body")")
  problem=''
  [ "$keys" = '["PRIV","PUB"]' ] || problem="carol-priv-token GET /api/v1/projects: lists $keys, want [\"PRIV\",\"PUB\"]"
  judge "$status" "$problem"
fi
pass

if [ "$priv_private" = 1 ]; then
  begin walk
  # Each server writes the seed's windows and their vectors behind the seed's writes, in its own
  # time, and the walk reads them beside the twin (the coverage route, each block's embedding,
  # search's meaning leg): so both servers are read only once both have settled their meaning
  # index (access-lib.sh's await_index_settled). No section since the seed wrote anything either
  # server indexes.
  await_index_settled "$index_settle_seconds" main twin
  until_true 120 "both servers to reconcile delivery once" reconciled
  logs_hold_own_writes
  # The route index this server serves, and the Probes of its own tables.
  send main carol GET /api/v1
  expect_status 200 "main: carol reads the route index"
  jq -c '.routes[]' "$work/body" >"$work/routes"
  probes_read=0 probes_status=0
  # On a line of its own, so .github/scripts/ci_job_reads.py pins the go command to the whole line.
  go -C "$root/packages/envoy" run ./internal/dispatch/api/cmd/routeprobes >"$work/probes.json" 2>"$work/probes.err" ||
    probes_status=$?
  if [ "$probes_status" = 0 ]; then
    probes_read=1
    walk_rows 'select(.method == "GET")'
    note "walked $walked GET rows that name an object and $lists lists, as each unseeing caller"
    # A walk that compared nothing has not passed: the route index or the Probes held no row.
    [ "$walked" -gt 0 ] || unrun "the walk compared no GET row that names an object: the route index carol read, or the Probes, held none"
    [ "$lists" -gt 0 ] || unrun "the walk compared no list: the route index carol read held no GET row that names no object"
    walk_pub_rows
    note "walked $pub_walked GET rows filled with PUB's objects, beside the twin, as each unseeing caller"
    [ "$pub_walked" -gt 0 ] || unrun "the walk compared no GET row filled with PUB's objects: the route index carol read, or the Probes, held none"
    walk_queries
    note "walked $queried query parameters of GET rows that name an object, a person or a session, as each unseeing caller"
    [ "$queried" -gt 0 ] || unrun "the query walk compared no query parameter: the route index carol read, or the OpenAPI, held none that names an object"
  else
    unrun "the Probes could not be read, so no row was walked: go run ./internal/dispatch/api/cmd/routeprobes: $(head -c 300 "$work/probes.err")"
  fi
  pass

  begin search
  # Meaning search answers from the windows and vectors each server writes behind each write. The
  # walk waited for both servers to settle them and wrote nothing they index, so this wait reads
  # each server once, and a read of the walk's that wrote after all is waited for here.
  await_index_settled "$index_settle_seconds" main twin
  for caller in "${unseeing_callers[@]}"; do
    for query in "q=paragraph&keywords_only=true" "q=paragraph" "q=remark&keywords_only=true" "q=remark"; do
      compare_twin "$caller" "/api/v1/search/results?$query"
    done
    compare_twin "$caller" "/api/v1/search?q=paragraph"
  done
  pass

  begin sockets
  schema_version=$(curl -fsS --max-time 5 "${build_url[main]}/api/v1/schema/blocks" | jq -r '.version // .schema_version // empty')
  [ -n "$schema_version" ] || fail "the block schema names no version"
  printf 'document\t%s\n' "$(recalled main "PRIV spec")" >"$work/fill-hidden"
  printf 'document\t%s\n' "${missing[document]}" >"$work/fill-missing"
  declare -A live_pid=()
  # live_settled READY PID: the live probe PID has written its ready file READY, or has exited.
  live_settled() { [ -e "$1" ] || ! kill -0 "$2" 2>/dev/null; }
  for caller in "${unseeing_callers[@]}"; do
    for side in hidden missing; do
      id=${missing[document]}
      [ "$side" = missing ] || id=$(recalled main "PRIV spec")
      status=socket
      bun "$(dirname "$0")/socket-access.ts" doc "${build_url[main]}" "$work/auth/main-$caller" "$id" "$schema_version" >"$work/body" 2>&1 || status=error
      keep_answer "$side" main "$work/fill-$side"
    done
    same_answers "$caller /ws/doc/{room}" hidden missing
    # The live socket, live: the caller subscribes with no resume on each server, both at once, and
    # while both sockets are open carol comments on PRIV's issue (main only: the twin holds no
    # PRIV) and then on PUB's, on each server. Each server's answer is the frames the caller was
    # sent by that server; each comment's id is remembered under one label on both servers, so the
    # mask names it alike. A PRIV event on main differs from the twin's answer. Every unseeing
    # caller sees PUB, so the twin sends it the PUB comment, and an answer holding no event fails
    # the pair: the compare must see the socket send something.
    for server in main twin; do
      rm -f "$work/live-ready-$server" "$work/live-written-$server"
      bun "$(dirname "$0")/socket-access.ts" live "${build_url[$server]}" "$work/auth/$server-$caller" \
        "$work/live-ready-$server" "$work/live-written-$server" >"$work/live-body-$server" 2>&1 &
      live_pid[$server]=$!
    done
    # A probe that ends before it is ready, as one whose upgrade is refused does (it prints
    # `http <status> <body>` and writes no ready file), ends the wait at once: its output is its
    # answer, kept and compared as any other.
    for server in main twin; do
      until_true 10 "$caller's live socket on $server to start, or its probe to end" \
        live_settled "$work/live-ready-$server" "${live_pid[$server]}"
    done
    send main carol POST /api/v1/issues/PRIV-1/comments '{"body":"A live remark on PRIV."}'
    expect_status 201 "main: carol comments on PRIV-1 while $caller's live socket is open"
    for server in main twin; do
      send "$server" carol POST /api/v1/issues/PUB-1/comments '{"body":"A live remark on PUB."}'
      expect_status 201 "$server: carol comments on PUB-1 while $caller's live socket is open"
      remember "$server" "live comment for $caller" "$(jq -r .id "$work/body")"
    done
    for server in main twin; do
      : >"$work/live-written-$server"
    done
    for server in main twin; do
      status=socket
      wait "${live_pid[$server]}" || status=error
      cp "$work/live-body-$server" "$work/body"
      keep_answer "$server" "$server"
    done
    problem=''
    grep -q '"type":"event"' "$work/answers/twin.body" ||
      problem="$caller /ws/live: the twin sent no event of carol's PUB comment, so the compare saw nothing: $(head -c 300 "$work/answers/twin.body")"
    judge "$(<"$work/answers/twin.status")" "$problem"
    same_answers "$caller /ws/live (beside the twin)" main twin
  done
  # The document socket admits a person by their cookie alone, at the merge base as now, so a
  # token has no document-socket path to any project: bob and carol find PRIV's spec there, and a
  # token that lists PRIV is refused alike for PUB's spec and PRIV's. This is the one path on which
  # "find each object of PRIV" is read as "refused alike".
  for caller in bob carol; do
    answer=$(bun "$(dirname "$0")/socket-access.ts" doc "${build_url[main]}" "$work/auth/main-$caller" "$(recalled main "PRIV spec")" "$schema_version" 2>&1) || true
    problem=''
    [[ "$answer" == authenticated* ]] || problem="$caller /ws/doc/{PRIV spec}: $answer, want authenticated"
    judge socket "$problem"
  done
  printf 'document\t%s\n' "$(recalled main "PUB spec")" >"$work/fill-pub"
  for side in pub hidden; do
    project=PRIV
    [ "$side" = hidden ] || project=PUB
    status=socket
    bun "$(dirname "$0")/socket-access.ts" doc "${build_url[main]}" "$work/auth/main-carol-priv-token" \
      "$(recalled main "$project spec")" "$schema_version" >"$work/body" 2>&1 || status=error
    keep_answer "$side" main "$work/fill-$side"
  done
  same_answers "carol-priv-token /ws/doc/{room}: PUB's spec and PRIV's (a token is refused alike)" pub hidden
  pass

  begin proxy
  printf 'repository\tacme/priv-repo\n' >"$work/fill-hidden"
  printf 'repository\tacme/no-such-repo\n' >"$work/fill-missing"
  # The proxy is the dashboard's: a person's cookie reaches it.
  compare_missing alice GET "/api/github/rest/repos/{repository}" \
    /api/github/rest/repos/acme/priv-repo /api/github/rest/repos/acme/no-such-repo "$work/fill-hidden" "$work/fill-missing"
  pass

  # Last, since a write that is not hidden changes what every section above reads.
  begin writes
  if [ "$probes_read" = 1 ]; then
    walk_rows 'select(.method != "GET")'
    note "walked $walked write rows that name an object, as each unseeing caller; $refused calls of rows whose object no project owns, each refused to a caller that is not open"
    [ "$walked" -gt 0 ] || unrun "the write walk compared no write row that names an object: the route index carol read, or the Probes, held none"
  fi
  pass
elif [ "$all_public" = 1 ]; then
  for section in walk search sockets proxy writes; do
    not_run "$section"
  done
fi

begin verdict
note "$compared answers judged, each pair compared and each read held to a rule of its own; by status: $(statuses)"
[ "$compared" -gt 0 ] || unrun "no answer was judged"
if [ -s "$work/unrun" ]; then
  fail "$(wc -l <"$work/unrun") things did not run:
$(<"$work/unrun")
$([ "$differed" -eq 0 ] || printf '%s answers differ:\n%s' "$differed" "$(<"$work/differences")")"
fi
no_difference
pass
