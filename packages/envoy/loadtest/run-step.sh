#!/usr/bin/env bash
# run-step.sh: the one command for a named load step (README.md).
#
#   run-step.sh <name> --events <per s> --sessions <n> --requests <per s>
#     [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>]
#     [--route-mix <file>] [--human 0.1] [--live <n>] [--rng-seed <n>] [--xact-interval 10ms]
#     [--heap-samples] [--schedule <factor>x:<duration>,...] [--recovery-ref-p99 <ms>]
#     [--out <dir>] [--live-resume 0|1] [--live-stale <n>] [--restart-at <duration>]
#     [--mention-frac <0..1>] [--mentions <n>] [--envoy-rtt <duration>]
#     [--editors 0] [--editor-rate 2] [--editor-docs 10]
#
# --schedule varies the load over the window: each factor multiplies --events and --requests for its
# duration, and the window is the schedule's total (a --duration given must equal it). Both flags go
# to `loadgen run` as -schedule and -recovery-ref-p99 (README.md, "Load schedule and recovery").
#
#   run-step.sh <name> --replay [--rate F] [--reads RHO] [--read-mix M] [--uploads U]
#     [--rng-seed N] [--from D --to D] [--roster N] [--keys-file FILE] [--warmup D]
#     [--gomaxprocs N] [--dispatch-commit REV] [--manifest FILE] [--heap-samples] [--out <dir>]
#
# From a fresh checkout it sets up, starts and seeds the stack (stack.sh). Then, every step that is
# not part of a series: it rebuilds the binaries from this checkout (stack.sh setup, incremental) and
# refuses a build whose highest migration is older than the seed's. A step of run-series.sh skips
# that: the series did it once, before its first step, and passes its build's commit and dirty flag
# in LOADTEST_SERIES_BUILD, which a run-step.sh that does not hold the series' lock (common.sh)
# ignores. Every step then resets the stack to the seed (stack.sh reset: database recreated from
# dispatch_seed, NATS on a fresh store, listener and Dispatch restarted), runs `loadgen run` and
# renders `loadgen tables` in the step directory, $LOADTEST_ROOT/runs/<name>/ unless --out names
# another (it must not exist or be empty). Dispatch's log is the step's dispatch.log. --envoy-rtt
# starts Dispatch with ENVOY_URL through a latency proxy of that round trip in front of the
# listener (stack.sh LOADTEST_ENVOY_RTT); every other flag goes to `loadgen run`.
#
# Every flag takes a value but --replay and --heap-samples, which take none. --heap-samples passes
# -heap-samples to `loadgen run`: the sampler then reads Dispatch's heap profile every 15 s inside
# the window. It is off by default, since Dispatch writes that profile itself, so the read is
# charged to the CPU the step measures (README.md, "Why the harness reads no heap profile in the
# window"); without it the heap columns of the tables show -.
#
# A --replay step replays the LINUX bundle (README.md, The replay) instead: stack.sh setup
# (incremental; with --dispatch-commit, envoy-dispatch from that commit, a rev that names no commit
# refused before anything starts), start, replay-get (the manifest's parts, default
# replay-2026-10-26.json; nothing to do once done; LOADTEST_REPLAY_ISSUE names the issue they are
# file artifacts of and is required, refused here before anything starts), replay-prepare (this
# build's template, once per build) and reset --replay, then `loadgen run -source replay` with the
# -replay-* flags: --rate, --reads, --read-mix, --uploads, --from, --to, --roster and --keys-file
# are -replay-rate, -replay-reads, -replay-read-mix, -replay-uploads, -replay-from, -replay-to,
# -replay-roster and -replay-keys-file. --events, --sessions, --requests and the other flags of
# the generated load are refused; the synthetic seed and its migration check are not used.
# Dispatch reaches Postgres directly (LOADTEST_PG_DIRECT=1) unless the environment sets
# LOADTEST_PG_DIRECT; --gomaxprocs sets LOADTEST_GOMAXPROCS. The reset's replay/last-reset.json is
# copied into the step directory.
#
# Dispatch (and the listener's latency proxy) are stopped when the step ends, pass or fail, so the
# box is idle between steps; Postgres, the proxy, NATS and the listener keep running (stack.sh stop
# stops them).
#
# One run at a time per LOADTEST_ROOT (common.sh): a run-step.sh or run-series.sh on a root that
# another run holds is refused before it touches the stack. A step in which a stack process exited,
# or whose commit rows (public.loadtest_event_commits) could not be read, fails: loadgen run exits
# non-zero naming why, and so does this script; it still renders TABLES.md, where the row carries
# the mark.
#
# A step that never opens its window leaves a record of that (README.md, "Run one step"): every
# exit of this script before the window, whatever refuses it (a flag, the lock another run holds, a
# step directory that already holds a step, the CPU checks, the build, the migration check, the
# quiet-box gate, the reset, the GOMAXPROCS refusal, `loadgen run` ending before the window, a
# signal), writes <step dir>.not-run.json beside the step directory (common.sh not_run_record): the
# step's name and directory, the exit status, the refusal's own message and the last 40 lines the
# step wrote. An attempt removes the record an earlier one left as it starts, and one that opens
# its window leaves none. Only a call with no step name, or with a name that is not one, writes no
# record.
#
# The quiet-box gate (common.sh wait_for_quiet_box; README.md, "A quiet box before each step"):
# after the lock and the stack, while Dispatch is stopped, the step reads how busy the box is with
# `loadgen box-busy`, the measure of meta.json's box_busy_before. Above LOADTEST_MAX_BUSY_BEFORE
# (default 0.10) it waits for the box to go quiet, reading it every 30 s until its pauses add up to
# LOADTEST_QUIET_WAIT seconds (default 900; each reading takes 5 s besides), and starts on the first
# reading at or under the bar; `loadgen run` gets
# every reading and the bar (-box-busy-gate, -box-busy-gate-max), which meta.json keeps beside the
# step's own box_busy_before, read after the gate as before. A box still busy past the wait refuses
# the step, and its not-run record carries every reading (box_busy_gate_readings), the bar
# (box_busy_gate_max) and the wait (box_busy_gate_wait_s).
#
# A step whose generator sent more than a tenth over its schedule in a second of the window, or
# recorded nothing scheduled of a kind it was to send, is set aside (README.md, "The set-aside
# rule"): neither a pass nor a failure. This script says so first in its closing lines, and
# TABLES.md says so before its first table; it exits 0 for it, as for a step that ran, so a series
# goes on to its next step.
#
# LOADTEST_SERVER_CPUS and LOADTEST_GENERATOR_CPUS (common.sh, CPU pinning) pin the stack and
# loadgen to two CPU lists. Before anything starts, a list taskset refuses, two lists that overlap,
# a generator list that with the server list asks for more CPUs than the pod's CPU quota, a
# LOADTEST_GOMAXPROCS (--gomaxprocs) other than the server list's CPU count and a runtime that
# does not enforce the pin (gVisor) are refused; beside the server list under a quota, a generator
# list that is unset or `auto` becomes the CPUs the quota leaves it, which this script names and
# exports (common.sh generator_cpus). Dispatch's GOMAXPROCS is the server list's CPU
# count when the list is set (common.sh gomaxprocs); meta.json records the GOMAXPROCS the running
# Dispatch was started with, read back from its environment, and a step where it is not the list's
# count is refused before its window opens (loadgen run), which this script says first.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# The same path under a name of finish's own (below): prepare_stack (common.sh) has a local stack.
not_run_stack=$stack
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths
# Dispatch reaches the listener directly unless --envoy-rtt says otherwise, whatever this shell
# holds.
export LOADTEST_ENVOY_RTT=

[ $# -ge 1 ] || usage
name=$1
shift
case $name in
  -h | --help) usage ;;
  -* | "") die "the step name comes first, before the flags, not '$name'" ;;
esac
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"

replay=false
for flag in "$@"; do
  [ "$flag" = --replay ] && replay=true
done

# The step directory: the last --out's value (the loop below takes no other), read before any flag
# is checked, so that the record of a step refused for a flag lies beside the directory it names.
out=$ROOT/runs/$name
for ((i = 1; i <= $#; i++)); do
  case ${!i} in
    --replay | --heap-samples) ;;
    --out)
      i=$((i + 1))
      if ((i <= $#)) && [ -n "${!i}" ]; then out=${!i}; fi
      ;;
    *) i=$((i + 1)) ;;
  esac
done
out=$(realpath -m -- "$out")

# The not-run record of this attempt (finish, below): an earlier attempt's goes now. Everything this
# script and what it runs write goes to its stdout and stderr through two tees, which append it to
# the attempt's output file too, and every die, here or in a stack.sh it starts, keeps its message
# in LOADTEST_DIE_FILE (common.sh). The tees ignore the signals that stop a step, so that its last
# lines still reach the record, and close descriptor 9, so that they never hold the root's lock
# that a series step inherits. finish reads only not_run_* globals, the quiet-box gate's QUIET_*
# (common.sh) and LOADTEST_DIE_FILE: bash runs it in the scope of the function that exited, whose
# locals (lock_root's record, prepare_stack's stack) would hide other names.
not_run_name=$name not_run_dir=$out not_run_file=$(not_run_record "$out")
rm -f -- "$not_run_file"
not_run_attempt=$(mktemp -d)
export LOADTEST_DIE_FILE=$not_run_attempt/refusal
exec > >(trap '' INT TERM HUP && exec tee -a -- "$not_run_attempt/output" 9>&-)
not_run_tee_out=$!
exec 2> >(trap '' INT TERM HUP && exec tee -a -- "$not_run_attempt/output" >&2 9>&-)
not_run_tee_err=$!
# not_run_step_ours: the step directory held no step when this run checked it, so a window its
# meta.json names is this run's. not_run_dispatch_up: the reset has started Dispatch, which the step
# stops as it ends.
not_run_step_ours=false not_run_dispatch_up=false

# finish, on every exit: stop Dispatch and the listener's latency proxy when the reset started them;
# then, unless this run's window opened, write the not-run record: the step's name and directory,
# the exit status (null for a signal, which leaves 0 here and no refusal), the refusal's own
# message (the last die's, else what the status says), once the quiet-box gate took a reading its
# readings, bar and wait, and the last 40 lines the step wrote. It writes no line itself, so a
# refused step still closes on its refusal. A run whose window opened removes any record of its
# step instead, such as one a run refused at the lock wrote meanwhile.
finish() {
  local status=$? refusal="" opened=false exit_status=null reason lines i gate='{}'
  set +e
  # Read before the stop below, whose own refusal would take its place.
  [ ! -s "$LOADTEST_DIE_FILE" ] || refusal=$(<"$LOADTEST_DIE_FILE")
  if $not_run_dispatch_up; then "$not_run_stack" stop dispatch envoy_proxy; fi
  if $not_run_step_ours && window_start "$not_run_dir" >/dev/null; then opened=true; fi
  # Close this shell's ends of the tees' pipes, and give the tees 5 s to write out what they hold.
  exec >/dev/null 2>&1
  for ((i = 0; i < 100; i++)); do
    lock_pid_alive "$not_run_tee_out" || lock_pid_alive "$not_run_tee_err" || break
    sleep 0.05
  done
  if $opened; then
    rm -f -- "$not_run_file"
  else
    # Every exit of this script before its window is non-zero but one by a signal, for which bash
    # runs this handler with the status of the last command that ended (0, as a rule), then ends on
    # the signal: its exit status is not known here.
    if ((status == 0)); then
      reason="${0##*/} was stopped by a signal (SIGTERM, SIGINT or SIGHUP) before the window opened"
    elif [ -n "$refusal" ]; then
      exit_status=$status reason=$refusal
    else
      exit_status=$status
      reason="${0##*/} exited $status before the window opened, with no refusal of its own: a command it ran failed (last_lines)"
    fi
    lines=$(tail -n 40 -- "$not_run_attempt/output")
    # Each reading and the bar are JSON numbers (common.sh quiet_fraction), the wait whole seconds.
    ((${#QUIET_READINGS[@]} == 0)) ||
      gate="{\"box_busy_gate_readings\":[$(quiet_readings ,)],\"box_busy_gate_max\":$QUIET_BAR,\"box_busy_gate_wait_s\":$QUIET_WAIT}"
    mkdir -p -- "$(dirname -- "$not_run_file")" &&
      jq -n --arg name "$not_run_name" --arg dir "$not_run_dir" --argjson status "$exit_status" \
        --arg reason "$reason" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson gate "$gate" --arg lines "$lines" \
        '{name: $name, step_dir: $dir, exit_status: $status, reason: $reason, at: $at} + $gate +
          {last_lines: (if $lines == "" then [] else ($lines | split("\n")) end)}' >"$not_run_file.tmp" &&
      mv -f -- "$not_run_file.tmp" "$not_run_file"
  fi
  rm -rf -- "$not_run_attempt"
  exit "$status"
}
trap finish EXIT

args=()
have_events=false have_sessions=false have_requests=false
manifest="" gomaxprocs="" dispatch_rev=""
while [ $# -gt 0 ]; do
  flag=$1
  if [ "$flag" = --replay ]; then
    shift
    continue
  fi
  # A switch, not a flag with a value: loadgen run takes it as a Go bool flag.
  if [ "$flag" = --heap-samples ]; then
    args+=(-heap-samples)
    shift
    continue
  fi
  [ $# -ge 2 ] || die "$flag needs a value"
  value=$2
  shift 2
  if [ "$flag" = --out ]; then
    # Read above, before the flags were checked.
    [ -n "$value" ] || die "--out needs a directory, not ''"
    continue
  fi
  if $replay; then
    case $flag in
      --events | --sessions | --requests | --duration | --grace | --event-mix | --route-mix | --human | --live | --editors | --editor-rate | --editor-docs)
        die "$flag is not a replay flag: a --replay step sends the bundle's logged writes"
        ;;
      --rate | --reads | --read-mix | --uploads | --from | --to | --roster) flag=--replay-${flag#--} ;;
      --keys-file)
        flag=--replay-keys-file
        value=$(abs_file --keys-file "$value")
        ;;
      --warmup | --rng-seed) ;;
      --gomaxprocs)
        [[ $value =~ ^[1-9][0-9]*$ ]] || die "--gomaxprocs '$value' is not a positive integer"
        gomaxprocs=$value
        continue
        ;;
      --dispatch-commit)
        dispatch_rev=$value
        continue
        ;;
      --manifest)
        manifest=$(abs_file --manifest "$value")
        continue
        ;;
      *) die "unknown flag '$flag'" ;;
    esac
  else
    case $flag in
      --events) have_events=true ;;
      --sessions) have_sessions=true ;;
      --requests) have_requests=true ;;
      --duration | --warmup | --grace | --event-mix | --human | --live | --rng-seed | --xact-interval | --schedule | --recovery-ref-p99 | --live-stale | --restart-at | --mention-frac | --mentions) ;;
      --editors | --editor-rate | --editor-docs) ;;
      --live-resume)
        case $value in
          1) args+=(-live-resume) ;;
          0) ;;
          *) die "--live-resume takes 1 or 0, not '$value'" ;;
        esac
        continue
        ;;
      --envoy-rtt)
        [[ $value =~ ^[0-9]+(\.[0-9]+)?(ns|us|ms|s|m)$ ]] || die "--envoy-rtt takes a duration such as 200ms, not '$value'"
        LOADTEST_ENVOY_RTT=$value
        continue
        ;;
      --route-mix) value=$(abs_file --route-mix "$value") ;;
      *) die "unknown flag '$flag'" ;;
    esac
  fi
  args+=("-${flag#--}" "$value")
done
if ! $replay; then
  $have_events || die "--events is required (0 sends no writes)"
  $have_sessions || die "--sessions is required"
  $have_requests || die "--requests is required (0 sends no requests)"
fi
if [ -n "$dispatch_rev" ]; then
  dispatch_commit=$(git -C "$SRC" rev-parse --verify --quiet "$dispatch_rev^{commit}" 2>/dev/null) ||
    die "--dispatch-commit '$dispatch_rev' names no commit in $SRC"
fi
if $replay; then
  # stack.sh replay-get takes the issue the replay's parts are file artifacts of from the
  # environment, as the manifest names none; refuse here too, before the step touches the stack.
  [ -n "${LOADTEST_REPLAY_ISSUE:-}" ] ||
    die "LOADTEST_REPLAY_ISSUE is required for a --replay step: set it to the issue the replay's parts are file artifacts of"
fi
[ -z "$gomaxprocs" ] || export LOADTEST_GOMAXPROCS=$gomaxprocs
check_cpu_pinning

lock_root "$ROOT"

if [ -e "$out" ] && [ -n "$(ls -A -- "$out")" ]; then
  die "$out already holds a step; remove it or name another step"
fi
# From here the step directory is this run's: a window its meta.json names is this run's (finish).
not_run_step_ours=true

started=$SECONDS
if $replay; then
  # The caller's LOADTEST_PG_DIRECT wins; meta.json's pg_direct records what Dispatch used.
  export LOADTEST_PG_DIRECT=${LOADTEST_PG_DIRECT-1}
  [ -z "$dispatch_rev" ] || export LOADTEST_DISPATCH_COMMIT=$dispatch_commit
  get_args=()
  [ -z "$manifest" ] || get_args=(--manifest "$manifest")
  "$stack" setup
  "$stack" start
  "$stack" replay-get "${get_args[@]}"
  "$stack" replay-prepare
  BUILD=$("$stack" build-info)
elif $LOCK_INHERITED && [ -n "${LOADTEST_SERIES_BUILD:-}" ]; then
  # A step of run-series.sh, which holds the lock this script inherited: the series set up, started
  # and seeded the stack once, and every step runs that build.
  BUILD=$LOADTEST_SERIES_BUILD
else
  unset LOADTEST_SERIES_BUILD
  prepare_stack
fi
read -r commit dirty <<<"$BUILD"

# The quiet-box gate (common.sh wait_for_quiet_box), after the lock and the stack and while Dispatch
# is stopped: a box above LOADTEST_MAX_BUSY_BEFORE is waited for until the pauses add up to
# LOADTEST_QUIET_WAIT (each reading takes 5 s besides), and
# past that the step is refused, its not-run record carrying every reading and the bar (finish).
# `loadgen run` gets the readings and the bar, for meta.json.
wait_for_quiet_box
gate_args=(-box-busy-gate "$(quiet_readings ,)" -box-busy-gate-max "$QUIET_BAR")

mkdir -p -- "$out"
# The box's busy share before this step starts anything: Dispatch is stopped (the last step stopped
# it) and the rest of the stack is idle, so what it shows is other work on the machine. Read after
# the reset, it would show Dispatch's own start-up work instead. It stays the step's own reading,
# taken after the gate's, and meta.json keeps the two side by side.
box_busy=$(box_busy_reading)
# finish stops Dispatch and the latency proxy on any exit from here to their stop below.
not_run_dispatch_up=true
if $replay; then
  "$stack" reset --replay "$out/dispatch.log"
  cp -- "$ROOT/replay/last-reset.json" "$out/last-reset.json"
  args=(-source replay -events 0 -requests 0 -replay-inputs "$ROOT/replay/bundle" "${args[@]}")
else
  "$stack" reset "$out/dispatch.log"
fi
rc=0
"$stack" loadgen run -name "$name" -out "$out" -build-commit "$commit" -build-dirty="$dirty" -box-busy-before "$box_busy" "${gate_args[@]}" "${args[@]}" || rc=$?
"$stack" stop dispatch envoy_proxy
not_run_dispatch_up=false
cp -- "$ROOT/logs/listener.out" "$out/listener.log"
cp -- "$ROOT/logs/nats.out" "$out/nats.log"
[ -z "$LOADTEST_ENVOY_RTT" ] || cp -- "$ROOT/logs/envoy-proxy.out" "$out/envoy-proxy.log"

# set_aside_note: when summary.json sets the step aside, say so before any other closing line.
set_aside_note() {
  local why
  why=$(jq -er '.set_aside.why // empty' "$out/summary.json" 2>/dev/null) || return 0
  note "STEP $name IS SET ASIDE, neither a pass nor a failure: $why. Its rows describe the test bed, not Dispatch (summary.json set_aside, $out/arrivals.json); run it again where the generator keeps, and records, its schedule"
}

if ((rc != 0)); then
  # loadgen run printed why above (a Dispatch whose GOMAXPROCS is not the server list's CPU count,
  # refused before the window; a stack process that exited during the step, commit rows it could
  # not read, or its error).
  if why=$(gomaxprocs_refusal "$out/meta.json"); then
    die "STEP $name IS REFUSED before its window opened, so nothing was measured: $why. stack.sh starts Dispatch with GOMAXPROCS at the list's CPU count unless a GOMAXPROCS line of LOADTEST_DISPATCH_ENV says otherwise (see $out/meta.json and $out/run.log)"
  fi
  [ -f "$out/summary.json" ] || die "step $name failed: loadgen run exited $rc before it wrote summary.json (its message above says why); see $out/run.log"
  "$stack" loadgen tables "$out" || true
  set_aside_note
  die "step $name failed: loadgen run exited $rc (its message above says why); $out/TABLES.md holds the step's row for diagnosis; see $out/run.log"
fi
"$stack" loadgen tables "$out"
set_aside_note
note "step $name done in $((SECONDS - started)) s: $out"
