#!/usr/bin/env bash
# smoke-session-title.sh — the test that the name a person gave a Claude Code session is what this
# plugin registers with a real Envoy listener, that a rename replaces it, and that a session
# nobody named keeps the folder-and-short-id fallback (dispatch://INFRA-92). The
# `envoy-session-stall` job of `ci.yaml` runs it.
#
# It runs the bundles, not the TypeScript: a NATS server and an Envoy listener, both built from
# this tree, and a copy of the plugin built from this tree into the scratch directory
# (`scripts/build.ts --out`), whose `dist/session-hook.js` runs for each payload and
# `dist/envoy-channel.js` as the session, read back through the listener's own
# `GET /v1/sessions`.
#
# WHAT IS SIMULATED. This script does not start Claude Code, so the two hook payloads are JSON
# this script writes, shaped as Claude Code's hooks reference documents them
# (https://code.claude.com/docs/en/hooks: a SessionStart hook receives `session_title`, "the
# session's custom title, present when one is set, for example with `--name`, `/rename` …", and
# "UserPromptSubmit hooks also receive `session_title` …"). The run proves that the real hook
# bundle records what Claude Code would hand it and that the real channel server registers it
# with a real listener. It does not prove that Claude Code sends the field; that rests on Claude
# Code's own documentation.
#
# Each check prints `CHECK <name>: PASS` or `CHECK <name>: FAIL: <reason>`, and a FAIL exits 1:
# `setup` (the tools, the builds, NATS and the listener), then one check per case.
#
#   bash packages/claude-envoy/scripts/smoke-session-title.sh
set -euo pipefail

readonly heartbeat_ms="${ENVOY_HEARTBEAT_MS:-1000}"
readonly chosen_name="eeny meeny"
readonly renamed="miny moe"
readonly register_timeout=30

package_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly package_dir
repo_root="$(cd "${package_dir}/../.." && pwd)"
readonly repo_root

check=setup
pass() { printf 'CHECK %s: PASS\n' "$check"; }
fail() {
  printf 'CHECK %s: FAIL: %s\n' "$check" "$*"
  [[ -s "${workdir:-}/server.log" ]] && { printf -- '--- channel server log ---\n'; cat "${workdir}/server.log"; } >&2
  exit 1
}

for tool in bun curl jq go; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is not on PATH"
done

workdir="$(mktemp -d "${TMPDIR:-/tmp}/claude-session-title.XXXXXX")"
readonly workdir
nats_pid=""
listener_pid=""
server_pid=""
fifo_pid=""

cleanup() {
  [[ -n "$server_pid" ]] && kill "$server_pid" 2>/dev/null
  [[ -n "$fifo_pid" ]] && kill "$fifo_pid" 2>/dev/null
  [[ -n "$listener_pid" ]] && kill "$listener_pid" 2>/dev/null
  [[ -n "$nats_pid" ]] && kill "$nats_pid" 2>/dev/null
  wait 2>/dev/null || true
  rm -rf "$workdir"
  return 0
}
trap cleanup EXIT INT TERM

# The plugin under test, built from this tree: a pull request's tree carries no fresh `dist/`.
readonly plugin_dir="${workdir}/claude-envoy"
bun "${package_dir}/scripts/build.ts" --out "$plugin_dir" >"${workdir}/build.log" 2>&1 ||
  fail "the plugin did not build: $(cat "${workdir}/build.log")"

# NATS is packages/envoy's natsd (internal/testnats/cmd/natsd), the nats-server this module pins
# in one process, which e2e-local.sh and e2e-refill.sh run too. Each go command stands on a line of
# its own, which is how .github/scripts/check-ci-filters.sh reads what a script's go command
# compiles.
(
  cd "${repo_root}/packages/envoy"
  go build -o "${workdir}/natsd" ./internal/testnats/cmd/natsd
  go build -o "${workdir}/envoy-listener" ./cmd/listener
) >"${workdir}/go-build.log" 2>&1 || fail "natsd or the Envoy listener did not build: $(cat "${workdir}/go-build.log")"

# Both servers bind port 0, so the kernel chooses each port and nothing can take it before the
# server serves on it, and each reports the port it got: natsd (`-p -1`) on its `Listening for
# client connections on` line, once it logs `Server is ready`, and the listener (PORT=0) as the
# `addr` of its `envoy-listener listening` JSON line.
"${workdir}/natsd" -p -1 -store_dir "${workdir}/nats" >"${workdir}/nats.log" 2>&1 &
nats_pid=$!
for _ in $(seq 60); do
  grep -q 'Server is ready' "${workdir}/nats.log" && break
  kill -0 "$nats_pid" 2>/dev/null || fail "natsd exited at startup: $(cat "${workdir}/nats.log")"
  sleep 0.5
done
grep -q 'Server is ready' "${workdir}/nats.log" || fail "natsd was not ready within 30s: $(cat "${workdir}/nats.log")"
nats_port="$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "${workdir}/nats.log")"
readonly nats_port
[[ -n "$nats_port" ]] || fail "natsd logged no client port: $(cat "${workdir}/nats.log")"

env PORT=0 ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID=session-title-smoke \
  NATS_URLS="nats://127.0.0.1:${nats_port}" \
  "${workdir}/envoy-listener" >"${workdir}/listener.log" 2>&1 &
listener_pid=$!
listener_addr=""
for _ in $(seq 60); do
  listener_addr="$(jq -Rr 'fromjson? | select(.msg == "envoy-listener listening") | .addr' "${workdir}/listener.log")"
  [[ -n "$listener_addr" ]] && break
  kill -0 "$listener_pid" 2>/dev/null || fail "the listener exited at startup: $(cat "${workdir}/listener.log")"
  sleep 0.5
done
[[ -n "$listener_addr" ]] || fail "the listener reported no address within 30s: $(cat "${workdir}/listener.log")"
readonly envoy_url="http://${listener_addr}"
printf 'natsd on %s and an Envoy listener built from this tree on %s\n' "$nats_port" "$listener_addr"
for _ in $(seq 60); do
  [[ "$(curl -fsS --max-time 2 "${envoy_url}/healthz" 2>/dev/null)" == *healthy* ]] && break
  sleep 0.5
done
[[ "$(curl -fsS --max-time 2 "${envoy_url}/healthz" 2>/dev/null)" == *healthy* ]] ||
  fail "the listener was not healthy within 30s: $(cat "${workdir}/listener.log")"
pass

# The session's own scratch world: the envoy.json Claude Code's rule picks
# (`$CLAUDE_CONFIG_DIR/envoy.json`), the plugin state directory the hook records titles in, the
# `dispatch` command's state directory the registration stamps, and a project directory whose
# name is the fallback title's first half.
readonly config_dir="${workdir}/claude-config"
readonly plugin_data="${workdir}/plugin-data"
readonly dispatch_state="${workdir}/dispatch-state"
readonly project_dir="${workdir}/probe-project"
mkdir -p "$config_dir" "$plugin_data" "$dispatch_state" "$project_dir"
# Dispatch is enabled against port 1, where nothing serves: the session stamps its title for the
# `dispatch` command without this smoke needing a Dispatch server.
jq -n --arg envoy "$envoy_url" --arg nats "nats://127.0.0.1:${nats_port}" \
  --arg dispatch "http://127.0.0.1:1" \
  '{dispatch: {enabled: true, serverUrl: $dispatch, token: "session-title-smoke"},
    envoy: {enabled: true, url: $envoy, natsUrls: [$nats]}}' >"${config_dir}/envoy.json"

# The channel server runs as Claude Code runs it: no ENVOY_SESSION_ID, because that QA override
# also turns the `dispatch` command's title stamp off, and this smoke asserts the stamp.
run_channel_server() {
  local bundle="$1" sid="$2"
  mkfifo "${workdir}/stdin"
  sleep 600 >"${workdir}/stdin" &
  fifo_pid=$!
  env -i PATH="$PATH" HOME="$workdir" \
    CLAUDE_CONFIG_DIR="$config_dir" CLAUDE_PLUGIN_DATA="$plugin_data" \
    CLAUDE_PROJECT_DIR="$project_dir" CLAUDE_CODE_SESSION_ID="$sid" \
    DISPATCH_STATE_DIR="$dispatch_state" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    bun "$bundle" <"${workdir}/stdin" >"${workdir}/server.log" 2>&1 &
  server_pid=$!
}

stop_channel_server() {
  [[ -n "$server_pid" ]] && kill "$server_pid" 2>/dev/null
  [[ -n "$fifo_pid" ]] && kill "$fifo_pid" 2>/dev/null
  # A killed job's status is its signal, which `set -e` would otherwise take for a failure.
  wait "$server_pid" "$fifo_pid" 2>/dev/null || true
  server_pid=""
  fifo_pid=""
  rm -f "${workdir}/stdin"
  return 0
}

# The title the listener holds for the session `$1`, or empty when it holds no such session.
registered_title() {
  curl -fsS --max-time 5 "${envoy_url}/v1/sessions" 2>/dev/null |
    jq -r --arg id "$1" '[.. | objects | select(.session_id? == $id)][0].title // empty'
}

# Waits up to `register_timeout` for the listener's title for the session `$1` to equal `$2`.
await_title() {
  local sid="$1" expected="$2" seen=""
  for _ in $(seq $((register_timeout * 2))); do
    seen="$(registered_title "$sid")"
    [[ "$seen" == "$expected" ]] && return 0
    sleep 0.5
  done
  printf 'the listener holds %q for %s, not %q\n' "$seen" "$sid" "$expected" >&2
  return 1
}

# One hook run, with the environment Claude Code gives a hook and `payload` on its stdin. The
# title hook is a bundle of its own and takes no mode argument: Claude Code blocks the user's
# prompt on it, so it carries none of the SessionStart bundle's weight.
run_hook() {
  local mode="$1" payload="$2"
  local -a command=("${plugin_dir}/dist/session-hook.js" "$mode")
  if [[ "$mode" == session-title ]]; then
    command=("${plugin_dir}/dist/session-title-hook.js")
  fi
  printf '%s' "$payload" | env -i PATH="$PATH" HOME="$workdir" \
    CLAUDE_CONFIG_DIR="$config_dir" CLAUDE_PLUGIN_DATA="$plugin_data" \
    CLAUDE_PROJECT_DIR="$project_dir" \
    bun "${command[@]}" >"${workdir}/hook-${mode}.out" 2>&1 ||
    fail "the ${mode} hook exited non-zero: $(cat "${workdir}/hook-${mode}.out")"
}

readonly session_id="smoke-title-0001"
readonly short_id="${session_id:0:8}"
readonly fallback="probe-project ${short_id}"

check=chosen-name-registers
printf '\ncase 1: a SessionStart payload carrying the name a person gave the session\n'
run_hook open-asks "$(jq -nc --arg id "$session_id" --arg dir "$project_dir" --arg title "$chosen_name" \
  '{session_id: $id, cwd: $dir, hook_event_name: "SessionStart", source: "startup", session_title: $title}')"
run_channel_server "${plugin_dir}/dist/envoy-channel.js" "$session_id"
await_title "$session_id" "$chosen_name" || fail "the chosen name did not reach the listener"
printf 'the listener holds %q for %s\n' "$chosen_name" "$session_id"
pass

check=rename-replaces-it
printf '\ncase 2: the session is renamed, and the next registration carries the new name\n'
run_hook session-title "$(jq -nc --arg id "$session_id" --arg title "$renamed" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit", session_title: $title}')"
await_title "$session_id" "$renamed" || fail "the rename did not reach the listener"
printf 'the listener holds %q for %s\n' "$renamed" "$session_id"
pass

check=unnamed-keeps-the-fallback
printf '\ncase 3: the name is taken away, and the folder-and-short-id fallback returns\n'
run_hook session-title "$(jq -nc --arg id "$session_id" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit"}')"
await_title "$session_id" "$fallback" || fail "the fallback did not return"
printf 'the listener holds %q for %s\n' "$fallback" "$session_id"
pass

check=dispatch-command-carries-the-name
printf '\ncase 4: the dispatch command carries the same name on what this session writes\n'
run_hook session-title "$(jq -nc --arg id "$session_id" --arg title "$renamed" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit", session_title: $title}')"
await_title "$session_id" "$renamed" || fail "the rename did not reach the listener a second time"
readonly stamp="${dispatch_state}/sessions/${session_id}/title"
[[ -f "$stamp" ]] || fail "no title was stamped for the dispatch command at ${stamp}"
[[ "$(cat "$stamp")" == "$renamed" ]] ||
  fail "the dispatch command's title is $(cat "$stamp"), not ${renamed}"
printf '%s holds %q\n' "$stamp" "$renamed"
stop_channel_server
pass
