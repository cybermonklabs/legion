#!/usr/bin/env bash
# The measurement's supervisor: it runs a process of the measurement (the server, Postgres) in a
# container of its own at the driver's request, so each runs held to the CPU and memory the pod
# gives that container, while the driver (job.sh and run.sh, in a container of their own) builds,
# loads the fixture and measures (job.yaml). The driver and the supervisors share a directory on
# the pod's volume, one directory beneath it per supervised container, and speak through files.
#
#   supervise.sh serve <dir>                the supervisor, the command of the server's and
#                                           Postgres's containers (job.yaml); job.sh starts one
#                                           beside itself for each where nothing else runs them
#   supervise.sh exec <dir> <log> <cmd>...  runs cmd as the supervisor's child, in the supervisor's
#                                           container, its output appended to log, and stands in for
#                                           it until it exits: TERM, INT, HUP, QUIT, USR1 or USR2
#                                           sent to the stand-in goes to cmd, and the stand-in exits
#                                           with cmd's status (128 + n where signal n ended it)
#   supervise.sh signal <dir> <signal>      sends the supervisor's child a signal, such as STOP or
#                                           CONT, which no stand-in can pass on
#
# The files of <dir>, each written whole under a hidden name and renamed into place, so no reader
# meets half of one:
#   command    the script the next child runs, which exec writes
#   start      the request to start it, which exec writes after command; the supervisor removes
#              it, runs `bash command` as its one child, in a process group of its own, and writes
#              pid
#   pid        the running child's pid, removed when it exits
#   exit       the child's exit status, written when it exits; exec removes it before it asks
#   signal.<microseconds>.<pid>   the name of a signal for the child, one request a file, named
#              by when it was asked, so signals asked one after the other (CONT, then TERM) reach
#              the child in that order; the supervisor sends each and removes it, and exec removes
#              any left from an earlier child before it asks
#   container  what the kernel holds the supervisor's container to, written when the supervisor
#              starts: the CPUs its processes may run on (cpus), its cgroup's CPU quota in
#              millicores (cpu_limit_m) and its memory limit in MiB (memory_limit_mi), none where
#              the cgroup sets no limit; job.sh prints it in its SHAPE line
#
# A supervisor serves one driver. Whoever starts the supervisors holds the driver lock,
# driver.lock beside <dir>, for as long as they serve it: the pod's driver container (job.yaml), or
# job.sh where it starts them itself. A supervisor ends once it can take that lock, so once its
# driver has exited however it exited, or on TERM; either way it first stops a running child with
# TERM and waits for it.
set -euo pipefail

usage() {
  echo "usage: supervise.sh serve <dir> | supervise.sh exec <dir> <log> <command>... | supervise.sh signal <dir> <signal>" >&2
  exit 2
}

# put writes the file $1 whole, holding $2.
put() {
  local hidden=${1%/*}/.$$.${1##*/}
  printf '%s\n' "$2" >"$hidden"
  mv -f "$hidden" "$1"
}

# send asks the supervisor of directory $1 to send its child the signal $2.
send() {
  put "$1/signal.${EPOCHREALTIME/./}.$$" "$2"
}

# limits prints what the kernel holds this container to (the header's container file), from the
# cgroup v2 files or, where there are none, from cgroup v1's.
limits() {
  local cpu=none memory=none quota period bytes
  if [ -r /sys/fs/cgroup/cpu.max ]; then
    read -r quota period </sys/fs/cgroup/cpu.max
    bytes=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo max)
  else
    quota=$(cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us 2>/dev/null || echo -1)
    period=$(cat /sys/fs/cgroup/cpu/cpu.cfs_period_us 2>/dev/null || echo 100000)
    bytes=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo max)
  fi
  case "$quota" in '' | max | -*) ;; *) cpu=$((quota * 1000 / period)) ;; esac
  # cgroup v1 writes a huge number where no limit is set: more than 2^60 bytes is none.
  case "$bytes" in '' | max | *[!0-9]*) ;; *) [ "${#bytes}" -ge 19 ] || memory=$((bytes / 1048576)) ;; esac
  echo "cpus=$(nproc) cpu_limit_m=$cpu memory_limit_mi=$memory"
}

serve() {
  local dir=$1 lock child="" lockfd="" next=0 code sig request
  lock=$(dirname "$dir")/driver.lock
  mkdir -p "$dir"
  put "$dir/container" "$(limits)"
  # Each child runs in a process group of its own (job control), so a child that is stopped
  # leaves no orphaned group the kernel hangs up, and a child takes INT as any process does.
  set -m
  # reap waits for the child, which has exited or been asked to, and writes its status: pid goes
  # first, so a stand-in that returns on exit finds no pid when it asks again.
  reap() {
    code=0
    wait "$child" || code=$?
    rm -f "$dir/pid"
    put "$dir/exit" "$code"
    child=""
  }
  stop() {
    if [ -n "$child" ]; then
      kill -s TERM "$child" 2>/dev/null || true
      kill -s CONT "$child" 2>/dev/null || true
      reap
    fi
    exit 0
  }
  trap stop TERM
  while :; do
    if [ -e "$dir/start" ]; then
      rm -f "$dir/start" "$dir/exit"
      if [ -n "$child" ]; then
        echo "supervise.sh: $dir: a start while pid $child runs is refused" >&2
      else
        bash "$dir/command" &
        child=$!
        put "$dir/pid" "$child"
      fi
    fi
    for request in "$dir"/signal.*; do
      [ -e "$request" ] || continue
      sig=$(<"$request")
      rm -f "$request"
      if [ -n "$child" ]; then
        kill -s "$sig" "$child" 2>/dev/null || true
      fi
    done
    if [ -n "$child" ] && ! kill -0 "$child" 2>/dev/null; then
      reap
    fi
    if [ "$SECONDS" -ge "$next" ]; then
      next=$((SECONDS + 1))
      if [ -z "$lockfd" ] && [ -e "$lock" ]; then
        exec {lockfd}<"$lock"
      fi
      if [ -n "$lockfd" ] && flock -n "$lockfd"; then
        stop
      fi
    fi
    sleep 0.1
  done
}

stand_in() {
  local dir=$1 log=$2 started=$SECONDS wait=${SUPERVISE_WAIT:-300} signal
  shift 2
  if [ -e "$dir/pid" ]; then
    echo "supervise.sh: $dir runs pid $(<"$dir/pid") already" >&2
    return 2
  fi
  # A request left from an earlier child is no request of this one's. The directory may come
  # before its supervisor does, which then finds the request waiting.
  mkdir -p "$dir"
  rm -f "$dir/exit" "$dir"/signal.*
  put "$dir/command" "exec $(printf '%q ' "$@")>>$(printf '%q' "$log") 2>&1"
  for signal in TERM INT HUP QUIT USR1 USR2; do
    # shellcheck disable=SC2064 # the trap names this signal and this directory now
    trap "send $(printf '%q' "$dir") $signal" "$signal"
  done
  put "$dir/start" ""
  until [ -e "$dir/exit" ]; do
    if [ -e "$dir/start" ] && [ $((SECONDS - started)) -ge "$wait" ]; then
      rm -f "$dir/start"
      echo "supervise.sh: no supervisor in $dir took the request within $wait s" >&2
      return 2
    fi
    sleep 0.1
  done
  return "$(<"$dir/exit")"
}

[ $# -ge 2 ] || usage
verb=$1 dir=$2
shift 2
case "$verb" in
  serve)
    [ $# -eq 0 ] || usage
    serve "$dir"
    ;;
  exec)
    [ $# -ge 2 ] || usage
    stand_in "$dir" "$@"
    ;;
  signal)
    [ $# -eq 1 ] || usage
    [ -e "$dir/pid" ] || {
      echo "supervise.sh: $dir runs no child" >&2
      exit 1
    }
    send "$dir" "$1"
    ;;
  *) usage ;;
esac
