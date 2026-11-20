#!/usr/bin/env bash
# c-census (dispatch://INFRA-1152/artifact/verification-md#b-c-census): the child's census, run
# over a corpus half of which a build of the release before this one wrote.
#
# It starts a `nats-server -js` of its own under $TMPDIR, so the outbox publishes and
# events.published_destinations and the session deliveries the census counts exist (C4); builds
# the server of $BASELINE_SHA in $TMPDIR/base-census and boots it over $DATABASE_URL
# ($ISSUE_BOOT_DATABASE_URL, a database of this check's own) with DISPATCH_AGENT_TOKEN=$DISPATCH_TOKEN;
# seeds the first half of the corpus through that server's API; captures every row of the nine
# projected tables; stops it; boots this build over the same database, which migrates those rows
# forward; runs `envoy-dispatch backfill-graph-store`; seeds the second half through this build's
# API; runs `envoy-dispatch census --store --both-releases`; compares the capture; reads pg_proc
# and pg_trigger for the one-writer proof; greps the source for a reconcile - a writer that reads
# a marked row - rather than for the backfill (C5); and injects a failure between the store write
# and the projection write of each writer the census names, through
# PUT /api/v1/writes/_test/fail-between/{writer}.
#
# One PASS: line per assertion, with the counts and digests it read; every section runs whatever an
# earlier one found; the run exits 1 at the end when any assertion failed, 2 on a missing input.
#
# The mention arm (a comment that points at a person) is dispatch://INFRA-1152's T13, which waits
# on the person address of dispatch://INFRA-1702. Until that writer lands this driver's mention
# arm fails, naming that cause, and the comment_mentions census entry it proves is not written
# either.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && DATABASE_URL=$ISSUE_BOOT_DATABASE_URL DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE BASELINE_SHA=<commit> LEGION_TEST_PG_DSN=<dsn> \
         bash e2e/acceptance/one-inbox-census.sh
       (BASELINE_SHA is the release this branch lands after, and has no default. The database is
       this check's own: the servers truncate and seed whatever database they are given.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_env DATABASE_URL DISPATCH_TOKEN PROJECT BASELINE_SHA
require_commands go git curl psql jq python3

database_url="$DATABASE_URL"
project="$PROJECT"
token="$DISPATCH_TOKEN"
unset DISPATCH_TOKEN
workspace="${LEGION_WORKSPACE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
baseline_sha="$BASELINE_SHA"
work="${TMPDIR:?TMPDIR is required}/one-inbox-census-$$"
mkdir -p "$work"
failures_file="$work/failures"
: >"$failures_file"
# The bearer reaches curl from a file the run alone reads, never as an argument every process of
# the pod can read; printf is a builtin, so the token is in no command line.
auth_header="$work/auth-header"
(umask 077 && printf 'Authorization: Bearer %s' "$token" >"$auth_header")

# Every failure is printed on stderr and recorded in a file, so one inside a command substitution
# - an `expect` whose answer a variable takes - is still printed by the run and still counted.
pass() { printf 'PASS: %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  printf 'x' >>"$failures_file"
}

# What this run started, so the trap takes down exactly what it made.
nats_pid=""
listener_pid=""
base_pid=""
head_pid=""
worktree=""
cleanup() {
  local pid
  for pid in "$head_pid" "$base_pid" "$listener_pid" "$nats_pid"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  if [[ -n "$worktree" ]]; then
    git -C "$workspace" worktree remove --force "$worktree" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# free_port answers a port nothing is listening on now.
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# wait_for_line waits up to 180s for a file to hold a line.
wait_for_line() {
  local file="$1" line="$2" pid="$3" waited=0
  while ((waited < 180)); do
    grep -q "$line" "$file" && return 0
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
    waited=$((waited + 1))
  done
  printf 'the server never printed %q; its log:\n' "$line" >&2
  tail -40 "$file" >&2
  return 1
}

# ───── the bus ─────────────────────────────────────────────────────────────
nats_port="$(free_port)"
nats_url="nats://127.0.0.1:$nats_port"
(
  cd "$workspace/packages/envoy" &&
    GOFLAGS=-p=1 GOMAXPROCS=1 go run github.com/nats-io/nats-server/v2@v2.15.1 \
      -js -p "$nats_port" -sd "$work/nats" >"$work/nats.log" 2>&1
) &
nats_pid=$!
for _ in $(seq 1 120); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$nats_port") 2>/dev/null; then break; fi
  sleep 1
done
pass "a nats-server -js is listening on $nats_url, so the outbox publishes and the census counts what it reached"

# ───── the stand-in Envoy listener ─────────────────────────────────────────
listener_port="$(free_port)"
listener_url="http://127.0.0.1:$listener_port"
python3 - "$listener_port" >"$work/listener.log" 2>&1 <<'LISTENER' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

SESSIONS = []

def record(session):
    if not any(s["session_id"] == session for s in SESSIONS):
        SESSIONS.append({"session_id": session, "title": "census", "capabilities": ["btw", "aside", "steer"]})

class Listener(BaseHTTPRequestHandler):
    def answer(self, body):
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path.startswith("/v1/registry/"):
            session = self.path.split("?", 1)[0].rsplit("/", 1)[-1]
            record(session)
            self.answer({"session_id": session, "title": "census", "capabilities": ["btw", "aside", "steer"]})
        elif self.path.split("?", 1)[0].startswith("/v1/sessions"):
            self.answer(SESSIONS)
        else:
            self.answer({})

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        session = body.get("recipient") or body.get("session_id") or ""
        if session:
            record(session)
        self.answer({"event_id": "census-envelope", "recipient": session})

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), Listener).serve_forever()
LISTENER
listener_pid=$!
for _ in $(seq 1 60); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$listener_port") 2>/dev/null; then break; fi
  sleep 1
done
pass "a stand-in Envoy listener is answering on $listener_url, so a mention is delivered and a session may reply to it"

# ───── the two builds ──────────────────────────────────────────────────────
worktree="$work/base-census"
git -C "$workspace" worktree add --detach "$worktree" "$baseline_sha" >/dev/null 2>&1
(cd "$worktree/packages/envoy" && GOFLAGS=-p=1 GOMAXPROCS=1 GOGC=20 GOMEMLIMIT=3500MiB \
  go build -ldflags "-X main.buildCommit=$baseline_sha" -o "$work/base-dispatch" ./cmd/dispatch)
head_sha="$(git -C "$workspace" rev-parse HEAD)"
# HEAD_BINARY names a build of this tree already made, which the negative control gives: the
# control patches a scratch copy of the tree, builds it and runs this driver against that binary,
# so the run is the same in every other way.
if [[ -n "${HEAD_BINARY:-}" ]]; then
  cp "$HEAD_BINARY" "$work/head-dispatch"
  pass "built the release before ($baseline_sha) and took this build from HEAD_BINARY ($HEAD_BINARY)"
else
  (cd "$workspace/packages/envoy" && GOFLAGS=-p=1 GOMAXPROCS=1 GOGC=20 GOMEMLIMIT=3500MiB \
    go build -ldflags "-X main.buildCommit=$head_sha" -o "$work/head-dispatch" ./cmd/dispatch)
  pass "built the release before ($baseline_sha) and this build ($head_sha)"
fi

# boot BINARY LOG: starts a server on the shared database and the bus, and answers its base URL in
# `base`; its pid is left in `boot_pid`.
boot() {
  local binary="$1" log="$2" port
  port="$(free_port)"
  env -i \
    PATH="$PATH" HOME="$work/home" TMPDIR="$work" \
    DATABASE_URL="$database_url" \
    DISPATCH_AGENT_TOKEN="$token" \
    DISPATCH_PORT="$port" \
    DISPATCH_LISTEN_HOST=127.0.0.1 \
    DISPATCH_NATS_DISABLED=0 \
    NATS_URLS="$nats_url" \
    ENVOY_URL="$listener_url" \
    DISPATCH_TEST_HOOKS=1 \
    DISPATCH_TEST_HOLD_DECISIONS_STEP=1 \
    DISPATCH_IDENTITY="header:X-Dispatch-User" \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
    DISPATCH_SERVER_URL="http://127.0.0.1:$port" \
    "$binary" >"$log" 2>&1 &
  : >"$log"
  boot_pid=$!
  base="http://127.0.0.1:$port"
  wait_for_line "$log" "listening" "$boot_pid"
}

# The person every write of this corpus is made as, named to the server through the trusted header
# its identity setting reads (DISPATCH_IDENTITY=header:X-Dispatch-User): a write needs a signed-in
# person (api's writeNeedsASignedInPerson), and the agent bearer names a session and no person.
readonly census_person=alice
readonly census_other=bob

# request METHOD PATH [BODY]: one call as that person; it answers the body and leaves the status in
# `status`. Each call is given the agent bearer too, so a route a session drives (a reply) takes it.
request() {
  local method="$1" path="$2" body="${3:-}" out
  out="$work/answer.$$"
  if [[ -n "$body" ]]; then
    printf '%s' "$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "$base$path" \
      -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "$body")" >"$work/status"
  else
    printf '%s' "$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "$base$path" \
      -H "X-Dispatch-User: $census_person")" >"$work/status"
  fi
  cat "$out"
}

# request_as is request as another person, for a reply by somebody else.
request_as() {
  local login="$1" method="$2" path="$3" body="$4" out
  out="$work/answer.$$"
  printf '%s' "$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "$base$path" \
    -H "X-Dispatch-User: $login" -H 'Content-Type: application/json' -d "$body")" >"$work/status"
  cat "$out"
}

# expect_as is expect as another person.
expect_as() {
  local login="$1" want="$2" method="$3" path="$4" body="$5" what="$6" answer
  answer="$(request_as "$login" "$method" "$path" "$body")"
  status="$(<"$work/status")"
  if [[ "$status" != "$want" ]]; then
    fail "$what: $method $path answered $status, want $want: $answer"
    return 1
  fi
  printf '%s' "$answer"
}

# agent_request is request as the agent bearer, for the routes a session drives: a session's reply
# to a comment and to a direct message.
agent_request() {
  local method="$1" path="$2" body="${3:-}" out
  out="$work/answer.$$"
  printf '%s' "$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "$base$path" \
    -H @"$auth_header" -H 'Content-Type: application/json' -d "$body")" >"$work/status"
  cat "$out"
}

# expect_agent is expect as the agent bearer (agent_request).
expect_agent() {
  local want="$1" method="$2" path="$3" body="$4" what="$5" answer
  answer="$(agent_request "$method" "$path" "$body")"
  status="$(<"$work/status")"
  if [[ "$status" != "$want" ]]; then
    fail "$what: $method $path answered $status, want $want: $answer"
    return 1
  fi
  printf '%s' "$answer"
}

# expect WANT METHOD PATH [BODY] WHAT: a call whose status must be WANT.
expect() {
  local want="$1" method="$2" path="$3" body="$4" what="$5" answer
  answer="$(request "$method" "$path" "$body")"
  status="$(<"$work/status")"
  if [[ "$status" != "$want" ]]; then
    fail "$what: $method $path answered $status, want $want: $answer"
    return 1
  fi
  printf '%s' "$answer"
}

mkdir -p "$work/home"

# seed_half HALF: the corpus arms one build writes. Each arm is written through the API of the
# server now booted, so each is that build's own: the first half the release before's, the second
# this build's. HALF names the arm's rows apart.
seed_half() {
  local half="$1" issue closed ask answer comment reply message dm dm_message read_mark broadcast answer_at
  issue="$(expect 201 POST "/api/v1/issues" \
    "{\"project\":\"$project\",\"title\":\"$(printf %s "$half" | tr a-z A-Z) seeding $RANDOM\",\"body\":\"The $half half of the census corpus.\"}" \
    "$half: the issue" | jq -r .key)" || return 0
  closed="$(expect 201 POST "/api/v1/issues" \
    "{\"project\":\"$project\",\"title\":\"$(printf %s "$half" | tr a-z A-Z) shut $RANDOM\",\"body\":\"Closed.\"}" \
    "$half: the closed issue" | jq -r .key)" || true

  # Four ask arms: an assigned open ask, an answered one, one on a closed issue, one on a document.
  ask="$(expect 201 POST "/api/v1/issues/$issue/asks" \
    "{\"question\":\"Ship the $half half of dispatch://$issue?\",\"asked_of\":\"alice\"}" "$half: the open ask" | jq -r .id)" || true
  answered_ask="$(expect 201 POST "/api/v1/issues/$issue/asks" \
    "{\"question\":\"Answered in the $half half of dispatch://$issue?\",\"asked_of\":\"alice\"}" "$half: the answered ask")" || true
  answer="$(jq -r '.id // empty' <<<"$answered_ask")"
  if [[ -n "${answer:-}" && "$answer" != null ]]; then
    # The answer names the revision the person reviewed, which is the ask's edited_at exactly, and
    # an ask nothing has edited holds none: the route compares the two as they are (answerTransition).
    answer_at="$(request GET "/api/v1/asks/$answer" | jq '.edited_at')"
    expect 200 POST "/api/v1/asks/$answer/answer" \
      "{\"text\":\"Yes, in the $half half.\",\"expected_edited_at\":$answer_at}" "$half: the answer" >/dev/null || true
  fi
  expect 201 POST "/api/v1/issues/$closed/asks" \
    "{\"question\":\"An ask on a closed issue ($half)?\",\"asked_of\":\"alice\"}" "$half: the ask on a closed issue" >/dev/null || true

  # Thread arms: a comment thread with a session's reply, a thread read, a dismissed thread, a
  # snoozed ask.
  comment="$(expect 201 POST "/api/v1/issues/$issue/comments" \
    "{\"body\":\"A $half comment thread, for @census-$half, on dispatch://$issue.\",\"mentions\":[{\"target\":\"session:census-$half\"}],\"delivery\":\"btw\"}" \
    "$half: the comment thread" | jq -r .id)" || true
  if [[ -n "${comment:-}" && "$comment" != null ]]; then
    reply="$(expect_as "$census_other" 201 POST "/api/v1/issues/$issue/comments" \
      "{\"body\":\"A reply by somebody else ($half), on dispatch://$issue.\",\"reply_to\":\"$comment\",\"answers\":\"$comment\"}" \
      "$half: the reply in the comment thread" | jq -r .id)" || true
    # The session may reply only to the delivery it took, so the run waits for the mention's
    # attempt to settle against it before the session answers (model.DeliveryTaken).
    if settled_attempt "comment_deliveries" "comment_id = '$comment'"; then
      expect_agent 201 POST "/api/v1/comments/$comment/reply" \
        "{\"actor\":{\"kind\":\"session\",\"id\":\"census-$half\"},\"attempt\":1,\"target\":\"session:census-$half\",\"body\":\"A session's reply ($half).\"}" \
        "$half: the session's reply to its mention" >/dev/null || true
    else
      fail "$half: the mention's attempt never settled, so the session had no delivery of its own to answer"
    fi
    expect 204 PUT "/api/v1/me/threads/$comment/read" \
      "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: the thread read" >/dev/null || true
  fi

  # Message arms: an issue message thread, a direct message with a session's reply, a broadcast.
  message="$(expect 201 POST "/api/v1/issues/$issue/messages" \
    "{\"body\":\"A $half issue message about dispatch://$issue.\"}" "$half: the issue message" | jq -r .id)" || true
  if [[ -n "${message:-}" && "$message" != null ]]; then
    expect_as "$census_other" 201 POST "/api/v1/issues/$issue/messages" \
      "{\"body\":\"A reply in the message thread ($half).\",\"in_reply_to\":\"$message\"}" \
      "$half: the reply in the message thread" >/dev/null || true
  fi
  dm_message="$(expect 201 POST "/api/v1/agents/census-$half/messages" \
    "{\"body\":\"A $half direct message.\",\"delivery\":\"btw\"}" \
    "$half: the direct message")" || true
  dm="$(jq -r '.id // empty' <<<"$dm_message" 2>/dev/null)" || true
  # The Clear and the read mark of this session's conversation, below, stand at the direct
  # message's own moment, which the store's clock gave it and which is strictly before the
  # session's reply to it. A mark at or after that reply's created_at makes its reply_read row dead,
  # and the route that moves the mark deletes it (pruneAgentReplies, api/state.go), so a mark read
  # off the wall clock left a half with no user_agent_reply_read row whenever the reply fell in an
  # earlier second than the mark.
  read_mark="$(jq -r '.created_at // empty' <<<"$dm_message" 2>/dev/null)" || true
  if [[ -n "${dm:-}" && "$dm" != null ]]; then
    if settled_attempt "message_deliveries" "message_id = '$dm'"; then
      agent_reply="$(expect_agent 201 POST "/api/v1/messages/$dm/reply" \
        "{\"actor\":{\"kind\":\"session\",\"id\":\"census-$half\"},\"attempt\":1,\"body\":\"A session's direct reply ($half).\"}" \
        "$half: the session's direct reply" | jq -r .id)" || true
    else
      agent_reply=""
      fail "$half: the direct message's attempt never settled, so the session had no delivery of its own to answer"
    fi
    if [[ -n "${agent_reply:-}" && "$agent_reply" != null ]]; then
      # The reply a person read by its id (user_agent_reply_read), and the dismissal of the Agent
      # thread's root (user_agent_thread_dismissed), which carries what the page drew of that row.
      expect 200 PUT "/api/v1/me/agents/census-$half/state" \
        "{\"read_replies\":[\"$agent_reply\"]}" "$half: the reply read by its id" >/dev/null || true
      expect 200 POST "/api/v1/me/inbox/dismiss" \
        "{\"threads\":[\"agent:$dm\"],\"seen_replies\":{\"agent:$dm\":{\"newest_reply\":\"$agent_reply\",\"unread_replies\":[]}}}" \
        "$half: the Agent thread's dismissal" >/dev/null || true
    fi
  fi
  broadcast="$(request POST "/api/v1/broadcasts" \
    "{\"body\":\"A $half broadcast.\",\"delivery\":\"btw\",\"idempotency_key\":\"census-$half\"}")" || true

  # The read-state arms: a Clear and a read mark of a session's conversation, a snooze, a dismissal.
  expect 200 PUT "/api/v1/me/agents/census-$half/state" \
    "{\"cleared_before\":\"${read_mark:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}\",\"read_through\":\"${read_mark:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}\"}" \
    "$half: the Clear and the read mark" >/dev/null || true
  if [[ -n "${ask:-}" && "$ask" != null ]]; then
    expect 200 PUT "/api/v1/me/asks/$ask/snooze" \
      "{\"snoozed_until\":\"$(date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: the snooze" >/dev/null || true
  fi
  if [[ -n "${message:-}" && "$message" != null ]]; then
    expect 200 POST "/api/v1/me/inbox/dismiss" \
      "{\"threads\":[\"reply:$message\"],\"seen_replies\":{\"reply:$message\":{\"version\":0}}}" \
      "$half: the dismissal" >/dev/null || true
  fi
  expect 200 POST "/api/v1/me/inbox/read" \
    "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: Mark all read" >/dev/null || true

  # The mention arm: a comment that points at a person, a role and a session. The person half is
  # dispatch://INFRA-1152's T13, which waits on dispatch://INFRA-1702's person address; until it
  # lands this arm is refused, and this driver says so.
  if [[ -n "${comment:-}" && "$comment" != null ]]; then
    expect 201 POST "/api/v1/issues/$issue/comments" \
      "{\"body\":\"A $half comment that points at a session.\",\"mentions\":[{\"target\":\"session:census-$half\"}],\"delivery\":\"btw\"}" \
      "$half: a comment that points at a session" >/dev/null || true
    request POST "/api/v1/issues/$issue/comments" \
      "{\"body\":\"A $half comment that points at @alice.\",\"mentions\":[{\"target\":\"user:alice\"}],\"delivery\":\"btw\"}" >"$work/mention-$half.json" || true
    status="$(<"$work/status")"
    if [[ "$status" == "201" ]]; then
      pass "$half: a comment that points at a person, a role and a session was stored"
    else
      fail "$half: a comment that points at a person answered $status: the mention tuples are dispatch://INFRA-1152's T13, which waits on the person address of dispatch://INFRA-1702 ($(head -c 200 "$work/mention-$half.json"))"
    fi
  fi
  # A document of the project's own, whose body cites the issue, so this run writes the blocks and
  # the block links dispatch://INFRA-1149's census covers; and a role the issue is routed to, so
  # its role and its page are nodes the publisher writes.
  expect 201 POST "/api/v1/projects/$project/artifacts" \
    "{\"name\":\"census-$half.md\",\"content\":\"# The $half half\\n\\nA passage citing dispatch://$issue.\\n\"}" \
    "$half: the project document" >/dev/null || true
  expect 200 PATCH "/api/v1/issues/$issue" \
    "{\"route\":\"role:census-$half\"}" "$half: the role the issue is routed to" >/dev/null || true

  # One event the person wrote themselves, which draws no delivery to them.
  expect 201 POST "/api/v1/issues/$issue/comments" \
    "{\"body\":\"The person's own $half comment.\"}" "$half: the person's own comment" >/dev/null || true
}

# settled_attempt waits up to 30s for a row of TABLE that WHERE names to reach a state the listener
# took it in, with the session it reached (model.DeliveryTakenStatesSQL), and answers whether one
# did: the session may reply only to a delivery of its own, so a reply before the send settles is
# refused.
settled_attempt() {
  local table="$1" where="$2" held
  for _ in $(seq 1 30); do
    held="$(psql "$database_url" -At -c "select count(*) from $table where $where and state in ('sent', 'queued') and coalesce(session_id, '') <> ''")"
    [[ "$held" != "0" ]] && return 0
    sleep 1
  done
  return 1
}

# The nine tables the census covers, as the capture reads them.
capture_tables=(user_threads user_agent_read user_agent_state user_agent_reply_read
  user_agent_thread_dismissed user_ask_snooze comment_mentions comment_deliveries message_deliveries)

# capture FILE: every row of the nine tables, each as its table and its key columns, sorted.
capture() {
  local file="$1" table
  : >"$file"
  for table in "${capture_tables[@]}"; do
    psql "$database_url" -At -c "select '$table ' || row_to_json(t)::text from $table t order by 1" >>"$file"
  done
  sort -o "$file" "$file"
}

# ───── the release before writes the first half ────────────────────────────
boot "$work/base-dispatch" "$work/base.log"
base_pid="$boot_pid"
expect 201 POST "/api/v1/projects" "{\"key\":\"$project\",\"name\":\"Census\",\"default_assignee\":\"alice\"}" \
  "the project the release before creates" >/dev/null || true
seed_half "before"
capture "$work/capture.txt"
captured="$(wc -l <"$work/capture.txt")"
if ((captured > 0)); then
  pass "the release before wrote $captured row(s) of the nine projected tables, captured before this build booted"
else
  fail "the release before wrote no row of the nine projected tables, so this run compares nothing"
fi
kill "$base_pid" 2>/dev/null || true
wait "$base_pid" 2>/dev/null || true
base_pid=""

# ───── this build migrates them forward and writes the second half ─────────
boot "$work/head-dispatch" "$work/head.log"
head_pid="$boot_pid"
"$work/head-dispatch" backfill-graph-store >"$work/backfill.log" 2>&1 ||
  fail "envoy-dispatch backfill-graph-store exited non-zero: $(tail -5 "$work/backfill.log")"
pass "the backfill ran over the rows the release before wrote: $(grep -c . "$work/backfill.log") line(s) reported"
seed_half "now"

# The outbox publishes after the write commits, and published_destinations is what it records, so
# the run waits for every event to be published before it reads the census.
for _ in $(seq 1 60); do
  owed="$(psql "$database_url" -At -c "select count(*) from events where notify and published_at is null")"
  [[ "$owed" == "0" ]] && break
  sleep 2
done
pass "the outbox drained the event log before the census read it ($(psql "$database_url" -At -c "select count(*) from events where published_at is not null") published, $owed owed)"

# ───── the census ──────────────────────────────────────────────────────────
census_status=0
DATABASE_URL="$database_url" "$work/head-dispatch" census --store --both-releases >"$work/census.log" 2>&1 || census_status=$?
if ((census_status == 0)); then
  pass "census --store --both-releases exited 0: $(grep -c '^census: store kind=' "$work/census.log") kind line(s), $(grep '^census: store rows_checked=' "$work/census.log")"
else
  fail "census --store --both-releases exited $census_status: $(grep -E '^census: store (REFUSED|kind=[a-z_]+ difference)' "$work/census.log" | head -20)"
fi
while read -r line; do
  printf 'census: %s\n' "$line"
done < <(grep -E '^census: store kind=|^census: store rows_checked=' "$work/census.log" | sed 's/^census: store //')

for kind in user_threads user_threads_version user_agent_read user_agent_state user_agent_reply_read \
  user_agent_thread_dismissed user_ask_snooze comment_deliveries message_deliveries \
  published_destinations message comment_pointers comment block block_ref role page; do
  line="$(grep -m1 -E "^census: store kind=$kind (nodes|tuples)=" "$work/census.log" || true)"
  if [[ -z "$line" ]]; then
    fail "the census printed no line for kind $kind"
    continue
  fi
  populations="$(grep "^census: store kind=$kind release_before_rows=" "$work/census.log" || true)"
  if [[ "$line" == *" rows=0 "* ]]; then
    fail "the census checked no row of kind $kind, so it proves nothing of it: $line"
  elif [[ "$populations" == *"release_before_rows=0"* || "$populations" == *"this_release_rows=0"* ]]; then
    fail "kind $kind holds one population only, so the comparison proves nothing of the other: $populations"
  else
    pass "kind $kind agrees: $(sed 's/^census: store //' <<<"$line") | $(sed "s/^census: store kind=$kind //" <<<"$populations")"
  fi
done
grep '^census: store kind=[a-z_]* narrowing=' "$work/census.log" | sed 's/^census: store //' |
  while read -r line; do printf 'narrowing: %s\n' "$line"; done

# The capture: every row the release before wrote is still in its own table.
capture "$work/capture-after.txt"
missing="$(comm -23 <(cut -d' ' -f1 "$work/capture.txt" | sort | uniq -c) <(cut -d' ' -f1 "$work/capture-after.txt" | sort | uniq -c) || true)"
lost=0
for table in "${capture_tables[@]}"; do
  before="$(grep -c "^$table " "$work/capture.txt" || true)"
  after="$(grep -c "^$table " "$work/capture-after.txt" || true)"
  if ((after < before)); then
    fail "$table held $before row(s) when the release before stopped and holds $after now: the migration or the backfill lost a row from both sides, which the census cannot see"
    lost=$((lost + 1))
  fi
done
((lost == 0)) && pass "every row of the nine tables the release before wrote is still there ($captured captured)"

# ───── one writer ──────────────────────────────────────────────────────────
writers="$(psql "$database_url" -At -c "
  select string_agg(p.proname, ',' order by p.proname) from pg_proc p
  join pg_namespace n on n.oid = p.pronamespace
  where n.nspname = current_schema()
    and p.prolang <> (select l.oid from pg_language l where l.lanname = 'internal')
    and p.prosrc ~* '(insert into|update|delete from)[[:space:]]+(user_threads|user_agent_thread_dismissed)'")"
if [[ "$writers" == "user_agent_thread_dismissed_clear,user_threads_record" ]]; then
  pass "pg_proc holds two functions that write user_threads or user_agent_thread_dismissed, the one exemption: $writers"
else
  fail "pg_proc holds the functions $writers writing user_threads or user_agent_thread_dismissed, want user_agent_thread_dismissed_clear,user_threads_record alone"
fi
marker_tests="$(psql "$database_url" -At -c "
  select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace
  where n.nspname = current_schema() and p.proname in ('user_threads_comment','user_threads_message','user_threads_ask','user_agent_thread_dismissed_clear')
    and p.prosrc ~ 'tuples_stored|node_kind is not null'")"
if [[ "$marker_tests" == "4" ]]; then
  pass "each of the four trigger functions writes only under its marker test (tuples_stored, the node pair, tuples_stored_at)"
else
  fail "$marker_tests of the four trigger functions carry a marker test, want four"
fi
ask_skip="$(psql "$database_url" -At -c "
  select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace
  where n.nspname = current_schema() and p.proname = 'user_threads_ask'
    and p.prosrc ~ 'tuples_stored_at is distinct from \(new.answer->>''at''\)::timestamptz'")"
if [[ "$ask_skip" == "1" ]]; then
  pass "user_threads_ask skips an answer only where new.tuples_stored_at = (new.answer->>'at')::timestamptz"
else
  fail "user_threads_ask does not skip an answer by the marker R9 names"
fi
triggers="$(psql "$database_url" -At -c "
  select string_agg(c.relname || '.' || t.tgname, ',' order by c.relname || '.' || t.tgname)
  from pg_trigger t join pg_class c on c.oid = t.tgrelid
  join pg_namespace n on n.oid = c.relnamespace join pg_proc p on p.oid = t.tgfoid
  where not t.tgisinternal and n.nspname = current_schema()
    and p.proname in ('user_threads_comment','user_threads_message','user_threads_ask','user_agent_thread_dismissed_clear')")"
if [[ -n "$triggers" ]]; then
  pass "pg_trigger runs those functions: $triggers"
else
  fail "no trigger runs the four functions, so their marker tests stand in front of nothing"
fi

# A reconcile is a writer that reads a marked row. The backfill reads only the rows its marker
# test passes, so it is none (C5).
reconciles="$(grep -rn "marked_seq is not null\|tuples_stored_at is not null\|node_kind is not null" \
  "$workspace/packages/envoy/internal/dispatch" --include='*.go' |
  grep -v '_test.go' | grep -v '/graphcensus/census' | grep -v 'graph_edges' || true)"
reconcile_writes="$(printf '%s\n' "$reconciles" | grep -i "update \|insert into \|delete from " || true)"
if [[ -z "$reconcile_writes" ]]; then
  pass "no writer of this tree reads a marked row to write it again: there is no reconcile for these kinds, in either direction"
else
  fail "a writer reads a marked row and writes it: $(printf '%s' "$reconcile_writes" | head -3)"
fi
if grep -q "marked_seq is null" "$workspace/packages/envoy/internal/dispatch/graphcensus/backfill_inbox_threads.go"; then
  pass "the backfill's own query selects only the rows the marker test of R9 passes (marked_seq is null)"
else
  fail "the backfill's query no longer selects by the marker test of R9"
fi

# ───── the failure injection ───────────────────────────────────────────────
# Each writer the census names, armed through the seam and then driven: the write fails, and
# neither its tuples nor its projection row is there afterwards.
inject() {
  local writer="$1" what="$2" shift_body
  shift 2
  request PUT "/api/v1/writes/_test/fail-between/$writer" '' >/dev/null
  status="$(<"$work/status")"
  if [[ "$status" != "200" ]]; then
    fail "arming $writer answered $status"
    return
  fi
  local before after
  before="$(psql "$database_url" -At -c "select (select count(*) from graph_tuples), (select count(*) from user_threads), (select count(*) from comments), (select count(*) from messages)")"
  "$@" >/dev/null 2>&1 || true
  after="$(psql "$database_url" -At -c "select (select count(*) from graph_tuples), (select count(*) from user_threads), (select count(*) from comments), (select count(*) from messages)")"
  if [[ "$before" == "$after" ]]; then
    pass "a failure injected between $writer's store write and its projection write left neither side: $what ($before)"
  else
    fail "a failure injected into $writer left rows behind: $before before, $after after ($what)"
  fi
}

inject_issue="$(request POST "/api/v1/issues" "{\"project\":\"$project\",\"title\":\"Census injection\",\"body\":\"Injected.\"}" | jq -r .key)"
inject_comment="$(request POST "/api/v1/issues/$inject_issue/comments" '{"body":"A comment to reply to."}' | jq -r .id)"
inject_message="$(request POST "/api/v1/issues/$inject_issue/messages" '{"body":"A message to reply to."}' | jq -r .id)"
inject_ask="$(request POST "/api/v1/issues/$inject_issue/asks" '{"question":"Injected?","asked_of":"alice"}' | jq -r .id)"
inject_dm="$(request POST "/api/v1/agents/census-inject/messages" '{"body":"A direct message.","delivery":"btw"}' | jq -r .id)"

inject comment "a comment's write" \
  curl -sS -o /dev/null -X POST "$base/api/v1/issues/$inject_issue/comments" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d '{"body":"The comment the failure takes."}'
inject message "an issue message's write" \
  curl -sS -o /dev/null -X POST "$base/api/v1/issues/$inject_issue/messages" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d '{"body":"The message the failure takes."}'
inject direct-reply "a session's direct reply" \
  curl -sS -o /dev/null -X POST "$base/api/v1/messages/$inject_dm/reply" -H @"$auth_header" -H 'Content-Type: application/json' -d '{"actor":{"kind":"session","id":"census-inject"},"body":"The reply the failure takes."}'
inject user-threads "the one Go writer of user_threads" \
  curl -sS -o /dev/null -X POST "$base/api/v1/comments/$inject_comment/reply" -H @"$auth_header" -H 'Content-Type: application/json' -d '{"actor":{"kind":"session","id":"census-inject"},"attempt":1,"target":"session:census-inject","body":"The reply whose thread write the failure takes."}'
inject agent-dismissal-clear "the Go writer that clears an Agent dismissal" \
  curl -sS -o /dev/null -X POST "$base/api/v1/messages/$inject_dm/reply" -H @"$auth_header" -H 'Content-Type: application/json' -d '{"actor":{"kind":"session","id":"census-inject"},"body":"The reply whose clear the failure takes."}'
inject thread-read "a thread's read mark" \
  curl -sS -o /dev/null -X PUT "$base/api/v1/me/threads/$inject_comment/read" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
inject inbox-read "Mark all read" \
  curl -sS -o /dev/null -X POST "$base/api/v1/me/inbox/read" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
inject inbox-dismiss "a dismissal" \
  curl -sS -o /dev/null -X POST "$base/api/v1/me/inbox/dismiss" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"threads\":[\"reply:$inject_message\"],\"seen_replies\":{\"reply:$inject_message\":{\"version\":0}}}"
inject agent-state "an agent's Clear and read mark" \
  curl -sS -o /dev/null -X PUT "$base/api/v1/me/agents/census-inject/state" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"cleared_before\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
inject ask-snooze "a snooze" \
  curl -sS -o /dev/null -X PUT "$base/api/v1/me/asks/$inject_ask/snooze" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"snoozed_until\":\"$(date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)\"}"
inject_edited="$(request GET "/api/v1/asks/$inject_ask" | jq -r .edited_at)"
inject ask-answer "an ask's answer" \
  curl -sS -o /dev/null -X POST "$base/api/v1/asks/$inject_ask/answer" -H "X-Dispatch-User: $census_person" -H 'Content-Type: application/json' -d "{\"text\":\"The answer the failure takes.\",\"expected_edited_at\":\"$inject_edited\"}"

# The census reads no difference after every injected failure: a rollback leaves neither side.
after_status=0
DATABASE_URL="$database_url" "$work/head-dispatch" census --store >"$work/census-after.log" 2>&1 || after_status=$?
if ((after_status == 0)); then
  pass "the census after every injected failure reads no difference: $(grep '^census: store rows_checked=' "$work/census-after.log")"
else
  fail "the census after the injected failures exited $after_status: $(grep -E '^census: store (REFUSED|kind=[a-z_]+ difference)' "$work/census-after.log" | head -10)"
fi

failures="$(wc -c <"$failures_file" | tr -d ' ')"
if ((failures > 0)); then
  printf '%d assertion(s) failed\n' "$failures" >&2
  exit 1
fi
