#!/usr/bin/env bash
# The two model-driven round trips between Dispatch and a real Oh My Pi session running the packed
# pi-envoy, against a Dispatch, an Envoy listener and a NATS of the run's own:
#
#   E4  the session runs `dispatch ask` on an issue, a person answers the ask on Dispatch, and the
#       answer reaches the session's transcript within five seconds;
#   E5  an issue is routed to role:acceptance-r, the session claims that role with envoy_role_set, a
#       person posts a message on the issue, and it reaches the session's transcript within five
#       seconds.
#
#   omp-roundtrip.sh [E4|E5]     both, one session each, when neither is named
#
# The agent-harness job's plugin-smokes part runs it as a step. It takes the job's environment alone
# (scripts/e2e/lib/plugin-smoke.sh): Oh My Pi from LEGION_TEST_OMP, pi-envoy packed from this
# checkout, natsd and the Envoy listener of packages/envoy-client/test/envoy-stack.ts, Dispatch from
# packages/envoy-client/test/dispatch-stack.ts with its NATS on that stack, on a database of its own
# in LEGION_TEST_PG_DSN, and a model stand-in that makes the session's two tool calls. The person is
# one of the run's own, named by the server's trusted identity header. It prints one
# `CHECK <name>: PASS` or `CHECK <name>: FAIL: <why>` line per check and exits 1 after a FAIL.
set -euo pipefail

me=omp-roundtrip
# shellcheck source-path=SCRIPTDIR source=../../../../scripts/e2e/lib/plugin-smoke.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../../../scripts/e2e/lib/plugin-smoke.sh"

case "${1:-}" in
  "") scenarios=(E4 E5) ;;
  E4 | E5) scenarios=("$1") ;;
  *) fail "the one argument is E4 or E5, the scenario to run alone, not $1" ;;
esac
readonly role="acceptance-r"
readonly answer_option="Yes"
readonly steer_timeout=5

smoke_setup --dispatch
dispatch_person POST /api/v1/projects '{"key":"LEGSMOKE","name":"Round trips"}' >/dev/null ||
  fail "could not create the project LEGSMOKE"
pass

# new_issue TITLE prints the key of a new issue of the project, made by the person.
new_issue() {
  dispatch_person POST /api/v1/issues "$(jq -nc --arg t "$1" '{project: "LEGSMOKE", title: $t}')" | jq -er .key
}

# in_transcript LABEL TEXT succeeds once a transcript of the session LABEL holds TEXT.
in_transcript() {
  grep -rFq -- "$2" "$work/sessions/$1/sessions"
}

# await_in_transcript LABEL TEXT: the transcript holds TEXT within steer_timeout seconds.
await_in_transcript() {
  local start=$SECONDS
  until_true "$steer_timeout" in_transcript "$1" "$2" ||
    fail "no transcript of the session held $2 within ${steer_timeout}s; see $logs/rpc-$1.log"
  note "the transcript held $2 $((SECONDS - start))s later"
}

run_e4() {
  local issue previous candidate ask_id=""
  begin E4-ask-opened
  issue=$(new_issue "The session's ask") || fail "could not create the issue E4 asks on"
  previous=$(dispatch_person GET "/api/v1/issues/${issue}" | jq -r '.open_asks[]? | .id')
  smoke_start_omp e4 "$work/cwd-e4" -- --tools bash -e "$plugin_js"
  smoke_prompt e4 "Run this command in your shell, then wait: dispatch ask --issue ${issue} --question 'Ship it?' --option Yes --option No"
  new_ask() {
    while IFS= read -r candidate; do
      [[ -n "$candidate" ]] || continue
      grep -Fqx -- "$candidate" <<<"$previous" || { ask_id=$candidate; return 0; }
    done < <(dispatch_person GET "/api/v1/issues/${issue}" |
      jq -r '.open_asks[]? | select(.state == "open" and .question == "Ship it?") | .id')
    return 1
  }
  until_true 30 new_ask || fail "the session made no new open \"Ship it?\" ask on ${issue} within 30s; see $logs/rpc-e4.log"
  new_ask
  note "ask ${ask_id} is open on ${issue}"
  pass

  begin E4-answer-steered
  dispatch_person POST "/api/v1/asks/${ask_id}/answer" \
    "$(jq -nc --arg o "$answer_option" '{selected: [$o], expected_edited_at: null}')" >/dev/null ||
    fail "the person's answer to ask ${ask_id} was refused"
  await_in_transcript e4 ask.answered
  pass
}

run_e5() {
  local issue message
  begin E5-role-claimed
  issue=$(new_issue "The role's issue") || fail "could not create the issue E5 routes"
  dispatch_person PATCH "/api/v1/issues/${issue}" "{\"route\":\"role:${role}\"}" >/dev/null ||
    fail "could not route ${issue} to role:${role}"
  smoke_start_omp e5 "$work/cwd-e5" -- --tools bash -e "$plugin_js"
  smoke_prompt e5 "Call envoy_role_set with role ${role}, then wait."
  holds_role() { curl -fsS --max-time 5 "${envoy_url}/v1/roles/${role}" | jq -e --arg s "${omp_sid[e5]}" '.holder == $s'; }
  until_true 30 holds_role ||
    fail "session ${omp_sid[e5]} did not claim role ${role} within 30s: $(curl -sS --max-time 5 "${envoy_url}/v1/roles/${role}" || true)"
  pass

  begin E5-message-delivered
  message="I1 E5 routed human message $(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
  dispatch_person POST "/api/v1/issues/${issue}/messages" "$(jq -nc --arg b "$message" '{body: $b}')" >/dev/null ||
    fail "the person's message on ${issue} was refused"
  await_in_transcript e5 "$message"
  pass
}

for scenario in "${scenarios[@]}"; do
  case $scenario in
    E4) run_e4 ;;
    E5) run_e5 ;;
  esac
done
ok=1
