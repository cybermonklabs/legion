#!/usr/bin/env bash
# scripts/e2e/nats-callout-sessions.sh — the live proof of the NATS auth service's r-kinds and
# r-refusal with sessions (its verification's checks c-callout-kinds-plugins, the default mode,
# and c-callout-kinds, --real-hosts): an Oh My Pi session and
# a Claude Code session, each connecting to the hub with its own broker token (NATS_AUTH=broker),
# are admitted to their own channels, receive a direct message, and keep receiving across a
# restart of the listener and across a restart of the hub's server on the same store; and a probe
# run in one session's own environment is refused the other session's channel by name.
#
# The stack is packages/envoy/scripts/dev-stack.sh --callout: the brokers, the listener, Dispatch
# and nats-server behind the NATS auth service, each the process production runs, built from this
# checkout. By default (plugins mode, below) the sessions are the two plugins, each the real code a
# host runs, in processes of their own and with no model: this checkout's Oh My Pi extension
# (packages/pi-envoy) under the stand-in host its own suite drives (role-takeover/omp-client.ts,
# whose heartbeat this script ticks before each message, as the host's timer would), and Claude
# Code's channel server (packages/claude-envoy) under a stand-in `claude` that speaks MCP to it and
# plays the Legion inbox it writes each delivery to (role-takeover/claude-client.ts). The Oh My Pi session is told
# NATS_AUTH=broker in its environment and the Claude Code session `natsAuth: broker` in its
# envoy.json, so both ways of naming it run. Each trades the stack's pod identity (the exchange
# token file the issuer wrote, AGENT_SECRETS_MACHINE_IDENTITY=kubernetes) for a token naming its
# own session, which this script first registers for that pod, as a deployment's daemon does.
# dev-stack.sh restarts natsd and the listener in place on SIGUSR1 and SIGUSR2.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/nats-callout-sessions.sh [--real-hosts] [--binaries <dir>]
#
# It needs a Postgres server it may create two databases on and drops them on exit
# (LEGION_TEST_PG_DSN, else DATABASE_URL, as dev-stack.sh reads them), go (unless --binaries, passed
# on to dev-stack.sh, names the stack's binaries), bun with this checkout's node_modules
# (`bun install`), psql, curl, jq and python3; in plugins mode no model, no container and no
# credential, so a GitHub-hosted runner runs it. It inherits no AGENT_SECRETS_*, DISPATCH_*, ENVOY_*,
# NATS_*, OMP*, PI_*, ANTHROPIC_* or CLAUDE_* variable of its caller's: a Legion pod exports its
# deployment's, and none may reach this stack.
#
# Each check prints `PASS <name>` or `FAIL <name>: <reason>`, every check runs whatever the one
# before it did, and the last line says how many passed; the run exits 0 only when all did. A
# message has reached a session when the session's own consumer (`s-<id>`, read from the hub's
# /jsz) acks the stream sequence it was delivered: the listener acks only on the host's `done`,
# which a host sends once the message is in its turn, so no model reply is read. A receive that
# fails says which step it stopped at: `not admitted` when the hub's connection list
# (/connz?auth=true) names no connection by the user the auth service admits the session as,
# `session/<id>`; `not acked` when the host shows the message but its consumer acked nothing new.
#
#   receives                           a direct message to each session reaches it
#   receives-after-listener-restart    the listener restarted on its port; again
#   receives-after-hub-restart         natsd restarted on its port and store; again
#   receives-after-switch              the Oh My Pi session switched (/new) and the Claude Code
#                                      session cleared (/clear) to session ids of their own, each
#                                      plugin moving its connection to the new id's token; again
#   probe-refused-other-session        natsprobe in the Oh My Pi session's environment is served its
#                                      own channel and refused the Claude Code session's, by name
#
# Negative controls, NATS_CALLOUT_SESSIONS_CONTROL:
#   seedless      both sessions start with no credential named (neither NATS_AUTH nor natsAuth),
#                 so the hub's auth service admits neither connection and every receive FAILs
#                 `not admitted`; the run then exits 1.
#   withhold-ack  (default mode) the Claude Code stand-in takes each delivery into its inbox and
#                 never answers it, so the channel server never sends `done` and the listener never
#                 acks: the first Claude Code receive FAILs `not acked` (the host shows it, its
#                 consumer acked nothing), the later ones FAIL held behind it, and the Oh My Pi ones
#                 pass - the ack check failing for the one reason a transcript check is blind to.
#
# Two kinds of session:
#   default       what e2e.yaml runs on a hosted runner: each plugin's own code under a stand-in
#                 host, as described above, with no model.
#   --real-hosts  the hosts themselves: the Oh My Pi binary in RPC mode (`omp --mode rpc`) with this
#                 checkout's pi-envoy packed as the release packs it (lib/pack-plugin.sh), and the
#                 pinned Claude Code under `legion claude-bridge` (built from this checkout) with
#                 this checkout's claude-envoy, which writes each delivery to the bridge's inbox,
#                 which hands it to Claude Code as a user turn. Each delivery may start a model
#                 turn, so this mode needs a model route and the pins:
#     LEGION_E2E_MODEL_BASE_URL   the Anthropic-compatible endpoint both hosts call
#     LEGION_E2E_MODEL_KEY_FILE   a file holding that endpoint's key, preflighted
#                                 (lib/model-endpoint.sh) before anything starts
#     LEGION_E2E_MODEL_ID         the anthropic/<id> Oh My Pi runs
#     LEGION_E2E_CLAUDE_MODEL     the model id Claude Code runs
#     LEGION_E2E_OMP_PATH         the omp binary (default the build .omp-pin names, where mise has
#                                 it; else the omp on PATH; else the worker image's
#                                 /opt/omp/bin/omp), its --version held to .omp-pin's release and,
#                                 for a mise install of oh-my-pi, its install to .omp-pin's build
#     LEGION_E2E_CLAUDE_CODE_PIN  the directory holding the package.json and package-lock.json that
#                                 pin @anthropic-ai/claude-code (default the worker image's
#                                 /opt/legion/claude-code), installed under $TMPDIR with `npm ci`
#                                 and its --version held to the pin
#                 as envoy-session-states.sh takes them; a missing input refuses the run, naming
#                 it. It runs on a Legion pod, which carries the pins and a model route, or any box
#                 given them. The switch step runs in the default mode alone: it drives each host's
#                 /new and /clear through the stand-in.
#
# NATS_CALLOUT_SESSIONS_EVIDENCE_DIR (default a fresh directory under TMPDIR, kept and printed)
# keeps the stack's logs, each client's output and every answer the checks read.
set -Eeuo pipefail

pg_dsn=${LEGION_TEST_PG_DSN:-${DATABASE_URL:-}}
evidence=${NATS_CALLOUT_SESSIONS_EVIDENCE_DIR:-}
control=${NATS_CALLOUT_SESSIONS_CONTROL:-}
# --real-hosts is this script's own; every other argument is dev-stack.sh's.
hosts=plugins
stack_args=()
for argument in "$@"; do
  if [ "$argument" = --real-hosts ]; then hosts=real; else stack_args+=("$argument"); fi
done
for variable in $(compgen -e); do
  case $variable in
    AGENT_SECRETS_* | DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_* | ANTHROPIC_* | CLAUDE_* | DATABASE_URL | LEGION_TEST_PG_DSN) unset "$variable" ;;
  esac
done
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
drivers=$root/packages/pi-envoy/scripts/role-takeover
channel_server=$root/packages/claude-envoy/bin/envoy-channel.ts

refuse() {
  echo "nats-callout-sessions: $*" >&2
  exit 2
}
case $control in
  "" | seedless) ;;
  withhold-ack) [ "$hosts" = plugins ] || refuse "the withhold-ack control runs in the default mode alone: a real host's inbox is the bridge's" ;;
  *) refuse "NATS_CALLOUT_SESSIONS_CONTROL is \"$control\"; the controls are seedless and withhold-ack" ;;
esac
[ -n "$pg_dsn" ] || refuse "name a Postgres server it may create databases on: LEGION_TEST_PG_DSN or DATABASE_URL"
for tool in bun psql curl jq python3; do
  command -v "$tool" >/dev/null || refuse "$tool is not on PATH"
done
[ -f "$root/node_modules/nats/package.json" ] || refuse "$root/node_modules has no nats package: run bun install"
# lib/model-endpoint.sh's own messages go out through these two.
note() { echo "   $*"; }
fail() { refuse "$*"; }
if [ "$hosts" = real ]; then
  for tool in go npm setsid mkfifo; do
    command -v "$tool" >/dev/null || refuse "$tool is not on PATH"
  done
  model_base_url=${LEGION_E2E_MODEL_BASE_URL:-}
  model_key_file=${LEGION_E2E_MODEL_KEY_FILE:-}
  model_id=${LEGION_E2E_MODEL_ID:-}
  claude_model=${LEGION_E2E_CLAUDE_MODEL:-}
  omp_pin=$(<"$root/.omp-pin")
  omp_build=${omp_pin##*@}
  pinned_omp=$(mise where "$omp_pin" 2>/dev/null || true)
  if [ -n "${LEGION_E2E_OMP_PATH:-}" ]; then
    omp_bin=$LEGION_E2E_OMP_PATH
  elif [ -n "$pinned_omp" ] && [ -x "$pinned_omp/bin/omp" ]; then
    omp_bin=$pinned_omp/bin/omp
  else
    omp_bin=$(command -v omp || echo /opt/omp/bin/omp)
  fi
  claude_pin=${LEGION_E2E_CLAUDE_CODE_PIN:-/opt/legion/claude-code}
  # shellcheck source=scripts/e2e/lib/model-endpoint.sh
  . "$root/scripts/e2e/lib/model-endpoint.sh"
  [ -n "$model_base_url" ] || refuse "LEGION_E2E_MODEL_BASE_URL is required with --real-hosts: the endpoint both hosts call"
  if why=$(model_base_url_refusal "$model_base_url") && [ -n "$why" ]; then
    refuse "LEGION_E2E_MODEL_BASE_URL $why"
  fi
  [ -r "$model_key_file" ] || refuse "LEGION_E2E_MODEL_KEY_FILE names '$model_key_file', which cannot be read"
  [[ $model_id =~ ^anthropic/[^[:space:]]+$ ]] || refuse "LEGION_E2E_MODEL_ID is '$model_id'; it must be anthropic/<id>"
  [ -n "$claude_model" ] || refuse "LEGION_E2E_CLAUDE_MODEL is required with --real-hosts: the model id Claude Code runs"
  [[ -n "$omp_bin" && -x "$omp_bin" ]] || refuse "LEGION_E2E_OMP_PATH names '$omp_bin', which is no executable omp"
  # .omp-pin names github:sjawhar/oh-my-pi@<release>-<build>; omp --version prints omp/<release>
  # alone, so a mise install, whose directory is named for the build, is held to the build too.
  omp_release=${omp_build%%-*}
  omp_reported=$("$omp_bin" --version 2>/dev/null | head -n 1) || true
  [ "$omp_reported" = "omp/$omp_release" ] ||
    refuse "$omp_bin prints --version '$omp_reported'; .omp-pin names $omp_pin (omp/$omp_release)"
  omp_real=$(realpath -e -- "$omp_bin")
  case $omp_real in
    */github-sjawhar-oh-my-pi/*/bin/omp)
      omp_installed=${omp_real%/bin/omp}
      omp_installed=${omp_installed##*/}
      [ "$omp_installed" = "$omp_build" ] ||
        refuse "$omp_bin is the mise install of oh-my-pi $omp_installed; .omp-pin names $omp_build"
      ;;
  esac
  { [ -r "$claude_pin/package.json" ] && [ -r "$claude_pin/package-lock.json" ]; } ||
    refuse "LEGION_E2E_CLAUDE_CODE_PIN names $claude_pin, which holds no package.json and package-lock.json pinning @anthropic-ai/claude-code"
  claude_version=$(jq -er '.dependencies["@anthropic-ai/claude-code"]' "$claude_pin/package.json") ||
    refuse "$claude_pin/package.json pins no @anthropic-ai/claude-code"
  natives=$HOME/.omp/natives
fi

[ -n "$evidence" ] || evidence=$(mktemp -d "${TMPDIR:-/tmp}/nats-callout-sessions-evidence.XXXXXX")
mkdir -p "$evidence/answers"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/nats-callout-sessions.XXXXXX")
work=$scratch
mkdir -p "$work/logs"
echo "nats-callout-sessions: evidence in $evidence (hosts: $hosts${control:+, control: $control})"

# ---- teardown -----------------------------------------------------------------------------------
stack_pid=""
scenario_pids=()
scenario_fds=()
# The real hosts' transcripts, which the teardown keeps in the evidence.
transcripts=()
cleanup_running=false
cleanup() {
  local status=$1 pid fd
  # Once: a repeated INT or TERM, or the EXIT after one, finds the stop already running. Both are
  # ignored from here on rather than put back on their default, which a repeat would then end the
  # run by, before the stack stopped and dropped its databases.
  [ "$cleanup_running" = false ] || return 0
  cleanup_running=true
  trap - EXIT
  trap '' INT TERM
  set +e
  # A client's input ending is its host going away: each closes the end this shell holds of every
  # fifo before it starts, so it sees the end here, runs its shutdown (deregistering its session)
  # and exits; one still running 5 s later is killed with its process group.
  for fd in "${scenario_fds[@]}"; do
    exec {fd}>&-
  done
  for pid in "${scenario_pids[@]}"; do
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  done
  if [ -n "$stack_pid" ] && kill -0 "$stack_pid" 2>/dev/null; then
    kill -TERM "$stack_pid"
    wait "$stack_pid" 2>/dev/null
  fi
  cp -R "$work/logs" "$evidence/client-logs" 2>/dev/null
  # The clients' answers, which the receive checks read beside the hub's consumer state.
  for out in "$work"/*.out; do
    [ -f "$out" ] && cp "$out" "$evidence/client-logs/"
  done
  if [ -n "${claude_home:-}" ] && declare -F claude_transcript >/dev/null; then
    file=$(claude_transcript) && transcripts+=("$file")
  fi
  if [ -d "$work/real" ]; then
    mkdir -p "$evidence/sessions"
    for log in "$work"/real/*/rpc.log "$work"/real/*/stderr.log; do
      [ -f "$log" ] && cp "$log" "$evidence/sessions/$(basename "$(dirname "$log")")-$(basename "$log")"
    done
    for transcript in "${transcripts[@]}"; do
      [ -f "$transcript" ] && cp "$transcript" "$evidence/sessions/"
    done
  fi
  [ -z "${DEV_STACK_DIR:-}" ] || cp -R "$DEV_STACK_DIR/logs" "$evidence/stack-logs" 2>/dev/null
  case $scratch in
    "${TMPDIR:-/tmp}"/nats-callout-sessions.*) rm -rf -- "$scratch" ;;
  esac
  case ${claude_tmp:-} in
    /tmp/ncs-claude.*) rm -rf -- "$claude_tmp" ;;
  esac
  case ${stack_tmp:-} in
    /tmp/ncs-stack.*) rm -rf -- "$stack_tmp" ;;
  esac
  echo "nats-callout-sessions: evidence kept in $evidence"
  exit "$status"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

# ---- the real hosts' pieces, built from this checkout before anything starts ------------------
if [ "$hosts" = real ]; then
  mkdir -p "$work/bin" "$work/real" "$work/bun-cache"
  (umask 077 && tr -d '[:space:]' <"$model_key_file" >"$work/model-key")
  [ -s "$work/model-key" ] || refuse "LEGION_E2E_MODEL_KEY_FILE ($model_key_file) holds no key"
  model_key_preflight "$model_base_url" "$work/model-key" "$work/model-header" \
    "LEGION_E2E_MODEL_KEY_FILE ($model_key_file)"
  (cd "$root/packages/daemon" && go build -o "$work/bin/legion" ./cmd/legion) >"$work/logs/build-legion.log" 2>&1 ||
    refuse "legion did not build; see $work/logs/build-legion.log"
  tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$work/logs/pack-pi-envoy.log") ||
    refuse "pi-envoy did not pack; see $work/logs/pack-pi-envoy.log"
  mkdir -p "$work/pi-envoy"
  tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
  pi_plugin=$work/pi-envoy/dist/envoy.js
  [ -r "$pi_plugin" ] || refuse "the packed pi-envoy holds no dist/envoy.js"
  claude_plugin=$work/claude-envoy
  (cd "$root" && bun packages/claude-envoy/scripts/build.ts --out "$claude_plugin") >"$work/logs/build-claude-envoy.log" 2>&1 ||
    refuse "claude-envoy did not build; see $work/logs/build-claude-envoy.log"
  [ -r "$claude_plugin/dist/envoy-channel.js" ] || refuse "$claude_plugin has no dist/envoy-channel.js"
  # Claude Code itself: the pinned release, installed from the pin with `npm ci` into a directory
  # of the run's own, held to the pin's version - the install a pod's launch makes.
  mkdir -p "$work/claude-code" "$work/npm-cache"
  cp "$claude_pin/package.json" "$claude_pin/package-lock.json" "$work/claude-code/" ||
    refuse "could not copy the Claude Code pin out of $claude_pin"
  (cd "$work/claude-code" && npm ci --ignore-scripts --omit=dev --no-audit --no-fund --cache "$work/npm-cache") \
    >"$work/logs/npm-claude-code.log" 2>&1 ||
    refuse "installing the Claude Code $claude_version that $claude_pin pins failed; see $work/logs/npm-claude-code.log"
  claude_bin=$work/claude-code/node_modules/@anthropic-ai/claude-code-linux-x64/claude
  [ -x "$claude_bin" ] || refuse "the install left no executable at $claude_bin"
  claude_reported=$("$claude_bin" --version) || refuse "the installed Claude Code does not run"
  [ "${claude_reported%% *}" = "$claude_version" ] ||
    refuse "the installed Claude Code prints --version '$claude_reported'; $claude_pin pins $claude_version"
  note "Claude Code $claude_reported from $claude_pin; $omp_reported ($omp_real) as .omp-pin names; pi-envoy packed, claude-envoy built, legion built"
fi

# wait_for SECONDS COMMAND...: runs COMMAND every 0.2 s until it succeeds; 1 once SECONDS passed.
wait_for() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    ((SECONDS < deadline)) || return 1
    sleep 0.2
  done
}

# ---- the stack ----------------------------------------------------------------------------------
stack_log=$evidence/dev-stack.log
echo "nats-callout-sessions: starting dev-stack.sh --callout (log $stack_log)"
# The stack's TMPDIR is short and of its own: dev-stack.sh binds unix sockets in its work directory
# (its port keepers'), and a socket path past 107 bytes is refused, which a caller's deep TMPDIR
# with this run's scratch directory inside it can pass.
stack_tmp=$(mktemp -d /tmp/ncs-stack.XXXXXX)
TMPDIR=$stack_tmp bash "$root/packages/envoy/scripts/dev-stack.sh" --callout --database-url "$pg_dsn" "${stack_args[@]}" \
  >"$stack_log" 2>&1 &
stack_pid=$!
stack_ready() {
  grep -q '^dev-stack: ready\.' "$stack_log" 2>/dev/null && return 0
  kill -0 "$stack_pid" 2>/dev/null || {
    tail -n 30 "$stack_log" >&2
    refuse "dev-stack.sh stopped before it was ready"
  }
  return 1
}
wait_for 1200 stack_ready || refuse "dev-stack.sh was not ready within 20 minutes; see $stack_log"
# The banner's exports, read as values: each `  export NAME=value` line, a quoted value unquoted.
while IFS= read -r line; do
  name=${line#  export }
  value=${name#*=}
  name=${name%%=*}
  value=${value#\"}
  value=${value%\"}
  [ "$name" = PATH ] && value="${value%%:*}:$PATH"
  printf -v "$name" '%s' "$value"
done < <(sed -n 's/^\(  export [A-Z_0-9]*=.*\)$/\1/p' "$stack_log")
for required in AGENT_SECRETS_URL ENVOY_URL NATS_URLS DEV_STACK_DIR DEV_STACK_PID \
  DEV_STACK_EXCHANGE_TOKEN_FILE DEV_STACK_POD_TOKEN_FILE DEV_STACK_REGISTRAR_TOKEN_FILE \
  DEV_STACK_NATS_MONITOR_URL; do
  [ -n "${!required:-}" ] || refuse "dev-stack.sh exported no $required"
done
command -v natsprobe >/dev/null || refuse "natsprobe is not among the stack's binaries"

# ---- the two sessions ---------------------------------------------------------------------------
run=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
if [ "$hosts" = plugins ]; then
  omp_session=ses_ncs_omp_$run
  claude_session=ses_ncs_claude_$run
else
  # Claude Code takes the session id it is told, a UUID; Oh My Pi mints its own, read below.
  omp_session=""
  claude_session=$(cat /proc/sys/kernel/random/uuid)
fi
# A session trades only under a name its pod is registered for: the registrar stands in for the
# daemon that writes that row as it starts a pod. Both tokens reach curl by file, never in an argv,
# which /proc shows every user: the registrar's bearer as a 0600 header file (`-H @file`, as
# lib/model-endpoint.sh sends the model key), and the pod token read by jq itself (`--rawfile`)
# into a 0600 body file (`--data-binary @file`), both in a directory of this call's own under the
# run's scratch directory and removed before it returns.
register_session() {
  local session=$1 code sent
  sent=$(mktemp -d "$work/register.XXXXXX")
  (umask 077 && printf 'Authorization: Bearer %s\n' "$(<"$DEV_STACK_REGISTRAR_TOKEN_FILE")" >"$sent/header")
  (umask 077 && jq -cn --rawfile p "$DEV_STACK_POD_TOKEN_FILE" --arg s "$session" \
    '{pod_token: ($p | sub("\n+$"; "")), session: $s}' >"$sent/body")
  code=$(curl -sS --max-time 20 -o "$evidence/answers/register-$session.json" -w '%{http_code}' \
    -X POST "$AGENT_SECRETS_URL/v1/oauth/exchange/pod-sessions" \
    -H "@$sent/header" -H 'Content-Type: application/json' --data-binary "@$sent/body") || code=unreachable
  rm -rf -- "$sent"
  [ "$code" = 200 ] || [ "$code" = 201 ] || [ "$code" = 204 ] ||
    refuse "registering the pod for $session answered $code: $(head -c 300 "$evidence/answers/register-$session.json")"
}
[ -z "$omp_session" ] || register_session "$omp_session"
register_session "$claude_session"

# What both sessions trade with: the stack's pod identity, at its broker.
broker_env=(
  AGENT_SECRETS_URL="$AGENT_SECRETS_URL"
  AGENT_SECRETS_MACHINE_IDENTITY=kubernetes
  AGENT_SECRETS_MACHINE_TOKEN_FILE="$DEV_STACK_EXCHANGE_TOKEN_FILE"
)
heartbeat_ms=5000
client_reply_timeout=60
# clients.sh's client_fifo, client_ready and send: one stdin fifo per client, its ready line, and
# one command answered on its stdout.
# shellcheck source=packages/pi-envoy/scripts/role-takeover/clients.sh
source "$drivers/clients.sh"

omp_auth=(NATS_AUTH=broker)
[ "$control" != seedless ] || omp_auth=()

# close_held_fifos: closes, in a client's own subshell, the end this shell holds of every client's
# fifo, so the client sees its input end when this shell closes its own (a child that held one
# would hold its own input, or another client's, open).
close_held_fifos() {
  local fd
  for fd in "${scenario_fds[@]}"; do
    exec {fd}>&-
  done
}

# isolated VAR=VALUE... -- CMD...: CMD with the environment the assignments make and nothing of
# this shell's, the model key among them, so the key reaches the session's environment and never
# an argv, which /proc shows every user. Run it as `(isolated ... -- cmd) &`.
isolated() {
  local variable
  for variable in $(compgen -e); do unset "$variable"; done
  while [ "$1" != -- ]; do
    export "${1?}"
    shift
  done
  shift
  exec "$@"
}

if [ "$hosts" = plugins ]; then
# The Oh My Pi session, told NATS_AUTH=broker in its environment.
omp_home=$work/omp-home
mkdir -p "$omp_home/cwd"
jq -n --arg url "$ENVOY_URL" --arg nats "$NATS_URLS" \
  '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}, askgate: {enabled: false}}' \
  >"$omp_home/envoy.json"
client_fifo omp
# Every client closes the ends this shell holds of every client's fifo (close_held_fifos), so it
# sees its input end when this shell closes its own at teardown, and shuts down.
(close_held_fifos && cd -- "$root/packages/pi-envoy" &&
  exec env -i PATH="$PATH" HOME="$omp_home" TMPDIR="$scratch" ENVOY_CONFIG_FILE="$omp_home/envoy.json" \
    ENVOY_HEARTBEAT_MS="$heartbeat_ms" ROLE_TAKEOVER_CWD="$omp_home/cwd" \
    ROLE_TAKEOVER_CLIENT_LOG="$work/logs/omp-notices.log" "${broker_env[@]}" "${omp_auth[@]}" \
    bun --preload "$drivers/host-stub.ts" "$drivers/omp-client.ts") \
  <"$work/omp.stdin" >"${client_out[omp]}" 2>"$work/logs/omp.err" &
scenario_pids+=($!)
wait_for 90 client_ready omp || refuse "the Oh My Pi client did not start: $(tail -n 10 "$work/logs/omp.err")"
send omp "$(jq -cn --arg s "$omp_session" '{op: "start", session: $s}')"

# The Claude Code session, told `natsAuth: broker` in its envoy.json.
claude_config=$work/claude-config
claude_state=$work/claude-state
mkdir -p "$claude_config" "$claude_state"
jq -n --arg url "$ENVOY_URL" --arg nats "$NATS_URLS" --arg control "$control" \
  '{envoy: ({enabled: true, url: $url, natsUrls: [$nats]} + (if $control == "seedless" then {} else {natsAuth: "broker"} end)),
    dispatch: {enabled: false}}' >"$claude_config/envoy.json"
printf '%s\n' "${broker_env[@]}" >"$work/claude.env"
# The inbox socket goes in a short directory of its own: a unix socket path past 107 bytes is
# refused, which a caller's deep TMPDIR with this run's scratch directory inside it can pass.
claude_tmp=$(mktemp -d /tmp/ncs-claude.XXXXXX)
client_fifo claude
claude_flags=()
[ "$control" != withhold-ack ] || claude_flags=(--withhold-inbox-answers true)
(close_held_fifos && cd -- "$root/packages/claude-envoy" &&
  exec env -i PATH="$PATH" HOME="$work/bun-home" TMPDIR="$scratch" \
    bun "$drivers/claude-client.ts" --server "$channel_server" --session "$claude_session" \
    --state-dir "$claude_state" --config-dir "$claude_config" --heartbeat-ms "$heartbeat_ms" \
    --stderr "$work/logs/claude-channel.err" --env-file "$work/claude.env" --inbox "$claude_tmp/inbox" \
    "${claude_flags[@]}") \
  <"$work/claude.stdin" >"${client_out[claude]}" 2>"$work/logs/claude.err" &
scenario_pids+=($!)
wait_for 120 client_ready claude ||
  refuse "the Claude Code client did not start: $(tail -n 10 "$work/logs/claude.err") $(tail -n 10 "$work/logs/claude-channel.err" 2>/dev/null)"
else
  # Real Oh My Pi in RPC mode, this checkout's packed pi-envoy its one extension, told
  # NATS_AUTH=broker in its environment and its Envoy in ~/.config/legion/envoy.json.
  omp_home=$work/real/omp
  mkdir -p "$omp_home/home/.config/legion" "$omp_home/home/.omp" "$omp_home/cwd" "$omp_home/sessions" "$omp_home/tmp"
  jq -n --arg url "$ENVOY_URL" --arg nats "$NATS_URLS" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}, askgate: {enabled: false}}' \
    >"$omp_home/home/.config/legion/envoy.json"
  [ ! -d "$natives" ] || ln -sfT "$natives" "$omp_home/home/.omp/natives"
  mkfifo "$omp_home/stdin"
  exec {omp_fd}<>"$omp_home/stdin"
  scenario_fds+=("$omp_fd")
  (close_held_fifos && isolated PATH="$PATH" HOME="$omp_home/home" TMPDIR="$omp_home/tmp" OMP_SESSION_STORAGE=file \
    ANTHROPIC_BASE_URL="$model_base_url" ANTHROPIC_API_KEY="$(<"$work/model-key")" \
    ENVOY_HEARTBEAT_MS="$heartbeat_ms" "${broker_env[@]}" "${omp_auth[@]}" -- \
    setsid "$omp_bin" --mode rpc --no-ui --model "$model_id" --thinking off --cwd "$omp_home/cwd" \
    --session-dir "$omp_home/sessions" --no-extensions --no-skills --no-rules --no-tools --no-lsp \
    --no-title -e "$pi_plugin") <"$omp_home/stdin" >"$omp_home/rpc.log" 2>"$omp_home/stderr.log" &
  scenario_pids+=($!)
  printf '%s\n' '{"type":"get_state","id":"ncs-state"}' >&"$omp_fd"
  rpc_state() { jq -ecnR 'first(inputs | fromjson? | select(.type == "response" and .id == "ncs-state"))' "$omp_home/rpc.log"; }
  wait_for 120 rpc_state >/dev/null || refuse "Oh My Pi did not answer get_state: $(tail -n 10 "$omp_home/stderr.log")"
  state=$(rpc_state)
  omp_session=$(jq -r '.data.sessionId' <<<"$state")
  omp_transcript=$(jq -r '.data.sessionFile' <<<"$state")
  transcripts+=("$omp_transcript")
  [ -n "$omp_session" ] && [ "$omp_session" != null ] || refuse "Oh My Pi named no session id: $state"
  # Its plugin trades under this name at its next pass, once the pod is registered for it.
  register_session "$omp_session"
  note "Oh My Pi session $omp_session"

  # Real Claude Code under `legion claude-bridge`, with this checkout's claude-envoy as its one MCP
  # server and plugin, told `natsAuth: broker` in its envoy.json. The bridge's inbox takes each
  # delivery the channel server writes and hands it to Claude Code as a user turn.
  claude_home=$work/real/claude
  mkdir -p "$claude_home/home" "$claude_home/config" "$claude_home/cwd"
  # The bridge's TMPDIR is short and of its own: it binds its inbox socket there, and a unix socket
  # path past 107 bytes is refused, which a run's nested scratch directory can pass.
  claude_tmp=$(mktemp -d /tmp/ncs-claude.XXXXXX)
  jq -n --arg url "$ENVOY_URL" --arg nats "$NATS_URLS" --arg control "$control" \
    '{envoy: ({enabled: true, url: $url, natsUrls: [$nats]} + (if $control == "seedless" then {} else {natsAuth: "broker"} end)),
      dispatch: {enabled: false}}' >"$claude_home/config/envoy.json"
  jq -n --arg bun "$(command -v bun)" --arg entry "$claude_plugin/dist/envoy-channel.js" \
    --arg data "$claude_home/config/envoy-plugin-data" \
    '{mcpServers: {legion: {command: $bun, args: [$entry], env: {CLAUDE_PLUGIN_DATA: $data}}}}' \
    >"$claude_home/config/mcp.json"
  plugin_dirs=(--plugin-dir "$claude_plugin")
  [ ! -d "$claude_plugin/legion" ] || plugin_dirs+=(--plugin-dir "$claude_plugin/legion")
  mkfifo "$claude_home/stdin"
  exec {claude_fd}<>"$claude_home/stdin"
  scenario_fds+=("$claude_fd")
  (close_held_fifos && cd "$claude_home/cwd" && isolated PATH="$PATH" HOME="$claude_home/home" TMPDIR="$claude_tmp" \
    CLAUDE_CONFIG_DIR="$claude_home/config" \
    CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 CLAUDE_CODE_DISABLE_CRON=1 \
    LEGION_ROLE=implementer LEGION_ISSUE=NCS-1 ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    ANTHROPIC_BASE_URL="$model_base_url" ANTHROPIC_API_KEY="$(<"$work/model-key")" "${broker_env[@]}" -- \
    setsid "$work/bin/legion" claude-bridge -- "$claude_bin" -p --input-format stream-json \
    --output-format stream-json --verbose --replay-user-messages --permission-mode bypassPermissions \
    --strict-mcp-config --mcp-config "$claude_home/config/mcp.json" "${plugin_dirs[@]}" \
    --session-id "$claude_session" --model "$claude_model") \
    <"$claude_home/stdin" >"$claude_home/rpc.log" 2>"$claude_home/stderr.log" &
  scenario_pids+=($!)
  # One turn of its own first, in the bridge's own prompt frame (it ignores Claude Code's raw user
  # frames): Claude Code's print mode starts its MCP servers with a turn. The session is up once its
  # channel server has registered it with the listener; a session whose connection the hub does not
  # admit (the seedless control) may never register, and its receives then say so.
  jq -cn '{type: "prompt", id: "ncs-first", message: "Reply with the one word: ready."}' >&"$claude_fd"
  claude_registered() { curl -fs --max-time 5 -o /dev/null "$ENVOY_URL/v1/registry/$claude_session"; }
  wait_for 240 claude_registered ||
    note "Claude Code's channel server has not registered $claude_session: $(tail -n 3 "$claude_home/stderr.log")"
  note "Claude Code session $claude_session"
fi

# ---- the checks ---------------------------------------------------------------------------------
ran=0
passed=0
pass() {
  ran=$((ran + 1))
  passed=$((passed + 1))
  echo "PASS $1"
}
failed() {
  ran=$((ran + 1))
  echo "FAIL $1: $2"
}

# admitted_as SESSION: the hub's connection list names a connection the auth service admitted as
# that session's own user.
admitted_as() {
  curl -sf --max-time 10 "$DEV_STACK_NATS_MONITOR_URL/connz?auth=true&limit=1024" |
    jq -e --arg u "session/$1" '[.connections[]? | select(.authorized_user == $u)] | length > 0' >/dev/null
}

# received LABEL MARKER: that session holds a delivery naming MARKER. A stand-in host answers what
# its plugin handed it; a real host's own record holds it: an envoy-message card or a user message
# of Oh My Pi's, a user message of Claude Code's (the turn the bridge made of the delivery). Oh My
# Pi's record is read from the host itself (`get_entries` over its RPC), never from its session
# file: the host writes that file first with the session's first assistant turn, so a card it
# holds the moment its plugin hands it over reaches the file only once the model has answered
# (15.6 s after the card in one run, past the wait below).
# shellcheck disable=SC2016 # jq's variables, not the shell's
readonly omp_entry_text='def text: if type == "string" then . else (map(.text? // empty) | join("\n")) end;
  select((.type == "custom_message" and .customType == "envoy-message")
    or (.type == "message" and .message.role == "user")) | (.content // .message.content | text)'
# shellcheck disable=SC2016 # jq's variables, not the shell's
readonly claude_entries='def text: if type == "string" then . else (map(select(.type? == "text") | .text) | join("\n")) end;
  inputs | fromjson? | select(.type == "user" and (.isMeta | not) and .message.role == "user") | (.message.content | text)'
claude_transcript() {
  local file
  for file in "$claude_home/config/projects"/*/"$claude_session.jsonl"; do
    [ -f "$file" ] && { printf '%s\n' "$file"; return 0; }
  done
  return 1
}
# omp_entries ID: the texts of Oh My Pi's session entries as the host holds them now, its answer
# to one `get_entries` request named ID. Each request needs an ID of its own, or the first answer
# in the log would be read again; received numbers them in its own shell, since this function
# runs in the subshell of the command substitution that reads it. An answer that is an error is
# no record of the host's: it fails, and its error is kept in omp_entries_error for the failure
# line, which would otherwise say the host shows no message.
omp_requests=0
omp_entries_error=$scratch/omp-entries-error
omp_answer() {
  jq -ecnR --arg id "$1" 'first(inputs | fromjson? | select(.type == "response" and .id == $id))' \
    "$omp_home/rpc.log"
}
omp_entries() {
  local answer
  printf '{"type":"get_entries","id":"%s"}\n' "$1" >&"$omp_fd"
  wait_for 10 omp_answer "$1" >/dev/null || return 1
  answer=$(omp_answer "$1") || return 1
  if ! jq -e '.success == true' <<<"$answer" >/dev/null; then
    jq -r '.error // "an answer with success not true"' <<<"$answer" >"$omp_entries_error"
    return 1
  fi
  rm -f -- "$omp_entries_error"
  jq -r ".data.entries[]? | $omp_entry_text" <<<"$answer"
}
received() {
  local file texts
  if [ "$hosts" = plugins ]; then
    send "$1" '{"op":"deliveries"}'
    jq -e --arg m "$2" '(.deliveries // []) + (.turns // []) | any(contains($m))' <<<"$reply" >/dev/null
  elif [ "$1" = omp ]; then
    omp_requests=$((omp_requests + 1))
    texts=$(omp_entries "ncs-entries-$omp_requests") && grep -qF -- "$2" <<<"$texts"
  else
    file=$(claude_transcript) && jq -rnR "$claude_entries" "$file" 2>/dev/null | grep -qF -- "$2"
  fi
}
# not_received LABEL MARKER: why received found no MARKER: the host's own error, where its last
# answer was one, else that it shows no such message.
not_received() {
  if [ "$1" = omp ] && [ "$hosts" = real ] && [ -s "$omp_entries_error" ]; then
    printf "Oh My Pi's get_entries answered an error, so the host's record could not be read: %s" \
      "$(<"$omp_entries_error")"
  else
    printf "the host shows no message naming '%s'" "$2"
  fi
}

# consumer SESSION: the hub's own record of the session's consumer (`s-<id>`, as the listener names
# it), read from the monitor's /jsz; nothing where it has none yet.
consumer() {
  curl -sf --max-time 10 "$DEV_STACK_NATS_MONITOR_URL/jsz?accounts=true&streams=true&consumers=true" |
    jq -c --arg c "s-$1" 'first(.account_details[]?.stream_detail[]?.consumer_detail[]? | select(.name == $c)) // empty'
}
# delivered_through SESSION: the last stream sequence the session's consumer delivered, 0 for none.
delivered_through() {
  local state
  state=$(consumer "$1") || true
  jq -r '.delivered.stream_seq // 0' <<<"${state:-{\}}"
}
# acked_past SESSION FLOOR: the session's consumer delivered a stream sequence past FLOOR and acked
# every one it delivered, so the message sent after FLOOR was read was acked; it prints that
# sequence.
acked_past() {
  local state
  state=$(consumer "$1") && [ -n "$state" ] || return 1
  jq -er --argjson floor "$2" \
    'select(.delivered.stream_seq > $floor and .ack_floor.stream_seq >= .delivered.stream_seq and .num_ack_pending == 0) | .delivered.stream_seq' \
    <<<"$state"
}

# message_each STEP: one direct message to each session, by the listener's own send route, then a
# check per session that its own consumer acked it, naming where it stopped when it did not.
message_each() {
  local step=$1 label session marker code floor sequence acked
  # A stand-in Oh My Pi host's timer is this script's to tick: a pass, which registers the session
  # again, as it would by now. A real host ticks its own.
  [ "$hosts" = real ] || send omp '{"op":"heartbeat"}'
  for label in omp claude; do
    session=$omp_session
    [ "$label" = omp ] || session=$claude_session
    marker="nats-callout-sessions $step $label $run"
    floor=$(delivered_through "$session")
    code=$(curl -sS --max-time 20 -o "$evidence/answers/$step-$label-send.json" -w '%{http_code}' \
      -X POST "$ENVOY_URL/v1/messages/send" -H 'Content-Type: application/json' \
      -d "$(jq -cn --arg t "$session" --arg m "$marker" \
        '{source: "agent", source_session: "nats-callout-sessions", target_session: $t, message: $m}')") ||
      code="unreachable"
    acked=false
    wait_for 45 acked_past "$session" "$floor" >/dev/null && acked=true
    # The hub's record of the session's consumer, kept whatever the receive came to.
    consumer "$session" >"$evidence/answers/$step-$label-consumer.json" || true
    if [ "$acked" = true ]; then
      sequence=$(acked_past "$session" "$floor")
      if ! admitted_as "$session"; then
        failed "$step ($label)" "s-$session acked stream sequence $sequence, but the hub lists no connection admitted as session/$session"
      elif ! wait_for 15 received "$label" "$marker"; then
        failed "$step ($label)" "s-$session acked stream sequence $sequence, but $(not_received "$label" "$marker")"
      else
        pass "$step ($label): s-$session acked stream sequence $sequence, admitted as session/$session"
      fi
    elif received "$label" "$marker"; then
      failed "$step ($label)" "not acked: the host shows the message, but s-$session acked nothing past stream sequence $floor within 45 s"
    elif admitted_as "$session"; then
      failed "$step ($label)" "admitted as session/$session, but the message (send answered $code) did not arrive within 45 s"
    else
      failed "$step ($label)" "not admitted: the hub lists no connection admitted as session/$session (send answered $code)"
    fi
  done
}

# restart WHAT SIGNAL: dev-stack.sh restarts that process in place, and says so once it serves.
# natsd serving again is not yet the listener back on it: its reconnect goes through the auth
# service a moment later, and a send before then is refused, so a natsd restart also waits for
# the listener's own `nats reconnected` line.
restart() {
  local what=$1 signal=$2 before reconnects
  before=$(grep -c "^dev-stack: $what restarted " "$stack_log" || true)
  reconnects=$(grep -c 'nats reconnected' "$DEV_STACK_DIR/logs/listener.log" 2>/dev/null || true)
  # shellcheck disable=SC2153 # DEV_STACK_PID is the banner's, read above by name
  kill "-$signal" "$DEV_STACK_PID"
  restarted() { [ "$(grep -c "^dev-stack: $what restarted " "$stack_log" || true)" -gt "$before" ]; }
  wait_for 60 restarted || refuse "dev-stack.sh did not restart $what on SIG$signal; see $stack_log"
  echo "   $(grep "^dev-stack: $what restarted " "$stack_log" | tail -n 1)"
  if [ "$what" = natsd ]; then
    reconnected() { [ "$(grep -c 'nats reconnected' "$DEV_STACK_DIR/logs/listener.log" 2>/dev/null || true)" -gt "${reconnects:-0}" ]; }
    wait_for 60 reconnected || refuse "the listener did not reconnect to the restarted natsd within 60 s; see $DEV_STACK_DIR/logs/listener.log"
  fi
}

# Both sessions connect once their tokens are traded; the first message waits for that.
wait_for 60 admitted_as "$omp_session" || true
wait_for 60 admitted_as "$claude_session" || true
message_each receives

restart listener USR2
message_each receives-after-listener-restart

restart natsd USR1
message_each receives-after-hub-restart

# A session switch in each host: Oh My Pi's /new (the extension's session_switch) and Claude Code's
# /clear (the handoff file its SessionStart hook writes, which the channel server adopts on its
# own). Each plugin moves its connection to the new id's token before it subscribes that id's
# subjects, so the new id is admitted as its own user and receives. Driven through the stand-ins.
if [ "$hosts" = plugins ]; then
  omp_session=ses_ncs_omp_next_$run
  claude_session=ses_ncs_claude_next_$run
  register_session "$omp_session"
  register_session "$claude_session"
  send omp "$(jq -cn --arg s "$omp_session" '{op: "switch", session: $s, reason: "new"}')"
  send claude "$(jq -cn --arg s "$claude_session" '{op: "clear", session: $s}')"
  wait_for 60 admitted_as "$omp_session" || true
  wait_for 60 admitted_as "$claude_session" || true
  message_each receives-after-switch
fi

# natsprobe in the Oh My Pi session's own environment: its trade names that session, so the hub
# serves its channel and refuses the other session's, in nats-server's own words.
probe() {
  probe_rc=0
  probe_out=$(env -i PATH="$PATH" HOME="$work/probe-home" "${broker_env[@]}" AGENT_SECRETS_SESSION="$omp_session" \
    natsprobe --servers "$NATS_URLS" "$@" 2>&1) || probe_rc=$?
  printf '%s\n' "$probe_out" >>"$evidence/answers/probe.txt"
}
mkdir -p "$work/probe-home"
own=notifications.envoy.deliver.$omp_session
other=notifications.envoy.deliver.$claude_session
probe --subscribe "$own" --wait 1s
served=$(printf '%s\n' "$probe_out" | sed -n 's/^served //p' | head -n1)
own_rc=$probe_rc
probe --subscribe "$other" --wait 2s
refusal=$(printf '%s\n' "$probe_out" | sed -n 's/^refused: //p' | head -n1)
want="Permissions Violation for Subscription to \"$other\""
if [ "$own_rc" -ne 0 ] || [ "$served" != "$own" ]; then
  failed probe-refused-other-session "the probe was not served its own channel $own (exit $own_rc): $(head -c 300 "$evidence/answers/probe.txt")"
elif [ "$probe_rc" -eq 3 ] && [ "$refusal" = "$want" ]; then
  pass "probe-refused-other-session: served $own, refused: $refusal"
else
  failed probe-refused-other-session "the probe was not refused $other by name (exit $probe_rc): $probe_out"
fi

echo "nats-callout-sessions: $passed of $ran checks passed (hosts: $hosts${control:+, control: $control})"
[ "$ran" -gt 0 ] && [ "$passed" -eq "$ran" ]
