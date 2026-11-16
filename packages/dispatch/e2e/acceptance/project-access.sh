#!/usr/bin/env bash
# c-private-unseen (dispatch://INFRA-3862, verification v17; plan task t-a-22): a private project
# is unseen by every caller its access list does not let in, on every read path, and seen by
# every caller it does.
#
# It starts two servers of this checkout, `main` and `twin`, each on a database of its own, with
# a NATS of its own (the bus a private project's record is written to), a fake Envoy of its own
# and one fake GitHub, and seeds them through the routes (access-lib.sh):
#   main  verification v16's seed: PUB, public, and PRIV, made private through the visibility
#         route; alice, a full member no list names; bob, a restricted member PRIV's list names;
#         carol, a full member PRIV's list names, who owns both; carol's token that lists PRIV,
#         alice's token that lists PRIV, carol's token that lists no project, and the shared
#         token. Each project holds an issue whose spec is a document with blocks, a comment, an
#         ask, a message, an issue document, a project document, an agent session of its own
#         that the fake Envoy lists, and a repository mapped to it.
#   twin  the same seed with PRIV never written: the same PUB, the same people, carol's token
#         that lists no project, and alice's token, whose list names PRIV, a project the twin
#         never holds.
# Then, in sections, each of which runs whatever the ones before found:
#   listed      bob, carol and carol's PRIV token find each object of PRIV.
#   confined    bob and carol's PRIV token find nothing of PUB, no person and no agent outside
#               PRIV, and no direct message that is not their own.
#   private     PRIV is made private through PATCH /api/v1/projects/PRIV/access; the sections
#               below need it, and are reported as not run when the route refuses. With
#               --private-by-sql, a route that refuses is stood in for by the store write it
#               makes (`update projects set visibility = 'private'`), so every section below
#               runs; the stand-in is written to what did not run, so such a run never passes.
#   walk        for alice, alice's PRIV token, carol's token that lists no project and the shared
#               token ("the unseeing callers"): every GET row of GET /api/v1 and every write row
#               whose path names an object, filled once with PRIV's objects and once with ids that
#               do not exist, each kind of object as the row's Probe names it, answers the same
#               status and bytes; a row that names an object and has no Probe fails the run,
#               named. A GET row that names no object is a list: its answer equals the twin's.
#   search      both legs of search, the keyword leg alone and the two together, answer each
#               unseeing caller what the twin answers.
#   sockets     the document socket answers an unseeing caller for PRIV's spec what it answers
#               for a document that does not exist, and lets bob, carol and carol's PRIV token in;
#               the live socket replays each unseeing caller the event log the twin replays.
#   proxy       the dashboard's GitHub proxy answers an unseeing person for PRIV's repository
#               what it answers for a repository the App is not installed on.
# The Probes are read from the server's own table (`go run ./internal/dispatch/api/cmd/routeprobes`),
# never from a list here. Answers are compared after access-lib.sh's mask, and an answer to a
# filled path also with the ids it was filled with masked as the wildcard they fill, which is the
# only text in which a hidden object's answer and a missing one's may differ.
#
# Usage, from packages/dispatch:
#
#   DATABASE_URL=<a loopback PostgreSQL this run may create databases on> \
#     bash e2e/acceptance/with-harness.sh bash e2e/acceptance/project-access.sh
#
# The harness is not needed: the run starts every process it reads, and reads DATABASE_URL only
# for the server it names. It prints `ok <section>` for a section that held, and fails at the end
# naming every pair of answers that differed and everything it could not run, with why.
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
case "${1:-}" in
  '') ;;
  --private-by-sql) private_by_sql=1 ;;
  *) fail "unknown argument '$1': this driver takes --private-by-sql alone" ;;
esac
[ $# -le 1 ] || fail "this driver takes --private-by-sql alone"
# two_builds_setup reads DATABASE_URL, drops every DISPATCH_*, ENVOY_*, NATS_* and PG* setting the
# shell inherited, and arms two-builds' teardown, which access_cleanup below runs last.
two_builds_setup 'project access e2e'
main_pid='' twin_pid='' nats_main_pid='' nats_twin_pid='' envoy_main_pid='' envoy_twin_pid='' fake_github_pid=''
access_cleanup() {
  local pid
  for pid in "$main_pid" "$twin_pid" "$nats_main_pid" "$nats_twin_pid" "$envoy_main_pid" "$envoy_twin_pid" "$fake_github_pid"; do
    stop_pid "$pid"
  done
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

# The kinds of object a Probe fills a wildcard with (read_classes.go's probeObjects), each with
# the id that does not exist that fills it on the missing side. PRIV's own object of each kind is
# priv_object's.
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
)
# The kinds no project owns: a row that names one reads the same object for every caller, so it
# has no object of PRIV to hide, and the walk names it rather than compares it.
readonly projectless_kinds=" priority delivery-run live-pause "
# The kinds of no-project node a Probe names (spec v34, Design: "A node of no project"): both
# servers hold the same ones, and the walk fills them alike on both sides.
readonly no_project_kinds=" session person "

# priv_object KIND PATTERN prints PRIV's object of KIND in main's seed for a row of PATTERN, or
# nothing when the seed holds none. A document is PRIV-1's spec, whose blocks the seed knows; a
# document's slug is the slug of a document of what the path names: the project's playbook under
# /projects/, the issue's notes under /issues/.
priv_object() {
  case "$1" in
    project) printf PRIV ;;
    issue) printf PRIV-1 ;;
    document) recalled main "PRIV spec" ;;
    document-slug)
      case "$2" in
        /api/v1/projects/*) printf playbook-md ;;
        /api/v1/issues/*) printf notes-md ;;
      esac
      ;;
    document-version) printf 1 ;;
    block) recalled main "PRIV block 1" ;;
    ask) recalled main "PRIV ask" ;;
    comment) recalled main "PRIV comment" ;;
    message) recalled main "PRIV message" ;;
    handoff-phase) printf plan ;;
    session) printf ses-priv-session ;;
    person) printf '%s' "$bob" ;;
    repository-owner) printf acme ;;
    repository-name) printf priv-repo ;;
  esac
}

begin services
# Read through ${!port_var} below.
# shellcheck disable=SC2034
main_port=$(free_port)
# shellcheck disable=SC2034
twin_port=$(free_port)
# shellcheck disable=SC2034 # read through ${!database_var} below
main_database_url='' twin_database_url=''
github_port=$(free_port)
FAKE_GITHUB_PORT=$github_port start_process fake_github bun "$root/packages/dispatch/e2e/fake-github.ts"
await_start fake_github "$fake_github_pid" 60 "the fake GitHub to answer /healthz" \
  curl -fsS --max-time 2 "http://127.0.0.1:$github_port/healthz"
# The App is installed on both projects' repositories and on no other.
curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$github_port/__fixture/repos" \
  -d '{"acme/pub-repo":{"contents":"read","installation_id":71},"acme/priv-repo":{"contents":"read","installation_id":72}}' \
  >/dev/null || fail "the fake GitHub refused its repositories"
agent_token=$(openssl rand -hex 24)
signin_key=$(openssl rand -hex 32)
app_pem=$(openssl genrsa 2048 2>/dev/null)
for server in main twin; do
  port_var=${server}_port
  nats_port=$(free_port)
  envoy_port=$(free_port)
  mkdir -p "$work/nats-$server"
  start_process "nats_$server" "$work/bin/natsd" -p "$nats_port" -store_dir "$work/nats-$server"
  nats_pid_var=nats_${server}_pid
  await_start "nats_$server" "${!nats_pid_var}" 30 "the $server NATS to listen" port_answers "$nats_port"
  FAKE_ENVOY_PORT=$envoy_port start_process "envoy_$server" bun "$root/packages/dispatch/e2e/fake-envoy.ts"
  envoy_pid_var=envoy_${server}_pid
  await_start "envoy_$server" "${!envoy_pid_var}" 60 "the $server fake Envoy to answer /healthz" \
    curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz"
  # Both servers' listeners hold the same sessions: a session is a no-project node, not an object
  # PRIV wrote, so the twin holds PRIV's session too.
  sessions='[{"session_id":"ses-pub-session","title":"Works on PUB","projects":["PUB"]}'
  sessions+=',{"session_id":"ses-priv-session","title":"Works on PRIV","projects":["PRIV"]}'
  curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$envoy_port/__fixture/sessions" \
    -d "$sessions]" >/dev/null || fail "the $server fake Envoy refused its sessions"
  database_var=${server}_database_url
  printf -v "$database_var" '%s' ''
  own_database "$database_var" "$server"
  # The server as start_server starts it, with the bus on and the deployment's own settings for
  # what this check reads: private projects allowed, the dev embedder for the meaning leg, the
  # test hooks the route index lists, and the GitHub App the proxy mints its tokens as.
  envoy_port=$envoy_port database_url=${!database_var} start_server "$server" "$work/bin/dispatch" "${!port_var}" \
    DISPATCH_NATS_DISABLED= \
    NATS_URLS="nats://127.0.0.1:$nats_port" \
    DISPATCH_PRIVATE_PROJECTS=1 \
    DISPATCH_DEV_EMBEDDER=1 \
    DISPATCH_TEST_HOOKS=1 \
    DISPATCH_APP_CLIENT_ID=Iv1.e2efake \
    DISPATCH_APP_PEM_B64="$app_pem" \
    DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$github_port"
  build_url[$server]=http://127.0.0.1:${!port_var}
done
await_start main "$main_pid" 120 "main to answer /healthz" curl -fsS --max-time 2 "${build_url[main]}/healthz"
await_start twin "$twin_pid" 120 "the twin to answer /healthz" curl -fsS --max-time 2 "${build_url[twin]}/healthz"
note "main serves ${build_url[main]}, the twin ${build_url[twin]}"
pass

# mark_restricted BUILD PERSON makes PERSON a restricted member. No route of this stack writes a
# member kind: the outside sign-in does (PlanB's B2), when it signs in an email outside the
# deployment's domain that a project names. Until it is in this stack, the people row is written
# as that sign-in writes it.
mark_restricted() {
  local url_var=${1}_database_url
  psql "${!url_var}" -X -q -v ON_ERROR_STOP=1 \
    -c "update people set member_kind = 'restricted' where email = '$(email_of "$2")'" >/dev/null ||
    fail "$1: could not make $2 a restricted member"
}

# map_repository BUILD REPOSITORY PROJECT maps REPOSITORY to PROJECT, as carol at Settings.
map_repository() {
  send "$1" carol PUT "/api/v1/settings/repo-projects/$2" "$(jq -cn --arg p "$3" '{project: $p}')"
  expect_status 200 "$1: carol maps $2 to $3"
}

begin seed
# PUB first, in both, so every id and event number the twin writes is the one main writes for
# the same object.
for server in main twin; do
  seed_people "$server"
  make_project "$server" PUB
  seed_objects "$server" PUB alice
  map_repository "$server" acme/pub-repo PUB
  mint_token "$server" carol carol-token
  send "$server" carol POST /api/v1/agents/ses-pub-session/messages \
    '{"body":"A direct message to the PUB session.","delivery":"btw"}'
  expect_status 201 "$server: carol messages the PUB session"
  remember "$server" "PUB direct message" "$(jq -r .id "$work/body")"
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
map_repository main acme/priv-repo PRIV
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

begin private
send main carol PATCH /api/v1/projects/PRIV/access '{"visibility":"private"}'
if [ "$status" = 200 ]; then
  priv_private=1
  note "PRIV is private"
else
  priv_private=0
  refusal="PATCH /api/v1/projects/PRIV/access {visibility: private} answered HTTP $status $(head -c 300 "$work/body")"
  if [ "$private_by_sql" = 1 ]; then
    psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PRIV'" >/dev/null ||
      fail "main: the stand-in could not make PRIV private"
    priv_private=1
    unrun "PRIV was made private by the store write, not the route: $refusal"
    note "PRIV is private by the stand-in (--private-by-sql); the route answered HTTP $status"
  else
    unrun "PRIV is not private: $refusal; the walk, search, sockets, proxy and writes sections need it and did not run"
    note "PRIV is not private: HTTP $status"
  fi
fi
pass

begin listed
# Each listed caller reads each object of PRIV: 200, and the answer names the object.
found=0
for caller in "${listed_callers[@]}"; do
  for path in "${object_reads[@]}"; do
    path=${path//\{P\}/PRIV}
    send main "$caller" GET "$(resolve main "$path")"
    if [ "$status" = 200 ] && grep -q "PRIV" "$work/body"; then
      found=$((found + 1))
    else
      printf '%s GET %s: HTTP %s, want 200 naming PRIV: %s\n' "$caller" "$path" "$status" "$(head -c 300 "$work/body")" >>"$work/differences"
      differed=$((differed + 1))
    fi
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

# compare_twin CALLER PATH: as CALLER, main and the twin answer GET PATH the same.
compare_twin() {
  local server
  for server in main twin; do
    send "$server" "$1" GET "$(resolve "$server" "$2")"
    keep_answer "$server" "$server"
  done
  same_answers "$1 GET $2 (beside the twin)" main twin
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
  [ "$keys" = '["PRIV"]' ] || {
    printf '%s GET /api/v1/projects: lists %s, want ["PRIV"]\n' "$caller" "$keys" >>"$work/differences"
    differed=$((differed + 1))
  }
  send main "$caller" GET /api/v1/users
  if grep -q "$alice" "$work/body"; then
    printf '%s GET /api/v1/users: names %s, who is outside PRIV\n' "$caller" "$alice" >>"$work/differences"
    differed=$((differed + 1))
  fi
  send main "$caller" GET /api/v1/agents
  if grep -q ses-pub-session "$work/body"; then
    printf '%s GET /api/v1/agents: lists ses-pub-session, an agent of PUB\n' "$caller" >>"$work/differences"
    differed=$((differed + 1))
  fi
  send main "$caller" GET /api/v1/agents/ses-pub-session/messages
  if grep -q "$(recalled main "PUB direct message")" "$work/body"; then
    printf '%s GET /api/v1/agents/ses-pub-session/messages: holds carol'"'"'s direct message to it\n' "$caller" >>"$work/differences"
    differed=$((differed + 1))
  fi
done
note "bob and carol's PRIV token read PUB's objects, the projects, the people, the agents and a direct message"
pass

# Meaning search answers from vectors the embedding queue writes behind each write, so both
# servers are read only once neither owes a vector.
indexed() {
  local server
  for server in main twin; do
    send "$server" carol GET /api/v1/search/coverage
    [ "$status" = 200 ] && [ "$(jq '[.not_indexed // 0] | flatten | map(if type == "object" then (.count // 0) else . end) | add // 0' "$work/body")" = 0 ] || return 1
  done
}

# walk_rows SELECT walks the rows of the route index that the jq filter SELECT keeps: a row whose
# path names an object is filled from its Probe with PRIV's objects and with missing ones, and
# each unseeing caller is answered the same for both; a GET row that names no object is a list,
# answered as the twin answers it. A row that names an object with no Probe, or one whose object
# the seed does not hold, is written to what did not run. A test hook (RouteProbe's test_hook) is
# left out, named: the server mounts it only under DISPATCH_TEST_HOOKS, which no deployment sets.
# A session and a person are no-project nodes, which spec v34 (Design, "A node of no project")
# shows to an open viewer as it does today, and both servers hold the same ones: a row whose every
# wildcard is one of them is filled alike on both servers and answered as the twin answers it.
walk_rows() {
  local row method pattern wildcards entry hidden none skip wildcard kind object body caller
  walked=0 lists=0
  while IFS= read -r row; do
    method=$(jq -r .method <<<"$row")
    pattern=$(jq -r .path <<<"$row")
    entry=$(jq -c --arg m "$method" --arg p "$pattern" 'map(select(.method == $m and .pattern == $p))[0] // empty' "$work/probes.json")
    if [ "$(jq -r '.test_hook // false' <<<"${entry:-null}")" = true ]; then
      note "left out: $method $pattern is a test hook, which only DISPATCH_TEST_HOOKS mounts"
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
    if [ -z "$entry" ] || [ "$(jq -c '.probe' <<<"$entry")" = null ]; then
      unrun "$method $pattern names an object and has no Probe"
      continue
    fi
    hidden=$pattern
    none=$pattern
    : >"$work/fill-hidden"
    : >"$work/fill-missing"
    skip=''
    for wildcard in $wildcards; do
      kind=$(jq -r --arg w "$wildcard" '.probe[$w] // empty' <<<"$entry")
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
      hidden=${hidden//"{$wildcard}"/$(jq -rn --arg v "$object" '$v|@uri')}
      # A no-project node fills both sides alike: it is no object PRIV wrote.
      if [[ "$no_project_kinds" == *" $kind "* ]]; then
        none=${none//"{$wildcard}"/$(jq -rn --arg v "$object" '$v|@uri')}
        continue
      fi
      none=${none//"{$wildcard}"/$(jq -rn --arg v "${missing[$kind]}" '$v|@uri')}
      # A fill that is the same on both sides is no difference to mask.
      if [ "$object" != "${missing[$kind]}" ]; then
        printf '%s\t%s\n' "$wildcard" "$object" >>"$work/fill-hidden"
        printf '%s\t%s\n' "$wildcard" "${missing[$kind]}" >>"$work/fill-missing"
      fi
    done
    [ -z "$skip" ] || continue
    if [ "$hidden" = "$none" ]; then
      # Every wildcard is a no-project node: the path is the same on both servers.
      [ "$method" = GET ] || { unrun "$method $pattern: a write that names only no-project nodes is not walked"; continue; }
      for caller in "${unseeing_callers[@]}"; do compare_twin "$caller" "$hidden"; done
      lists=$((lists + 1))
      continue
    fi
    body=$(jq -c '.body // empty' <<<"$entry")
    for caller in "${unseeing_callers[@]}"; do
      compare_missing "$caller" "$method" "$pattern" "$hidden" "$none" "$work/fill-hidden" "$work/fill-missing" "$body"
    done
    walked=$((walked + 1))
  done < <(jq -c "$1" "$work/routes")
}

if [ "$priv_private" = 1 ]; then
  begin walk
  # The route index this server serves, and the Probes of its own tables.
  send main carol GET /api/v1
  expect_status 200 "main: carol reads the route index"
  jq -c '.routes[]' "$work/body" >"$work/routes"
  probes_read=0
  if go -C "$root/packages/envoy" run ./internal/dispatch/api/cmd/routeprobes >"$work/probes.json" 2>"$work/probes.err"; then
    probes_read=1
    walk_rows 'select(.method == "GET")'
    note "walked $walked GET rows that name an object and $lists lists, as each unseeing caller"
  else
    unrun "the Probes could not be read, so no row was walked: go run ./internal/dispatch/api/cmd/routeprobes: $(head -c 300 "$work/probes.err")"
  fi
  pass

  begin search
  until_true 120 "both servers to owe no vector" indexed
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
  for caller in "${unseeing_callers[@]}"; do
    for side in hidden missing; do
      id=${missing[document]}
      [ "$side" = missing ] || id=$(recalled main "PRIV spec")
      status=socket
      bun "$(dirname "$0")/socket-access.ts" doc "${build_url[main]}" "$work/auth/main-$caller" "$id" "$schema_version" >"$work/body" 2>&1 || status=error
      keep_answer "$side" main "$work/fill-$side"
    done
    same_answers "$caller /ws/doc/{room}" hidden missing
    for server in main twin; do
      status=socket
      bun "$(dirname "$0")/socket-access.ts" live "${build_url[$server]}" "$work/auth/$server-$caller" >"$work/body" 2>&1 || status=error
      keep_answer "$server" "$server"
    done
    same_answers "$caller /ws/live (beside the twin)" main twin
  done
  # The document socket admits a person by their cookie alone, at the merge base as now, so a
  # token has no document-socket path to any project: bob and carol find PRIV's spec there, and a
  # token that lists PRIV is refused alike for PUB's spec and PRIV's. This is the one path on which
  # "find each object of PRIV" is read as "refused alike" (the integrator's decision of t-a-22).
  for caller in bob carol; do
    answer=$(bun "$(dirname "$0")/socket-access.ts" doc "${build_url[main]}" "$work/auth/main-$caller" "$(recalled main "PRIV spec")" "$schema_version" 2>&1) || true
    [[ "$answer" == authenticated* ]] || {
      printf '%s /ws/doc/{PRIV spec}: %s, want authenticated\n' "$caller" "$answer" >>"$work/differences"
      differed=$((differed + 1))
    }
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
    note "walked $walked write rows that name an object, as each unseeing caller"
  fi
  pass
fi

begin verdict
note "$compared pairs of answers compared; by status: $(statuses)"
if [ -s "$work/unrun" ]; then
  fail "$(wc -l <"$work/unrun") things did not run:
$(<"$work/unrun")
$([ "$differed" -eq 0 ] || printf '%s answers differ:\n%s' "$differed" "$(<"$work/differences")")"
fi
no_difference
pass
