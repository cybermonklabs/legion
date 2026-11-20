# shellcheck shell=bash
# test-lib.sh: what the load test's suites (vm/vm.test.sh, cluster/cluster.test.sh, common.test.sh
# and restart-step.test.sh) share about a process a case started and has to see end or hold still.
# They source it before they put a fake sleep first on PATH. Not executable, and no part of what
# the scripts it tests run. It sources common.sh, the drivers' own library, for the one reading of
# a stop they share (proc_threads_stopped); nothing of the drivers' sources this file.
#
# process_ended PID: the process PID names has ended: its /proc entry is gone, its main thread is a
# zombie and its task directory lists no thread but the main one, or it reads dead (X), its parent
# reaping it, from the moment the parent claims it until its entry goes. A zombie main thread with
# another thread listed is a process still ending, whose threads still hold its files, its ports
# and its locks, so it has not ended. An entry there that cannot be read has not ended either, as
# far as a case can tell. It reads /proc with builtins alone, so a case can ask it at the moment a
# stop returns. It is the suites' own reading of an end, kept apart from the drivers' proc_ended,
# so that a case does not judge a driver by the driver's own reader.
#
# wait_gone PID [SECONDS]: waited until PID has ended (process_ended), for SECONDS (10 by default)
# on the clock at most. It returns 1 when it has not, and the caller says so. A case's stand-in that
# outlives the shell that started it is reaped by no wait of the case's, so it is waited for in the
# table rather than by wait.
#
# end_process PID [SECONDS]: PID sent SIGKILL, and waited until it has ended (wait_gone).
#
# stop_process PID [SECONDS]: PID sent SIGSTOP, and waited until every thread of it reads stopped
# (T or t) or ended (Z or X: a thread that has ended forks nothing): common.sh's
# proc_threads_stopped, the drivers' reading too. It waits SECONDS (10 by default) at most, and
# returns 1 when a thread still runs then, or when its entry cannot be read. A signal is delivered
# when the kernel next runs its process, so a parent sent SIGSTOP that is not run before its child
# is killed reaps that child at once, from the wait it is in, and only then stops: a case that
# holds a dead child in the table under a stopped parent waits for the stop first. A child that
# ends after its parent is stopped stays a zombie, which never reads T, so a zombie counts as
# stopped.
#
# process_start PID: the process's start time (field 22 of /proc/PID/stat), or nothing when its
# entry cannot be read, so a pid the kernel gave another process since is told apart.
#
# process_state PID: the state letter of its main thread (field 3 of /proc/PID/stat: R, S, T, Z
# and so on), or nothing when its entry cannot be read.
#
# record_pid FILE PID: "PID START" appended to FILE, the record of a process a suite started that
# may outlive the case that started it -- a fake that runs in the background, a stand-in, a step
# started with & -- for end_recorded. A pid that has already gone is not recorded.
#
# end_recorded FILE: each process FILE records that is still the process recorded (its start the
# same) ended with every process below it (end_tree). A suite's exit ends what its cases left
# this way, by the pids they recorded, never by a pattern of command lines, which would reach
# another run's processes as well. It returns 1 when end_tree did, whose lines name each process.
#
# end_tree PID [SECONDS]: PID and every process below it ended: each sent SIGSTOP and seen stopped
# (stop_process) before its children are read, so that none starts one the reading misses, then
# all of them SIGKILL, and each waited until it has ended (wait_gone). The walk has one deadline,
# SECONDS (10 by default) from its start, and the waits after the SIGKILL one more, SECONDS from
# it: a tree of many processes the walk cannot see stopped, or that do not end, holds it no
# longer than one. Every process is walked: one that is not seen stopped is named, still gets the
# SIGKILL, and its children are still read. It returns 1, naming each, when a process was not seen
# stopped or has not ended, or when a children file held a token that is no pid; a PID already
# gone is nothing to end.

# shellcheck source=packages/envoy/loadtest/common.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/common.sh"
TEST_LIB_SLEEP=$(command -v sleep)

process_ended() {
  local stat task
  [ -e "/proc/$1" ] || return 0
  # A failed read takes the branch after ||: bash gives a brace group whose own redirection fails
  # the status 1 without applying a `!` in front of it.
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || {
    [ -e "/proc/$1" ] && return 1
    return 0
  }
  stat=${stat##*) }
  case ${stat%% *} in
  X) return 0 ;;
  Z) ;;
  *) return 1 ;;
  esac
  for task in "/proc/$1"/task/*; do
    if [ "$task" = "/proc/$1/task/*" ]; then
      [ -e "/proc/$1" ] && return 1
      return 0
    fi
    [ "${task##*/}" = "$1" ] || return 1
  done
  return 0
}

wait_gone() {
  local pid=$1 bound=${2:-10} t0=$EPOCHSECONDS
  until process_ended "$pid"; do
    ((EPOCHSECONDS - t0 <= bound)) || return 1
    "$TEST_LIB_SLEEP" 0.1
  done
}

end_process() {
  local pid=$1
  kill -KILL "$pid" 2>/dev/null || true
  wait_gone "$pid" "${2:-10}" || return 1
}

stop_process() {
  local pid=$1 until=$((EPOCHSECONDS + ${2:-10}))
  kill -STOP "$pid" 2>/dev/null || return 1
  while :; do
    proc_threads_stopped "$pid" && return 0
    [ -e "/proc/$pid" ] || return 1
    ((EPOCHSECONDS <= until)) || return 1
    "$TEST_LIB_SLEEP" 0.05
  done
}

process_start() {
  local stat fields
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 0
  read -r -a fields <<<"${stat##*) }"
  printf '%s\n' "${fields[19]:-}"
}

process_state() {
  local stat
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 0
  stat=${stat##*) }
  printf '%s\n' "${stat%% *}"
}

# socket_inodes PID: the inode of each socket PID holds, one a line: the "socket:[N]" links among
# the descriptors of any of its threads, so a process whose main thread has left is read through
# another thread. A socket is told by its inode, not by its port: once a process has let go of a
# port, the machine can give that number to the next program that binds port 0, and a connect to
# the number then reaches that program.
socket_inodes() {
  local fd link
  for fd in "/proc/$1"/task/*/fd/*; do
    link=$(readlink -- "$fd" 2>/dev/null) || continue
    case $link in socket:\[*\]) link=${link#socket:[}; printf '%s\n' "${link%]}" ;; esac
  done | sort -u
}

# socket_held INODE...: some socket of these inodes still exists: a line of /proc/net/tcp or
# /proc/net/tcp6 (the asking process's network namespace) names one of them in its inode column.
# One awk reads the tables: a machine running many programs holds tens of thousands of lines there
# (31,130 on the box this was written on), which a loop of the shell's read takes minutes to go
# through. It returns 0 when a table names one, 1 when none does, and 2 when no table could be
# read or awk failed, since nothing is known then; a caller counts 2 as a socket still held.
socket_held() {
  local file tables=() rc=0
  for file in /proc/net/tcp /proc/net/tcp6; do
    [ ! -r "$file" ] || tables+=("$file")
  done
  ((${#tables[@]})) || return 2
  awk -v want=" $* " 'index(want, " " $10 " ") { found = 1; exit } END { exit !found }' "${tables[@]}" || rc=$?
  ((rc <= 1)) || rc=2
  return "$rc"
}

record_pid() {
  local start
  start=$(process_start "$2")
  [ -z "$start" ] || printf '%s %s\n' "$2" "$start" >>"$1"
}

end_recorded() {
  local pid start now rc=0
  [ -f "$1" ] || return 0
  while read -r pid start; do
    now=$(process_start "$pid")
    [ -n "$now" ] && [ "$now" = "$start" ] || continue
    end_tree "$pid" || {
      printf 'end_recorded: pid %s, which %s records, was not shown ended with every process below it (the lines above say which)\n' "$pid" "$1" >&2
      rc=1
    }
  done <"$1"
  return "$rc"
}

# end_tree_left UNTIL: the seconds left until UNTIL, 0 once it has passed.
end_tree_left() {
  local left=$(($1 - EPOCHSECONDS))
  printf '%s\n' "$((left > 0 ? left : 0))"
}

end_tree() {
  local pid=$1 bound=${2:-10} walked seen i=0 file kid found=() rc=0 until
  [[ $pid =~ ^[1-9][0-9]*$ ]] || {
    printf 'end_tree: refusing invalid process id %q\n' "$pid" >&2
    return 1
  }
  walked=("$pid") seen=" $pid " until=$((EPOCHSECONDS + bound))
  if ! stop_process "$pid" "$bound"; then
    [ -e "/proc/$pid" ] || return 0
    printf "end_tree: pid %s was not seen stopped by the walk's deadline, %s s from its start; it is sent the SIGKILL with the rest, and its children are read\n" "$pid" "$bound" >&2
    rc=1
  fi
  while ((i < ${#walked[@]})); do
    for file in "/proc/${walked[i]}"/task/*/children; do
      found=()
      # The file ends with no newline, so read answers 1 at its end; only the redirection's own
      # failure makes this group fail, and that takes the else branch (a `!` in front of a group
      # whose redirection fails is not applied by bash). A pattern left unmatched is a task
      # directory that could not be read, or a process that has gone, which its own entry tells.
      if { read -r -a found || :; } 2>/dev/null <"$file"; then
        :
      else
        if [ -e "$file" ] || { [ "$file" = "/proc/${walked[i]}/task/*/children" ] && [ -e "/proc/${walked[i]}" ]; }; then
          printf 'end_tree: the children of pid %s could not be read, so a process below it may not be ended\n' "${walked[i]}" >&2
          rc=1
        fi
        continue
      fi
      for kid in "${found[@]}"; do
        [[ $seen != *" $kid "* ]] || continue
        seen+="$kid "
        [[ $kid =~ ^[1-9][0-9]*$ ]] || {
          printf 'end_tree: refusing invalid process id %q below pid %s\n' "$kid" "${walked[i]}" >&2
          rc=1
          continue
        }
        if ! stop_process "$kid" "$(end_tree_left "$until")"; then
          [ -e "/proc/$kid" ] || continue
          printf "end_tree: pid %s, below pid %s, was not seen stopped by the walk's deadline, %s s from its start; it is sent the SIGKILL with the rest, and its children are read\n" "$kid" "${walked[i]}" "$bound" >&2
          rc=1
        fi
        walked+=("$kid")
      done
    done
    i=$((i + 1))
  done
  # no-wait: each of walked[@] is waited for just below, one at a time, until it has ended
  # (wait_gone).
  kill -KILL "${walked[@]}" 2>/dev/null || true
  until=$((EPOCHSECONDS + bound))
  for i in "${!walked[@]}"; do
    wait_gone "${walked[i]}" "$(end_tree_left "$until")" || {
      printf 'end_tree: pid %s has not ended %s s after its SIGKILL\n' "${walked[i]}" "$bound" >&2
      rc=1
    }
  done
  return "$rc"
}
