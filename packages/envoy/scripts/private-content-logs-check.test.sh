#!/usr/bin/env bash
# packages/envoy/scripts/private-content-logs-check.test.sh
#
# Proves private-content-logs-check.sh's verdicts without a stack. An exported callout stack
# (DEV_STACK_CALLOUT=1) names a work directory of this test's own, whose logs/ holds the three
# service logs and whose fake-secrets.json is the stack's secret store, and a fake curl on PATH
# answers every request the check makes as Dispatch, the listener and the broker answer it. A fake
# sleep returns at once, so the check's wait for a delivery that never comes costs nothing.
#
# The fakes write their logs as the services do: Dispatch one request line per request; the
# listener a line per registration and per role claim, and a send line when Dispatch hands it the
# comment's mention or the direct message for a session that registered the delivery mode steer
# (Dispatch records a delivery to a session that did not as failed, "does not advertise steer");
# the broker nothing for a reread it took, and one refusal line for a reread under a proof no
# session signed. The listener logs a soft role claim that names a live predecessor with its role,
# as the real listener does when one side carries no client instance id (cmd/listener/api.go),
# which is the claim --control makes. Files in a control directory change that:
#   echo-dispatch       Dispatch's line holds the request's path, as the request line at the merge
#                       base writes it: a log with the marker, in the lower case a slug carries;
#   echo-body           Dispatch's line holds the request's body, as an error string that quotes a
#                       caller's text would: a log with the marker in its upper case alone;
#   echo-binary         Dispatch also writes the request's path to logs/natsd.log after a NUL byte:
#                       a log none of the three service logs, which grep would read as binary;
#   silent-listener     the listener writes nothing: a log that did not grow;
#   quiet-sends         the listener logs registrations and role claims, and no send: a log that
#                       grew only with the set-up before the first marked write;
#   drop-role           the listener logs a role claim without its role;
#   delivery-failed     the comment's delivery to PRIV's session is recorded failed;
#   delivery-queued     it is recorded queued: taken into the session's own stream, which no client
#                       of the stack reads, as the real stack records it;
#   switch-refused      PATCH /api/v1/projects/PRIV/access answers 404, as a Dispatch without the
#                       private switch does;
#   switch-noop         it answers 200 and leaves PRIV public;
#   unreadable-log      logs/natsd.log is there and cannot be read.
#
# It asserts that: (a) clean logs pass, exit 0, after every write, the private switch first and
# every content write after it, the secret in the store tagged PRIV with the marker as its value,
# and PRIV's session registered with a consumer and steer; (b) a log that holds the marker FAILs,
# exit 1, naming the file, the line and the marker, whether the line holds it in a slug's lower case
# or in the marker's upper case, and in a file beside the three service logs that holds a NUL byte;
# (c) a log that did not grow is UNKNOWN, exit 2, naming it, and so is a log that cannot be read;
# (d) a delivery that failed is UNKNOWN, exit 2, naming the session, and one queued passes; (e)
# --control FAILs, exit 1, naming listener.log, and a control run whose listener.log does not name
# the marker is UNKNOWN, exit 2, though another log does; (f) an exported stack missing a variable
# is refused, exit 2, naming it, before any request; (g) a private switch refused, or answered while
# PRIV stays public, is UNKNOWN, exit 2, and no content is written after it; and (h) a stack whose
# banner has no PRIV pod is UNKNOWN, exit 2, naming the variable, while --control on the same stack
# still FAILs naming listener.log.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly check="${project_root}/packages/envoy/scripts/private-content-logs-check.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/private-content-logs-check-test.XXXXXX")"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly control_dir="${temporary_dir}/control"
readonly stack_dir="${temporary_dir}/stack"
mkdir -p "$fake_bin_dir" "$control_dir"
trap 'chmod -R u+rwX "$temporary_dir" 2>/dev/null; rm -rf "$temporary_dir"' EXIT

readonly priv_session="legion-priv-worker-session"
readonly priv_owner="carol@example.com"
# What the check sends the fake listener as its bearer: a word of this test's own, passed by name so
# no line of this file reads as a credential.
readonly listener_word="stand-in"
printf '%s' "$stack_dir" >"${control_dir}/stack-dir"
printf '%s' "$priv_session" >"${control_dir}/priv-session"

# fake writes the fake command $1 from the body on stdin, after the header every fake shares: the
# control directory it reads.
fake() {
  { printf '#!/usr/bin/env bash\ncontrol=%q\n' "$control_dir" && cat; } >"${fake_bin_dir}/$1"
  chmod +x "${fake_bin_dir}/$1"
}

fake sleep <<'EOF'
exit 0
EOF

# --- Fake curl: the three services. It reads the flags the check sends (-sS, --max-time, -o, -w,
# -X, -H, -b, -c, --data-binary and the URL) and refuses any other, records each request as
# "<method> <url>" in requests, writes the answer's body to the -o file and prints the status for
# -w '%{http_code}'. ---
fake curl <<'EOF'
out=/dev/null format="" method=GET jar_out="" data="" url="" proof=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    -sS) shift ;;
    --max-time) shift 2 ;;
    -o) out="$2"; shift 2 ;;
    -w) format="$2"; shift 2 ;;
    -X) method="$2"; shift 2 ;;
    -H) case "$2" in Proof:*) proof=true ;; esac; shift 2 ;;
    -b) shift 2 ;;
    -c) jar_out="$2"; shift 2 ;;
    --data-binary) data="$2"; shift 2 ;;
    -*) echo "fake curl: a flag the check does not send: $1" >&2; exit 2 ;;
    *) url="$1"; shift ;;
  esac
done
body=""
[ -z "$data" ] || body="$(cat "${data#@}")"
printf '%s %s\n' "$method" "$url" >>"$control/requests"
logs="$(cat "$control/stack-dir")/logs"
session="$(cat "$control/priv-session")"
path="${url#http://*/}"
path="/${path%%\?*}"
status=200 answer='{}'
listener_line() { [ -e "$control/silent-listener" ] || printf '%s\n' "$1" >>"$logs/listener.log"; }
# send hands the session one message as Dispatch does, which the listener takes, and logs, only
# for a session that registered steer.
send() {
  if grep -qxF steer "$control/capabilities" 2>/dev/null && [ ! -e "$control/quiet-sends" ]; then
    listener_line '{"level":"INFO","msg":"listener send","target_session":"'"$session"'","outcome":"accepted"}'
  fi
}
case "$url" in
  http://dispatch.test/*)
    case "$method $path" in
      "GET /auth/_dev/signin") status=302; [ -z "$jar_out" ] || printf '127.0.0.1\tFALSE\t/\tFALSE\t0\tdispatch_session\tfake\n' >"$jar_out" ;;
      "POST /api/v1/projects") status=201; answer='{"key": "PRIV"}' ;;
      "PATCH /api/v1/projects/PRIV/access")
        answer='{"visibility": "private"}'
        [ -e "$control/switch-noop" ] && answer='{"visibility": "public"}'
        if [ -e "$control/switch-refused" ]; then status=404; answer='{"code": "NOT_FOUND", "error": "no route"}'; fi ;;
      "PUT /api/v1/projects/PRIV/access/people/"*) ;;
      "POST /api/v1/issues") status=201; answer='{"key": "PRIV-1"}' ;;
      "POST /api/v1/projects/PRIV/artifacts") status=201; answer='{"artifact": {"id": "d1"}}' ;;
      "GET /api/v1/projects/PRIV/artifacts/"*) answer="{\"slug\": \"${path##*/}\", \"project\": \"PRIV\"}" ;;
      "POST /api/v1/issues/PRIV-1/comments") status=201; answer='{"id": "c1"}'; send ;;
      "POST /api/v1/issues/PRIV-1/asks") status=201; answer='{"id": "a1", "edited_at": null}' ;;
      "POST /api/v1/asks/a1/answer") ;;
      "POST /api/v1/agents/$session/messages") status=201; answer='{"id": "m1"}'; send ;;
      "GET /api/github/rest/"*) status=404; answer='{"code": "GITHUB_PATH_REFUSED", "error": "refused"}' ;;
      "GET /api/v1/comments/c1")
        state=sent error=""
        grep -qxF steer "$control/capabilities" 2>/dev/null || { state=failed; error="session $session () does not advertise steer"; }
        [ -e "$control/delivery-failed" ] && state=failed
        [ -e "$control/delivery-queued" ] && state=queued
        answer="$(jq -cn --arg s "$session" --arg state "$state" --arg error "$error" \
          '{comment: {id: "c1", deliveries: [{target: ("session:" + $s), session_id: $s, attempt: 1, state: $state, error: $error}]}}')" ;;
      *) status=404; answer='{"code": "NOT_FOUND"}' ;;
    esac
    if [ -e "$control/echo-dispatch" ]; then
      printf 'level=INFO msg="dispatch: request" method=%s path=%s status=%s\n' "$method" "$path" "$status" >>"$logs/dispatch.log"
    elif [ -e "$control/echo-body" ] && [ -n "$body" ]; then
      printf 'level=WARN msg="dispatch: write failed" method=%s error=%s\n' "$method" "$(printf '%s' "$body" | jq -c .)" >>"$logs/dispatch.log"
    else
      printf 'level=INFO msg="dispatch: request" method=%s status=%s\n' "$method" "$status" >>"$logs/dispatch.log"
    fi
    if [ -e "$control/echo-binary" ]; then
      printf 'nats: \0 %s %s\n' "$method" "$path" >>"$logs/natsd.log"
    fi ;;
  http://listener.test/*)
    case "$path" in
      # The listener refuses a registration that asks for no session consumer (subscribeHandler).
      /v1/interests/subscribe)
        if [ "$(printf '%s' "$body" | jq -r '.delivery.consumer == true')" != true ]; then
          status=409 answer='{"error": "asked for no session consumer: register with delivery.consumer true"}'
        elif [ "$(printf '%s' "$body" | jq -r .session_id)" = "$session" ]; then
          printf '%s' "$body" | jq -r '.capabilities[]?' >"$control/capabilities"
        fi
        listener_line '{"level":"INFO","msg":"listener subscribe"}' ;;
      /v1/roles/set)
        if [ "$(printf '%s' "$body" | jq -r '.soft and (.previous_session_id // "") != ""')" = true ]; then
          role="$(printf '%s' "$body" | jq -r .role)"
          [ ! -e "$control/drop-role" ] || role="(dropped)"
          listener_line '{"level":"INFO","msg":"listener role taken from a live predecessor because one side carries no client instance id","role":"'"$role"'"}'
        else
          listener_line '{"level":"INFO","msg":"listener role claimed"}'
        fi ;;
      *) status=404; answer='{"error": "not found"}' ;;
    esac ;;
  http://broker.test/*)
    case "$method $path" in
      "POST /v1/secrets/"*/reread)
        if [ "$proof" = true ]; then
          status=401 answer='{"code": "PROOF_INVALID"}'
          printf '{"level":"WARN","message":"broker: refused","status":401,"code":"PROOF_INVALID"}\n' >>"$logs/broker.log"
        else
          status=202 answer='{"accepted": true}'
        fi ;;
      *) status=404; answer='{"error": "not found"}' ;;
    esac ;;
  *) echo "fake curl: no service at $url" >&2; exit 7 ;;
esac
printf '%s' "$answer" >"$out"
[ "$format" != '%{http_code}' ] || printf '%s' "$status"
EOF

fail() {
  echo "FAIL: $*" >&2
  if [ -n "${output:-}" ]; then
    printf 'the check printed:\n%s\n' "$output" >&2
  fi
  exit 1
}

# fresh_stack lays out the stack's work directory anew: three service logs with a line each from
# before the run, and an empty secret store; with unreadable-log, a natsd.log nobody may read.
fresh_stack() {
  chmod -R u+rwX "$stack_dir" 2>/dev/null || true
  rm -rf "$stack_dir"
  mkdir -p "$stack_dir/logs"
  local service
  for service in dispatch listener broker; do
    echo "$service: started before the run" >"$stack_dir/logs/$service.log"
  done
  echo '{"secrets": []}' >"$stack_dir/fake-secrets.json"
  if [ -e "${control_dir}/unreadable-log" ]; then
    echo "natsd: started before the run" >"$stack_dir/logs/natsd.log"
    chmod 000 "$stack_dir/logs/natsd.log"
  fi
}

# run_check runs the check against the fakes with the control files named and every variable of a
# callout stack's banner exported, PRIV's pod included, after a fresh stack; it sets output, status
# and hex, the marker's sixteen hex digits as the check printed them. The variables in unset_vars
# are left out of the environment.
output="" status=0 hex=""
unset_vars=()
run_check() {
  local name
  for name in echo-dispatch echo-body echo-binary silent-listener quiet-sends drop-role delivery-failed delivery-queued \
    switch-refused switch-noop unreadable-log requests capabilities; do
    rm -f "${control_dir}/${name}"
  done
  local -a args=()
  for name in "$@"; do
    case "$name" in
      --*) args+=("$name") ;;
      *) : >"${control_dir}/${name}" ;;
    esac
  done
  fresh_stack
  local -a env_vars=(PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" DEV_STACK_CALLOUT=1
    DEV_STACK_DIR="$stack_dir" DISPATCH_URL=http://dispatch.test ENVOY_URL=http://listener.test
    ENVOY_TOKEN="$listener_word" AGENT_SECRETS_URL=http://broker.test DEV_STACK_PRIV_PROJECT=PRIV
    DEV_STACK_PRIV_OWNER="$priv_owner" DEV_STACK_PRIV_SESSION="$priv_session")
  local -a kept=()
  local assignment
  for assignment in "${env_vars[@]}"; do
    for name in "${unset_vars[@]}"; do
      [ "${assignment%%=*}" != "$name" ] || continue 2
    done
    kept+=("$assignment")
  done
  status=0
  output="$(env -i HOME="$temporary_dir" "${kept[@]}" bash "$check" "${args[@]}" 2>&1)" || status=$?
  hex="$(printf '%s\n' "$output" | sed -n 's/^private-content-logs-check: marker R14-MARK-\([0-9a-f]\{16\}\) .*/\1/p' | head -n1)"
}
has_line() { printf '%s\n' "$output" | grep -qxF -- "$1"; }
has_line_like() { printf '%s\n' "$output" | grep -qE -- "$1"; }
requested() { grep -qxF -- "$1" "${control_dir}/requests" 2>/dev/null; }
last_line() { printf '%s\n' "$output" | tail -n1; }

# (a) Clean logs.
run_check
[ -n "$hex" ] || fail "clean logs: the check printed no marker line"
[ "$status" -eq 0 ] || fail "clean logs: exit $status, want 0"
[ "$(last_line)" = "private-content-logs-check: passed: no file under $stack_dir/logs names R14-MARK-$hex" ] ||
  fail "clean logs: the last line is '$(last_line)'"
for want in "PATCH http://dispatch.test/api/v1/projects/PRIV/access" \
  "PUT http://dispatch.test/api/v1/projects/PRIV/access/people/r14-mark-$hex@example.com" \
  "POST http://dispatch.test/api/v1/issues" "POST http://dispatch.test/api/v1/projects/PRIV/artifacts" \
  "GET http://dispatch.test/api/v1/projects/PRIV/artifacts/r14-mark-$hex" \
  "POST http://dispatch.test/api/v1/issues/PRIV-1/comments" "POST http://dispatch.test/api/v1/issues/PRIV-1/asks" \
  "POST http://dispatch.test/api/v1/asks/a1/answer" "POST http://dispatch.test/api/v1/agents/$priv_session/messages" \
  "GET http://dispatch.test/api/github/rest/r14-mark-$hex" "POST http://broker.test/v1/secrets/PRIV_LOG_CHECK/reread" \
  "GET http://dispatch.test/api/v1/comments/c1"; do
  requested "$want" || fail "clean logs: the check never sent $want"
done
switch_at="$(grep -nxF "PATCH http://dispatch.test/api/v1/projects/PRIV/access" "${control_dir}/requests" | cut -d: -f1)"
issue_at="$(grep -nxF "POST http://dispatch.test/api/v1/issues" "${control_dir}/requests" | cut -d: -f1)"
[ "$switch_at" -lt "$issue_at" ] || fail "clean logs: the issue (request $issue_at) was written before PRIV was made private (request $switch_at)"
[ "$(jq -r --arg mark "R14-MARK-$hex" '[.secrets[] | select(.name == "example/agent-secrets/priv-log-check"
    and .tags["dispatch-project"] == "PRIV" and .value == $mark)] | length' "$stack_dir/fake-secrets.json")" = 1 ] ||
  fail "clean logs: the store holds no PRIV_LOG_CHECK tagged PRIV with the marker as its value: $(cat "$stack_dir/fake-secrets.json")"
[ "$(cat "${control_dir}/capabilities")" = steer ] || fail "clean logs: PRIV's session registered capabilities '$(cat "${control_dir}/capabilities")', not steer"
echo "PASS: clean logs pass with exit 0, after every write, PRIV made private before any content, the secret stored tagged PRIV and PRIV's session registered for steer"

# (b) A log with the marker: the request line names the person's, the document's and the GitHub
# path's slug.
run_check echo-dispatch
[ "$status" -eq 1 ] || fail "a log with the marker: exit $status, want 1"
has_line_like "^FAIL logs/dispatch\\.log:[0-9]+: R14-MARK-$hex: .*path=/api/v1/projects/PRIV/artifacts/r14-mark-$hex" ||
  fail "a log with the marker: no FAIL line names logs/dispatch.log, a line, the marker and the line's text"
[ "$(last_line)" = "private-content-logs-check: failed: R14-MARK-$hex is on 3 lines under $stack_dir/logs" ] ||
  fail "a log with the marker: the last line is '$(last_line)'"
echo "PASS: a log that holds the marker, in a slug's lower case, FAILs with exit 1, naming the file, the line and the marker"

# A log with the marker in its upper case alone: the comment's body quoted in an error string. Six
# writes carry a body with the marker (the issue, the document, the comment, the ask, the answer and
# the direct message), and of them only the document's name holds its lower case.
run_check echo-body
[ "$status" -eq 1 ] || fail "a log with the marker's upper case: exit $status, want 1"
has_line_like "^FAIL logs/dispatch\\.log:[0-9]+: R14-MARK-$hex: .*R14-MARK-$hex comment for the session" ||
  fail "a log with the marker's upper case: no FAIL line names the comment's body"
[ "$(last_line)" = "private-content-logs-check: failed: R14-MARK-$hex is on 6 lines under $stack_dir/logs" ] ||
  fail "a log with the marker's upper case: the last line is '$(last_line)'"
echo "PASS: a log that holds the marker in its upper case alone FAILs, every such line named"

# A log beside the three service logs, whose NUL byte makes grep read it as binary.
run_check echo-binary
[ "$status" -eq 1 ] || fail "a binary log beside the service logs: exit $status, want 1"
has_line_like "^FAIL logs/natsd\\.log:[0-9]+: R14-MARK-$hex: .*GET /api/github/rest/r14-mark-$hex" ||
  fail "a binary log beside the service logs: no FAIL line names logs/natsd.log, the line and its text"
echo "PASS: a log beside the three service logs that holds a NUL byte is read, and its line FAILs"

# (c) A log that did not grow, one that grew only before the first marked write, and one that
# cannot be read.
run_check silent-listener
[ "$status" -eq 2 ] || fail "a log that did not grow: exit $status, want 2"
has_line "UNKNOWN logs/listener.log did not grow during the run" || fail "a log that did not grow: no UNKNOWN line names logs/listener.log"
has_line_like '^FAIL ' && fail "a log that did not grow: a FAIL line"
[[ "$(last_line)" == "private-content-logs-check: UNKNOWN: "* ]] || fail "a log that did not grow: the last line is '$(last_line)'"
echo "PASS: a log that did not grow is UNKNOWN with exit 2, naming it"
run_check quiet-sends
[ "$status" -eq 2 ] || fail "a log that grew only with the set-up: exit $status, want 2"
has_line "UNKNOWN logs/listener.log did not grow during the run" || fail "a log that grew only with the set-up: no UNKNOWN line names logs/listener.log"
echo "PASS: a log that grew only with the set-up, before the first marked write, is UNKNOWN with exit 2, naming it"
if [ "$(id -u)" -ne 0 ]; then
  run_check unreadable-log
  [ "$status" -eq 2 ] || fail "an unreadable log: exit $status, want 2"
  has_line_like "^UNKNOWN logs/natsd\\.log could not be read: " || fail "an unreadable log: no UNKNOWN line names logs/natsd.log"
  echo "PASS: a log that cannot be read is UNKNOWN with exit 2, naming it"
else
  echo "SKIP: an unreadable log: this test runs as root, which reads a file of mode 000"
fi

# (d) A delivery that failed, then one queued, which is what the real stack records: the listener
# took it into the session's own stream, and no client of the stack reads that stream.
run_check delivery-failed
[ "$status" -eq 2 ] || fail "a failed delivery: exit $status, want 2"
has_line_like "^UNKNOWN the comment's delivery to session $priv_session never came: .*\"state\":\"failed\"" ||
  fail "a failed delivery: no UNKNOWN line names the session and the state Dispatch recorded"
run_check delivery-queued
[ "$status" -eq 0 ] || fail "a queued delivery: exit $status, want 0"
has_line "  Dispatch recorded the comment's delivery to session $priv_session queued" ||
  fail "a queued delivery: no line says Dispatch recorded it queued"
echo "PASS: a delivery that failed is UNKNOWN with exit 2, naming the session, and one the listener queued in the session's stream passes"

# (e) The negative control, then a control whose role never reaches listener.log while another log
# names the marker.
run_check --control
[ "$status" -eq 1 ] || fail "--control: exit $status, want 1"
has_line_like "^FAIL logs/listener\\.log:[0-9]+: R14-MARK-$hex: .*\"role\":\"r14-mark-$hex\"" ||
  fail "--control: no FAIL line names logs/listener.log and the role"
requested "POST http://listener.test/v1/roles/set" || fail "--control: no role was claimed"
echo "PASS: --control FAILs with exit 1, naming listener.log, where the listener logged the claimed role"
run_check --control drop-role echo-dispatch
[ "$status" -eq 2 ] || fail "--control with the role dropped: exit $status, want 2"
has_line "UNKNOWN the control's role claim left no line naming R14-MARK-$hex in logs/listener.log, so this run shows nothing of what the search can see" ||
  fail "--control with the role dropped: no UNKNOWN line names the control"
echo "PASS: a control run whose listener.log does not name the marker is UNKNOWN with exit 2, though dispatch.log does"

# (f) An exported stack missing a variable.
unset_vars=(ENVOY_TOKEN)
run_check
unset_vars=()
[ "$status" -eq 2 ] || fail "a stack without ENVOY_TOKEN: exit $status, want 2"
has_line "private-content-logs-check: ENVOY_TOKEN is not set: export a dev-stack.sh --callout banner, or run with none exported" ||
  fail "a stack without ENVOY_TOKEN: no line names it"
[ ! -s "${control_dir}/requests" ] || fail "a stack without ENVOY_TOKEN: the check sent $(wc -l <"${control_dir}/requests") requests"
echo "PASS: an exported stack missing ENVOY_TOKEN is refused with exit 2, naming it, before any request"

# (g) A refused private switch, then one that answers and leaves PRIV public: nothing of PRIV's is
# written to a project that stayed public.
run_check switch-refused
[ "$status" -eq 2 ] || fail "a refused switch: exit $status, want 2"
has_line_like "^UNKNOWN PRIV could not be made private: PATCH /api/v1/projects/PRIV/access answered 404 " ||
  fail "a refused switch: no UNKNOWN line names the route and its answer"
requested "POST http://dispatch.test/api/v1/issues" && fail "a refused switch: an issue was written to a project that stayed public"
run_check switch-noop
[ "$status" -eq 2 ] || fail "a switch that left PRIV public: exit $status, want 2"
has_line_like "^UNKNOWN PRIV is not private after PATCH /api/v1/projects/PRIV/access: it answered 200 " ||
  fail "a switch that left PRIV public: no UNKNOWN line names it"
requested "POST http://dispatch.test/api/v1/issues" && fail "a switch that left PRIV public: an issue was written to it"
echo "PASS: a private switch refused, or answered with PRIV left public, is UNKNOWN with exit 2, and no content is written after it"

# (h) A stack whose banner has no PRIV pod, then the control on such a stack.
unset_vars=(DEV_STACK_PRIV_SESSION)
run_check
[ "$status" -eq 2 ] || fail "no PRIV pod: exit $status, want 2"
has_line "UNKNOWN the stack's banner names no DEV_STACK_PRIV_SESSION, PRIV's pod; nothing of PRIV's was written" ||
  fail "no PRIV pod: no UNKNOWN line names DEV_STACK_PRIV_SESSION"
requested "PATCH http://dispatch.test/api/v1/projects/PRIV/access" && fail "no PRIV pod: the check still wrote to Dispatch"
run_check --control
unset_vars=()
[ "$status" -eq 1 ] || fail "no PRIV pod with --control: exit $status, want 1"
has_line_like "^FAIL logs/listener\\.log:[0-9]+: R14-MARK-$hex: " || fail "no PRIV pod with --control: no FAIL line names logs/listener.log"
echo "PASS: a stack with no PRIV pod is UNKNOWN with exit 2, naming the variable, and --control on it still FAILs naming listener.log"
