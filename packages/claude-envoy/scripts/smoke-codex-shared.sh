#!/usr/bin/env bash
# Run after release from a machine connected to production Envoy and Dispatch.
# Use a Codex 0.171.0 binary on PATH and an interactive terminal.
set -euo pipefail

unknown() { printf 'UNKNOWN %s\n' "$*" >&2; exit 2; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

for tool in codex dispatch jq tmux; do
  command -v "$tool" >/dev/null || unknown "$tool is required"
done
[[ "$(codex --version)" == "codex-cli 0.171.0" ]] || unknown "Use Codex CLI 0.171.0"
[[ -t 0 ]] || unknown "Run from an interactive terminal"

work="$(mktemp -d)"
export CODEX_HOME="$work/codex"
mkdir -p "$CODEX_HOME" "$work"/{first,second,overview}
socket="envoy-smoke-${work##*.}"
mux() { tmux -f /dev/null -L "$socket" "$@"; }
cleanup() {
  mux kill-server 2>/dev/null || true
  codex app-server daemon stop >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT
trap 'exit 2' INT TERM

if [[ -n "${CODEX_ENVOY_VERSION:-}" ]]; then
  command -v npm >/dev/null || unknown "npm is required to install the specified plugin release"
  market="$work/market"
  mkdir -p "$market/.agents/plugins" "$market/plugins/codex-envoy"
  archive="$(npm pack --silent --pack-destination "$work" "@sjawhar/codex-envoy@${CODEX_ENVOY_VERSION}")" ||
    unknown "Could not download codex-envoy ${CODEX_ENVOY_VERSION}"
  tar -xf "$work/$archive" --strip-components=1 -C "$market/plugins/codex-envoy" ||
    unknown "Could not unpack codex-envoy ${CODEX_ENVOY_VERSION}"
  cat >"$market/.agents/plugins/marketplace.json" <<'EOF'
{"name":"envoy-smoke","plugins":[{"name":"codex-envoy","source":{"source":"local","path":"./plugins/codex-envoy"}}]}
EOF
  codex plugin marketplace add "$market" || unknown "Could not configure the local marketplace"
  codex plugin add codex-envoy@envoy-smoke || unknown "Could not install the specified codex-envoy release"
else
  codex plugin marketplace add cybermonklabs/legion || unknown "Could not configure the Legion marketplace"
  codex plugin add codex-envoy@legion-plugins || unknown "Could not install the published codex-envoy release"
fi

if ! codex login status >/dev/null 2>&1; then
  codex login || unknown "Could not sign in to Codex"
fi

roster() {
  dispatch api --path /api/v1/agents | jq -ce 'if type == "array" then . else error("expected agent rows") end'
}
listed() {
  jq -e --arg id "$1" 'any(.[]; .session_id == $id)' <<<"$2" >/dev/null
}
wait_listed() {
  local deadline=$((SECONDS + 10)) rows
  while (( SECONDS <= deadline )); do
    rows="$(roster)" || return 2
    listed "$1" "$rows" && return 0
    sleep 1
  done
  return 1
}
assert_listed() {
  local row="$1" id="$2" status
  wait_listed "$id" && return 0
  status=$?
  (( status == 1 )) || unknown "$row could not read the Dispatch roster"
  fail "$row did not list $id within 10 seconds"
}
transcript_for() {
  local dir="$1" log
  while IFS= read -r -d '' log; do
    if head -n 1 "$log" | jq -e --arg dir "$dir" \
      '.type == "session_meta" and .payload.cwd == $dir and (.payload.id | type == "string")' >/dev/null; then
      printf '%s\n' "$log"
      return
    fi
  done < <(find "$CODEX_HOME/sessions" -type f -name 'rollout-*.jsonl' -print0 2>/dev/null)
  return 1
}
thread_id() {
  head -n 1 "$1" |
    jq -er '.payload.id | select(test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$"))'
}
heard() {
  jq -e -Rs --arg nonce "$2" '
    split("\n") | any(.[] | fromjson?;
      .type == "event_msg" and .payload.type == "agent_message" and
      (.payload.message // "" | contains($nonce)))' "$1" >/dev/null
}
operator() {
  printf '%s\n' "$*"
  read -r -p "Press Enter here when ready. " || unknown "Operator input ended"
}

roster >/dev/null || unknown "Could not read the Dispatch roster"
mux new-session -d -s probe -n first -c "$work/first" 'codex -m gpt-5.5'
mux new-window -t probe -n second -c "$work/second" 'codex -m gpt-5.5'
printf 'Attach from another terminal with tmux -L %q attach -t probe\n' "$socket"
operator "In the first and second windows, trust the plugin hooks and send a first prompt. Wait for each answer."
first_log="$(transcript_for "$work/first")" || unknown "L1 has no transcript for the first window"
second_log="$(transcript_for "$work/second")" || unknown "L1 has no transcript for the second window"
first_id="$(thread_id "$first_log")" || unknown "L1 could not read the first thread ID"
second_id="$(thread_id "$second_log")" || unknown "L1 could not read the second thread ID"
[[ "$first_id" != "$second_id" ]] || fail "L1 both terminals wrote the same thread ID"
l1_failed=()
for id in "$first_id" "$second_id"; do
  if wait_listed "$id"; then
    continue
  elif [[ "$?" == 1 ]]; then
    l1_failed+=("$id")
  else
    unknown "L1 could not read the Dispatch roster"
  fi
done
(( ${#l1_failed[@]} == 0 )) || fail "L1 missing threads ${l1_failed[*]}"
printf 'PASS L1 listed %s and %s\n' "$first_id" "$second_id"

# Sending stays in the operator's Oh My Pi session. This script only observes Codex and Dispatch.
seed="${work##*.}"
nonce_first="envoy-first-$seed"
nonce_second="envoy-second-$seed"
first_target="$first_id"
second_target="$second_id"
if [[ "${CODEX_SMOKE_MISROUTE_SECOND:-0}" == 1 ]]; then second_target="$first_id"; fi
operator "From an Oh My Pi session send 'Reply with $nonce_first' to $first_target and 'Reply with $nonce_second' to $second_target. Wait for the replies."
if ! heard "$first_log" "$nonce_first" || ! heard "$second_log" "$nonce_second" ||
  heard "$first_log" "$nonce_second" || heard "$second_log" "$nonce_first"; then
  fail "L2 the replies did not contain only their own message nonce"
fi
printf 'PASS L2 each thread replied to its own message\n'

mux kill-window -t probe:first
deadline=$((SECONDS + 70))
while :; do
  rows="$(roster)" || unknown "L3 could not read the Dispatch roster"
  listed "$second_id" "$rows" || fail "L3 the second thread disappeared"
  listed "$first_id" "$rows" || break
  (( SECONDS < deadline )) || fail "L3 the first thread still appeared after 70 seconds"
  sleep 1
done
printf 'PASS L3 the first thread stopped and the second stayed listed\n'

mux new-window -t probe -n resumed -c "$work/first" "codex resume -m gpt-5.5 $first_id"
operator "In the resumed window, send one prompt and wait for its answer."
assert_listed L4 "$first_id"
printf 'PASS L4 the resumed thread is listed under %s\n' "$first_id"

mux new-window -t probe -n overview -c "$work/overview" "codex agents -c 'model=\"gpt-5.5\"'"
operator "In the overview window start a new thread in $work/overview. Send a first prompt and wait for the answer."
overview_log="$(transcript_for "$work/overview")" || unknown "L5 has no transcript from codex agents"
overview_id="$(thread_id "$overview_log")" || unknown "L5 could not read the thread ID"
assert_listed L5 "$overview_id"
nonce_overview="envoy-overview-$seed"
operator "From Oh My Pi send 'Reply with $nonce_overview' to $overview_id. Wait for its reply."
heard "$overview_log" "$nonce_overview" || fail "L5 the overview thread did not reply to its message"
printf 'PASS L5 the overview thread received its message\n'
