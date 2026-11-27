#!/usr/bin/env bash
# Drives the people and the agents of the node and tuple store end to end against a running
# server (the check c-person-graph). It writes comments, a message and an ask
# through the real HTTP API, has the server deliver them to sessions and to a role, adds one
# comment that no session receives, one delivery attempt that fails and one the listener queues
# for a session that is not running, and then reads the store back and checks each tuple: what
# each person authored, what reached each session, that a failed or pending attempt earns no
# tuple and that a queued one earns one, and that a failed one earns its session no node. It
# checks the role, the session and the page, and it holds every read against a table of
# identities a person wrote into this driver. A message earns no
# tuple in this child - only a comment has a node on the near end of a delivery, and
# the child that moves the inbox, the messages and the read state gives a message its own - so
# what the driver reads of the message is the
# role it reaches and that role's page, and that nothing stands on the message itself (G10).
#
# The run writes at least one tuple of every kind it holds, and G14 holds every tuple standing on
# this run's nodes, of every kind, to the writer, the causing event and a time its record allows:
# authored_by and the base attributes (visibility, state, and a comment's project) of every node
# it reads, delivered_to - the answer to an attempt the listener had not taken among them (G12) -
# a page's and a session's part_of, a role's held_by with its retraction (G9), and alias_of
# (G13). A kind that holds no tuple of this run fails there too, so a writer that stopped writing
# one cannot pass as right.
#
# Which clause is read which way, and why:
#
#   - What a person authored, and which person a spelling resolves to, are read through the
#     server's own read of the store, `GET /api/v1/people/{login}/nodes`, which answers the
#     authored_by and delivered_to tuples into that person and nothing else.
#   - Whether the store holds a session at all is read through `GET /api/v1/references`, whose
#     node loader reads `graph_nodes` for a session (refs.loadStoreNodes): a session the store
#     never recorded is absent from the answer, and no read writes one.
#   - A page's `part_of` is served by no route at all - the tuple arm of `graph_edges` carries
#     that kind for no page, and no handler reads it - so it is read through DATABASE_URL, the
#     database this driver already holds for the census it runs. The same goes for a role's
#     `held_by` and the history of its claim, and for a tuple's time, its writer and the event
#     that caused it, which no read answers and which this driver holds against the records.
#
# Two writes a shell driver once could not reach, and how this one reaches them:
#
#   - A role's `held_by` and a session's `part_of` that role. Dispatch's outbox resolves a role
#     route's holder from the bus itself, the real listener's role claims and session
#     registrations (roles.Buckets, which cmd/dispatch builds over the outbox's NATS
#     connection), and never from the listener's `GET /v1/roles/{role}`. Nothing in the harness
#     wrote those buckets, so every role route read unclaimed, was kept for the role's next
#     holder, and stated no claim. The fake Envoy now writes them for the live holder its own
#     lookup names (`PUT /__fixture/role-claims/{role}`, e2e/fake-envoy.ts), on the bus the
#     harness started it with (DISPATCH_E2E_NATS_URLS), so the role route is forwarded to that
#     session and the record states the claim.
#   - An `alias_of`, the identity table's row of a session the server itself proved a person's.
#     The server writes one only for a token the secrets broker scoped to a person, presented
#     under the DPoP scheme with an ES256 proof on every request. The harness's broker is
#     e2e/fake-broker.ts, whose introspection calls a token minted by e2e/fake-broker-token.ts
#     live, and e2e/acceptance/broker-session.ts mints one bound to a key of its own and signs
#     each request's proof, so this driver writes as that session (G13) without holding the
#     fake broker's address. The other half of the same rule stands beside it: a session a
#     credential merely claims in a request body earns no alias (G6).
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DATABASE_URL (the database that
# server serves), FAKE_ENVOY_PORT (the harness's fake Envoy listener, e2e/fake-envoy.ts, whose
# session fixture this driver adds role-less sessions, one of them stopped and one whose sends it
# refuses, and two holders of a role to, and which claims that role on the bus - as
# block-graph.sh takes FAKE_GITHUB_PORT for the fixture it needs), and optionally
# DEV_SIGNIN_LOGIN (default alice), the person the dev sign-in route signs in. Each unmet input
# is exit 2. The server must read the harness's fake broker (run-server.sh's default) for G13.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The driver never prints a token.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/db FAKE_ENVOY_PORT=9023 \
         DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         [DEV_SIGNIN_LOGIN=alice] graph-store.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; DATABASE_URL
       must name the database that server serves, on this machine, and FAKE_ENVOY_PORT the fake
       Envoy listener it delivers through. The server must have been started with a bus
       (DISPATCH_E2E_NATS_URLS, which e2e/run-server.sh hands the server as its NATS_URLS):
       without one the publisher writes no delivered_to tuple and the delivery checks here
       wait out their poll. The fake Envoy must hold that same bus, which the harness gives it
       from the same variable, to claim a role there, and the server must read the harness's
       fake broker, e2e/fake-broker.ts, to take the token of a session it proves a person's.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DATABASE_URL FAKE_ENVOY_PORT
require_commands bun curl jq psql sleep
read_bearer

# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own subshell's; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# shellcheck disable=SC2153 # these are the caller's inputs, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2153 # DATABASE_URL is the caller's input, checked by name above
readonly database_url="$DATABASE_URL"
readonly envoy="http://127.0.0.1:$FAKE_ENVOY_PORT"
readonly person="${DEV_SIGNIN_LOGIN:-alice}"
# The second person of the two that must stay apart: the first person's login with one character
# changed. A canonicalisation that folded the two would answer one person node for both.
twin="${person%?}$(printf '%s' "${person: -1}" | tr 'a-y' 'b-z')"
readonly twin
stamp="$(date +%s)-$$"
readonly stamp
session="graph-store-$stamp"
readonly session
# The sessions this run adds to the listener's fixture: one that claims no role, which the issue's
# route names; another that claims no role, which a mention reaches; a third that claims no role
# and that the listener holds stopped, which a mention reaches through the message the listener
# stores for it; a fourth that claims no role and whose every send the listener refuses, so the
# attempt of its mention fails before it answers it (G12); and two holding a role of this run's
# own, so the role this driver routes a comment to is nobody else's, and a second forward of it
# resolves another holder (G9). The session the broker proves a person's (G13) is no session of
# the listener's: nothing is delivered to it.
loose_session="019a1c09-0000-7000-8000-$(printf '%012d' "$$")"
holder_session="019a1c09-1111-7000-8000-$(printf '%012d' "$$")"
second_holder_session="019a1c09-1112-7000-8000-$(printf '%012d' "$$")"
mentioned_session="019a1c09-6666-7000-8000-$(printf '%012d' "$$")"
stopped_session="019a1c09-7777-7000-8000-$(printf '%012d' "$$")"
answering_session="019a1c09-aaaa-7000-8000-$(printf '%012d' "$$")"
proved_session="019a1c09-9999-7000-8000-$(printf '%012d' "$$")"
readonly loose_session holder_session second_holder_session mentioned_session stopped_session \
  answering_session proved_session
readonly role="acceptance-$stamp"
# The role the message of G10 is delivered to, the route of an issue of that section's own: no
# other issue or write of this run names it, so its role and page nodes are that route's and the
# message reaches nobody else.
readonly message_role="acceptance-message-$stamp"
# The helper that mints the proved session's token and signs each of its requests (G13).
broker_session="$(dirname "${BASH_SOURCE[0]}")/broker-session.ts"
readonly broker_session

umask 077
work="$(mktemp -d)"
readonly work
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
message_key=""
fixture_saved=0
role_claimed=0
cleanup() {
  local code=$?
  if [[ "$fixture_saved" == 1 ]]; then
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
      --data-binary "@$work/sessions.json" "$envoy/__fixture/sessions" ||
      printf 'graph-store.sh: could not put the fake Envoy session fixture back\n' >&2
  fi
  # The claim is the bus's and outlives the fixture's reseed, so it is released by name.
  if [[ "$role_claimed" == 1 ]]; then
    curl -sSf -o /dev/null -X DELETE "$envoy/__fixture/role-claims/$(q "$role")" ||
      printf 'graph-store.sh: could not release the claim of %s on the bus\n' "$role" >&2
  fi
  for issue in "$key" "$message_key"; do
    [[ -n "$issue" ]] || continue
    send PATCH "/api/v1/issues/$issue" \
      "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] ||
      printf 'graph-store.sh: could not close %s: HTTP %s\n' "$issue" "$status" >&2
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# request AS METHOD PATH [JSON]: sends METHOD PATH as AS: `agent`, with the bearer in $work/auth;
# `person`, with the cookie the dev sign-in left in $work/jar.<login> and the dashboard's origin,
# which the server requires of every write a cookie authenticates; a personal token's own
# bearer, named `token:<token>`; or `broker`, the session the fake broker proves a person's
# (G13), whose token and key are in $work and whose every request broker-session.ts signs a proof
# of its own for, since a DPoP proof is spent by the request it is presented with. The answer's
# body goes to $work/body and its status to $status.
request() {
  local -a auth data=()
  case "$1" in
    person:*) auth=(-b "$work/jar.${1#person:}" -H "Origin: $origin") ;;
    token:*)
      # From a file, as $work/auth is: a header on the command line is read by every process.
      printf 'Authorization: Bearer %s\n' "${1#token:}" >"$work/auth.token"
      auth=(-H "@$work/auth.token")
      ;;
    broker)
      status="$(bun "$broker_session" send "$work/broker.key" "$work/broker.token" "$2" "$base$3" \
        "$work/body" ${4+"$4"} 2>"$work/curl")" || status="broker-session.ts failed: $(<"$work/curl")"
      return
      ;;
    *) auth=(-H "@$work/auth") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# send METHOD PATH [JSON]: request as the agent bearer.
send() { request agent "$@"; }
# get PATH: send GET PATH.
get() { send GET "$1"; }

# want_value SECTION WHAT WANT SQL: the one value SQL reads is WANT.
want_value() {
  query "$1" "$2" "$4"
  [[ "$answer" == "$3" ]] || fail "$1" "$2" "read '$answer', want '$3' (from: $4)"
  pass "$1" "$2"
}

# tuples_carry SECTION WHAT ROWS: every tuple ROWS selects carries the actor and the event of the
# write that appended it, and a time its record allows. ROWS is a select of one row per tuple:
# `tuple`, the tuple as a failure names it; `who`, `source_event` and `at`, what it carries;
# `want_who`, the actor of the write that appended it; `caused`, whether its event is that write's,
# which `cause` describes; and `timely`, whether its time is one its record allows, which
# `when_want` describes. A failure names each tuple that breaks any of the three, with what it
# carries and what it should. Every kind this driver holds is one its run writes, so ROWS
# selecting no tuple at all fails too: a writer that stopped stating the kind would otherwise pass
# every rule here by stating nothing to break one. A pass says how many tuples it held.
tuples_carry() {
  query "$1" "$2" "select count(*) || '|' || coalesce(string_agg(tuple || ': ' || concat_ws('; ',
             case when who is distinct from want_who then format('actor %s, want %s', who, want_who) end,
             case when not coalesce(caused, false) then format('event %s, want %s', source_event, cause) end,
             case when not coalesce(timely, false) then format('time %s, want %s', at, when_want) end),
           ' | ' order by tuple)
           filter (where who is distinct from want_who or not coalesce(caused, false)
                         or not coalesce(timely, false)), '')
     from ($3) tuples"
  [[ -z "${answer#*|}" ]] || fail "$1" "$2" "${answer#*|}"
  [[ "${answer%%|*}" != 0 ]] || fail "$1" "$2" "this run wrote no tuple of the kind it holds"
  pass "$1" "$2 (${answer%%|*} tuple(s))"
}

# settles SECTION WHAT WANT SQL [HINT]: polls SQL until it reads WANT, for up to 30 s. The server
# sends a mention and records what it came to after the request that wrote the comment has
# answered, and the outbox states what a batch delivered a moment later still. HINT, where it is
# given, is printed with the failure: a poll that runs out says what the count it never reached
# usually means, which is what tells a reader of this driver which half of the system to look at.
settles() {
  local section="$1" what="$2" want="$3" sql="$4" hint="${5:-}" waited=0
  while true; do
    query "$section" "$what" "$sql"
    [[ "$answer" != "$want" ]] || break
    [[ "$waited" -lt 60 ]] ||
      fail "$section" "$what" "read '$answer', want '$want' after 30 s (from: $sql)${hint:+. $hint}"
    sleep 0.5
    waited=$((waited + 1))
  done
  pass "$section" "$what"
}

# sign_in LOGIN: signs LOGIN in through the dev sign-in route, keeping the cookie in
# $work/jar.LOGIN.
sign_in() {
  curl -sS -o /dev/null -c "$work/jar.$1" "$base/auth/_dev/signin?login=$(q "$1")" 2>"$work/curl" ||
    fail setup "sign $1 in" "curl: $(<"$work/curl")"
  [[ -s "$work/jar.$1" ]] ||
    fail setup "sign $1 in" "the dev sign-in route left no cookie; this driver needs two people"
  pass setup "signed $1 in"
}

# comment SECTION WHAT AS BODY [MENTION]: writes a comment on the issue as AS (agent, person:LOGIN,
# token:TOKEN or broker), optionally mentioning MENTION, and sets $comment to its id and
# $comment_at to the time the record holds.
comment() {
  local section="$1" what="$2" as="$3" text="$4" mention="${5-}" payload
  if [[ "$as" == agent ]]; then
    payload="$(jq -cn --arg b "$text" --arg s "$session" --arg m "$mention" \
      '{body: $b, actor: {kind: "session", id: $s}}
       + (if $m == "" then {} else {mentions: [{target: $m}]} end)')"
  else
    payload="$(jq -cn --arg b "$text" --arg m "$mention" \
      '{body: $b} + (if $m == "" then {} else {mentions: [{target: $m}]} end)')"
  fi
  request "$as" POST "/api/v1/issues/$key/comments" "$payload"
  want_status 201 "$section" "$what"
  comment="$(jq -er '.id' "$work/body")" || fail "$section" "$what" "no comment id: $(body)"
  comment_at="$(jq -er '.created_at' "$work/body")" || fail "$section" "$what" "no created_at: $(body)"
  pass "$section" "$what ($comment)"
}

# token_comment SECTION WHAT TOKEN SESSION BODY: writes a comment with a personal token, which
# names the session it acts for in the body - a session that credential merely claims.
token_comment() {
  request "token:$3" POST "/api/v1/issues/$key/comments" \
    "$(jq -cn --arg b "$5" --arg s "$4" '{body: $b, actor: {kind: "session", id: $s}}')"
  want_status 201 "$1" "$2"
  comment="$(jq -er '.id' "$work/body")" || fail "$1" "$2" "no comment id: $(body)"
  pass "$1" "$2 ($comment)"
}

# claim_role SECTION HOLDER PREVIOUS: has the fake Envoy claim $role on the bus for the live
# holder its own lookup names, as the real listener writes a claim when a session takes a role,
# and holds its answer to HOLDER, replacing the claim of PREVIOUS ("" for none).
claim_role() {
  local code
  code="$(curl -sS -o "$work/claim.json" -w '%{http_code}' -X PUT \
    "$envoy/__fixture/role-claims/$(q "$role")" 2>"$work/curl")" ||
    fail "$1" "claim $role for $2 on the bus" "curl: $(<"$work/curl")"
  if [[ "$code" == 200 ]]; then role_claimed=1; fi
  jq -e --arg h "$2" --arg p "$3" '.holder == $h and .previous == $p' "$work/claim.json" >/dev/null ||
    fail "$1" "claim $role for $2 on the bus" "HTTP $code: $(head -c 300 "$work/claim.json")"
  pass "$1" "the fake Envoy claims $role for $2 on the bus${3:+, in place of $3}"
}

# The identity table. A person wrote these rows, and no code the server runs produces them: each
# names a representation a record can hold and the person it must resolve to, stated here before
# this driver issues any read. $1 of each row is the spelling, $2 the person it must answer.
#
#   - the login in mixed case, and the same login with spaces around it: one person node, whose
#     aliases hang off it;
#   - the raw actor id a record stored, read out of comments.author in G4 and held against this
#     same person;
#   - the two that must stay apart: two logins differing by one character, and two personal
#     tokens of one deployment held by two people (G7), each token the one its own person mints,
#     and each resolving to that person;
#   - a session the server itself proved a person's (G13): the session the broker's token names,
#     which resolves to the person that token acts for, both as the server answers who that
#     credential is and as the store's alias_of tuple names them. It is read once the session has
#     written, since only that write ties it to anybody.
identities=(
  "$person|$person"
  "$(printf '%s' "$person" | tr '[:lower:]' '[:upper:]')|$person"
  "  $person  |$person"
  "$twin|$twin"
)
readonly identities
readonly proved_identity="$proved_session|$person"

# S: the inputs, the people, the listener's sessions and the issue.
loopback_database_url "$database_url" ||
  fail setup "DATABASE_URL names this machine" "the URL must name one loopback host with no connection override; this driver reads this pod's own test database, never a deployment's"
pass setup "DATABASE_URL names this machine"
[[ "$twin" != "$person" ]] ||
  fail setup "two logins differing by one character" "$person and $twin are one login"
sign_in "$person"
sign_in "$twin"

curl -sS -o "$work/sessions.json" "$envoy/v1/sessions" 2>"$work/curl" ||
  fail setup "read the listener's sessions" "curl: $(<"$work/curl")"
jq -e 'type == "array" and length > 0' "$work/sessions.json" >/dev/null ||
  fail setup "read the listener's sessions" "$(head -c 300 "$work/sessions.json")"
fixture_saved=1
jq -c --arg loose "$loose_session" --arg mentioned "$mentioned_session" \
  --arg stopped "$stopped_session" --arg answering "$answering_session" \
  --arg holder "$holder_session" --arg second "$second_holder_session" --arg role "$role" \
  '[.[] | {session_id, title, dir, machine_id, host, roles, capabilities}]
   + [{session_id: $loose, title: "Acceptance loose", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $mentioned, title: "Acceptance mentioned", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $stopped, title: "Acceptance stopped", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $answering, title: "Acceptance answering", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $holder, title: "Acceptance holder", machine_id: "acceptance", roles: [$role],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $second, title: "Acceptance second holder", machine_id: "acceptance",
       roles: [$role], capabilities: ["aside", "btw", "steer"]}]' \
  "$work/sessions.json" >"$work/fixture.json" ||
  fail setup "build the session fixture" "$(head -c 300 "$work/sessions.json")"
curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
  --data-binary "@$work/fixture.json" "$envoy/__fixture/sessions" 2>"$work/curl" ||
  fail setup "seed the session fixture" "curl: $(<"$work/curl")"
# A seeded session is a running one. Stopping it leaves the listener knowing it by its
# known-session record alone (e2e/fake-envoy.ts, `stopped`): Dispatch's delivery read, which asks
# for stopped sessions too, still resolves it, and a send to it is stored and answered
# `state: stopped`. The reseed on exit starts every session over, this one with the rest.
curl -sS -o "$work/stopped.json" -X PATCH -H 'Content-Type: application/json' \
  --data-binary '{"stopped": true}' "$envoy/__fixture/sessions/$stopped_session" 2>"$work/curl" ||
  fail setup "stop $stopped_session in the session fixture" "curl: $(<"$work/curl")"
jq -e '.stopped == true and .live == false' "$work/stopped.json" >/dev/null ||
  fail setup "stop $stopped_session in the session fixture" "$(head -c 300 "$work/stopped.json")"
# The listener answers every send to the answering session 503, its starting gate: Dispatch
# resolves that session, tries the send through its take-over window and records the attempt
# failed against it, which is the attempt G12's answer finds untaken.
curl -sS -o "$work/answering.json" -X PATCH -H 'Content-Type: application/json' \
  --data-binary '{"send_status": 503}' "$envoy/__fixture/sessions/$answering_session" 2>"$work/curl" ||
  fail setup "refuse every send to $answering_session" "curl: $(<"$work/curl")"
jq -e '.send_status == 503 and .live == true' "$work/answering.json" >/dev/null ||
  fail setup "refuse every send to $answering_session" "$(head -c 300 "$work/answering.json")"
pass setup "the listener holds $loose_session and $mentioned_session (no role), $stopped_session (no role, stopped), $answering_session (no role, every send refused), and $holder_session and $second_holder_session (role $role)"

send POST "/api/v1/issues" "$(jq -cn --arg p "$project" --arg s "$session" \
  '{project: $p, title: "Person and agent graph check", spec: "What the store holds of people and agents.", actor: {kind: "session", id: $s}}')"
want_status 201 setup "create the issue"
key="$(jq -er '.key' "$work/body")" || fail setup "create the issue" "no key: $(body)"
pass setup "created $key in $project"

# G1: what a person authored is an authored_by tuple into their node, and the server's own read
# of the store answers it.
comment G1 "$person writes a comment" "person:$person" "A note $person wrote in $key."
readonly person_comment="$comment"
readonly person_comment_at="$comment_at"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G1 "read $person's page"
expect G1 "the page holds the comment $person wrote" \
  "[{\"id\":\"$person_comment\",\"kind\":\"comment\",\"relations\":[\"authored_by\"]}]" \
  --arg id "$person_comment" '[.rows[] | select(.id == $id) | {id, kind, relations}]'
expect G1 "the page is $person's own" "\"$person\"" '.person'
expect G1 "the row carries the time the record holds" "\"$person_comment_at\"" \
  --arg id "$person_comment" '.rows[] | select(.id == $id) | .created_at'
expect G1 "the row opens the comment in its thread" "\"/issues/$key/comments/$person_comment\"" \
  --arg id "$person_comment" '.rows[] | select(.id == $id) | .href'

# The tuple itself: its time, its writer and the write that caused it are the record's own.
want_value G1 "the authored_by tuple carries the record's time, actor and causing write" "true|true|true" \
  "select (t.at = c.created_at)::text || '|' || (t.who = c.author)::text || '|'
          || (t.source_event = e.id)::text
   from graph_tuples t
   join comments c on c.id::text = t.from_id
   join events e on e.type = 'comment.created' and e.payload->>'id' = c.id::text
   where t.from_kind = 'comment' and t.from_id = '$person_comment' and t.kind = 'authored_by'"
want_value G1 "the author is the person node, not a session" "person|$person" \
  "select to_kind || '|' || to_id from graph_tuples
   where from_kind = 'comment' and from_id = '$person_comment' and kind = 'authored_by'"

# G2: a thing a session wrote stays that session's and is on no person's page.
comment G2 "a session writes a comment" agent "A note the $session session wrote."
readonly session_comment="$comment"
want_value G2 "the session's comment is authored by its session node" "session|$session" \
  "select to_kind || '|' || to_id from graph_tuples
   where from_kind = 'comment' and from_id = '$session_comment' and kind = 'authored_by'"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G2 "read $person's page again"
expect G2 "the session's comment is on no person's page" "0" \
  --arg id "$session_comment" '[.rows[] | select(.id == $id)] | length'

# G3: the identity table, each row read back through the server and held against the person it
# must resolve to. The second person writes first, so the store holds a node for each of the two
# logins that differ by one character and the two pages can be told apart.
comment G3 "$twin writes a comment" "person:$twin" "A note $twin wrote in $key."
readonly twin_comment_own="$comment"
for row in "${identities[@]}"; do
  spelling="${row%%|*}"
  expected="${row##*|}"
  request "person:$person" GET "/api/v1/people/$(q "$spelling")/nodes"
  want_status 200 G3 "read the page of '$spelling'"
  expect G3 "'$spelling' resolves to $expected" "\"$expected\"" '.person'
done
want_value G3 "one person node answers every spelling of $person" 1 \
  "select count(*) from graph_nodes where kind = 'person' and id = '$person'"
want_value G3 "two logins differing by one character are two person nodes" 2 \
  "select count(*) from graph_nodes where kind = 'person' and id in ('$person', '$twin')"
request "person:$person" GET "/api/v1/people/$(q "$twin")/nodes"
want_status 200 G3 "read $twin's page"
expect G3 "what $person wrote is on no page of $twin's" "0" \
  --arg id "$person_comment" '[.rows[] | select(.id == $id)] | length'
expect G3 "$twin's page holds what $twin wrote" "1" \
  --arg id "$twin_comment_own" '[.rows[] | select(.id == $id)] | length'
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G3 "read $person's page beside it"
expect G3 "what $twin wrote is on no page of $person's" "0" \
  --arg id "$twin_comment_own" '[.rows[] | select(.id == $id)] | length'
request "person:$person" GET "/api/v1/people/$(q "nobody-$stamp")/nodes"
want_status 404 G3 "a login the store holds no person node for"
pass G3 "a login the store holds no person node for answers 404"

# G4: the raw actor id a record stored is the fourth row of the table, read out of the record and
# held against the same person.
query G4 "read the actor id the record stored" \
  "select author->>'id' from comments where id = '$person_comment'"
readonly raw_actor="$answer"
[[ -n "$raw_actor" ]] || fail G4 "read the actor id the record stored" "the record names no actor"
request "person:$person" GET "/api/v1/people/$(q "$raw_actor")/nodes"
want_status 200 G4 "read the page of the raw actor id '$raw_actor'"
expect G4 "the raw actor id '$raw_actor' resolves to $person" "\"$person\"" '.person'

# G5: a comment that reaches nobody earns no tuple. It is written before any route is set on
# the issue, and read after G5b below, where a later comment is proven delivered: the count is
# 0 the moment the comment is written, so polling for 0 here would pass before the delivery
# path had run at all.
comment G5 "a comment that reaches nobody" agent "A note nobody is told about."
readonly quiet_comment="$comment"

# G5b: a delivery to a session. The issue's route names the session that claims no role, so every
# comment on it reaches that session over the bus and the record of what the batch delivered
# states the delivered_to tuple of it (outbox.deliveredGraph). The far end is an address and not
# a foreign key (R1), and a destination reached over the bus leaves no row in any table this
# child projects, so the session's own node is no part of what this delivery writes (R19): the
# node of a session comes from a comment it wrote or an attempt that settled sent against it.
request "person:$person" PATCH "/api/v1/issues/$key" \
  "$(jq -cn --arg r "session:$loose_session" '{route: $r}')"
want_status 200 G5 "route the issue to the session that claims no role"
comment G5 "a comment on the routed issue" "person:$person" \
  "A note for whoever holds this issue's route."
readonly loose_comment="$comment"
# This poll is the one a dead bus fails, and it fails looking like the store. A delivered_to
# tuple is written by the outbox publisher when it records what a batch delivered, so a server
# whose bus is missing, or whose publishes it refuses, delivers nothing and this count stays 0 -
# the same reading the store would give if it dropped a tuple it was handed. Measured twice:
# against a server started with no DISPATCH_E2E_NATS_URLS, and against a NATS without JetStream,
# whose publishes the server logs as refused. A route delivery leaves no row in any table this
# child projects, so there is nothing of the attempt to read here instead; what the driver can do
# is say so when the poll runs out, which the hint below does.
settles G5 "the store holds one delivered_to tuple of it" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$loose_comment'
     and kind = 'delivered_to' and to_kind = 'session' and to_id = '$loose_session' and asserted" \
  "A zero here is the bus before it is the store: the publisher writes this tuple only for a
     batch it delivered. Read the server's log for 'no NATS client' or 'jetstream not enabled',
     and start the server with DISPATCH_E2E_NATS_URLS naming a nats-server run with -js"
want_value G5 "the comment that reached nobody has no delivered_to tuple" 0 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$quiet_comment'
     and kind = 'delivered_to'"
want_value G5 "the delivery reached that session alone" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$loose_comment'
     and kind = 'delivered_to'"
want_value G5 "the tuple's time and causing write are the record's" "true|true" \
  "select (t.at >= c.created_at)::text || '|' || (t.source_event = e.id)::text
   from graph_tuples t
   join comments c on c.id::text = t.from_id
   join events e on e.type = 'comment.created' and e.payload->>'id' = c.id::text
   where t.from_kind = 'comment' and t.from_id = '$loose_comment' and t.kind = 'delivered_to'"

# G5c: a session that claimed no role carries its own page. A mention reaches its session through
# an attempt, and an attempt that settles sent is a row this child projects (comment_deliveries),
# so the record of what that batch delivered writes the session's node, its page and the page's
# part_of to it (outbox.deliveredGraph), and a session that claimed no role is part_of nothing.
# The mention names a session the listener holds with no role, and not the route's session, whose
# reach over the bus writes no node at all (read below).
comment G5 "a comment mentioning a session that claimed no role" agent \
  "A mention for a session with no role." "session:$mentioned_session"
readonly mention_comment="$comment"
settles G5 "the mention's attempt settles sent" "sent" \
  "select coalesce(max(state), 'none') from comment_deliveries where comment_id = '$mention_comment'"
settles G5 "the session that claimed no role carries its own page" 1 \
  "select count(*) from graph_nodes where kind = 'page' and id = 'session/$mentioned_session' and state = 'live'"
want_value G5 "the mentioned session is a node" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$mentioned_session' and state = 'live'"
want_value G5 "its page hangs under the session" 1 \
  "select count(*) from graph_tuples where from_kind = 'page' and from_id = 'session/$mentioned_session'
     and kind = 'part_of' and to_kind = 'session' and to_id = '$mentioned_session' and asserted"
want_value G5 "a session that claimed no role is part_of no role" 0 \
  "select count(*) from graph_tuples where from_kind = 'session' and from_id = '$mentioned_session'
     and kind = 'part_of'"
want_value G5 "the mention's delivery is a delivered_to tuple into that session" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$mention_comment'
     and kind = 'delivered_to' and to_kind = 'session' and to_id = '$mentioned_session' and asserted"

# G5d: an attempt the listener queued is a delivery. The mention names the session the listener
# holds stopped: it knows that session by its known-session record and no plugin runs it, so the
# send is stored for it and answered `state: stopped`, and Dispatch records the attempt `queued`
# (cybermonklabs/legion#2643). A queued attempt is one the listener took (model.DeliveryTaken), so
# its settle writes the session's node (graphstore.QueueReached) and the outbox's record of its receipt
# states the comment delivered to that session, once (outbox's mentionReached). Before #2643 the
# same send ended failed, wrote no node, and a fallback copy carried the comment instead.
#
# The person who owns the session would read the comment on their own page, but this fixture
# proves no owner of the stopped session: the alias_of tuple a delivery reaches a person through
# is written only by a write of the session itself, under a token the secrets broker scoped to a
# person, and a stopped session writes nothing. G13 has the session the broker proves a person's
# write, and holds its alias_of; TestAMentionQueuedForAStoppedSessionReachesItsPerson in
# packages/envoy carries the person's half of a queued delivery. What this driver reads here is
# the other half of the same rule: a session no credential proved is nobody's, so the comment its
# person wrote is on that person's page as authored and not as received.
comment G5 "a comment mentioning a session the listener holds stopped" "person:$person" \
  "A mention for a session that is not running." "session:$stopped_session"
readonly queued_comment="$comment"
settles G5 "the mention's attempt settles queued against the stopped session" "queued|$stopped_session" \
  "select coalesce(max(state || '|' || coalesce(session_id, '')), 'none') from comment_deliveries
   where comment_id = '$queued_comment'"
settles G5 "the comment states one delivered_to tuple into the stopped session" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$queued_comment'
     and kind = 'delivered_to' and to_kind = 'session' and to_id = '$stopped_session' and asserted"
want_value G5 "the stopped session is a node" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$stopped_session' and state = 'live'"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G5 "read $person's page after the queued delivery"
expect G5 "a delivery to a session no credential proved is on no person's page as received" \
  '["authored_by"]' --arg id "$queued_comment" '.rows[] | select(.id == $id) | .relations'

# Every session an attempt the listener took reached is a node: a sent attempt's, and a queued
# one's (model.DeliveryTaken, the test graphcensus's recordedSessions binds its session rows by).
# The read spans every run's attempts, so it is held to this run's own first: a read over no
# attempt of this issue reads zero for nothing, and one over no queued attempt proves nothing of
# the queued half.
query G5 "count this run's sent and queued attempts" \
  "select count(*) filter (where d.state = 'sent') || '|' || count(*) filter (where d.state = 'queued')
   from comment_deliveries d join comments c on c.id = d.comment_id
   where c.issue_key = '$key'"
sent_here="${answer%|*}"
queued_here="${answer#*|}"
[[ "$sent_here" -ge 1 && "$queued_here" -ge 1 ]] ||
  fail G5 "this run's issue holds a sent attempt and a queued one" \
    "read $sent_here sent and $queued_here queued attempt(s) on $key"
query G5 "count the sessions a sent or queued attempt reached" \
  "select count(distinct session_id) from comment_deliveries
   where state in ('sent', 'queued') and coalesce(session_id, '') <> ''"
taken_sessions="$answer"
want_value G5 "every session a sent or queued attempt reached is a node ($taken_sessions session(s); $sent_here sent and $queued_here queued attempt(s) on $key)" 0 \
  "select count(distinct d.session_id) from comment_deliveries d
   where d.state in ('sent', 'queued') and coalesce(d.session_id, '') <> ''
     and not exists (select 1 from graph_nodes n
                     where n.kind = 'session' and n.id = d.session_id and n.state = 'live')"
want_value G5 "a session no projected row names is no node of this writer's" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$loose_session'"

# G6: a session a credential merely claims earns no alias. The personal token proves its person
# and no session; the session is the one its body named.
request "person:$person" POST "/api/v1/me/agent-tokens" \
  "$(jq -cn --arg n "graph-store $stamp" '{name: $n}')"
want_status 201 G6 "mint a personal token for $person"
person_token="$(jq -er '.token' "$work/body")" || fail G6 "mint a personal token" "no token: $(body)"
claimed_session="019a1c09-2222-7000-8000-$(printf '%012d' "$$")"
readonly claimed_session
token_comment G6 "a personal token writes, naming a session it claims" \
  "$person_token" "$claimed_session" "A note a personal token wrote for $person."
readonly claimed_comment="$comment"
want_value G6 "the claimed session is the author" "session|$claimed_session" \
  "select to_kind || '|' || to_id from graph_tuples
   where from_kind = 'comment' and from_id = '$claimed_comment' and kind = 'authored_by'"
want_value G6 "a session an actor merely claims is nobody's" 0 \
  "select count(*) from graph_tuples where from_kind = 'session' and from_id = '$claimed_session'
     and kind = 'alias_of'"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G6 "read $person's page after the token's write"
expect G6 "what that token wrote is on no person's page" "0" \
  --arg id "$claimed_comment" '[.rows[] | select(.id == $id)] | length'

# G7: two personal tokens of one deployment, held by two people, stay apart.
request "person:$twin" POST "/api/v1/me/agent-tokens" \
  "$(jq -cn --arg n "graph-store twin $stamp" '{name: $n}')"
want_status 201 G7 "mint a personal token for $twin"
twin_token="$(jq -er '.token' "$work/body")" || fail G7 "mint a personal token" "no token: $(body)"
twin_session="019a1c09-3333-7000-8000-$(printf '%012d' "$$")"
readonly twin_session
token_comment G7 "$twin's token writes" "$twin_token" "$twin_session" \
  "A note $twin's own token wrote."
readonly twin_comment="$comment"
want_value G7 "the two tokens' writes are two sessions, not one" 2 \
  "select count(distinct t.to_id) from graph_tuples t
   where t.kind = 'authored_by' and t.from_kind = 'comment'
     and t.from_id in ('$claimed_comment', '$twin_comment')"
want_value G7 "the owner of each token is on neither write's authored_by tuple" 0 \
  "select count(*) from graph_tuples where kind = 'authored_by' and to_kind = 'person'
     and from_id in ('$claimed_comment', '$twin_comment')"
request "person:$twin" GET "/api/v1/people/$(q "$twin")/nodes"
want_status 200 G7 "read $twin's page"
expect G7 "what $person's token wrote reaches no page of $twin's" "0" \
  --arg id "$claimed_comment" '[.rows[] | select(.id == $id)] | length'
# Each token's person, read back through the server and held against the person this driver
# minted it as. A personal token alone writes no comment: a bearer caller names the session it
# acts for, and a write naming none is 400 ACTOR_KIND (api's bearerSessionActor), so each write
# above is authored by the session its body claims. The person the token proved is what the
# server answers that credential at `GET /api/v1/whoami`, and what the write's own record keeps
# as its actor's owner, in the comment's author and on its authored_by tuple alike.
for row in "$person_token|$person|$claimed_comment" "$twin_token|$twin|$twin_comment"; do
  held_token="${row%%|*}"
  written="${row##*|}"
  expected="${row#*|}"
  expected="${expected%|*}"
  request "token:$held_token" GET "/api/v1/whoami"
  want_status 200 G7 "read who the server takes $expected's token for"
  expect G7 "$expected's token resolves to $expected" "\"$expected\"" '.owner'
  want_value G7 "the write $expected's token made records $expected as its owner" "$expected|$expected" \
    "select (t.who->>'owner') || '|' || (c.author->>'owner') from graph_tuples t
     join comments c on c.id::text = t.from_id
     where t.from_kind = 'comment' and t.from_id = '$written' and t.kind = 'authored_by'"
done
want_value G7 "the two tokens answer two people, and each is a person node" "2|2" \
  "select count(distinct t.who->>'owner') || '|' || count(distinct n.id) from graph_tuples t
   left join graph_nodes n on n.kind = 'person' and n.id = t.who->>'owner'
   where t.kind = 'authored_by' and t.from_kind = 'comment'
     and t.from_id in ('$claimed_comment', '$twin_comment')"

# G8: an attempt that fails earns no tuple and no node. The mention names a session the listener
# does not hold, so its attempt settles failed and pins no session; the issue's own route still
# reaches its session, which is why this reads the tuples of the mentioned session alone. No other
# write of this run names the mentioned session, so no node of it may stand: a failed attempt
# reached nobody (graphstore.QueueReached). With no session pinned, the settle hands the store's
# writer none, so this read cannot see whether that writer passes over a failed attempt; G12 reads
# that for an attempt that pins its session.
dead_session="019a1c09-4444-7000-8000-$(printf '%012d' "$$")"
readonly dead_session
comment G8 "a comment mentioning a session the listener does not hold" agent \
  "A mention nobody can take." "session:$dead_session"
readonly failed_comment="$comment"
settles G8 "the attempt settles failed" "failed" \
  "select coalesce(max(state), 'none') from comment_deliveries where comment_id = '$failed_comment'"
want_value G8 "a failed attempt earns no tuple" 0 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$failed_comment'
     and kind = 'delivered_to' and to_id = '$dead_session'"
want_value G8 "no delivered_to tuple stands on an attempt the listener did not take" 0 \
  "select count(*) from graph_tuples t
   where t.kind = 'delivered_to' and t.to_kind = 'session'
     and exists (select 1 from comment_deliveries d
                 where d.comment_id::text = t.from_id and d.session_id = t.to_id)
     and not exists (select 1 from comment_deliveries d
                     where d.comment_id::text = t.from_id and d.session_id = t.to_id
                       and d.state in ('sent', 'queued'))"
want_value G8 "a failed attempt earns no node" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$dead_session'"

# G9: a delivery to a role ends at the role node and not on a person; the role carries its page
# and its held_by claim, and the session it resolved to is part_of that role. The outbox resolves
# a role route's holder from the bus (roles.Buckets), so the fake Envoy claims the role there
# first, for the live holder its own lookup names.
claim_role G9 "$holder_session" ""
request "person:$person" PATCH "/api/v1/issues/$key" \
  "$(jq -cn --arg r "role:$role" '{route: $r}')"
want_status 200 G9 "route the issue to the role"
query G9 "read the event that routed $key to the role" \
  "select coalesce(max(id), 0) from events where issue_key = '$key' and type = 'issue.updated'
     and payload->>'route' = 'role:$role'"
readonly role_route_event="$answer"
[[ "$role_route_event" != 0 ]] ||
  fail G9 "read the event that routed $key to the role" "no issue.updated event routes it to role:$role"
comment G9 "a comment on the issue the role holds" "person:$person" \
  "A note for whoever holds $role."
readonly role_comment="$comment"
settles G9 "the delivery ends at the role node" "role|$role" \
  "select coalesce(max(to_kind || '|' || to_id), 'none') from graph_tuples
   where from_kind = 'comment' and from_id = '$role_comment' and kind = 'delivered_to'"
want_value G9 "the delivery reached no person and no session" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$role_comment'
     and kind = 'delivered_to'"
# The write that routed the issue is the first event to name the role, and a person's write is
# delivered to the route it sets, so the batch that recorded it resolved the holder through the
# claim: the role's held_by and its holder's part_of are that event's, and the comment after it
# restates the same claim and writes nothing of it again (graphstore.Claim). The comment's own
# delivery, settled above, comes after that record, so each read below is of a record that
# exists. The holder was reached over the bus, which no table this child projects records, so it
# is no node and carries no page of its own (outbox.deliveredGraph, R19).
want_value G9 "the role is held by the session the claim names, from the write that routed the issue" \
  "true|session|$holder_session|$role_route_event" \
  "select string_agg(asserted || '|' || to_kind || '|' || to_id || '|' || source_event, ' ' order by seq)
   from graph_tuples where from_kind = 'role' and from_id = '$role' and kind = 'held_by'"
want_value G9 "the holder is part_of the role it claimed, from that same write" \
  "true|$role_route_event" \
  "select string_agg(asserted || '|' || source_event, ' ' order by seq) from graph_tuples
   where from_kind = 'session' and from_id = '$holder_session' and kind = 'part_of'
     and to_kind = 'role' and to_id = '$role'"
want_value G9 "a session holding a role is no node and carries no page of its own" 0 \
  "select count(*) from graph_nodes
   where (kind = 'session' and id = '$holder_session') or (kind = 'page' and id = 'session/$holder_session')"
want_value G9 "the role's page hangs under the role" 1 \
  "select count(*) from graph_tuples where from_kind = 'page' and from_id = 'role/$role'
     and kind = 'part_of' and to_kind = 'role' and to_id = '$role' and asserted"
want_value G9 "the role and its page are nodes" 2 \
  "select count(*) from graph_nodes
   where (kind = 'role' and id = '$role') or (kind = 'page' and id = 'role/$role')"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G9 "read $person's page after the role delivery"
# The person wrote this comment, so it is on their page as theirs; what must not be there is a
# received row, because the delivery ended at the role and reached no person.
expect G9 "a delivery to a role is on no person's page as received" '["authored_by"]' \
  --arg id "$role_comment" '.rows[] | select(.id == $id) | .relations'

# The retraction. The first holder stops answering the fake Envoy's lookup, which then names the
# second, and the claim on the bus moves with it, as a listener moves a role whose holder another
# session took it from. The next forward resolves the second holder: the record of that batch
# ends the first holder's claim and asserts the second's, both under the role-routed comment the
# batch resolved it for, and the first claim stays in the history, since the store appends only.
curl -sS -o "$work/first-holder.json" -X PATCH -H 'Content-Type: application/json' \
  --data-binary '{"live": false}' "$envoy/__fixture/sessions/$holder_session" 2>"$work/curl" ||
  fail G9 "take $holder_session off the listener's live sessions" "curl: $(<"$work/curl")"
jq -e '.live == false' "$work/first-holder.json" >/dev/null ||
  fail G9 "take $holder_session off the listener's live sessions" "$(head -c 300 "$work/first-holder.json")"
claim_role G9 "$second_holder_session" "$holder_session"
comment G9 "a comment the role's second holder is forwarded" "person:$person" \
  "Another note for whoever holds $role."
readonly second_role_comment="$comment"
settles G9 "the second delivery ends at the role node" "role|$role" \
  "select coalesce(max(to_kind || '|' || to_id), 'none') from graph_tuples
   where from_kind = 'comment' and from_id = '$second_role_comment' and kind = 'delivered_to'"
query G9 "read the second comment's event" \
  "select coalesce(max(id), 0) from events where type = 'comment.created' and payload->>'id' = '$second_role_comment'"
readonly second_role_event="$answer"
want_value G9 "the first claim, its retraction and the second claim stand in the history, in that order" \
  "true|session|$holder_session|$role_route_event false|session|$holder_session|$second_role_event true|session|$second_holder_session|$second_role_event" \
  "select string_agg(asserted || '|' || to_kind || '|' || to_id || '|' || source_event, ' ' order by seq)
   from graph_tuples where from_kind = 'role' and from_id = '$role' and kind = 'held_by'"
want_value G9 "the role's current holder is the second" "$second_holder_session" \
  "select coalesce(string_agg(to_id, ' '), '') from (
     select distinct on (to_kind, to_id) to_id, asserted from graph_tuples
     where from_kind = 'role' and from_id = '$role' and kind = 'held_by'
     order by to_kind, to_id, seq desc) latest
   where asserted"
want_value G9 "the second holder is part_of the role, from the forward that resolved it" \
  "true|$second_role_event" \
  "select string_agg(asserted || '|' || source_event, ' ' order by seq) from graph_tuples
   where from_kind = 'session' and from_id = '$second_holder_session' and kind = 'part_of'
     and to_kind = 'role' and to_id = '$role'"
# A session is part_of the role it claimed while it ran, so the first holder's stands.
want_value G9 "the first holder's part_of stands after its claim ended" "true|$role_route_event" \
  "select string_agg(asserted || '|' || source_event, ' ' order by seq) from graph_tuples
   where from_kind = 'session' and from_id = '$holder_session' and kind = 'part_of'
     and to_kind = 'role' and to_id = '$role'"

# G10: a message is a node of its project and earns no delivery or authorship tuple in this child.
# Its create writes its node before its row, in the issue's project, and the message.created event
# causes that node and its three base attributes: its visibility, its state and its project. Only
# a comment has a node on the near end of a delivery today (outbox.deliveredItem), so the record
# of what a batch delivered appends no delivered_to tuple for a message, and no writer appends its
# authored_by. What else a message on a role-routed issue has in the store is that role, its page
# and the page's part_of: the outbox writes a role and its page for every role an event names, and
# the first event to name this one is the write that routed the issue to it, so that write is the
# cause the store records.
#
# The message is on an issue of its own, routed to a role of this run's own, so the role it
# reaches is named by that route and that message alone. It names no target: a targeted message
# reaches its target through an attempt of its own and not through the route.
send POST "/api/v1/issues" "$(jq -cn --arg p "$project" --arg s "$session" \
  '{project: $p, title: "A message on a role-routed issue", spec: "What a message earns in the store.", actor: {kind: "session", id: $s}}')"
want_status 201 G10 "create the issue the message is posted on"
message_key="$(jq -er '.key' "$work/body")" || fail G10 "create the issue the message is posted on" "no key: $(body)"
send PATCH "/api/v1/issues/$message_key" \
  "$(jq -cn --arg r "role:$message_role" --arg s "$session" '{route: $r, actor: {kind: "session", id: $s}}')"
want_status 200 G10 "route $message_key to a role only the message reaches"
query G10 "read the event that routed $message_key" \
  "select coalesce(max(id), 0) from events where issue_key = '$message_key' and type = 'issue.updated'
     and payload->>'route' = 'role:$message_role'"
readonly route_event="$answer"
[[ "$route_event" != 0 ]] || fail G10 "read the event that routed $message_key" "no issue.updated event routes it to role:$message_role"
request "person:$person" POST "/api/v1/issues/$message_key/messages" \
  "$(jq -cn --arg b "A message for whoever holds $message_role." '{body: $b}')"
want_status 201 G10 "post a message on $message_key"
message="$(jq -er '.id' "$work/body")" || fail G10 "post a message on $message_key" "no message id: $(body)"
readonly message
query G10 "read the message's event" \
  "select coalesce(max(id), 0) from events where type = 'message.created' and payload->>'id' = '$message'"
readonly message_event="$answer"
[[ "$message_event" != 0 ]] || fail G10 "read the message's event" "no message.created event names $message"
pass G10 "posted message $message (event $message_event) on $message_key, routed by event $route_event"
# The outbox records what a batch delivered in the statement that marks its events published
# (outbox.recordBatch), so once the message's event reads published to the role's topic, every
# tuple its delivery would write is written, and each read below is of a record that exists.
settles G10 "the outbox delivered the message to the role" "true" \
  "select coalesce((select (published_at is not null
                            and 'notifications.role.$message_role' = any(published_destinations))::text
                     from events where id = $message_event), 'none')"
want_value G10 "the role and its page are nodes, made by the write that routed the issue" 2 \
  "select count(*) from graph_nodes
   where ((kind = 'role' and id = '$message_role') or (kind = 'page' and id = 'role/$message_role'))
     and state = 'live' and source_event = $route_event"
want_value G10 "the role's page hangs under the role" 1 \
  "select count(*) from graph_tuples where from_kind = 'page' and from_id = 'role/$message_role'
     and kind = 'part_of' and to_kind = 'role' and to_id = '$message_role' and asserted
     and source_event = $route_event"
# The reads of the message, each beside a read of something: the route's event caused the role's
# and the page's tuples, and the comment G9 delivered to a role stands in tuples, so the message's
# own node and base attributes, and nothing more, are what its event caused, and a zero for any
# other tuple on the message is the message earning nothing more and not a read that matched
# nothing.
query G10 "count the tuples the route's event caused" \
  "select count(*) from graph_tuples where source_event = $route_event"
[[ "$answer" -gt 0 ]] ||
  fail G10 "the route's event caused the role's tuples" "no tuple names event $route_event as its cause"
route_caused="$answer"
want_value G10 "the message is a node of $project, live, made by its own write" 1 \
  "select count(*) from graph_nodes where kind = 'message' and id = '$message'
     and project_key = '$project' and parties is null and state = 'live' and source_event = $message_event"
want_value G10 "the message's event caused its node and the node's visibility, state and project (the route's caused $route_caused tuple(s))" "1|3|3" \
  "select (select count(*) from graph_nodes where source_event = $message_event) || '|'
          || (select count(*) from graph_tuples where source_event = $message_event) || '|'
          || (select count(*) from graph_tuples where source_event = $message_event
                and from_kind = 'message' and from_id = '$message' and to_kind = 'value' and asserted
                and kind in ('visibility', 'state', 'project'))"
query G10 "count the tuples that stand on the comment G9 delivered" \
  "select count(*) from graph_tuples where from_id = '$role_comment' or to_id = '$role_comment'"
[[ "$answer" -gt 0 ]] ||
  fail G10 "the comment G9 delivered stands in tuples" "no tuple names $role_comment"
comment_tuples="$answer"
want_value G10 "no delivered_to, authored_by or other tuple but its base attributes stands on the message (the comment G9 delivered stands in $comment_tuples)" 0 \
  "select count(*) from graph_tuples where (from_id = '$message' or to_id = '$message')
     and kind not in ('visibility', 'state', 'project', 'parties')"

# G11: an ask a session follows records that session, and a session the store never recorded
# answers no node at all, read through the server's own node reader. An ask's followers are the
# one edge of graph_edges whose far end is a session. A session that follows through the API is a
# node from that follow on (R14: "an ask a session opened or follows"), the one that never wrote
# anything among them; a follower row written as the release before the store writes one, with no
# node, is the session the store never saw, and the read leaves it out and writes no node for it.
send POST "/api/v1/issues/$key/asks" "$(jq -cn --arg s "$session" \
  '{question: "Which sessions does the store hold?", actor: {kind: "session", id: $s}}')"
want_status 201 G11 "open an ask"
ask="$(jq -er '.id' "$work/body")" || fail G11 "open an ask" "no ask id: $(body)"
followed_session="019a1c09-8888-7000-8000-$(printf '%012d' "$$")"
unseen_session="019a1c09-5555-7000-8000-$(printf '%012d' "$$")"
readonly followed_session unseen_session
for follower in "$session" "$followed_session"; do
  request "person:$person" PUT "/api/v1/asks/$ask/followers/$follower"
  [[ "$status" == 200 || "$status" == 201 || "$status" == 204 ]] ||
    fail G11 "follow the ask as $follower" "HTTP $status: $(body)"
done
pass G11 "both sessions follow the ask"
want_value G11 "the store holds a node of the session that wrote" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$session'"
want_value G11 "the store holds a node of the session that only follows" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$followed_session'"
query G11 "write a follower row as the release before the store writes one" \
  "insert into ask_followers (ask_id, session_id) values ('$ask', '$unseen_session')"
want_value G11 "the store holds no node of the session it never recorded" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$unseen_session'"
get "/api/v1/references?from=$(q "dispatch://$key/ask/$ask")&kind=followed_by"
want_status 200 G11 "read the ask's followers through the graph"
for recorded in "$session" "$followed_session"; do
  expect G11 "the recorded session $recorded answers a node" "[\"$recorded\"]" \
    --arg id "$recorded" '[.edges[] | select(.node.id == $id) | .node.id]'
done
expect G11 "a session the store never recorded answers no node at all" "[]" \
  --arg id "$unseen_session" '[.edges[] | select(.node.id == $id) | .node.id]'
want_value G11 "the read wrote no node" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$unseen_session'"
# The row stood in for one the release before the store wrote, which only the backfill gives a
# node. This run backfills nothing, so the row goes once it is read: left behind, it would stand in
# the server's database as a session the store census holds the store to a node of, and every
# later census over that database, another driver's among them, would read it as a difference.
query G11 "remove the row written as the release before the store writes one" \
  "delete from ask_followers where ask_id = '$ask' and session_id = '$unseen_session'"
want_value G11 "the row the release before the store wrote is gone" 0 \
  "select count(*) from ask_followers where ask_id = '$ask' and session_id = '$unseen_session'"

# G12: the answer to an attempt the listener had not taken. The mention names the session whose
# every send the listener refuses, so its attempt settles failed against that session: the
# listener took nothing, and the record of its receipt states no delivery (outbox's
# mentionReached). The session then answers that very attempt through the reply route. The reply
# is a comment of its own, stored by its comment.answered event, and the answer flips the attempt
# to sent (api's replyComment). That flip is what makes the attempt a delivery, so the answer's
# own write states it, under the answering session and its comment.answered event (api's
# queueAnsweredDelivery): the one writer of a delivered_to beside the outbox. The attempt pins
# its session, as G8's does not, so its settle hands that session to the store's writer
# (graphstore.QueueReached), which passes over a failed attempt: the session is no node until the
# answer records it.
comment G12 "a comment mentioning a session whose every send the listener refuses" agent \
  "A mention the listener will not carry." "session:$answering_session"
readonly unanswered_comment="$comment"
settles G12 "the mention's attempt settles failed against that session" "failed|$answering_session" \
  "select coalesce(max(state || '|' || coalesce(session_id, '')), 'none') from comment_deliveries
   where comment_id = '$unanswered_comment'"
want_value G12 "the failed attempt earns its session no node before the answer" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$answering_session'"
query G12 "read the failed attempt's number" \
  "select max(attempt) from comment_deliveries where comment_id = '$unanswered_comment'"
readonly answered_attempt="$answer"
want_value G12 "the failed attempt earns no tuple" 0 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$unanswered_comment'
     and kind = 'delivered_to'"
send POST "/api/v1/comments/$unanswered_comment/reply" \
  "$(jq -cn --arg s "$answering_session" --argjson a "$answered_attempt" \
    '{body: "The answer to a mention the listener never carried.", attempt: $a, actor: {kind: "session", id: $s}}')"
want_status 201 G12 "the session answers the attempt the listener had not taken"
reply="$(jq -er '.id' "$work/body")" || fail G12 "the session answers the attempt" "no reply id: $(body)"
readonly reply
pass G12 "$answering_session answers attempt $answered_attempt with the reply $reply"
want_value G12 "the answer reads the attempt sent, answered by the reply" "sent|$reply" \
  "select state || '|' || coalesce(reply_id::text, '') from comment_deliveries
   where comment_id = '$unanswered_comment' and attempt = $answered_attempt"
query G12 "read the reply's event" \
  "select coalesce(max(id), 0) from events where type = 'comment.answered' and payload->>'id' = '$reply'"
readonly reply_event="$answer"
[[ "$reply_event" != 0 ]] || fail G12 "read the reply's event" "no comment.answered event names $reply"
want_value G12 "the reply is authored by the answering session, under the comment.answered event that stored it" \
  "session|$answering_session|$reply_event" \
  "select string_agg(to_kind || '|' || to_id || '|' || source_event, ' ') from graph_tuples
   where from_kind = 'comment' and from_id = '$reply' and kind = 'authored_by'"
want_value G12 "the answer states the comment delivered to the answering session, by that session under its comment.answered event" \
  "session|$answering_session|$reply_event" \
  "select string_agg((who->>'kind') || '|' || (who->>'id') || '|' || source_event, ' ') from graph_tuples
   where from_kind = 'comment' and from_id = '$unanswered_comment' and kind = 'delivered_to'
     and to_kind = 'session' and to_id = '$answering_session' and asserted"

# G13: a session the secrets broker proved a person's is an alias of that person. The token is the
# harness's fake broker's (e2e/fake-broker-token.ts): it names the proved session and the person
# it acts for under the project scope `person`, and it is bound to a key broker-session.ts makes
# here, which signs a fresh DPoP proof for every request presenting it. The session's comment is
# that session's own, and the same write ties the session to its person by an alias_of tuple
# (graphstore's aliases), the identity table's proved row, which is read back both as the server
# answers who the credential is and as the store's alias names that person.
bun "$broker_session" mint "$work/broker.key" "$proved_session" "$person" "$project" \
  >"$work/broker.token" 2>"$work/curl" ||
  fail G13 "mint a token of the fake broker for $proved_session" "$(<"$work/curl")"
pass G13 "minted a token of the fake broker naming $proved_session, acting for $person"
proved_spelling="${proved_identity%%|*}"
proved_person="${proved_identity##*|}"
request broker GET "/api/v1/whoami"
want_status 200 G13 "read who the server takes the proved session's credential for"
expect G13 "the credential is the session '$proved_spelling', acting for $proved_person" \
  "{\"owner\":\"$proved_person\",\"session\":\"$proved_spelling\"}" '{owner, session}'
comment G13 "the proved session writes a comment" broker "A note the session the broker proved wrote."
readonly proved_comment="$comment"
want_value G13 "the comment is authored by the proved session, not by its person" "session|$proved_spelling" \
  "select string_agg(to_kind || '|' || to_id, ' ') from graph_tuples
   where from_kind = 'comment' and from_id = '$proved_comment' and kind = 'authored_by'"
want_value G13 "'$proved_spelling' is an alias of $proved_person" "person|$proved_person" \
  "select string_agg(to_kind || '|' || to_id, ' ') from graph_tuples
   where from_kind = 'session' and from_id = '$proved_spelling' and kind = 'alias_of' and asserted"
want_value G13 "one person node answers the person the alias names" 1 \
  "select count(*) from graph_nodes where kind = 'person' and id = '$proved_person'"
request "person:$person" GET "/api/v1/people/$(q "$person")/nodes"
want_status 200 G13 "read $person's page after the proved session's write"
expect G13 "what the proved session wrote stays that session's and is on no person's page" "0" \
  --arg id "$proved_comment" '[.rows[] | select(.id == $id)] | length'

# G14: every tuple standing on this run's nodes, of every kind, carries the actor and the event of
# the write that appended it and a time its record allows. Each kind is held to the write that
# appends it, which this driver states here rather than reads from the store, so a writer that
# records a tuple under another actor, another event or an earlier time than its write fails here
# even where every read of the store agrees with it; and each kind it holds holds at least one
# tuple of this run (tuples_carry).
#
#   - authored_by is the comment's own write (graphstore.QueueAuthored): the comment's author, the
#     event that stored the comment by that author - comment.created, or comment.answered for a
#     reply (G12) - and the comment's own time, each equal to the record's.
#   - alias_of is appended by that same write beside it (graphstore's aliases): the actor of the
#     event that stored the comment, that event, and a time no earlier than the comment (G13).
#   - delivered_to, a page's part_of, a session's part_of the role it claimed and a role's held_by
#     are the outbox's record of what a batch delivered (outbox.deliveredGraph), which the store
#     records as written by the outbox itself (outbox's graphWriter): that a thing reached a
#     session is the server's fact and not an act of whoever wrote the thing. So the actor each
#     carries is that writer, $outbox_writer below, and never its causing event's, at no time
#     earlier than that event. The event is the one the record states: for a delivery, any
#     comment.* event whose payload names the comment and which was published to the session or
#     the role the tuple names, as the outbox records a delivery of every such event (outbox's
#     deliveredItem), or the comment.delivery receipt of an attempt the listener took against
#     that session; for a session's page, the receipt that recorded the session; for a role's
#     page, the event that first named the role, as the first record to name a role writes its
#     page once; and for a claim - a role's held_by, asserted or retracted, and a session's
#     part_of that role - the first event of its record published to the role, the one that
#     record resolved the holder for.
#   - delivered_to has one other writer: the answer to an attempt the listener had not taken (api's
#     queueAnsweredDelivery), which states the delivery its answer makes (G12). That tuple carries
#     the answering session, the actor of the answer's comment.answered event, and that event,
#     which stored the reply the attempt of that session to that comment records. An attempt whose
#     receipt says the listener took it before the answer earns its tuple from the outbox, so an
#     answer to it states none. A delivered_to under any other actor, or under one of these two
#     actors with the other's event, fails.
#   - visibility and state, the base attributes, are stated by the write that made the node or
#     moved them, under that write's writer, event and time (graphstore's queueNodes), as is a
#     comment's project, the third base attribute this run's nodes carry: a comment's by its own
#     write at its own time; a person's and a session's that wrote by the comment that
#     first named them, at that comment's time; a session's an attempt reached by the settle of
#     that attempt, at the attempt's time (graphstore.QueueReached), or by an answer it gave, at the
#     answer's (api's queueAnsweredDelivery); a follower's by the follow, no earlier than it
#     (asks.Follow); and a role's, a page's, and a session's a copy of a comment reached, by the
#     outbox, under the same events its claims are stated under, at no earlier time.
#
# No tuple of another kind may stand on this run's comments, sessions, roles and pages. A person
# node is the one node of this run that other writes share - a login's node is that login's
# whatever wrote it - so of a person this driver holds its base attributes, and the tuples into
# it from the far end.
readonly outbox_writer='{"kind": "system", "id": "dispatch-outbox"}'
# The nodes of this run, as SQL lists: its sessions, its two roles and their pages, its two
# people, and the issues whose comments are its own.
run_sessions=("$session" "$loose_session" "$holder_session" "$second_holder_session"
  "$mentioned_session" "$stopped_session" "$answering_session" "$proved_session"
  "$claimed_session" "$twin_session" "$dead_session" "$followed_session" "$unseen_session")
sessions_sql="$(printf "'%s'," "${run_sessions[@]}")"
readonly sessions_sql="${sessions_sql%,}"
roles_sql="$(printf "'%s'," "$role" "$message_role")"
readonly roles_sql="${roles_sql%,}"
pages_sql="$(printf "'session/%s'," "${run_sessions[@]}")$(printf "'role/%s'," "$role" "$message_role")"
readonly pages_sql="${pages_sql%,}"
readonly persons_sql="'$person', '$twin'"
readonly issues_sql="'$key', '$message_key'"
# Whether the tuple t stands on a node of this run, an SQL predicate.
readonly run_nodes="((t.from_kind = 'comment'
      and t.from_id in (select id::text from comments where issue_key in ($issues_sql)))
   or (t.from_kind = 'person' and t.from_id in ($persons_sql))
   or (t.from_kind = 'session' and t.from_id in ($sessions_sql))
   or (t.from_kind = 'role' and t.from_id in ($roles_sql))
   or (t.from_kind = 'page' and t.from_id in ($pages_sql)))"

# first_naming ROLE: the id of the first event that named the role ROLE, an SQL expression: one
# published to the role's topic, or one whose payload addresses it, as the outbox reads an event's
# roles (outbox's addressedRoles). The record of that event is the first to write the role, its
# page and the page's part_of, and each is written once.
first_naming() {
  printf '%s' "(select min(n.id) from events n
     where 'notifications.role.' || $1 = any(n.published_destinations)
        or (n.type in ('issue.created', 'issue.updated', 'issue.closed')
            and n.payload->>'route' = 'role:' || $1)
        or (n.type in ('message.created', 'message.answered') and n.payload->>'target' = 'role:' || $1)
        or (n.type = 'comment.created'
            and n.payload->'mentions' @> jsonb_build_array(jsonb_build_object('target', 'role:' || $1))))"
}
# first_routed EVENT ROLE: whether the event EVENT is the first event of its record published to
# the role ROLE, an SQL predicate: the one the record resolved the role's holder for, whose claim
# it states (outbox.deliveredGraph). Every event a record marks published carries that record's
# one published_at (outbox.recordBatch).
first_routed() {
  printf '%s' "('notifications.role.' || $2 = any($1.published_destinations)
     and not exists (select 1 from events n
                     where n.id < $1.id and n.published_at = $1.published_at
                       and 'notifications.role.' || $2 = any(n.published_destinations)))"
}
# taken_receipt EVENT SESSION: whether the event EVENT is the comment.delivery receipt of an
# attempt the listener took against the session SESSION, an SQL predicate (outbox's
# mentionReached).
taken_receipt() {
  printf '%s' "($1.type = 'comment.delivery' and btrim($1.payload->>'session_id') = $2
     and $1.payload->>'state' in ('sent', 'queued'))"
}

tuples_carry G14 "every authored_by tuple of this run carries its comment's author, the event that stored it and its time" \
  "select format('comment:%s authored_by %s:%s', t.from_id, t.to_kind, t.to_id) as tuple,
          t.who, t.source_event, t.at, c.author as want_who,
          e.id is not null as caused, 'the ' || stored.kind || ' event of the comment, by its author' as cause,
          t.at = c.created_at as timely, c.created_at::text || ', the time of the comment' as when_want
   from graph_tuples t
   join comments c on c.id::text = t.from_id
   cross join lateral (select case when exists (select 1 from comment_deliveries d where d.reply_id = c.id)
                                   then 'comment.answered' else 'comment.created' end as kind) stored
   left join events e on e.id = t.source_event and e.type = stored.kind
     and e.payload->>'id' = t.from_id and e.actor = c.author
   where t.kind = 'authored_by' and $run_nodes"
tuples_carry G14 "every delivered_to tuple of this run carries the outbox's writer or the answering session, and the event that delivered it" \
  "select format('comment:%s delivered_to %s:%s', t.from_id, t.to_kind, t.to_id) as tuple,
          t.who, t.source_event, t.at,
          case when pair.answer then e.actor else '$outbox_writer'::jsonb end as want_who,
          coalesce(pair.outbox or pair.answer, false) as caused,
          'a comment.* event of the comment published to that ' || t.to_kind
            || ', the receipt of an attempt the listener took against that session, or the'
            || ' comment.answered event of an answer to an attempt of that session it had not taken' as cause,
          t.at >= greatest(c.created_at, e.created_at) as timely,
          'no earlier than ' || greatest(c.created_at, e.created_at)::text as when_want
   from graph_tuples t
   join comments c on c.id::text = t.from_id
   left join events e on e.id = t.source_event
   cross join lateral (select
     (e.type like 'comment.%' and e.payload->>'id' = t.from_id
      and case t.to_kind when 'session' then 'notifications.agent.' when 'role' then 'notifications.role.' end
          || t.to_id = any(e.published_destinations))
     or (t.to_kind = 'session' and e.payload->>'comment_id' = t.from_id and $(taken_receipt e t.to_id))
       as outbox,
     e.type = 'comment.answered' and t.to_kind = 'session' and e.actor->>'kind' = 'session'
       and exists (select 1 from comment_deliveries d
                   where d.comment_id::text = t.from_id and d.reply_id::text = e.payload->>'id'
                     and d.session_id = e.actor->>'id' and btrim(d.session_id) = t.to_id
                     and not exists (select 1 from events r
                                     where r.type = 'comment.delivery' and r.id < e.id
                                       and r.payload->>'comment_id' = t.from_id
                                       and r.payload->>'target' = d.target
                                       and r.payload->>'attempt' = d.attempt::text
                                       and r.payload->>'state' in ('sent', 'queued')))
       as answer) pair
   where t.kind = 'delivered_to' and $run_nodes"
tuples_carry G14 "every part_of tuple of this run's sessions and pages carries the outbox's writer and the event that recorded it" \
  "select format('%s:%s part_of %s:%s%s', t.from_kind, t.from_id, t.to_kind, t.to_id,
                 case when t.asserted then '' else ' (retracted)' end) as tuple,
          t.who, t.source_event, t.at, '$outbox_writer'::jsonb as want_who,
          coalesce(case
            when t.from_kind = 'page' and t.to_kind = 'session' and t.from_id = 'session/' || t.to_id
              then $(taken_receipt e t.to_id)
            when t.from_kind = 'page' and t.to_kind = 'role' and t.from_id = 'role/' || t.to_id
              then e.id = $(first_naming t.to_id)
            when t.from_kind = 'session' and t.to_kind = 'role' then $(first_routed e t.to_id)
          end, false) as caused,
          case when t.from_kind = 'session' then 'the first event of its record published to the role'
               when t.to_kind = 'role' then 'the event that first named the role'
               else 'the receipt of an attempt the listener took against the session' end as cause,
          t.at >= e.created_at as timely,
          'no earlier than ' || coalesce(e.created_at::text, 'its event') as when_want
   from graph_tuples t
   left join events e on e.id = t.source_event
   where t.kind = 'part_of' and $run_nodes"
tuples_carry G14 "every held_by tuple of this run's roles, asserted or retracted, carries the outbox's writer and the event that resolved it" \
  "select format('role:%s held_by %s:%s%s', t.from_id, t.to_kind, t.to_id,
                 case when t.asserted then '' else ' (retracted)' end) as tuple,
          t.who, t.source_event, t.at, '$outbox_writer'::jsonb as want_who,
          coalesce($(first_routed e t.from_id), false) as caused,
          'the first event of its record published to the role' as cause,
          t.at >= e.created_at as timely,
          'no earlier than ' || coalesce(e.created_at::text, 'its event') as when_want
   from graph_tuples t
   left join events e on e.id = t.source_event
   where t.kind = 'held_by' and $run_nodes"
tuples_carry G14 "every alias_of tuple of this run's sessions carries the actor and the event of the write that proved it" \
  "select format('session:%s alias_of %s:%s', t.from_id, t.to_kind, t.to_id) as tuple,
          t.who, t.source_event, t.at, e.actor as want_who,
          c.id is not null as caused,
          'the event that stored a comment whose credential proved the session' as cause,
          t.at >= c.created_at as timely,
          'no earlier than ' || coalesce(c.created_at::text, 'the comment') as when_want
   from graph_tuples t
   left join events e on e.id = t.source_event and e.type in ('comment.created', 'comment.answered')
   left join comments c on c.id::text = e.payload->>'id' and c.author = e.actor
     and t.from_id in (c.author->>'id', c.author->'act'->>'session')
   where t.kind = 'alias_of' and $run_nodes"
for attribute in visibility state project; do
  tuples_carry G14 "every $attribute tuple of this run's nodes carries the writer, the event and the time of the write that made or moved it" \
    "select format('%s:%s %s %s', t.from_kind, t.from_id, t.kind, t.to_id) as tuple,
            t.who, t.source_event, t.at,
            case when w.write in ('outbox', 'copy') then '$outbox_writer'::jsonb else e.actor end as want_who,
            w.write is not null as caused,
            case t.from_kind
              when 'comment' then 'the event that stored the comment, by its author'
              when 'person' then 'the event of a comment its author wrote as that person'
              when 'session' then 'the event of a comment the session wrote or answered, a receipt of an'
                || ' attempt the listener took against it, a follow of it, or a copy of a comment published to it'
              when 'role' then 'the event that first named the role'
              else 'the event that first named its role, or the receipt of an attempt the listener took'
                || ' against its session' end as cause,
            case w.write
              when 'comment' then t.at = own.created_at
              when 'author' then t.at = stored.created_at
              when 'answer' then t.at = e.created_at
              when 'settle' then t.at = attempt.created_at
              else t.at >= e.created_at end as timely,
            case w.write
              when 'comment' then own.created_at::text || ', the time of the comment'
              when 'author' then stored.created_at::text || ', the time of the comment'
              when 'answer' then e.created_at::text || ', the time of the answer'
              when 'settle' then attempt.created_at::text || ', the time of the attempt'
              else 'no earlier than ' || coalesce(e.created_at::text, 'its event') end as when_want
     from graph_tuples t
     left join events e on e.id = t.source_event
     left join comments own on t.from_kind = 'comment' and own.id::text = t.from_id
     left join comments stored on e.type in ('comment.created', 'comment.answered')
       and stored.id::text = e.payload->>'id'
     left join comment_deliveries attempt on e.type = 'comment.delivery'
       and attempt.comment_id::text = e.payload->>'comment_id' and attempt.target = e.payload->>'target'
       and attempt.attempt::text = e.payload->>'attempt'
     cross join lateral (select case
       when t.from_kind = 'comment' then
         case when e.payload->>'id' = t.from_id and stored.author = e.actor then 'comment' end
       when t.from_kind = 'person' then
         case when stored.author = e.actor and t.from_id in (
                case when e.actor->>'kind' = 'user' then lower(btrim(e.actor->>'id')) end,
                lower(btrim(e.actor->>'owner'))) then 'author' end
       when t.from_kind = 'session' then
         case
           when stored.author = e.actor and e.actor->>'kind' = 'session' and e.actor->>'id' = t.from_id
             then 'author'
           when e.type = 'comment.answered' and exists (select 1 from comment_deliveries d
                                                        where d.reply_id = stored.id
                                                          and btrim(d.session_id) = t.from_id)
             then 'answer'
           when $(taken_receipt e t.from_id) then 'settle'
           when e.type = 'ask.follower_added' and e.payload->>'session_id' = t.from_id then 'follow'
           when e.type like 'comment.%' and 'notifications.agent.' || t.from_id = any(e.published_destinations)
             then 'copy'
         end
       when t.from_kind = 'role' then case when e.id = $(first_naming t.from_id) then 'outbox' end
       when t.from_kind = 'page' and t.from_id like 'role/%' then
         case when e.id = $(first_naming "substr(t.from_id, 6)") then 'outbox' end
       when t.from_kind = 'page' then
         case when $(taken_receipt e "substr(t.from_id, 9)") then 'outbox' end
     end as write) w
     where t.kind = '$attribute' and t.to_kind = 'value' and $run_nodes"
done
want_value G14 "no tuple of a kind this driver holds no rule for stands on this run's comments, sessions, roles and pages" 0 \
  "select count(*) || coalesce(': ' || string_agg(distinct t.from_kind || ' ' || t.kind, ', '), '')
   from graph_tuples t
   where $run_nodes and t.from_kind <> 'person'
     and t.kind not in ('authored_by', 'delivered_to', 'part_of', 'held_by', 'alias_of', 'visibility', 'state', 'project')"

printf 'graph-store.sh: every check passed\n'
