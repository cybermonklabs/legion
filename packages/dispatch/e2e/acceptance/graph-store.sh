#!/usr/bin/env bash
# Drives the people and the agents of the node and tuple store end to end against a running
# server (dispatch://INFRA-1149's c-person-graph). It writes comments, a message and an ask
# through the real HTTP API, has the server deliver them to sessions and to a role, adds one
# comment that reaches nobody and one delivery attempt that fails, and then reads the store back
# and checks each tuple: what each person authored, what reached each session, and that a failed
# or pending attempt earns no tuple. It checks the role, the session and the page, and it holds
# every read against a table of identities a person wrote into this driver. A message earns no
# tuple in this child - only a comment has a node on the near end of a delivery, and
# dispatch://INFRA-1152 gives a message its own - so what the driver reads of the message is the
# role it reaches and that role's page, and that nothing stands on the message itself (G10).
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
#     database this driver already holds for the census it runs. The same goes for a tuple's
#     time, its writer and the event that caused it, which no read answers and which this
#     driver holds against the records. `held_by` and the history of a claim are read in Go
#     instead (G9): no resolution here reaches a holder at all.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DATABASE_URL (the database that
# server serves), FAKE_ENVOY_PORT (the harness's fake Envoy listener, e2e/fake-envoy.ts, whose
# session fixture this driver adds a role-less session and a role's holder to - as
# block-graph.sh takes FAKE_GITHUB_PORT for the fixture it needs), and optionally
# DEV_SIGNIN_LOGIN (default alice), the person the dev sign-in route signs in. Each unmet input
# is exit 2.
#
# One row of the identity table is not here, and this is where it went. A session the server
# itself proved a person's is recorded by one credential alone - a token the secrets broker
# scoped to a person, which proves the session and the person together and presents an ES256
# DPoP proof on every request. No shell driver can present one, so that row is carried by
# TestTheStoreHoldsNodesAndAppendedTuples in packages/envoy, which builds that credential and
# reads the alias_of tuple it leaves. What this driver does carry is the other half of the same
# rule: a session a credential merely claims in a request body earns no alias (G6).
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.
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
       (NATS_URLS): without one the publisher writes no delivered_to tuple and the delivery
       checks here wait out their poll.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DATABASE_URL FAKE_ENVOY_PORT
require_commands curl jq psql sleep
read_bearer

readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# shellcheck disable=SC2153 # these are the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$DATABASE_URL"
readonly envoy="http://127.0.0.1:$FAKE_ENVOY_PORT"
readonly person="${DEV_SIGNIN_LOGIN:-alice}"
# The second person of the two that must stay apart: the first person's login with one character
# changed. A canonicalisation that folded the two would answer one person node for both.
readonly twin="${person%?}$(printf '%s' "${person: -1}" | tr 'a-y' 'b-z')"
stamp="$(date +%s)-$$"
readonly stamp
session="graph-store-$stamp"
readonly session
# The sessions this run adds to the listener's fixture: one that claims no role, which the issue's
# route names; another that claims no role, which a mention reaches; and one holding a role of
# this run's own, so the role this driver routes a comment to is nobody else's.
readonly loose_session="019a1c09-0000-7000-8000-$(printf '%012d' "$$")"
readonly holder_session="019a1c09-1111-7000-8000-$(printf '%012d' "$$")"
readonly mentioned_session="019a1c09-6666-7000-8000-$(printf '%012d' "$$")"
readonly role="acceptance-$stamp"
# The role the message of G10 is delivered to, the route of an issue of that section's own: no
# other issue or write of this run names it, so its role and page nodes are that route's and the
# message reaches nobody else.
readonly message_role="acceptance-message-$stamp"

umask 077
work="$(mktemp -d)"
readonly work
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
message_key=""
fixture_saved=0
cleanup() {
  local code=$?
  if [[ "$fixture_saved" == 1 ]]; then
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
      --data-binary "@$work/sessions.json" "$envoy/__fixture/sessions" ||
      printf 'graph-store.sh: could not put the fake Envoy session fixture back\n' >&2
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
# which the server requires of every write a cookie authenticates; or a personal token's own
# bearer, named `token:<token>`. The answer's body goes to $work/body and its status to $status.
request() {
  local -a auth data=()
  case "$1" in
    person:*) auth=(-b "$work/jar.${1#person:}" -H "Origin: $origin") ;;
    token:*) auth=(-H "Authorization: Bearer ${1#token:}") ;;
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

# comment SECTION WHAT AS BODY [MENTION]: writes a comment on the issue as AS (agent, person:LOGIN
# or token:TOKEN), optionally mentioning MENTION, and sets $comment to its id and $comment_at to
# the time the record holds.
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
#     and each resolving to that person.
identities=(
  "$person|$person"
  "$(printf '%s' "$person" | tr 'a-z' 'A-Z')|$person"
  "  $person  |$person"
  "$twin|$twin"
)
readonly identities

# S: the inputs, the people, the listener's sessions and the issue.
host="${database_url#*://}"
host="${host#*@}"
host="${host%%[:/]*}"
case "$host" in
  127.0.0.1 | ::1 | localhost) pass setup "DATABASE_URL names this machine ($host)" ;;
  *) fail setup "DATABASE_URL names this machine" "host $host is not loopback; this driver reads this pod's own test database and never a deployment's" ;;
esac
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
  --arg holder "$holder_session" --arg role "$role" \
  '[.[] | {session_id, title, dir, machine_id, host, roles, capabilities}]
   + [{session_id: $loose, title: "Acceptance loose", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $mentioned, title: "Acceptance mentioned", machine_id: "acceptance", roles: [],
       capabilities: ["aside", "btw", "steer"]},
      {session_id: $holder, title: "Acceptance holder", machine_id: "acceptance", roles: [$role],
       capabilities: ["aside", "btw", "steer"]}]' \
  "$work/sessions.json" >"$work/fixture.json" ||
  fail setup "build the session fixture" "$(head -c 300 "$work/sessions.json")"
curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' \
  --data-binary "@$work/fixture.json" "$envoy/__fixture/sessions" 2>"$work/curl" ||
  fail setup "seed the session fixture" "curl: $(<"$work/curl")"
pass setup "the listener holds $loose_session and $mentioned_session (no role) and $holder_session (role $role)"

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
# against a server started with no NATS_URLS, and against a NATS without JetStream, whose
# publishes the server logs as refused. A route delivery leaves no row in any table this child
# projects, so there is nothing of the attempt to read here instead; what the driver can do is
# say so when the poll runs out, which the hint below does.
settles G5 "the store holds one delivered_to tuple of it" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$loose_comment'
     and kind = 'delivered_to' and to_kind = 'session' and to_id = '$loose_session' and asserted" \
  "A zero here is the bus before it is the store: the publisher writes this tuple only for a
     batch it delivered. Read the server's log for 'no NATS client' or 'jetstream not enabled',
     and start the server with NATS_URLS naming a nats-server run with -js"
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

# Every session a sent attempt reached is a node. The read spans every run's attempts, so it is
# held to this run's own first: a read over no attempt of this issue reads zero for nothing.
query G5 "count this run's sent attempts" \
  "select count(*) from comment_deliveries d join comments c on c.id = d.comment_id
   where c.issue_key = '$key' and d.state = 'sent'"
[[ "$answer" -ge 1 ]] ||
  fail G5 "this run's issue holds a sent attempt" "read $answer sent attempt(s) on $key"
sent_here="$answer"
query G5 "count the sessions a sent attempt reached" \
  "select count(distinct session_id) from comment_deliveries
   where state = 'sent' and coalesce(session_id, '') <> ''"
sent_sessions="$answer"
want_value G5 "every session a sent attempt reached is a node ($sent_sessions session(s), $sent_here sent attempt(s) on $key)" 0 \
  "select count(distinct d.session_id) from comment_deliveries d
   where d.state = 'sent' and coalesce(d.session_id, '') <> ''
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
readonly claimed_session="019a1c09-2222-7000-8000-$(printf '%012d' "$$")"
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
readonly twin_session="019a1c09-3333-7000-8000-$(printf '%012d' "$$")"
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

# G8: an attempt that fails earns no tuple. The mention names a session the listener does not
# hold, so its attempt settles failed; the issue's own route still reaches its session, which is
# why this reads the tuples of the mentioned session alone.
readonly dead_session="019a1c09-4444-7000-8000-$(printf '%012d' "$$")"
comment G8 "a comment mentioning a session the listener does not hold" agent \
  "A mention nobody can take." "session:$dead_session"
readonly failed_comment="$comment"
settles G8 "the attempt settles failed" "failed" \
  "select coalesce(max(state), 'none') from comment_deliveries where comment_id = '$failed_comment'"
want_value G8 "a failed attempt earns no tuple" 0 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$failed_comment'
     and kind = 'delivered_to' and to_id = '$dead_session'"
want_value G8 "no delivered_to tuple stands on an attempt that is not sent" 0 \
  "select count(*) from graph_tuples t
   where t.kind = 'delivered_to' and t.to_kind = 'session'
     and exists (select 1 from comment_deliveries d
                 where d.comment_id::text = t.from_id and d.session_id = t.to_id)
     and not exists (select 1 from comment_deliveries d
                     where d.comment_id::text = t.from_id and d.session_id = t.to_id
                       and d.state = 'sent')"

# G9: a delivery to a role ends at the role node and not on a person; the role carries its page
# and its held_by claim, and the session it resolved to is part_of that role.
request "person:$person" PATCH "/api/v1/issues/$key" \
  "$(jq -cn --arg r "role:$role" '{route: $r}')"
want_status 200 G9 "route the issue to the role"
comment G9 "a comment on the issue the role holds" "person:$person" \
  "A note for whoever holds $role."
readonly role_comment="$comment"
settles G9 "the delivery ends at the role node" "role|$role" \
  "select coalesce(max(to_kind || '|' || to_id), 'none') from graph_tuples
   where from_kind = 'comment' and from_id = '$role_comment' and kind = 'delivered_to'"
want_value G9 "the delivery reached no person and no session" 1 \
  "select count(*) from graph_tuples where from_kind = 'comment' and from_id = '$role_comment'
     and kind = 'delivered_to'"
# The `held_by` tuple from a role to the session a forward resolved, the retraction that ends
# that claim with the earlier one still in the history, and a session's `part_of` to the role it
# claimed are proven in c-store-tuples (TestTheStoreHoldsNodesAndAppendedTuples in
# packages/envoy), which drives the write path in process with a resolved holder this pod's fake
# Envoy cannot give a shell driver: the claim lives in Envoy's role bucket (roles.ClaimStore),
# which `bun run dispatch:local` has none of, so every resolution here reads heldStateUnresolved
# and the publisher states no claim. The role and its page are written all the same, and this
# driver reads them.
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

# G10: a message earns no tuple in this child. Only a comment has a node on the near end of a
# delivery today (outbox.deliveredItem), so the record of what a batch delivered appends no
# delivered_to tuple for a message, and no writer appends its authored_by. What a message on a
# role-routed issue does have in the store is that role, its page and the page's part_of: the
# outbox writes a role and its page for every role an event names, and the first event to name
# this one is the write that routed the issue to it, so that write is the cause the store records.
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
# The two reads of nothing, each beside the same read of something: the route's event caused the
# role's and the page's tuples, and the comment G9 delivered to a role stands in tuples, so a zero
# for the message is the message earning nothing and not a read that matched nothing.
query G10 "count the tuples the route's event caused" \
  "select count(*) from graph_tuples where source_event = $route_event"
[[ "$answer" -gt 0 ]] ||
  fail G10 "the route's event caused the role's tuples" "no tuple names event $route_event as its cause"
route_caused="$answer"
want_value G10 "the message's event caused no node and no tuple (the route's caused $route_caused tuple(s))" "0|0" \
  "select (select count(*) from graph_nodes where source_event = $message_event) || '|'
          || (select count(*) from graph_tuples where source_event = $message_event)"
query G10 "count the tuples that stand on the comment G9 delivered" \
  "select count(*) from graph_tuples where from_id = '$role_comment' or to_id = '$role_comment'"
[[ "$answer" -gt 0 ]] ||
  fail G10 "the comment G9 delivered stands in tuples" "no tuple names $role_comment"
comment_tuples="$answer"
want_value G10 "no tuple stands on the message (the comment G9 delivered stands in $comment_tuples)" 0 \
  "select count(*) from graph_tuples where from_id = '$message' or to_id = '$message'"
want_value G10 "the message is no node of this child" 0 \
  "select count(*) from graph_nodes where id = '$message'"

# G11: a session the store never recorded answers no node at all, read through the server's own
# node reader. An ask's followers are the one edge of graph_edges whose far end is a session, so
# the read answers a session the store holds - this run's own, which wrote the comments of G2 -
# and leaves out the one the store never saw.
send POST "/api/v1/issues/$key/asks" "$(jq -cn --arg s "$session" \
  '{question: "Which sessions does the store hold?", actor: {kind: "session", id: $s}}')"
want_status 201 G11 "open an ask"
ask="$(jq -er '.id' "$work/body")" || fail G11 "open an ask" "no ask id: $(body)"
readonly unseen_session="019a1c09-5555-7000-8000-$(printf '%012d' "$$")"
for follower in "$session" "$unseen_session"; do
  request "person:$person" PUT "/api/v1/asks/$ask/followers/$follower"
  [[ "$status" == 200 || "$status" == 201 || "$status" == 204 ]] ||
    fail G11 "follow the ask as $follower" "HTTP $status: $(body)"
done
pass G11 "both sessions follow the ask"
want_value G11 "the store holds a node of the session that wrote" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$session'"
want_value G11 "the store holds no node of the session it never recorded" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$unseen_session'"
get "/api/v1/references?from=$(q "dispatch://$key/ask/$ask")&kind=followed_by"
want_status 200 G11 "read the ask's followers through the graph"
expect G11 "the recorded session answers a node" "[\"$session\"]" \
  --arg id "$session" '[.edges[] | select(.node.id == $id) | .node.id]'
expect G11 "a session the store never recorded answers no node at all" "[]" \
  --arg id "$unseen_session" '[.edges[] | select(.node.id == $id) | .node.id]'
want_value G11 "the read wrote no node" 0 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$unseen_session'"

# G12: every tuple this run wrote carries a time, an actor and a causing write the records hold.
want_value G12 "every authored_by tuple of this run matches its record" 0 \
  "select count(*) from graph_tuples t
   join comments c on c.id::text = t.from_id
   where t.from_kind = 'comment' and t.kind = 'authored_by' and c.issue_key = '$key'
     and (t.at <> c.created_at or t.who <> c.author
          or t.source_event <> (select e.id from events e
                                where e.type = 'comment.created' and e.payload->>'id' = c.id::text))"
want_value G12 "every delivered_to tuple of this run names an event that exists" 0 \
  "select count(*) from graph_tuples t
   join comments c on c.id::text = t.from_id
   where t.from_kind = 'comment' and t.kind = 'delivered_to' and c.issue_key = '$key'
     and (t.at < c.created_at or not exists (select 1 from events e where e.id = t.source_event))"
want_value G12 "no tuple of this run was written by nobody" 0 \
  "select count(*) from graph_tuples t
   join comments c on c.id::text = t.from_id
   where t.from_kind = 'comment' and c.issue_key = '$key'
     and coalesce(t.who->>'kind', '') = ''"

printf 'graph-store.sh: every check passed\n'
