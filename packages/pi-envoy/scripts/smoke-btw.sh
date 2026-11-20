#!/usr/bin/env bash
# smoke-btw.sh — the end-to-end check of a targeted Dispatch message to one live Oh My Pi session
# running the packed pi-envoy: sent while the session's turn is in the middle of a tool call, as a
# BTW (answered on its side turn) and as a steer (read at the turn's next tool boundary), it gets a
# correlated reply on the issue, and the session's end takes it out of the listener's registry. The
# agent-harness job's plugin-smokes part runs it as a step; packages/pi-envoy/scripts/README.md says
# what it proves and how to run it.
#
#   smoke-btw.sh [btw|steer]     both modes, one session each, when neither is named
#
# It takes the job's environment alone (scripts/e2e/lib/plugin-smoke.sh): Oh My Pi from
# LEGION_TEST_OMP, pi-envoy packed from this checkout, natsd and the Envoy listener of
# packages/envoy-client/test/envoy-stack.ts, Dispatch from packages/envoy-client/test/dispatch-stack.ts
# with its NATS on that stack, on a database of its own in LEGION_TEST_PG_DSN, and a model stand-in.
# It prints one `CHECK <name>: PASS` or `CHECK <name>: FAIL: <why>` line per check and exits 1 after
# a FAIL.
set -euo pipefail

me=smoke-btw
# shellcheck source-path=SCRIPTDIR source=../../../scripts/e2e/lib/plugin-smoke.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/e2e/lib/plugin-smoke.sh"

case "${1:-}" in
  "") modes=(btw steer) ;;
  btw | steer) modes=("$1") ;;
  *) fail "the one argument is btw or steer, the mode to run alone, not $1" ;;
esac
readonly session_timeout=60
readonly answer_timeout=90
readonly teardown_timeout=15

smoke_setup --dispatch
# The issue the targeted messages go on and their replies come back to, made by a person.
dispatch_person POST /api/v1/projects '{"key":"LEGSMOKE","name":"Targeted messages"}' >/dev/null ||
  fail "could not create the project LEGSMOKE"
issue=$(dispatch_person POST /api/v1/issues '{"project":"LEGSMOKE","title":"The targeted message smoke"}' | jq -er .key) ||
  fail "could not create the issue the messages go on"
readonly issue
note "messages go on ${issue}"
pass

# run_mode MODE: one session, one targeted message of MODE sent to it while its turn sleeps, the
# reply, and the session's end.
run_mode() {
  local mode=$1 hex label cwd sentinel session_id message response message_id target
  hex="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
  label=$mode
  cwd=$work/cwd-$mode
  sentinel=$cwd/sleep-started
  message="smoke-${mode}-payload-${hex}"

  begin "${mode}-registered"
  # The session's only tool is bash: the sleep, and the `dispatch message` a steer is answered with,
  # which the plugin puts first on the session's PATH from its own bin/.
  smoke_start_omp "$label" "$cwd" -- --tools bash -e "$plugin_js"
  session_id=${omp_sid[$label]}
  registered() {
    curl -fsS --max-time 5 "${envoy_url}/v1/sessions?dir=${cwd}" |
      jq -e --arg id "$session_id" 'any(.[]; .session_id == $id)'
  }
  until_true "$session_timeout" registered ||
    fail "session ${session_id} did not register under ${cwd} within ${session_timeout}s"
  pass

  # The primary turn must be mid-sleep when the targeted message lands: that is what proves BTW does
  # not wait for a turn boundary. The prompt asks for one bash command that creates a sentinel file
  # and then sleeps, so the file exists exactly while the tool call is blocked in the sleep: omp's
  # bash `sleep` is an in-process builtin with no OS process to look for.
  begin "${mode}-sleeping"
  smoke_prompt "$label" "Run exactly this one bash command first, before anything else: touch ${sentinel} && sleep 45. When a Dispatch message arrives, follow its reply_with instruction exactly, running its dispatch command with bash; do not message any other session."
  until_true "$session_timeout" test -e "$sentinel" ||
    fail "the session never started the sleep within ${session_timeout}s; see $logs/rpc-$label.log"
  pass

  # Sent by an agent, with the server's bearer, naming itself in `actor`, so the card on the issue is
  # agent-authored; this session is the only target.
  begin "${mode}-targeted"
  response=$(dispatch_agent POST "/api/v1/issues/${issue}/messages" "$(jq -nc --arg body "$message" \
    --arg target "session:${session_id}" --arg delivery "$mode" --arg actor "smoke-btw-${hex}" \
    '{body: $body, target: $target, delivery: $delivery, actor: {kind: "session", id: $actor}}')") ||
    fail "the targeted ${mode} POST on ${issue} was refused"
  message_id=$(jq -er '.id' <<<"$response") || fail "the targeted ${mode} POST had no message id: ${response}"
  target=$(jq -er '.target' <<<"$response") || fail "the targeted ${mode} POST had no target: ${response}"
  [[ "$target" == "session:${session_id}" ]] || fail "Dispatch targeted ${target}, not this session ${session_id}"
  note "message ${message_id} targets ${target}"
  pass

  # A BTW is answered on the session's side turn and posted by the plugin; a steer is read once the
  # sleep returns, and the session runs the `dispatch message` its reply_with names. Either reply
  # names the message it answers.
  begin "${mode}-replied"
  replied() {
    dispatch_agent GET "/api/v1/issues/${issue}/messages/${message_id}" >"$logs/reply-$mode.json" &&
      jq -e --arg m "$message" 'any(.replies[]?; .body | contains($m))' "$logs/reply-$mode.json"
  }
  until_true "$answer_timeout" replied ||
    fail "no reply naming ${message} arrived for ${message_id} within ${answer_timeout}s: $(cat "$logs/reply-$mode.json" 2>/dev/null)"
  note "reply: $(jq -c '[.replies[] | {author, body}]' "$logs/reply-$mode.json")"
  pass

  # A read that fails (no answer, the listener's refilling 503, a body that is not a list) says
  # nothing about the session, so it is retried, never taken for its absence.
  begin "${mode}-teardown"
  omp_running "$label" || fail "the session had already exited before the teardown"
  smoke_end_omp "$label" "$teardown_timeout" ||
    fail "the session was still running ${teardown_timeout}s after its input closed"
  cleared() {
    curl -fsS --max-time 5 "${envoy_url}/v1/sessions?dir=${cwd}" |
      jq -e --arg id "$session_id" 'type == "array" and all(.[]; .session_id != $id)'
  }
  until_true "$teardown_timeout" cleared ||
    fail "session ${session_id} was still listed ${teardown_timeout}s after it ended: $(curl -sS --max-time 5 "${envoy_url}/v1/sessions?dir=${cwd}" || true)"
  pass
}

for mode in "${modes[@]}"; do
  run_mode "$mode"
done
ok=1
