#!/usr/bin/env bash
# restart-step.sh: one load step, on the stack's own processes, whose listener is killed in the
# middle of the window and started again, and the report of what the sessions then got (README.md,
# "The listener restart").
#
#   restart-step.sh <name> [--events 40] [--sessions 1000] [--requests 600] [--duration 300s]
#     [--out <dir>] [--warm-at 60] [--kill-at 120] [--down 30] [--backlog-per 3]
#     [--backlog-over 25s] [--warm-over 10s] [other run-step.sh flags]
#
# --warm-at, --kill-at and --down are whole seconds; --duration, --backlog-over and --warm-over are
# durations as Go reads them ("25s", "1m30s").
#
# The step is a run-step.sh step under consumer delivery, as every step is: every session asks for
# a consumer of its own. Counted from the window start:
#
#   --warm-at   one envelope per session (`loadgen backlog -per 1`), so every session has answered
#               a frame done and holds a resume point, as a session running for a while does. With
#               LOADTEST_NO_RESUME=1 this step is left out: no session holds a resume point, and
#               its registrations carry none.
#   --kill-at   the listener is SIGKILLed.
#   +2 s        the backlog: --backlog-per envelopes to each session over --backlog-over, so every
#               session has a backlog while the listener is down.
#   --down      after the kill, the listener is started again with the same binary, arguments and
#               environment (read from /proc/<pid>), its output appended to the stack's listener
#               log and its pid in the stack's pidfile, so `stack.sh stop` stops the new one. A
#               start that exits at once (on gVisor the killed listener's port can still be held:
#               "bind: address already in use") is started again after 1 s; the restart time is the
#               start that stayed up, and restart.json counts the attempts.
#
# The step itself fails by design: its sampler sees the listener's pid exit (summary.json
# `stack_exits`), and run-step.sh exits non-zero. It still writes summary.json, relay-frames.csv
# and TABLES.md, and this script then runs `loadgen restart-report`, which writes
# restart-report.json in the step directory and fails when a relay frame would reach a session's
# transcript twice. A step that failed for any other reason fails this script.
#
# The step directory holds, beside what a step writes: restart.json (the kill, the start that
# stayed up and its first answer, in Unix ns, and the start attempts), backlog.json and warm.json
# (the backlog runs' summaries) and restart-report.json.
#
# One run at a time per LOADTEST_ROOT: this script takes the root's lock (common.sh) and the
# run-step.sh it starts inherits it. A failure of this script ends that step, so the root is free
# for the next run instead of being held by a step that measures on alone.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths

[ $# -ge 1 ] || usage
name=$1
shift
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"

events=40 sessions=1000 requests=600 duration=300s
warm_at=60 kill_at=120 down=30 backlog_per=3 backlog_over=25s warm_over=10s
out=$ROOT/runs/$name
step_args=()
while [ $# -gt 0 ]; do
  flag=$1
  [ $# -ge 2 ] || die "$flag needs a value"
  value=$2
  shift 2
  case $flag in
    --events) events=$value ;;
    --sessions) sessions=$value ;;
    --requests) requests=$value ;;
    --duration) duration=$value ;;
    --out) out=$value ;;
    --warm-at) warm_at=$value ;;
    --kill-at) kill_at=$value ;;
    --down) down=$value ;;
    --backlog-per) backlog_per=$value ;;
    --backlog-over) backlog_over=$value ;;
    --warm-over) warm_over=$value ;;
    *) step_args+=("$flag" "$value") ;;
  esac
done
for seconds in warm_at kill_at down; do
  [[ ${!seconds} =~ ^[0-9]+$ ]] || die "--${seconds//_/-} is a whole number of seconds, not '${!seconds}'"
done
((warm_at < kill_at)) || die "--warm-at ($warm_at s) must come before --kill-at ($kill_at s)"

lock_root "$ROOT"
out=$(realpath -m -- "$out")
RUN=$ROOT/run
LOGS=$ROOT/logs
no_resume=false
if [ "${LOADTEST_NO_RESUME:-}" = 1 ]; then no_resume=true; fi

# backlog PER OVER OUT [ID-BASE]: publish PER envelopes to each session over OVER, as the stack's
# own loadgen, and keep the summary in OUT.
backlog() {
  local args=(-sessions "$sessions" -per "$1" -over "$2" -out "$3")
  if [ $# -ge 4 ]; then args+=(-id-base "$4"); fi
  "$stack" loadgen backlog "${args[@]}" >/dev/null
}

# sleep_until SECONDS: wait for that Unix time; a time already past waits not at all.
sleep_until() {
  local left=$(($1 - $(date +%s)))
  if ((left > 0)); then sleep "$left"; fi
}

# listener_wait_ended PID START: the listener at PID has left the process table, every thread of
# it. A zombie main thread is not enough: other threads can still hold the listener port.
listener_wait_ended() {
  local pid=$1 start=$2 rc=0
  proc_wait_ended "$pid" "$start" "$((EPOCHSECONDS + 60))" || rc=$?
  case $rc in
  0) ;;
  1) die "the killed listener (pid $pid) has not ended 60 s after its SIGKILL" ;;
  *) die "the killed listener's /proc entry (pid $pid) is there and could not be read, so nothing shows it ended" ;;
  esac
}

# kill_and_restart: SIGKILL the stack's listener, keep it down, start it again with the same
# binary, arguments and environment, and write restart.json. The killed listener is down once it
# has ended (listener_wait_ended): its main thread a zombie is not enough, since its other threads
# still hold its port until they too have gone. A listener not ended 60 s after its SIGKILL, or a
# /proc entry that is there and cannot be read, fails this script, naming it. Each start is read in
# this shell (proc_start_into), so a read that a low descriptor limit stops fails the script rather
# than pass for a listener that has ended; a listener whose start could not be read is not killed.
kill_and_restart() {
  local pidf=$RUN/listener.pid pid port token envs cmd
  pid=$(cat -- "$pidf")
  [ -r "/proc/$pid/environ" ] || die "the stack's listener (pid $pid) is not running; nothing to restart"
  mapfile -d '' envs <"/proc/$pid/environ"
  mapfile -d '' cmd <"/proc/$pid/cmdline"
  port=$(jq -er .ports.listener "$ROOT/stack.json")
  token=$(jq -er .listener_token "$ROOT/stack.json")
  local kill_ns first_ns restart_ns ready_ns attempts=0 new up start new_start rc
  proc_start_into start "$pid" || die "the start of the stack's listener (pid $pid) could not be read, so nothing could show it ended once killed; it is not killed"
  [ -n "$start" ] || die "the stack's listener (pid $pid) left the process table before this script killed it; nothing to restart"
  kill_ns=$(date +%s%N)
  # no-wait: listener_wait_ended below waits for it, every thread of it (common.sh).
  kill -KILL "$pid"
  listener_wait_ended "$pid" "$start"
  note "killed the listener (pid $pid); it stays down $down s"
  sleep "$down"
  first_ns=$(date +%s%N)
  while :; do
    attempts=$((attempts + 1))
    restart_ns=$(date +%s%N)
    rm -f -- "$pidf"
    # stack.sh spawn's own start, so the pidfile names the listener and the lock (descriptor 9) is
    # not inherited.
    # shellcheck disable=SC2016
    setsid -f env -i "${envs[@]}" /bin/sh -c 'echo "$$" >"$0.tmp" && mv -f -- "$0.tmp" "$0" && exec "$@"' \
      "$pidf" "${cmd[@]}" >>"$LOGS/listener.out" 2>&1 </dev/null 9>&-
    until [ -s "$pidf" ]; do sleep 0.02; done
    new=$(cat -- "$pidf")
    proc_start_into new_start "$new" || die "the start of the restarted listener (pid $new) could not be read"
    up=false
    while :; do
      rc=0
      proc_ended "$new" "$new_start" || rc=$?
      ((rc == 1)) || { ((rc == 0)) || die "the restarted listener's /proc entry (pid $new) is there and could not be read"; break; }
      if printf 'Authorization: Bearer %s' "$token" | curl -fsS --max-time 1 -H @- "http://127.0.0.1:$port/v1/sessions" >/dev/null 2>&1; then
        up=true
        break
      fi
      sleep 0.05
    done
    $up && break
    ((attempts < 120)) || die "the listener did not stay up in $attempts starts; see $LOGS/listener.out"
    sleep 1
  done
  ready_ns=$(date +%s%N)
  note "the listener answered again as pid $new after $attempts start(s)"
  jq -n --argjson o "$pid" --argjson n "$new" --argjson k "$kill_ns" --argjson f "$first_ns" \
    --argjson r "$restart_ns" --argjson a "$ready_ns" --argjson s "$attempts" \
    '{old_pid: $o, new_pid: $n, kill_ns: $k, first_start_ns: $f, restart_ns: $r, answered_ns: $a, starts: $s}' \
    >"$out/restart.json"
}

# step_loadgen: the pid of this step's `loadgen run`: the process below the run-step.sh this
# script started ($step) whose command is loadgen -- the binary, or a script of that name its
# interpreter runs -- with the arguments `run -name NAME -out OUT`, read in that process tree
# alone, so that no process of another run or session is taken for it whatever its command line.
# It prints nothing when none is there.
step_loadgen() {
  local walked=("$step") i=0 j file kid found=() args=()
  while ((i < ${#walked[@]})); do
    for file in "/proc/${walked[i]}"/task/*/children; do
      found=()
      { read -r -a found <"$file"; } 2>/dev/null || true
      for kid in "${found[@]}"; do
        walked+=("$kid")
        { mapfile -d '' args <"/proc/$kid/cmdline"; } 2>/dev/null || continue
        for ((j = 0; j < 2 && j < ${#args[@]}; j++)); do
          if [ "${args[j]##*/}" = loadgen ] && [ "${args[*]:j+1:5}" = "run -name $name -out $out" ]; then
            printf '%s\n' "$kid"
            return 0
          fi
        done
      done
    done
    i=$((i + 1))
  done
}

# end_step: on a failure of this script, end the step that is still running, so that it frees the
# root's lock instead of measuring on alone. SIGTERM goes to `loadgen run`, not to run-step.sh, so
# the step renders its tables, stops Dispatch and releases the lock as it does for any failed run:
# to the `loadgen run` below the step this script started, by its pid (step_loadgen), which the
# stop waits for, every thread of it, before it waits for the step itself. Whether the step still
# runs is its own state, by the pid and start this script recorded when it started it (proc_ended):
# a step that has exited and that this shell has not reaped yet is a zombie, which `kill -0` calls
# running, and which has nothing left to end. A step whose start could not be read is ended as one
# that runs, and so is one whose /proc entry is there and cannot be read.
end_step() {
  local rc=$? lg lg_start ended running=1
  if ((rc != 0)) && [ -n "${step:-}" ] && [ -n "${step_start:-}" ]; then
    running=0
    proc_ended "$step" "$step_start" || running=$?
  fi
  if ((rc != 0)) && [ -n "${step:-}" ] && ((running != 0)); then
    note "ending the step: this script failed with $rc"
    lg=$(step_loadgen)
    lg_start=""
    if [ -n "$lg" ] && ! proc_start_into lg_start "$lg"; then
      note "the start of the step's loadgen run (pid $lg) could not be read, so nothing tells it from another process that took its pid: it is sent nothing, and the step goes on to its own end"
    elif [ -z "$lg_start" ]; then
      note "no loadgen run of step $name runs below its run-step.sh (pid $step), so it is sent nothing, and the step goes on to its own end"
    fi
    if [ -n "$lg_start" ]; then
      # no-wait: proc_wait_ended below waits for it, every thread of it (common.sh), up to 120 s.
      kill -TERM "$lg" 2>/dev/null || true
      ended=0
      proc_wait_ended "$lg" "$lg_start" "$((EPOCHSECONDS + 120))" || ended=$?
      case $ended in
      0) ;;
      1) note "the step's loadgen run (pid $lg) has not ended 120 s after its SIGTERM; the wait goes on with the step itself" ;;
      *) note "the /proc entry of the step's loadgen run (pid $lg) is there and could not be read, so nothing shows it ended" ;;
      esac
    fi
    wait "$step" || true
  fi
}

started=$SECONDS
rc=0
"$here/run-step.sh" "$name" --events "$events" --sessions "$sessions" --requests "$requests" \
  --duration "$duration" --out "$out" "${step_args[@]}" &
step=$!
step_start=""
proc_start_into step_start "$step" || step_start=""
trap end_step EXIT

# The window opens once the sessions have registered; meta.json carries its start from then on.
until jq -e '.window_start | type == "string" and (startswith("0001") | not)' "$out/meta.json" >/dev/null 2>&1; do
  kill -0 "$step" 2>/dev/null || die "the step ended before its window opened; see $out/run.log"
  sleep 1
done
window_start=$(date -d "$(jq -er .window_start "$out/meta.json")" +%s)
note "window starts $(date -u -d "@$window_start" +%FT%TZ); kill at $(date -u -d "@$((window_start + kill_at))" +%FT%TZ)"

if $no_resume; then
  note "LOADTEST_NO_RESUME=1: no warm envelope, so no session holds a resume point"
else
  sleep_until $((window_start + warm_at))
  backlog 1 "$warm_over" "$out/warm.json" 910000000
  note "every session has answered one envelope done"
fi

sleep_until $((window_start + kill_at))
kill_and_restart &
restart=$!
sleep 2
backlog "$backlog_per" "$backlog_over" "$out/backlog.json"
wait "$restart" || die "the listener was not restarted (above); the step is still running, see $out/run.log"
wait "$step" || rc=$?
if ((rc != 0)); then
  # The step fails by design: the sampler sees the listener exit. Any other failure is this run's.
  [ -f "$out/summary.json" ] || die "the step exited $rc before it wrote summary.json; see $out/run.log"
  jq -e '[(.stack_exits // [])[].name] | length > 0 and all(. == "listener")' "$out/summary.json" >/dev/null ||
    die "the step exited $rc for something other than the listener this script killed: stack_exits $(jq -c '.stack_exits // []' "$out/summary.json"); see $out/run.log"
fi
"$stack" loadgen restart-report -run "$out"
note "restart step $name done in $((SECONDS - started)) s: $out"
