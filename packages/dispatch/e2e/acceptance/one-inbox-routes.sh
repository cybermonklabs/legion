#!/usr/bin/env bash
# Drives dispatch://INFRA-1152's one read, GET /api/v1/me/inbox, and the gate that reads it against
# the three reads it replaces (c-routes-gone), at the last commit at which the Inbox's default still
# names those three reads. It runs against that commit's own `bun run dispatch:local` (DISPATCH_URL)
# and boots the build of BASELINE_SHA beside it, on port 8779 over ISSUE_PREFLIP_DATABASE_URL, the
# database that server serves, so both builds answer with one set of ids. It sends the base build
# reads alone, and stops it before it exits.
#
#   route_scan        the routes this build adds beside the base build's route table: none takes a
#                     node kind, an edge kind or a traversal, the one read's `kind` narrowing aside,
#                     a planted such route is found first, and a `kind` the store holds no tuple of
#                     answers an empty read rather than a wider one.
#   bad_asks          an identity selector, an unknown key, an anonymous request and the agent
#                     bearer, which holds no person scope, each refused by what offends.
#   isolation         two people in turn, neither reading a row of the other's.
#   cross_project     an identifier of another project in each key that takes one - project,
#                     issue, artifact and cursor - reading no row of the project it does not name.
#   traversal         a traversal parameter the route does not declare, refused by its key.
#   horizon           a comment a person writes, read in their inbox once the server's safe
#                     horizon passes that write's event, and no event of the run's corpus at or
#                     below that horizon naming an entry the read does not answer.
#   gate_missing_shape
#                     the comparison over a corpus without a reply chain at the walk's bound: the
#                     shape named, the run refused and the gate not met.
#   gate_mismatch     a row of one person's written as the release before writes it, after the
#                     backfill: the comparison names the person, the row, the arm and the kind,
#                     counts it per person and per arm, and reads the gate not met.
#   stored_row        the comparison's own stored row for that run, holding its difference count,
#                     people checked and uncovered and the shapes it covered, and the gate route
#                     answering those counts and none of the rows.
#   gate_uncovered    a person the comparison is told not to reach, counted uncovered, the gate not
#                     met however clean the others read.
#   gate_met          the mismatch cleared and nobody excluded: the gate met over the corpus, with
#                     every earlier run's row as it was written.
#   old_routes        GET /api/v1/inbox, GET /api/v1/me/replies and GET
#                     /api/v1/agents/{session_id}/messages answering each seeded person on this
#                     build with the shape and the rows the base build answers.
#   default           the Inbox still served by the three reads: no commit of this checkout carries
#                     the flip's trailer, the dashboard this server serves calls the three reads
#                     and draws the comparison page, and the gate route answers.
#   read_then_mention a thread the person wrote in and read, and one they were only mentioned in:
#                     GET /api/v1/me/replies' default unread state answers neither, and the one
#                     read answers both.
#
# The corpus the gate runs over is the server's own seed and what the run writes through the API
# beside it: an open ask that waits on the person, a comment thread, an issue message thread and an
# ask thread they wrote in with somebody else's reply, a direct message to a session with its
# reply, and a person with no rows. The one shape only the release before could write - a comment
# reply chain at the outbox walk's bound, 32 links deep, since POST comments climbs reply_to to the
# thread's head - is written through psql as that release wrote it, and `envoy-dispatch
# backfill-graph-store` then runs over it, as a deployment's backfill runs before anybody reads the
# new read. The mismatch is a thread written the same way after the backfill, as a write an older
# build makes during a roll.
#
# What the store holds and what the release before writes are served by no route, so they are
# read and written through psql on ISSUE_PREFLIP_DATABASE_URL, as dispatch://INFRA-1149's drivers
# read the store.
#
# Inputs: DISPATCH_URL (the server at the commit under test), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE
# (its agent bearer, read here and never printed), PROJECT (a project the server holds),
# ISSUE_PREFLIP_DATABASE_URL (the database it serves, on this machine), BASELINE_SHA (the release
# this branch lands after, which has no default) or BASELINE_BINARY (a prebuilt base build of it,
# which spares the run its build), and optionally DEV_SIGNIN_LOGIN (default alice) and
# DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY (the dev sign-in key the server was started with; unset, the
# fixed `e2e-dev-signin` e2e/run-server.sh gives a one-process server, which the base build must
# share to start beside it). The comparison and the backfill are this checkout's own
# envoy-dispatch, packages/envoy/dispatch-e2e, which e2e/run-server.sh built for the server; a
# missing one is exit 2, as each unmet input is.
#
# Each passing assertion prints `PASS: <section> <what>`; a failing one prints
# `FAIL: <section> <what>: <observed>` and ends its section. Every section runs whatever an earlier
# one found, and the script exits 1 at its end if any failed. A failure while it writes the corpus
# the sections read ends the run there.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: ISSUE_PREFLIP_DATABASE_URL=postgres://user@127.0.0.1:5433/db \
         DISPATCH_URL=http://127.0.0.1:8778 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         {BASELINE_SHA=<commit> | BASELINE_BINARY=<path>} [DEV_SIGNIN_LOGIN=alice] \
         one-inbox-routes.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       ISSUE_PREFLIP_DATABASE_URL must name the database that server serves, on this machine)
USAGE
}

# What lib.sh's functions set for this script, named so that shellcheck sees them assigned.
token="" work="" answer="" status=""
# shellcheck source-path=SCRIPTDIR source=lib.sh disable=SC1091 # followed under shellcheck -x
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT ISSUE_PREFLIP_DATABASE_URL
# The base commit has no default: main's merge base is older than any sibling branch this one is
# stacked on, so a comparison built on it could pass without being able to fail.
if [[ -z "${BASELINE_SHA:-}" && -z "${BASELINE_BINARY:-}" ]]; then
  printf 'BASELINE_SHA is unset: set it to the commit of the release this branch lands after, or set BASELINE_BINARY to a server already built from it\n' >&2
  usage
  exit 2
fi
require_commands base64 curl git go jq psql sed sleep sort tar
read_bearer

# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# shellcheck disable=SC2153 # the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$ISSUE_PREFLIP_DATABASE_URL"
readonly person="${DEV_SIGNIN_LOGIN:-alice}"
readonly other="bob"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
checkout="$(cd -- "$script_dir/../../../.." && pwd)"
readonly checkout
readonly envoy_dispatch="$checkout/packages/envoy/dispatch-e2e"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="one-inbox-routes-$stamp"
readonly actor="{\"kind\": \"session\", \"id\": \"$session\"}"
# A person who writes nothing, and one the comparison is told not to reach.
readonly nobody="nobody-$stamp@example.test"
readonly excluded="excluded-$stamp@example.test"
# The base build's own port and its work directory, which the check names.
readonly base_port=8779
readonly base_url="http://127.0.0.1:$base_port"
# shellcheck disable=SC2031 # lib.sh's `cli` exports its own TMPDIR; this is the run's
readonly base_dir="${TMPDIR:-/tmp}/base-routes"
# The depth of the comment reply chain the gate requires: the outbox walk's bound,
# internal/dispatch/inboxcompare's ChainLinks.
readonly chain_links=32

case "$database_url" in
  *@127.0.0.1:* | *@localhost:* | *@\[::1\]:* | *@127.0.0.1/* | *@localhost/*) ;;
  *)
    printf 'one-inbox-routes.sh: ISSUE_PREFLIP_DATABASE_URL names no loopback host; this driver writes rows and runs only against a test database on this machine\n' >&2
    exit 2
    ;;
esac
[[ -x "$envoy_dispatch" ]] || {
  printf 'one-inbox-routes.sh: %s is missing: it is the envoy-dispatch e2e/run-server.sh builds for the server under test, whose inbox-compare and backfill this run uses\n' "$envoy_dispatch" >&2
  exit 2
}

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

base_pid=""
made_base_dir=""
cleanup() {
  local code=$? waited=0
  if [[ -n "$base_pid" ]]; then
    kill -TERM "$base_pid" 2>/dev/null || true
    while kill -0 "$base_pid" 2>/dev/null && [[ "$waited" -lt 50 ]]; do
      sleep 0.2
      waited=$((waited + 1))
    done
    kill -KILL "$base_pid" 2>/dev/null || true
    wait "$base_pid" 2>/dev/null || true
  fi
  [[ -z "$made_base_dir" ]] || rm -rf "$base_dir"
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# request AS METHOD URL [JSON]: sends METHOD URL as AS: `agent`, with the bearer; `anonymous`, with
# no credential; or a login, with the cookie that login's dev sign-in left in its jar, and on a
# write the dashboard's origin, which the server requires of every write a cookie authenticates.
# URL is a path of the server under test, or a whole URL: both servers sign cookies with one key,
# so the one cookie reads either. The body goes to $work/body and the status to $status.
request() {
  local -a auth data=()
  local address="$3"
  case "$1" in
    agent) auth=(-H "@$work/auth") ;;
    anonymous) auth=() ;;
    *) auth=(-b "$work/jar-$1") ;;
  esac
  [[ "$address" == http* ]] || address="$base$address"
  if [[ $# -ge 4 ]]; then
    data=(-H 'Content-Type: application/json' --data-binary "$4")
    [[ "$1" == agent || "$1" == anonymous ]] || data+=(-H "Origin: $origin")
  fi
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$address" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# sign_in LOGIN: signs LOGIN in through the server's dev sign-in into LOGIN's jar.
sign_in() {
  curl -sS -o /dev/null -c "$work/jar-$1" "$base/auth/_dev/signin?login=$(q "$1")" \
    2>"$work/curl" || fail setup "sign $1 in" "curl: $(<"$work/curl")"
  grep -q dsession "$work/jar-$1" || fail setup "sign $1 in" "the dev sign-in route left no session cookie"
}

# psql_do SECTION WHAT SQL: runs SQL, which answers nothing the run reads.
psql_do() {
  env -u PGHOSTADDR psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c "$3" >/dev/null 2>"$work/psql" ||
    fail "$1" "$2" "psql: $(<"$work/psql")"
}

# post SECTION WHAT AS PATH JSON: writes JSON at PATH as AS and sets $id to the id it answers.
post() {
  request "$3" POST "$4" "$5"
  [[ "$status" == 200 || "$status" == 201 ]] || fail "$1" "$2" "HTTP $status: $(body)"
  id="$(jq -er '.id // .artifact.id' "$work/body")" || fail "$1" "$2" "no id: $(body)"
  pass "$1" "$2 ($id)"
}
# as_session JSON: JSON with this run's session as its actor.
as_session() { jq -cn --argjson f "$1" --argjson a "$actor" '$f + {actor: $a}'; }
# as_agent JSON: JSON with the seeded agent session as its actor.
as_agent() { jq -cn --argjson f "$1" --arg s "$agent" '$f + {actor: {kind: "session", id: $s}}'; }

# one_read LOGIN [QUERY]: reads GET /api/v1/me/inbox as LOGIN into $work/body, wanting 200.
one_read() {
  request "$1" GET "/api/v1/me/inbox${2:+?$2}"
  [[ "$status" == 200 ]] || fail "${section:-read}" "read $1's one read${2:+ ?$2}" "HTTP $status: $(body)"
}
# holds NODE: whether the one read in $work/body holds an entry for NODE (<kind>:<id>).
holds() { jq -e --arg n "$1" 'any(.entries[]; .node.kind + ":" + .node.id == $n)' "$work/body" >/dev/null; }

# compare NAME ARGS...: runs this checkout's inbox-compare with ARGS against the two servers,
# keeping its output in $work/NAME.out and its exit status in $compared.
compared=""
compare() {
  local name="$1"
  shift
  set +e
  # shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH; this is the run's
  env -i PATH="$PATH" HOME="$work" DATABASE_URL="$database_url" \
    "$envoy_dispatch" inbox-compare --old-url "$base_url" --new-url "$base" "$@" >"$work/$name.out" 2>&1
  compared=$?
  set -e
  sed 's/^/  | /' "$work/$name.out"
}
# summary NAME FIELD: the value of FIELD in the summary line of comparison NAME.
summary() { sed -n "s/^inbox-compare: run=.* $2=\\([^ ]*\\).*/\\1/p" "$work/$1.out" | tail -n 1; }
# run_id NAME: the id of the run comparison NAME stored, and nothing where it stored none or never
# ran: an assignment from it under errexit would otherwise end its section without a FAIL line.
run_id() { [[ ! -f "$work/$1.out" ]] || sed -n 's/^inbox-compare: run=\([0-9]*\) .*/\1/p' "$work/$1.out" | tail -n 1; }

# isolated COMMAND...: runs COMMAND in a subshell of its own, with errexit, and sets
# $isolated_status to its exit status, so a `fail` inside ends COMMAND alone.
isolated() {
  set +e
  (
    set -e
    "$@"
  )
  isolated_status=$?
  set -e
}

# --- setup: the people, the agent, the base build ---------------------------------------------

for login in "$person" "$other" "$nobody" "$excluded"; do
  sign_in "$login"
done
pass setup "signed $person, $other, $nobody and $excluded in"

# The agent: a session the server lists that claims no role, which takes a direct message.
request agent GET /api/v1/agents
[[ "$status" == 200 ]] || fail setup "list the agents" "HTTP $status: $(body)"
agent="$(jq -r '[(if type == "array" then . else .agents end)[] | select(((.roles // []) | length) == 0) | .session_id][0] // empty' "$work/body")"
[[ -n "$agent" ]] || fail setup "find an agent that claims no role" "$(body)"
readonly agent
pass setup "the agent is $agent"

# The base build: BASELINE_BINARY, or the commit's tree taken out with git archive into
# $TMPDIR/base-routes and built with the stamp the image build gives a server.
if [[ -n "${BASELINE_BINARY:-}" ]]; then
  base_binary="$BASELINE_BINARY"
  [[ -x "$base_binary" ]] || fail setup "boot the base build" "BASELINE_BINARY $base_binary is no executable file"
  base_commit="(prebuilt $base_binary)"
else
  sha="$BASELINE_SHA"
  base_commit="$(git -C "$checkout" rev-parse --verify --quiet "${sha}^{commit}")" ||
    fail setup "find the base commit" "BASELINE_SHA $sha names no commit"
  [[ ! -e "$base_dir" ]] || fail setup "build the base" "$base_dir exists already; another run may still read it"
  mkdir -p "$base_dir/tree" || fail setup "build the base" "could not make $base_dir"
  made_base_dir=1
  git -C "$checkout" archive "$base_commit" | tar -x -C "$base_dir/tree" ||
    fail setup "build the base" "could not take the tree of $base_commit"
  go -C "$base_dir/tree/packages/envoy" build -ldflags "-X main.buildCommit=$base_commit" \
    -o "$base_dir/dispatch" ./cmd/dispatch >"$work/base-build.log" 2>&1 ||
    fail setup "build the base" "$(tail -n 20 "$work/base-build.log")"
  rm -rf "$base_dir/tree"
  base_binary="$base_dir/dispatch"
fi
readonly base_binary base_commit
mkdir -p "$work/base-home/web-dist"
# The base serves reads alone. It inherits nothing of this shell's but PATH; its cookie key is the
# server's, since a server process refuses to start beside a live one whose keys it does not hold;
# and its Envoy is port 1, which refuses every delivery at once, as lib-baseline.sh gives its own.
# shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH; this is the run's
env -i PATH="$PATH" HOME=/nonexistent XDG_CACHE_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent TMPDIR="$work/base-home" DATABASE_URL="$database_url" \
  DISPATCH_AGENT_TOKEN="$token" DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_DEV_SIGNIN_SHARED_KEY="${DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY:-e2e-dev-signin}" \
  DISPATCH_IDENTITY=cookie DISPATCH_INSECURE_COOKIE=1 DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_NATS_DISABLED=1 DISPATCH_PORT="$base_port" DISPATCH_SERVER_URL="$base_url" \
  DISPATCH_WEB_DIST="$work/base-home/web-dist" ENVOY_URL=http://127.0.0.1:1 \
  "$base_binary" >"$work/base-server.log" 2>&1 &
base_pid=$!
waited=0
until curl -sS -o /dev/null -w '%{http_code}' "$base_url/healthz" 2>/dev/null | grep -q '^200$'; do
  kill -0 "$base_pid" 2>/dev/null || fail setup "boot the base build on $base_port" "it exited: $(tail -n 20 "$work/base-server.log")"
  [[ "$waited" -lt 360 ]] || fail setup "boot the base build on $base_port" "no /healthz 200 in 180 s: $(tail -n 20 "$work/base-server.log")"
  sleep 0.5
  waited=$((waited + 1))
done
pass setup "the base build of $base_commit serves on $base_url over the server's database"

# --- the corpus, written through the API ------------------------------------------------------

# create_issue TITLE: an issue in PROJECT the person is assigned, made as this run's session; sets $key.
create_issue() {
  request agent POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$1" --arg w "$person" --argjson a "$actor" \
    '{project: $p, title: $t, spec: "The first line of the spec.", assignee: $w, force: true, actor: $a}')"
  [[ "$status" == 201 ]] || fail setup "create \"$1\"" "HTTP $status: $(body)"
  key="$(jq -er '.key' "$work/body")" || fail setup "create \"$1\"" "no key: $(body)"
  pass setup "created $key in $project, assigned to $person"
}
create_issue "The release the one read is compared over, $stamp"
readonly issue="$key"

post seed "an open ask that waits on $person" agent "/api/v1/issues/$issue/asks" \
  "$(as_session '{"question": "Approve the release window?"}')"
pass seed "the open ask $id waits on $person, $issue's assignee"
post seed "$person's comment thread root" "$person" "/api/v1/issues/$issue/comments" '{"body": "Is the checklist complete?"}'
readonly comment_root="$id"
post seed "the session's reply in it" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg r "$comment_root" '{body: "Two items are still open.", reply_to: $r}')")"
post seed "$person's issue message thread root" "$person" "/api/v1/issues/$issue/messages" '{"body": "Who owns the rollback step?"}'
readonly message_root="$id"
post seed "$other's reply in it" "$other" "/api/v1/issues/$issue/messages" \
  "$(jq -cn --arg r "$message_root" '{body: "I own it.", in_reply_to: $r}')"
post seed "an ask the session asks $person" agent "/api/v1/issues/$issue/asks" \
  "$(as_session '{"question": "Ship on Friday?", "options": [{"label": "Ship"}, {"label": "Hold"}]}')"
readonly answered_ask="$id"
request "$person" POST "/api/v1/asks/$answered_ask/answer" '{"selected": ["Ship"], "expected_edited_at": null}'
[[ "$status" == 200 || "$status" == 201 ]] || fail seed "$person answers $answered_ask" "HTTP $status: $(body)"
pass seed "$person answers $answered_ask"
post seed "the session's reply in the answered ask" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg k "$answered_ask" '{body: "Friday it is.", ask_id: $k}')")"
post seed "$person's direct message to $agent" "$person" "/api/v1/agents/$agent/messages" \
  '{"body": "Please check the staging gate.", "delivery": "btw"}'
readonly direct="$id"
post seed "$agent's reply to it" agent "/api/v1/messages/$direct/reply" \
  "$(as_agent '{"attempt": 1, "body": "The gate is open."}')"

# --- route_scan --------------------------------------------------------------------------------

# added_routes INDEX BASE_INDEX: the "METHOD PATH" lines of INDEX the base's index does not hold.
added_routes() {
  comm -23 <(jq -r '.routes[] | "\(.method) \(.path)"' "$1" | LC_ALL=C sort -u) \
    <(jq -r '.routes[] | "\(.method) \(.path)"' "$2" | LC_ALL=C sort -u)
}
# graph_parameters OPENAPI ROUTES: for each "METHOD PATH" of ROUTES, each parameter of that operation
# in the OpenAPI document whose name says it takes a node kind, an edge kind or a traversal, as
# "METHOD PATH name", the one read's `kind` narrowing aside, which only narrows.
readonly graph_words='(^|_)(kinds?|nodes?|edges?|relations?|tuples?|travers(e|al)|depth|hops?|follow|directions?|walk)($|_)'
graph_parameters() {
  local method path
  while read -r method path; do
    [[ "$path" != *"{kind}"* ]] || printf '%s %s {kind}\n' "$method" "$path"
    jq -r --arg p "$path" --arg m "${method,,}" --arg w "$graph_words" '
      (.paths[$p][$m].parameters // [])[] | .name | select(test($w))' "$1" |
      while read -r name; do
        [[ "$method $path $name" == "GET /api/v1/me/inbox kind" ]] || printf '%s %s %s\n' "$method" "$path" "$name"
      done
  done <"$2"
}

route_scan_section() {
  local section=route_scan
  "$envoy_dispatch" routes >"$work/routes-head.json" 2>"$work/err" || fail "$section" "read this build's route table" "$(<"$work/err")"
  "$base_binary" routes >"$work/routes-base.json" 2>"$work/err" || fail "$section" "read the base build's route table" "$(<"$work/err")"
  "$envoy_dispatch" openapi >"$work/openapi-head.json" 2>"$work/err" || fail "$section" "read this build's OpenAPI document" "$(<"$work/err")"
  added_routes "$work/routes-head.json" "$work/routes-base.json" >"$work/added"
  grep -qx 'GET /api/v1/me/inbox' "$work/added" ||
    fail "$section" "the scan reads the routes this branch adds" "GET /api/v1/me/inbox is not among them: $(paste -sd, "$work/added")"
  pass "$section" "this build adds $(wc -l <"$work/added") routes beside the base build's, GET /api/v1/me/inbox among them"
  # The control: a planted route taking an edge kind is found, so a scan that finds nothing over
  # the real routes is read from an instrument that finds one.
  jq '.paths["/api/v1/planted/{kind}"] = {get: {parameters: [{name: "edge_kind", in: "query"}]}}' \
    "$work/openapi-head.json" >"$work/openapi-planted.json"
  printf 'GET /api/v1/planted/{kind}\n' >"$work/planted"
  graph_parameters "$work/openapi-planted.json" "$work/planted" >"$work/planted-found"
  [[ "$(paste -sd, "$work/planted-found")" == "GET /api/v1/planted/{kind} {kind},GET /api/v1/planted/{kind} edge_kind" ]] ||
    fail "$section" "the scan finds a planted route that takes a node kind and an edge kind" "found: $(paste -sd, "$work/planted-found")"
  pass "$section" "the scan finds a planted route's {kind} and edge_kind"
  graph_parameters "$work/openapi-head.json" "$work/added" >"$work/found"
  [[ ! -s "$work/found" ]] ||
    fail "$section" "no route this branch adds takes a node kind, an edge kind or a traversal" "$(paste -sd, "$work/found")"
  pass "$section" "no route this branch adds takes a node kind, an edge kind or a traversal, the one read's kind narrowing aside"
  one_read "$person" "kind=no_such_kind_${stamp//-/_}"
  [[ "$(jq -c '[.total, (.entries | length)]' "$work/body")" == "[0,0]" ]] ||
    fail "$section" "a kind the store holds no tuple of answers an empty read" "$(body)"
  pass "$section" "a kind the store holds no tuple of answers an empty read, not a wider one"
}

# --- bad_asks ----------------------------------------------------------------------------------

# refused SECTION WHAT STATUS CODE [NAMED]: the last answer is STATUS with error code CODE, its
# message naming NAMED when given.
refused() {
  [[ "$status" == "$3" ]] || fail "$1" "$2" "HTTP $status, want $3: $(body)"
  [[ "$(jq -r '.code // empty' "$work/body")" == "$4" ]] || fail "$1" "$2" "code is not $4: $(body)"
  [[ -z "${5:-}" ]] || jq -e --arg k "$5" '.error | contains($k)' "$work/body" >/dev/null ||
    fail "$1" "$2" "the refusal does not name $5: $(body)"
  pass "$1" "$2"
}

bad_asks_section() {
  local section=bad_asks
  request "$person" GET "/api/v1/me/inbox?assignee=$other"
  refused "$section" "an identity selector is refused by its key" 400 INVALID_QUERY assignee
  request "$person" GET "/api/v1/me/inbox?frobnicate=1"
  refused "$section" "an unknown key is refused by its key" 400 INVALID_QUERY frobnicate
  request anonymous GET /api/v1/me/inbox
  [[ "$status" == 401 ]] || fail "$section" "an anonymous request is refused" "HTTP $status: $(body)"
  pass "$section" "an anonymous request is refused, 401"
  request agent GET /api/v1/me/inbox
  refused "$section" "the agent bearer, which holds no person scope, is refused for the scope" 403 OAUTH_SCOPE_REQUIRED
  # The route's own scope, not the handler behind it, is what refuses the bearer: the route index
  # the server serves names the person scope on the row, and a bearer asking with a key the
  # handler would refuse is refused for the scope before the handler reads its query.
  request anonymous GET /api/v1
  [[ "$status" == 200 ]] || fail "$section" "read the server's route index" "HTTP $status: $(body)"
  jq -e 'any(.routes[]; .method == "GET" and .path == "/api/v1/me/inbox" and ((.scopes // []) | index("person")))' \
    "$work/body" >/dev/null ||
    fail "$section" "the route index names the person scope on GET /api/v1/me/inbox" \
      "$(jq -c '[.routes[] | select(.path == "/api/v1/me/inbox")]' "$work/body")"
  pass "$section" "the route index names the person scope on GET /api/v1/me/inbox"
  request agent GET "/api/v1/me/inbox?frobnicate=1"
  refused "$section" "the agent bearer is refused for the scope before the route reads its query" 403 OAUTH_SCOPE_REQUIRED
}

# --- isolation ---------------------------------------------------------------------------------

isolation_section() {
  local section=isolation mine theirs
  post "$section" "a comment of $person's" "$person" "/api/v1/issues/$issue/comments" '{"body": "Mine alone."}'
  mine="comment:$id"
  post "$section" "a comment of $other's" "$other" "/api/v1/issues/$issue/comments" '{"body": "Theirs alone."}'
  theirs="comment:$id"
  one_read "$person"
  holds "$mine" || fail "$section" "$person reads their own comment" "no entry $mine"
  ! holds "$theirs" || fail "$section" "$person reads no row of $other's" "$theirs is an entry of $person's read"
  [[ "$(jq -r '.person' "$work/body")" == "$person" ]] || fail "$section" "the read answers for $person" "$(jq -c '.person' "$work/body")"
  pass "$section" "$person reads their own comment and not $other's"
  one_read "$other"
  holds "$theirs" || fail "$section" "$other reads their own comment" "no entry $theirs"
  ! holds "$mine" || fail "$section" "$other reads no row of $person's" "$mine is an entry of $other's read"
  pass "$section" "$other reads their own comment and not $person's"
}

# --- cross_project -----------------------------------------------------------------------------

# foreign_entries PROJECT: how many entries of the read in $work/body the store holds in PROJECT.
foreign_entries() {
  local list
  list="$(jq -r '[.entries[] | "(\u0027" + .node.kind + "\u0027, \u0027" + .node.id + "\u0027)"] | join(", ")' "$work/body")"
  if [[ -z "$list" ]]; then
    answer=0
    return
  fi
  query cross_project "read the projects of the entries" "
    select count(*) from graph_nodes where (kind, id) in ($list) and project_key = '$1'"
}

cross_project_section() {
  local section=cross_project other_project="X${stamp//-/}" other_issue other_document other_comment core_comment cursor
  other_project="${other_project:0:10}"
  request "$person" POST /api/v1/projects "$(jq -cn --arg k "$other_project" '{key: $k, name: "The other project"}')"
  [[ "$status" == 201 ]] || fail "$section" "create the project $other_project" "HTTP $status: $(body)"
  request agent POST /api/v1/issues "$(jq -cn --arg p "$other_project" --argjson a "$actor" \
    '{project: $p, title: "An issue of the other project", spec: "One line.", force: true, actor: $a}')"
  [[ "$status" == 201 ]] || fail "$section" "create an issue in $other_project" "HTTP $status: $(body)"
  other_issue="$(jq -er '.key' "$work/body")"
  post "$section" "$person's comment in $other_project" "$person" "/api/v1/issues/$other_issue/comments" '{"body": "Over there."}'
  other_comment="comment:$id"
  post "$section" "a document of $other_project's" "$person" "/api/v1/projects/$other_project/artifacts" \
    '{"name": "Other notes", "content": "# Other notes\n\nOne line.\n"}'
  other_document="$id"
  post "$section" "$person's comment in $project" "$person" "/api/v1/issues/$issue/comments" '{"body": "Over here."}'
  core_comment="comment:$id"
  one_read "$person"
  if ! holds "$other_comment" || ! holds "$core_comment"; then
    fail "$section" "$person's unnarrowed read holds both comments" "$(jq -c '[.entries[].node]' "$work/body")"
  fi
  pass "$section" "$person's unnarrowed read holds a row of each project"

  one_read "$person" "project=$other_project"
  foreign_entries "$project"
  if [[ "$answer" != 0 ]] || holds "$core_comment"; then
    fail "$section" "project=$other_project reads no row of $project" "$answer entries of $project"
  fi
  pass "$section" "project=$other_project reads no row of $project"
  one_read "$person" "project=$project&issue=$other_issue"
  foreign_entries "$other_project"
  [[ "$answer" == 0 ]] || fail "$section" "issue=$other_issue under project=$project reads no row of $other_project" "$answer entries"
  pass "$section" "issue=$other_issue under project=$project reads no row of $other_project"
  one_read "$person" "project=$project&artifact=$other_document"
  foreign_entries "$other_project"
  [[ "$answer" == 0 ]] || fail "$section" "artifact=$other_document under project=$project reads no row of $other_project" "$answer entries"
  pass "$section" "artifact=$other_document under project=$project reads no row of $other_project"
  cursor="$(printf '%s\n%s\n%s' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ -d '+1 day')" comment "${other_comment#comment:}" |
    base64 -w0 | tr '+/' '-_' | tr -d '=')"
  one_read "$person" "project=$project&cursor=$cursor"
  foreign_entries "$other_project"
  if [[ "$answer" != 0 ]] || holds "$other_comment"; then
    fail "$section" "a cursor naming $other_project's comment under project=$project reads no row of $other_project" "$answer entries"
  fi
  pass "$section" "a cursor naming $other_project's comment under project=$project reads no row of $other_project"
}

# --- traversal ---------------------------------------------------------------------------------

traversal_section() {
  local section=traversal
  request "$person" GET "/api/v1/me/inbox?traverse=part_of"
  refused "$section" "a traversal parameter the route does not declare is refused by its key" 400 INVALID_QUERY traverse
  request "$person" GET "/api/v1/me/inbox?depth=2"
  refused "$section" "a depth the route does not declare is refused by its key" 400 INVALID_QUERY depth
}

# --- horizon -----------------------------------------------------------------------------------

horizon_section() {
  local section=horizon comment event waited=0 horizon missing
  post "$section" "$person writes a comment under their own credential" "$person" "/api/v1/issues/$issue/comments" \
    '{"body": "Read me at the horizon."}'
  comment="$id"
  query "$section" "read the comment's event" \
    "select coalesce(max(id)::text, '') from events where type = 'comment.created' and payload->>'id' = '$comment'"
  event="$answer"
  [[ -n "$event" ]] || fail "$section" "read the comment's event" "no comment.created event names $comment"
  while true; do
    one_read "$person"
    horizon="$(jq -r '.horizon' "$work/body")"
    [[ "$horizon" -lt "$event" ]] || break
    [[ "$waited" -lt 60 ]] || fail "$section" "the safe horizon passes event $event" "it reads $horizon after 30 s"
    sleep 0.5
    waited=$((waited + 1))
  done
  pass "$section" "the safe horizon $horizon has passed the comment's event $event"
  holds "comment:$comment" || fail "$section" "the comment is in $person's inbox at that horizon" "no entry comment:$comment"
  pass "$section" "the comment is in $person's inbox at horizon $horizon"
  query "$section" "read the events of the run's corpus at or below the horizon" "
    select coalesce(string_agg(distinct payload->>'id', ' '), '') from events
    where id <= $horizon and type = 'comment.created' and actor->>'id' = '$person'
      and issue_key = '$issue'"
  missing=""
  for id in $answer; do
    holds "comment:$id" || missing+=" comment:$id"
  done
  [[ -z "$missing" ]] || fail "$section" "no event at or below the horizon names an entry the read does not answer" "missing:$missing"
  pass "$section" "every comment.created event of $person's on $issue at or below horizon $horizon names an entry the read answers"
}

# --- the gate ----------------------------------------------------------------------------------

# run_rows: how many run rows the comparison has stored.
run_rows() { query "$1" "count the comparison's run rows" "select count(*) from inbox_comparison_runs"; }

gate_missing_shape_section() {
  local section=gate_missing_shape before
  run_rows "$section"
  before="$answer"
  compare missing-shape
  [[ "$compared" == 1 ]] || fail "$section" "the comparison exits 1 over a corpus without a reply chain at the bound" "exit $compared"
  grep -q '^FAIL: shape reply_chain_at_bound: the corpus holds none of it' "$work/missing-shape.out" ||
    fail "$section" "the comparison names the missing shape" "no FAIL line for reply_chain_at_bound"
  pass "$section" "the comparison names reply_chain_at_bound as the shape the corpus lacks"
  grep -q '^FAIL: comparison refused: the corpus lacks reply_chain_at_bound' "$work/missing-shape.out" ||
    fail "$section" "the comparison refuses the run" "no refusal line"
  ! grep -q '^PASS: differences' "$work/missing-shape.out" ||
    fail "$section" "the refused run reads no count as a pass" "$(grep '^PASS: differences' "$work/missing-shape.out")"
  pass "$section" "the comparison refuses the run and reads no count of it as a pass"
  [[ "$(summary missing-shape gate)" == not-met ]] || fail "$section" "the gate is not met" "gate=$(summary missing-shape gate)"
  pass "$section" "the gate is not met"
  run_rows "$section"
  [[ "$answer" == $((before + 1)) ]] || fail "$section" "the refused run stores its row" "$before rows before, $answer after"
  query "$section" "read the refused run's shapes" "select shapes->>'reply_chain_at_bound' from inbox_comparison_runs where id = $(run_id missing-shape)"
  [[ "$answer" == 0 ]] || fail "$section" "the refused run's row holds the shape at none" "reply_chain_at_bound=$answer"
  pass "$section" "the refused run's row holds reply_chain_at_bound at 0, so the thin corpus stays readable"
}

# The chain, as the release before writes it, then the backfill over it.
seed_the_chain() {
  psql_do seed "write a reply chain $chain_links links deep as the release before writes it" "
    do \$\$
    declare
      parent uuid;
      link uuid;
    begin
      for depth in 0..$chain_links loop
        insert into comments (issue_key, author, body, reply_to, created_at)
        values ('$issue',
                case when depth % 2 = 0 then jsonb_build_object('kind', 'user', 'id', '$person')
                     else jsonb_build_object('kind', 'session', 'id', '$agent') end,
                'A link of the chain, ' || depth || ' deep.', parent,
                now() - interval '1 hour' + depth * interval '1 second')
        returning id into link;
        parent := link;
      end loop;
    end \$\$"
  pass seed "wrote a reply chain $chain_links links deep on $issue as the release before writes it"
  # shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH; this is the run's
  env -i PATH="$PATH" HOME="$work" DATABASE_URL="$database_url" "$envoy_dispatch" backfill-graph-store \
    >"$work/backfill.out" 2>&1 || fail seed "backfill-graph-store runs over the chain" "$(tail -n 8 "$work/backfill.out")"
  pass seed "backfill-graph-store ran over the chain"
}

# The mismatch: a thread of the person's as the release before writes it, after the backfill.
plant_mismatch() {
  query seed "write $person's thread as the release before writes it" "
    insert into comments (issue_key, author, body, created_at)
    values ('$issue', jsonb_build_object('kind', 'user', 'id', '$person'), 'Written by the release before.', now())
    returning id"
  mismatch_root="$answer"
  query seed "write $other's reply in it as the release before writes it" "
    insert into comments (issue_key, author, body, reply_to, created_at)
    values ('$issue', jsonb_build_object('kind', 'user', 'id', '$other'), 'Answered by the release before.',
            '$mismatch_root', now() + interval '1 second')
    returning id"
  mismatch_reply="$answer"
  pass seed "planted $person's thread comment:$mismatch_root as the release before writes it"
}

gate_mismatch_section() {
  local section=gate_mismatch arms
  compare mismatch --exclude "$excluded"
  [[ "$compared" == 1 ]] || fail "$section" "the comparison exits 1 over the planted mismatch" "exit $compared"
  grep -q "^FAIL: difference person=$person arm=replies kind=thread_reply row=reply:$mismatch_root node=comment:$mismatch_root: " "$work/mismatch.out" ||
    fail "$section" "the comparison names the person, the row, the arm and the kind of the mismatch" "no such difference line"
  pass "$section" "the comparison names $person, row reply:$mismatch_root, arm replies, kind thread_reply"
  grep -q "^FAIL: arm person=$person arm=replies: [1-9]" "$work/mismatch.out" ||
    fail "$section" "the count is $person's in the replies arm" "$(grep "arm person=$person arm=replies" "$work/mismatch.out")"
  grep -q "^FAIL: kind thread_reply: [1-9]" "$work/mismatch.out" || fail "$section" "the count is under thread_reply" "no such line"
  pass "$section" "the difference is counted under $person, the replies arm and thread_reply"
  [[ "$(summary mismatch people_checked)" =~ ^[0-9]+$ && "$(summary mismatch people_uncovered)" == 1 ]] ||
    fail "$section" "the comparison reads the people checked and the one uncovered" "$(tail -n 1 "$work/mismatch.out")"
  pass "$section" "the comparison checked $(summary mismatch people_checked) people and reads 1 uncovered"
  [[ "$(summary mismatch gate)" == not-met ]] || fail "$section" "the gate is not met" "gate=$(summary mismatch gate)"
  pass "$section" "the gate is not met while the Inbox is still served by the reads it replaces"
  # Per person and per arm, never a total: the person's one read answers more entries than their old
  # rows, and the arms that agree cancel none of the difference.
  arms="$(grep -c "^PASS: arm person=$person " "$work/mismatch.out" || true)"
  [[ "$arms" -ge 1 ]] || fail "$section" "an arm that agrees is counted on its own" "no passing arm of $person's"
  [[ "$(summary mismatch difference_count)" == "$(grep -c '^FAIL: difference ' "$work/mismatch.out")" ]] ||
    fail "$section" "the difference count is the differences named" "$(summary mismatch difference_count) counted, $(grep -c '^FAIL: difference ' "$work/mismatch.out") named"
  pass "$section" "$arms arms of $person's agree and the replies arm still counts its difference: per person and per arm"
}

stored_row_section() {
  local section=stored_row run
  run="$(run_id mismatch)"
  [[ -n "$run" ]] || fail "$section" "read the mismatch run's id" "the comparison printed none"
  query "$section" "read run $run's stored row" "
    select difference_count || ' ' || people_checked || ' ' || people_uncovered || ' '
           || jsonb_array_length(differences) || ' ' || (select count(*) from jsonb_object_keys(shapes))
    from inbox_comparison_runs where id = $run"
  [[ "$answer" == "$(summary mismatch difference_count) $(summary mismatch people_checked) $(summary mismatch people_uncovered) $(summary mismatch difference_count) 9" ]] ||
    fail "$section" "the stored row holds the run's counts and its nine shapes" "row reads '$answer'"
  pass "$section" "run $run's row holds its difference count, people checked and uncovered, and the nine shapes it covered"
  query "$section" "read the row's mismatch" "
    select count(*) from inbox_comparison_runs, jsonb_array_elements(differences) d
    where id = $run and d->>'person' = '$person' and d->>'arm' = 'replies' and d->>'row' = 'reply:$mismatch_root'"
  [[ "$answer" == 1 ]] || fail "$section" "the row names the mismatch's person, arm and row" "$answer such differences"
  pass "$section" "the row names the mismatch's person, arm and row"
  request "$person" GET /api/v1/inbox/comparison
  [[ "$status" == 200 ]] || fail "$section" "the gate route answers" "HTTP $status: $(body)"
  [[ "$(jq -c '[.run.id, .run.difference_count, .run.people_checked, .run.people_uncovered, .run.met]' "$work/body")" == \
    "[$run,$(summary mismatch difference_count),$(summary mismatch people_checked),1,false]" ]] ||
    fail "$section" "the gate route answers the newest run's counts" "$(body)"
  ! jq -e '[.. | objects | select(has("differences") or has("person"))] | length > 0' "$work/body" >/dev/null ||
    fail "$section" "the gate route answers nobody's rows" "$(body)"
  pass "$section" "the gate route answers run $run's counts, met false, and none of its rows"
}

gate_uncovered_section() {
  local section=gate_uncovered
  compare uncovered --exclude "$excluded"
  grep -q "^FAIL: uncovered person=$excluded: the run did not reach them: excluded by --exclude" "$work/uncovered.out" ||
    fail "$section" "the excluded person is named uncovered" "no such line"
  [[ "$(summary uncovered people_uncovered)" == 1 && "$(summary uncovered gate)" == not-met && "$compared" == 1 ]] ||
    fail "$section" "one person uncovered leaves the gate unmet" "$(tail -n 1 "$work/uncovered.out")"
  pass "$section" "$excluded reads uncovered and the gate stays unmet, with $(summary uncovered difference_count) differences among the rest"
}

gate_met_section() {
  local section=gate_met earlier="" mismatch
  # The clean run's gate is judged whether or not the mismatch run stored a row: a missing row
  # fails the one check that reads it, after the gate's own.
  mismatch="$(run_id mismatch)"
  if [[ -n "$mismatch" ]]; then
    query "$section" "read the mismatch run's row before the clean run" "
      select difference_count || ' ' || md5(differences::text) from inbox_comparison_runs where id = $mismatch"
    earlier="$answer"
  fi
  compare clean
  [[ "$compared" == 0 && "$(summary clean gate)" == met ]] ||
    fail "$section" "the gate is met over the corpus with nobody excluded and the mismatch cleared" \
      "exit $compared: $(grep -E '^FAIL: ' "$work/clean.out" | paste -sd '|' -)"
  [[ "$(summary clean difference_count) $(summary clean people_uncovered) $(summary clean missing_shapes)" == "0 0 none" ]] ||
    fail "$section" "the clean run reads 0 differences, 0 uncovered and every shape" "$(tail -n 1 "$work/clean.out")"
  pass "$section" "the gate is met: 0 differences, 0 of $(summary clean people_checked) people uncovered, every shape held"
  [[ -n "$mismatch" ]] ||
    fail "$section" "the clean run leaves the earlier mismatch's row as it was" "the mismatch run stored no row: $work/mismatch.out names no run="
  query "$section" "read the mismatch run's row after the clean run" "
    select difference_count || ' ' || md5(differences::text) from inbox_comparison_runs where id = $mismatch"
  [[ "$answer" == "$earlier" ]] || fail "$section" "the clean run leaves the earlier mismatch's row as it was" "'$earlier' then '$answer'"
  pass "$section" "the earlier mismatch's row stands as it was written"
}

# --- old_routes --------------------------------------------------------------------------------

# shape FILE: the key paths of FILE's JSON, each array index written 0, sorted and unique.
shape() { jq -c '[paths | map(if type == "number" then 0 else . end)] | unique' "$1"; }

old_routes_section() {
  local section=old_routes login route ids
  for login in "$person" "$other" "$nobody"; do
    for route in "/api/v1/inbox|[.[].id]" "/api/v1/inbox?assignee=me|[.[].id]" "/api/v1/me/replies|[.rows[].thread_id]" \
      "/api/v1/me/replies?kind=reply,agent&state=inbox|[.rows[].thread_id]" "/api/v1/agents/$agent/messages|[.[].message.id]"; do
      ids="${route#*|}"
      route="${route%%|*}"
      request "$login" GET "$route"
      [[ "$status" == 200 ]] || fail "$section" "GET $route answers $login on this build" "HTTP $status: $(body)"
      cp "$work/body" "$work/head-answer"
      request "$login" GET "$base_url$route"
      [[ "$status" == 200 ]] || fail "$section" "GET $route answers $login on the base build" "HTTP $status: $(body)"
      [[ "$(shape "$work/head-answer")" == "$(shape "$work/body")" ]] ||
        fail "$section" "GET $route answers $login in the base build's shape" \
          "this build $(shape "$work/head-answer"), the base $(shape "$work/body")"
      [[ "$(jq -c "$ids" "$work/head-answer")" == "$(jq -c "$ids" "$work/body")" ]] ||
        fail "$section" "GET $route answers $login the base build's rows" \
          "this build $(jq -c "$ids" "$work/head-answer"), the base $(jq -c "$ids" "$work/body")"
      pass "$section" "GET $route answers $login with the base build's shape and rows"
    done
  done
}

# --- default -----------------------------------------------------------------------------------

default_section() {
  local section=default flip assets asset
  flip="$(git -C "$checkout" log -1 --format=%H --grep='^Inbox-Default: one-read$' HEAD)" ||
    fail "$section" "read this checkout's history" "git log failed"
  [[ -z "$flip" ]] || fail "$section" "no commit of this checkout flips the default" "$flip carries Inbox-Default: one-read"
  pass "$section" "no commit of this checkout carries Inbox-Default: one-read: the Inbox's default is the three reads"
  request anonymous GET /
  [[ "$status" == 200 ]] || fail "$section" "read the dashboard the server serves" "HTTP $status"
  assets="$(grep -o '/assets/[^"]*\.js' "$work/body" | LC_ALL=C sort -u)" ||
    fail "$section" "read the dashboard's scripts" "index.html names none"
  [[ -n "$assets" ]] || fail "$section" "read the dashboard's scripts" "index.html names none"
  : >"$work/bundle.js"
  for asset in $assets; do
    curl -sS "$base$asset" >>"$work/bundle.js" 2>"$work/curl" || fail "$section" "read $asset" "$(<"$work/curl")"
  done
  # The entry imports its lazy chunks by their names, which index.html does not list.
  grep -o 'assets/[A-Za-z0-9_.-]*\.js' "$work/bundle.js" >"$work/chunk-names" ||
    fail "$section" "read the dashboard's lazy chunks" "its scripts name none"
  LC_ALL=C sort -u "$work/chunk-names" >"$work/chunks"
  while read -r asset; do
    curl -sS "$base/$asset" >>"$work/bundle.js" 2>"$work/curl" || fail "$section" "read /$asset" "$(<"$work/curl")"
  done <"$work/chunks"
  for path in "/api/v1/inbox" "/api/v1/me/replies" "/api/v1/agents/{session_id}/messages" "/inbox/comparison" \
    "/api/v1/inbox/comparison"; do
    grep -qF "\"$path\"" "$work/bundle.js" || fail "$section" "the dashboard names $path" "no \"$path\" in its scripts"
  done
  pass "$section" "the dashboard the server serves calls the three reads and draws the comparison page"
  request "$person" GET /api/v1/inbox/comparison
  [[ "$status" == 200 ]] || fail "$section" "the gate route answers" "HTTP $status: $(body)"
  pass "$section" "the gate route the page reads answers"
}

# --- read_then_mention -------------------------------------------------------------------------

read_then_mention_section() {
  local section=read_then_mention read mention replied
  post "$section" "$person writes a thread" "$person" "/api/v1/issues/$issue/comments" '{"body": "Are the notes final?"}'
  read="$id"
  post "$section" "$other replies in it" "$other" "/api/v1/issues/$issue/comments" \
    "$(jq -cn --arg r "$read" '{body: "They are.", reply_to: $r}')"
  # Read through the reply's own moment, which the answer to its write carries: a mark taken from
  # the clock, at a second's precision, can fall a fraction of a second before the reply.
  replied="$(jq -er '.created_at' "$work/body")" || fail "$section" "read the reply's moment" "$(body)"
  request "$person" PUT "/api/v1/me/threads/$read/read" "$(jq -cn --arg t "$replied" '{read_through: $t}')"
  [[ "$status" == 204 ]] || fail "$section" "$person reads the thread" "HTTP $status: $(body)"
  pass "$section" "$person read the thread through its reply's moment, $replied"
  post "$section" "$other mentions $person in a thread of their own" "$other" "/api/v1/issues/$issue/comments" \
    "$(jq -cn --arg p "$person" '{body: ("person:" + $p + ", please read this.")}')"
  mention="$id"
  request "$person" GET /api/v1/me/replies
  [[ "$status" == 200 ]] || fail "$section" "read GET /api/v1/me/replies" "HTTP $status: $(body)"
  jq -e --arg a "$read" --arg b "$mention" 'all(.rows[]; .thread_id != $a and .thread_id != $b)' "$work/body" >/dev/null ||
    fail "$section" "GET /api/v1/me/replies' unread state answers neither thread" "$(jq -c '[.rows[].thread_id]' "$work/body")"
  pass "$section" "GET /api/v1/me/replies' default unread state answers neither thread"
  one_read "$person"
  holds "comment:$read" || fail "$section" "the one read answers the thread $person wrote in and read" "no entry comment:$read"
  pass "$section" "the one read answers the thread $person wrote in and read"
  holds "comment:$mention" || fail "$section" "the one read answers the thread $person was only mentioned in" "no entry comment:$mention"
  pass "$section" "the one read answers the thread $person was only mentioned in"
}

# --- the run -----------------------------------------------------------------------------------

failed_sections=()
run_section() {
  isolated "$1_section"
  [[ "$isolated_status" == 0 ]] || failed_sections+=("$1")
}

run_section route_scan
run_section bad_asks
run_section isolation
run_section cross_project
run_section traversal
run_section horizon
run_section gate_missing_shape
seed_the_chain
mismatch_root="" mismatch_reply=""
plant_mismatch
run_section gate_mismatch
run_section stored_row
psql_do seed "clear the planted thread" "
  delete from user_threads where thread_id = '$mismatch_root';
  delete from comments where id in ('$mismatch_reply', '$mismatch_root')"
pass seed "cleared the planted thread comment:$mismatch_root"
run_section gate_uncovered
run_section gate_met
run_section old_routes
run_section default
run_section read_then_mention

if [[ "${#failed_sections[@]}" -gt 0 ]]; then
  printf 'one-inbox-routes.sh: %d section(s) failed: %s\n' "${#failed_sections[@]}" "${failed_sections[*]}" >&2
  exit 1
fi
printf 'one-inbox-routes.sh: every check passed\n'
