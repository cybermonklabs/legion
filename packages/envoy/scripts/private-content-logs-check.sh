#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329 # stop is invoked by the traps
# packages/envoy/scripts/private-content-logs-check.sh
#
# Proves, on the whole local stack behind the NATS auth service (dev-stack.sh --callout: the real
# brokers, listener, Dispatch and nats-server, each the process production runs), that Dispatch,
# the listener and the broker write no private project's text to their logs. Every worker of a
# deployment can read those services' logs, so a caller's text in one of them has left its project.
# A search of the log calls by the names of their keys cannot see an error string that quotes a
# body, so this check reads the logs themselves.
#
# It mints a marker, R14-MARK-<16 hex>, whose lower-case form r14-mark-<16 hex> is also a slug, a
# role and the local part of an email. Then, each through the real route, as PRIV's owner
# (DEV_STACK_PRIV_OWNER, signed in to Dispatch at /auth/_dev/signin) unless it says otherwise, it
# sets the project up:
#   1. creates the project PRIV (DEV_STACK_PRIV_PROJECT), unless it exists;
#   2. makes PRIV private, PATCH /api/v1/projects/PRIV/access {"visibility": "private"}, and holds
#      the answer to "visibility": "private";
#   3. registers PRIV's session (DEV_STACK_PRIV_SESSION) at the listener, under the stack's listener
#      token, with a consumer and the delivery mode steer, which a comment's mention and a direct
#      message are sent in. A listener that delivers a private project's mention only to a session
#      whose own credential lists the project delivers nothing to a session registered so, and the
#      run is then UNKNOWN (step 9) until the session registers with a token of its own;
# notes the size of dispatch.log, listener.log and broker.log under the stack's logs/, from which
# point each of them must grow, and writes the marker:
#   4. adds the person r14-mark-<hex>@example.com to PRIV's access list,
#      PUT /api/v1/projects/PRIV/access/people/<email>;
#   5. writes the secret PRIV_LOG_CHECK, tagged dispatch-project=PRIV and holding the marker, into
#      the stack's secret store (its fake-secrets.json: the stack has no cloud store for
#      `agent-secrets secret create` to write), asks the broker to reread it as that command does
#      when it holds no session (POST /v1/secrets/PRIV_LOG_CHECK/reread, answered 202, which the
#      broker logs only if the reread fails), and asks again with a proof no session signed, which
#      the broker refuses and logs: the one answer about PRIV's secret that the broker always logs.
#      No step makes the broker read the secret's value, which only a grant to a session reads;
#   6. writes an issue of PRIV (its title and spec) and a project document named r14-mark-<hex>,
#      which is its slug, and reads the document back by that slug, holding the answer to that slug
#      and project;
#   7. writes a comment on the issue mentioning PRIV's session, an ask (its question and options),
#      the ask's answer (its option and text) and a direct message to the session;
#   8. asks Dispatch's GitHub proxy for /api/github/rest/r14-mark-<hex>, a path none of the proxy's
#      classes reads, which it refuses 404 GITHUB_PATH_REFUSED;
#   9. waits until Dispatch records the comment's delivery to PRIV's session as sent or queued:
#      the listener took it into that session's own stream, where a running session reads it.
#      This stack runs no client that reads the stream, so the listener records it queued.
# Every text field of every write holds the marker; no repository's name does, since a
# repository's name is an identifier. A step that is refused stops the run there, so nothing of
# PRIV's is written to a project that stayed public. Last, it searches every file under logs/ for
# the marker, ignoring case and reading a binary file as text.
#
# Each line of the verdict is FAIL or UNKNOWN; there is no PASS line but the last. A line of a log
# that names the marker is `FAIL logs/<file>:<line>: <marker>: <the line, clipped to 300 bytes>`.
# A service log that did not grow from the first marked write, a step that was refused, a delivery
# that never came, a file under logs/ that could not be read, and a logs/ that lists no file are
# each `UNKNOWN <what>`, since a log that saw nothing has not shown it holds nothing. The last line
# is `private-content-logs-check: passed: ...`, exit 0; `...: failed: <marker> is on <n> lines ...`,
# exit 1, on any FAIL; or `...: UNKNOWN: ...`, exit 2, on an UNKNOWN and no FAIL. A refusal to run
# at all (a stack that never came up, an exported stack missing a variable) is exit 2 too.
#
# The negative control, --control, first claims the role r14-mark-<hex> at the listener from a
# session with no client instance id, soft and naming as its predecessor the live session that
# holds it, which the listener logs at Info with the role ("listener role taken from a live
# predecessor because one side carries no client instance id", cmd/listener/api.go). The run must
# then FAIL naming logs/listener.log: a control run whose search finds no line of logs/listener.log
# naming the marker is UNKNOWN, exit 2, whatever else it found. It fails for the reason the probe
# would be blind: a caller's text reaching a service log is what the check hunts. The control needs
# nothing of PRIV's, so it runs before the rest, and a stack whose PRIV steps are refused still
# FAILs on it.
#
# It runs against a stack one of two ways, as scripts/callout-check.sh does. In a shell that
# exported a callout stack's banner (DEV_STACK_CALLOUT=1), against that stack. With nothing
# exported, it starts dev-stack.sh --callout itself, passing its other arguments on
# (--database-url, --binaries; else LEGION_TEST_PG_DSN or DATABASE_URL, as dev-stack.sh reads
# them), and stops it on exit, which drops its databases. Its own requests and answers are kept
# under a work directory of its own in TMPDIR, which it prints.
#
#   LEGION_TEST_PG_DSN=... bash scripts/private-content-logs-check.sh             # its own stack
#   bash scripts/private-content-logs-check.sh --database-url 'postgres://...' --control
#   (with a callout stack's banner exported) bash scripts/private-content-logs-check.sh
#
# It needs what dev-stack.sh needs, and curl and jq. scripts/private-content-logs-check.test.sh
# proves its verdicts against fakes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
usage: private-content-logs-check.sh [--control] [dev-stack.sh arguments...]

Writes a marker into every text field of a private project's issue, document, comment, ask,
answer, direct message and secret, and into the names a request path carries, on one
dev-stack.sh --callout run, then searches every file under the stack's logs/ for it.
--control also claims a role named for the marker at the listener, which logs it, so the run
must FAIL naming logs/listener.log. With a callout stack's banner exported it uses that stack;
otherwise it starts one, passing the other arguments to dev-stack.sh.

Exit 0: no log names the marker. 1: a log does. 2: something could not be done or read.
EOF
}

CONTROL=false
stack_args=()
for argument in "$@"; do
  case "$argument" in
    --control) CONTROL=true ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) stack_args+=("$argument") ;;
  esac
done

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/private-content-logs-check.XXXXXX")"
CALLS="$WORK_DIR/calls"
mkdir -p "$CALLS"
STACK_PID=""

stop() {
  local rc="$1"
  trap - EXIT
  trap '' INT TERM
  if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
    kill -TERM "$STACK_PID" 2>/dev/null || true
    wait "$STACK_PID" 2>/dev/null || true
  fi
  exit "$rc"
}
trap 'stop "$?"' EXIT
trap 'stop 130' INT
trap 'stop 143' TERM

refuse() {
  echo "private-content-logs-check: $*" >&2
  exit 2
}

# --- The stack: the caller's, when it exported one, else one of this run's own. ---
if [ "${DEV_STACK_CALLOUT:-}" != 1 ]; then
  stack_log="$WORK_DIR/dev-stack.log"
  echo "private-content-logs-check: starting dev-stack.sh --callout (log $stack_log)..." >&2
  bash "$SCRIPT_DIR/dev-stack.sh" --callout "${stack_args[@]}" >"$stack_log" 2>&1 &
  STACK_PID=$!
  ready=false
  # Twenty minutes: the stack builds its binaries first unless --binaries names them.
  for _ in $(seq 1 2400); do
    if grep -q '^dev-stack: ready\.' "$stack_log" 2>/dev/null; then
      ready=true
      break
    fi
    kill -0 "$STACK_PID" 2>/dev/null || break
    sleep 0.5
  done
  if [ "$ready" != true ]; then
    tail -n 40 "$stack_log" >&2 || true
    refuse "the stack never came up; see $stack_log"
  fi
  # The banner's exports, read as values: each `  export NAME=value` line, a quoted value unquoted.
  while IFS= read -r line; do
    name="${line#  export }"
    value="${name#*=}"
    name="${name%%=*}"
    value="${value#\"}"
    value="${value%\"}"
    [ "$name" = PATH ] && value="${value%%:*}:$PATH"
    printf -v "$name" '%s' "$value"
  done < <(sed -n 's/^\(  export [A-Z_0-9]*=.*\)$/\1/p' "$stack_log")
elif [ "${#stack_args[@]}" -gt 0 ]; then
  refuse "${stack_args[*]}: arguments for a stack of this run's own, but this shell exported one (DEV_STACK_CALLOUT=1)"
fi
for required in DEV_STACK_DIR DISPATCH_URL ENVOY_URL ENVOY_TOKEN AGENT_SECRETS_URL; do
  [ -n "${!required:-}" ] || refuse "$required is not set: export a dev-stack.sh --callout banner, or run with none exported"
done
LOG_DIR="$DEV_STACK_DIR/logs"
# The logs of the three services the check is about, each of which must grow from the first marked
# write, or it saw nothing of the run. The stack's other logs (the issuer's, natsd's, the port
# keepers', the second broker's) are searched as well, but none of them need write a line for what
# this run does.
GROWN_LOGS=(dispatch.log listener.log broker.log)
for log in "${GROWN_LOGS[@]}"; do
  [ -f "$LOG_DIR/$log" ] || refuse "the stack has no $LOG_DIR/$log"
done
declare -A size_before=()

HEX="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
MARK="R14-MARK-$HEX"
SLUG="r14-mark-$HEX"
PERSON="$SLUG@example.com"
echo "private-content-logs-check: marker $MARK (slug $SLUG), logs $LOG_DIR, requests $CALLS"

# The listener's shared token, read by curl from a file so no command line holds it.
LISTENER_HEADER="$WORK_DIR/listener-header"
(umask 077 && printf 'Authorization: Bearer %s\n' "$ENVOY_TOKEN" >"$LISTENER_HEADER")
JAR="$WORK_DIR/dispatch-cookies"

unknowns=0
unknown() {
  unknowns=$((unknowns + 1))
  echo "UNKNOWN $*"
}
note() { echo "  $*"; }

# call NAME AS METHOD URL [BODY] sends one request: as PRIV's owner (person: the cookie of the
# sign-in, with Dispatch's origin, which a cookie's write needs), as the stack's listener client
# (listener), signing in (signin: keeps the cookie), with a proof no session signed (unsigned-proof)
# or with no credential (none). It leaves the answer's status in STATUS (000 for no answer) and its
# body in ANSWER, both kept under CALLS.
STATUS="" ANSWER=""
call() {
  local name="$1" as="$2" method="$3" url="$4"
  ANSWER="$CALLS/$name.json"
  local -a args=(-sS --max-time 20 -o "$ANSWER" -w '%{http_code}' -X "$method")
  case "$as" in
    person) args+=(-b "$JAR" -H "Origin: $DISPATCH_URL") ;;
    signin) args+=(-c "$JAR") ;;
    listener) args+=(-H "@$LISTENER_HEADER") ;;
    unsigned-proof) args+=(-H "Proof: private-content-logs-check-signed-by-no-session") ;;
  esac
  if [ "$#" -ge 5 ]; then
    printf '%s' "$5" >"$CALLS/$name.request.json"
    args+=(-H 'Content-Type: application/json' --data-binary "@$CALLS/$name.request.json")
  fi
  STATUS="$(curl "${args[@]}" "$url" 2>"$CALLS/$name.err")" || true
  [ -n "$STATUS" ] || STATUS=000
}
# answered is the last call's status and its body, clipped, for a line that names a refusal.
answered() {
  local body
  body="$(jq -c . "$ANSWER" 2>/dev/null || head -c 300 "$ANSWER" 2>/dev/null || true)"
  [ -n "$body" ] || body="$(tr '\n' ' ' <"${ANSWER%.json}.err" 2>/dev/null || true)"
  printf '%s %s' "$STATUS" "${body:0:300}"
}
field() { jq -r "$1 // empty" "$ANSWER" 2>/dev/null || true; }
succeeded() { [[ "$STATUS" == 2[0-9][0-9] ]]; }
register() { # <name> <session id> [<capability>...]: a registration at the listener with a consumer
  local name="$1" session="$2"
  shift 2
  call "$name" listener POST "$ENVOY_URL/v1/interests/subscribe" \
    "$(jq -cn --arg s "$session" '{session_id: $s, topics: [], capabilities: $ARGS.positional, delivery: {consumer: true}}' --args "$@")"
}

# --- The negative control: the listener logs the role a soft claim takes from a live predecessor
# when one side carries no client instance id. Two sessions of this run's own, named for nothing
# of the marker's, so only the role can carry it. ---
control() {
  local run holder claimant session
  run="$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
  holder="private-content-logs-check-holder-$run"
  claimant="private-content-logs-check-claimant-$run"
  for session in "$holder" "$claimant"; do
    register "control-register-$session" "$session"
    succeeded || {
      unknown "the control could not register $session at the listener: POST /v1/interests/subscribe answered $(answered)"
      return
    }
  done
  call control-hold listener POST "$ENVOY_URL/v1/roles/set" "$(jq -cn --arg s "$holder" --arg r "$SLUG" '{session_id: $s, role: $r}')"
  succeeded || {
    unknown "the control's holder could not claim $SLUG: POST /v1/roles/set answered $(answered)"
    return
  }
  call control-take listener POST "$ENVOY_URL/v1/roles/set" \
    "$(jq -cn --arg s "$claimant" --arg r "$SLUG" --arg p "$holder" '{session_id: $s, role: $r, soft: true, previous_session_id: $p}')"
  succeeded || {
    unknown "the control's claimant could not take $SLUG from its predecessor: POST /v1/roles/set answered $(answered)"
    return
  }
  note "control: $claimant took the role $SLUG from its live predecessor $holder, neither naming a client instance"
}

# --- PRIV's set-up and its marked writes, stopping at the first refusal. ---
drive() {
  local missing=false required log
  for required in DEV_STACK_PRIV_PROJECT DEV_STACK_PRIV_OWNER DEV_STACK_PRIV_SESSION; do
    if [ -z "${!required:-}" ]; then
      unknown "the stack's banner names no $required, PRIV's pod; nothing of PRIV's was written"
      missing=true
    fi
  done
  [ "$missing" = false ] || return 0
  local priv="$DEV_STACK_PRIV_PROJECT" owner="$DEV_STACK_PRIV_OWNER" session="$DEV_STACK_PRIV_SESSION"

  call signin signin GET "$DISPATCH_URL/auth/_dev/signin?login=$owner"
  [[ "$STATUS" == [23][0-9][0-9] ]] || {
    unknown "$owner could not sign in to Dispatch: GET /auth/_dev/signin answered $(answered)"
    return
  }
  call project person POST "$DISPATCH_URL/api/v1/projects" \
    "$(jq -cn --arg key "$priv" --arg owner "$owner" '{key: $key, name: $key, default_assignee: $owner}')"
  [ "$STATUS" = 201 ] || [ "$STATUS" = 409 ] || {
    unknown "$priv could not be created: POST /api/v1/projects answered $(answered)"
    return
  }
  call switch person PATCH "$DISPATCH_URL/api/v1/projects/$priv/access" '{"visibility": "private"}'
  [ "$STATUS" = 200 ] || {
    unknown "$priv could not be made private: PATCH /api/v1/projects/$priv/access answered $(answered)"
    return
  }
  [ "$(field .visibility)" = private ] || {
    unknown "$priv is not private after PATCH /api/v1/projects/$priv/access: it answered $(answered)"
    return
  }
  note "$owner made $priv private"
  register session "$session" steer
  succeeded || {
    unknown "$priv's session $session could not register at the listener: POST /v1/interests/subscribe answered $(answered)"
    return
  }

  # From here every request carries the marker, so each service log must grow from this point.
  for log in "${GROWN_LOGS[@]}"; do
    size_before[$log]="$(stat -c %s "$LOG_DIR/$log")"
  done

  call access-person person PUT "$DISPATCH_URL/api/v1/projects/$priv/access/people/$PERSON"
  succeeded || {
    unknown "$PERSON could not be added to $priv's access list: PUT /api/v1/projects/$priv/access/people/$PERSON answered $(answered)"
    return
  }

  # The secret, in the store the stack's broker reads (dev-stack.sh's FAKE_SECRETS_FILE), under the
  # store name the broker reads PRIV_LOG_CHECK by (dev-stack.sh's SECRETS_PREFIX, then the name in
  # lower case with each underscore a hyphen) and the example KMS key dev-stack.sh's own secrets
  # name. Replaced in place, so the broker's reread finds it at the path it was given.
  local store="$DEV_STACK_DIR/fake-secrets.json" secret=PRIV_LOG_CHECK
  if ! jq --arg name "example/agent-secrets/priv-log-check" --arg project "$priv" --arg value "$MARK" \
    --arg kms "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab" \
    '.secrets |= (map(select(.name != $name)) + [{name: $name, kms_key_id: $kms, tags: {owner: "shared", tier: "agent", "dispatch-project": $project}, value: $value}])' \
    "$store" >"$WORK_DIR/fake-secrets.json" 2>"$WORK_DIR/fake-secrets.err"; then
    unknown "the secret $secret could not be written to $store: $(tr '\n' ' ' <"$WORK_DIR/fake-secrets.err")"
    return
  fi
  cat "$WORK_DIR/fake-secrets.json" >"$store"
  call reread none POST "$AGENT_SECRETS_URL/v1/secrets/$secret/reread"
  succeeded || {
    unknown "the broker would not reread $secret: POST /v1/secrets/$secret/reread answered $(answered)"
    return
  }
  call reread-refused unsigned-proof POST "$AGENT_SECRETS_URL/v1/secrets/$secret/reread"
  [[ "$STATUS" == 4[0-9][0-9] ]] || {
    unknown "the broker did not refuse a reread of $secret under a proof no session signed: it answered $(answered)"
    return
  }
  note "wrote $secret tagged dispatch-project=$priv; the broker took its reread and refused one under a proof no session signed"

  local issue
  call issue person POST "$DISPATCH_URL/api/v1/issues" \
    "$(jq -cn --arg p "$priv" --arg m "$MARK" '{project: $p, title: ($m + " issue"), spec: ("# " + $m + "\n\n" + $m + " spec.\n")}')"
  issue="$(field .key)"
  [ "$STATUS" = 201 ] && [ -n "$issue" ] || {
    unknown "the issue could not be written: POST /api/v1/issues answered $(answered)"
    return
  }
  call document person POST "$DISPATCH_URL/api/v1/projects/$priv/artifacts" \
    "$(jq -cn --arg slug "$SLUG" --arg m "$MARK" '{name: $slug, content: ("# " + $m + "\n\n" + $m + " document.\n"), summary: ($m + " summary")}')"
  [ "$STATUS" = 201 ] || {
    unknown "the document could not be written: POST /api/v1/projects/$priv/artifacts answered $(answered)"
    return
  }
  call document-read person GET "$DISPATCH_URL/api/v1/projects/$priv/artifacts/$SLUG"
  [ "$STATUS" = 200 ] && [ "$(field .slug)" = "$SLUG" ] && [ "$(field .project)" = "$priv" ] || {
    unknown "the document could not be read back by its slug: GET /api/v1/projects/$priv/artifacts/$SLUG answered $(answered)"
    return
  }
  note "wrote $issue and the document $SLUG, and read the document back by its slug"

  local comment
  call comment person POST "$DISPATCH_URL/api/v1/issues/$issue/comments" \
    "$(jq -cn --arg m "$MARK" --arg s "$session" '{body: ($m + " comment for the session."), mentions: [{target: ("session:" + $s)}]}')"
  comment="$(field .id)"
  [ "$STATUS" = 201 ] && [ -n "$comment" ] || {
    unknown "the comment could not be written: POST /api/v1/issues/$issue/comments answered $(answered)"
    return
  }
  local ask edited
  call ask person POST "$DISPATCH_URL/api/v1/issues/$issue/asks" \
    "$(jq -cn --arg m "$MARK" '{question: ($m + " question?"), options: [{label: ($m + " yes"), description: ($m + " the first option.")}, {label: ($m + " no"), description: ($m + " the second option.")}]}')"
  ask="$(field .id)"
  edited="$(jq -c '.edited_at' "$ANSWER" 2>/dev/null || echo null)"
  [ "$STATUS" = 201 ] && [ -n "$ask" ] || {
    unknown "the ask could not be written: POST /api/v1/issues/$issue/asks answered $(answered)"
    return
  }
  call answer person POST "$DISPATCH_URL/api/v1/asks/$ask/answer" \
    "$(jq -cn --arg m "$MARK" --argjson edited "$edited" '{selected: [($m + " yes")], text: ($m + " answer."), expected_edited_at: $edited}')"
  succeeded || {
    unknown "the ask's answer could not be written: POST /api/v1/asks/$ask/answer answered $(answered)"
    return
  }
  call message person POST "$DISPATCH_URL/api/v1/agents/$session/messages" \
    "$(jq -cn --arg m "$MARK" '{body: ($m + " direct message."), delivery: "steer"}')"
  succeeded || {
    unknown "the direct message could not be written: POST /api/v1/agents/$session/messages answered $(answered)"
    return
  }
  note "wrote a comment mentioning session $session, an ask, its answer and a direct message to the session"

  call github person GET "$DISPATCH_URL/api/github/rest/$SLUG"
  [ "$STATUS" = 404 ] && [ "$(field .code)" = GITHUB_PATH_REFUSED ] || {
    unknown "the GitHub proxy did not refuse /api/github/rest/$SLUG as a path it does not read: it answered $(answered)"
    return
  }
  note "the GitHub proxy refused /api/github/rest/$SLUG"

  # Sixty seconds, read once a second, for the listener to take the comment: the API sends a
  # mention after its answer, and a send that failed may be sent again by the delivery sweep.
  # sent and queued are both the listener taking the message into the session's own stream
  # (model.DeliveryTaken); queued is what it records here, where no client reads that stream.
  local delivered=false last="" state=""
  for _ in $(seq 1 60); do
    call delivery person GET "$DISPATCH_URL/api/v1/comments/$comment"
    if [ "$STATUS" = 200 ]; then
      last="$(jq -c --arg s "$session" '[.comment.deliveries[]? | select(.session_id == $s or .target == ("session:" + $s))] | last // empty' "$ANSWER" 2>/dev/null || true)"
      state="$(jq -r '.state // empty' <<<"$last" 2>/dev/null || true)"
      if [ "$state" = sent ] || [ "$state" = queued ]; then
        delivered=true
        break
      fi
    else
      last="GET /api/v1/comments/$comment answered $(answered)"
    fi
    sleep 1
  done
  if [ "$delivered" = true ]; then
    note "Dispatch recorded the comment's delivery to session $session $state"
  else
    unknown "the comment's delivery to session $session never came: ${last:-Dispatch recorded no delivery to it}"
  fi
}

if [ "$CONTROL" = true ]; then
  control
fi
drive

# --- The verdict: every line of every file under logs/ that names the marker, each file that
# could not be read, then each service log that did not grow from the first marked write. ---
hits=0 searched=0 listener_named=false
if ! find "$LOG_DIR" -type f -print0 >"$WORK_DIR/log-files" 2>"$WORK_DIR/log-files.err"; then
  unknown "$LOG_DIR could not be listed whole: $(tr '\n' ' ' <"$WORK_DIR/log-files.err")"
fi
while IFS= read -r -d '' file; do
  searched=$((searched + 1))
  rc=0
  grep -naiF -- "$SLUG" "$file" >"$WORK_DIR/hits" 2>"$WORK_DIR/hits.err" || rc=$?
  case "$rc" in
    0)
      [ "$file" != "$LOG_DIR/listener.log" ] || listener_named=true
      while IFS= read -r hit; do
        hits=$((hits + 1))
        text="${hit#*:}"
        echo "FAIL logs/${file#"$LOG_DIR"/}:${hit%%:*}: $MARK: ${text:0:300}"
      done <"$WORK_DIR/hits"
      ;;
    1) ;;
    *) unknown "logs/${file#"$LOG_DIR"/} could not be read: $(tr '\n' ' ' <"$WORK_DIR/hits.err")" ;;
  esac
done < <(sort -z "$WORK_DIR/log-files")
[ "$searched" -gt 0 ] || unknown "no file under $LOG_DIR was read"
for log in "${GROWN_LOGS[@]}"; do
  [ -n "${size_before[$log]:-}" ] || continue
  if [ "$(stat -c %s "$LOG_DIR/$log" 2>/dev/null || echo 0)" -le "${size_before[$log]}" ]; then
    unknown "logs/$log did not grow during the run"
  fi
done
control_seen=true
if [ "$CONTROL" = true ] && [ "$listener_named" = false ]; then
  control_seen=false
  unknown "the control's role claim left no line naming $MARK in logs/listener.log, so this run shows nothing of what the search can see"
fi
if [ "$hits" -gt 0 ] && [ "$control_seen" = true ]; then
  lines=lines
  [ "$hits" -ne 1 ] || lines=line
  echo "private-content-logs-check: failed: $MARK is on $hits $lines under $LOG_DIR"
  exit 1
fi
if [ "$unknowns" -gt 0 ]; then
  echo "private-content-logs-check: UNKNOWN: $unknowns parts of the run could not be done or read, each named above"
  exit 2
fi
echo "private-content-logs-check: passed: no file under $LOG_DIR names $MARK"
