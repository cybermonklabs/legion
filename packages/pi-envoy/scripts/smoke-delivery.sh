#!/usr/bin/env bash
# smoke-delivery.sh — the end-to-end delivery check of the packed pi-envoy plugin: a real Oh My Pi
# session claims a role, a message published to that role reaches the session's model, and the
# session's end frees the role at once. The agent-harness job's plugin-smokes part runs it as a
# step; packages/pi-envoy/scripts/README.md says what it proves and how to run it.
#
# It takes the job's environment alone (scripts/e2e/lib/plugin-smoke.sh): Oh My Pi from
# LEGION_TEST_OMP, pi-envoy packed from this checkout, natsd and the Envoy listener of
# packages/envoy-client/test/envoy-stack.ts, and a model stand-in. It prints one
# `CHECK <name>: PASS` or `CHECK <name>: FAIL: <why>` line per check and exits 1 after a FAIL.
set -euo pipefail

me=smoke-delivery
# shellcheck source-path=SCRIPTDIR source=../../../scripts/e2e/lib/plugin-smoke.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/e2e/lib/plugin-smoke.sh"

readonly role_claim_timeout=60
readonly delivery_timeout=90
readonly teardown_timeout=15

smoke_setup
hex="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
readonly role="legion-smoke-${hex}"
readonly message="smoke-delivery-payload-${hex}"
readonly session_cwd="$work/cwd"
readonly received_file="${session_cwd}/received.txt"
# The session's only extension is the packed bundle, and its only tool bash, the one it writes the
# delivered message with.
smoke_start_omp delivery "$session_cwd" -- --tools bash -e "$plugin_js"
readonly session_id=${omp_sid[delivery]}
pass

# The role resolves to this session once its model has called envoy_role_set.
begin role-claimed
smoke_prompt delivery "Call the envoy_role_set tool with role=\"${role}\". When a message addressed to that role arrives, run a bash command that writes its exact text (no extra formatting, no trailing newline) to a file named received.txt in the current directory, then reply with the single word DONE."
holder() { curl -fsS --max-time 5 "${envoy_url}/v1/roles/${role}" | jq -e --arg s "$session_id" '.holder == $s'; }
claim_start=$SECONDS
until_true "$role_claim_timeout" holder ||
  fail "role ${role} did not resolve to session ${session_id} within ${role_claim_timeout}s: $(curl -sS --max-time 5 "${envoy_url}/v1/roles/${role}" || true)"
note "role ${role} claimed by ${session_id} after $((SECONDS - claim_start))s"
pass

# The bare role token is a JetStream subject with no bound stream (500); the namespaced
# notifications.role.<role> topic is what the listener's role routing subscribes to. The window
# spans Envoy's delivery and the session's turn that reads the message and writes the file.
begin delivered
publish_topic="notifications.role.${role}"
publish_status=$(curl -sS -o "$logs/publish.json" -w '%{http_code}' --max-time 5 -X POST \
  -H 'Content-Type: application/json' "${envoy_url}/v1/messages/publish" \
  -d "$(jq -nc --arg topic "$publish_topic" --arg message "$message" '{topic: $topic, message: $message}')" || true)
[[ "$publish_status" == 200 ]] || fail "publish to ${publish_topic} returned ${publish_status}, want 200: $(cat "$logs/publish.json" 2>/dev/null)"
# Command substitution strips trailing newlines, which tolerates one after the text.
delivered() { [[ -f "$received_file" && "$(cat -- "$received_file")" == "$message" ]]; }
delivery_start=$SECONDS
until_true "$delivery_timeout" delivered ||
  fail "${received_file} did not hold ${message} within ${delivery_timeout}s: $(cat -- "$received_file" 2>/dev/null || echo 'no file')"
note "message delivered after $((SECONDS - delivery_start))s"
pass

# Teardown is part of the contract: the session's end must release the role at once, not leave a
# stale holder until the listener's live window papers over it. The session must be alive right
# before it is ended, or a prior exit would let the check pass a teardown it never caused. Oh My
# Pi ends an RPC session when its input closes, and runs the plugin's session_shutdown, which
# deregisters it; a signal ends it with no shutdown, and leaves the role to the live window.
begin teardown
omp_running delivery || fail "the session had already exited before the teardown"
smoke_end_omp delivery "$teardown_timeout" ||
  fail "the session was still running ${teardown_timeout}s after its input closed"
role_cleared() { [[ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "${envoy_url}/v1/roles/${role}")" == 404 ]]; }
teardown_start=$SECONDS
until_true "$teardown_timeout" role_cleared ||
  fail "role ${role} still resolved ${teardown_timeout}s after the session ended: $(curl -sS --max-time 5 "${envoy_url}/v1/roles/${role}" || true)"
note "role ${role} cleared $((SECONDS - teardown_start))s after the session ended"
pass
ok=1
