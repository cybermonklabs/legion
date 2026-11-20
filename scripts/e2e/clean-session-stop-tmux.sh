#!/usr/bin/env bash
# The daemon's clean stop, measured end to end (dispatch://INFRA-392): a Legion role whose agent
# process is SIGKILLed under its worker shim is `gone` to the Envoy listener within ten seconds,
# because the daemon deletes that session's registration the moment its probe finds the process
# gone (supervise.Deps.SessionGone -> cleanSessionStop's DELETE /v1/sessions/<id>), rather than the
# record waiting out the gone horizon.
#
# The clause holds for both harnesses a role runs on, so the proof kills both: Oh My Pi under
# `legion worker-shim`, and Claude Code under `legion claude-bridge`, which the shim runs in Oh My
# Pi's place for a role `worker_backends` puts on claude.
#
# The measurement's control — a session no daemon launched, killed the same way, waits out the
# gone horizon — is no part of this run: packages/envoy-client/test/session-stall.ts holds it ("a
# killed session stays unresponsive until the gone horizon, then reads gone"), against the same
# listener. What keeps the ten seconds here a measurement of the daemon's stop rather than of the
# listener's reaper is the horizon this run sets, nine times the limit, and the daemon's own line
# for each stop (clean_stop_recorded, below).
#
# The agent-harness job's tmux-scripts part runs it as a step
# (.github/workflows/ci-job-agent-harness.yaml):
#
#   LEGION_TEST_OMP=… LEGION_TEST_CLAUDE=… LEGION_TEST_PG_DSN=… \
#     bash scripts/e2e/clean-session-stop-tmux.sh
#
# ---- inputs ---------------------------------------------------------------------------------------
# - LEGION_TEST_OMP (required) is the Oh My Pi the daemon runs in every Oh My Pi pane and in its
#   own boot gate: the build .omp-pin pins, which .github/actions/install-omp installs in CI.
# - LEGION_TEST_CLAUDE (required) is the Claude Code binary the Claude Code pane runs, by an
#   absolute path: the release .claude-code-pin pins, which .github/actions/install-claude-code
#   installs in CI.
#   The proof knows each agent's process by the program the kernel records it runs, so a wrapper
#   script or a mise shim named in either is refused.
# - LEGION_TEST_PG_DSN (required) is a loopback PostgreSQL the run creates the daemon's own database
#   in and drops on exit. This script starts no database.
# - CLEAN_SESSION_STOP_CHECKS, or --checks <names>, is the comma-separated list of named checks to
#   run; unset, every check runs. --list prints every name with what it needs. A selection is an
#   operator's explicit choice and the summary line says which selection ran.
# - CLEAN_SESSION_STOP_EVIDENCE_DIR (default a fresh directory under $TMPDIR, kept and printed)
#   holds the run's transcript, every service's log, the daemon's log and each check's answers.
# - CLEAN_SESSION_STOP_HEARTBEAT_MS (default 5000) is how often every session of the run
#   heartbeats its registration, the panes' ENVOY_HEARTBEAT_MS. The window section below says what
#   it is for; 5000 is the value the rig used before this input existed, so a caller that names
#   none runs what it always ran.
#
# Nothing else comes from outside the runner. The model both harnesses call is
# lib/messages-standin.ts, an Anthropic Messages stand-in on loopback that answers every turn
# `ready` and refuses a request without the placeholder key of the run's own; no pane carries that
# key in its environment: the Oh My Pi profile and the Claude Code claim directory each name a key
# command that prints it. NATS is natsd, built from this checkout. Everything the run builds goes
# under $TMPDIR, as does everything it writes.
#
# ---- the windows, and why they are these numbers ------------------------------------------------
# The listener judges every record with
#   ENVOY_SESSION_LIVE_WINDOW=30s     a pane that stops heartbeating reads unresponsive six
#                                     heartbeats after its last one
#   ENVOY_SESSION_GONE_HORIZON=90s    the boundary the clean stop must beat by an order of
#                                     magnitude; ten minutes would measure it no better
#   ENVOY_SESSION_REAP_INTERVAL=10s   the reaper acts promptly once a record passes the horizon
# and every pane heartbeats at ENVOY_HEARTBEAT_MS, CLEAN_SESSION_STOP_HEARTBEAT_MS's 5000 by
# default (runtime.tmux.pane_env), so a live pane refreshes six times inside the live window; the
# plugin's own default is 90 s, which no 30 s window could hold. The ten seconds the clean stop is
# held to are this issue's acceptance, not a number this rig chose: the daemon's probe runs the
# moment the shim's stream closes, and the delete is one HTTP call after it.
#
# The heartbeat is an input because it decides how often a kill lands just after a registration,
# which is the window a clean stop dated before the moment its probe judged refuses a stop that
# is owed in (dispatch://INFRA-2064). A small value makes a kill inside that window likely —
# 3 of 4 runs at main with 500 — so the defect's reproduction is a command rather than an edit,
# and 5000, the value before this input existed, leaves every run as it was. It moves nothing
# else: the live window, the gone horizon, the reap interval and the ten-second acceptance are
# the numbers above whatever it is.
#
# ---- the named checks ---------------------------------------------------------------------------
# Each prints `CHECK <name>: PASS` or `CHECK <name>: FAIL: <reason>`, every selected check runs
# whatever the one before it did, and the last line says how many ran and how many passed; the run
# exits 1 when any failed. The setup steps before them print a CHECK line each as well, and a
# failed one ends the run there.
#   clean-stop-of-an-omp-role        an architect on Oh My Pi, killed under `legion worker-shim`
#   clean-stop-of-a-claude-role      an implementer `worker_backends` puts on claude, killed under
#                                    `legion claude-bridge`
#
# ---- what the run starts, and what stops it ------------------------------------------------------
# The model stand-in, a NATS server (internal/testnats/cmd/natsd), one Envoy listener, one `legion
# start` daemon with a private tmux server of its own, and the panes that daemon launches. Every
# one of them is stopped on any exit, by the pid it was started as; the daemon's tmux server is
# killed, the run's database is dropped and its work directory removed. The run is re-runnable: it
# takes a work directory of its own each time and inherits no DISPATCH_*, ENVOY_*, LEGION_*,
# NATS_*, OMP*, PI_*, TMUX*, ANTHROPIC_*, CLAUDE_*, PGHOSTADDR, PGSERVICE or PGSERVICEFILE
# variable of the caller's.
# Several runs at once out of one jj checkout leave divergent commits behind, whatever that
# checkout's own state. The build step reads the working copy with jj to report what it built
# from (lib/built-from.sh), and jj snapshots the working copy at every read; the plugin pack
# before it rewrites packages/<plugin>/package.json in place and puts it back (lib/pack-plugin.sh),
# so each run has a window in which a tracked file differs. Two runs snapshotting that window at
# once leave the checkout with divergent working-copy commits for someone to abandon by hand.
# Commit the tree before the runs and expect to check `jj log` after them.
set -Eeuo pipefail

# This script's own inputs are read first: the sweep below drops every variable of those families
# from the caller's environment, so that nothing the run starts inherits a Legion pane's real
# daemon, listener, bus or Oh My Pi profile, and it would otherwise drop these with them.
pg_dsn=${LEGION_TEST_PG_DSN:-}
wanted=${CLEAN_SESSION_STOP_CHECKS:-}
evidence=${CLEAN_SESSION_STOP_EVIDENCE_DIR:-}
heartbeat_ms=${CLEAN_SESSION_STOP_HEARTBEAT_MS:-5000}
omp_bin=${LEGION_TEST_OMP:-}
claude_bin=${LEGION_TEST_CLAUDE:-}
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go with the rest: require_loopback_dsn in
# scripts/e2e/lib/loopback-dsn.sh says why a caller of the loopback guard clears the three.
for variable in $(compgen -e); do
  case $variable in
    DISPATCH_* | ENVOY_* | LEGION_* | NATS_* | OMP* | PI_* | TMUX* | ANTHROPIC_* | CLAUDE_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$variable" ;;
  esac
done

root=$(cd "$(dirname "$0")/../.." && pwd)
check=setup
timeout_hook=
ok=
ran=0
passed=0
selection=all
reason=
answer=/dev/null
api_status=
state=
# measured is the latency the running check measured, in milliseconds: the time from the SIGKILL to
# the first read of `gone`. Each check prints its own, and the summary prints them all, because a
# latency is the kind of fact a reader quotes.
measured=
declare -A latencies=()

# ---- the check table ----------------------------------------------------------------------------
# Each row is `<name> <what it kills>`.
checks=(
  "clean-stop-of-an-omp-role an Oh My Pi pane the daemon launched"
  "clean-stop-of-a-claude-role a Claude Code pane the daemon launched"
)

usage() {
  cat <<'EOF'
clean-session-stop-tmux.sh — the daemon's clean stop, measured (dispatch://INFRA-392)

  bash scripts/e2e/clean-session-stop-tmux.sh [--checks <name,name,…>] [--list]

--checks, or CLEAN_SESSION_STOP_CHECKS, runs only the named checks; unset, every check runs.
--list prints each check's name and the process it kills.
EOF
}

list_checks() {
  local row
  for row in "${checks[@]}"; do printf '%-36s %s\n' "${row%% *}" "${row#* }"; done
}

while [ $# -gt 0 ]; do
  case $1 in
    --checks)
      [ $# -ge 2 ] || {
        echo "--checks needs a comma-separated list of check names" >&2
        exit 2
      }
      wanted=$2
      shift 2
      ;;
    --checks=*)
      wanted=${1#--checks=}
      shift
      ;;
    --list)
      list_checks
      exit 0
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

[ -n "$evidence" ] || evidence=$(mktemp -d "${TMPDIR:-/tmp}/clean-session-stop-evidence.XXXXXXXX")
mkdir -p "$evidence/logs" "$evidence/answers"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/clean-session-stop.$$.XXXXXXXX")
# Every program the run execs from here on carries its mark, and the sweep in cleanup ends what
# carries it and nothing else (marked_processes, lib/process-tree.sh); a start that clears its
# environment (`env -i`) names the mark itself, and a fork of this shell that execs nothing carries
# none and is stopped by its pid (run_marks' comment there). The transcript's tee, started above,
# carries none, so it outlives the sweep and writes the run's last line.
export LEGION_E2E_RUN=$work

note() { echo "   $*"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
begin() {
  check=$1
  echo "== $1"
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/model-endpoint.sh
. "$root/scripts/e2e/lib/model-endpoint.sh"

# ---- the selection ------------------------------------------------------------------------------
selected=()
if [ -z "$wanted" ]; then
  for row in "${checks[@]}"; do selected+=("${row%% *}"); done
else
  selection=$wanted
  IFS=, read -r -a asked <<<"$wanted"
  for name in "${asked[@]}"; do
    [ -n "$name" ] || continue
    found=
    for row in "${checks[@]}"; do [ "${row%% *}" != "$name" ] || found=1; done
    [ -n "$found" ] || fail "'$name' is no check of this proof; --list prints every name"
    selected+=("$name")
  done
  [ ${#selected[@]} -gt 0 ] || fail "--checks named no check"
fi
# Every check needs the daemon, its listener and both harnesses' builds: the daemon resolves Claude
# Code at boot for a configuration whose worker_backends names it, so a selection that kills only
# an Oh My Pi pane still boots that daemon.

# ---- the run's state ----------------------------------------------------------------------------
natsd_pid=
standin_pid=
listener_pid=
listener_keeper_pid=
daemon_pid=
daemon_keeper_pid=
database=
nats_port=
nats_url=
standin_port=
listener_port=
daemon_port=
project=
ptoken=
state_dir=$work/state
omp_home=$work/omp-home
profile=clean-session-stop-$$
# The model id both harnesses name, which the stand-in answers whatever it is: the Oh My Pi panes as
# anthropic/<it>, the Claude Code pane as <it>.
model_id=standin-model
# The windows the listener judges every record against; the header says why each is this number.
# The heartbeat every session of the run keeps is CLEAN_SESSION_STOP_HEARTBEAT_MS, read above.
live_window=30s
gone_horizon=90s
reap_interval=10s
# The acceptance this proof measures: a role's record is gone this many seconds after the kill.
clean_stop_limit_seconds=10

cleanup() {
  local status=$1 p name
  trap - EXIT INT TERM ERR
  set +e
  stop_pid "$daemon_pid"
  [ -z "$ptoken" ] || TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1
  stop_pid "$listener_pid"
  stop_pid "$natsd_pid"
  stop_pid "$standin_pid"
  stop_pid "$daemon_keeper_pid"
  stop_pid "$listener_keeper_pid"
  # Any process of the run's the daemon's tmux server left: each carries the run's mark, and nothing
  # of this user's else does.
  stop_marked_processes "$work"
  if [ -n "$database" ]; then
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\"" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand" >&2
  fi
  rm -rf "$work"
  for name in "${!latencies[@]}"; do
    echo "   measured SIGKILL-to-gone latency, $name: ${latencies[$name]}"
  done
  if [ -n "$ok" ] && [ "$ran" -eq "$passed" ] && [ "$status" -eq 0 ]; then
    echo "clean session stop e2e: $ran checks ran, $passed passed — PASS ($selection, ${SECONDS}s, evidence: $evidence)"
  else
    [ "$status" -ne 0 ] || status=1
    echo "clean session stop e2e: $ran checks ran, $passed passed — FAIL at $check ($selection, ${SECONDS}s, evidence: $evidence)" >&2
  fi
  exit "$status"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM
# Any command that fails outside a check's own assertions still names the step it ended.
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND"' ERR

# ---- running a check ----------------------------------------------------------------------------
# A check is a function that returns 0, or sets `reason` and returns 1. run_check reports it and
# goes on to the next, so one failure never hides the checks after it, and the summary counts both.
run_check() {
  local name=$1
  check=$name
  begin "$name"
  reason=
  measured=
  ran=$((ran + 1))
  if "check_${name//-/_}"; then
    passed=$((passed + 1))
    echo "CHECK $name: PASS"
  else
    echo "CHECK $name: FAIL: ${reason:-no reason recorded}"
  fi
}

# refuse MESSAGE records why the running check failed and fails it, so every failure leaves one
# sentence a reader can act on rather than a bare non-zero.
refuse() {
  reason=$1
  return 1
}

# wait_until SECONDS DESCRIPTION COMMAND… polls COMMAND twice a second until it succeeds, and fails
# the check when it has not by SECONDS: lib/rig.sh's until_true for a check, which fails its own
# check instead of ending the run, so the checks after it still run.
wait_until() {
  local limit=$1 what=$2 waited=0
  shift 2
  while [ "$waited" -lt "$((limit * 2))" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    waited=$((waited + 1))
    sleep 0.5
  done
  refuse "timed out after ${limit}s waiting for $what"
}

# ---- the listener's HTTP surface ------------------------------------------------------------------
# api sets api_status and writes the body to $answer rather than printing either: a caller that read
# the status out of a command substitution would lose the body with the subshell.
api() { # api METHOD PATH
  local method=$1 path=$2 slug
  slug=$(printf '%s' "$path" | tr -c '[:alnum:]' '_' | cut -c1-48)
  answer=$evidence/answers/$check$slug.$RANDOM.json
  api_status=$(curl -sS --max-time 20 -o "$answer" -w '%{http_code}' -X "$method" \
    -H "@$work/envoy-auth-header" "http://127.0.0.1:$listener_port$path" 2>>"$evidence/logs/curl.log") || api_status=000
  [ "$api_status" = 200 ]
}

# session_state SESSION sets `state` to what the listener says of a session: the row's own `state`
# for an answered read, `gone` for the 404 that names no live session. Any other answer leaves
# `state` empty and fails, so a caller never reads a 503 as a verdict.
session_state() {
  state=
  api GET "/v1/registry/$1?include=unresponsive" || true
  case $api_status in
    200) state=$(jq -r '.state' "$answer") ;;
    404) state=$(jq -r '.state // "gone"' "$answer") ;;
    *) return 1 ;;
  esac
}

# state_is SESSION WANT is session_state as a predicate, for wait_until.
state_is() { session_state "$1" && [ "$state" = "$2" ]; }

serving() { curl -fsS --max-time 3 -H "@$work/envoy-auth-header" "http://127.0.0.1:$1/v1/sessions"; }

# ---- the kill, and the measurement ----------------------------------------------------------------

# agent_descendant PID NAME prints the pane's agent: the one process below PID, the pid the claim's
# locator recorded, that runs the program NAME stands for ($omp_bin for omp, $claude_bin for
# claude, the two the daemon is told: LEGION_OMP_PATH and LEGION_CLAUDE_PATH) by the kernel's record
# (launched, lib/process-tree.sh), whatever name it was called by. No argv[0] is read: a wrapper or
# an argv[0] of another name cannot stand in for the agent or hide it.
agent_descendant() {
  case $2 in
    omp) launched "$1" "$omp_bin" ;;
    claude) launched "$1" "$claude_bin" ;;
    *)
      printf 'no agent program is named %s\n' "$2"
      return 1
      ;;
  esac
}

# parent_pid PID prints its parent's, from the stat fields after the comm field's own `) `.
parent_pid() {
  local stat
  stat=$(cat "/proc/$1/stat") || return 1
  read -r -a stat <<<"${stat##*) }"
  printf '%s\n' "${stat[1]}"
}

# shimmed PID succeeds when `legion worker-shim` is an ancestor of PID: the agent this proof kills
# is the one the daemon supervises, not a stray process of the same name. It prints the ancestry it
# walked, so a failure says what the pane's processes were.
shimmed() {
  local pid=$1 depth=0
  local -a args
  while [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" != 1 ] && [ "$depth" -lt 12 ]; do
    if process_args "$pid" args; then
      printf '     %s %s\n' "$pid" "${args[0]} ${args[1]-}" >&2
      [ "${args[1]-}" != worker-shim ] || return 0
    fi
    pid=$(parent_pid "$pid") || return 1
    depth=$((depth + 1))
  done
  return 1
}

# gone_within SESSION KILLED_MS LIMIT polls the listener ten times a second until it reads the
# session gone, and holds the latency it measures to LIMIT seconds. It reports the number it
# measured either way: a clean stop that took too long is a different fact from one that never
# happened, and a reader needs to be told which.
gone_within() {
  local session=$1 killed=$2 limit=$3 now elapsed
  while :; do
    session_state "$session" || true
    now=$(date +%s%3N)
    elapsed=$((now - killed))
    if [ "$state" = gone ]; then
      measured="${elapsed}ms"
      latencies[$check]=$measured
      note "GET /v1/registry/$session?include=unresponsive answered $api_status, $(jq -c '{state, error} | with_entries(select(.value != null))' "$answer"), ${elapsed}ms after the SIGKILL (the limit is ${limit}s)"
      [ "$elapsed" -le "$((limit * 1000))" ] ||
        refuse "the session reached gone ${elapsed}ms after the SIGKILL, past the ${limit}s this issue's acceptance allows" || return 1
      return 0
    fi
    if [ "$elapsed" -gt 60000 ]; then
      refuse "the session read '${state:-no answer}' 60s after the SIGKILL and never reached gone; the daemon's clean stop did not land" || return 1
    fi
    sleep 0.1
  done
}

# ---- the daemon's own surface -----------------------------------------------------------------------
legion() {
  env -i PATH="$PATH" HOME="$omp_home" TMPDIR="$work/tmp" XDG_STATE_HOME="$work/xdg" LEGION_E2E_RUN="$work" \
    "$work/bin/legion" "$@" --config "$work/legion.yaml" --operator-token-file "$work/operator-token"
}

# claim_json TOKEN prints the daemon's record of one claim.
claim_json() {
  legion claims list --json 2>>"$evidence/logs/legion-cli.log" |
    jq -ce --arg t "$1" '.claims[] | select(.token == $t)'
}
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; }
claim_field() { claim_json "$1" | jq -r "$2"; }
# claim_pane_pid TOKEN prints the process its pane was launched as, out of the claim's locator: the
# incarnation `<pid>:<start ticks>`, and only while that pid is still the process that started then
# (incarnation_pid, lib/process-tree.sh), never a pid the kernel has since given to another.
claim_pane_pid() {
  local inc
  inc=$(claim_field "$1" '.locator.incarnation // empty') && [ -n "$inc" ] || return 1
  incarnation_pid "$inc"
}

# daemon_said PATTERN counts the daemon's log lines matching PATTERN.
daemon_said() {
  local count
  count=$(grep -c -- "$1" "$evidence/logs/daemon.log" 2>/dev/null) || count=0
  printf '%s\n' "$count"
}
daemon_said_at_least() { [ "$(daemon_said "$1")" -ge "$2" ]; }

# clean_stop_recorded SESSION reports whether the daemon's clean stop of SESSION ended in one of
# the two lines that mean the registration is gone because the daemon acted, and leaves the one it
# found in stop_record.
#
# The first is the stop it made. The second is the listener answering that it holds no
# registration of the session, which the daemon reports rather than sending a delete: on the
# Claude arm claude-envoy's own deregistration can land first, and a record already gone is the
# outcome the stop asked for.
#
# The line this does NOT accept is the stop the listener kept. Until dispatch://INFRA-2064 it
# did, on the ground that the session's own plugin would deregister it instead — which is false
# for a SIGKILL of the process that plugin runs in (packages/pi-envoy/src/registration-state.ts,
# unregisterSession is an explicit call). The listener keeps a registration for three reasons
# (packages/envoy/internal/session/delete.go): another client instance wrote it, it was written
# after the moment the delete belongs to, or the delete lost its compare-and-swap twice. For a
# role this run SIGKILLed, each of the three is a defect worth failing on. The first cannot
# happen inside the relaunch's first backoff and a process boot. The second is the stop dated
# before the probe's own moment, which is the defect this issue fixed, or a heartbeat whose
# write landed after the probe. The third is a registration that changed under a delete nothing
# should have been writing. So the check fails on a kept answer whichever reason it carries, and
# the daemon's line names that reason for whoever reads the failure.
#
# What the clause claims is that the session is gone well inside the horizon, and gone_within
# above is what measures that; this asks that the daemon's own stop is why, which is what keeps
# the measurement from passing on the reaper (dispatch://INFRA-392).
stop_record=
clean_stop_recorded() {
  local session=$1 line
  for line in "the Envoy session of a role whose process is gone was stopped" \
    "the Envoy listener answered no registration of a dead role's session, so none was stopped"; do
    if grep -F -- "$line" "$evidence/logs/daemon.log" 2>/dev/null | grep -qF -- "$session"; then
      stop_record=$line
      return 0
    fi
  done
  return 1
}

# spawn_claim ISSUE ROLE launches one claim of the run's tree and prints its token.
spawn_claim() {
  local spawned
  spawned=$(legion claims spawn --json --tree "$tree" --issue "$1" --role "$2" \
    --prompt-file "$work/role-prompt.md" 2>>"$evidence/logs/legion-cli.log") ||
    return 1
  printf '%s\n' "$spawned" >"$evidence/answers/spawn-$1-$2.json"
  jq -er .token <<<"$spawned"
}

# ensure_tree_root spawns the tree's root architect once and prints its token. A worker claim is
# bound to its tree, and a daemon with no workflow holds a tree's lifecycle only from the moment
# its root claim is admitted ("bind work of tree …: tree lifecycle is absent"), so the claude check
# run on its own starts the root too; with the Oh My Pi check before it, the claim is already
# there and this adds nothing.
ensure_tree_root() {
  local token=legion-$ptoken-${tree,,}-architect
  if claim_json "$token" >/dev/null 2>&1; then
    printf '%s\n' "$token"
    return 0
  fi
  spawn_claim "$tree" architect
}

# kill_the_agent TOKEN NAME SIGKILLs the agent process of that claim's pane and prints the
# millisecond it was killed at. The pane's own process is left alone: the clause is about the agent
# dying under its shim, which is what a crashed Oh My Pi or Claude Code does. The pane is the
# locator's incarnation, checked by its start ticks (claim_pane_pid), and the agent is signalled
# only while it is the process launched found and still runs under a worker shim (freeze_if,
# lib/process-tree.sh).
kill_the_agent() {
  local token=$1 name=$2 pane agent start
  pane=$(claim_pane_pid "$token") || return 1
  agent=$(agent_descendant "$pane" "$name") || return 1
  start=$(proc_start "$agent") && [ -n "$start" ] || return 1
  shimmed "$agent" || return 1
  freeze_if "$agent" "$start" KILL shimmed || return 1
  date +%s%3N
}

# ---- prerequisites --------------------------------------------------------------------------------
begin prerequisites
note "selection: $selection (${#selected[@]} checks)"
for tool in bun curl go jq od psql python3 tar tmux; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done
[[ $heartbeat_ms =~ ^[1-9][0-9]*$ ]] ||
  fail "CLEAN_SESSION_STOP_HEARTBEAT_MS is '$heartbeat_ms'; it must be a positive whole number of milliseconds"
[ -n "$pg_dsn" ] ||
  fail "LEGION_TEST_PG_DSN is required: a loopback PostgreSQL the run creates the daemon's own database in (the agent-harness job's service in CI)"
case $pg_dsn in
  postgres://* | postgresql://*) ;;
  *) fail "LEGION_TEST_PG_DSN must be a postgres:// or postgresql:// URL" ;;
esac
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$pg_dsn" "the run creates and drops a database there" || fail "$db_host"
# The proof kills the agent's own process, which it knows by the program the kernel records it runs
# (agent_descendant), so the Oh My Pi and the Claude Code it is given are the binaries themselves
# (native_program, lib/process-tree.sh): a wrapper script's process runs its interpreter and a mise
# shim's runs the version mise picks, and the agent either starts runs another file.
[ -n "$omp_bin" ] ||
  fail "LEGION_TEST_OMP is required: the Oh My Pi every Oh My Pi pane runs (.github/actions/install-omp exports it in CI)"
[ -x "$omp_bin" ] || fail "LEGION_TEST_OMP names $omp_bin, which is not an executable file"
omp_bin=$(native_program "$omp_bin") || fail "LEGION_TEST_OMP: $omp_bin"
[ -n "$claude_bin" ] ||
  fail "LEGION_TEST_CLAUDE is required: the Claude Code binary the Claude Code pane runs (.github/actions/install-claude-code exports it in CI)"
[[ $claude_bin == /* ]] && [ -x "$claude_bin" ] || fail "LEGION_TEST_CLAUDE names $claude_bin, which is not an absolute path to an executable file"
claude_native=$(native_program "$claude_bin") || fail "LEGION_TEST_CLAUDE: $claude_native"
installed=$("$claude_bin" --version) || fail "the Claude Code LEGION_TEST_CLAUDE names does not run"

mkdir -p "$work/bin" "$work/tmp" "$work/tmux" "$work/xdg" "$omp_home/.omp" "$state_dir"
chmod 0700 "$work/tmux" "$state_dir"
(
  umask 077
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/envoy-token"
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/operator-token"
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header"
  # The placeholder key the model stand-in holds every request to.
  printf 'clean-session-stop-standin-%s\n' "$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')" >"$work/model-key"
)
project=CSS$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')
project=${project:0:10}
ptoken=${project,,}
tree=CSS-1
note "work $work; evidence $evidence; project $project"
echo "CHECK prerequisites: PASS"

# ---- builds ----------------------------------------------------------------------------------------
begin build
go -C "$root/packages/daemon" build -o "$work/bin/legion" ./cmd/legion
go -C "$root/packages/envoy" build -o "$work/bin/envoy-listener" ./cmd/listener
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd
while IFS= read -r line; do note "$line"; done < <(
  bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/bin/legion" "$work/bin/envoy-listener"
) || fail "lib/built-from.sh could not say what the run built"
(cd "$root" && bun install --frozen-lockfile) >"$evidence/logs/bun-install.log" 2>&1 ||
  fail "bun install failed; see $evidence/logs/bun-install.log"

# The Oh My Pi profile every pane and the daemon's own boot gate run under: this checkout's two
# plugins, packed as the release packs them and installed as the worker image installs them, under
# a HOME of the run's own. The caller's native-module cache is linked in, so a fresh home downloads
# nothing and the caller's own ~/.omp is neither written nor removed.
natives=$HOME/.omp/natives
[ -z "${XDG_DATA_HOME:-}" ] || [ ! -d "$XDG_DATA_HOME/omp" ] || natives=$XDG_DATA_HOME/omp/natives
mkdir -p "$natives"
ln -sfT "$natives" "$omp_home/.omp/natives"

# profile_omp runs that Oh My Pi as the daemon runs it: the binary by its absolute path, under the
# run's home and profile, from a directory holding no configuration of anyone's.
profile_omp() {
  (cd "$work" && env -i PATH="$PATH" HOME="$omp_home" TMPDIR="$work/tmp" OMP_PROFILE="$profile" LEGION_E2E_RUN="$work" \
    "$omp_bin" "$@")
}

install_plugin() { # install_plugin <pi-envoy|pi-legion>
  local package=$1 tarball dest=$work/$1 name version
  tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" "$package" "$work/pack-$package" \
    2>"$evidence/logs/pack-$package.log") ||
    fail "$package did not pack; see $evidence/logs/pack-$package.log"
  mkdir -p "$dest"
  tar xzf "$tarball" -C "$dest" --strip-components=1
  name=$(jq -r .name "$dest/package.json")
  version=$(jq -r .version "$dest/package.json")
  profile_omp plugin install "$dest" >>"$evidence/logs/omp-plugin-install.log" 2>&1 ||
    fail "omp plugin install of $package failed; see $evidence/logs/omp-plugin-install.log"
  profile_omp plugin list --json |
    jq -e --arg n "$name" --arg v "$version" \
      '[.npm[]? | select(.name == $n and .version == $v and .enabled == true)] | length == 1' >/dev/null ||
    fail "$name@$version is not installed and enabled in the run's OMP profile $profile"
  note "$name@$version from this checkout, in OMP profile $profile"
}
install_plugin pi-envoy
install_plugin pi-legion

# The panes' model route: the stand-in (started with the services, below) as the anthropic
# provider, keyed by a command that reads the run's 0600 placeholder, so no key is in a pane's
# environment, its argv or its transcript. It is written once the stand-in has its port.
write_omp_route() {
  mkdir -p "$omp_home/.omp/profiles/$profile/agent"
  {
    printf 'providers:\n  anthropic:\n'
    printf '    baseUrl: %s\n' "$model_base_url"
    printf '    auth: apiKey\n    api: anthropic-messages\n'
    printf "    apiKey: '!cat %s'\n" "$work/model-key"
    printf '    models:\n'
    printf '      - {id: %s, name: %s, reasoning: true, input: [text], contextWindow: 200000, maxTokens: 8192}\n' \
      "$model_id" "$model_id"
  } >"$omp_home/.omp/profiles/$profile/agent/models.yml"
  {
    echo 'defaultThinkingLevel: low'
    echo 'modelRoles:'
    for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
      echo "  $role: anthropic/$model_id"
    done
  } >"$omp_home/.omp/profiles/$profile/agent/config.yml"
}

# The claude-envoy plugin a Claude Code pane loads, built from this checkout outside it: a pull
# request's tree carries no fresh dist/ or legion/.
bun "$root/packages/claude-envoy/scripts/build.ts" --out "$work/claude-envoy" \
  >"$evidence/logs/build-claude-envoy.log" 2>&1 ||
  fail "packages/claude-envoy did not build; see $evidence/logs/build-claude-envoy.log"
note "Claude Code $installed at $claude_bin, with this checkout's claude-envoy as its pane plugin"
echo "CHECK build: PASS"

# ---- services ---------------------------------------------------------------------------------------
begin services
# The model both harnesses call, on a port it reports, every request it answered or refused kept
# in the evidence directory.
start_process standin bun "$root/scripts/e2e/lib/messages-standin.ts" "$work/standin.port" "$work/model-key" \
  "$evidence/answers/standin-requests.jsonl"
await_port_file standin "$work/standin.port" standin_port
model_base_url=http://127.0.0.1:$standin_port
# The preflight holds the stand-in to the key: GET /v1/models answers 200 to it and refuses a call
# without it, so a pane's model turn is proof its key command ran (lib/model-endpoint.sh).
model_key_preflight "$model_base_url" "$work/model-key" "$work/model-header" "the run's placeholder key ($work/model-key)"
write_omp_route
note "the model stand-in on $model_base_url answers both harnesses: the Oh My Pi panes run anthropic/$model_id, the Claude Code pane $model_id"

start_process natsd "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start natsd "$natsd_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/natsd.log"
nats_port=$(report_port "$evidence/logs/natsd.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/natsd.log"
nats_url=nats://127.0.0.1:$nats_port
note "NATS on $nats_url, its JetStream store in $work/nats-store"

keep_port listener_keeper listener_port
start_kept listener listener_keeper ENVOY_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent LEGION_E2E_RUN="$work" \
  ENVOY_LISTEN_FD=3 ENVOY_API_TOKEN="$(cat "$work/envoy-token")" \
  ENVOY_MACHINE_ID="clean-session-stop-$$" ENVOY_LISTEN_HOST=127.0.0.1 NATS_URLS="$nats_url" \
  ENVOY_SESSION_LIVE_WINDOW="$live_window" ENVOY_SESSION_GONE_HORIZON="$gone_horizon" \
  ENVOY_SESSION_REAP_INTERVAL="$reap_interval" "$work/bin/envoy-listener"
await_start listener "$listener_pid" 60 "the Envoy listener to serve" serving "$listener_port"
note "the listener on 127.0.0.1:$listener_port judges records with a ${live_window} live window and a ${gone_horizon} gone horizon, against sessions that heartbeat every ${heartbeat_ms}ms"

database=clean_session_stop_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null ||
  fail "could not create database $database on LEGION_TEST_PG_DSN's server"
run_database_url database_url LEGION_TEST_PG_DSN "$pg_dsn" "$database" \
  "the run creates and drops a database there" || fail "$database_url"
# That string holds spaces and single quotes, so the daemon's configuration file takes it as a
# single-quoted YAML scalar, in which a single quote is written twice.
daemon_dsn=${database_url//\'/\'\'}
note "the daemon's store is the run's own database $database"
echo "CHECK services: PASS"

# ---- the daemon ---------------------------------------------------------------------------------------
begin daemon
keep_port daemon_keeper daemon_port
# omp_invocation is left out of the file: it takes the `mise x <tool> -- omp` form alone, so the Oh
# My Pi under proof is named by LEGION_OMP_PATH in the daemon's own environment
# (omplaunch.ResolveInvocation), which the daemon resolves to its real path for every pane and for
# its own boot gate.
cat >"$work/legion.yaml" <<EOF
project: $project
port: $daemon_port
postgres_dsn: '$daemon_dsn'
state_dir: $state_dir
operator_token_file: $work/operator-token
envoy_url: http://127.0.0.1:$listener_port
envoy_token_file: $work/envoy-token
nats_urls:
  - $nats_url
probe_interval_seconds: 5
worker_backends:
  implementer: claude
runtime:
  tmux:
    pane_env:
      ANTHROPIC_BASE_URL: "$model_base_url"
      ENVOY_HEARTBEAT_MS: "$heartbeat_ms"
EOF
cat >"$work/role-prompt.md" <<'EOF'
You are a role in a Legion proof of the daemon's clean session stop. You have no work to do. Reply
to every message with the single word `ready`, run no tools, and wait.
EOF
# The Claude Code pane's own model route, written before the claim exists: the daemon makes the
# claim's Claude Code directory only if it is missing, and Claude Code reads settings.json there.
# Its apiKeyHelper prints the run's copy of the key, so the key reaches no argv and no environment.
claude_claim_dir=$state_dir/claude/legion-$ptoken-css-2-implementer
(
  umask 077
  mkdir -p "$claude_claim_dir"
  printf '#!/bin/sh\nexec cat %s\n' "$work/model-key" >"$work/claude-key-command"
  jq -n --arg helper "$work/claude-key-command" --arg model "$model_id" \
    '{apiKeyHelper: $helper, model: $model}' >"$claude_claim_dir/settings.json"
)
chmod 0700 "$work/claude-key-command"

start_kept daemon daemon_keeper LEGION_LISTEN_FD -- env -i PATH="$PATH" LEGION_E2E_RUN="$work" \
  LEGION_LISTEN_FD=3 HOME="$omp_home" TMPDIR="$work/tmp" OMP_PROFILE="$profile" \
  TMUX_TMPDIR="$work/tmux" XDG_STATE_HOME="$work/xdg" LEGION_OMP_PATH="$omp_bin" \
  LEGION_CLAUDE_PATH="$claude_bin" LEGION_BUN_PATH="$(command -v bun)" \
  LEGION_CLAUDE_PLUGIN_DIR="$work/claude-envoy" \
  "$work/bin/legion" start --config "$work/legion.yaml"
await_start daemon "$daemon_pid" 300 "the daemon to answer /healthz" \
  curl -fsS --max-time 5 "http://127.0.0.1:$daemon_port/healthz"
note "the daemon serves on 127.0.0.1:$daemon_port, its private tmux server is legion-$ptoken under $work/tmux"
note "$(jq -R -c 'fromjson? | select(.msg == "legion daemon resolved Claude Code for the roles worker_backends puts on claude") | {claude, plugin}' \
  "$evidence/logs/daemon.log" | sed -n 1p)"
echo "CHECK daemon: PASS"

# ---- 1. an Oh My Pi role, killed under its worker shim ---------------------------------------------------
# The clause itself, on the harness every role runs by default. The architect's pane is `legion
# worker-shim … -- omp --mode rpc …`; the kill takes the omp process alone, which is what a crashed
# Oh My Pi leaves the shim with.
check_clean_stop_of_an_omp_role() {
  local token session killed
  token=$(ensure_tree_root) ||
    refuse "the architect claim could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  wait_until 420 "the architect claim $token to be ready" claim_is "$token" '.state == "ready"' || return 1
  session=$(claim_field "$token" .session)
  [ -n "$session" ] && [ "$session" != null ] || refuse "the ready claim $token records no session" || return 1
  wait_until 120 "the architect's session to read live" state_is "$session" live || return 1
  note "claim $token is ready on session $session, pane $(claim_field "$token" .locator.tmux.pane)"
  killed=$(kill_the_agent "$token" omp) ||
    refuse "the architect pane runs no omp under a worker shim (its ancestry is above)" || return 1
  note "SIGKILLed the pane's omp process at $killed"
  gone_within "$session" "$killed" "$clean_stop_limit_seconds" || return 1
  # The daemon acted on this death, so the ten seconds above are its stop and not the horizon: the
  # listener's reaper could not have run inside ten seconds of a record whose horizon is ninety.
  wait_until 30 "the daemon to record its clean stop of $session" \
    clean_stop_recorded "$session" || return 1
  note "the daemon recorded the clean stop of $session: $stop_record"
}

# ---- 2. a Claude Code role, killed under its bridge ------------------------------------------------------
# The same clause for a role `worker_backends` puts on claude: the pane is `legion worker-shim …
# -- legion claude-bridge -- claude -p …`, and the kill takes the claude process. The daemon's
# supervision is the same for either harness, and the point of killing both is that the session a
# clean stop names is the one that harness registered.
check_clean_stop_of_a_claude_role() {
  local token session killed root
  root=$(ensure_tree_root) ||
    refuse "the tree's root architect could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  note "the tree's root claim is $root"
  token=$(spawn_claim CSS-2 implementer) ||
    refuse "the implementer claim could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  wait_until 600 "the implementer claim $token to be ready" claim_is "$token" '.state == "ready"' || return 1
  session=$(claim_field "$token" .session)
  [ -n "$session" ] && [ "$session" != null ] || refuse "the ready claim $token records no session" || return 1
  [ "$(claim_field "$token" .sessionFile)" != null ] ||
    refuse "the Claude Code claim $token registered no transcript" || return 1
  wait_until 120 "the implementer's session to read live" state_is "$session" live || return 1
  note "claim $token is ready on Claude Code session $session, transcript $(claim_field "$token" .sessionFile)"
  killed=$(kill_the_agent "$token" claude) ||
    refuse "the implementer pane runs no claude under a worker shim (its ancestry is above)" || return 1
  note "SIGKILLed the pane's claude process at $killed"
  gone_within "$session" "$killed" "$clean_stop_limit_seconds" || return 1
  wait_until 30 "the daemon to record its clean stop of $session" \
    clean_stop_recorded "$session" || return 1
  note "the daemon recorded the clean stop of $session: $stop_record"
}

# ---- the run -----------------------------------------------------------------------------------------
for name in "${selected[@]}"; do
  run_check "$name"
done
check=summary
ok=1
