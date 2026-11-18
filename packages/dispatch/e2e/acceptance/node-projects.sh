#!/usr/bin/env bash
# Drives the project of every node, which a read limited to a project depends on: every issue,
# document, ask, message, comment and block a create writes is a node of the store that names its
# project, written by its kind's own writer in the create's transaction.
#
# It creates one object of each kind through the API - an issue (whose spec is a document with
# blocks), a document uploaded to the issue, a document of the project, an ask, a comment and a
# message on the issue - and reads each one's node in graph_nodes: in the project, with the
# `project` value tuple that states it. It then writes the three things no project owns - a file
# sent in an agent's conversation, a direct message to that agent and a broadcast's copy to it -
# and reads each one's node between its parties, with the `parties` value tuple that states them:
# the conversation's session for the file, and for each message the session that sent it and the
# one it went to. It then runs `envoy-dispatch backfill-graph-store` and `envoy-dispatch census
# --store` and holds the census to `missing=0 differs=0 unscoped=0` for every project-owned kind.
# Last, the control: it takes the ask's node away and holds the census to naming exactly that ask,
# `missing=1` on the ask kind and `census: project incomplete ask <id>: missing`, so the zeros
# before are read from an instrument that sees a missing node.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), AGENT_SESSION (a session the
# server's Envoy listener lists, which takes direct messages and broadcasts), DEV_SIGNIN_LOGIN (a
# login the server's dev sign-in route signs in: a direct message and a broadcast are a person's
# writes, which an agent's bearer may not make), DATABASE_URL (the database that server serves,
# which `envoy-dispatch` reads from its own environment), and `envoy-dispatch` on PATH. It reads
# this pod's own test database and never a deployment's: it refuses a DATABASE_URL whose host is
# not loopback, and the one node it deletes is the ask this run made.
#
# Under e2e/acceptance/with-harness.sh, which exports DISPATCH_E2E_PORT, FAKE_ENVOY_PORT and
# DATABASE_URL and none of the other inputs, the driver is its own caller, so the check that runs
# it (c-a-node-projects) is one command: it
# addresses the harness's server with the agent bearer the harness pins (`e2e-token`), signs in
# `alice`, whom the harness seeds, creates a project of its own, adds an agent session of its own
# to the fake Envoy's session list and puts the list back when it ends, and builds
# `envoy-dispatch` from this checkout where PATH holds none.
#
# The node writers' own grep, where a reader wants one: `graphstore\.(WriteUncaused|Write|WriteArms)\(|graphstore\.Node\{`,
# with api/comment_create.go as its control. `graphstore.Caused` is left out on purpose: it names
# every deferred write of the store, a block index's and a follow's among them, so it cannot tell a
# node writer from the cause of another write.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/db DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE AGENT_SESSION=s1 node-projects.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the built
       `envoy-dispatch` must be on PATH, DATABASE_URL must name the database that server
       serves, on this machine, and AGENT_SESSION a session its listener lists)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
harness=""
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
if [[ -z "${DISPATCH_URL:-}" && -n "${DISPATCH_E2E_PORT:-}" && -n "${FAKE_ENVOY_PORT:-}" ]]; then
  harness=1
  DISPATCH_URL="http://127.0.0.1:$DISPATCH_E2E_PORT"
  DISPATCH_TOKEN=e2e-token
  DEV_SIGNIN_LOGIN=alice
  PROJECT="NODE$(printf '%05d' "$((RANDOM % 100000))" | tr 0-9 A-J)"
  AGENT_SESSION="node-projects-agent-$$"
fi
readonly harness
require_env DISPATCH_URL PROJECT AGENT_SESSION DEV_SIGNIN_LOGIN DATABASE_URL
require_commands curl jq psql
command -v envoy-dispatch >/dev/null || [[ -n "$harness" ]] || require_commands envoy-dispatch
read_bearer

readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
readonly person="$DEV_SIGNIN_LOGIN"
# shellcheck disable=SC2153 # PROJECT and DATABASE_URL are the caller's inputs, checked by name above
readonly project="$PROJECT" database_url="$DATABASE_URL"
case "$database_url" in
  *@127.0.0.1:* | *@localhost:* | *@\[::1\]:* | *@127.0.0.1/* | *@localhost/*) ;;
  *)
    printf 'node-projects.sh: DATABASE_URL names no loopback host; this driver deletes a node and runs only on its own test database\n' >&2
    exit 2
    ;;
esac
stamp="$(date +%s)-$$"
readonly stamp
readonly session="node-projects-$stamp"

umask 077
work="$(mktemp -d)"
readonly work
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"
key=""
fixture_saved=""
cleanup() {
  local code=$?
  if [[ -n "$key" ]]; then
    send PATCH "/api/v1/issues/$key" \
      "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] || printf 'node-projects.sh: could not close %s: HTTP %s\n' "$key" "$status" >&2
  fi
  if [[ -n "$fixture_saved" ]]; then
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
      --data-binary "@$work/sessions.json" "http://127.0.0.1:$FAKE_ENVOY_PORT/__fixture/sessions" ||
      printf 'node-projects.sh: could not put the fake Envoy session list back\n' >&2
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# send METHOD PATH [JSON]: sends METHOD PATH with the agent's bearer; send_as_person does the same
# with the person's cookie and the dashboard's origin. The answer's body goes to $work/body and
# its status to $status.
send() { request "$work/auth.args" "$@"; }
send_as_person() { request "$work/person.args" "$@"; }
request() {
  local -a auth data=()
  mapfile -t auth <"$1"
  shift
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS -X "$1" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}
printf '%s\n' -H "@$work/auth" >"$work/auth.args"
printf '%s\n' -b "$work/jar" -H "Origin: $origin" >"$work/person.args"

actor() { jq -cn --arg s "$session" '{kind: "session", id: $s}'; }

# Under the harness the driver makes what its inputs would otherwise name (the header says what).
if [[ -n "$harness" ]]; then
  if ! command -v envoy-dispatch >/dev/null; then
    require_commands go
    mkdir "$work/bin"
    go -C "$script_dir/../../../envoy" build -o "$work/bin/envoy-dispatch" ./cmd/dispatch 2>"$work/build" ||
      fail setup "build envoy-dispatch from this checkout" "$(tail -n 5 "$work/build")"
    # shellcheck disable=SC2031 # this shell's own PATH, which every command after it reads
    PATH="$work/bin:$PATH"
    pass setup "built envoy-dispatch from this checkout"
  fi
  curl -sS -o /dev/null -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$person")" 2>"$work/curl" ||
    fail setup "sign $person in" "$(<"$work/curl")"
  send POST /api/v1/projects "$(jq -cn --arg k "$project" --arg p "$person" --argjson a "$(actor)" \
    '{key: $k, name: "Node projects", default_assignee: $p, actor: $a}')"
  want_status 201 setup "create project $project"
  curl -sS -o "$work/sessions.json" "http://127.0.0.1:$FAKE_ENVOY_PORT/v1/sessions" 2>"$work/curl" ||
    fail setup "read the fake Envoy's sessions" "$(<"$work/curl")"
  jq -e 'type == "array"' "$work/sessions.json" >/dev/null ||
    fail setup "read the fake Envoy's sessions" "$(head -c 300 "$work/sessions.json")"
  jq -c --arg s "$AGENT_SESSION" \
    '[.[] | {session_id, title, dir, machine_id, host, roles, capabilities}]
     + [{session_id: $s, title: "Node projects agent", machine_id: "acceptance", roles: [],
         capabilities: ["aside", "btw", "steer"]}]' "$work/sessions.json" >"$work/fixture.json"
  fixture_saved=1
  curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
    --data-binary "@$work/fixture.json" "http://127.0.0.1:$FAKE_ENVOY_PORT/__fixture/sessions" 2>"$work/curl" ||
    fail setup "add $AGENT_SESSION to the fake Envoy's sessions" "$(<"$work/curl")"
  pass setup "made project $project and agent session $AGENT_SESSION on the harness"
fi

# node_names_project SECTION KIND ID: the node KIND/ID is in the project, and its `project` value
# tuple says so.
node_names_project() {
  query "$1" "the $2 node of $3 names $project" \
    "select coalesce((select project_key from graph_nodes where kind = '$2' and id = '$3'), '(no node)')"
  [[ "$answer" == "$project" ]] || fail "$1" "the $2 node of $3 names $project" "it names '$answer'"
  query "$1" "the $2 node of $3 states its project" \
    "select count(*) from graph_tuples where from_kind = '$2' and from_id = '$3' and kind = 'project' and to_kind = 'value' and to_id = '$project'"
  [[ "$answer" == 1 ]] || fail "$1" "the $2 node of $3 states its project" "it states it $answer time(s)"
  pass "$1" "the $2 node of $3 names $project"
}

# node_names_parties SECTION KIND ID PARTY...: the node KIND/ID is in no project and between the
# PARTYs, and its `parties` value tuple names them sorted, each once, as the store writes them.
node_names_parties() {
  local section="$1" kind="$2" id="$3" want
  shift 3
  want="$(printf '%s\n' "$@" | LC_ALL=C sort -u | paste -sd' ' -)"
  query "$section" "the $kind node of $id is between $want" \
    "select coalesce((select coalesce(project_key, '-') || '|' || coalesce(array_to_string(parties, ' '), '-') from graph_nodes where kind = '$kind' and id = '$id'), '(no node)')"
  [[ "$answer" == "-|$want" ]] || fail "$section" "the $kind node of $id is between $want" "it holds '$answer' (project|parties)"
  query "$section" "the $kind node of $id states its parties" \
    "select count(*) from graph_tuples where from_kind = '$kind' and from_id = '$id' and kind = 'parties' and to_kind = 'value' and to_id = '$want'"
  [[ "$answer" == 1 ]] || fail "$section" "the $kind node of $id states its parties" "it states them $answer time(s)"
  pass "$section" "the $kind node of $id is between $want"
}

# census_line KIND: the census's project line of KIND, from the last census run.
census_line() { grep -E "^census: project kind=$1 " "$work/census" || true; }

section=create
send POST /api/v1/issues "$(jq -cn --arg p "$project" --argjson a "$(actor)" \
  '{project: $p, title: "Every node names its project", spec: "First paragraph.\n\nSecond paragraph.\n", actor: $a}')"
want_status 201 "$section" "create an issue"
key="$(jq -r .key "$work/body")"
pass "$section" "created $key"
node_names_project "$section" issue "$key"
node_names_project "$section" artifact "$key/spec"
query "$section" "the spec's blocks name $project" \
  "select count(*) filter (where project_key is distinct from '$project') || '/' || count(*) from graph_nodes where kind = 'block' and starts_with(id, '$key/spec#')"
[[ "$answer" =~ ^0/[1-9] ]] || fail "$section" "the spec's blocks name $project" "blocks naming another / all: $answer"
pass "$section" "the spec's blocks name $project"

send POST "/api/v1/issues/$key/artifacts" "$(jq -cn --argjson a "$(actor)" '{name: "notes.md", content: "# Notes\n", actor: $a}')"
want_status 201 "$section" "upload an issue document"
node_names_project "$section" artifact "$(jq -r .artifact.ref_key "$work/body")"

send POST "/api/v1/projects/$project/artifacts" "$(jq -cn --argjson a "$(actor)" --arg n "Playbook $stamp.md" '{name: $n, content: "# Playbook\n", actor: $a}')"
want_status 201 "$section" "upload a project document"
node_names_project "$section" artifact "$(jq -r .artifact.ref_key "$work/body")"

send POST "/api/v1/issues/$key/asks" "$(jq -cn --argjson a "$(actor)" '{question: "Does every node name its project?", actor: $a}')"
want_status 201 "$section" "open an ask"
ask="$(jq -r .id "$work/body")"
node_names_project "$section" ask "$ask"

send POST "/api/v1/issues/$key/comments" "$(jq -cn --argjson a "$(actor)" '{body: "A remark.", actor: $a}')"
want_status 201 "$section" "write a comment"
node_names_project "$section" comment "$(jq -r .id "$work/body")"

send POST "/api/v1/issues/$key/messages" "$(jq -cn --argjson a "$(actor)" '{body: "A message.", actor: $a}')"
want_status 201 "$section" "write a message"
node_names_project "$section" message "$(jq -r .id "$work/body")"

section=parties
readonly agent="$AGENT_SESSION"
curl -sS -o /dev/null -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$person")" 2>"$work/curl" ||
  fail "$section" "sign $person in" "$(<"$work/curl")"
[[ -s "$work/jar" ]] || fail "$section" "sign $person in" "the dev sign-in route left no cookie"
pass "$section" "signed $person in"
send GET /api/v1/agents
want_status 200 "$section" "list the agents"
jq -e --arg s "$agent" 'any(.. | objects | select(has("session_id")); .session_id == $s)' "$work/body" >/dev/null ||
  fail "$section" "the listener lists $agent" "it lists $(jq -c '[.. | objects | .session_id? // empty]' "$work/body")"
pass "$section" "the listener lists $agent"

printf '\x89PNG\r\n\x1a\nnode-projects %s' "$stamp" >"$work/photo.png"
status="$(curl -sS -X POST -o "$work/body" -w '%{http_code}' -H "@$work/auth" \
  -F "name=photo-$stamp.png" -F "actor=$(actor)" -F "file=@$work/photo.png;type=image/png" \
  "$base/api/v1/agents/$(q "$agent")/artifacts" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
want_status 201 "$section" "send a file in $agent's conversation"
node_names_parties "$section" artifact "$(jq -r .artifact.ref_key "$work/body")" "session:$agent"

# sender_party ID: the party the message row's own author names - a person by the canonical login
# the server stored for them, lower-cased and trimmed as graphstore.AuthorOf reads it - into
# $answer. It reads the row the create wrote, not the node under test.
sender_party() {
  query "$section" "read the sender of $1" \
    "select case when author->>'kind' = 'user' then 'person:' || lower(btrim(author->>'id')) else '(not a person)' end from messages where id = '$1'"
}

send_as_person POST "/api/v1/agents/$(q "$agent")/messages" "$(jq -cn --arg b "Direct $stamp." '{body: $b, delivery: "btw"}')"
want_status 201 "$section" "$person sends $agent a direct message"
direct="$(jq -r .id "$work/body")"
sender_party "$direct"
node_names_parties "$section" message "$direct" "$answer" "session:$agent"

send_as_person POST /api/v1/broadcasts "$(jq -cn --arg s "$agent" --arg b "Broadcast $stamp." \
  '{body: $b, delivery: "btw", session_ids: [$s]}')"
want_status 201 "$section" "$person broadcasts to $agent"
query "$section" "read the broadcast's copy" \
  "select coalesce((select id::text from messages where broadcast_id is not null and body = 'Broadcast $stamp.'), '(no copy)')"
copy="$answer"
sender_party "$copy"
node_names_parties "$section" message "$copy" "$answer" "session:$agent"

section=census
envoy-dispatch backfill-graph-store >"$work/backfill" 2>&1 ||
  fail "$section" "backfill-graph-store runs" "$(tail -n 5 "$work/backfill")"
pass "$section" "backfill-graph-store runs"
envoy-dispatch census --store >"$work/census" 2>&1 || true
for kind in issue artifact ask message comment block; do
  line="$(census_line "$kind")"
  [[ "$line" =~ rows=[1-9][0-9]*\ missing=0\ differs=0\ unscoped=0$ ]] ||
    fail "$section" "the $kind census reads zeros over rows it read" "${line:-no line for $kind}"
  pass "$section" "the $kind census reads zeros: ${line#census: project }"
done

section=control
query "$section" "take the ask's node away" "
  with tuples as (delete from graph_tuples where from_kind = 'ask' and from_id = '$ask' returning 1),
       inbound as (delete from graph_tuples where to_kind = 'ask' and to_id = '$ask' returning 1)
  delete from graph_nodes where kind = 'ask' and id = '$ask' returning (select count(*) from tuples)"
pass "$section" "took the ask's node away"
envoy-dispatch census --store >"$work/census" 2>&1 && fail "$section" "the census refuses" "it exited 0" || true
line="$(census_line ask)"
[[ "$line" =~ missing=1\ differs=0\ unscoped=0$ ]] || fail "$section" "the ask census reads one missing node" "${line:-no line for ask}"
pass "$section" "the ask census reads one missing node"
grep -qxF "census: project incomplete ask $ask: missing" "$work/census" ||
  fail "$section" "the census names the ask" "$(grep '^census: project incomplete' "$work/census" | head -n 5)"
pass "$section" "the census names ask $ask"
