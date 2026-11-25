#!/usr/bin/env bash
# packages/envoy/scripts/approve-check.sh
#
# A person's approvals through Cipher's proxy, checked on the local stack `dev-stack.sh --approve`
# stands up: the stack's broker with its proxy port and the dpop block DISPATCH_APPROVE_TOKEN,
# bound to dispatch.approve.test and sent through BROKER_PROXY_PRIVATE_UPSTREAM to Dispatch built
# from this tree, whose issuer of broker tokens is that broker. A person signs in on a box of the
# run (`agent-secrets login` at the stack's issuer, the machine's identity from the metadata
# stand-in), and the agent session on that box is scripts/approve/session.ts, the TypeScript client
# a session runs; step 1e's scan is scripts/approve/scan.ts, run on the box with the session's
# environment.
#
# Four stacks, one after the other:
#
#   1. --approve. (a) the session trades the sign-in for an approve grant and approves the spec of
#      an issue of the person's through the proxy port (reviewArtifact); the review's author is the
#      person, and the grant's row names the session and the person, the scope approve, no secret,
#      no profile, no approver, and ends 15 minutes after it began. (b) the same review with the
#      box's own Dispatch token, direct, is refused 403. (c) a comment and (d) a personal token's
#      mint and change, each with the approve placeholder through the proxy port, are refused by
#      the proxy naming its rule, and Dispatch records no comment and no token. Controls: (b) the
#      box's token posts a comment, so it is live; (a) with the sign-in's credential revoked, the
#      same review on the same placeholder is refused 407. (e) the scan of every file the session
#      and the sign-in wrote under the box, of the session's stderr and stdout, and of the session's
#      environment, finds no candidate whose SHA-256 is a minted token's the broker logged, and no
#      private key, as PEM or as a JWK, whose thumbprint is a logged binding; a scan that read no
#      file, no file under a path it was named, any one file, directory or process environment it
#      could not read, or a broker log that names no minted token or no binding is UNKNOWN. A
#      symbolic link whose target does not exist holds no data: the scan names it and reads nothing.
#   2. --approve --approve-allow-all: the allow list off. (i) the comment and the token mint reach
#      Dispatch, which refuses them APPROVE_ONLY_TOKEN and records no comment; (ii) the review is
#      recorded with the person as its author.
#   3. as 2, with Dispatch built with the approve-only refusal removed
#      (approve/dispatch-without-approve-only-refusal.patch): the comment of (i) is recorded.
#   4. --approve with a broker built to write each approve token it mints into files the session
#      reads, one form a file - on a line of its own, in a token endpoint's answer naming it, and as
#      DISPATCH_APPROVE_TOKEN=<token> - and its pod key as PEM
#      (approve/broker-plants-approve-tokens.patch), and a token lifetime of 60 seconds: of three
#      approve tokens, one expired and one revoked at the broker before the scans, the scan of each
#      form finds all three by hash, and the scan of the first the pod key by its thumbprint; the
#      three tokens written to the session's stderr and stdout are found by a scan of step 1e's own
#      paths, so 1e reads what the session prints. The DPoP diagnostic's answers are printed and
#      decide nothing.
#
# It prints one line per step, "approve-check: <step>: PASS|FAIL|UNKNOWN <what it read>", and
# exits 0 when every step passed, 1 when one failed, 2 on a usage error, and 3 when none failed and
# one is UNKNOWN: a stack that did not start, a sign-in or a setup that did not complete, or a scan
# that compared nothing or could not read a file, a directory or a process environment it was
# given. The Postgres server is LEGION_TEST_PG_DSN, else DATABASE_URL, as dev-stack.sh reads them.
# It needs what dev-stack.sh needs, and bun, openssl and patch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$ENVOY_DIR/../.." && pwd)"
if [ -z "${LEGION_TEST_PG_DSN:-}${DATABASE_URL:-}" ]; then
  echo "approve-check: set LEGION_TEST_PG_DSN or DATABASE_URL to a Postgres server dev-stack.sh may create databases on" >&2
  exit 2
fi
for tool in bun go python3 psql curl jq openssl patch setsid; do
  command -v "$tool" >/dev/null || {
    echo "approve-check: $tool is required on PATH" >&2
    exit 2
  }
done
# The real binaries, ahead of any version manager's shims: the stack starts its processes with a
# HOME of their own, where a shim reads no configuration of this user's.
PYTHON_DIR="$(dirname "$(python3 -c 'import sys; print(sys.executable)')")"
BUN_BIN="$(bun -e 'process.stdout.write(process.execPath)')"
GO_DIR="$(go env GOROOT)/bin"
PATH="$GO_DIR:$PYTHON_DIR:$(dirname "$BUN_BIN"):$PATH"
export PATH

WORK="$(mktemp -d "${TMPDIR:-/tmp}/approve-check.XXXXXX")"
FAILED=0
UNKNOWN=0
STACK_PID=""
METADATA_PID=""
# stop_stack stops the metadata stand-in and the stack this run started, where either runs: after
# each stack, and from the EXIT trap below, as every early stop and finish exit.
stop_stack() {
  [ -z "$METADATA_PID" ] || kill "$METADATA_PID" 2>/dev/null || true
  METADATA_PID=""
  if [ -n "$STACK_PID" ]; then
    kill -TERM "$STACK_PID" 2>/dev/null || true
    wait "$STACK_PID" 2>/dev/null || true
  fi
  STACK_PID=""
}
trap stop_stack EXIT
echo "approve-check: work directory $WORK" >&2

# result, stop_unknown and finish: how each step is recorded and how the run ends
# (approve-check.test.sh drives them).
# shellcheck source=lib/approve-check-steps.sh
. "$SCRIPT_DIR/lib/approve-check-steps.sh"

# --- The two control builds, from this tree with one patch each, through go build's -overlay so
# the tree is never written. ---
build_with_patch() { # <patch> <out> <package>
  local patch="$1" out="$2" package="$3" scratch overlay file
  scratch="$(mktemp -d "$WORK/overlay.XXXXXX")"
  overlay="$scratch/overlay.json"
  local replace="{}"
  while IFS= read -r file; do
    mkdir -p "$scratch/tree/$(dirname "$file")"
    cp "$REPO_ROOT/$file" "$scratch/tree/$file"
    replace="$(jq -c --arg from "$REPO_ROOT/$file" --arg to "$scratch/tree/$file" '. + {($from): $to}' <<<"$replace")"
  done < <(sed -n 's|^+++ b/\([^[:space:]]*\).*|\1|p' "$patch")
  patch -s -p1 -d "$scratch/tree" <"$patch"
  jq -n --argjson replace "$replace" '{Replace: $replace}' >"$overlay"
  (cd "$ENVOY_DIR" && go build -overlay "$overlay" -o "$out" "$package")
}
echo "approve-check: building the two control builds..." >&2
build_with_patch "$SCRIPT_DIR/approve/dispatch-without-approve-only-refusal.patch" "$WORK/control/dispatch" ./cmd/dispatch
build_with_patch "$SCRIPT_DIR/approve/broker-plants-approve-tokens.patch" "$WORK/control/broker" ./cmd/cipherd

# --- One stack: started in a session of its own, its banner's exports read into this shell. ---
start_stack() { # <name> <dev-stack.sh options>...
  local name="$1"
  shift
  STACK_DIR="$WORK/$name"
  mkdir -p "$STACK_DIR"
  setsid bash "$SCRIPT_DIR/dev-stack.sh" "$@" >"$STACK_DIR/stack.log" 2>&1 &
  STACK_PID=$!
  local waited=0
  until grep -q '^dev-stack: ready\.$' "$STACK_DIR/stack.log" 2>/dev/null; do
    if ! kill -0 "$STACK_PID" 2>/dev/null || [ "$waited" -ge 1800 ]; then
      stop_unknown "$name stack" "dev-stack.sh $* did not start; see $STACK_DIR/stack.log"
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  sed -n 's/^  export \([A-Z_0-9]*\)=\(.*\)$/\1=\2/p' "$STACK_DIR/stack.log" | grep -v '^PATH=' >"$STACK_DIR/exports"
  while IFS='=' read -r variable value; do
    printf -v "$variable" '%s' "$value"
  done <"$STACK_DIR/exports"
  BIN_DIR="$DEV_STACK_DIR/bin"
  BROKER_LOG="$DEV_STACK_DIR/logs/broker.log"
  # The two databases the stack created, by the names its banner prints.
  local admin="${LEGION_TEST_PG_DSN:-${DATABASE_URL:-}}" database
  for database in broker dispatch; do
    local database_name url
    database_name="$(sed -n "s/^  databases:  .*\(dev_stack_${database}_[a-z0-9]*\).*/\1/p" "$STACK_DIR/stack.log" | head -n1)"
    url="${admin%%\?*}"
    url="${url%/*}/$database_name"
    [[ "$admin" != *\?* ]] || url="$url?${admin#*\?}"
    if [ "$database" = broker ]; then BROKER_DB_URL="$url"; else DISPATCH_DB_URL="$url"; fi
  done
  echo "approve-check: $name stack up: $DEV_STACK_DIR" >&2
}

# --- The person's box on the stack: its machine's metadata stand-in, and the person's sign-in. ---
PERSON="ada@example.com"
sign_in_box() {
  BOX="$STACK_DIR/box"
  mkdir -p "$BOX/home" "$BOX/keys" "$BOX/signins" "$BOX/session"
  chmod 700 "$BOX" "$BOX"/*
  env -i PATH="$PATH" HOME="$BOX/home" DEV_STACK_METADATA_PORT_FILE="$BOX/metadata-port" \
    python3 "$DEV_STACK_METADATA_SCRIPT" >"$STACK_DIR/metadata.log" 2>&1 &
  METADATA_PID=$!
  local waited=0
  until [ -s "$BOX/metadata-port" ] || [ "$waited" -ge 120 ]; do
    sleep 0.5
    waited=$((waited + 1))
  done
  METADATA_HOST="127.0.0.1:$(tr -dc '0-9' <"$BOX/metadata-port")"
  # BOX_ENV: the variables every process on the box has, which agent-secrets login and each run of
  # run_on_box start under alone (env -i): the box's HOME, the broker, the machine identity of the
  # metadata stand-in, and the sign-in's issuer, client and directories, with no Dispatch bearer.
  BOX_ENV=(
    "PATH=$BIN_DIR:$PATH" "HOME=$BOX/home" "AGENT_SECRETS_URL=$AGENT_SECRETS_URL"
    "AGENT_SECRETS_MACHINE_IDENTITY=metadata" "AGENT_SECRETS_MACHINE_AUDIENCE=dev-stack-machine"
    "GCE_METADATA_HOST=$METADATA_HOST"
    "AGENT_SECRETS_SIGNIN_ISSUER=$AGENT_SECRETS_SIGNIN_ISSUER" "AGENT_SECRETS_SIGNIN_CLIENT_ID=$AGENT_SECRETS_SIGNIN_CLIENT_ID"
    "AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE=$AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"
    "AGENT_SECRETS_SIGNIN_DIR=$BOX/signins" "AGENT_SECRETS_KEY_DIR=$BOX/keys" "AGENT_SECRETS_HELPER_SOCK="
  )
  : >"$BOX/login.out"
  : >"$BOX/login.err"
  env -i "${BOX_ENV[@]}" agent-secrets login >"$BOX/login.out" 2>"$BOX/login.err" &
  local login=$! code=""
  for _ in $(seq 240); do
    code="$(sed -n 's/.*type the code \([^ ]*\).*/\1/p' "$BOX/login.err" | head -n1)"
    [ -z "$code" ] || break
    kill -0 "$login" 2>/dev/null || break
    sleep 0.5
  done
  if [ -z "$code" ]; then
    stop_unknown "sign-in" "agent-secrets login printed no code: $(tr '\n' ' ' <"$BOX/login.err")"
  fi
  curl -sf -X POST "$DEV_STACK_ISSUER_URL/_dev/sign-in-as" -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg p "$PERSON" '{sub: $p, email: $p, email_verified: true, hd: ($p | split("@")[1])}')" >/dev/null
  curl -sf -X POST "$DEV_STACK_ISSUER_URL/_dev/approve-device-code" -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg c "$code" '{user_code: $c}')" >/dev/null
  wait "$login" || true
  if ! grep -qF -- "$PERSON is signed in" "$BOX/login.out"; then
    stop_unknown "sign-in" "$PERSON's agent-secrets login did not sign in: $(tr '\n' ' ' <"$BOX/login.err")"
  fi
  # The person's own sign-in to Dispatch, which a project's default assignee needs: the stack's
  # Dispatch serves no sign-in route under its https origin (dev sign-in takes a loopback origin
  # alone), so the row a browser sign-in writes is written here.
  psql "$DISPATCH_DB_URL" -v ON_ERROR_STOP=1 -Atqc "insert into people (email) values ('$PERSON') on conflict do nothing" >/dev/null
}

# run_on_box <file> <args>...: scripts/approve/<file> run with bun on the box, under BOX_ENV and the
# variables a session there has besides, and no Dispatch bearer, so its credential is the broker
# credential of the person's sign-in. Each step of the session (session.ts) runs this way, and so
# does step 1e's scan (scan.ts), which on the local stack reads its own environment as the
# session's. Its stderr goes to $STACK_DIR/session.err, and its stdout, which holds every answer it
# printed, is kept in $STACK_DIR/session.out as well as handed back: step 1e scans both
# (session_scan_paths), since a token the session printed is the likeliest leak on a box.
run_on_box() {
  local file="$1"
  shift
  env -i "${BOX_ENV[@]}" TMPDIR="$BOX/session" \
    DISPATCH_URL="$DEV_STACK_DISPATCH_SERVER_URL" APPROVE_CHECK_DISPATCH_LOCAL="$DISPATCH_URL" \
    APPROVE_CHECK_STATE="$STACK_DIR/state.json" APPROVE_CHECK_REVOKE_COMMAND="${REVOKE_COMMAND:-}" \
    "$BUN_BIN" "$SCRIPT_DIR/approve/$file" "$@" 2>>"$STACK_DIR/session.err" | tee -a "$STACK_DIR/session.out"
}

# session <step> [args]...: one step of the agent session on the box.
session() { run_on_box session.ts "$@"; }

# session_scan_paths prints the paths step 1e scans, one a line: every file the session and the
# sign-in wrote under the box, and the session's stderr and stdout.
session_scan_paths() { printf '%s\n' "$BOX" "$STACK_DIR/session.err" "$STACK_DIR/session.out"; }

broker_sql() { psql "$BROKER_DB_URL" -v ON_ERROR_STOP=1 -Atqc "$1"; }
dispatch_sql() { psql "$DISPATCH_DB_URL" -v ON_ERROR_STOP=1 -Atqc "$1"; }

# body_count <member>: how many items the body of the session's answer on stdin holds: the body
# itself where it is an array, else its <member> array.
body_count() { jq --arg member "$1" '[.body | if type == "array" then .[] else (.[$member] // [])[] end] | length'; }

# run_scan <paths...>: the scan of the paths against the broker's log, on the box. It sets scan to
# the scan's one JSON line, or {} where it printed none, and scan_exit to its exit status, which
# set -e must not end the run on: 3 is the scan's UNKNOWN, a step result like any other.
run_scan() {
  scan_exit=0
  scan="$(run_on_box scan.ts "$BROKER_LOG" "$@")" || scan_exit=$?
  jq -e . >/dev/null 2>&1 <<<"$scan" || scan="{}"
}

# setup_issue: the person's project, issue and spec on this stack (the session's setup), or the
# run ends UNKNOWN naming why, since no step after it could read anything.
setup_issue() {
  local status=0 setup
  setup="$(session setup)" || status=$?
  if [ "$status" != 0 ]; then
    stop_unknown "setup" "the session's setup exited $status: $(tail -n 3 "$STACK_DIR/session.err" | tr '\n' ' ')"
  fi
  echo "approve-check: $setup" >&2
}

# --- Stack 1: the approve route as it ships. ---
start_stack approve --approve
sign_in_box
setup_issue
# The Idempotency-Key the review carries must reach Dispatch through the proxy: Dispatch keeps one
# request_keys row per keyed write, so the reviews route holds no answered key before the review
# and exactly one after it.
review_keys() {
  dispatch_sql "select count(*) from request_keys where method = 'POST' and path like '/api/v1/artifacts/%/reviews' and status between 200 and 299"
}
keys_before="$(review_keys)"
approved="$(session approve)"
keys_after="$(review_keys)"
reviews="$(session reviews)"
author="$(jq -r '[.body | if type == "array" then .[] else (.reviews // [])[] end | (.actor.person // .actor.email // .actor // empty)] | map(tostring) | join(",")' <<<"$reviews")"
# The grant's lifetime in whole seconds: created_at is the transaction's start and expires_at Go's
# clock fifteen minutes on, a fraction of a second later, so the floor reads 900.
grant="$(broker_sql "select session, extra->>'person', array_to_string(scopes, ','), floor(extract(epoch from expires_at - created_at))::int from approve_grants order by created_at desc limit 1")"
columns="$(broker_sql "select string_agg(column_name, ',' order by ordinal_position) from information_schema.columns where table_name = 'approve_grants'")"
if [ "$(jq -r .recorded <<<"$approved")" = true ] && [[ "$author" == *"$PERSON"* ]] &&
  [[ "$grant" == *"|$PERSON|approve|900" ]] && [[ "$columns" != *secret* ]] && [[ "$columns" != *profile* ]] && [[ "$columns" != *approver* ]] &&
  [ "$keys_before" = 0 ] && [ "$keys_after" = 1 ]; then
  result "1a review through the proxy" PASS "review $(jq -c '{recorded, state, version, actor}' <<<"$approved"), reviews' authors [$author], grant session|person|scopes|seconds = $grant, its columns $columns; Dispatch's answered keys on the reviews route $keys_before before the review and $keys_after after, so its Idempotency-Key reached Dispatch through the proxy"
else
  result "1a review through the proxy" FAIL "review $approved; reviews $reviews; grant $grant; columns $columns; answered review keys $keys_before before, $keys_after after"
fi
state="$(cat "$STACK_DIR/state.json")"
artifact="$(jq -r .artifact <<<"$state")"
version="$(jq -r .version <<<"$state")"
issue="$(jq -r .issue <<<"$state")"
# The control of 1a's key check: the same review sent through the proxy with no Idempotency-Key
# (the session's `proxy` command sends none) reaches Dispatch, which runs it unkeyed, and the
# reviews route still holds one answered key row, 1a's: the count reads 1 only for a review whose
# key reached Dispatch.
keyless="$(session proxy POST "/api/v1/artifacts/$artifact/versions/$version/reviews" '{"state":"approved"}')"
keys_keyless="$(review_keys)"
if [[ "$(jq -r .status <<<"$keyless")" == 2?? ]] && [ "$keys_keyless" = 1 ]; then
  result "1a control: a review with no key leaves no key row" PASS "the keyless review through the proxy answered $(jq -r .status <<<"$keyless") and the reviews route still holds $keys_keyless answered key row, 1a's"
else
  result "1a control: a review with no key leaves no key row" FAIL "the keyless review answered $(jq -c 'del(.body)' <<<"$keyless"); answered key rows $keys_keyless, want 1"
fi
box_review="$(session box POST "/api/v1/artifacts/$artifact/versions/$version/reviews" '{"state":"approved"}')"
if [ "$(jq -r .status <<<"$box_review")" = 403 ]; then
  result "1b the box's token reviews" PASS "403 $(jq -r .code <<<"$box_review")"
else
  result "1b the box's token reviews" FAIL "$(jq -c 'del(.body)' <<<"$box_review")"
fi
comment="$(session proxy POST "/api/v1/issues/$issue/comments" '{"body":"A comment the approve placeholder must not write."}')"
mint="$(session proxy POST /api/v1/me/agent-tokens '{"name":"from-the-proxy","scopes":["person","approve"]}')"
change="$(session proxy PATCH /api/v1/me/agent-tokens/5f070414-1a7c-43a3-8818-cf76eded9806 '{"scopes":["person","approve"]}')"
comments="$(session comments | body_count comments)"
tokens="$(session agent-tokens | body_count tokens)"
for step in "1c comment:$comment" "1d token mint:$mint" "1d token change:$change"; do
  name="${step%%:*}"
  answer="${step#*:}"
  if [ "$(jq -r .status <<<"$answer")" = 403 ] && [ "$(jq -r .proxy_code <<<"$answer")" != null ] && [ "$(jq -r .forwarded <<<"$answer")" != true ]; then
    result "$name through the proxy" PASS "403 $(jq -r .proxy_code <<<"$answer"): $(jq -r .error <<<"$answer")"
  else
    result "$name through the proxy" FAIL "$(jq -c 'del(.body)' <<<"$answer")"
  fi
done
if [ "$comments" = 0 ] && [ "$tokens" = 0 ]; then
  result "1c,1d nothing written" PASS "comments on $issue: $comments; the person's agent tokens: $tokens"
else
  result "1c,1d nothing written" FAIL "comments on $issue: $comments; agent tokens: $tokens"
fi
box_comment="$(session box POST "/api/v1/issues/$issue/comments" '{"body":"The box token is live."}')"
if [[ "$(jq -r .status <<<"$box_comment")" == 2?? ]]; then
  result "1b control: the box's token comments" PASS "$(jq -r .status <<<"$box_comment"), so the token is live and its 403 on the review comes from its scope"
else
  result "1b control: the box's token comments" FAIL "$(jq -c 'del(.body)' <<<"$box_comment")"
fi
# The revoke control runs before the scan, so the revoked grant's minted token is among the hashes
# the scan compares with. (A diagnostic that presented arbitrary candidates once spent Dispatch's
# broker-token rate limit for loopback, which the proxy shares; it now presents minted tokens
# alone.)
REVOKE_COMMAND="psql '$BROKER_DB_URL' -Atqc \"update launcher_credentials set revoked_at = now() where id = (select credential_id from approve_grants order by created_at desc limit 1)\""
revoked="$(session approve-then-revoked)"
if [[ "$(jq -r .first.status <<<"$revoked")" == 2?? ]] && [ "$(jq -r .second.status <<<"$revoked")" = 407 ]; then
  result "1a control: the grant revoked" PASS "the review $(jq -r .first.status <<<"$revoked"), then on the same placeholder after the sign-in's credential was revoked 407 $(jq -r .second.proxy_code <<<"$revoked")"
else
  result "1a control: the grant revoked" FAIL "$(jq -c 'del(.second.body)' <<<"$revoked")"
fi
REVOKE_COMMAND=""
mapfile -t scan_paths < <(session_scan_paths)
run_scan "${scan_paths[@]}"
if [ "$(jq '(.hash_matches // []) + (.key_matches // []) | length' <<<"$scan")" != 0 ]; then
  result "1e scan" FAIL "$(jq -c . <<<"$scan")"
elif [ "$scan_exit" = 0 ] && [ "$(jq -r .result <<<"$scan")" = clean ]; then
  result "1e scan" PASS "$(jq -c '{result, files_read, files_skipped, dangling_links, candidates, base64url, token68, dpop_answers, private_keys, logged_hashes, logged_bindings, hash_matches, key_matches, diagnostic, diagnostic_presented, environment_variables: (.environment_names | length), environment_matches}' <<<"$scan")"
elif [ "$scan_exit" = 3 ]; then
  result "1e scan" UNKNOWN "$(jq -c '{unknown, files_read, files_skipped, dangling_links, logged_hashes, logged_bindings}' <<<"$scan")"
else
  result "1e scan" FAIL "the scan exited $scan_exit with $(jq -c . <<<"$scan"): $(tail -n 3 "$STACK_DIR/session.err" | tr '\n' ' ')"
fi
stop_stack

# --- Stack 2: the allow list off. ---
start_stack allow-all --approve --approve-allow-all
sign_in_box
setup_issue
issue="$(jq -r .issue <"$STACK_DIR/state.json")"
comment="$(session proxy POST "/api/v1/issues/$issue/comments" '{"body":"A comment the approve-only token must not write."}')"
mint="$(session proxy POST /api/v1/me/agent-tokens '{"name":"from-the-proxy","scopes":["person","approve"]}')"
comments="$(session comments | body_count comments)"
approved="$(session approve)"
if [ "$(jq -r .status <<<"$comment")" = 403 ] && [ "$(jq -r .code <<<"$comment")" = APPROVE_ONLY_TOKEN ] && [ "$comments" = 0 ]; then
  result "2i comment with the allow list off" PASS "Dispatch answered 403 APPROVE_ONLY_TOKEN (forwarded $(jq -r .forwarded <<<"$comment")), comments on $issue: $comments"
else
  result "2i comment with the allow list off" FAIL "$(jq -c 'del(.body)' <<<"$comment"); comments $comments"
fi
if [ "$(jq -r .status <<<"$mint")" = 403 ] && [ "$(jq -r .code <<<"$mint")" = APPROVE_ONLY_TOKEN ]; then
  result "1c,1d control: the refusals come from the allow list" PASS "with it off the comment and the token mint reach Dispatch (its 403 APPROVE_ONLY_TOKEN)"
else
  result "1c,1d control: the refusals come from the allow list" FAIL "$(jq -c 'del(.body)' <<<"$mint")"
fi
if [ "$(jq -r .recorded <<<"$approved")" = true ]; then
  result "2ii review with the allow list off" PASS "$(jq -c '{recorded, state, version, actor}' <<<"$approved")"
else
  result "2ii review with the allow list off" FAIL "$approved"
fi
stop_stack

# --- Stack 3: Dispatch without its approve-only refusal. ---
start_stack no-refusal --approve --approve-allow-all --dispatch-bin "$WORK/control/dispatch"
sign_in_box
setup_issue
issue="$(jq -r .issue <"$STACK_DIR/state.json")"
comment="$(session proxy POST "/api/v1/issues/$issue/comments" '{"body":"Recorded only without the approve-only refusal."}')"
comments="$(session comments | body_count comments)"
if [[ "$(jq -r .status <<<"$comment")" == 2?? ]] && [ "$comments" = 1 ]; then
  result "2 control: Dispatch without the approve-only refusal" PASS "the comment of (i) was recorded ($(jq -r .status <<<"$comment"), comments on $issue: $comments), so the refusal in 2i comes from that check"
else
  result "2 control: Dispatch without the approve-only refusal" FAIL "$(jq -c 'del(.body)' <<<"$comment"); comments $comments"
fi
stop_stack

# --- Stack 4: a broker that plants what the scan looks for. ---
BROKER_ACCESS_TOKEN_SECONDS=60 start_stack planted --approve --broker-bin "$WORK/control/broker"
sign_in_box
setup_issue
session approve >/dev/null
# The control build writes each token it mints into three files, one form a file.
planted="$DEV_STACK_DIR/planted-approve-tokens"
answers="$DEV_STACK_DIR/planted-approve-answers"
variables="$DEV_STACK_DIR/planted-approve-env"
expired="$(sed -n 1p "$planted" 2>/dev/null || true)"
echo "approve-check: waiting 65 s for the first approve token to expire..." >&2
sleep 65
session approve >/dev/null
session approve >/dev/null
live="$(sed -n 2p "$planted" 2>/dev/null || true)"
revoked_token="$(sed -n 3p "$planted" 2>/dev/null || true)"
revoke_status="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$AGENT_SECRETS_URL/v1/oauth/revoke" \
  -H @<(printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")") \
  --data-urlencode "token@"<(printf '%s' "$revoked_token") --data-urlencode client_id=dispatch-token-exchange)"
# The broker's own answer for each planted token, at its introspection route as the caller
# "dispatch": Dispatch caches a live answer it read, so its diagnostic of a token it saw live a
# moment ago can still read live; the broker's is the one that says whether the token is.
introspected() { # <token>
  curl -s -X POST "$AGENT_SECRETS_URL/v1/oauth/introspect" \
    -H @<(printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")") \
    --data-urlencode "token@"<(printf '%s' "$1") | jq -r '.active'
}
active="expired $(introspected "$expired"), revoked $(introspected "$revoked_token"), live $(introspected "$live")"
want=()
for token in "$expired" "$live" "$revoked_token"; do
  [ -z "$token" ] || want+=("$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)")
done
tokens_ok=false
if [ "${#want[@]}" = 3 ] && [ "$revoke_status" = 200 ] && [ "$active" = "expired false, revoked false, live true" ]; then
  tokens_ok=true
fi
# planted_control <step> <what the form is> <keys wanted> <paths...>: the scan of the paths must
# end found, exit 0, with every planted token's hash, and with at least <keys wanted> planted keys.
planted_control() {
  local step="$1" form="$2" keys_wanted="$3" found=0 hash
  shift 3
  run_scan "$@"
  for hash in "${want[@]}"; do
    jq -e --arg h "$hash" 'any((.hash_matches // [])[]; .sha256 == $h)' <<<"$scan" >/dev/null && found=$((found + 1))
  done
  local keys
  keys="$(jq '(.key_matches // []) | length' <<<"$scan")"
  if [ "$tokens_ok" = true ] && [ "$scan_exit" = 0 ] && [ "$found" = 3 ] && [ "$keys" -ge "$keys_wanted" ]; then
    result "$step" PASS "the scan of $form found 3 of 3 planted approve tokens by hash (one expired, one revoked at the broker: $revoke_status, one live; the broker introspects them as $active) and $keys planted binding key(s) by thumbprint; Dispatch's diagnostic of the matches, the only values it is sent, which decides nothing: $(jq -c .diagnostic_of_matches <<<"$scan"), $(jq -c '{diagnostic_presented}' <<<"$scan")"
  else
    result "$step" FAIL "the scan of $form exited $scan_exit and found $found of ${#want[@]} planted tokens and $keys key(s); revoke $revoke_status, broker introspection $active; $(jq -c '{result, unknown, files_read, logged_hashes, hash_matches, key_matches}' <<<"$scan")"
  fi
}
planted_control "1e control: planted tokens on lines of their own, and the binding key" "each token on a line of its own" 1 \
  "$BOX" "$planted" "$DEV_STACK_DIR/planted-binding-key.pem"
planted_control "1e control: planted tokens in token endpoint answers" "token endpoint answers naming each token" 0 "$answers"
planted_control "1e control: planted tokens as NAME=value" "DISPATCH_APPROVE_TOKEN=<token> lines" 0 "$variables"
# What the session prints: one token on its stderr and two on its stdout, the way a session that
# printed a token would leave it, scanned through step 1e's own paths.
printf 'approve-session: %s\n' "$expired" >>"$STACK_DIR/session.err"
printf '{"step":"proxy","body":{"access_token":"%s"}}\nDPoP %s\n' "$live" "$revoked_token" >>"$STACK_DIR/session.out"
mapfile -t scan_paths < <(session_scan_paths)
planted_control "1e control: planted tokens in what the session printed" "the session's stderr and stdout, through step 1e's paths" 0 \
  "${scan_paths[@]}"
stop_stack

finish
