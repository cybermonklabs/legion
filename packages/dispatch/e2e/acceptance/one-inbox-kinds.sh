#!/usr/bin/env bash
# Drives this child's move of its kinds into the node and tuple store end to end against
# a running server (c-kinds-moved): the message kind, the comment pointers and the body blocks of a
# comment, a message and an ask, with the thirteen arms of graph_edges that read them and the item
# rows of its refs arm.
#
# It writes through the real HTTP API, on two issues of its own (one whose comments, messages and
# ask it writes, and one every citation it writes points at), and then reads the store back:
#
#   messages      an issue message sent to an agent, a reply to it naming one of its blocks, and a
#                 broadcast's copy, each read back as its node and the tuples its row's in_reply_to,
#                 issue_key, target, broadcast_id and replies_to_block are; the reply's replies_to
#                 and part_of both point at the message it answers, from the one write.
#   comments      a reply to a comment, a comment that answers that reply and names one of its
#                 blocks, and a comment on an ask that names the ask's question, each read back as
#                 the tuples its reply_to, ask_id, answers and replies_to_block are; the answering
#                 comment's answers and part_of both point at the comment it answers, from the one
#                 write.
#   blocks        every body block of a comment, an issue message and an ask is a `block` node
#                 whose parent is that item, no ask_block, comment_block or message_block node or
#                 tuple is in the store, and a block of each sort holds its link as a tuple.
#   agent_page    a message that belongs to no issue, sent to an agent, hangs from that agent's
#                 page by a part_of the transaction that wrote the message node wrote; the agent's
#                 reply under it hangs from the message and never from the page; and the issue
#                 message sent to the same agent has one part_of, to its issue, and none to the page.
#   conversation  the issue message sent to the agent is in the agent's session-conversation read,
#                 GET /api/v1/agents/{session_id}/conversation, through its own target or the
#                 attempt that names the session (R7) and not through a delivered_to tuple: once
#                 with the delivery the listener took, and once for a second message sent while the
#                 fake Envoy answered the agent's sends 503, whose one attempt settled failed and
#                 states no delivered_to. The listener answers again before that read, a retry of
#                 the second message is taken and states one, and the conversation read answers
#                 the rows GET /api/v1/agents/{session_id}/messages answers for the agent, the pair
#                 read up to five times while another caller's write moves one of them.
#   retractions   one tuple of each of the thirteen moved arms, and each of the three citations,
#                 retracted: graph_edges answers it once before, not at all after while the row's
#                 own column still names it, and once again when the tuple is stated again.
#   older_rows    one row per arm, and one citation of each item sort, written through psql as the
#                 release before this one writes them, with no marker of this child's: each is
#                 answered once, by its old arm, the store holding no tuple of it. A comment row
#                 carries its node, as the node store's writer gives it one. The rows are read in
#                 the transaction that writes them, which then rolls back, so they are gone once
#                 read.
#   backfill      one row per arm, and one citation of each item sort, written through psql as a
#                 build before this child writes them, each comment and the message and the ask
#                 naming no node yet: each is answered once, by its old arm, in the transaction that
#                 writes them, and once, by the tuple arm, after
#                 `envoy-dispatch backfill-graph-store` runs over the server's database, which
#                 reports every kind complete=true, each comment carrying the node the node store's
#                 backfill gave it. The rows are removed once read.
#   artifact_citation
#                 a document's citation, which another child moves and not this child, is
#                 still answered by the refs arm.
#   kinds         the delivery_attempt, person and role supertags are in the store, each kind has a
#                 label in NODE_LABELS in the web build the server serves, and every supertag the
#                 store holds, and every node's supertag, is one of the root's closed list.
#   editor        the delivery_attempt supertag is in the store with the attribute nodes this child
#                 adds, the server serves the schema the store holds, a document socket admits a
#                 writer, and a write over it is accepted and reaches the document's text.
#
# One sentence of the check reads what no route of this build serves, and no section of this
# driver reads it: that a delivery_attempt, a person and a role are each addressed, listed and
# counted. text.ParseRef reads an address of a person (`dispatch://person/<login>`) and of a role
# (`dispatch://role/<token>`) but none of a delivery attempt, and refs.BacklinkCounts declares an
# arm for each of the three kinds, yet the only routes that answer a backlink count are an ask's
# and an issue's: no route counts a node of these three. The kinds section reads what the store
# and the web build hold of them instead.
#
# Which clause is read which way, and why: what the store holds - a node, a tuple, a row's marker,
# the transaction that wrote a row (its xmin) - is served by no route, so it is read through psql on
# ISSUE_DATABASE_URL, the database the server serves, as the node store's graph-store.sh
# reads it. A retraction and a row of the release before are written there too: no route of this
# build ends one tuple while its row stands, and no route writes as an older build does.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), ISSUE_DATABASE_URL (the database that
# server serves, on this machine, which the pod's setup in the issue's Verification document
# exports), FAKE_ENVOY_PORT (the port of the harness's fake Envoy listener, e2e/fake-envoy.ts, that
# server sends to: the conversation section has it refuse the agent's sends for one message and
# then answer them again, and the Verification preamble reads it from the dispatch:local log),
# envoy-dispatch on PATH (this build's packages/envoy/cmd/dispatch, which the backfill section runs
# over that database, as the Verification preamble builds it) and optionally DEV_SIGNIN_LOGIN
# (default alice), the person the dev sign-in route signs in: a direct message, a broadcast, a
# document socket and the session-conversation read are a person's. Each unmet input is exit 2,
# but envoy-dispatch, whose absence fails the backfill section. The server's agent list must hold a
# session that claims no role, as the harness's Observer does; without one the setup fails, naming
# the sessions it lists.
#
# Each passing assertion prints `PASS: <section> <what>`; a failing one prints
# `FAIL: <section> <what>: <observed>` and ends its section. Every section runs whatever an
# earlier one found, each arm of the retractions, the older rows and the backfill runs whatever another arm
# found, and the script exits 1 at its end if anything failed. A failure while it writes what the
# sections read ends the run there, with nothing to read.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: ISSUE_DATABASE_URL=postgres://user@127.0.0.1:5433/db \
         DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         FAKE_ENVOY_PORT=<port> [DEV_SIGNIN_LOGIN=alice] one-inbox-kinds.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       ISSUE_DATABASE_URL must name the database that server serves, on this machine: the run
       writes and removes rows in it; FAKE_ENVOY_PORT is the port of the fake Envoy listener that
       server sends to, which dispatch:local prints)
USAGE
}

# What lib.sh's functions set for this script: the bearer (read_bearer), the work directory
# (work_dir) and each read's answer (query). They are named here so that shellcheck, which reads
# one file at a time unless it is run with -x, sees them assigned.
token="" work="" answer=""
# shellcheck source-path=SCRIPTDIR source=lib.sh disable=SC1091 # followed under shellcheck -x
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT ISSUE_DATABASE_URL FAKE_ENVOY_PORT
require_commands bun cmp curl diff jq psql sed sort paste sleep
read_bearer

# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# shellcheck disable=SC2153 # these are the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$ISSUE_DATABASE_URL"
[[ "$FAKE_ENVOY_PORT" =~ ^[0-9]+$ ]] || {
  printf 'one-inbox-kinds.sh: FAKE_ENVOY_PORT is %s, not a port number\n' "$FAKE_ENVOY_PORT" >&2
  usage
  exit 2
}
readonly fake_envoy="http://127.0.0.1:$FAKE_ENVOY_PORT"
readonly person="${DEV_SIGNIN_LOGIN:-alice}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
stamp="$(date +%s)-$$"
readonly stamp
readonly session="one-inbox-kinds-$stamp"
readonly actor="{\"kind\": \"session\", \"id\": \"$session\"}"

loopback_database_url "$database_url" || {
  printf 'one-inbox-kinds.sh: ISSUE_DATABASE_URL names no single loopback host; this driver writes and removes rows and runs only against a test database on this machine\n' >&2
  exit 2
}

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The issues the run writes, each closed when the run ends. The rows older_rows_section and
# backfill_section write as an older build writes them are each section's own to remove, on its
# way out. A fake Envoy the conversation section left refusing the agent's sends answers them
# again.
issues=()
cleanup() {
  local code=$? key
  if [[ -e "$work/envoy-refusing" && -n "${agent:-}" ]]; then
    fake_envoy_sends 200 >"$work/envoy.out" ||
      printf 'one-inbox-kinds.sh: could not have the fake Envoy answer sends to %s again: %s\n' \
        "$agent" "$(<"$work/envoy.out")" >&2
  fi
  for key in "${issues[@]}"; do
    send PATCH "/api/v1/issues/$key" "$(jq -cn --argjson a "$actor" '{status: "done", actor: $a}')"
    [[ "$status" == 200 ]] ||
      printf 'one-inbox-kinds.sh: could not close %s: HTTP %s: %s\n' "$key" "$status" "$(body)" >&2
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# request AS METHOD PATH [JSON]: sends METHOD PATH as AS: `agent`, with the bearer in $work/auth, or
# `person`, with the cookie the dev sign-in left in $work/jar and the dashboard's origin, which the
# server requires of every write a cookie authenticates. The answer's body goes to $work/body and
# its HTTP status to $status.
request() {
  local -a auth data=()
  if [[ "$1" == person ]]; then
    auth=(-b "$work/jar" -H "Origin: $origin")
  else
    auth=(-H "@$work/auth")
  fi
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

# settles SECTION WHAT WANT SQL: polls SQL until it reads WANT, for up to 30 s: the block index
# writes a document's citations a tick after the write that stored it.
settles() {
  local waited=0
  while true; do
    query "$1" "$2" "$4"
    [[ "$answer" != "$3" ]] || break
    [[ "$waited" -lt 60 ]] || fail "$1" "$2" "read '$answer', want '$3' after 30 s (from: $4)"
    sleep 0.5
    waited=$((waited + 1))
  done
  pass "$1" "$2"
}

# sorted WORD...: the words sorted bytewise and joined by spaces, the order `collate "C"` sorts in.
sorted() { printf '%s\n' "$@" | LC_ALL=C sort | paste -sd' ' -; }

# sql_list WORD...: the words as a list of SQL string literals.
sql_list() {
  local word list=""
  for word in "$@"; do
    list+="${list:+, }'$word'"
  done
  printf '%s' "$list"
}

# isolated COMMAND...: runs COMMAND in a subshell of its own, with errexit, and sets
# $isolated_status to its exit status, so a `fail` inside ends COMMAND alone. errexit is turned off
# around it, not left to the caller's `||`: under an `||` bash ignores errexit in the whole
# subshell, and an unguarded failing command inside would run on.
isolated() {
  set +e
  (
    set -e
    "$@"
  )
  isolated_status=$?
  set -e
}

# The kinds of relation a message's write states of what it answers, where it sits, where it was
# sent and who wrote it, and the ones a comment's write states of what it answers and where it
# sits (api's message_pointer_tuples_test.go and comment_pointer_tuples_test.go read the same).
readonly message_kinds="'addressed_to', 'authored_by', 'part_of', 'replies_to'"
readonly comment_kinds="'answers', 'part_of', 'replies_to'"

# current_out SECTION WHAT KIND ID KINDS: the relations of KINDS the store holds now out of node
# KIND/ID - each tuple's latest statement, where that asserts it - as `kind>to_kind:to_id`, sorted
# bytewise and space-separated, `(none)` for none, in $answer.
current_out() {
  query "$1" "$2" "
    select coalesce(string_agg(relation, ' ' order by relation collate \"C\"), '(none)')
    from (select distinct on (kind, to_kind, to_id) kind || '>' || to_kind || ':' || to_id as relation, asserted
          from graph_tuples
          where from_kind = '$3' and from_id = '$4' and kind in ($5) and to_kind <> 'value'
          order by kind, to_kind, to_id, seq desc) latest
    where asserted"
}

# want_out SECTION WHAT KIND ID KINDS RELATION...: the relations of KINDS out of KIND/ID are exactly
# the RELATIONs.
want_out() {
  local section="$1" what="$2" kind="$3" id="$4" kinds="$5" want
  shift 5
  want="$(sorted "$@")"
  current_out "$section" "$what" "$kind" "$id" "$kinds"
  [[ "$answer" == "$want" ]] || fail "$section" "$what" "$kind $id states '$answer', want '$want'"
  pass "$section" "$what"
}

# one_write SECTION WHAT KIND ID TABLE KINDS EVENTS: every tuple of KINDS out of KIND/ID was
# appended by the one write that stored the row of TABLE: one transaction (one xmin), caused by the
# event of one of EVENTS that names the row, and dated by the row's own created_at.
one_write() {
  want_value "$1" "$2" "true|1|1|true|true" "
    select (count(*) > 0)::text || '|' || count(distinct t.source_event) || '|'
           || count(distinct t.xmin::text) || '|' || bool_and(t.at = r.created_at)::text || '|'
           || bool_and(t.source_event = (select max(e.id) from events e
                                         where e.type in ($7) and e.payload->>'id' = '$4'))::text
    from graph_tuples t join $5 r on r.id::text = t.from_id
    where t.from_kind = '$3' and t.from_id = '$4' and t.kind in ($6) and t.to_kind <> 'value' and t.asserted"
}

# names_its_node SECTION WHAT KIND ID TABLE: the row of TABLE names its node KIND/ID, which the
# store holds live.
names_its_node() {
  want_value "$1" "$2" "$3|$4|live" "
    select coalesce(r.node_kind, '(none)') || '|' || coalesce(r.node_address, '(none)') || '|'
           || coalesce(n.state, '(no node)')
    from $5 r left join graph_nodes n on n.kind = r.node_kind and n.id = r.node_address
    where r.id = '$4'"
}

# The document socket's writer, run by bun from this file's directory so that its imports resolve
# in this package's node_modules, as e2e/acceptance/socket-scope.ts's do. It opens the document
# socket as the person whose Cookie header the file it is given holds, through the browser's own
# Hocuspocus provider. It presents the manifest's readable version and computes the creatable
# version with the same helper as the browser. It prints the authentication scope.
# On `read-write` it appends the marker to the first paragraph. It prints `accepted` once the
# server acknowledges each update as applied (a SyncStatus frame per update, counted down in
# unsyncedChanges). A refusal, a read-only admission, a close or no answer within fifteen seconds
# exits 1 and says which. Its inputs are environment variables, so no argument list names a file.
read -r -d '' socket_writer <<'TS' || true
import { HocuspocusProvider, HocuspocusProviderWebsocket } from "@hocuspocus/provider";
import * as Y from "yjs";
import { schemaCreatable } from "../../web/src/features/doc/schema-creatable";

const env = (name: string): string => {
  const value = process.env[name];
  if (value === undefined || value === "") {
    console.error(`${name} is unset`);
    process.exit(2);
  }
  return value;
};
const server = env("SOCKET_SERVER");
const documentId = env("SOCKET_DOCUMENT");
const schema: Parameters<typeof schemaCreatable>[0] = await Bun.file(env("SOCKET_SCHEMA_FILE")).json();
const version = String(schema.version);
const marker = env("SOCKET_MARKER");
const cookie = (await Bun.file(env("SOCKET_COOKIE_FILE")).text()).trim();
const origin = new URL(server).origin;
const answerWithinMs = 15_000;

// Bun's WebSocket client takes request headers; the provider constructs its socket with a URL
// alone, so the cookie and the origin ride in this subclass.
class CookieWebSocket extends WebSocket {
  constructor(url: string | URL, protocols?: string | string[]) {
    super(url, { headers: { Cookie: cookie, Origin: origin }, protocols } as unknown as string[]);
  }
}

const outcome = Promise.withResolvers<void>();
const timer = setTimeout(
  () => outcome.reject(new Error(`no acknowledgement within ${answerWithinMs / 1000} s`)),
  answerWithinMs
);
const doc = new Y.Doc();
const websocket = new HocuspocusProviderWebsocket({
  WebSocketPolyfill: CookieWebSocket,
  parameters: { schema_creatable: schemaCreatable(schema), schema_version: version },
  url: `${origin.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(documentId)}`,
});
let typed = false;
const provider = new HocuspocusProvider({
  document: doc,
  name: documentId,
  onAuthenticated: () => {
    console.log(`scope ${provider.authorizedScope ?? "(none)"}`);
    if (provider.authorizedScope !== "read-write") {
      outcome.reject(new Error("the socket was admitted, but not as a writer"));
      return;
    }
    typeWhenSynced();
  },
  onAuthenticationFailed: ({ reason }) => outcome.reject(new Error(`the server denied the socket: ${reason}`)),
  onClose: ({ event }) => outcome.reject(new Error(`the socket closed: ${event.code} ${event.reason}`)),
  onSynced: () => typeWhenSynced(),
  token: version,
  websocketProvider: websocket,
});
provider.on("unsyncedChanges", (count: number) => {
  if (typed && count === 0) outcome.resolve();
});

function typeWhenSynced(): void {
  if (typed || !provider.isSynced || provider.authorizedScope !== "read-write") return;
  for (const block of doc.getXmlFragment("prosemirror").toArray()) {
    const text = block instanceof Y.XmlElement && block.nodeName === "paragraph" ? block.get(0) : undefined;
    if (text instanceof Y.XmlText) {
      typed = true;
      text.insert(text.length, ` ${marker}`);
      return;
    }
  }
  outcome.reject(new Error(`the document holds no paragraph to type in: ${doc.getXmlFragment("prosemirror").toString()}`));
}

try {
  await outcome.promise;
  console.log("accepted");
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
} finally {
  clearTimeout(timer);
  provider.destroy();
  websocket.destroy();
}
TS
readonly socket_writer

# --- setup: the person, the agent and the two issues ------------------------------------------

curl -sS -o /dev/null -D "$work/signin" -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$person")" \
  2>"$work/curl" || fail setup "sign $person in" "curl: $(<"$work/curl")"
# curl writes a jar's header lines whatever the answer, so the jar holds a cookie only where it
# holds a cookie's line, whose fields a tab parts.
grep -q $'\t' "$work/jar" || fail setup "sign $person in" "the dev sign-in route left no cookie"
# The document socket's writer reads the Cookie header itself, as e2e/acceptance/ci-check.sh writes it.
sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' "$work/signin" | paste -sd ';' - >"$work/cookie"
[[ -s "$work/cookie" ]] || fail setup "sign $person in" "the dev sign-in set no cookie"
pass setup "signed $person in"

# The agent: a session the server lists that claims no role, and of which the store records no
# claim either, so the page of its own is the page a message to it hangs from (graphstore.AgentPage).
get /api/v1/agents
want_status 200 setup "list the agents"
mapfile -t candidates < <(jq -r '(if type == "array" then . else .agents end)[]
  | select(((.roles // []) | length) == 0) | .session_id' "$work/body")
agent=""
for candidate in "${candidates[@]}"; do
  query setup "read the store's claims of $candidate" "
    select count(*) from graph_tuples
    where ((to_kind = 'session' and to_id = '$candidate' and kind = 'held_by' and from_kind = 'role')
        or (from_kind = 'session' and from_id = '$candidate' and kind = 'part_of' and to_kind = 'role'))
      and asserted"
  if [[ "$answer" == 0 ]]; then
    agent="$candidate"
    break
  fi
done
[[ -n "$agent" ]] ||
  fail setup "find an agent that claims no role" "the server lists none: $(jq -c '[(if type == "array" then . else .agents end)[] | {session_id, roles}]' "$work/body")"
readonly agent
readonly agent_page="session/$agent"
pass setup "the agent is $agent, which claims no role, so its page is $agent_page"

# create_issue TITLE: creates an issue in PROJECT as this run's session, recorded for cleanup, and
# sets $key to its key. `force` takes it past the duplicate check: a second run, or the run's other
# issue, writes a title like one already there, and the check would refuse it as a duplicate.
create_issue() {
  send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$1" --argjson a "$actor" \
    '{project: $p, title: $t, spec: "The first line of the spec.", force: true, actor: $a}')"
  want_status 201 setup "create \"$1\""
  key="$(jq -er '.key' "$work/body")" || fail setup "create \"$1\"" "no key: $(body)"
  issues+=("$key")
  pass setup "created $key in $project"
}
create_issue "What the kinds the store moved cite"
readonly cited="$key"
create_issue "The kinds the store moved"
readonly issue="$key"
send GET "/api/v1/issues/$issue"
want_status 200 setup "read $issue"
spec="$(jq -er '.primary_artifact_id' "$work/body")" || fail setup "read $issue's spec" "$(body)"
readonly spec

# --- seed: what the sections read, written through the API --------------------------------------

# post SECTION WHAT AS PATH JSON: writes JSON at PATH as AS (agent or person) and sets $id to the id
# the answer names.
post() {
  request "$3" POST "$4" "$5"
  want_status 201 "$1" "$2"
  id="$(jq -er '.id' "$work/body")" || fail "$1" "$2" "no id: $(body)"
  pass "$1" "$2 ($id)"
}
# as_session JSON: JSON with this run's session as its actor, as a bearer caller names it.
as_session() { jq -cn --argjson f "$1" --argjson a "$actor" '$f + {actor: $a}'; }

readonly citation="after dispatch://$cited"

post seed "a root comment citing $cited" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg b "The root of the thread, $citation.

Its second paragraph." '{body: $b}')")"
readonly root="$id"
post seed "a reply to the root" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg r "$root" '{body: "Alpha.\n\nBeta.", reply_to: $r}')")"
readonly reply="$id"
post seed "a comment that answers the reply and names its block b2" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg r "$reply" '{body: "On beta.", reply_to: $r, reply_to_block: "b2"}')")"
readonly leaf="$id"
post seed "an ask citing $cited" agent "/api/v1/issues/$issue/asks" \
  "$(as_session "$(jq -cn --arg q "Which day do we ship, $citation?" \
    '{question: $q, options: [{label: "Monday"}, {label: "Tuesday"}]}')")"
readonly ask="$id"
post seed "a comment on the ask naming its question, part b1" agent "/api/v1/issues/$issue/comments" \
  "$(as_session "$(jq -cn --arg k "$ask" '{body: "On the question itself.", ask_id: $k, reply_to_block: "b1"}')")"
readonly on_ask="$id"

post seed "an issue message sent to $agent, citing $cited" agent "/api/v1/issues/$issue/messages" \
  "$(as_session "$(jq -cn --arg b "One, $citation.

Two." --arg t "session:$agent" '{body: $b, target: $t, delivery: "btw"}')")"
readonly message="$id"
post seed "a reply to the message naming its block b2" agent "/api/v1/issues/$issue/messages" \
  "$(as_session "$(jq -cn --arg m "$message" '{body: "On two.", in_reply_to: $m, in_reply_to_block: "b2"}')")"
readonly message_reply="$id"

request person POST /api/v1/broadcasts "$(jq -cn --arg s "$agent" --arg b "A broadcast to $agent, $stamp." \
  '{body: $b, delivery: "btw", session_ids: [$s]}')"
want_status 201 seed "$person broadcasts to $agent"
broadcast="$(jq -er '.id' "$work/body")" || fail seed "$person broadcasts to $agent" "no id: $(body)"
readonly broadcast
copy="$(jq -er --arg s "$agent" '.recipients[] | select(.session_id == $s) | .message.id' "$work/body")" ||
  fail seed "$person broadcasts to $agent" "no copy for $agent: $(body)"
readonly copy
pass seed "$person broadcasts $broadcast to $agent, its copy $copy"

post seed "$person sends $agent a message of no issue" person "/api/v1/agents/$(q "$agent")/messages" \
  "$(jq -cn --arg b "A direct message to $agent, $stamp." '{body: $b, delivery: "btw"}')"
readonly direct="$id"
post seed "$agent replies to it" agent "/api/v1/messages/$direct/reply" \
  "$(jq -cn --arg s "$agent" '{body: "On it.", attempt: 1, actor: {kind: "session", id: $s}}')"
readonly direct_reply="$id"

# --- the sections ------------------------------------------------------------------------------

messages_section() {
  local section=messages
  names_its_node "$section" "the issue message is a node its row names" message "$message" messages
  want_out "$section" "the issue message states its issue_key and target as tuples, and who wrote it" \
    message "$message" "$message_kinds" \
    "part_of>issue:$issue" "addressed_to>session:$agent" "authored_by>session:$session"
  one_write "$section" "the issue message's tuples are its write's, dated by its row" \
    message "$message" messages "$message_kinds" "'message.created'"

  names_its_node "$section" "the reply is a node its row names" message "$message_reply" messages
  # A reply's target is what its row holds - a person's reply continues its thread's, a session's
  # names none of its own here - and its addressed_to is that column's tuple, where there is one.
  query "$section" "read the reply's target" "select coalesce(target, '') from messages where id = '$message_reply'"
  local -a reply_target=()
  [[ -z "$answer" ]] || reply_target=("addressed_to>${answer%%:*}:${answer#*:}")
  want_out "$section" "the reply states its in_reply_to, replies_to_block and target (${answer:-none}) as tuples, under the message" \
    message "$message_reply" "$message_kinds" \
    "replies_to>message:$message" "replies_to>block:message/$message#b2" "part_of>message:$message" \
    "authored_by>session:$session" ${reply_target[@]+"${reply_target[@]}"}
  one_write "$section" "the reply's replies_to and part_of both point at the message it answers, from the one write" \
    message "$message_reply" messages "'part_of', 'replies_to'" "'message.answered'"
  want_value "$section" "the reply's replies_to and part_of both point at $message" "message:$message|message:$message" "
    select coalesce(max(to_kind || ':' || to_id) filter (where kind = 'replies_to' and to_kind = 'message'), '(none)')
           || '|' || coalesce(max(to_kind || ':' || to_id) filter (where kind = 'part_of'), '(none)')
    from graph_tuples where from_kind = 'message' and from_id = '$message_reply' and asserted"

  names_its_node "$section" "the broadcast's copy is a node its row names" message "$copy" messages
  want_value "$section" "the copy states its broadcast_id as a value tuple" "$broadcast|$broadcast" "
    select coalesce((select to_id from graph_tuples
                     where from_kind = 'message' and from_id = '$copy' and kind = 'broadcast_id'
                       and to_kind = 'value' and asserted order by seq desc limit 1), '(none)')
           || '|' || coalesce((select broadcast_id::text from messages where id = '$copy'), '(none)')"
  want_out "$section" "the copy states its target as a tuple and sits on the agent's page" \
    message "$copy" "$message_kinds" \
    "part_of>page:$agent_page" "addressed_to>session:$agent" "authored_by>person:$person"
}

comments_section() {
  local section=comments
  names_its_node "$section" "the root is a node its row names" comment "$root" comments
  want_out "$section" "the root states no pointer" comment "$root" "$comment_kinds" "(none)"

  names_its_node "$section" "the reply is a node its row names" comment "$reply" comments
  want_value "$section" "the reply's row holds tuples_stored" true \
    "select tuples_stored::text from comments where id = '$reply'"
  want_out "$section" "the reply states its reply_to and answers as tuples, under the root" \
    comment "$reply" "$comment_kinds" \
    "replies_to>comment:$root" "answers>comment:$root" "part_of>comment:$root"

  want_out "$section" "the answering comment states its reply_to, answers and replies_to_block as tuples" \
    comment "$leaf" "$comment_kinds" \
    "replies_to>comment:$root" "answers>comment:$reply" "replies_to>block:comment/$reply#b2" \
    "part_of>comment:$reply"
  want_value "$section" "the answering comment's answers and part_of both point at the reply it answers" \
    "comment:$reply|comment:$reply" "
    select coalesce(max(to_kind || ':' || to_id) filter (where kind = 'answers'), '(none)') || '|'
           || coalesce(max(to_kind || ':' || to_id) filter (where kind = 'part_of'), '(none)')
    from graph_tuples where from_kind = 'comment' and from_id = '$leaf' and asserted"
  one_write "$section" "the answering comment's answers and part_of come from the one write" \
    comment "$leaf" comments "'answers', 'part_of'" "'comment.created'"
  one_write "$section" "every pointer tuple of the answering comment is its write's, dated by its row" \
    comment "$leaf" comments "$comment_kinds" "'comment.created'"

  want_out "$section" "the comment on the ask states its ask_id and replies_to_block as tuples, under the ask" \
    comment "$on_ask" "$comment_kinds" \
    "replies_to>ask:$ask" "replies_to>block:ask/$ask#b1" "part_of>ask:$ask"
  one_write "$section" "the comment on the ask's pointer tuples are its write's, dated by its row" \
    comment "$on_ask" comments "$comment_kinds" "'comment.created'"
}

# item_blocks SECTION ITEM-KIND ID TABLE COLUMN: every current block of the item is a live `block`
# node whose one parent is the item, read as `<address>=<node kind>/<state>/<parents>` in body order.
item_blocks() {
  local section="$1" kind="$2" id="$3" table="$4" column="$5" want="" count index
  query "$section" "count $kind $id's blocks" \
    "select count(*) from $table where $column = '$id' and position is not null"
  count="$answer"
  [[ "$count" -gt 0 ]] || fail "$section" "the $kind's blocks" "$table holds no current block of $id"
  for ((index = 1; index <= count; index++)); do
    want+="${want:+ }$kind/$id#b$index=block/live/$kind:$id"
  done
  want_value "$section" "each of the $kind's $count body blocks is a block node whose parent is the $kind" "$want" "
    select string_agg(b.node_id || '=' || coalesce(n.kind || '/' || n.state, '(no node)') || '/'
                      || coalesce(parents.ends, '(none)'), ' ' order by b.position)
    from $table b
    left join graph_nodes n on n.kind = 'block' and n.id = b.node_id
    left join lateral (
      select string_agg(latest.to_kind || ':' || latest.to_id, ',' order by latest.to_kind, latest.to_id) as ends
      from (select distinct on (to_kind, to_id) to_kind, to_id, asserted from graph_tuples
            where from_kind = 'block' and from_id = b.node_id and kind = 'part_of'
            order by to_kind, to_id, seq desc) latest
      where latest.asserted) parents on true
    where b.$column = '$id' and b.position is not null"
}

# block_link SECTION ITEM-KIND ID TABLE COLUMN: the item's first block holds its citation of the
# cited issue as a link row naming the block's node, read as `<link kind>:<node kind>:<asserted>`,
# and the store's latest statement of that link, out of the block node, asserts it.
block_link() {
  want_value "$1" "a block of the $2 links $cited, as a tuple out of its block node" "mentions:block:true" "
    select coalesce(string_agg(r.kind || ':' || coalesce(r.node_kind, '(none)') || ':'
                               || coalesce((select t.asserted::text from graph_tuples t
                                            where t.from_kind = 'block' and t.from_id = r.from_node
                                              and t.kind = r.kind and t.to_kind = r.to_kind and t.to_id = r.to_id
                                            order by t.seq desc limit 1), '(no tuple)'), ','), '(no link row)')
    from $4 r
    where r.$5 = '$3' and r.from_node = '$2/$3#b1' and r.to_kind = 'issue' and r.to_id = '$cited'"
}

blocks_section() {
  local section=blocks
  item_blocks "$section" comment "$root" comment_blocks comment_id
  item_blocks "$section" message "$message" message_blocks message_id
  item_blocks "$section" ask "$ask" ask_blocks ask_id
  # The absence, beside the same read finding this run's block nodes of each item sort: a read
  # that matched nothing would read 0 on both sides.
  want_value "$section" "the store holds no ask_block, comment_block or message_block node, beside this run's item block nodes" \
    "0|true|true|true" "
    select count(*) filter (where kind in ('ask_block', 'comment_block', 'message_block')) || '|'
           || (count(*) filter (where kind = 'block' and id like 'comment/$root#%') > 0)::text || '|'
           || (count(*) filter (where kind = 'block' and id like 'message/$message#%') > 0)::text || '|'
           || (count(*) filter (where kind = 'block' and id like 'ask/$ask#%') > 0)::text
    from graph_nodes"
  want_value "$section" "no tuple of the store starts or ends at an ask_block, comment_block or message_block node" 0 "
    select count(*) from graph_tuples
    where from_kind in ('ask_block', 'comment_block', 'message_block')
       or to_kind in ('ask_block', 'comment_block', 'message_block')"
  block_link "$section" comment "$root" comment_block_refs comment_id
  block_link "$section" message "$message" message_block_refs message_id
  block_link "$section" ask "$ask" ask_block_refs ask_id
}

agent_page_section() {
  local section=agent_page
  names_its_node "$section" "the message of no issue is a node its row names" message "$direct" messages
  want_out "$section" "the message of no issue sits on $agent's page" message "$direct" "$message_kinds" \
    "part_of>page:$agent_page" "addressed_to>session:$agent" "authored_by>person:$person"
  want_value "$section" "its part_of was written by the transaction that wrote the message node and its row" \
    "true|true|true" "
    select (t.xmin = n.xmin)::text || '|' || (t.xmin = m.xmin)::text || '|'
           || (t.source_event = n.source_event and t.at = m.created_at)::text
    from graph_tuples t
    join graph_nodes n on n.kind = 'message' and n.id = t.from_id
    join messages m on m.id::text = t.from_id
    where t.from_kind = 'message' and t.from_id = '$direct' and t.kind = 'part_of'
      and t.to_kind = 'page' and t.to_id = '$agent_page' and t.asserted"
  want_value "$section" "$agent's page is a live node of the store" live \
    "select coalesce(max(state), '(no node)') from graph_nodes where kind = 'page' and id = '$agent_page'"

  names_its_node "$section" "$agent's reply is a node its row names" message "$direct_reply" messages
  want_out "$section" "$agent's reply sits under the message it answers" message "$direct_reply" "$message_kinds" \
    "replies_to>message:$direct" "part_of>message:$direct" "addressed_to>session:$agent" \
    "authored_by>session:$agent"
  # Every part_of the store ever held out of each, retractions among them, so a page parent that
  # was stated and then ended is read too. The message the reply answers holds one to the page,
  # read by the same statement, so a zero for the reply is not a read that matched nothing.
  want_value "$section" "the reply was never given the page as its parent, as the message it answers was" "0|1" "
    select count(*) filter (where from_id = '$direct_reply') || '|' || count(*) filter (where from_id = '$direct')
    from graph_tuples
    where from_kind = 'message' and from_id in ('$direct_reply', '$direct') and kind = 'part_of'
      and to_kind = 'page'"
  want_value "$section" "the issue message sent to $agent holds one part_of, to $issue, and none to $agent's page" \
    "1|1|0" "
    select count(*) || '|' || count(*) filter (where to_kind = 'issue' and to_id = '$issue') || '|'
           || count(*) filter (where to_kind = 'page')
    from graph_tuples where from_kind = 'message' and from_id = '$message' and kind = 'part_of'"
}

# The states of an attempt the listener took (model.DeliveryTakenStatesSQL).
readonly taken_states="('sent', 'queued')"

# fake_envoy_sends STATUS: has the harness's fake Envoy listener answer every send to $agent with
# STATUS (200, or the 503 of a listener behind its starting gate), and reads its answer back. A
# section runs in a subshell of its own, so whether the listener is left refusing is a file,
# $work/envoy-refusing, which the cleanup reads to put it back.
fake_envoy_sends() {
  local answer_status
  [[ "$1" == 200 ]] || : >"$work/envoy-refusing"
  answer_status="$(curl -sS -X PATCH -o "$work/envoy" -w '%{http_code}' -H 'Content-Type: application/json' \
    --data-binary "{\"send_status\": $1}" "$fake_envoy/__fixture/sessions/$(q "$agent")" 2>"$work/curl")" ||
    answer_status="curl failed: $(<"$work/curl")"
  if [[ "$answer_status" != 200 ]] || ! jq -e --argjson s "$1" '.send_status == $s' "$work/envoy" >/dev/null 2>&1; then
    printf 'the fake Envoy at %s answered %s: %s\n' "$fake_envoy" "$answer_status" "$(head -c 300 "$work/envoy" 2>/dev/null)"
    return 1
  fi
  [[ "$1" != 200 ]] || rm -f "$work/envoy-refusing"
}

# deliveries_to_agent MESSAGE: the SQL that counts the attempts of MESSAGE whose node states a
# delivered_to $agent now, each tuple's latest statement being the one that counts.
deliveries_to_agent() {
  printf '%s' "select count(*) from (
      select distinct on (t.from_id) t.asserted
      from graph_tuples t
      join message_deliveries d on d.node_kind = t.from_kind and d.node_address = t.from_id
      where d.message_id = '$1' and t.from_kind = 'delivery_attempt' and t.kind = 'delivered_to'
        and t.to_kind = 'session' and t.to_id = '$agent'
      order by t.from_id, t.seq desc) latest
    where latest.asserted"
}

# in_conversation SECTION WHAT MESSAGE: $agent's session-conversation read, read as $person, holds
# MESSAGE once, as a root.
in_conversation() {
  request person GET "/api/v1/agents/$(q "$agent")/conversation"
  want_status 200 "$1" "$2"
  expect "$1" "$2" 1 --arg m "$3" '[.[] | select(.message.id == $m)] | length'
}

conversation_section() {
  local section=conversation
  # A delivery the listener took: the issue message the seed sent $agent while the fake Envoy
  # answered its sends.
  settles "$section" "the listener took the issue message $message's delivery to $agent" "true" "
    select coalesce((select state in $taken_states from message_deliveries
                     where message_id = '$message' order by attempt desc limit 1)::text, '(no attempt)')"
  settles "$section" "that attempt states a delivered_to $agent" 1 "$(deliveries_to_agent "$message")"
  in_conversation "$section" "$agent's conversation holds $message, read through its delivery" "$message"

  # An attempt the listener did not take: the fake Envoy answers every send to $agent 503 while a
  # second issue message is sent it, so the sender waits out its take-over window and records the
  # attempt failed, which states no delivered_to. The listener answers again before the read.
  fake_envoy_sends 503 >"$work/envoy.out" ||
    fail "$section" "have the fake Envoy refuse sends to $agent" "$(<"$work/envoy.out")"
  pass "$section" "the fake Envoy answers every send to $agent 503"
  post "$section" "an issue message sent to $agent while its listener refuses it" agent "/api/v1/issues/$issue/messages" \
    "$(as_session "$(jq -cn --arg t "session:$agent" '{body: "Sent while the listener was starting.", target: $t, delivery: "btw"}')")"
  local refused="$id"
  settles "$section" "the attempt to deliver $refused settled failed, with the listener's refusal" "failed|true" "
    select coalesce((select state || '|' || (coalesce(error, '') like 'envoy listener: %')::text
                     from message_deliveries where message_id = '$refused' order by attempt desc limit 1),
                    '(no attempt)')"
  fake_envoy_sends 200 >"$work/envoy.out" ||
    fail "$section" "have the fake Envoy answer sends to $agent again" "$(<"$work/envoy.out")"
  pass "$section" "the fake Envoy answers sends to $agent again"
  want_value "$section" "no attempt of $refused states a delivered_to $agent" 0 "$(deliveries_to_agent "$refused")"
  in_conversation "$section" "$agent's conversation holds $refused, whose one attempt the listener did not take" "$refused"

  # The retry the listener takes, which states the delivery the failed attempt did not.
  send POST "/api/v1/messages/$refused/deliveries" "$(as_session '{"delivery": "btw"}')"
  want_status 201 "$section" "retry delivering $refused"
  expect "$section" "the retry is $refused's second attempt" 2 '.attempt'
  settles "$section" "the listener took the retry" "true" "
    select coalesce((select state in $taken_states from message_deliveries
                     where message_id = '$refused' and attempt = 2)::text, '(no attempt)')"
  settles "$section" "the retry states a delivered_to $agent" 1 "$(deliveries_to_agent "$refused")"
  in_conversation "$section" "$agent's conversation still holds $refused once" "$refused"

  # The conversation read answers the rows GET .../messages answers for $agent. A write another
  # caller makes to $agent between the two reads moves one of them, so the pair is read again, up
  # to five times, before a difference fails.
  local pair=0 rows
  while true; do
    pair=$((pair + 1))
    request person GET "/api/v1/agents/$(q "$agent")/conversation"
    want_status 200 "$section" "read $agent's conversation ($pair)"
    jq -S . "$work/body" >"$work/conversation.json" || fail "$section" "read $agent's conversation ($pair)" "$(body)"
    request person GET "/api/v1/agents/$(q "$agent")/messages"
    want_status 200 "$section" "read $agent's messages ($pair)"
    jq -S . "$work/body" >"$work/messages.json" || fail "$section" "read $agent's messages ($pair)" "$(body)"
    cmp -s "$work/conversation.json" "$work/messages.json" && break
    [[ "$pair" -lt 5 ]] ||
      fail "$section" "GET /api/v1/agents/$agent/conversation answers the rows GET /api/v1/agents/$agent/messages answers" \
        "five pairs of reads differed; the last: conversation roots $(jq -c '[.[].message.id]' "$work/conversation.json"), messages roots $(jq -c '[.[].message.id]' "$work/messages.json"), first differing line: $(diff "$work/conversation.json" "$work/messages.json" | sed -n 2p | head -c 300)"
    sleep 1
  done
  rows="$(jq 'length' "$work/conversation.json")"
  [[ "$rows" -gt 0 ]] || fail "$section" "the two reads of $agent answer rows" "both answered none"
  pass "$section" "GET /api/v1/agents/$agent/conversation answers the $rows rows GET /api/v1/agents/$agent/messages answers (pair $pair)"
}

# retract_arm NAME TUPLE-FROM-KIND FROM-ID KIND TUPLE-TO-KIND TO-ID EDGE-FROM-KIND EDGE-TO-KIND
#   COLUMN COLUMN-SQL: reads graph_edges answer the relation once (from EDGE-FROM-KIND to
#   EDGE-TO-KIND, the address forms the view renders the store's kinds under), appends the
#   retraction of its tuple, reads graph_edges answer it no more while COLUMN, which COLUMN-SQL
#   counts the row of, still names it, states the tuple again as it stood - its time, its writer
#   and its legacy sequence - and reads graph_edges answer it once again. The retraction and the
#   statement carry the sentinel event, which no event records: a later write that ends a relation
#   would append the first. The three counts are one assertion.
retract_arm() {
  local name="$1" tuple_from="$2" from="$3" kind="$4" tuple_to="$5" to="$6" edge_from="$7" edge_to="$8"
  local column="$9" column_sql="${10}" edge tuple before after restated hint=""
  edge="from_kind = '$edge_from' and from_id = '$from' and kind = '$kind' and to_kind = '$edge_to' and to_id = '$to'"
  tuple="from_kind = '$tuple_from' and from_id = '$from' and kind = '$kind' and to_kind = '$tuple_to' and to_id = '$to'"
  query retractions "$name: read it before the retraction" "select count(*) from graph_edges where $edge"
  before="$answer"
  query retractions "$name: retract its tuple" "
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
    values ('$tuple_from', '$from', '$kind', '$tuple_to', '$to', clock_timestamp(), '$actor'::jsonb, 0, false)"
  query retractions "$name: read it after the retraction" \
    "select (select count(*) from graph_edges where $edge) || '|' || ($column_sql)"
  after="$answer"
  query retractions "$name: state its tuple again" "
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
    select from_kind, from_id, kind, to_kind, to_id, at, who, 0, legacy_source_seq, true
    from graph_tuples where $tuple and asserted order by seq desc limit 1"
  query retractions "$name: read it once its tuple is stated again" "select count(*) from graph_edges where $edge"
  restated="$answer"
  [[ "${after%%|*}" == 0 ]] || hint=": an arm over $column answers it while its tuple is retracted"
  [[ "$before|$after|$restated" == "1|0|1|1" ]] ||
    fail retractions "$name" "graph_edges answered it $before time(s) before its tuple was retracted, ${after%%|*} after while $column named it in ${after#*|} row(s), and $restated once the tuple was stated again; want 1, 0 while 1 row named it, and 1$hint"
  pass retractions "$name: graph_edges answers it from its tuple alone (1 before the retraction, 0 after while $column still names it, 1 once stated again)"
}

retractions_section() {
  local failed=0 arm
  local -a arms=(
    "comment_blocks part_of|block|comment/$root#b1|part_of|comment|$root|comment_block|comment|comment_blocks|select count(*) from comment_blocks where comment_id = '$root' and node_id = 'comment/$root#b1' and position is not null"
    "message_blocks part_of|block|message/$message#b1|part_of|message|$message|message_block|message|message_blocks|select count(*) from message_blocks where message_id = '$message' and node_id = 'message/$message#b1' and position is not null"
    "ask_blocks part_of|block|ask/$ask#b1|part_of|ask|$ask|ask_block|ask|ask_blocks|select count(*) from ask_blocks where ask_id = '$ask' and node_id = 'ask/$ask#b1' and position is not null"
    "comment_block_refs link|block|comment/$root#b1|mentions|issue|$cited|comment_block|issue|comment_block_refs|select count(*) from comment_block_refs where comment_id = '$root' and from_node = 'comment/$root#b1' and to_kind = 'issue' and to_id = '$cited'"
    "message_block_refs link|block|message/$message#b1|mentions|issue|$cited|message_block|issue|message_block_refs|select count(*) from message_block_refs where message_id = '$message' and from_node = 'message/$message#b1' and to_kind = 'issue' and to_id = '$cited'"
    "ask_block_refs link|block|ask/$ask#b1|mentions|issue|$cited|ask_block|issue|ask_block_refs|select count(*) from ask_block_refs where ask_id = '$ask' and from_node = 'ask/$ask#b1' and to_kind = 'issue' and to_id = '$cited'"
    "comment replies_to comment_block|comment|$leaf|replies_to|block|comment/$reply#b2|comment|comment_block|comments.replies_to_block|select count(*) from comments where id = '$leaf' and replies_to_block = 'comment/$reply#b2'"
    "comment replies_to ask_block|comment|$on_ask|replies_to|block|ask/$ask#b1|comment|ask_block|comments.replies_to_block|select count(*) from comments where id = '$on_ask' and replies_to_block = 'ask/$ask#b1'"
    "message replies_to message_block|message|$message_reply|replies_to|block|message/$message#b2|message|message_block|messages.replies_to_block|select count(*) from messages where id = '$message_reply' and replies_to_block = 'message/$message#b2'"
    "comment replies_to comment|comment|$reply|replies_to|comment|$root|comment|comment|comments.reply_to|select count(*) from comments where id = '$reply' and reply_to = '$root'"
    "comment answers comment|comment|$leaf|answers|comment|$reply|comment|comment|comments.answers|select count(*) from comments where id = '$leaf' and answers = '$reply'"
    "comment replies_to ask|comment|$on_ask|replies_to|ask|$ask|comment|ask|comments.ask_id|select count(*) from comments where id = '$on_ask' and ask_id = '$ask'"
    "message replies_to message|message|$message_reply|replies_to|message|$message|message|message|messages.in_reply_to|select count(*) from messages where id = '$message_reply' and in_reply_to = '$message'"
    "the comment's citation|comment|$root|mentions|issue|$cited|comment|issue|its refs row|select count(*) from refs where from_kind = 'comment' and from_id = '$root' and to_kind = 'issue' and to_id = '$cited' and kind = 'mentions'"
    "the message's citation|message|$message|mentions|issue|$cited|message|issue|its refs row|select count(*) from refs where from_kind = 'message' and from_id = '$message' and to_kind = 'issue' and to_id = '$cited' and kind = 'mentions'"
    "the ask's citation|ask|$ask|mentions|issue|$cited|ask|issue|its refs row|select count(*) from refs where from_kind = 'ask' and from_id = '$ask' and to_kind = 'issue' and to_id = '$cited' and kind = 'mentions'"
  )
  for arm in "${arms[@]}"; do
    local -a fields
    IFS='|' read -r -a fields <<<"$arm"
    isolated retract_arm "${fields[@]}"
    [[ "$isolated_status" == 0 ]] || failed=1
  done
  return "$failed"
}

# The rows the release before this one writes, planted by older_rows_section: two comments, a
# message and an ask, their ids drawn once the section starts.
older_comment="" older_ask_comment="" older_message="" older_ask=""

# node_as_the_release_before KIND ID LIKE AT: the SQL that writes node KIND/ID as the release
# before this one writes one of its kind - the node of LIKE, a node of the same kind on the same
# issue, with its own id, time and writer, and the value tuples every node of the store carries -
# at the sentinel event, which no event of this run records.
node_as_the_release_before() {
  printf '%s\n' "
    insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
    select kind, '$2', supertag, visibility, state, attributes, $4, '$actor'::jsonb, 0, project_key, parties
    from graph_nodes where kind = '$1' and id = '$3';
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
    select from_kind, '$2', kind, to_kind, to_id, $4, '$actor'::jsonb, 0, true
    from graph_tuples
    where from_kind = '$1' and from_id = '$3' and to_kind = 'value' and kind in ('visibility', 'state', 'project')
      and asserted;"
}

# blocks_as_the_release_before KIND ID TEXT CITES: the SQL that writes the one block of item
# KIND/ID holding TEXT, and, when CITES is set, that block's link to the cited issue and the item's
# citation of it, as the release before this one's block index writes them in the item's own
# transaction: no row naming a node, no citation holding tuples_stored. It settles the item's
# block index and its re-index queue row as that index does (refs.IndexItem's recordItemIndexed,
# refs' settleOwed), so this build's tick reads nothing owed and leaves the rows as they were
# written; the window marker is settled too, for the same reason.
blocks_as_the_release_before() {
  local kind="$1" id="$2" text="$3" cites="$4" sum
  sum="encode(sha256(convert_to('$text', 'UTF8')), 'hex')"
  case "$kind" in
    comment)
      printf '%s\n' "
        insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
                                    created_at, made_at, changed_revision, changed_at)
        values ('$id', 'b1', 'comment/$id#b1', 0, 'paragraph', $sum, '$text', now(), now(), 1, now());
        insert into comment_block_texts (comment_id, block_id, revision, text) values ('$id', 'b1', 1, '$text');
        update comment_block_state set revision = 1, next_block = 2, body_sha256 = $sum,
                                       owed_at = null, windows_owed_at = null
        where comment_id = '$id';"
      ;;
    message)
      printf '%s\n' "
        insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt,
                                    created_at, made_at, changed_at)
        values ('$id', 'b1', 'message/$id#b1', 0, 'paragraph', $sum, '$text', now(), now(), now());
        update message_block_state set body_sha256 = $sum, owed_at = null, windows_owed_at = null
        where message_id = '$id';"
      ;;
    ask)
      printf '%s\n' "
        insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt,
                                created_at, made_at, changed_revision, changed_at)
        values ('$id', 'b1', 'ask/$id#b1', 0, 'question', $sum, '$text', now(), now(), 1, now());
        insert into ask_block_texts (ask_id, block_id, revision, text) values ('$id', 'b1', 1, '$text');
        update ask_block_state set revision = 1, next_block = 2, body_sha256 = $sum,
                                   owed_at = null, windows_owed_at = null
        where ask_id = '$id';"
      ;;
  esac
  if [[ -n "$cites" ]]; then
    printf '%s\n' "
      insert into ${kind}_block_refs (${kind}_id, block_id, from_node, to_kind, to_id, kind, created_at)
      values ('$id', 'b1', '$kind/$id#b1', 'issue', '$cited', 'mentions', now());
      insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at)
      values ('$kind', '$id', 'issue', '$cited', 'mentions', now());"
  fi
  printf '%s\n' "delete from refs_grammar_owed where kind = '$kind' and id = '$id';"
}

# The server's ticks state what a build before this child wrote once they see it: an insert into
# comments, asks or messages queues its item in refs_forms_owed (migrations 20261117025901-03),
# and the refs forms drain re-indexes each queued item (docs' RunRefsFormsReconciliation), which
# writes its blocks' nodes and tuples; and the block index's object pass gives an ask and a message
# the node their row names none of. A row written as that build writes it is therefore read by its
# old arm only before any other process sees it: older_rows_section and backfill_section read each
# such row in the transaction that writes it (in_one_transaction).

# in_one_transaction SECTION WHAT END WRITE READ...: runs WRITE and then each READ, a select of one
# value, in one psql session and one transaction that ends with END (`commit` or `rollback`), each
# read's value one line of $work/reads, in order. No other process sees what WRITE wrote before
# every READ has read it.
in_one_transaction() {
  local section="$1" what="$2" end="$3" write="$4" read
  shift 4
  local -a commands=(-c begin -c "$write")
  for read in "$@"; do
    commands+=(-c "$read")
  done
  commands+=(-c "$end")
  env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -Atq -v ON_ERROR_STOP=1 \
    "${commands[@]}" >"$work/reads" 2>"$work/psql" || fail "$section" "$what" "psql: $(<"$work/psql")"
  [[ "$(wc -l <"$work/reads")" == "$#" ]] ||
    fail "$section" "$what" "read $(wc -l <"$work/reads") value(s), want $#: $(<"$work/reads")"
}

# arm_sql EDGE TUPLE MARKER-SQL: the select of `<how often graph_edges answers the relation EDGE
# names>|<how many tuples TUPLE names>|<what the row reads through MARKER-SQL>`.
arm_sql() {
  printf '%s' "select (select count(*) from graph_edges where $1) || '|' || (select count(*) from graph_tuples where $2)
           || '|' || coalesce(($3), '(no row)')"
}

# read_arm SECTION WHAT WANT GOT: a value in_one_transaction read, GOT, is WANT. One assertion.
read_arm() {
  [[ "$4" == "$3" ]] || fail "$1" "$2" "read '$4', want '$3'"
  pass "$1" "$2"
}

older_rows_section() {
  local failed=0 arm index=1 read
  local -a fields reads planted
  query older_rows "draw the ids of the rows the release before writes" \
    "select gen_random_uuid() || ' ' || gen_random_uuid() || ' ' || gen_random_uuid() || ' ' || gen_random_uuid()"
  read -r older_comment older_ask_comment older_message older_ask <<<"$answer"
  local comment_text="Written by the release before, $citation."
  local ask_comment_text="On the question, by the release before."
  local message_text="Written by the release before, $citation."
  local ask_text="Asked by the release before, $citation?"
  # The two comments as the node store's writer writes one: its node first, then the row
  # naming it, with no tuples_stored, which is this child's and which that writer never sets, and
  # its author's authored_by. The message and the ask as the release before writes them: a node of
  # the store (graphstore.MessageNode, graphstore.AskNode) that the row names nowhere, since that
  # release's rows carry no node pair, and no pointer as a tuple.
  local write="
    $(node_as_the_release_before comment "$older_comment" "$root" 'now()')
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
    values ('comment', '$older_comment', 'authored_by', 'session', '$session', now(), '$actor'::jsonb, 0, true);
    insert into comments (id, issue_key, author, body, reply_to, answers, replies_to_block, created_at,
                          node_kind, node_address)
    values ('$older_comment', '$issue', '$actor'::jsonb, '$comment_text', '$root', '$reply',
            'comment/$reply#b1', now(), 'comment', '$older_comment');
    $(blocks_as_the_release_before comment "$older_comment" "$comment_text" cites)
    $(node_as_the_release_before comment "$older_ask_comment" "$on_ask" 'now()')
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
    values ('comment', '$older_ask_comment', 'authored_by', 'session', '$session', now(), '$actor'::jsonb, 0, true);
    insert into comments (id, issue_key, author, body, ask_id, replies_to_block, created_at, node_kind, node_address)
    values ('$older_ask_comment', '$issue', '$actor'::jsonb, '$ask_comment_text', '$ask', 'ask/$ask#b1', now(),
            'comment', '$older_ask_comment');
    $(blocks_as_the_release_before comment "$older_ask_comment" "$ask_comment_text" '')
    $(node_as_the_release_before message "$older_message" "$message" 'now()')
    insert into messages (id, issue_key, author, body, in_reply_to, replies_to_block, created_at)
    values ('$older_message', '$issue', '$actor'::jsonb, '$message_text', '$message', 'message/$message#b1', now());
    $(blocks_as_the_release_before message "$older_message" "$message_text" cites)
    $(node_as_the_release_before ask "$older_ask" "$ask" 'now()')
    insert into asks (id, issue_key, author, question, created_at)
    values ('$older_ask', '$issue', '$actor'::jsonb, '$ask_text', now());
    $(blocks_as_the_release_before ask "$older_ask" "$ask_text" cites)"
  reads=("
    select (select string_agg(coalesce(c.node_kind, '(none)') || ':' || coalesce(n.state, '(no node)') || ':'
                              || c.tuples_stored::text, '|' order by c.body)
            from comments c left join graph_nodes n on n.kind = c.node_kind and n.id = c.node_address
            where c.id in ('$older_comment', '$older_ask_comment'))
           || '|' || (select coalesce(node_kind, '(none)') from messages where id = '$older_message')
           || '|' || (select coalesce(node_kind, '(none)') || ':' || coalesce(tuples_stored_at::text, '(none)')
                      from asks where id = '$older_ask')")

  local -a arms=(
    "comment_blocks part_of|from_kind = 'comment_block' and from_id = 'comment/$older_comment#b1' and kind = 'part_of' and to_kind = 'comment' and to_id = '$older_comment'|from_kind = 'block' and from_id = 'comment/$older_comment#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from comment_blocks where comment_id = '$older_comment'|unmarked"
    "message_blocks part_of|from_kind = 'message_block' and from_id = 'message/$older_message#b1' and kind = 'part_of' and to_kind = 'message' and to_id = '$older_message'|from_kind = 'block' and from_id = 'message/$older_message#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from message_blocks where message_id = '$older_message'|unmarked"
    "ask_blocks part_of|from_kind = 'ask_block' and from_id = 'ask/$older_ask#b1' and kind = 'part_of' and to_kind = 'ask' and to_id = '$older_ask'|from_kind = 'block' and from_id = 'ask/$older_ask#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from ask_blocks where ask_id = '$older_ask'|unmarked"
    "comment_block_refs link|from_kind = 'comment_block' and from_id = 'comment/$older_comment#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'comment/$older_comment#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from comment_block_refs where comment_id = '$older_comment'|unmarked"
    "message_block_refs link|from_kind = 'message_block' and from_id = 'message/$older_message#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'message/$older_message#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from message_block_refs where message_id = '$older_message'|unmarked"
    "ask_block_refs link|from_kind = 'ask_block' and from_id = 'ask/$older_ask#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'ask/$older_ask#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from ask_block_refs where ask_id = '$older_ask'|unmarked"
    "comment replies_to comment_block|from_kind = 'comment' and from_id = '$older_comment' and kind = 'replies_to' and to_kind = 'comment_block' and to_id = 'comment/$reply#b1'|from_kind = 'comment' and from_id = '$older_comment' and kind = 'replies_to' and to_kind = 'block'|select tuples_stored::text from comments where id = '$older_comment'|false"
    "comment replies_to ask_block|from_kind = 'comment' and from_id = '$older_ask_comment' and kind = 'replies_to' and to_kind = 'ask_block' and to_id = 'ask/$ask#b1'|from_kind = 'comment' and from_id = '$older_ask_comment' and kind = 'replies_to' and to_kind = 'block'|select tuples_stored::text from comments where id = '$older_ask_comment'|false"
    "message replies_to message_block|from_kind = 'message' and from_id = '$older_message' and kind = 'replies_to' and to_kind = 'message_block' and to_id = 'message/$message#b1'|from_kind = 'message' and from_id = '$older_message' and kind = 'replies_to' and to_kind = 'block'|select coalesce(node_kind, 'unmarked') from messages where id = '$older_message'|unmarked"
    "comment replies_to comment|from_kind = 'comment' and from_id = '$older_comment' and kind = 'replies_to' and to_kind = 'comment' and to_id = '$root'|from_kind = 'comment' and from_id = '$older_comment' and kind = 'replies_to' and to_kind = 'comment'|select tuples_stored::text from comments where id = '$older_comment'|false"
    "comment answers comment|from_kind = 'comment' and from_id = '$older_comment' and kind = 'answers' and to_kind = 'comment' and to_id = '$reply'|from_kind = 'comment' and from_id = '$older_comment' and kind = 'answers'|select tuples_stored::text from comments where id = '$older_comment'|false"
    "comment replies_to ask|from_kind = 'comment' and from_id = '$older_ask_comment' and kind = 'replies_to' and to_kind = 'ask' and to_id = '$ask'|from_kind = 'comment' and from_id = '$older_ask_comment' and kind = 'replies_to' and to_kind = 'ask'|select tuples_stored::text from comments where id = '$older_ask_comment'|false"
    "message replies_to message|from_kind = 'message' and from_id = '$older_message' and kind = 'replies_to' and to_kind = 'message' and to_id = '$message'|from_kind = 'message' and from_id = '$older_message' and kind = 'replies_to' and to_kind = 'message'|select coalesce(node_kind, 'unmarked') from messages where id = '$older_message'|unmarked"
    "the comment's citation, by the refs arm|from_kind = 'comment' and from_id = '$older_comment' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'comment' and from_id = '$older_comment' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'comment' and from_id = '$older_comment' and to_id = '$cited'|false"
    "the message's citation, by the refs arm|from_kind = 'message' and from_id = '$older_message' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'message' and from_id = '$older_message' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'message' and from_id = '$older_message' and to_id = '$cited'|false"
    "the ask's citation, by the refs arm|from_kind = 'ask' and from_id = '$older_ask' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'ask' and from_id = '$older_ask' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'ask' and from_id = '$older_ask' and to_id = '$cited'|false"
  )
  for arm in "${arms[@]}"; do
    IFS='|' read -r -a fields <<<"$arm"
    reads+=("$(arm_sql "${fields[1]}" "${fields[2]}" "${fields[3]}")")
  done
  # Written and read in one transaction, which then rolls back: nothing of it outlives the read.
  in_one_transaction older_rows "write two comments, a message and an ask as the release before writes them, and read them back" \
    rollback "$write" "${reads[@]}"
  pass older_rows "wrote comments $older_comment and $older_ask_comment, message $older_message and ask $older_ask as the release before writes them, and read them in the transaction that wrote them, which then rolled back"
  read="$(sed -n 1p "$work/reads")"
  isolated read_arm older_rows "the comments carry their node and hold no tuples_stored; the message and the ask name no node" \
    "comment:live:false|comment:live:false|(none)|(none):(none)" "$read"
  [[ "$isolated_status" == 0 ]] || failed=1
  for arm in "${arms[@]}"; do
    IFS='|' read -r -a fields <<<"$arm"
    index=$((index + 1))
    read="$(sed -n "${index}p" "$work/reads")"
    isolated read_arm older_rows "${fields[0]}: the row the release before wrote is answered once, by its old arm" \
      "1|0|${fields[4]}" "$read"
    [[ "$isolated_status" == 0 ]] || failed=1
  done
  planted=("$older_comment" "$older_ask_comment" "$older_message" "$older_ask")
  want_value older_rows "the rows the release before stood in for are gone with the transaction that wrote them" 0 "
    select (select count(*) from graph_nodes where id in ($(sql_list "${planted[@]}")))
         + (select count(*) from graph_tuples where from_id in ($(sql_list "${planted[@]}")))
         + (select count(*) from refs where from_id in ($(sql_list "${planted[@]}")))
         + (select count(*) from comments where id::text in ($(sql_list "${planted[@]}")))
         + (select count(*) from messages where id::text in ($(sql_list "${planted[@]}")))
         + (select count(*) from asks where id::text in ($(sql_list "${planted[@]}")))"
  return "$failed"
}

# The rows written before this child that backfill_section plants: two comments, a message and an
# ask, their ids drawn once the section starts.
before_comment="" before_ask_comment="" before_message="" before_ask=""

# remove_backfilled_rows: removes every row backfill_section planted and what the backfill stated
# of them - the items, which take their blocks, links, texts and block state with them, their
# citations, the store's nodes of them and of their blocks and every tuple out of or into
# either - so no later run reads them.
remove_backfilled_rows() {
  local ids blocks
  [[ -n "$before_comment" ]] || return 0
  ids="$(sql_list "$before_comment" "$before_ask_comment" "$before_message" "$before_ask")"
  blocks="$(sql_list "comment/$before_comment#%" "comment/$before_ask_comment#%" "message/$before_message#%" "ask/$before_ask#%")"
  env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -Atq -v ON_ERROR_STOP=1 -c "
    begin;
    delete from comments where id in ('$before_comment', '$before_ask_comment');
    delete from messages where id = '$before_message';
    delete from asks where id = '$before_ask';
    delete from refs where from_id in ($ids);
    delete from graph_tuples where from_id in ($ids) or to_id in ($ids) or from_id like any (array[$blocks]);
    delete from graph_nodes where id in ($ids) or id like any (array[$blocks]);
    commit;" >/dev/null 2>"$work/remove" ||
    printf 'one-inbox-kinds.sh: could not remove the rows the backfill section planted: %s\n' "$(<"$work/remove")" >&2
}

# backfilled_arm NAME EDGE TUPLE MARKER-SQL BEFORE AFTER: after this child's backfill, graph_edges
# answers the relation EDGE names once, the store holds the one tuple TUPLE names, and the row reads
# AFTER through MARKER-SQL, so the tuple arm answers it and the old arm answers it no more.
# backfill_section reads BEFORE in the transaction that wrote the row. One assertion.
backfilled_arm() {
  want_value backfill "$1: the row written before this child is answered after it, once, by the tuple arm" "1|1|$6" \
    "$(arm_sql "$2" "$3 and asserted" "$4")"
}

backfill_section() {
  local failed=0 arm index=0 read
  local -a fields reads
  command -v envoy-dispatch >/dev/null ||
    fail backfill "find envoy-dispatch on PATH" "no envoy-dispatch: build packages/envoy/cmd/dispatch onto PATH, as the Verification preamble does"
  trap remove_backfilled_rows EXIT
  query backfill "draw the ids of the rows written before this child" \
    "select gen_random_uuid() || ' ' || gen_random_uuid() || ' ' || gen_random_uuid() || ' ' || gen_random_uuid()"
  read -r before_comment before_ask_comment before_message before_ask <<<"$answer"
  local comment_text="Written before this child, $citation."
  local ask_comment_text="On the question, before this child."
  local message_text="Written before this child, $citation."
  local ask_text="Asked before this child, $citation?"
  # Each row as a build before this child writes it: a comment with no node, which
  # the node store's backfill gives it, and a message and an ask with no node either, which
  # the object-node backfill gives them; none holds a marker of this child's, and their triggers
  # project them into user_threads as that build's do.
  local write="
    insert into comments (id, issue_key, author, body, reply_to, answers, replies_to_block, created_at)
    values ('$before_comment', '$issue', '$actor'::jsonb, '$comment_text', '$root', '$reply', 'comment/$reply#b1', now());
    $(blocks_as_the_release_before comment "$before_comment" "$comment_text" cites)
    insert into comments (id, issue_key, author, body, ask_id, replies_to_block, created_at)
    values ('$before_ask_comment', '$issue', '$actor'::jsonb, '$ask_comment_text', '$ask', 'ask/$ask#b1', now());
    $(blocks_as_the_release_before comment "$before_ask_comment" "$ask_comment_text" '')
    insert into messages (id, issue_key, author, body, in_reply_to, replies_to_block, created_at)
    values ('$before_message', '$issue', '$actor'::jsonb, '$message_text', '$message', 'message/$message#b1', now());
    $(blocks_as_the_release_before message "$before_message" "$message_text" cites)
    insert into asks (id, issue_key, author, question, created_at)
    values ('$before_ask', '$issue', '$actor'::jsonb, '$ask_text', now());
    $(blocks_as_the_release_before ask "$before_ask" "$ask_text" cites)"

  local -a arms=(
    "comment_blocks part_of|from_kind = 'comment_block' and from_id = 'comment/$before_comment#b1' and kind = 'part_of' and to_kind = 'comment' and to_id = '$before_comment'|from_kind = 'block' and from_id = 'comment/$before_comment#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from comment_blocks where comment_id = '$before_comment'|unmarked|block"
    "message_blocks part_of|from_kind = 'message_block' and from_id = 'message/$before_message#b1' and kind = 'part_of' and to_kind = 'message' and to_id = '$before_message'|from_kind = 'block' and from_id = 'message/$before_message#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from message_blocks where message_id = '$before_message'|unmarked|block"
    "ask_blocks part_of|from_kind = 'ask_block' and from_id = 'ask/$before_ask#b1' and kind = 'part_of' and to_kind = 'ask' and to_id = '$before_ask'|from_kind = 'block' and from_id = 'ask/$before_ask#b1' and kind = 'part_of'|select coalesce(max(node_kind), 'unmarked') from ask_blocks where ask_id = '$before_ask'|unmarked|block"
    "comment_block_refs link|from_kind = 'comment_block' and from_id = 'comment/$before_comment#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'comment/$before_comment#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from comment_block_refs where comment_id = '$before_comment'|unmarked|block"
    "message_block_refs link|from_kind = 'message_block' and from_id = 'message/$before_message#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'message/$before_message#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from message_block_refs where message_id = '$before_message'|unmarked|block"
    "ask_block_refs link|from_kind = 'ask_block' and from_id = 'ask/$before_ask#b1' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'block' and from_id = 'ask/$before_ask#b1' and kind = 'mentions'|select coalesce(max(node_kind), 'unmarked') from ask_block_refs where ask_id = '$before_ask'|unmarked|block"
    "comment replies_to comment_block|from_kind = 'comment' and from_id = '$before_comment' and kind = 'replies_to' and to_kind = 'comment_block' and to_id = 'comment/$reply#b1'|from_kind = 'comment' and from_id = '$before_comment' and kind = 'replies_to' and to_kind = 'block'|select tuples_stored::text from comments where id = '$before_comment'|false|true"
    "comment replies_to ask_block|from_kind = 'comment' and from_id = '$before_ask_comment' and kind = 'replies_to' and to_kind = 'ask_block' and to_id = 'ask/$ask#b1'|from_kind = 'comment' and from_id = '$before_ask_comment' and kind = 'replies_to' and to_kind = 'block'|select tuples_stored::text from comments where id = '$before_ask_comment'|false|true"
    "message replies_to message_block|from_kind = 'message' and from_id = '$before_message' and kind = 'replies_to' and to_kind = 'message_block' and to_id = 'message/$message#b1'|from_kind = 'message' and from_id = '$before_message' and kind = 'replies_to' and to_kind = 'block'|select coalesce(node_kind, 'unmarked') from messages where id = '$before_message'|unmarked|message"
    "comment replies_to comment|from_kind = 'comment' and from_id = '$before_comment' and kind = 'replies_to' and to_kind = 'comment' and to_id = '$root'|from_kind = 'comment' and from_id = '$before_comment' and kind = 'replies_to' and to_kind = 'comment'|select tuples_stored::text from comments where id = '$before_comment'|false|true"
    "comment answers comment|from_kind = 'comment' and from_id = '$before_comment' and kind = 'answers' and to_kind = 'comment' and to_id = '$reply'|from_kind = 'comment' and from_id = '$before_comment' and kind = 'answers'|select tuples_stored::text from comments where id = '$before_comment'|false|true"
    "comment replies_to ask|from_kind = 'comment' and from_id = '$before_ask_comment' and kind = 'replies_to' and to_kind = 'ask' and to_id = '$ask'|from_kind = 'comment' and from_id = '$before_ask_comment' and kind = 'replies_to' and to_kind = 'ask'|select tuples_stored::text from comments where id = '$before_ask_comment'|false|true"
    "message replies_to message|from_kind = 'message' and from_id = '$before_message' and kind = 'replies_to' and to_kind = 'message' and to_id = '$message'|from_kind = 'message' and from_id = '$before_message' and kind = 'replies_to' and to_kind = 'message'|select coalesce(node_kind, 'unmarked') from messages where id = '$before_message'|unmarked|message"
    "the comment's refs row|from_kind = 'comment' and from_id = '$before_comment' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'comment' and from_id = '$before_comment' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'comment' and from_id = '$before_comment' and to_id = '$cited'|false|true"
    "the message's refs row|from_kind = 'message' and from_id = '$before_message' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'message' and from_id = '$before_message' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'message' and from_id = '$before_message' and to_id = '$cited'|false|true"
    "the ask's refs row|from_kind = 'ask' and from_id = '$before_ask' and kind = 'mentions' and to_kind = 'issue' and to_id = '$cited'|from_kind = 'ask' and from_id = '$before_ask' and kind = 'mentions'|select tuples_stored::text from refs where from_kind = 'ask' and from_id = '$before_ask' and to_id = '$cited'|false|true"
  )
  for arm in "${arms[@]}"; do
    IFS='|' read -r -a fields <<<"$arm"
    reads+=("$(arm_sql "${fields[1]}" "${fields[2]} and asserted" "${fields[3]}")")
  done
  # Written and read in one transaction, which then commits for the backfill to read the rows.
  in_one_transaction backfill "write two comments, a message and an ask as a build before this child writes them, and read them back" \
    commit "$write" "${reads[@]}"
  pass backfill "wrote comments $before_comment and $before_ask_comment, message $before_message and ask $before_ask as a build before this child writes them, and read them in the transaction that wrote them"
  for arm in "${arms[@]}"; do
    IFS='|' read -r -a fields <<<"$arm"
    index=$((index + 1))
    read="$(sed -n "${index}p" "$work/reads")"
    isolated read_arm backfill "${fields[0]}: the row written before this child is answered before this child's backfill, once, by its old arm" \
      "1|0|${fields[4]}" "$read"
    [[ "$isolated_status" == 0 ]] || failed=1
  done

  local backfill_status=0
  # shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH; this is the caller's
  env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE DATABASE_URL="$database_url" envoy-dispatch backfill-graph-store >"$work/backfill" 2>&1 || backfill_status=$?
  [[ "$backfill_status" == 0 ]] ||
    fail backfill "envoy-dispatch backfill-graph-store over the server's database" "exit $backfill_status: $(<"$work/backfill")"
  # The command exits non-zero while any kind remains, so a zero exit is every kind complete; the
  # kinds it reports are what shows it read any.
  local complete
  complete="$(grep -c 'complete=true' "$work/backfill" || true)"
  [[ "$complete" -gt 0 ]] ||
    fail backfill "envoy-dispatch backfill-graph-store reports the kinds it completed" "it reports none complete=true: $(<"$work/backfill")"
  pass backfill "envoy-dispatch backfill-graph-store over the server's database reports $complete kinds complete=true"
  want_value backfill "the comments carry their node from the node store's backfill, and the message and the ask theirs" \
    "comment|comment|message|ask" "
    select (select string_agg(coalesce(c.node_kind, '(none)'), '|' order by c.body) from comments c
            where c.id in ('$before_comment', '$before_ask_comment'))
           || '|' || (select coalesce(node_kind, '(none)') from messages where id = '$before_message')
           || '|' || (select coalesce(node_kind, '(none)') from asks where id = '$before_ask')"
  for arm in "${arms[@]}"; do
    local -a fields
    IFS='|' read -r -a fields <<<"$arm"
    isolated backfilled_arm "${fields[@]}"
    [[ "$isolated_status" == 0 ]] || failed=1
  done
  local -a planted=("$before_comment" "$before_ask_comment" "$before_message" "$before_ask")
  remove_backfilled_rows
  before_comment=""
  want_value backfill "the rows planted before this child are removed" 0 "
    select (select count(*) from graph_nodes where id in ($(sql_list "${planted[@]}"))
                                                or id like any (array[$(sql_list "comment/${planted[0]}#%" "comment/${planted[1]}#%" "message/${planted[2]}#%" "ask/${planted[3]}#%")]))
         + (select count(*) from graph_tuples where from_id in ($(sql_list "${planted[@]}")) or to_id in ($(sql_list "${planted[@]}"))
                                                 or from_id like any (array[$(sql_list "comment/${planted[0]}#%" "comment/${planted[1]}#%" "message/${planted[2]}#%" "ask/${planted[3]}#%")]))
         + (select count(*) from refs where from_id in ($(sql_list "${planted[@]}")))
         + (select count(*) from comments where id::text in ($(sql_list "${planted[@]}")))
         + (select count(*) from messages where id::text in ($(sql_list "${planted[@]}")))
         + (select count(*) from asks where id::text in ($(sql_list "${planted[@]}")))"
  return "$failed"
}

artifact_citation_section() {
  local section=artifact_citation document ref_key
  send POST "/api/v1/issues/$issue/artifacts" \
    "$(as_session "$(jq -cn --arg c "$(printf '# Notes\n\nA note, %s.\n' "$citation")" '{name: "notes.md", content: $c}')")"
  [[ "$status" == 2?? ]] || fail "$section" "upload a document citing $cited" "HTTP $status: $(body)"
  document="$(jq -er '.artifact.id' "$work/body")" || fail "$section" "upload a document citing $cited" "no id: $(body)"
  ref_key="$(jq -er '.artifact.ref_key' "$work/body")" || fail "$section" "upload a document citing $cited" "no ref_key: $(body)"
  pass "$section" "uploaded $ref_key ($document), citing $cited"
  settles "$section" "the document's citation of $cited is a refs row" 1 "
    select count(*) from refs where from_kind = 'artifact' and from_id = '$document'
      and to_kind = 'issue' and to_id = '$cited' and kind = 'mentions'"
  # Answered once, while its row holds no tuples_stored and the store states no citation out of
  # the document under either of the ids it goes by: the refs arm is the one that answers it.
  want_value "$section" "the document's citation is answered once, by the refs arm" "1|false|0" "
    select (select count(*) from graph_edges
            where from_kind = 'artifact' and from_id = '$document' and kind = 'mentions'
              and to_kind = 'issue' and to_id = '$cited') || '|'
           || (select tuples_stored::text from refs
               where from_kind = 'artifact' and from_id = '$document' and to_kind = 'issue'
                 and to_id = '$cited' and kind = 'mentions') || '|'
           || (select count(*) from graph_tuples
               where from_kind = 'artifact' and from_id in ('$document', '$ref_key') and kind = 'mentions'
                 and to_kind = 'issue' and to_id = '$cited')"
}

# The root's closed list of supertags (the root spec's R21: "This tree
# creates thirty supertags, and the list is closed"), as a person wrote it there and no code of
# this build produces it, in the spelling the store keys a supertag node by.
readonly closed_supertags=(
  issue document "document version" block callout ask requirement plan_task check result
  decision reading criterion lens lens_kind constraint risk comment message person
  session role page interaction delivery_attempt component supertag attribute
  query "query version"
)

# read_node_labels: the NODE_LABELS map of the web build the server serves, into $work/labels. It
# reads the scripts the server's own index.html names and the chunks each of them names in turn,
# and keeps the object literal web/src/features/refs/ReferencedBy.tsx compiles that map to, whose
# first two entries are an artifact's and an ask's labels.
read_node_labels() {
  local -a queue=()
  local asset name index=0 found=""
  status="$(curl -sS -o "$work/index.html" -w '%{http_code}' "$base/" 2>"$work/curl")" ||
    fail kinds "read the web build the server serves" "curl: $(<"$work/curl")"
  [[ "$status" == 200 ]] || fail kinds "read the web build the server serves" "GET / answered HTTP $status"
  mapfile -t queue < <(grep -oE '/assets/[A-Za-z0-9._-]+\.js' "$work/index.html" | sort -u)
  [[ "${#queue[@]}" -gt 0 ]] || fail kinds "read the web build the server serves" "its index.html names no script"
  while [[ "$index" -lt "${#queue[@]}" ]]; do
    asset="${queue[$index]}"
    index=$((index + 1))
    status="$(curl -sS -o "$work/asset.js" -w '%{http_code}' "$base$asset" 2>"$work/curl")" ||
      fail kinds "read $asset" "curl: $(<"$work/curl")"
    [[ "$status" == 200 ]] || fail kinds "read $asset" "HTTP $status"
    if grep -oE '\{"?artifact"?:"Artifact","?ask"?:"Ask",[^{}]*\}' "$work/asset.js" >"$work/labels"; then
      found="$asset"
      break
    fi
    while read -r name; do
      [[ " ${queue[*]} " == *" /$name "* ]] || queue+=("/$name")
    done < <(grep -oE 'assets/[A-Za-z0-9._-]+\.js' "$work/asset.js" | sort -u)
  done
  [[ -n "$found" ]] ||
    fail kinds "find NODE_LABELS in the web build the server serves" "none of the ${#queue[@]} scripts it serves holds it"
  pass kinds "the web build the server serves holds NODE_LABELS, in $found"
}

# labelled KIND: KIND has a label in NODE_LABELS, read by read_node_labels.
labelled() {
  local label
  label="$(grep -oE "[{,]\"?$1\"?:\"[^\"]+\"" "$work/labels")" ||
    fail kinds "$1 has a label in NODE_LABELS" "the web build labels no $1: $(<"$work/labels")"
  label="${label#*:}"
  pass kinds "$1 has a label in NODE_LABELS: $label"
}

# closed_list WHAT SQL: SQL reads `<the values outside the root's closed list>|<how many inside it>`
# over the column it names; the first is empty and the second, the control that the read reads
# the store at all, is above zero.
closed_list() {
  local outside inside
  query kinds "$1" "$2"
  outside="${answer%|*}"
  inside="${answer##*|}"
  [[ -z "$outside" && "$inside" -gt 0 ]] ||
    fail kinds "$1" "the store holds $outside outside the root's closed list, beside $inside inside it"
  pass kinds "$1 ($inside inside it, none outside)"
}

kinds_section() {
  local failed=0 kind closed
  closed="$(sql_list "${closed_supertags[@]}")"
  isolated want_value kinds "the store declares the delivery_attempt, person and role supertags" \
    "delivery_attempt:live person:live role:live" "
    select coalesce(string_agg(id || ':' || state, ' ' order by id collate \"C\"), '(none)') from graph_nodes
    where kind = 'supertag' and id in ('delivery_attempt', 'person', 'role')"
  [[ "$isolated_status" == 0 ]] || failed=1
  isolated read_node_labels
  if [[ "$isolated_status" == 0 ]]; then
    for kind in delivery_attempt person role; do
      isolated labelled "$kind"
      [[ "$isolated_status" == 0 ]] || failed=1
    done
  else
    failed=1
  fi
  isolated closed_list "every supertag node the store holds is one of the root's closed list" "
    select coalesce(string_agg(id, ', ' order by id) filter (where id not in ($closed)), '') || '|'
           || count(*) filter (where id in ($closed))
    from graph_nodes where kind = 'supertag'"
  [[ "$isolated_status" == 0 ]] || failed=1
  isolated closed_list "every node of the store carries a supertag of the root's closed list" "
    select coalesce(string_agg(distinct supertag, ', ') filter (where supertag not in ($closed)), '') || '|'
           || count(*) filter (where supertag in ($closed))
    from graph_nodes"
  [[ "$isolated_status" == 0 ]] || failed=1
  return "$failed"
}

# The attribute nodes this child adds to the delivery_attempt supertag
# (20261116102759_delivery_attempt_attributes.up.sql), each by the id the store keys it by:
# `delivery_attempt/<store name>`, the state stored as delivery_attempt_state because `state` is a
# base attribute every node carries.
readonly attempt_attributes=(
  attempt delivery_attempt_state envelope_id error resolve_error duplicate claimed_at accepted_at accepted_as
)

editor_section() {
  local section=editor version marker="typed over the document socket, $stamp" socket_status=0
  want_value "$section" "the store holds the delivery_attempt supertag" live \
    "select coalesce(max(state), '(no node)') from graph_nodes where kind = 'supertag' and id = 'delivery_attempt'"
  want_value "$section" "its attribute nodes are the ones this child adds, each tied to it by attribute_of" \
    "$(sorted "${attempt_attributes[@]/#/delivery_attempt/}")" "
    select coalesce(string_agg(n.id, ' ' order by n.id collate \"C\"), '(none)') from graph_nodes n
    where n.kind = 'attribute' and n.state = 'live'
      and exists (select 1 from (select distinct on (to_kind, to_id) to_kind, to_id, asserted from graph_tuples t
                                 where t.from_kind = 'attribute' and t.from_id = n.id and t.kind = 'attribute_of'
                                 order by to_kind, to_id, seq desc) latest
                  where latest.asserted and latest.to_kind = 'supertag' and latest.to_id = 'delivery_attempt')"
  get /api/v1/schema/blocks
  want_status 200 "$section" "read the block schema the server serves"
  version="$(jq -er '.version' "$work/body")" || fail "$section" "read the block schema the server serves" "$(body)"
  want_value "$section" "the server serves the schema version the store holds, $version" "$version" "
    select coalesce(max((attributes->>'version')::int)::text, '(none)') from graph_nodes
    where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)"

  (
    cd -- "$script_dir"
    # shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH, HOME and TMPDIR; these are this run's
    exec env -i PATH="$PATH" HOME="${HOME:-$work}" TMPDIR="${TMPDIR:-/tmp}" \
      SOCKET_SERVER="$base" SOCKET_DOCUMENT="$spec" SOCKET_SCHEMA_FILE="$work/body" \
      SOCKET_COOKIE_FILE="$work/cookie" SOCKET_MARKER="$marker" bun -e "$socket_writer"
  ) >"$work/socket.out" 2>"$work/socket.err" || socket_status=$?
  grep -qx 'scope read-write' "$work/socket.out" ||
    fail "$section" "a document socket on $issue's spec at schema version $version admits $person as a writer" \
      "exit $socket_status: $(cat "$work/socket.out" "$work/socket.err")"
  pass "$section" "a document socket on $issue's spec at schema version $version admits $person as a writer"
  grep -qx accepted "$work/socket.out" ||
    fail "$section" "the server accepts a write over the socket" "exit $socket_status: $(cat "$work/socket.out" "$work/socket.err")"
  pass "$section" "the server acknowledges the write over the socket as applied"
  local waited=0
  while true; do
    get "/api/v1/artifacts/$spec/text"
    want_status 200 "$section" "read $issue's spec"
    jq -e --arg m "$marker" '.markdown | contains($m)' "$work/body" >/dev/null && break
    [[ "$waited" -lt 60 ]] || fail "$section" "the write reaches the document's text" "after 30 s it reads $(body)"
    sleep 0.5
    waited=$((waited + 1))
  done
  pass "$section" "the write reaches the document's text"
}

# --- the run -----------------------------------------------------------------------------------

# run_section NAME: runs NAME_section on its own (isolated), recording it in $failed_sections when
# it fails, so every section runs whatever an earlier one found.
failed_sections=()
run_section() {
  isolated "$1_section"
  [[ "$isolated_status" == 0 ]] || failed_sections+=("$1")
}

run_section messages
run_section comments
run_section blocks
run_section agent_page
run_section conversation
run_section retractions
run_section older_rows
run_section backfill
run_section artifact_citation
run_section kinds
# No section addresses, lists and counts a delivery_attempt, a person and a role: no route of this
# build counts one (the header says why).
run_section editor

if [[ "${#failed_sections[@]}" -gt 0 ]]; then
  printf 'one-inbox-kinds.sh: %d section(s) failed: %s\n' "${#failed_sections[@]}" "${failed_sections[*]}" >&2
  exit 1
fi
printf 'one-inbox-kinds.sh: every check passed\n'
