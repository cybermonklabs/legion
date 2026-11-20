# shellcheck shell=bash
# The rig the Oh My Pi plugin's live scripts share: packages/pi-envoy/scripts/smoke-delivery.sh,
# smoke-btw.sh and smoke-registration-stall.sh, and packages/dispatch/e2e/acceptance/omp-roundtrip.sh.
# Each is itself the test and a step of the agent-harness job's plugin-smokes part
# (.github/workflows/ci-job-agent-harness.yaml). Sourced, never run.
#
# Each runs real Oh My Pi sessions with this checkout's pi-envoy against services of its own, and
# takes nothing else from outside the runner:
#   - Oh My Pi is LEGION_TEST_OMP, the build .omp-pin pins, which .github/actions/install-omp
#     installs; each session runs it in `--mode rpc`, as a Legion pane does, under a HOME of its own;
#   - the plugin is pi-envoy packed as the release packs it (lib/pack-plugin.sh) and unpacked as the
#     worker image unpacks it, its bundle loaded with `-e` and nothing else discovered;
#   - NATS and the Envoy listener are natsd and the listener of
#     packages/envoy-client/test/envoy-stack.ts, and Dispatch, for a script that needs one, the
#     server of packages/envoy-client/test/dispatch-stack.ts with its NATS on that natsd, on a
#     database of its own on LEGION_TEST_PG_DSN (lib/plugin-smoke-stack.ts starts them);
#   - the model is lib/plugin-smoke-model.ts, a stand-in on loopback keyed by a placeholder of the
#     run's own, which the sessions reach as their anthropic provider (ANTHROPIC_BASE_URL).
# No session or service is handed a variable of the caller's shell but PATH: sourcing this file
# unsets every DISPATCH_*, ENVOY_*, NATS_*, OMP*, PI_*, ANTHROPIC_*, CLAUDE_* and LEGION_* variable
# but the two inputs above, and each session starts from an empty environment.
#
# The caller sets `me`, its name, before sourcing. A check begins with `begin <name>`; `pass`
# prints `CHECK <name>: PASS`, and `fail <why>` prints `CHECK <name>: FAIL: <why>` and exits 1, which
# every failing step of a run, a refused input among them, ends in.
#
# Everything the run writes is under one `mktemp -d` of TMPDIR, `$work`. Its logs (the stand-in's
# requests, the services', each session's RPC stream, stderr and transcript) are under `$logs`,
# PLUGIN_SMOKE_EVIDENCE_DIR when that is set (the CI step sets it, and uploads it when the part
# fails), and `$work/logs` otherwise. On exit everything the run started is stopped by the pid it was
# started as, Dispatch's database is dropped, and `$work` is removed, unless the run failed with no
# evidence directory, when it is kept and named.

if [ -z "${me:-}" ]; then
  echo "plugin-smoke.sh: set \`me\` before sourcing it" >&2
  exit 2
fi
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

# The two inputs, read before the sweep takes every other LEGION_* variable.
omp=${LEGION_TEST_OMP:-}
pg_dsn=${LEGION_TEST_PG_DSN:-}
evidence=${PLUGIN_SMOKE_EVIDENCE_DIR:-}
for variable in $(compgen -e); do
  case $variable in
    DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_* | ANTHROPIC_* | CLAUDE_* | LEGION_* | PLUGIN_SMOKE_*) unset "$variable" ;;
  esac
done

check=setup
failed=""
pass() { printf 'CHECK %s: PASS\n' "$check"; }
fail() {
  printf 'CHECK %s: FAIL: %s\n' "$check" "$*"
  failed=1
  exit 1
}
begin() {
  check=$1
  printf '== %s\n' "$check"
}
note() { printf '   %s\n' "$*"; }

work=""
logs=""
standin_pid=""
stack_pid=""
ok=""
declare -A omp_pid=() omp_fd=() omp_sid=() omp_transcript=() omp_sent=()

# alive PID succeeds while PID is a process that has not exited: one that has, and that this shell
# has not reaped yet, answers `kill -0` all the same.
alive() {
  local state
  state=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
  [[ $state != Z* ]]
}

# stop_group PID SECONDS: TERM to PID, the leader of a process group the run started, then KILL to
# the whole group once PID has exited or SECONDS have passed, so nothing it started outlives it.
# The leader alone is asked first, so it can stop what it started in its own order. Best effort: a
# cleanup that fails must not hide the check that failed.
stop_group() {
  local pid=$1 limit=$2 i
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < limit * 5; i++)); do
    alive "$pid" || break
    sleep 0.2
  done
  kill -KILL -- "-$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

smoke_cleanup() {
  local status=$? label file fd
  trap - EXIT
  set +e
  # A command that failed outside a check's own test ended the run under `set -e` with no line
  # of its own: the check it ended is named here, so no run fails without a FAIL line.
  if [ "$status" -ne 0 ] && [ -z "$failed" ]; then
    printf 'CHECK %s: FAIL: the run exited %s inside the check (the lines above say where)\n' "$check" "$status"
  fi
  for label in "${!omp_pid[@]}"; do
    # A session still running is ended as smoke_end_omp ends it, so it deregisters, and stopped.
    fd=${omp_fd[$label]:-}
    [ -z "$fd" ] || exec {fd}>&-
    until_true 5 omp_gone "$label"
    stop_group "${omp_pid[$label]}" 5
    file=${omp_transcript[$label]:-}
    [ -z "$file" ] || [ ! -f "$file" ] || cp "$file" "$logs/transcript-$label.jsonl"
  done
  # The stack stops Dispatch, which drops its database, before the listener and natsd.
  stop_group "$stack_pid" 30
  stop_group "$standin_pid" 5
  if [ -n "$work" ] && [ -d "$work/stack" ]; then
    for file in "$work"/stack/envoy/natsd.log "$work"/stack/envoy/listener.log "$work"/stack/dispatch/dispatch.log; do
      [ ! -f "$file" ] || cp "$file" "$logs/"
    done
  fi
  if [ -n "$work" ]; then
    if [ -n "$ok" ] || [ -n "$evidence" ]; then
      rm -rf "$work"
    else
      printf '%s: the run failed; its work directory, logs under logs/, is kept: %s\n' "$me" "$work"
    fi
  fi
  [ -z "$evidence" ] || printf '%s: logs in %s\n' "$me" "$evidence"
  exit "$status"
}
trap smoke_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# until_true SECONDS COMMAND...: polls COMMAND every half second until it succeeds, for at most
# SECONDS of wall time, its own runs included.
until_true() {
  local deadline=$((SECONDS + $1))
  shift
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  "$@" >/dev/null 2>&1
}

# smoke_isolated NAME=VALUE... -- COMMAND...: replaces this subshell with COMMAND, run with PATH and
# the assignments alone, which are exported rather than put on a command line /proc shows. It
# closes every session's RPC input first, so no child holds another session's stdin open. Run it as
# `(smoke_isolated ...) &`.
smoke_isolated() {
  local name fd
  for fd in "${omp_fd[@]}"; do [ -z "$fd" ] || exec {fd}>&-; done
  for name in $(compgen -e); do
    [ "$name" = PATH ] || export -n "${name?}"
  done
  while [ "$1" != -- ]; do export "${1?}"; shift; done
  shift
  exec "$@"
}

# smoke_setup [--dispatch] [--listener-env NAME=VALUE]...: the `setup` check: the inputs and
# tools, the pack of pi-envoy, the model stand-in and the stack, with Dispatch when --dispatch is
# given; --listener-env sets a variable of the listener's environment. It sets envoy_url, nats_url,
# monitor_url, plugin_js (the bundle a session loads) and plugin_version, and with --dispatch
# dispatch_url and dispatch_token_file, the 0600 file of the agent bearer the server accepts.
smoke_setup() {
  local with_dispatch="" stack_args=() tarball state port
  while [ $# -gt 0 ]; do
    case $1 in
      --dispatch) with_dispatch=1; stack_args+=(--dispatch); shift ;;
      --listener-env) stack_args+=(--listener-env "$2"); shift 2 ;;
      *) fail "smoke_setup: unknown argument $1" ;;
    esac
  done
  begin setup
  [ -n "$omp" ] ||
    fail "LEGION_TEST_OMP is required: the Oh My Pi every session runs (.github/actions/install-omp exports it in CI)"
  [ -x "$omp" ] || fail "LEGION_TEST_OMP names $omp, which is not an executable file"
  if [ -n "$with_dispatch" ]; then
    [ -n "$pg_dsn" ] ||
      fail "LEGION_TEST_PG_DSN is required: the PostgreSQL with pgvector Dispatch makes its database on (the agent-harness job's service in CI)"
  fi
  for tool in bun go curl jq tar od mkfifo flock; do
    command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
  done
  work=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
  logs=${evidence:-$work/logs}
  mkdir -p "$logs" "$work/stack"
  note "Oh My Pi $omp ($("$omp" --version 2>/dev/null | head -n 1)); work directory $work; logs $logs"

  # This checkout's pi-envoy as the release packs it, unpacked as the worker image unpacks it.
  tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$logs/pack-pi-envoy.log") ||
    fail "pi-envoy did not pack; see $logs/pack-pi-envoy.log"
  mkdir -p "$work/pi-envoy"
  tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
  plugin_js=$work/pi-envoy/dist/envoy.js
  [ -r "$plugin_js" ] || fail "the packed pi-envoy has no dist/envoy.js"
  plugin_version=$(jq -r .version "$work/pi-envoy/package.json")
  note "pi-envoy $plugin_version, packed from this checkout"

  # The model: every request must carry the run's placeholder key, which each session is given.
  (umask 077 && printf '%s-standin-%s\n' "$me" "$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')" >"$work/model-key")
  set -m
  (cd "$root" && exec bun scripts/e2e/lib/plugin-smoke-model.ts "$work/standin.port" "$work/model-key" \
    "$logs/standin-requests.jsonl") </dev/null >>"$logs/standin.log" 2>&1 &
  standin_pid=$!
  set +m
  until_true 30 test -s "$work/standin.port" || fail "the model stand-in wrote no port; see $logs/standin.log"
  read -r port <"$work/standin.port"
  standin_url=http://127.0.0.1:$port

  # NATS, the listener and Dispatch, built from this checkout. The stack inherits this shell, for
  # go's build cache and module settings; each service it starts is given its own environment.
  set -m
  (cd "$root" && DISPATCH_TEST_DATABASE_URL=$pg_dsn exec bun scripts/e2e/lib/plugin-smoke-stack.ts "$work/stack" \
    "$work/stack.json" "${stack_args[@]}") </dev/null >>"$logs/stack.log" 2>&1 &
  stack_pid=$!
  set +m
  # A cold build of ./cmd/dispatch comes first, and the server migrates its database as it boots.
  local deadline=$((SECONDS + 600))
  until [ -s "$work/stack.json" ]; do
    alive "$stack_pid" || fail "the stack did not start: $(tail -n 5 "$logs/stack.log" | tr '\n' ' ')"
    ((SECONDS < deadline)) || fail "the stack was not up within 600s; see $logs/stack.log"
    sleep 1
  done
  state=$(<"$work/stack.json")
  envoy_url=$(jq -r .envoyUrl <<<"$state")
  nats_url=$(jq -r .natsUrl <<<"$state")
  monitor_url=$(jq -r .monitorUrl <<<"$state")
  if [ -n "$with_dispatch" ]; then
    dispatch_url=$(jq -r .dispatchUrl <<<"$state")
    dispatch_token_file=$work/dispatch-token
    (umask 077 && jq -j .dispatchToken <<<"$state" >"$dispatch_token_file")
    (umask 077 && printf 'Authorization: Bearer %s\n' "$(<"$dispatch_token_file")" >"$work/dispatch-auth-header")
  fi
  note "model stand-in $standin_url; listener $envoy_url; NATS $nats_url${dispatch_url:+; Dispatch $dispatch_url}"
}

# dispatch_person METHOD PATH [BODY]: a request to the run's Dispatch as the person PERSON names,
# through the server's trusted identity header (dispatch-stack.ts runs it so). It prints the body
# and fails on an answer that is not 2xx.
PERSON=plugin-smoke-person
dispatch_person() {
  local -a body=()
  [ $# -lt 3 ] || body=(--data-binary "$3")
  curl -fsS --max-time 10 -X "$1" -H "X-Dispatch-User: $PERSON" -H 'Content-Type: application/json' \
    "${body[@]}" "$dispatch_url$2"
}

# dispatch_agent METHOD PATH [BODY]: the same request as an agent, with the bearer the server
# accepts, which a header file carries rather than argv.
dispatch_agent() {
  local -a body=()
  [ $# -lt 3 ] || body=(--data-binary "$3")
  curl -fsS --max-time 10 -X "$1" -H "@$work/dispatch-auth-header" -H 'Content-Type: application/json' \
    "${body[@]}" "$dispatch_url$2"
}

# smoke_start_omp LABEL CWD [NAME=VALUE]... -- [OMP ARGUMENT]...: one Oh My Pi session in
# `--mode rpc`, in a process group of its own, in CWD, with a HOME, TMPDIR and session directory of
# its own under $work/sessions/LABEL, the stand-in as its anthropic provider, the run's listener and
# NATS, and the run's Dispatch when there is one; the assignments are added to its environment and
# the arguments to its command line (the extensions it loads, its tools). Its input is a fifo this
# shell holds open, so the session runs until smoke_end_omp closes it. It sets omp_pid, omp_sid and
# omp_transcript for LABEL once the session answers get_state.
smoke_start_omp() {
  local label=$1 cwd=$2 dir=$work/sessions/$1 fd natives state
  shift 2
  local -a env=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do env+=("$1"); shift; done
  [ $# -eq 0 ] || shift
  mkdir -p "$dir/home/.omp" "$dir/tmp" "$dir/sessions" "$cwd"
  # The caller's native-module cache, as lib/omp-home.sh's make_omp_home links it, so a fresh HOME
  # downloads nothing and nothing of the caller's ~/.omp is written.
  natives=$HOME/.omp/natives
  if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
  mkdir -p "$natives"
  ln -sfT "$natives" "$dir/home/.omp/natives"
  [ -z "${dispatch_url:-}" ] || env+=(DISPATCH_URL="$dispatch_url" DISPATCH_TOKEN_FILE="$dispatch_token_file")
  rm -f "$dir/stdin"
  mkfifo "$dir/stdin"
  exec {fd}<>"$dir/stdin"
  # Known before the start, so the session's own process closes this shell's end of its input too
  # (smoke_isolated), and reads the end of it once that end is closed.
  omp_fd[$label]=$fd
  omp_sent[$label]=0
  set -m
  (smoke_isolated HOME="$dir/home" TMPDIR="$dir/tmp" ANTHROPIC_BASE_URL="$standin_url" \
    ANTHROPIC_API_KEY="$(<"$work/model-key")" ENVOY_URL="$envoy_url" ENVOY_NATS_URL="$nats_url" "${env[@]}" -- \
    "$omp" --mode rpc --no-ui --model anthropic/claude-haiku-4-5 --thinking off --cwd "$cwd" \
    --session-dir "$dir/sessions" --no-extensions --no-skills --no-rules --no-lsp --no-title "$@") \
    <"$dir/stdin" >"$logs/rpc-$label.log" 2>"$logs/stderr-$label.log" &
  omp_pid[$label]=$!
  set +m
  smoke_send "$label" '{"type":"get_state","id":"state"}'
  until_true 90 rpc_frame "$label" state || {
    omp_running "$label" ||
      fail "Oh My Pi exited before it answered get_state: $(tail -n 5 "$logs/stderr-$label.log" | tr '\n' ' ')"
    fail "Oh My Pi did not answer get_state within 90s; see $logs/rpc-$label.log"
  }
  state=$(rpc_frame "$label" state)
  omp_sid[$label]=$(jq -r '.data.sessionId' <<<"$state")
  omp_transcript[$label]=$(jq -r '.data.sessionFile' <<<"$state")
  note "Oh My Pi session $label (pid ${omp_pid[$label]}): ${omp_sid[$label]} in $cwd"
}

# rpc_frame LABEL ID prints the response the session LABEL wrote to its command ID, and fails while
# there is none.
rpc_frame() {
  jq -ecnR --arg id "$2" 'first(inputs | fromjson? | select(.type == "response" and .id == $id))' "$logs/rpc-$1.log"
}

# smoke_send LABEL JSON writes one RPC command to the session's input.
smoke_send() { printf '%s\n' "$2" >&"${omp_fd[$1]}"; }

# smoke_prompt LABEL TEXT sends the session TEXT as a prompt.
smoke_prompt() {
  omp_sent[$1]=$((omp_sent[$1] + 1))
  smoke_send "$1" "$(jq -nc --arg id "prompt-${omp_sent[$1]}" --arg m "$2" '{type: "prompt", id: $id, message: $m}')"
}

# omp_running LABEL succeeds while the session's process lives.
omp_running() { alive "${omp_pid[$1]}"; }
omp_gone() { ! alive "${omp_pid[$1]}"; }

# smoke_end_omp LABEL SECONDS ends the session as its host ends it, by closing its input, and
# succeeds once its process has exited, within SECONDS. Oh My Pi then runs the plugin's
# session_shutdown, which deregisters the session; a signal ends it with no shutdown at all.
smoke_end_omp() {
  local label=$1 fd=${omp_fd[$1]}
  exec {fd}>&-
  omp_fd[$label]=""
  if until_true "$2" omp_gone "$label"; then
    wait "${omp_pid[$label]}" 2>/dev/null || true
    return 0
  fi
  return 1
}
