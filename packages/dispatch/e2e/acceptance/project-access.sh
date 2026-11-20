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
#         alice's token that lists PRIV, carol's token that lists no project, and the shared
#         token. Each project holds an issue whose spec is a document with blocks, a comment, an
#         ask, a message, an issue document, a project document, an agent session of its own
#         that the fake Envoy lists, and a repository its issue names as its own `repo`. PUB's
#         issue holds a comment of carol's that names PRIV's issue and its spec, the seed's
#         reference from PUB to PRIV. PRIV also holds ses-priv-worker, a session main's fake Envoy
#         lists, recorded as having acted in PRIV; a broadcast to PRIV's session and a file of its
#         conversation; and a pull request of the deploy repository naming PRIV's issue, with a
#         PR-checks run of it.
#   twin  the same seed with PRIV never written: the same PUB, its comment naming a key the twin
#         never holds, the same people, carol's token that lists no project, and alice's token,
#         whose list names PRIV, a project the twin never holds. Its fake Envoy lists PUB's
#         session alone: a session whose credential lists PRIV is PRIV's, as hidden as its
#         objects.
# Then, in sections, each of which runs whatever the ones before found:
#   private     the two commands an operator runs on the deploy (envoy-dispatch
#               backfill-session-projects and scrub-access-events) run against main's database,
#               and PRIV is made private through PATCH /api/v1/projects/PRIV/access. walk, search,
#               sockets, proxy and writes need it, and are reported as not run when the route
#               refuses, as it always does in a build with no writer of the private-projects
#               record (409 PRIVATE_PROJECTS_UNAVAILABLE); listed and confined run either way. With
#               --private-by-sql, that refusal is stood in for by the store write the route makes
#               (`update projects set visibility = 'private'`), so every section below runs, and
#               the run passes when they do. The route checks, in the transaction it refuses, that
#               PRIV is provisioned and that the store is complete (PRIV's census and the markers
#               of the two commands) before it reaches the record, so that refusal says the
#               switch's preconditions hold; any other answer of the route fails the run. The
#               stand-in does not append the route's project.access_changed event, an event of
#               PRIV, which no unseeing caller reads.
#               With --all-public, PRIV is left public and listed and confined alone run: every
#               other section prints `not run: <section> (slice A3b, #t-a-36)`, the slice that
#               makes a project private, and the run passes when those two do.
#   listed      bob, carol and carol's PRIV token find each object of PRIV.
#   confined    bob and carol's PRIV token find nothing of PUB, no person (once PRIV is private)
#               and no agent outside PRIV, and no direct message that is not their own.
#   walk        for alice, alice's PRIV token, carol's token that lists no project and the shared
#               token ("the unseeing callers"): every GET row of GET /api/v1 whose path names an
#               object, filled once with PRIV's objects and once with ids that do not exist, each
#               kind of object as the row's Probe names it, answers the same status, headers and
#               bytes; a row that names an object and has no Probe fails the run, named. A GET row
#               that names no object is a list, and one whose every wildcard names a node of no
#               project is filled alike on both servers: either answers as the twin answers it.
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

# The kinds of object a Probe fills a wildcard with (read_classes.go's probeObjects, and
# write_probes.go's for the kinds only a write's path names), each with the id that does not exist
# that fills it on the missing side. PRIV's own object of each kind is priv_object's.
declare -A missing=(
  [project]=ZZNONE
  [issue]=ZZNONE-1
  [document]=00000000-0000-4000-8000-000000000000
  [document-slug]=no-such-document
  [document-version]=1
  [block]=b-nosuchblock
  [ask]=00000000-0000-4000-8000-000000000001
  [comment]=00000000-0000-4000-8000-000000000002
  [message]=00000000-0000-4000-8000-000000000003
  [handoff]=00000000-0000-4000-8000-000000000004
  [handoff-phase]=plan
  [session]=ses-nosuchsession
  [person]=nobody@example.com
  [broadcast]=00000000-0000-4000-8000-000000000005
  [going-run]=00000000-0000-4000-8000-000000000006
  [repository-owner]=acme
  [repository-name]=no-such-repo
  [result]=00000000-0000-4000-8000-000000000007
  [thread]=00000000-0000-4000-8000-000000000008
  [attempt]=1
  [email]=nobody@example.com
  [group]=no-such-group
  [agent-token]=00000000-0000-4000-8000-000000000009
  [near-copy-check]=00000000-0000-4000-8000-00000000000a
  # A repository no issue of any project names, for a row whose body names an external reference
  # (write_probes.go's probeMappedRepository). PRIV's is acme/priv-repo, PRIV-1's own.
  [mapped-repository]=no-such-repo
  # A step record's id no run holds (write_probes.go's probeStep); the issue the path names decides
  # what is hidden, so PRIV's side names the same id.
  [step]=00000000-0000-4000-8000-00000000000b
  # A run of the deploy repository no run holds. PRIV's is priv_run, the PR-checks run the seed
  # writes of the deploy repository's pull request naming PRIV-1.
  [delivery-run]=731000009
  # A priority nobody wrote. A priority is a node of no project (no_project_kinds), filled alike on
  # both servers with the one both hold.
  [priority]=no-such-priority
)
readonly priv_run=731000001
# The kinds no project owns and no seed here writes: a Delivery session group, and the secrets
# broker's credential requests, grants, machine logins and sign-ins, which no broker of this run
# holds (a live-write pause point is a test hook's). A row that names one reads or writes the same
# object for every caller, so it has no object of PRIV to hide, and the walk names it rather than
# compares it.
readonly projectless_kinds=" live-pause session-group credential-request credential-grant machine-login signin "
# The kinds of no-project node a Probe names (spec v34, Design: "A node of no project"): both
# servers hold the same ones, and the walk fills them alike on both sides. A personal token is a
# row of its person's, which both servers hold under one label; only its list can name PRIV. A
# priority is the deployment's, access-work on both servers; only its history names an issue of
# PRIV, PRIV-2's advance of it.
readonly no_project_kinds=" person agent-token priority "

# priv_object KIND PATTERN prints PRIV's object of KIND in main's seed for a row of PATTERN, or
# nothing when the seed holds none. A document is PRIV-1's spec, whose blocks the seed knows; a
# document's slug is the slug of a document of what the path names: the project's playbook under
# /projects/, the issue's notes under /issues/, the file of the conversation under /agents/. A
# session is PRIV's, ses-priv-session, whose credential lists PRIV, and a broadcast and a file are
# of its conversation.
priv_object() {
  case "$1" in
    project) printf PRIV ;;
    issue) printf PRIV-1 ;;
    document) recalled main "PRIV spec" ;;
    document-slug)
      case "$2" in
        /api/v1/projects/*) printf playbook-md ;;
        /api/v1/issues/*) printf notes-md ;;
        /api/v1/agents/*) recalled main "PRIV session file slug" ;;
      esac
      ;;
    document-version) printf 1 ;;
    block) recalled main "PRIV block 1" ;;
    ask) recalled main "PRIV ask" ;;
    comment) recalled main "PRIV comment" ;;
    message | thread) recalled main "PRIV message" ;;
    handoff) recalled main "PRIV handoff" ;;
    result) recalled main "PRIV result" ;;
    going-run) recalled main "PRIV going run" ;;
    near-copy-check) recalled main "PRIV near-copy check" ;;
    group) printf priv-group ;;
    broadcast) recalled main "PRIV broadcast" ;;
    attempt) printf 1 ;;
    email) printf '%s' "$bob" ;;
    # The label resolve fills on each server with that server's own token.
    agent-token) printf '{carol-priv-token id}' ;;
    handoff-phase) printf plan ;;
    session) printf ses-priv-session ;;
    person) printf '%s' "$bob" ;;
    repository-owner) printf acme ;;
    repository-name) printf priv-repo ;;
    mapped-repository) printf priv-repo ;;
    step) printf 00000000-0000-4000-8000-00000000000b ;;
    delivery-run) printf '%s' "$priv_run" ;;
    priority) printf access-work ;;
  esac
}

# pub_object KIND PATTERN prints PUB's object of KIND for a row of PATTERN as a path names it on
# both servers: a literal both hold alike, or a {LABEL} resolve fills with each server's own id.
# It prints nothing for a kind PUB holds none of: a handoff, a check result, a going run, a
# near-copy check, a broadcast, a run of the deploy repository, a file of a conversation, a step.
# PUB's comment is the one that names PRIV-1 and its spec.
pub_object() {
  case "$1" in
    project) printf PUB ;;
    issue) printf PUB-1 ;;
    document) printf '{PUB spec}' ;;
    document-slug)
      case "$2" in
        /api/v1/projects/*) printf playbook-md ;;
        /api/v1/issues/*) printf notes-md ;;
      esac
      ;;
    document-version | attempt) printf 1 ;;
    block) printf '{PUB block 1}' ;;
    ask) printf '{PUB ask}' ;;
    comment) printf '{PUB reference}' ;;
    message | thread) printf '{PUB message}' ;;
    handoff-phase) printf plan ;;
    session) printf ses-pub-session ;;
    repository-owner) printf acme ;;
    repository-name | mapped-repository) printf pub-repo ;;
    group | email | agent-token | person | priority) priv_object "$1" "$2" ;;
  esac
}

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

# mark_restricted BUILD PERSON makes PERSON a restricted member. No route of this stack writes a
# member kind: the outside sign-in does, when it signs in an email outside the
# deployment's domain that a project names. Until it is in this stack, the people row is written
# as that sign-in writes it.
mark_restricted() {
  local url_var=${1}_database_url
  psql "${!url_var}" -X -q -v ON_ERROR_STOP=1 \
    -c "update people set member_kind = 'restricted' where email = '$(email_of "$2")'" >/dev/null ||
    fail "$1: could not make $2 a restricted member"
}

# give_repository BUILD ISSUE REPOSITORY sets ISSUE's own repository, as carol at the issue: a
# repository belongs to the projects whose issues name it, and no mapping is written.
give_repository() {
  send "$1" carol PATCH "/api/v1/issues/$2" "$(jq -cn --arg r "$3" '{repo: $r}')"
  expect_status 200 "$1: carol names $3 as the repository of $2"
}

begin seed
# PUB first, in both, so every id and event number the twin writes is the one main writes for
# the same object.
for server in main twin; do
  seed_people "$server"
  make_project "$server" PUB
  seed_objects "$server" PUB alice
  # The blocks of PUB's notes and playbook, which the walk of PUB's rows reads under each
  # document's slug, each remembered as seed_objects remembers the spec's.
  for document in notes playbook; do
    send "$server" carol GET "$(resolve "$server" "/api/v1/artifacts/{PUB $document}/blocks")"
    expect_status 200 "$server: carol reads the blocks of PUB's $document"
    index=0
    while IFS= read -r block; do
      index=$((index + 1))
      remember "$server" "PUB $document block $index" "$block"
    done < <(jq -r '.[].id' "$work/body")
  done
  give_repository "$server" PUB-1 acme/pub-repo
  mint_token "$server" carol carol-token
  send "$server" carol POST /api/v1/agents/ses-pub-session/messages \
    '{"body":"A direct message to the PUB session.","delivery":"btw"}'
  expect_status 201 "$server: carol messages the PUB session"
  remember "$server" "PUB direct message" "$(jq -r .id "$work/body")"
  # The reference from PUB to PRIV: carol's comment on PUB-1 names PRIV's issue and its spec. It is
  # written before main holds PRIV, so it is the same comment on both servers, with the same event
  # number; once main holds PRIV it names PRIV's objects, and on the twin keys nobody holds.
  send "$server" carol POST /api/v1/issues/PUB-1/comments \
    '{"body":"This waits on dispatch://PRIV-1 and on its spec, dispatch://PRIV-1/spec."}'
  expect_status 201 "$server: carol comments on PUB-1, naming PRIV-1 and its spec"
  remember "$server" "PUB reference" "$(jq -r .id "$work/body")"
  # An access group, the deployment's, alike on both servers, which PRIV's list names on main below.
  send "$server" carol PUT /api/v1/access-groups/priv-group "$(jq -cn --arg c "$carol" '{members: [$c]}')"
  [[ "$status" == 2?? ]] || fail "$server: carol makes the group priv-group: HTTP $status $(head -c 300 "$work/body")"
  # Delivery, configured alike on both servers, so a delivery route answers a timeline rather than
  # 404 DELIVERY_NOT_CONFIGURED; and a priority, which no project owns, alike on both.
  send "$server" carol PUT /api/v1/settings/delivery \
    '{"deploy_repo":"acme/pub-repo","deploy_workflow_path":".github/workflows/deploy.yml","production_job_names":["release"],"pr_checks_workflow_path":".github/workflows/ci.yml","population_authors":["octocat"]}'
  expect_status 200 "$server: carol configures delivery"
  send "$server" carol POST /api/v1/priorities '{"id":"access-work","title":"The access work"}'
  [[ "$status" == 2?? ]] || fail "$server: carol writes the priority access-work: HTTP $status $(head -c 300 "$work/body")"
done
# bob is a person, not an object PRIV wrote, so both servers hold him alike.
mark_restricted main bob
mark_restricted twin bob
make_project main PRIV
for person in "$bob" "$carol"; do
  send main carol PUT "/api/v1/projects/PRIV/access/people/$(jq -rn --arg v "$person" '$v|@uri')"
  expect_status 200 "main: carol names $person on PRIV's list"
done
seed_objects main PRIV bob
give_repository main PRIV-1 acme/priv-repo
send main carol PUT /api/v1/projects/PRIV/access/groups/priv-group
expect_status 200 "main: carol names the group priv-group on PRIV's list"
# ses-priv-worker's history in PRIV, which shows it to bob and carol's PRIV token (confined). Only
# a session its own credential proves records history (events.queueSessionProjects: a
# broker-issued token's session; the shared token only names one), and this stack has no broker.
# So the row is a stand-in for a broker-proven session's write, as --private-by-sql's store write
# stands in for the visibility route: it is written as that write writes it, and the stand-in goes
# once the stack can mint a broker-issued session token. No event names the session, so it is no
# node.
psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 \
  -c "insert into session_projects (session_id, project_key) values ('ses-priv-worker', 'PRIV')" >/dev/null ||
  fail "main: could not record ses-priv-worker's history in PRIV"
# The objects of PRIV the walk's Probes name beyond seed_objects': a broadcast to PRIV's session
# and a file of its conversation, a handoff, a check result and a going run. Every uuid the
# broadcast's answer names is remembered, in order, so the mask names each.
send main carol POST /api/v1/broadcasts '{"body":"A broadcast to the PRIV session.","delivery":"btw","session_ids":["ses-priv-session"]}'
expect_status 201 "main: carol broadcasts to the PRIV session"
remember main "PRIV broadcast" "$(jq -r .id "$work/body")"
index=0
while IFS= read -r uuid; do
  index=$((index + 1))
  remember main "PRIV broadcast uuid $index" "$uuid"
done < <(jq -r --arg id "$(recalled main "PRIV broadcast")" '[.. | strings | select(test("^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$") and . != $id)]
  | reduce .[] as $u ([]; if index([$u]) then . else . + [$u] end) | .[]' "$work/body")
printf '\x89PNG\r\n\x1a\nproject-access' >"$work/session-file.png"
status=$(curl -sS -o "$work/body" -w '%{http_code}' --max-time "$call_seconds" -H "@$work/auth/main-shared" \
  -F "name=priv-session-file.png" -F "actor=$(jq -cn --arg s "$seed_session" '{kind: "session", id: $s}')" \
  -F "file=@$work/session-file.png;type=image/png" "${build_url[main]}/api/v1/agents/ses-priv-session/artifacts" 2>"$work/curl") ||
  status="curl failed: $(<"$work/curl")"
expect_status 201 "main: the shared token sends a file in the PRIV session's conversation"
remember main "PRIV session file slug" "$(jq -r '.artifact.slug' "$work/body")"
remember main "PRIV session file" "$(jq -r '.artifact.id' "$work/body")"
send main shared POST /api/v1/issues/PRIV-1/handoffs/plan "$(with_actor shared '{"body":"{\"next\":\"seeded\"}"}')"
expect_status 201 "main: the shared token records a handoff on PRIV-1"
remember main "PRIV handoff" "$(jq -r '.id // .handoff.id' "$work/body")"
send main carol POST /api/v1/issues/PRIV-1/artifacts "$(jq -cn '{name: "verification.md", content: ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe server records a result against a check.\n:::\n"}')"
expect_status 201 "main: carol uploads PRIV-1's Verification document"
send main shared POST /api/v1/issues/PRIV-1/results \
  "$(with_actor shared '{"check":"k1","role":"implementer","verdict":"pass","commit":"1111111111111111111111111111111111111111"}')"
expect_status 201 "main: the shared token records a result of PRIV-1's check"
remember main "PRIV result" "$(jq -r '.id // .result.id' "$work/body")"
send main carol POST /api/v1/going/requests '{"words":"What does PRIV hold?","project":"PRIV"}'
if [ "$status" = 201 ]; then
  remember main "PRIV going run" "$(jq -r .id "$work/body")"
else
  note "no going run of PRIV: POST /api/v1/going/requests answered HTTP $status $(head -c 200 "$work/body")"
fi
# The objects of PRIV the query walk's fills select (walk_queries), so each answers carol
# otherwise than a missing one does: a child of PRIV-1 that advances the priority, a pull request
# of PRIV's repository naming PRIV-1, and a thread of carol's on PRIV-1 and one on its spec, each
# of which bob answered.
send main carol POST /api/v1/issues '{"project":"PRIV","title":"A child of PRIV-1","parent":"PRIV-1","advances":["access-work"]}'
expect_status 201 "main: carol makes PRIV-2 under PRIV-1"
# The near-copy gate answered that create and records its answer as a row of PRIV's
# (recordNearCopyCheck, written after the create commits): the one object of PRIV that
# GET /api/v1/near-copy-checks/{id} names. carol, who sees PRIV, reads it back from PRIV's page.
near_copy=''
for _ in $(seq 1 20); do
  send main carol GET '/api/v1/near-copy-checks?project=PRIV'
  near_copy=$(jq -r '[.checks[]? | select(.issue == "PRIV-2") | .id][0] // empty' "$work/body")
  [ -z "$near_copy" ] || break
  sleep 0.5
done
remember main "PRIV near-copy check" "$near_copy"
psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "insert into delivery_pull_requests
    (repo, number, title, url, author, created_at, merged_at, issue_key)
  values ('acme/priv-repo', 7, 'The change of PRIV-1', 'https://github.com/acme/priv-repo/pull/7', 'octocat',
    now() - interval '3 hours', now() - interval '1 hour', 'PRIV-1')" >/dev/null ||
  fail "main: could not write the pull request of PRIV's repository"
# The run of PRIV that GET /api/v1/delivery/runs/{id} names: a PR-checks run of the deploy
# repository is shown where its pull request is (delivery.go's deliveryRunShown), and the pull
# request where its issue is, so a run of a pull request naming PRIV-1 is PRIV's. The delivery loop
# writes both rows from GitHub, which the fake GitHub does not serve, so they are written as it
# writes them, the run's jobs complete.
psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "insert into delivery_pull_requests
    (repo, number, title, url, author, created_at, merged_at, issue_key)
  values ('acme/pub-repo', 8, 'A change for PRIV-1', 'https://github.com/acme/pub-repo/pull/8', 'octocat',
    now() - interval '3 hours', now() - interval '2 hours', 'PRIV-1')" \
  -c "insert into delivery_runs
    (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, completed_at, conclusion, url, jobs_complete)
  values ('acme/pub-repo', $priv_run, 'pr_checks', 8, '2222222222222222222222222222222222222222',
    now() - interval '3 hours', now() - interval '3 hours', now() - interval '170 minutes', 'success',
    'https://github.com/acme/pub-repo/actions/runs/$priv_run', true)" >/dev/null ||
  fail "main: could not write the deploy repository's pull request naming PRIV-1 and its PR-checks run"
# A thread on PRIV-1, and one on PRIV's playbook, a document of the project, which takes its
# comments on its own route (a document of an issue takes them through the issue's).
for thread in /api/v1/issues/PRIV-1/comments "/api/v1/artifacts/$(recalled main "PRIV playbook")/comments"; do
  send main carol POST "$thread" '{"body":"carol asks something here."}'
  expect_status 201 "main: carol writes a thread at $thread"
  send main bob POST "$thread" "$(jq -cn --arg r "$(jq -r .id "$work/body")" '{body: "bob answers carol.", reply_to: $r}')"
  expect_status 201 "main: bob answers carol's thread at $thread"
done
mint_token main carol carol-priv-token '["PRIV"]'
mint_token main alice alice-priv-token '["PRIV"]'
# The twin's alice holds a token whose list names PRIV, which the twin never wrote: no route mints
# a list naming a project nobody sees, so the list is written beside the mint, on the token the
# mint made. It sees what alice's PRIV token sees once PRIV is private: nothing.
mint_token twin alice alice-priv-token
psql "$twin_database_url" -X -q -v ON_ERROR_STOP=1 \
  -c "update agent_tokens set projects = '{PRIV}' where id = '$(recalled twin "alice-priv-token id")'" >/dev/null ||
  fail "twin: could not write the list of alice's PRIV token"
# carol's second token is a row of carol's, a no-project object; only its list names PRIV, and
# that list is what a reader who does not see PRIV may not read. So the twin holds the same token,
# listing nothing, used once as main's is used, and carol's list of her tokens compares alike.
mint_token twin carol carol-priv-token
send twin carol-priv-token GET /api/v1/whoami
expect_status 200 "twin: carol's second token reads whoami"
note "main holds PUB and PRIV, the twin PUB alone"
pass

readonly listed_callers=(bob carol carol-priv-token)
readonly unseeing_callers=(alice alice-priv-token carol-token shared)
# The object routes the listed callers read, each naming one object of {P}'s seed.
readonly object_reads=(
  "/api/v1/projects/{P}/artifacts"
  "/api/v1/issues/{P}-1"
  "/api/v1/comments/{{P} comment}"
  "/api/v1/asks/{{P} ask}"
  "/api/v1/messages/{{P} message}"
  "/api/v1/artifacts/{{P} spec}"
  "/api/v1/artifacts/{{P} notes}"
  "/api/v1/artifacts/{{P} playbook}"
)

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
    elif [ "$status" != 409 ] || [ "$(jq -r '.code // empty' "$work/body" 2>/dev/null)" != PRIVATE_PROJECTS_UNAVAILABLE ]; then
      # The route reaches the record only once PRIV is provisioned and the store complete: any
      # other refusal is a precondition the stand-in would skip.
      fail "--private-by-sql stands in only for 409 PRIVATE_PROJECTS_UNAVAILABLE, the refusal of a switch whose preconditions hold: $refusal"
    else
      psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PRIV'" >/dev/null ||
        fail "main: the stand-in could not make PRIV private"
      priv_private=1
      note "PRIV is private by the store write the route makes (--private-by-sql): the route answered 409 PRIVATE_PROJECTS_UNAVAILABLE, its preconditions held"
    fi
  fi
  pass
fi

begin listed
# Each listed caller reads each object of PRIV: 200, and the answer names the object.
found=0
for caller in "${listed_callers[@]}"; do
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
note "$found of $((${#listed_callers[@]} * ${#object_reads[@]})) reads of PRIV's objects found them"
pass

# compare_missing CALLER METHOD TEMPLATE PRIV_PATH MISSING_PATH PRIV_MASK MISSING_MASK [BODY]: as
# CALLER on main, the request to PRIV_PATH and the one to MISSING_PATH answer the same, each
# masked with its own fill (the files PRIV_MASK and MISSING_MASK).
compare_missing() {
  local caller=$1 method=$2 template=$3
  send main "$caller" "$method" "$4" "${8:-}"
  keep_answer hidden main "$6"
  send main "$caller" "$method" "$5" "${8:-}"
  keep_answer missing main "$7"
  same_answers "$caller $method $template" hidden missing
}

# Every answer says whether the deployment holds a private project (KF7): main, which provisions
# PRIV, says 1, and the twin, which provisions none, 0. Beside the twin, each server's answer must
# say its own, and the line is dropped from both before the headers are compared.
readonly main_header='dispatch-private-projects: 1' twin_header='dispatch-private-projects: 0'

# compare_twin CALLER PATH [METHOD BODY]: as CALLER, main and the twin answer PATH the same, each
# with its own ids filled in for the {LABEL}s PATH names. A write (METHOD other than GET) writes a
# row of its own on each server, whose new id and time no seed names, so of a write the two
# statuses alone are held alike.
compare_twin() {
  local caller=$1 path=$2 method=${3:-GET} body=${4:-} server
  for server in main twin; do
    send "$server" "$caller" "$method" "$(resolve "$server" "$path")" "$body"
    keep_answer "$server" "$server"
  done
  if [ "$method" != GET ]; then
    for server in main twin; do
      : >"$work/answers/$server.body"
      : >"$work/answers/$server.headers"
    done
    same_answers "$caller $method $path (beside the twin)" main twin
  else
    same_answers "$caller $method $path (beside the twin)" main twin "$main_header" "$twin_header"
  fi
}

begin confined
# bob and carol's PRIV token: PUB's objects answer as missing, and the lists hold PRIV alone.
: >"$work/fill-empty"
for caller in bob carol-priv-token; do
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
note "bob and carol's PRIV token read PUB's objects, the projects, the people, the agents and a direct message"
pass

# Each server's delivery loop reconciles on its own timer once delivery is configured (every second
# here, DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL, rather than five minutes), and a reconcile
# writes what it found into the settings and the pipeline's freshness. So both servers are read
# only once each has reconciled at least once, and their answers hold the same outcome.
reconciled() {
  local server
  for server in main twin; do
    send "$server" carol GET /api/v1/settings/delivery
    [ "$status" = 200 ] && [ "$(jq '.last_error != null or .last_reconcile_at != null' "$work/body")" = true ] || return 1
  done
}

# Each server pages its event log only up to its safe horizon (listEventLog, sinceHorizon), and a
# server can answer a read before that horizon has passed what the seed wrote, so one server's log
# would read short beside the other's. The walk compares the log as each unseeing caller, so each
# server is read only once its log, as each caller that wrote in the seed as itself reads it, holds
# that caller's own newest event, which a caller always reads: carol, who wrote last, and alice. A
# token wrote nothing, and the shared token's writes name a session it does not prove and are in
# PRIV, which it does not see. After carol's newest event the seed writes only bob's answers in
# PRIV, which no unseeing caller reads.
#
# own_newest_event BUILD PERSON prints the id of the newest event of BUILD's database that PERSON
# wrote as themself, or 0 when they wrote none.
own_newest_event() {
  local url_var=${1}_database_url
  psql "${!url_var}" -X -tAc "select coalesce(max(id), 0) from events
    where actor->>'kind' = 'user' and actor->>'id' = '$(email_of "$2")'"
}

# logs_hold_own_writes waits, bounded, on each server until the log as carol and as alice holds
# that caller's own newest event.
logs_hold_own_writes() {
  local server caller newest
  for server in main twin; do
    for caller in carol alice; do
      newest=$(own_newest_event "$server" "$caller") || fail "$server: could not read $caller's newest event"
      [ "$newest" -gt 0 ] || continue
      until_true 120 "$server's event log, as $caller reads it, to hold $caller's newest event, $newest" \
        event_log_reaches "$server" "$caller" "$newest"
    done
  done
}

# route_rows SELECT prints, in their order, the rows of the route index that the jq filter SELECT
# keeps, each with what its Probe (RouteProbe, read once from probes.json) says of it, one line a
# row and its fields joined by the unit separator (\x1f), a byte no compact JSON holds: the method;
# the path; 1 when a Probe names the row; its test_hook; its no_project; its body as compact JSON;
# 1 when its Probe map is not null; and that map, each wildcard and its kind joined by \x1d and the
# pairs by \x1e (read_probe). A field the Probe does not hold is empty.
route_rows() {
  jq -r --slurpfile probes "$work/probes.json" "$1"' | . as $row
    | (first($probes[0][] | select(.method == $row.method and .pattern == $row.path)) // null) as $entry
    | [$row.method, $row.path,
       (if $entry == null then "" else "1" end),
       ($entry.test_hook // false | tostring),
       ($entry.no_project // ""),
       ((($entry.body // empty) | tojson) // ""),
       (if $entry.probe == null then "" else "1" end),
       ([$entry.probe // {} | to_entries[] | "\(.key)\u001d\(.value // "")"] | join("\u001e"))]
    | join("\u001f")' "$work/routes"
}

# read_probe PAIRS sets probe_kind to the kind of each wildcard PAIRS names (route_rows' last field).
declare -A probe_kind=()
read_probe() {
  local pair
  probe_kind=()
  while IFS= read -r -d $'\x1e' pair; do
    probe_kind[${pair%%$'\x1d'*}]=${pair#*$'\x1d'}
  done <<<"${1:+$1$'\x1e'}"
}

# walk_rows SELECT walks the rows of the route index that the jq filter SELECT keeps: a row whose
# path names an object is filled from its Probe with PRIV's objects and with missing ones, and
# each unseeing caller is answered the same for both; a GET row that names no object is a list,
# answered as the twin answers it. A row that names an object with no Probe, or one whose object
# the seed does not hold, is written to what did not run. A test hook (RouteProbe's test_hook) is
# left out, named: the server mounts it only under DISPATCH_TEST_HOOKS, which no deployment sets.
# A person, a personal token and a priority are no-project nodes, which spec v34 (Design, "A node
# of no project") shows to an open viewer as it does today, and both servers hold the same ones: a
# row whose every wildcard is one of them is filled alike on both servers and answered as the twin
# answers it. The rows whose object no project owns (RouteProbe's no_project) are walked first:
# each must be refused, which changes nothing, and a write walked before them would change the
# callers they are refused to, as DELETE /api/v1/me/agent-tokens/{id} does, which carol's token
# walks on carol's PRIV token.
walk_rows() {
  local method pattern has_entry test_hook no_project body has_probe pairs wildcards hidden none skip wildcard kind object caller target
  walked=0 lists=0 refused=0
  while IFS=$'\x1f' read -r method pattern has_entry test_hook no_project body has_probe pairs; do
    if [ "$test_hook" = true ]; then
      note "left out: $method $pattern is a test hook, which only DISPATCH_TEST_HOOKS mounts"
      continue
    fi
    if [ -n "$no_project" ]; then
      # The object belongs to no project, so there is no object of PRIV to hide: the row is held
      # to its class's refusal (scopeDeployment) of every caller that is not open.
      target=$(sed -E 's/\{[^}]+\}/project-access-walk/g' <<<"$pattern")
      for caller in bob alice-priv-token carol-priv-token; do
        send main "$caller" "$method" "$target" "$body"
        refused=$((refused + 1))
        problem=''
        [ "$status" = 403 ] ||
          problem="$caller $method $pattern ($no_project): HTTP $status, want 403 for a caller that is not open: $(head -c 300 "$work/body")"
        judge "$status" "$problem"
      done
      continue
    fi
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    if [ -z "$wildcards" ]; then
      if [ "$method" = GET ]; then
        for caller in "${unseeing_callers[@]}"; do compare_twin "$caller" "$pattern"; done
        lists=$((lists + 1))
      fi
      continue
    fi
    if [ -z "$has_entry" ] || [ -z "$has_probe" ]; then
      unrun "$method $pattern names an object and has no Probe"
      continue
    fi
    read_probe "$pairs"
    hidden=$pattern
    none=$pattern
    : >"$work/fill-hidden"
    : >"$work/fill-missing"
    skip=''
    for wildcard in $wildcards; do
      kind=${probe_kind[$wildcard]:-}
      if [[ "$projectless_kinds" == *" $kind "* ]]; then
        skip=projectless
        note "not walked: $method $pattern: {$wildcard} is a $kind, which no project owns"
        break
      fi
      object=$(priv_object "$kind" "$pattern")
      if [ -z "$object" ] || [ -z "${missing[$kind]:-}" ]; then
        skip=unseeded
        unrun "$method $pattern: the seed holds no ${kind:-object} of PRIV for {$wildcard}"
        break
      fi
      # A no-project node fills both sides alike: it is no object PRIV wrote. It goes into the
      # path as it is, so a {LABEL} it names is filled with each server's own id (compare_twin).
      if [[ "$no_project_kinds" == *" $kind "* ]]; then
        hidden=${hidden//"{$wildcard}"/$object}
        none=${none//"{$wildcard}"/$object}
        continue
      fi
      hidden=${hidden//"{$wildcard}"/$(jq -rn --arg v "$object" '$v|@uri')}
      none=${none//"{$wildcard}"/$(jq -rn --arg v "${missing[$kind]}" '$v|@uri')}
      # A fill that is the same on both sides is no difference to mask. An issue's key names its
      # project up to its first -, which a refusal built from the key the caller sent names
      # (ROLE_HOLDER_FORBIDDEN): it is masked too, after the key, so a hidden answer naming the
      # project and a missing one naming ZZNONE read alike, and an answer naming another object
      # of PRIV still differs.
      if [ "$object" != "${missing[$kind]}" ]; then
        printf '%s\t%s\n' "$wildcard" "$object" >>"$work/fill-hidden"
        printf '%s\t%s\n' "$wildcard" "${missing[$kind]}" >>"$work/fill-missing"
        if [ "$kind" = issue ]; then
          printf '%s project\t%s\n' "$wildcard" "${object%%-*}" >>"$work/fill-hidden"
          printf '%s project\t%s\n' "$wildcard" "${missing[$kind]%%-*}" >>"$work/fill-missing"
        fi
      fi
    done
    [ -z "$skip" ] || continue
    if [ "$hidden" = "$none" ]; then
      # Every wildcard is a no-project node: the path is the same on both servers.
      for caller in "${unseeing_callers[@]}"; do
        compare_twin "$caller" "$hidden" "$method" "$body"
      done
      lists=$((lists + 1))
      continue
    fi
    for caller in "${unseeing_callers[@]}"; do
      compare_missing "$caller" "$method" "$pattern" "$hidden" "$none" "$work/fill-hidden" "$work/fill-missing" "$body"
    done
    walked=$((walked + 1))
  done < <(route_rows "$1" | LC_ALL=C sort -s -r -t $'\x1f' -k5,5)
}

# walk_pub_rows walks every GET row whose path names an object of a project, filled with PUB's
# objects (pub_object) alike on both servers, and holds each unseeing caller's answer to the
# twin's. PUB is the same on both, but for the reference from PUB to PRIV, which on main names
# PRIV's objects and on the twin keys nobody holds: a read of PUB that names an object of PRIV
# through it, or answers otherwise for it, differs. A row whose object PUB holds none of is not
# walked here, and one whose every wildcard is a no-project node is walked by walk_rows.
walk_pub_rows() {
  local pattern test_hook pairs wildcards wildcard kind object path of_project caller
  pub_walked=0
  while IFS=$'\x1f' read -r _ pattern _ test_hook _ _ _ pairs; do
    [ "$test_hook" != true ] || continue
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    [ -n "$wildcards" ] || continue
    read_probe "$pairs"
    path=$pattern of_project=''
    for wildcard in $wildcards; do
      kind=${probe_kind[$wildcard]:-}
      object=$(pub_object "$kind" "$pattern")
      if [ -z "$object" ]; then
        path=''
        break
      fi
      [[ "$no_project_kinds" == *" $kind "* ]] || of_project=1
      path=${path//"{$wildcard}"/$object}
    done
    [ -n "$path" ] && [ -n "$of_project" ] || continue
    for caller in "${unseeing_callers[@]}"; do
      compare_twin "$caller" "$path"
    done
    pub_walked=$((pub_walked + 1))
  done < <(route_rows 'select(.method == "GET")')
}

# The query parameters of the GET rows, read from the OpenAPI this checkout's server serves (`go run
# ./cmd/dispatch openapi --check` holds the checked-in document equal to the route table), each
# classified once. A parameter that names an object of a project is filled with PRIV's and with a
# missing one, as a path's wildcard is, and each unseeing caller is answered the same for both; one
# that names a person, a node of no project both servers hold, or a session, filled with PRIV's,
# which the twin does not hold, is answered as the twin answers it; one that names no object is
# not filled. A parameter these tables do not classify did not run, so a new one cannot pass
# unread. A key "ROUTE NAME" classifies NAME on ROUTE alone, ahead of NAME.
declare -A query_kind=(
  [project]=project
  [number_project]=project
  [issue]=issue
  [parent]=issue
  [ref]=issue
  [artifact]=document
  [repo]=repository
  # Of these routes, from and to name nodes, written as dispatch:// references; of every other
  # route that takes them, times.
  ["/api/v1/block-links from"]=spec-ref
  ["/api/v1/block-links to"]=spec-ref
  ["/api/v1/references from"]=issue-ref
  ["/api/v1/references to"]=issue-ref
  # What many references point at: each ref is a dispatch:// reference, as the two above.
  ["/api/v1/reference-targets ref"]=issue-ref
  # The question map's id names a comment once its kind says so (query_fixed).
  ["/api/v1/references/questions id"]=comment
  # An issue's comments are those of the path's issue, so ?artifact= names a document of that
  # issue, and a document of PRIV goes under a path of PRIV, which the path walk answers as missing.
  ["/api/v1/issues/{key}/comments artifact"]=none
)
# What a route, or a "ROUTE NAME", needs beside the parameter filled: a document at the other end
# of a block link, a kind for the question map, the text of a search.
declare -A query_fixed=(
  ["/api/v1/references/questions"]='kind=comment&'
  ["/api/v1/block-links from"]='to=dispatch%3A%2F%2FPUB-1%2Fspec&'
  ["/api/v1/block-links to"]='from=dispatch%3A%2F%2FPUB-1%2Fspec&'
  ["/api/v1/search"]='q=paragraph&'
  ["/api/v1/search/results"]='q=paragraph&'
)
readonly query_no_project=" author author_session parent_agent assignee approver person "
# exclude_issue and exclude_issues name an issue of the path's own project, so an issue of PRIV
# goes under a path of PRIV, which the path walk answers as missing; thread is a flag (1 or none).
# account, account_issuer, account_subject and credential_created_at describe the credential a
# membership question asks about (the broker's introspection's account, or none, and the moment it
# was made): a Google account and a time, never an object a project owns.
readonly query_no_object=" account account_issuer account_subject advances after at before blocks closed_before closed_since component credential_created_at cursor deployed download exclude_issue exclude_issues from ids keywords_only kind label limit max_chars moved offset open order pinned priority q rework route_status session since source_kind state status summary text thread title to unlinked until updated_since versions wait "

# query_fill KIND prints PRIV's object of KIND and the missing one, a tab between them.
query_fill() {
  case "$1" in
    project) printf 'PRIV\t%s' "${missing[project]}" ;;
    issue) printf 'PRIV-1\t%s' "${missing[issue]}" ;;
    issue-ref) printf 'dispatch://PRIV-1\tdispatch://%s' "${missing[issue]}" ;;
    spec-ref) printf 'dispatch://PRIV-1/spec\tdispatch://%s/spec' "${missing[issue]}" ;;
    # A thread is on a document of its own route only for a project's document (the seed's
    # thread on PRIV's playbook); a document of an issue takes its comments through the issue.
    document) printf '%s\t%s' "$(recalled main "PRIV playbook")" "${missing[document]}" ;;
    comment) printf '%s\t%s' "$(recalled main "PRIV comment")" "${missing[comment]}" ;;
    repository) printf 'acme/priv-repo\tacme/%s' "${missing[repository-name]}" ;;
  esac
}

# walk_queries walks every query parameter of every GET row of the route index (query_kind). Of a
# row whose path names an object, the path is filled with PUB's, which every unseeing caller but
# alice's PRIV token sees; that token sees no project, so it is answered alike for the two fills
# whatever the parameter names. carol, who sees PRIV, must be answered 2xx for PRIV's fill and
# otherwise than for the missing one, each masked with its own fill: a refusal says the fill is no
# value the parameter takes, and an answer alike says the fill selects nothing of PRIV, and either
# way a hidden and a missing answer would be alike whatever the server hides, so the parameter did
# not run.
walk_queries() {
  local pattern test_hook pairs wildcards wildcard kind path param fixed values hidden none caller
  local -A query_params=()
  queried=0
  # Each GET path's query parameters, in the OpenAPI's order, one per line, read in one pass.
  while IFS=$'\x1f' read -r pattern param; do
    query_params[$pattern]+=$param$'\n'
  done < <(jq -r '.paths | to_entries[] | .key as $p | .value.get.parameters[]? | select(.in == "query") | [$p, .name] | join("\u001f")' \
    "$root/packages/contracts/schemas/dispatch-openapi.json")
  while IFS=$'\x1f' read -r _ pattern _ test_hook _ _ _ pairs; do
    [ "$test_hook" != true ] || continue
    read_probe "$pairs"
    path=$pattern
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    for wildcard in $wildcards; do
      case "${probe_kind[$wildcard]:-}" in
        project) path=${path//"{$wildcard}"/PUB} ;;
        issue) path=${path//"{$wildcard}"/PUB-1} ;;
        *)
          path=''
          break
          ;;
      esac
    done
    while IFS= read -r param; do
      [ -n "$param" ] || continue
      kind=${query_kind["$pattern $param"]:-${query_kind[$param]:-}}
      [ "$kind" != none ] || continue
      if [ -z "$kind" ] && [[ "$query_no_project" != *" $param "* ]]; then
        [[ "$query_no_object" == *" $param "* ]] || unrun "GET $pattern takes ?$param=, which the query walk does not classify"
        continue
      fi
      if [ -z "$path" ]; then
        unrun "GET $pattern?$param=: the query walk cannot fill this path with PUB's objects"
        continue
      fi
      fixed=${query_fixed[$pattern]:-}
      if [ -z "$kind" ]; then
        # A person, whom both servers hold alike, or PRIV's session, which the twin does not hold:
        # either is answered as the twin answers it.
        case "$param" in
          author_session | parent_agent) kind=session ;;
          *) kind=person ;;
        esac
        for caller in "${unseeing_callers[@]}"; do
          compare_twin "$caller" "$path?$fixed$param=$(jq -rn --arg v "$(priv_object "$kind" "$pattern")" '$v|@uri')"
        done
        queried=$((queried + 1))
        continue
      fi
      values=$(query_fill "$kind")
      fixed=${query_fixed["$pattern $param"]:-${query_fixed[$pattern]:-}}
      hidden="$path?$fixed$param=$(jq -rn --arg v "${values%%$'\t'*}" '$v|@uri')"
      none="$path?$fixed$param=$(jq -rn --arg v "${values#*$'\t'}" '$v|@uri')"
      printf '%s\t%s\n' "$param" "${values%%$'\t'*}" >"$work/fill-hidden"
      printf '%s\t%s\n' "$param" "${values#*$'\t'}" >"$work/fill-missing"
      # A reference's refusal names the issue by its bare key, which the masks name too.
      if [[ "$kind" == *-ref ]]; then
        printf '%s key\tPRIV-1\n' "$param" >>"$work/fill-hidden"
        printf '%s key\t%s\n' "$param" "${missing[issue]}" >>"$work/fill-missing"
      fi
      send main carol GET "$hidden"
      keep_answer seen-hidden main "$work/fill-hidden"
      send main carol GET "$none"
      keep_answer seen-missing main "$work/fill-missing"
      if [[ "$(<"$work/answers/seen-hidden.status")" != 2* ]]; then
        unrun "GET $pattern?$param=<$kind>: carol, who sees PRIV, is answered HTTP $(<"$work/answers/seen-hidden.status") for PRIV's, so the fill is no value the parameter takes: $(head -c 200 "$work/answers/seen-hidden.body")"
        continue
      fi
      if [ "$(<"$work/answers/seen-hidden.status")" = "$(<"$work/answers/seen-missing.status")" ] &&
        cmp -s "$work/answers/seen-hidden.body" "$work/answers/seen-missing.body"; then
        unrun "GET $pattern?$param=<$kind>: carol, who sees PRIV, is answered alike for PRIV's and a missing one, so the fill selects nothing of PRIV and a leak could not show"
        continue
      fi
      for caller in "${unseeing_callers[@]}"; do
        compare_missing "$caller" GET "$pattern?$param=<$kind>" "$hidden" "$none" "$work/fill-hidden" "$work/fill-missing"
      done
      queried=$((queried + 1))
    done <<<"${query_params[$pattern]:-}"
  done < <(route_rows 'select(.method == "GET")')
}

if [ "$priv_private" = 1 ]; then
  begin walk
  until_true 120 "both servers to reconcile delivery once" reconciled
  # The walk holds every list beside the twin, GET /api/v1/search/coverage among them, which counts
  # what each server's embedding queue has written so far: main embeds PRIV's items beside PUB's,
  # each on its own scan cadence, so mid-queue the two count PUB's items current differently. So the
  # walk starts only once neither owes a vector (access-lib.sh's indexed), as the search section's
  # reads do.
  until_true 120 "both servers to owe no vector" indexed main twin
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
  # Meaning search answers from vectors the embedding queue writes behind each write, so both
  # servers are read only once neither owes a vector (access-lib.sh's indexed).
  until_true 120 "both servers to owe no vector" indexed main twin
  for caller in "${unseeing_callers[@]}"; do
    for query in "q=paragraph&keywords_only=true" "q=paragraph" "q=remark&keywords_only=true" "q=remark"; do
      compare_twin "$caller" "/api/v1/search/results?$query"
    done
    compare_twin "$caller" "/api/v1/search?q=paragraph"
  done
  pass

  begin sockets
  # The block schema needs a credential, as every route but sign-in, health and the web app does.
  schema_version=$(curl -fsS --max-time 5 -H "@$work/auth/main-shared" "${build_url[main]}/api/v1/schema/blocks" | jq -r '.version // .schema_version // empty')
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
    # caller but alice's PRIV token sees PUB, so the twin sends it the PUB comment, and an answer
    # holding no event fails the pair: the compare must see the socket send something. alice's PRIV
    # token sees no project, so both servers send it nothing, which the compare holds them to.
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
    if [ "$caller" != alice-priv-token ]; then
      problem=''
      grep -q '"type":"event"' "$work/answers/twin.body" ||
        problem="$caller /ws/live: the twin sent no event of carol's PUB comment, so the compare saw nothing: $(head -c 300 "$work/answers/twin.body")"
      judge "$(<"$work/answers/twin.status")" "$problem"
    fi
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
