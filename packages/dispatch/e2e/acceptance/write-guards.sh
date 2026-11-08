#!/usr/bin/env bash
# Drives Dispatch's three write guards end to end against a running server, through the real
# `dispatch` command, as an agent and a person see them:
#
#   review    a person approves an issue's specification, and the question an agent opens with
#             `dispatch ask` names that review: who gave it, when, that they approved, the reason
#             they gave, and the line asking the sender to say what changed since it.
#   identity  a document holds a question, a person answers it, and the agent uploads the body it
#             composed again, writing the same question with no `#id` and none of the attributes
#             the server keeps. The uploaded block takes the live block's id, so the ask stays on
#             its block, the answered row stays the issue's one row for that question, and the
#             settlement writes the answer onto the block again.
#   base      an upload that replaces a document says which text it replaces. A base behind the
#             live document is refused `409 PRECONDITION_FAILED`, whose answer carries the live
#             token, the live version and where to read a fresh token; the command exits non-zero
#             and nothing is written. The same upload with the base `dispatch doc-read` prints is
#             taken. A replacement that names no base is taken on a server carrying the default,
#             and refused `400 PRECONDITION_REQUIRED` on one with DISPATCH_UPLOAD_BASE_REQUIRED=on.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DEV_SIGNIN_LOGIN (a login the
# server's dev sign-in route, /auth/_dev/signin, signs in: the local harness has it, a deployed
# server does not). The review and the answer are a person's writes, which no bearer may make, so
# the login is required rather than skipped: without it this driver proves nothing.
#
# BASE_REQUIRED_DATABASE_URL is optional, and names an empty Postgres database this driver may
# use. The refusal of a replacement that names no base is the server's own
# DISPATCH_UPLOAD_BASE_REQUIRED setting, which is off by default, so the server under test — which
# carries that default — cannot show it: with the database named, this driver builds the server of
# this checkout, starts one of its own with the setting on, on a port of its own and that
# database, drives the refusal against it and stops it by the pid it captured. Unset, the `base`
# section still drives the other half on the server under test, that such a replacement is taken
# while the setting is off, and prints one `SKIP: base …` line for the refusal it did not drive.
#
# The driver writes three issues in PROJECT — one whose specification a person approves, one whose
# document holds the question a person answers, and one whose document an upload replaces — and
# closes every one of them, also when a check fails. Nothing it writes is read by a second run, so
# it may be run again at once.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. A run that printed nothing did not
# happen.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script. A negative check — `grep -q … ||
# fail` and the like — is written as the AND list it reads as, and is safe for that same reason:
# its failing command is not the last one, so `set -e` leaves the `|| fail` to decide.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice [BASE_REQUIRED_DATABASE_URL=postgres://…] write-guards.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       DEV_SIGNIN_LOGIN is required: the review and the answer this driver needs are a person's
       writes, and only a server with dev sign-in mints that person's cookie;
       BASE_REQUIRED_DATABASE_URL names an empty Postgres database this driver may start a second
       server on, with DISPATCH_UPLOAD_BASE_REQUIRED=on, to drive the refusal of a replacement
       that names no base; unset, that one check is skipped and the other half is driven instead)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands bun curl jq
read_bearer
# No child process inherits the bearer from here: read_bearer has unset it from the environment,
# curl reads it from a header file, and the CLI and the second server get it in an environment
# this script builds.

readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# The server the CLI and every request of this run address. It is the server under test for the
# whole run but for the one block that drives the server this driver starts itself, which sets it
# to that server and sets it back; cleanup sets it back too, since the issues to close are the
# server under test's.
server="$base"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
readonly dev_login="$DEV_SIGNIN_LOGIN"
readonly base_required_database_url="${BASE_REQUIRED_DATABASE_URL-}"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
readonly cli_entry="$script_dir/../../../envoy-client/bin/dispatch.ts"
readonly server_package="$script_dir/../../../envoy"
bun_bin="$(command -v bun)"
readonly bun_bin
readonly saved_path="$PATH"
readonly saved_tmpdir="${TMPDIR:-}"
session="write-guards-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The issues the run writes, each cleared from the list once it is closed, and the pid of the
# server this run started itself, empty until it has started one.
written=()
closed=()
second_server_pid=""

# close_written ISSUE: closes ISSUE, an issue this script wrote, from cleanup.
close_written() {
  cli issue-update --issue "$1" --status "done" \
    --reason "write-guards.sh stopped before its last check; closing the issue it wrote." \
    >"$work/cleanup" 2>&1 ||
    printf 'write-guards.sh: could not close %s: %s\n' "$1" "$(<"$work/cleanup")" >&2
}
# The teardown touches nothing it did not make: the issues this run wrote on the server under
# test, the one server process it started, by the pid it captured from that start and only when it
# captured one, and its own temporary directory. It kills no pattern and no process group.
cleanup() {
  local code=$? issue index
  server="$base"
  for index in "${!written[@]}"; do
    issue="${written[$index]}"
    [[ "${closed[$index]}" == 1 ]] || close_written "$issue"
  done
  if [[ -n "$second_server_pid" ]]; then
    kill "$second_server_pid" 2>/dev/null || true
    wait "$second_server_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}
skip() { printf 'SKIP: %s %s\n' "$1" "$2"; }

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$server"
    export DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}

q() { jq -rn --arg v "$1" '$v|@uri'; }

# request AS METHOD PATH [JSON]: sends METHOD PATH as AS: `agent`, with the bearer in $work/auth,
# or `person`, with the session cookie the dev sign-in left in $work/jar and the dashboard's origin,
# which the server requires of every write a cookie authenticates. The answer's body goes to
# $work/body and its HTTP status to $status.
request() {
  local -a auth data=()
  if [[ "$1" == person ]]; then
    auth=(-b "$work/jar" -H "Origin: $origin")
  else
    auth=(-H "@$work/auth")
  fi
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$server$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# send METHOD PATH [JSON]: request as the agent.
send() { request agent "$@"; }
# person_send METHOD PATH [JSON]: request as the person DEV_SIGNIN_LOGIN signed in.
person_send() { request person "$@"; }

# get PATH: send GET PATH.
get() { send GET "$1"; }
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# expect SECTION WHAT WANT JQ-ARGS...: jq -cS JQ-ARGS over the last answer's body prints WANT.
expect() {
  local section="$1" what="$2" want="$3" got
  shift 3
  got="$(jq -cS "$@" "$work/body" 2>&1)" || fail "$section" "$what" "jq: $got; body: $(body)"
  [[ "$got" == "$want" ]] || fail "$section" "$what" "got $got, want $want"
  pass "$section" "$what"
}

# How long a check waits for a settlement: the document service writes a document's version, and
# an `:::ask` block's row, once its room has been quiet for its settle delay, which is two seconds
# on a server that sets none.
readonly settle_wait=60

# wait_until SECTION WHAT CONDITION ARGS...: runs CONDITION every quarter second until it
# succeeds, and fails the run when it has not succeeded within settle_wait seconds.
wait_until() {
  local section="$1" what="$2" deadline=$((SECONDS + settle_wait))
  shift 2
  until "$@"; do
    ((SECONDS < deadline)) ||
      fail "$section" "$what" "nothing after ${settle_wait}s: $(body)"
    sleep 0.25
  done
}

# issue SECTION TITLE FILE: creates an issue with the spec FILE; sets $key to its key and records
# it for cleanup.
issue() {
  run "$1" "create \"$2\"" issue --project "$project" --title "$2" --spec-file "$3" --force
  [[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
    fail "$1" "create \"$2\"" "no issue key in: $(<"$work/out")"
  key="${BASH_REMATCH[1]}"
  written+=("$key")
  closed+=(0)
  pass "$1" "created $key in $project"
}

# close_issue SECTION ISSUE: closes ISSUE and marks it closed for cleanup.
close_issue() {
  local index
  run "$1" "close $2" issue-update --issue "$2" --status "done" \
    --reason "The write guards check closes the issue it wrote."
  for index in "${!written[@]}"; do
    [[ "${written[$index]}" != "$2" ]] || closed[index]=1
  done
  pass "$1" "closed $2"
}

# upload SECTION ISSUE NAME FILE [BASE]: uploads FILE as the document NAME of ISSUE. BASE is the
# document token the upload replaces, which an upload over a document the issue already holds
# names; a first upload of a name has none to name.
upload() {
  if [[ -n "${5:-}" ]]; then
    run "$1" "upload $3 to $2" artifact --issue "$2" --name "$3" --path "$4" \
      --precondition-json "$(jq -cn --arg t "$5" '{document: $t}')"
  else
    run "$1" "upload $3 to $2" artifact --issue "$2" --name "$3" --path "$4"
  fi
  pass "$1" "uploaded $3 to $2"
}

# read_token SECTION ISSUE NAME: sets $doc_token to the base `dispatch doc-read` prints for
# ISSUE's document NAME, the `Document token: sha256:…` line of its own output, which is where an
# upload's base comes from.
read_token() {
  run "$1" "read $2's $3" doc-read --issue "$2" --artifact "$3"
  doc_token="$(sed -n 's/^Document token: //p' "$work/out" | tail -n 1)"
  [[ -n "$doc_token" ]] || fail "$1" "read $2's $3" "no document token in: $(<"$work/out")"
}

# document_body ISSUE NAME FILE [BASE]: the JSON an artifact POST takes for FILE under NAME, with
# the base it replaces where BASE is given.
document_body() {
  if [[ -n "${4:-}" ]]; then
    jq -cn --arg a "$session" --arg n "$2" --rawfile c "$3" --arg t "$4" \
      '{actor: {kind: "session", id: $a}, name: $n, content: $c, precondition: {document: $t}}'
  else
    jq -cn --arg a "$session" --arg n "$2" --rawfile c "$3" \
      '{actor: {kind: "session", id: $a}, name: $n, content: $c}'
  fi
}

# refusal SECTION WHAT SUBSTRING: the last answer's `error` holds SUBSTRING. The message is read
# out of the JSON, so the comparison is the words a reader sees, not their escaping on the wire.
refusal() {
  local message
  message="$(jq -er '.error' "$work/body")" || fail "$1" "$2 names $3" "no error in: $(body)"
  [[ "$message" == *"$3"* ]] || fail "$1" "$2 names $3" "the refusal reads $message"
  pass "$1" "$2 names $3"
}

# artifact_id SECTION ISSUE NAME: sets $artifact to the id of ISSUE's document NAME.
artifact_id() {
  get "/api/v1/issues/$2/artifacts"
  want_status 200 "$1" "read $2's documents"
  artifact="$(jq -er --arg n "$3" 'map(select(.name == $n)) | .[0].id' "$work/body")" ||
    fail "$1" "find $2's $3" "$(body)"
}

# --- setup: the person this run signs in ---------------------------------------------------------

curl -sS -o /dev/null -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$dev_login")" \
  2>"$work/curl" || fail setup "sign $dev_login in" "$(<"$work/curl")"
[[ -s "$work/jar" ]] || fail setup "sign $dev_login in" "the dev sign-in set no cookie"
pass setup "signed $dev_login in at the dev sign-in route"

# --- review: the question names the review the issue's specification carries ----------------------

# The reason the person gives, and a question that asks about something else: the line the
# question prints is about the document, so nothing of the reason may reach it through the
# related-item check, which searches the question's own words.
readonly review_reason="Measured against the ledger figures of last quarter."
readonly review_question="Which cipher suite should the gateway negotiate when a client offers only the ones withdrawn in 2024? Recommendation: refuse the connection and record the client."

cat >"$work/review-spec.md" <<'SPEC'
The gateway terminates the ledger service's transport.

The gateway holds one connection pool for each tenant.
SPEC
issue review "Write guards: a question names the review the spec carries" "$work/review-spec.md"
reviewed="$key"
readonly reviewed

artifact_id review "$reviewed" spec.md
person_send POST "/api/v1/artifacts/$artifact/reviews" \
  "$(jq -cn --arg r "$review_reason" '{state: "approved", reason: $r}')"
want_status 201 review "$dev_login approves $reviewed's specification"
review_at="$(jq -er '.created_at' "$work/body")" ||
  fail review "read the time of $dev_login's review" "$(body)"
readonly review_at
pass review "$dev_login approved $reviewed's specification at version 1"

run review "open a question on $reviewed" ask --issue "$reviewed" --question "$review_question"
readonly review_line="$dev_login approved this issue's spec at version 1 on $review_at: \"$review_reason\""
grep -qxF "$review_line" "$work/out" ||
  fail review "the question names the review the specification carries" "$(<"$work/out")"
pass review "the question's answer reads: $review_line"
grep -qxF "Say what changed since that review in a reply on this ask." "$work/out" ||
  fail review "the question asks the sender to say what changed since the review" "$(<"$work/out")"
pass review "the question's answer asks the sender to say what changed since that review"

# --- identity: a whole-document upload leaves an answered question where it is --------------------

# ask_settled ISSUE: the settlement has made the document's question an ask row of ISSUE.
ask_settled() {
  get "/api/v1/issues/$1/asks"
  [[ "$status" == 200 ]] || return 1
  [[ "$(jq -c '[.[] | select(.block_id == "transport")] | length' "$work/body")" == 1 ]]
}

# block_answered ISSUE: the settlement has written the person's answer onto the document's block,
# under the id the document gave it.
block_answered() {
  get "/api/v1/issues/$1/artifacts/design-md/text"
  [[ "$status" == 200 ]] || return 1
  jq -e --arg who "$dev_login" \
    '.markdown | test(":::ask\\{#transport .*state=\"answered\" answered_by=\"\($who)\"")' \
    "$work/body" >/dev/null
}

# carry_decided ISSUE: the settlement has decided what the uploaded body's question block is,
# either way: it has written the person's answer onto the block the question was answered on, or
# the issue holds a second ask row, which is what a question the upload did not recognise opens.
# It is the wait a check of what the upload decided takes, and no part of that check: a wait on
# the carry alone would make a question that split read as a wait that timed out.
carry_decided() {
  block_answered "$1" && return 0
  get "/api/v1/issues/$1/asks"
  [[ "$status" == 200 ]] || return 1
  [[ "$(jq -c 'length' "$work/body")" != 1 ]]
}

cat >"$work/identity-spec.md" <<'SPEC'
The ledger service reaches the gateway over one transport.
SPEC
issue identity "Write guards: an upload leaves an answered question where it is" \
  "$work/identity-spec.md"
carry="$key"
readonly carry

# The document the agent uploads, holding one question under an id of its own.
cat >"$work/identity-document.md" <<'DOCUMENT'
Context for the transport choice.

:::ask{#transport urgency="med" multiple="false"}
Which transport should the gateway expose to the ledger service?

- REST: Matches the platform.
- gRPC: Streams.
:::
DOCUMENT
upload identity "$carry" design.md "$work/identity-document.md"
wait_until identity "the document's question becomes an ask" ask_settled "$carry"
answered_ask="$(jq -er '.[] | select(.block_id == "transport") | .id' "$work/body")" ||
  fail identity "read the ask the document's question opened" "$(body)"
readonly answered_ask
edited_at="$(jq -c --arg id "$answered_ask" 'map(select(.id == $id)) | .[0].edited_at' "$work/body")"
pass identity "the document's question opened ask $answered_ask on block transport"

person_send POST "/api/v1/asks/$answered_ask/answer" \
  "$(jq -cn --argjson e "$edited_at" \
    '{selected: ["REST"], text: "REST, to match the platform.", expected_edited_at: $e}')"
want_status 200 identity "$dev_login answers the document's question"
pass identity "$dev_login answered ask $answered_ask with REST"
wait_until identity "the answer is written onto the block" block_answered "$carry"
pass identity "the settlement wrote $dev_login's answer onto block transport"

# The body the agent composes again: the same question, written with no id of its own and none of
# the attributes the server keeps, since an agent writing a body knows neither.
cat >"$work/identity-composed.md" <<'DOCUMENT'
Context for the transport choice.

:::ask{urgency="med" multiple="false"}
Which transport should the gateway expose to the ledger service?

- REST: Matches the platform.
- gRPC: Streams.
:::
DOCUMENT
read_token identity "$carry" design.md
upload identity "$carry" design.md "$work/identity-composed.md" "$doc_token"
wait_until identity "the upload's question block is decided" carry_decided "$carry"

get "/api/v1/issues/$carry/asks"
want_status 200 identity "the issue's asks after the upload answer"

expect identity "the issue still holds one ask" 1 'length'
expect identity "that ask is the row the person answered" "\"$answered_ask\"" '.[0].id'
expect identity "the row is still on the block it was answered on" '"transport"' '.[0].block_id'
expect identity "the row still asks the question the agent composed again" \
  '"Which transport should the gateway expose to the ledger service?"' '.[0].question'
expect identity "the row still carries the person's answer" \
  "[\"answered\",\"$dev_login\",[\"REST\"]]" '[.[0].state, .[0].answer.user, .[0].answer.selected]'

get "/api/v1/issues/$carry/artifacts/design-md/text"
want_status 200 identity "the document after the upload answers"
expect identity "the uploaded block took the id the live block had, and the settlement wrote the person's answer onto it again" \
  true \
  '.markdown | test(":::ask\\{#transport .*state=\"answered\" answered_by=\"'"$dev_login"'\"")'

# --- base: the base an upload names, and the two refusals ----------------------------------------

cat >"$work/base-spec.md" <<'SPEC'
The gateway's upload path replaces a document whole.
SPEC
issue base "Write guards: an upload names the base it replaces" "$work/base-spec.md"
guard="$key"
readonly guard

printf 'The first body of the guarded document.\n' >"$work/base-one.md"
printf 'The second body of the guarded document.\n' >"$work/base-two.md"
printf 'The third body, which the stale base must not write.\n' >"$work/base-three.md"
printf 'The fourth body, which names no base.\n' >"$work/base-four.md"

upload base "$guard" guard.md "$work/base-one.md"
read_token base "$guard" guard.md
stale_base="$doc_token"
readonly stale_base
upload base "$guard" guard.md "$work/base-two.md" "$stale_base"

# Where the document stands now, which the refusal below must name and must leave alone.
get "/api/v1/issues/$guard/artifacts/guard-md/text"
want_status 200 base "the guarded document answers"
live_token="$(jq -er '.token' "$work/body")" || fail base "read the live token" "$(body)"
live_version="$(jq -er '.version' "$work/body")" || fail base "read the live version" "$(body)"
readonly live_token live_version
[[ "$live_token" != "$stale_base" ]] ||
  fail base "the second upload moved the document past the base the first read" "$live_token"
pass base "the base read before the second upload is behind the live document"

cli artifact --issue "$guard" --name guard.md --path "$work/base-three.md" \
  --precondition-json "$(jq -cn --arg t "$stale_base" '{document: $t}')" >"$work/out" 2>&1 &&
  fail base "an upload naming a base behind the live document is refused" \
    "the command exited 0: $(<"$work/out")"
pass base "the upload naming a base behind the live document exits non-zero"
grep -qF 'dispatch doc-read' "$work/out" ||
  fail base "the refusal says where a base that is not behind comes from" "$(<"$work/out")"
pass base "the refusal tells the reader to read the document with dispatch doc-read"

get "/api/v1/issues/$guard/artifacts/guard-md/text"
want_status 200 base "the guarded document answers after the refusal"
expect base "the refused upload wrote no version" "$live_version" '.version'
expect base "the refused upload left the document's text as it was" "\"$live_token\"" '.token'

# The same upload over the route the command sends it on, for the status and the fields the
# answer carries, which the command's reader never sees.
send POST "/api/v1/issues/$guard/artifacts" \
  "$(document_body "$guard" guard.md "$work/base-three.md" "$stale_base")"
want_status 409 base "the same upload over the artifacts route"
expect base "it is refused PRECONDITION_FAILED" '"PRECONDITION_FAILED"' '.code'
expect base "the refusal names the live document token" "\"$live_token\"" '.current.document'
expect base "the refusal names the live version" "$live_version" '.current.version'
refusal base "the refusal" 'dispatch doc-read'

read_token base "$guard" guard.md
[[ "$doc_token" == "$live_token" ]] ||
  fail base "dispatch doc-read prints the live token" "$doc_token, want $live_token"
pass base "dispatch doc-read prints the live token the refusal named"
upload base "$guard" guard.md "$work/base-three.md" "$doc_token"

run base "replace the guarded document naming no base" artifact --issue "$guard" --name guard.md \
  --path "$work/base-four.md"
pass base "a replacement naming no base is taken, which is what a server with DISPATCH_UPLOAD_BASE_REQUIRED off, the default, does with one"

# --- base: the refusal a server with the setting on gives, on a server this driver starts --------

# second_server_ready: the server this driver started answers its health route, and is still up.
second_server_ready() {
  kill -0 "$second_server_pid" 2>/dev/null ||
    fail base "the server this driver started stays up" "$(<"$work/second-server.log")"
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$second_base/healthz" 2>/dev/null)" == 200 ]]
}

if [[ -z "$base_required_database_url" ]]; then
  skip base "a replacement naming no base is refused 400 PRECONDITION_REQUIRED: that is the DISPATCH_UPLOAD_BASE_REQUIRED setting, which the server under test carries off, as every server does by default; set BASE_REQUIRED_DATABASE_URL to an empty Postgres database and this driver starts a server of its own with the setting on and drives it"
else
  require_commands go
  (cd "$server_package" && go build -o "$work/dispatch" ./cmd/dispatch) >"$work/build" 2>&1 ||
    fail base "build the server to start with DISPATCH_UPLOAD_BASE_REQUIRED=on" "$(<"$work/build")"
  pass base "built this checkout's server, to start with DISPATCH_UPLOAD_BASE_REQUIRED=on"

  # A port nothing is listening on: bun binds 127.0.0.1:0, reports the port the kernel chose and
  # closes it, as the e2e harness's own held port does. A port taken between the two leaves the
  # server's bind to fail, and its log is what this run then reports.
  second_port="$("$bun_bin" -e \
    'const s = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } }); console.log(s.port); s.stop(true);')" ||
    fail base "choose a port for the server this driver starts" "bun bound no port"
  second_base="http://127.0.0.1:$second_port"
  readonly second_port second_base

  # The server runs in an environment this script builds, as the CLI does: nothing the calling
  # shell exported reaches it. Dev sign-in is mounted because it refuses a DATABASE_URL whose host
  # is not loopback, which keeps this block off any database but a local one; no person signs in
  # on this server, since the one check it serves is an agent's upload.
  mkdir "$work/server-home"
  second_env=(
    PATH="$saved_path"
    HOME="$work/server-home"
    DATABASE_URL="$base_required_database_url"
    DISPATCH_AGENT_TOKEN="$token"
    DISPATCH_DEV_SIGNIN=1
    DISPATCH_IDENTITY=cookie
    DISPATCH_INSECURE_COOKIE=1
    DISPATCH_LISTEN_HOST=127.0.0.1
    DISPATCH_NATS_DISABLED=1
    DISPATCH_PORT="$second_port"
    DISPATCH_SERVER_URL="$second_base"
    DISPATCH_UPLOAD_BASE_REQUIRED=on
  )
  if [[ -n "$saved_tmpdir" ]]; then
    second_env+=(TMPDIR="$saved_tmpdir")
  fi
  env -i "${second_env[@]}" "$work/dispatch" >"$work/second-server.log" 2>&1 &
  second_server_pid=$!
  wait_until base "the server this driver started answers" second_server_ready
  pass base "started a server with DISPATCH_UPLOAD_BASE_REQUIRED=on on port $second_port"

  server="$second_base"
  second_project="WG$$"
  send POST "/api/v1/projects" \
    "$(jq -cn --arg s "$session" --arg k "$second_project" \
      '{actor: {kind: "session", id: $s}, key: $k, name: "Write guards, base required"}')"
  want_status 201 base "create $second_project on that server"
  pass base "created $second_project on that server"
  run base "create an issue on that server" issue --project "$second_project" \
    --title "Write guards: a replacement names no base" --spec-file "$work/base-spec.md" --force
  [[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
    fail base "create an issue on that server" "no issue key in: $(<"$work/out")"
  second_issue="${BASH_REMATCH[1]}"
  pass base "created $second_issue on that server"
  run base "upload the first body of $second_issue's document" artifact --issue "$second_issue" \
    --name guard.md --path "$work/base-one.md"
  pass base "uploaded guard.md to $second_issue, which names no base because nothing is replaced"

  cli artifact --issue "$second_issue" --name guard.md --path "$work/base-two.md" \
    >"$work/out" 2>&1 &&
    fail base "a replacement naming no base, where the server asks for one" \
      "the command exited 0: $(<"$work/out")"
  pass base "with the setting on, the replacement naming no base exits non-zero"
  grep -qF 'must name the base it replaces' "$work/out" ||
    fail base "the refusal says the replacement must name its base" "$(<"$work/out")"
  pass base "the refusal says the replacement must name the base it replaces"
  send POST "/api/v1/issues/$second_issue/artifacts" \
    "$(document_body "$second_issue" guard.md "$work/base-two.md")"
  want_status 400 base "the same replacement over the artifacts route"
  expect base "it is refused PRECONDITION_REQUIRED" '"PRECONDITION_REQUIRED"' '.code'
  refusal base "the refusal" 'dispatch doc-read'
  server="$base"
fi

# --- close: the issues the run wrote -------------------------------------------------------------

close_issue close "$reviewed"
close_issue close "$carry"
close_issue close "$guard"
