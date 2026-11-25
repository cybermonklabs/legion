#!/usr/bin/env bash
# The check c-census: the child's census, run
# over a corpus half of which a build of the release before this one wrote.
#
# It empties $DATABASE_URL ($ISSUE_BOOT_DATABASE_URL, a database of this check's own); starts a
# `nats-server -js` of its own under $TMPDIR, so the outbox publishes and
# events.published_destinations and the session deliveries the census counts exist (C4); builds
# the server of $BASELINE_SHA in $TMPDIR/base-census, once per commit, and boots it over that
# database with DISPATCH_AGENT_TOKEN=$DISPATCH_TOKEN; seeds the first half of the corpus through
# that server's API; captures every row of the nine projected tables, by its key; stops it; boots
# this build over the same database, which migrates those rows forward; runs
# `envoy-dispatch backfill-graph-store`;
# captures the rows again; seeds the second half through this build's API; runs
# `envoy-dispatch census --store --both-releases` and holds every kind it covers to zero
# differences in both directions, the unmarked class of every kind that counts one to zero, and
# the two narrowings the check names (no_project and session_authors) to the corpus's own count of
# each; compares the two captures; reads pg_proc and
# pg_trigger for the one-writer proof; greps the source for a reconcile - a writer that reads a
# marked row - rather than for the backfill (C5); and injects a failure between the store write
# and the projection write of each writer the census names, through
# PUT /api/v1/writes/_test/fail-between/{writer}.
#
# One PASS: line per assertion, with the counts and digests it read; every section runs whatever an
# earlier one found; the run exits 1 at the end when any assertion failed, 2 on a missing input.
#
# The mention arm names a person by the address the text grammar reads, `dispatch://person/<login>`,
# whose login is an email lowercased: the harness's bare logins (alice, bob) are nobody a mention
# reaches.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && DATABASE_URL=$ISSUE_BOOT_DATABASE_URL DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE BASELINE_SHA=<commit> [HEAD_BINARY=<path>] \
         bash e2e/acceptance/one-inbox-census.sh
       (BASELINE_SHA is the release this branch lands after, and has no default. HEAD_BINARY is a
       build of this tree already made, which the negative control gives. The database is this
       check's own, on this machine: the run empties it first, and the servers then migrate and
       seed it.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_env DATABASE_URL DISPATCH_TOKEN PROJECT BASELINE_SHA
require_commands go git curl psql jq python3 tar flock
loopback_database_url "$DATABASE_URL" || {
  printf 'one-inbox-census.sh: DATABASE_URL names no single loopback host; the servers this driver boots truncate and seed it, so it runs only against a test database on this machine\n' >&2
  exit 2
}

database_url="$DATABASE_URL"
project="$PROJECT"
token="$DISPATCH_TOKEN"
unset DISPATCH_TOKEN
# The checkout this driver is part of, which it builds as this build and resolves BASELINE_SHA in:
# the tree whose code the check measures, whatever checkout the caller's environment names.
workspace="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
baseline_sha="$BASELINE_SHA"
: "${TMPDIR:?TMPDIR is required}"
work_dir
failures_file="$work/failures"
: >"$failures_file"
# The bearer reaches curl from a file the run alone reads, never as an argument every process of
# the pod can read; printf is a builtin, so the token is in no command line.
auth_header="$work/auth-header"
printf 'Authorization: Bearer %s' "$token" >"$auth_header"

# Every failure is printed on stderr and recorded in a file, so one inside a command substitution
# - an `expect` whose answer a variable takes - is still printed by the run and still counted.
pass() { printf 'PASS: %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  printf 'x' >>"$failures_file"
}

# psql_db ARGS...: psql over the run's database without PGHOSTADDR, PGSERVICE or PGSERVICEFILE,
# any of which libpq would let name another server than the URL's: the URL alone names the
# server, as the loopback guard read it (lib.sh's `query`, which exits, where this run's `fail`
# does not).
psql_db() { env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" "$@"; }

# empty_database: empties the run's database of every object its login owns, and keeps what
# another role installed there.
# The database is this check's own, and the release before writes its half into one that holds
# nothing yet: a schema an earlier run, or another build, left migrated would have that release
# serve an older schema over a newer one, and this build refuse a migration another tree recorded.
# It drops what the login owns (DROP OWNED) and not the schema public, because public holds the
# `vector` extension: where the login is no superuser, as on a pod's test Postgres, the database
# took `vector` from its template, and migration 0072's `create extension if not exists vector`
# could not install it again. DROP OWNED keeps an extension another role owns and drops one the
# login owns. It would drop a public the login owns with everything in it, `vector` among them, so
# a public the login made itself (as a drop of public and `create schema public` leave it) goes
# first to pg_database_owner, the owner CREATE DATABASE gives it; a database left with no public
# gets one again.
# The one login DROP OWNED refuses is the bootstrap superuser, which owns the system catalogs, such
# as the `postgres` of the pgvector image CI's Postgres service runs: that login drops public and
# makes it again, and its migration 0072 installs `vector` again.
# It refuses first, as the run's start does, a DATABASE_URL that names no single loopback host
# (lib.sh's loopback_database_url), so it drops nothing but a test database on this machine.
empty_database() {
  loopback_database_url "$database_url" || {
    printf 'one-inbox-census.sh: DATABASE_URL names no single loopback host; the run empties it, so it runs only against a test database on this machine\n' >&2
    exit 2
  }
  psql_db -q -v ON_ERROR_STOP=1 >"$work/empty.log" 2>&1 <<'SQL' || {
do $empty$
begin
  if (select pg_get_userbyid(nspowner) from pg_namespace where nspname = 'pg_catalog') = current_user then
    drop schema if exists public cascade;
  else
    if (select pg_get_userbyid(nspowner) from pg_namespace where nspname = 'public') = current_user then
      alter schema public owner to pg_database_owner;
    end if;
    drop owned by current_user cascade;
  end if;
  create schema if not exists public;
end
$empty$;
SQL
    printf 'one-inbox-census.sh: could not empty DATABASE_URL: %s\n' "$(<"$work/empty.log")" >&2
    exit 2
  }
}
empty_database

# What this run started, so the trap takes down exactly what it made.
nats_pid=""
listener_pid=""
base_pid=""
head_pid=""
cleanup() {
  local pid
  for pid in "$head_pid" "$base_pid" "$listener_pid" "$nats_pid"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# free_port answers a port nothing is listening on now.
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# wait_for_port PORT SECONDS: whether something takes a connection on 127.0.0.1:PORT within
# SECONDS.
wait_for_port() {
  local waited
  for ((waited = 0; waited < $2; waited++)); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then return 0; fi
    sleep 1
  done
  return 1
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
# The nats-server is installed, then started as a process of this run's own, so the pid the trap
# stops is the server's: a `go run` would leave its server running when it is stopped.
nats_port="$(free_port)"
nats_url="nats://127.0.0.1:$nats_port"
if (cd "$workspace/packages/envoy" && GOBIN="$work/bin" GOFLAGS=-p=1 GOMAXPROCS=1 \
  go install github.com/nats-io/nats-server/v2@v2.15.1) >"$work/nats-install.log" 2>&1; then
  "$work/bin/nats-server" -js -p "$nats_port" -sd "$work/nats" >"$work/nats.log" 2>&1 &
  nats_pid=$!
fi
if [[ -n "$nats_pid" ]] && wait_for_port "$nats_port" 120; then
  pass "a nats-server -js is listening on $nats_url, so the outbox publishes and the census counts what it reached"
else
  fail "no nats-server listened on $nats_url within 120 s: $(tail -5 "$work/nats-install.log" "$work/nats.log" 2>/dev/null)"
fi

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
if wait_for_port "$listener_port" 60; then
  pass "a stand-in Envoy listener is answering on $listener_url, so a mention is delivered and a session may reply to it"
else
  fail "the stand-in Envoy listener took no connection on $listener_url within 60 s: $(tail -5 "$work/listener.log")"
fi

# ───── the two builds ──────────────────────────────────────────────────────
# The release before is built once per commit in $TMPDIR/base-census, under a lock, from the
# commit's tree taken with git archive (no worktree, no checkout), stamped with its commit as the
# image build stamps a server; a later run, a negative control's among them, boots that build.
base_commit="$(git -C "$workspace" rev-parse --verify --quiet "${baseline_sha}^{commit}")" || {
  printf "one-inbox-census.sh: BASELINE_SHA is '%s', which names no commit of %s\n" "$baseline_sha" "$workspace" >&2
  exit 2
}
base_dir="$TMPDIR/base-census"
mkdir -p "$base_dir"
base_binary="$base_dir/dispatch-$base_commit"
if ! flock "$base_dir/build.lock" bash -c '
  set -euo pipefail
  binary="$1" commit="$2" repo="$3" tree="$4"
  [[ -x "$binary" ]] && exit 0
  rm -rf "$tree"
  mkdir -p "$tree"
  git -C "$repo" archive "$commit" | tar -x -C "$tree"
  GOFLAGS=-p=1 GOMAXPROCS=1 GOGC=20 GOMEMLIMIT=3500MiB go -C "$tree/packages/envoy" build \
    -ldflags "-X main.buildCommit=$commit" -o "$binary.partial" ./cmd/dispatch
  mv "$binary.partial" "$binary"
  rm -rf "$tree"
' build "$base_binary" "$base_commit" "$workspace" "$base_dir/tree-$base_commit" >"$work/base-build.log" 2>&1; then
  fail "build the release before ($base_commit) in $base_dir: $(tail -20 "$work/base-build.log")"
  exit 1
fi
head_sha="$(git -C "$workspace" rev-parse HEAD)"
# HEAD_BINARY names a build of this tree already made, which the negative control gives: the
# control patches a scratch copy of the tree, builds it and runs this driver against that binary,
# so the run is the same in every other way.
if [[ -n "${HEAD_BINARY:-}" ]]; then
  cp "$HEAD_BINARY" "$work/head-dispatch"
  pass "built the release before ($base_commit, $base_binary) and took this build from HEAD_BINARY ($HEAD_BINARY)"
else
  if ! GOFLAGS=-p=1 GOMAXPROCS=1 GOGC=20 GOMEMLIMIT=3500MiB go -C "$workspace/packages/envoy" build \
    -ldflags "-X main.buildCommit=$head_sha" -o "$work/head-dispatch" ./cmd/dispatch >"$work/head-build.log" 2>&1; then
    fail "build this tree ($head_sha): $(tail -20 "$work/head-build.log")"
    exit 1
  fi
  pass "built the release before ($base_commit, $base_binary) and this build ($head_sha)"
fi

# boot BINARY LOG: starts a server on the shared database and the bus, and answers its base URL in
# `base`; its pid is left in `boot_pid`. The log is made before the server starts, so the wait
# reads a file from its first look, and the server's own redirect then writes it from its start.
boot() {
  local binary="$1" log="$2" port
  port="$(free_port)"
  : >"$log"
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
  boot_pid=$!
  base="http://127.0.0.1:$port"
  wait_for_line "$log" "listening" "$boot_pid"
}

# The person every write of this corpus is made as, named to the server through the trusted header
# its identity setting reads (DISPATCH_IDENTITY=header:X-Dispatch-User): a write needs a signed-in
# person (api's writeNeedsASignedInPerson), and the agent bearer names a session and no person.
readonly census_person=alice
readonly census_other=bob
# The person the mention arm names: a reference names a person by their login as Dispatch stores
# one, an email lowercased (`dispatch://person/<login>`), and no bare login.
readonly census_named=person-1@example.com

# request WHO METHOD PATH [BODY]: one call as WHO - a person's login, named through the trusted
# header, or `agent`, the agent bearer, for the routes a session drives (a session's reply to a
# comment and to a direct message). It answers the body and leaves the status in $work/status.
request() {
  local who="$1" method="$2" path="$3" body="${4:-}"
  local -a auth data=()
  if [[ "$who" == agent ]]; then
    auth=(-H @"$auth_header")
  else
    auth=(-H "X-Dispatch-User: $who")
  fi
  [[ -z "$body" ]] || data=(-H 'Content-Type: application/json' -d "$body")
  : >"$work/answer"
  curl -sS -o "$work/answer" -w '%{http_code}' -X "$method" "${auth[@]}" "${data[@]}" "$base$path" \
    >"$work/status" 2>>"$work/curl.log" || true
  cat "$work/answer"
}

# expect WHO WANT METHOD PATH BODY WHAT: a call as WHO whose status must be WANT; it answers the
# body.
expect() {
  local who="$1" want="$2" method="$3" path="$4" body="$5" what="$6" answer status
  answer="$(request "$who" "$method" "$path" "$body")"
  status="$(<"$work/status")"
  if [[ "$status" != "$want" ]]; then
    fail "$what: $method $path answered $status, want $want: $answer"
    return 1
  fi
  printf '%s' "$answer"
}

mkdir -p "$work/home"

# The corpus's own count of each narrowing the check names, which seed_half adds to as each write
# that makes one of them lands, so the census's figure of each is held to what the run wrote and not
# to what the census read. no_project: each direct message and each broadcast copy the person sends
# is the root of a thread that names no project, which they take part in and keep no row of
# (inboxwrite.Write.Project). session_authors: each reply a session writes - to its mention, to its
# direct message and to its broadcast copy - is that session's part in a thread, which no row of
# people holds.
no_project_threads=0
session_threads=0

# seed_half HALF: the corpus arms one build writes. Each arm is written through the API of the
# server now booted, so each is that build's own: the first half the release before's, the second
# this build's. HALF names the arm's rows apart.
seed_half() {
  local half="$1" issue closed ask answered answer answer_at comment message version dismissed
  local dm_message dm read_mark agent_reply broadcast copy document document_id
  issue="$(expect "$census_person" 201 POST "/api/v1/issues" \
    "{\"project\":\"$project\",\"title\":\"$(printf %s "$half" | tr a-z A-Z) seeding $RANDOM\",\"body\":\"The $half half of the census corpus.\"}" \
    "$half: the issue" | jq -r .key)" || return 0
  closed="$(expect "$census_person" 201 POST "/api/v1/issues" \
    "{\"project\":\"$project\",\"title\":\"$(printf %s "$half" | tr a-z A-Z) shut $RANDOM\",\"body\":\"Closed.\"}" \
    "$half: the issue that closes" | jq -r .key)" || true

  # Four ask arms: an assigned open ask, an answered one, one on a closed issue, and one on a
  # project document, written below with the document.
  ask="$(expect "$census_person" 201 POST "/api/v1/issues/$issue/asks" \
    "{\"question\":\"Ship the $half half of dispatch://$issue?\",\"asked_of\":\"alice\"}" "$half: the open ask" | jq -r .id)" || true
  answered="$(expect "$census_person" 201 POST "/api/v1/issues/$issue/asks" \
    "{\"question\":\"Answered in the $half half of dispatch://$issue?\",\"asked_of\":\"alice\"}" "$half: the answered ask")" || true
  answer="$(jq -r '.id // empty' <<<"$answered")"
  if [[ -n "${answer:-}" && "$answer" != null ]]; then
    # The answer names the revision the person reviewed, which is the ask's edited_at exactly, and
    # an ask nothing has edited holds none: the route compares the two as they are (answerTransition).
    answer_at="$(request "$census_person" GET "/api/v1/asks/$answer" | jq '.edited_at')"
    expect "$census_person" 200 POST "/api/v1/asks/$answer/answer" \
      "{\"text\":\"Yes, in the $half half.\",\"expected_edited_at\":$answer_at}" "$half: the answer" >/dev/null || true
  fi
  if expect "$census_person" 201 POST "/api/v1/issues/$closed/asks" \
    "{\"question\":\"An ask on a closed issue ($half)?\",\"asked_of\":\"alice\"}" "$half: the ask on the issue that closes" >/dev/null; then
    # The issue closes under its open ask, which the close withdraws.
    expect "$census_person" 200 PATCH "/api/v1/issues/$closed" '{"status":"done"}' \
      "$half: the close of the issue the ask is on" >/dev/null || true
  fi

  # Thread arms: a comment thread with a session's reply, a thread read, a dismissed thread, a
  # snoozed ask.
  comment="$(expect "$census_person" 201 POST "/api/v1/issues/$issue/comments" \
    "{\"body\":\"A $half comment thread, for @census-$half, on dispatch://$issue.\",\"mentions\":[{\"target\":\"session:census-$half\"}],\"delivery\":\"btw\"}" \
    "$half: the comment thread" | jq -r .id)" || true
  if [[ -n "${comment:-}" && "$comment" != null ]]; then
    expect "$census_other" 201 POST "/api/v1/issues/$issue/comments" \
      "{\"body\":\"A reply by somebody else ($half), on dispatch://$issue.\",\"reply_to\":\"$comment\",\"answers\":\"$comment\"}" \
      "$half: the reply in the comment thread" >/dev/null || true
    # The session may reply only to the delivery it took, so the run waits for the mention's
    # attempt to settle against it before the session answers (model.DeliveryTaken).
    if settled_attempt "comment_deliveries" "comment_id = '$comment'"; then
      if expect agent 201 POST "/api/v1/comments/$comment/reply" \
        "{\"actor\":{\"kind\":\"session\",\"id\":\"census-$half\"},\"attempt\":1,\"target\":\"session:census-$half\",\"body\":\"A session's reply ($half).\"}" \
        "$half: the session's reply to its mention" >/dev/null; then
        session_threads=$((session_threads + 1))
      fi
    else
      fail "$half: the mention's attempt never settled, so the session had no delivery of its own to answer"
    fi
    expect "$census_person" 204 PUT "/api/v1/me/threads/$comment/read" \
      "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: the thread read" >/dev/null || true
  fi

  # Message arms: an issue message thread, a direct message with a session's reply, a broadcast
  # copy with a reply.
  message="$(expect "$census_person" 201 POST "/api/v1/issues/$issue/messages" \
    "{\"body\":\"A $half issue message about dispatch://$issue.\"}" "$half: the issue message" | jq -r .id)" || true
  if [[ -n "${message:-}" && "$message" != null ]]; then
    expect "$census_other" 201 POST "/api/v1/issues/$issue/messages" \
      "{\"body\":\"A reply in the message thread ($half).\",\"in_reply_to\":\"$message\"}" \
      "$half: the reply in the message thread" >/dev/null || true
  fi
  dm_message="$(expect "$census_person" 201 POST "/api/v1/agents/census-$half/messages" \
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
    no_project_threads=$((no_project_threads + 1))
    agent_reply=""
    if settled_attempt "message_deliveries" "message_id = '$dm'"; then
      agent_reply="$(expect agent 201 POST "/api/v1/messages/$dm/reply" \
        "{\"actor\":{\"kind\":\"session\",\"id\":\"census-$half\"},\"attempt\":1,\"body\":\"A session's direct reply ($half).\"}" \
        "$half: the session's direct reply" | jq -r .id)" || true
    else
      fail "$half: the direct message's attempt never settled, so the session had no delivery of its own to answer"
    fi
    if [[ -n "${agent_reply:-}" && "$agent_reply" != null ]]; then
      session_threads=$((session_threads + 1))
      # The reply a person read by its id (user_agent_reply_read), and the dismissal of the Agent
      # thread's root (user_agent_thread_dismissed), which carries what the page drew of that row.
      expect "$census_person" 200 PUT "/api/v1/me/agents/census-$half/state" \
        "{\"read_replies\":[\"$agent_reply\"]}" "$half: the reply read by its id" >/dev/null || true
      expect "$census_person" 200 POST "/api/v1/me/inbox/dismiss" \
        "{\"threads\":[\"agent:$dm\"],\"seen_replies\":{\"agent:$dm\":{\"newest_reply\":\"$agent_reply\",\"unread_replies\":[]}}}" \
        "$half: the Agent thread's dismissal" >/dev/null || true
    fi
  fi
  # A broadcast to the half's session, and the session's reply to the copy it was sent, once that
  # copy's attempt settled against it.
  broadcast="$(expect "$census_person" 201 POST "/api/v1/broadcasts" \
    "{\"body\":\"A $half broadcast.\",\"delivery\":\"btw\",\"session_ids\":[\"census-$half\"]}" \
    "$half: the broadcast")" || true
  copy="$(jq -r --arg s "census-$half" '[.recipients[]? | select(.session_id == $s) | .message.id] | first // empty' \
    <<<"$broadcast" 2>/dev/null)" || true
  if [[ -n "${copy:-}" ]]; then
    no_project_threads=$((no_project_threads + 1))
    if settled_attempt "message_deliveries" "message_id = '$copy'"; then
      if expect agent 201 POST "/api/v1/messages/$copy/reply" \
        "{\"actor\":{\"kind\":\"session\",\"id\":\"census-$half\"},\"attempt\":1,\"body\":\"A session's reply to its broadcast copy ($half).\"}" \
        "$half: the session's reply to its broadcast copy" >/dev/null; then
        session_threads=$((session_threads + 1))
      fi
    else
      fail "$half: the broadcast copy's attempt never settled, so the session had no delivery of its own to answer"
    fi
  elif [[ -n "${broadcast:-}" ]]; then
    fail "$half: the broadcast answered no copy for census-$half: $(head -c 300 <<<"$broadcast")"
  fi

  # The read-state arms: a Clear and a read mark of a session's conversation, a snooze, a dismissal.
  expect "$census_person" 200 PUT "/api/v1/me/agents/census-$half/state" \
    "{\"cleared_before\":\"${read_mark:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}\",\"read_through\":\"${read_mark:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}\"}" \
    "$half: the Clear and the read mark" >/dev/null || true
  if [[ -n "${ask:-}" && "$ask" != null ]]; then
    expect "$census_person" 200 PUT "/api/v1/me/asks/$ask/snooze" \
      "{\"snoozed_until\":\"$(date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: the snooze" >/dev/null || true
  fi
  if [[ -n "${message:-}" && "$message" != null ]]; then
    # What the page drew of the thread is its row's version, which every write by somebody else
    # moves: a dismissal that carries another version dismisses nothing (inboxDismissMarkThreads).
    version="$(psql_db -At -c "select version from user_threads where login = '$census_person' and thread_id = '$message'")" || version=""
    expect "$census_person" 200 POST "/api/v1/me/inbox/dismiss" \
      "{\"threads\":[\"reply:$message\"],\"seen_replies\":{\"reply:$message\":{\"version\":${version:-0}}}}" \
      "$half: the dismissal" >/dev/null || true
    dismissed="$(psql_db -At -c "select count(*) from user_threads where login = '$census_person' and thread_id = '$message' and dismissed_at is not null")" || dismissed=""
    [[ "$dismissed" == 1 ]] ||
      fail "$half: the dismissal left $census_person's row of the issue message thread $message undismissed (version ${version:-none})"
  fi
  expect "$census_person" 200 POST "/api/v1/me/inbox/read" \
    "{\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" "$half: Mark all read" >/dev/null || true

  # The mention arm: a comment that points at a person, by the address its body writes, and at a
  # role and a session, by its mentions.
  expect "$census_person" 201 POST "/api/v1/issues/$issue/comments" \
    "{\"body\":\"A $half comment that points at [@Person One](dispatch://person/$census_named), the role census-$half and the session census-$half.\",\"mentions\":[{\"target\":\"role:census-$half\"},{\"target\":\"session:census-$half\"}],\"delivery\":\"btw\"}" \
    "$half: a comment that points at a person, a role and a session" >/dev/null || true
  # A document of the project's own, whose body cites the issue, so this run writes the blocks and
  # the block links the node store's census covers, with an ask on it; and a role the issue is
  # routed to, so its role and its page are nodes the publisher writes.
  document="$(expect "$census_person" 201 POST "/api/v1/projects/$project/artifacts" \
    "{\"name\":\"census-$half.md\",\"content\":\"# The $half half\\n\\nA passage citing dispatch://$issue.\\n\"}" \
    "$half: the project document")" || true
  document_id="$(jq -r '.artifact.id // empty' <<<"$document" 2>/dev/null)" || true
  if [[ -n "${document_id:-}" ]]; then
    expect "$census_person" 201 POST "/api/v1/artifacts/$document_id/asks" \
      "{\"question\":\"Is the $half document final?\",\"asked_of\":\"alice\"}" "$half: the ask on the project document" >/dev/null || true
  elif [[ -n "${document:-}" ]]; then
    fail "$half: the project document answered no artifact id: $(head -c 300 <<<"$document")"
  fi
  expect "$census_person" 200 PATCH "/api/v1/issues/$issue" \
    "{\"route\":\"role:census-$half\"}" "$half: the role the issue is routed to" >/dev/null || true

  # One event the person wrote themselves, which draws no delivery to them.
  expect "$census_person" 201 POST "/api/v1/issues/$issue/comments" \
    "{\"body\":\"The person's own $half comment.\"}" "$half: the person's own comment" >/dev/null || true
}

# settled_attempt waits up to 30s for a row of TABLE that WHERE names to reach a state the listener
# took it in, with the session it reached (model.DeliveryTakenStatesSQL), and answers whether one
# did: the session may reply only to a delivery of its own, so a reply before the send settles is
# refused. A read that fails counts nothing.
settled_attempt() {
  local table="$1" where="$2" held
  for _ in $(seq 1 30); do
    held="$(psql_db -At -c "select count(*) from $table where $where and state in ('sent', 'queued') and coalesce(session_id, '') <> ''")" || held=""
    [[ "$held" =~ ^[1-9][0-9]*$ ]] && return 0
    sleep 1
  done
  return 1
}

# The nine tables the census covers, as the capture reads them.
capture_tables=(user_threads user_agent_read user_agent_state user_agent_reply_read
  user_agent_thread_dismissed user_ask_snooze comment_mentions comment_deliveries message_deliveries)

# capture_sql: the one statement that reads every row of the nine tables as `<table> <key>`, the
# key being the row's primary-key columns, which read_capture_sql reads from the catalog once,
# before the first capture, so both captures name each row by the same columns. A column the
# migration adds or a value the backfill moves leaves a row's key as it was: a row that is gone is
# what the comparison sees.
capture_sql=""
read_capture_sql() {
  local table columns
  for table in "${capture_tables[@]}"; do
    columns="$(psql_db -At -v ON_ERROR_STOP=1 -c "
      select string_agg(quote_ident(a.attname), ', ' order by k.n)
      from pg_index i cross join lateral unnest(i.indkey) with ordinality k(attnum, n)
      join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
      where i.indrelid = '$table'::regclass and i.indisprimary")" || return 1
    if [[ -z "$columns" ]]; then
      fail "$table has no primary key, so the capture cannot name its rows"
      return 1
    fi
    capture_sql+="${capture_sql:+ union all }select '$table ' || row($columns)::text from $table"
  done
}

# capture FILE: every row of the nine tables, by its key, sorted bytewise.
capture() {
  psql_db -At -v ON_ERROR_STOP=1 -c "$capture_sql" >"$1.unsorted" && LC_ALL=C sort -o "$1" "$1.unsorted"
}

# ───── the release before writes the first half ────────────────────────────
boot "$base_binary" "$work/base.log"
base_pid="$boot_pid"
expect "$census_person" 201 POST "/api/v1/projects" "{\"key\":\"$project\",\"name\":\"Census\",\"default_assignee\":\"alice\"}" \
  "the project the release before creates" >/dev/null || true
seed_half "before"
captured=0
if read_capture_sql && capture "$work/capture.txt"; then
  captured="$(wc -l <"$work/capture.txt")"
fi
if ((captured > 0)); then
  pass "the release before wrote $captured row(s) of the nine projected tables, captured before this build booted"
else
  fail "the release before wrote no row of the nine projected tables, or the capture could not read them, so this run compares nothing"
fi
kill "$base_pid" 2>/dev/null || true
wait "$base_pid" 2>/dev/null || true
base_pid=""

# ───── this build migrates them forward and writes the second half ─────────
boot "$work/head-dispatch" "$work/head.log"
head_pid="$boot_pid"
if "$work/head-dispatch" backfill-graph-store >"$work/backfill.log" 2>&1; then
  pass "the backfill ran over the rows the release before wrote: $(grep -c . "$work/backfill.log") line(s) reported"
else
  fail "envoy-dispatch backfill-graph-store exited non-zero: $(tail -5 "$work/backfill.log")"
fi
# The rows once this build migrated them and the backfill ran over them, before the second half
# writes any row of its own.
captured_after=0
if [[ -n "$capture_sql" ]] && capture "$work/capture-after.txt"; then
  captured_after=1
fi
seed_half "now"

# The outbox publishes after the write commits, and published_destinations is what it records, so
# the run waits for every event to be published before it reads the census.
owed=""
for _ in $(seq 1 60); do
  owed="$(psql_db -At -c "select count(*) from events where notify and published_at is null")" || owed="(unread)"
  [[ "$owed" == "0" ]] && break
  sleep 2
done
if [[ "$owed" == "0" ]]; then
  pass "the outbox drained the event log before the census read it ($(psql_db -At -c "select count(*) from events where published_at is not null") published, 0 owed)"
else
  fail "the outbox still owed $owed event(s) after 120 s, so the census would read the published destinations short"
fi

# ───── the census ──────────────────────────────────────────────────────────
census_status=0
DATABASE_URL="$database_url" "$work/head-dispatch" census --store --both-releases >"$work/census.log" 2>&1 || census_status=$?
if ((census_status == 0)); then
  pass "census --store --both-releases exited 0: $(grep -cE '^census: store kind=[a-z_]+ (node|tuple)s=' "$work/census.log") kind line(s), $(grep '^census: store rows_checked=' "$work/census.log")"
else
  fail "census --store --both-releases exited $census_status: $(grep -E '^census: store (REFUSED|kind=[a-z_]+ difference)' "$work/census.log" | head -20)"
fi
while read -r line; do
  printf 'census: %s\n' "$line"
done < <(grep -E '^census: store kind=|^census: store rows_checked=' "$work/census.log" | sed 's/^census: store //')

# A kind agrees where neither direction of the census's comparison holds a difference: no row the
# store does not state, and nothing the store states that no row projects.
readonly agrees=' rows_without_a_(node|tuple)=0 (node|tuple)s_without_a_row=0 '
for kind in user_threads user_threads_version user_agent_read user_agent_state user_agent_reply_read \
  user_agent_thread_dismissed user_ask_snooze comment_mentions comment_deliveries message_deliveries \
  published_destinations message comment_pointers comment block block_ref role page; do
  line="$(grep -m1 -E "^census: store kind=$kind (nodes|tuples)=" "$work/census.log" || true)"
  if [[ -z "$line" ]]; then
    fail "the census printed no line for kind $kind"
    continue
  fi
  populations="$(grep "^census: store kind=$kind release_before_rows=" "$work/census.log" || true)"
  if [[ "$line" == *" rows=0 "* ]]; then
    fail "the census checked no row of kind $kind, so it proves nothing of it: $line"
  elif [[ ! "$line" =~ $agrees ]]; then
    fail "kind $kind differs between its rows and the store: $line"
  elif [[ "$populations" == *"release_before_rows=0"* || "$populations" == *"this_release_rows=0"* ]]; then
    fail "kind $kind holds one population only, so the comparison proves nothing of the other: $populations"
  else
    pass "kind $kind agrees: $(sed 's/^census: store //' <<<"$line") | $(sed "s/^census: store kind=$kind //" <<<"$populations")"
  fi
done

# The unmarked class: the rows a build of the release before wrote that no backfill has marked yet,
# which each kind of the one inbox counts as a class of its own and never as a difference. The
# census refuses nothing for that class (graphcensus's StoreReport.Refused): during a roll a pod of
# the release before writes such rows by design, until the block-index tick's catch-up marks them
# (graphcensus.CatchUpOwedRows), so only the caller knows when the class must be empty. This run
# stopped the release before ahead of the backfill and wrote nothing through it since, so here the
# class reads zero in every kind that counts it: each kind of the one inbox the check names must
# print its class at zero, and so must every other kind that prints one.
for kind in user_threads user_agent_read user_agent_state user_agent_reply_read user_agent_thread_dismissed \
  user_ask_snooze comment_mentions comment_deliveries message_deliveries published_destinations message \
  comment_pointers; do
  line="$(grep -m1 -E "^census: store kind=$kind unmarked_rows=" "$work/census.log" || true)"
  if [[ -z "$line" ]]; then
    fail "the census printed no unmarked class for kind $kind, so a row the backfill left unmarked would be a silence"
  elif [[ "$line" != *" unmarked_rows=0 "* ]]; then
    fail "kind $kind holds rows the backfill left unmarked once it ended, which the census holds to no tuple: ${line#census: store }"
  else
    pass "kind $kind reads its unmarked class at zero once the backfill ended: ${line#census: store }"
  fi
done
unmarked_classes="$(grep -cE '^census: store kind=[a-z_]+ unmarked_rows=' "$work/census.log" || true)"
unmarked_left="$(grep -E '^census: store kind=[a-z_]+ unmarked_rows=' "$work/census.log" | grep -v ' unmarked_rows=0 ' || true)"
if [[ -n "$unmarked_left" ]]; then
  fail "the census reads rows the backfill left unmarked once it ended: $(sed 's/^census: store //; s/ (rows a writer .*//' <<<"$unmarked_left" | paste -sd ';' -)"
else
  pass "every one of the $unmarked_classes unmarked classes the census prints reads zero once the backfill ended"
fi

# The narrowings the check names: the tuples a projection does not hold by design, which the
# census counts on a line of their own and leaves out of both comparisons. A figure is no proof
# unless it is held to something: a narrowing whose test reached past its class would carry a
# difference out of the comparison into its count, and the census would still read zero. So each
# is held to the corpus's own count of it (no_project_threads and session_threads, which seed_half
# counts), and a figure the census does not print fails as a silence.
for narrowed in "no_project $no_project_threads" "session_authors $session_threads"; do
  name="${narrowed%% *}" want="${narrowed#* }"
  line="$(grep -m1 -E "^census: store kind=user_threads narrowing=$name count=" "$work/census.log" || true)"
  count="$(sed -nE 's/^census: store kind=user_threads narrowing=[a-z_]+ count=([0-9,]+) .*/\1/p' <<<"$line" | tr -d ,)"
  if [[ -z "$line" ]]; then
    fail "the census printed no figure for the narrowing $name of user_threads, so it is a silence and not a figure"
  elif [[ "$count" != "$want" ]]; then
    fail "the narrowing $name of user_threads counts ${count:-no number}, and the corpus wrote $want of it: ${line#census: store }"
  else
    pass "the narrowing $name of user_threads counts the $want the corpus wrote: ${line#census: store }"
  fi
done
# The other narrowings are figures of the corpus too, which the check's words do not name; each is
# printed as the census read it.
sed -n 's/^census: store \(kind=[a-z_]* narrowing=\)/narrowing: \1/p' "$work/census.log" |
  grep -vE '^narrowing: kind=user_threads narrowing=(no_project|session_authors) ' || true

# The capture: every row the release before wrote is still in its own table, by its key, once this
# build had migrated it forward and the backfill had run over it.
if ((captured == 0 || captured_after == 0)); then
  fail "no pair of captures to compare: the capture before this build booted read $captured row(s), and the one after the backfill was$( ((captured_after)) || printf ' not') read"
else
  lost_rows="$(LC_ALL=C comm -23 "$work/capture.txt" "$work/capture-after.txt")"
  if [[ -z "$lost_rows" ]]; then
    pass "every row of the nine tables the release before wrote is still there after the migration and the backfill, by its key ($captured captured)"
  else
    fail "$(grep -c . <<<"$lost_rows") row(s) the release before wrote are gone from their table after the migration and the backfill, which the census cannot see once both sides lost them: $(head -20 <<<"$lost_rows" | paste -sd ';' -)"
  fi
fi

# ───── one writer ──────────────────────────────────────────────────────────
# pg_proc is read for every function of this schema whose code writes one of the nine projected
# tables, and for every function whose code calls user_threads_record, the one writer of
# user_threads; pg_trigger for the triggers that run them. A function's code is its body less its
# comments, so a comment that names a table writes nothing and one that names a marker tests
# nothing. A write is an insert, an update, a delete or a merge of one of the nine, named bare,
# with a schema or in quotes.
projected_write="\\m(insert[[:space:]]+into|update|delete[[:space:]]+from|merge[[:space:]]+into)[[:space:]]+(only[[:space:]]+)?(\"?[[:alnum:]_]+\"?[[:space:]]*\\.[[:space:]]*)?\"?($(IFS='|' && printf '%s' "${capture_tables[*]}"))\"?([^[:alnum:]_\"]|\$)"
# The marker test of R9: an `if` on the marker a write this release recorded in Go leaves on its
# row - a comment's tuples_stored, a message's node pair, an ask's tuples_stored_at, or
# threads_recorded - that returns before the function writes.
marker_test='\mif\M[^;]*(tuples_stored|node_kind[[:space:]]+is[[:space:]]+not[[:space:]]+null|threads_recorded)[^;]*\mthen[[:space:]]+return\M'
# one_writer_code: the functions of this schema as `parts`, each with its code, its first write of a
# projected table (first_write), and its code before that write (before_write) and before its first
# call of user_threads_record (before_call), each null where it has none.
one_writer_code="$(
  cat <<'SQL'
with code as (
  select p.oid, p.proname,
         regexp_replace(regexp_replace(p.prosrc, '/\*.*?\*/', '', 'g'), '--[^\n]*', '', 'g') as body
  from pg_proc p join pg_namespace n on n.oid = p.pronamespace
  join pg_language l on l.oid = p.prolang
  where n.nspname = current_schema() and l.lanname not in ('internal', 'c')
), parts as (
  select oid, proname,
         btrim(regexp_replace((regexp_match(body, '(' || :'write_re' || ')', 'i'))[1], '[[:space:]]+', ' ', 'g')) as first_write,
         substring(body from '(?i)^(.*?)' || :'write_re') as before_write,
         substring(body from '(?i)^(.*?)\muser_threads_record[[:space:]]*\(') as before_call
  from code
)
SQL
)"
# one_writer_read QUERY: QUERY over one_writer_code's `parts`, one row a line, its fields split by |.
one_writer_read() {
  printf '%s\n%s;\n' "$one_writer_code" "$1" |
    psql_db -At -v ON_ERROR_STOP=1 -v write_re="$projected_write" -v marker_re="$marker_test" 2>"$work/psql"
}
want_writers="user_agent_thread_dismissed_clear,user_threads_record"
if ! written="$(one_writer_read "select proname, first_write from parts where first_write is not null order by proname")"; then
  fail "read pg_proc for the functions that write a projected table: $(<"$work/psql")"
else
  writers="$(cut -d'|' -f1 <<<"$written" | paste -sd, -)"
  if [[ "$writers" == "$want_writers" ]]; then
    pass "pg_proc holds two functions whose code writes one of the nine projected tables, the one writer of user_threads and the one exemption: $writers"
  else
    fail "pg_proc holds the functions ${writers:-none} whose code writes one of the nine projected tables, want $want_writers alone: $(sed -E 's/\|/ (/; s/$/)/' <<<"$written" | paste -sd ';' -)"
  fi
fi
# Each function that calls user_threads_record, and each other writer of a projected table, holds the
# marker test before its first call and before its first write, and a trigger runs it.
if ! guarded="$(one_writer_read "
  select g.proname,
         concat_ws(' and ', case when g.before_call is not null then 'calls user_threads_record' end,
                   case when g.before_write is not null then 'writes ' || g.first_write end),
         coalesce(g.before_call ~* :'marker_re', true) and coalesce(g.before_write ~* :'marker_re', true),
         coalesce(string_agg(c.relname || '.' || t.tgname, ',' order by c.relname, t.tgname), '')
  from parts g
  left join pg_trigger t on t.tgfoid = g.oid and not t.tgisinternal
  left join pg_class c on c.oid = t.tgrelid
  where g.proname <> 'user_threads_record' and (g.before_call is not null or g.before_write is not null)
  group by g.proname, g.before_call, g.before_write, g.first_write
  order by g.proname")"; then
  fail "read pg_proc for the functions that call user_threads_record: $(<"$work/psql")"
else
  unguarded=0 callers=0 names=() fired_by=()
  while IFS='|' read -r name does marked fired; do
    [[ -n "$name" ]] || continue
    names+=("$name")
    [[ "$does" == *"calls user_threads_record"* ]] && callers=$((callers + 1))
    if [[ "$marked" != t ]]; then
      fail "$name $does with no marker test of R9 before it, an if on tuples_stored, the node pair, tuples_stored_at or threads_recorded that returns"
      unguarded=$((unguarded + 1))
    fi
    if [[ -z "$fired" ]]; then
      fail "pg_trigger runs no trigger of $name, which $does, so its marker test stands in front of nothing"
      unguarded=$((unguarded + 1))
    else
      fired_by+=("$fired→$name")
    fi
  done <<<"$guarded"
  if ((callers == 0)); then
    fail "pg_proc holds no function that calls user_threads_record, the one writer of user_threads"
  elif ((unguarded == 0)); then
    pass "each of the ${#names[@]} functions that call user_threads_record or write a projected table beside it writes only under its marker test of R9, and pg_trigger runs each: ${fired_by[*]}"
  fi
fi
ask_skip="$(psql_db -At -c "
  select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace
  where n.nspname = current_schema() and p.proname = 'user_threads_ask'
    and p.prosrc ~ 'tuples_stored_at is distinct from \(new.answer->>''at''\)::timestamptz'")"
if [[ "$ask_skip" == "1" ]]; then
  pass "user_threads_ask skips an answer only where new.tuples_stored_at = (new.answer->>'at')::timestamptz"
else
  fail "user_threads_ask does not skip an answer by the marker R9 names"
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
# Each writer the census names, armed through the seam and then driven: the write fails at the
# seam, and neither its tuples nor its projection row nor its event is there afterwards.

# The words the injected failure is logged with (failpoint.ErrInjected), which the server's line
# for a failed write carries.
readonly injected_failure='failure injected between the store write and the projection write'

# snapshot_sql: what a write that failed must leave as it found it, read in one statement: how many
# tuples, nodes, events, comments and messages there are, and a digest of every row, whole, of the
# asks and of the nine projected tables, one line each.
snapshot_sql="select 'graph_tuples ' || count(*) from graph_tuples
  union all select 'graph_nodes ' || count(*) from graph_nodes
  union all select 'events ' || count(*) from events
  union all select 'comments ' || count(*) from comments
  union all select 'messages ' || count(*) from messages"
for table in asks "${capture_tables[@]}"; do
  snapshot_sql+="
  union all select '$table ' || md5(coalesce(string_agg(t::text, ' ' order by t::text), '')) from $table t"
done
readonly snapshot_sql

# injections: how many times the server has logged the injected failure.
injections() { grep -cF "$injected_failure" "$work/head.log" || true; }

# inject WRITER WHAT WHO METHOD PATH [BODY]: arms WRITER, then sends the write that reaches it as
# WHO (request's arguments). The write must answer 500 and the server must log the injected
# failure once more, so the write reached the seam and was not refused before it; and the
# snapshot must read after the write as it read before it.
inject() {
  local writer="$1" what="$2" code logged waited=0
  shift 2
  request "$census_person" PUT "/api/v1/writes/_test/fail-between/$writer" '' >/dev/null
  code="$(<"$work/status")"
  if [[ "$code" != 200 ]]; then
    fail "arming $writer answered $code: $(head -c 300 "$work/answer")"
    return 0
  fi
  logged="$(injections)"
  if ! psql_db -At -v ON_ERROR_STOP=1 -c "$snapshot_sql" >"$work/before" 2>"$work/psql"; then
    fail "$writer: read the state before $what: $(<"$work/psql")"
    return 0
  fi
  request "$@" >/dev/null
  code="$(<"$work/status")"
  # The server logs a failed write once it has answered it, so its line may follow the answer.
  while [[ "$(injections)" == "$logged" ]] && ((waited < 50)); do
    sleep 0.1
    waited=$((waited + 1))
  done
  if [[ "$code" != 500 || "$(injections)" != "$((logged + 1))" ]]; then
    fail "$writer: $what answered $code ($(head -c 300 "$work/answer")) and the server logged the injected failure $(($(injections) - logged)) time(s) since it was armed, want 500 and once: the write never reached the seam between its store write and its projection write"
    return 0
  fi
  if ! psql_db -At -v ON_ERROR_STOP=1 -c "$snapshot_sql" >"$work/after" 2>"$work/psql"; then
    fail "$writer: read the state after $what: $(<"$work/psql")"
    return 0
  fi
  if cmp -s "$work/before" "$work/after"; then
    pass "a failure injected between $writer's store write and its projection write left neither side: $what answered 500, and $(head -5 "$work/before" | paste -sd ' ' -) and every row of the asks and the nine tables read as before"
  else
    fail "a failure injected into $writer left rows behind ($what): $(diff "$work/before" "$work/after" | grep '^[<>]' | paste -sd ';' -)"
  fi
}

# soon: a moment thirty seconds ahead, which a read mark is stored as now (no mark is stored ahead
# of now), so the mark moves past every row written before it, whatever second it falls in.
soon() { date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%SZ; }

# What the injected writes reach: an issue; a comment of the person's that points at the session
# census-inject, whose reply is the one Go writer of user_threads; an issue message of theirs with
# somebody else's reply under it, so Mark all read and the dismissal each have a thread to change;
# an ask; and a direct message to the session, whose replies are the direct reply and the clear.
# The session replies only to a delivery it took, so both deliveries settle first.
inject_issue="$(expect "$census_person" 201 POST "/api/v1/issues" \
  "{\"project\":\"$project\",\"title\":\"Census injection\",\"body\":\"Injected.\"}" "the injection's issue" | jq -r .key)" || inject_issue=""
inject_comment="$(expect "$census_person" 201 POST "/api/v1/issues/$inject_issue/comments" \
  '{"body":"A comment to reply to, for the session census-inject.","mentions":[{"target":"session:census-inject"}],"delivery":"btw"}' \
  "the comment the injection replies to" | jq -r .id)" || inject_comment=""
inject_message="$(expect "$census_person" 201 POST "/api/v1/issues/$inject_issue/messages" \
  '{"body":"A message to reply to."}' "the message the injection reads and dismisses" | jq -r .id)" || inject_message=""
expect "$census_other" 201 POST "/api/v1/issues/$inject_issue/messages" \
  "{\"body\":\"A reply by somebody else.\",\"in_reply_to\":\"$inject_message\"}" \
  "somebody else's reply in the injection's message thread" >/dev/null || true
inject_ask="$(expect "$census_person" 201 POST "/api/v1/issues/$inject_issue/asks" \
  '{"question":"Injected?","asked_of":"alice"}' "the ask the injection snoozes and answers" | jq -r .id)" || inject_ask=""
inject_dm="$(expect "$census_person" 201 POST "/api/v1/agents/census-inject/messages" \
  '{"body":"A direct message.","delivery":"btw"}' "the direct message the injection replies to" | jq -r .id)" || inject_dm=""
settled_attempt comment_deliveries "comment_id = '$inject_comment'" ||
  fail "the injection's comment never settled its delivery to census-inject, so the session has no delivery of its own to reply to"
settled_attempt message_deliveries "message_id = '$inject_dm'" ||
  fail "the injection's direct message never settled its delivery to census-inject, so the session has no delivery of its own to reply to"
inject_version="$(psql_db -At -c "select version from user_threads where login = '$census_person' and thread_id = '$inject_message'")" || inject_version=""
# The ask's edited_at as JSON, null for an ask nothing has edited, which the answer names as it is.
inject_edited="$(request "$census_person" GET "/api/v1/asks/$inject_ask" | jq -c .edited_at)" || inject_edited=""

inject comment "a comment's write" "$census_person" POST "/api/v1/issues/$inject_issue/comments" \
  '{"body":"The comment the failure takes."}'
inject message "an issue message's write" "$census_person" POST "/api/v1/issues/$inject_issue/messages" \
  '{"body":"The message the failure takes."}'
inject direct-reply "a session's direct reply" agent POST "/api/v1/messages/$inject_dm/reply" \
  '{"actor":{"kind":"session","id":"census-inject"},"attempt":1,"body":"The reply the failure takes."}'
inject user-threads "the one Go writer of user_threads" agent POST "/api/v1/comments/$inject_comment/reply" \
  '{"actor":{"kind":"session","id":"census-inject"},"attempt":1,"target":"session:census-inject","body":"The reply whose thread write the failure takes."}'
inject agent-dismissal-clear "the Go writer that clears an Agent dismissal" agent POST "/api/v1/messages/$inject_dm/reply" \
  '{"actor":{"kind":"session","id":"census-inject"},"attempt":1,"body":"The reply whose clear the failure takes."}'
inject thread-read "a thread's read mark" "$census_person" PUT "/api/v1/me/threads/$inject_comment/read" \
  "{\"read_through\":\"$(soon)\"}"
inject inbox-read "Mark all read" "$census_person" POST "/api/v1/me/inbox/read" \
  "{\"read_through\":\"$(soon)\"}"
inject inbox-dismiss "a dismissal" "$census_person" POST "/api/v1/me/inbox/dismiss" \
  "{\"threads\":[\"reply:$inject_message\"],\"seen_replies\":{\"reply:$inject_message\":{\"version\":${inject_version:-0}}}}"
inject agent-state "an agent's Clear and read mark" "$census_person" PUT "/api/v1/me/agents/census-inject/state" \
  "{\"cleared_before\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"read_through\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
inject ask-snooze "a snooze" "$census_person" PUT "/api/v1/me/asks/$inject_ask/snooze" \
  "{\"snoozed_until\":\"$(date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)\"}"
inject ask-answer "an ask's answer" "$census_person" POST "/api/v1/asks/$inject_ask/answer" \
  "{\"text\":\"The answer the failure takes.\",\"expected_edited_at\":${inject_edited:-null}}"

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
