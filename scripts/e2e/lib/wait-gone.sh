# shellcheck shell=bash
# The one wait for a process a script has sent a signal, before a later step binds its port, takes
# its lock, reads a file it writes, removes its directory or reads the process table:
# lib/process-tree.sh (which lib/rig.sh, lib/box-login.sh, stage2-tmux-supervision.sh and
# packages/dispatch/e2e/acceptance/replace-blocks.sh source), verifiers-staging-token.sh and
# envoy-session-delivery.sh; packages/envoy/scripts/airlock-check.sh, callout-check.sh and
# dev-stack.test.sh; packages/envoy/deploy/scripts/autodeploy_test_env.sh;
# packages/pi-envoy/scripts/smoke-registration-stall.sh and smoke-role-takeover.sh; and
# docs/research/temporal-evaluation-2026-11/run.sh source it. Sourced, never run: it defines
# wait_gone, kill_gone, stop_gone, wait_gone_name, process_start and read_until, and needs nothing
# of its caller's but bash 5 (EPOCHSECONDS and EPOCHREALTIME), timeout, xargs and grep.
#
# A signal ends nothing by itself. A process given SIGTERM goes on until its handler has run, and a
# server's handler drains first. One given SIGKILL ends the next time the kernel runs it, and until
# its exit has finished it keeps its port, its descriptors (an inherited lock among them) and its
# working directory: for a process holding a few GiB of memory that is a measurable time, in which
# its port refuses a bind and /proc still shows it. Each thread of a process exits on its own, and
# the port, the descriptors and the memory are released only when the last one has: /proc/PID/stat
# shows the main thread alone, which can be a zombie (Z) while another thread of the same process
# still holds them all, as a Go program, Bun and any other process with threads can. So a pid is
# gone once /proc holds no process of that pid, or holds it only as a zombie whose task directory
# lists no thread but the main one: that zombie holds nothing but its pid, which its parent reaps.
#
# A pid gone does not make what it held free. A child it forked, or any process that inherited its
# socket or the lock's descriptor, keeps them after the pid is gone; so does any other process that
# holds a reference to the open file, since the file closes when its last reference goes. So a
# caller whose next step binds a port or takes a lock names that port or lock as a target too, and
# the wait reads that fact itself rather than inferring it from a pid: `port:N` is free once no
# socket listens on TCP port N (ss -Hltn, as a stage proof's ports_free reads it), and `lock:FILE`
# once FILE's flock can be taken (or FILE is not there).
#
# Every wait here ends at a DEADLINE, an absolute time in seconds since the epoch, whole as
# EPOCHSECONDS reads it or with up to six decimals as EPOCHREALTIME does: the caller's own, so that
# two waits of one stop (its SIGTERM's and its SIGKILL's) share one bound rather than each starting
# a fresh one. `$((EPOCHSECONDS + 10))` is ten seconds from now, less the part of this second
# already gone.

# wait_gone_proc is the /proc the waits read; a test points it at a tree of its own.
wait_gone_proc=/proc

# wait_gone DEADLINE TARGET... returns 0 once every TARGET is gone, and 1 once the clock has reached
# DEADLINE with one still there; it never gives up before DEADLINE, and it reads the targets once
# more at it. A TARGET is a pid, -PGID for every process of that group, `port:N` or `lock:FILE`. A
# pid is gone as the top of this file says, and the start time the first read finds is held to: a
# pid the kernel hands a new process during the wait is not taken for the one waited on. A pid whose
# /proc entry is there but cannot be read is not gone. A group is gone once kill -0 finds no process
# in it, or each process in it is gone as a pid is (_wait_gone_group_there): kill -0 alone answers
# for a zombie member too, which holds nothing. Where /proc cannot be read at all, kill -0 answers, a
# zombie included. A port or a lock is gone once it is free as the top of this file says; one that
# cannot be read (no ss, a lock file that cannot be opened, an flock that fails for another reason)
# is not. It prints nothing: the targets still there are left in the array wait_gone_left, for the
# caller to name in its own terms. A DEADLINE that is no time since the epoch (a count of seconds
# from now, say) is refused with exit 2, naming it. The locals' names are ones no caller passes.
wait_gone() {
  local _wg_deadline=$1 _wg_deadline_us _wg_target _wg_start _wg_read _wg_proc=1
  shift
  local -a _wg_targets=() _wg_still=()
  local -A _wg_started=() _wg_members=()
  _wait_gone_deadline wait_gone "$_wg_deadline" _wg_deadline_us || return 2
  [ -r "$wait_gone_proc/self/stat" ] || _wg_proc=
  for _wg_target in "$@"; do
    [ -n "$_wg_target" ] || continue
    if [ -n "$_wg_proc" ] && [[ $_wg_target != -* && $_wg_target != port:* && $_wg_target != lock:* ]]; then
      # A pid gone at the first read is gone: a process that takes its pid later is another one.
      _wg_start='' _wg_read=0
      process_start "$_wg_target" _wg_start || _wg_read=$?
      [ "$_wg_read" != 1 ] || continue
      _wg_started[$_wg_target]=$_wg_start
    fi
    _wg_targets+=("$_wg_target")
  done
  while :; do
    _wg_still=()
    for _wg_target in ${_wg_targets[@]+"${_wg_targets[@]}"}; do
      case $_wg_target in
        port:* | lock:*)
          _wait_gone_free "$_wg_target" || _wg_still+=("$_wg_target")
          continue
          ;;
      esac
      if [ -z "$_wg_proc" ]; then
        # No /proc: kill -0 answers, a zombie member or pid included.
        ! kill -0 -- "$_wg_target" 2>/dev/null || _wg_still+=("$_wg_target")
        continue
      fi
      if [ "${_wg_target#-}" != "$_wg_target" ]; then
        ! _wait_gone_group_there "${_wg_target#-}" "$_wg_deadline" || _wg_still+=("$_wg_target")
        continue
      fi
      _wg_start='' _wg_read=0
      process_start "$_wg_target" _wg_start || _wg_read=$?
      case $_wg_read in
        # Gone: no such process, or a zombie that is its group's last thread.
        1) ;;
        # There but unreadable: not known to be gone.
        2) _wg_still+=("$_wg_target") ;;
        # There: the same process unless its start time changed, which is a new process on a
        # reused pid. One the first read could not read is held to the first start time read.
        *)
          if [ -z "${_wg_started[$_wg_target]:-}" ]; then
            _wg_started[$_wg_target]=$_wg_start
            _wg_still+=("$_wg_target")
          elif [ "$_wg_start" = "${_wg_started[$_wg_target]}" ]; then
            _wg_still+=("$_wg_target")
          fi
          ;;
      esac
    done
    wait_gone_left=(${_wg_still[@]+"${_wg_still[@]}"})
    [ "${#_wg_still[@]}" -gt 0 ] || return 0
    _wg_targets=("${_wg_still[@]}")
    [ "${EPOCHREALTIME//[!0-9]/}" -lt "$_wg_deadline_us" ] || return 1
    sleep 0.1
  done
}

# _wait_gone_group_there PGID DEADLINE returns 0 while the process group PGID has a process that is
# not gone as a pid is (process_start), and 1 once it has none: kill -0 finds no process in it, or
# each one in it is a zombie whose task directory lists no thread but the main one, as when its
# parent, outside the group, is stopped or never waits. The group's processes are known from one walk
# of every process's stat (field 5, the group) under wait_gone_proc, held to the wait's own DEADLINE
# (read_until): a walk that did not end by then, or failed, leaves the group there, as an entry that
# cannot be read leaves a pid there. wait_gone's _wg_members keeps what the walk found, so a group
# with one of those still there is read with no walk; only a group whose known processes are all
# gone while kill -0 still finds one is walked again.
_wait_gone_group_there() {
  local _wgg_group=$1 _wgg_deadline=$2 _wgg_pid _wgg_start _wgg_read _wgg_record
  local -a _wgg_stats=() _wgg_fields=()
  kill -0 -- "-$_wgg_group" 2>/dev/null || return 1
  for _wgg_pid in ${_wg_members[$_wgg_group]-}; do
    _wgg_read=0
    process_start "$_wgg_pid" _wgg_start || _wgg_read=$?
    [ "$_wgg_read" = 1 ] || return 0
  done
  read_until "$_wgg_deadline" _wgg_stats '' "$wait_gone_proc"/[0-9]*/stat || return 0
  _wg_members[$_wgg_group]=
  for _wgg_record in ${_wgg_stats[@]+"${_wgg_stats[@]}"}; do
    read -r -a _wgg_fields <<<"${_wgg_record##*) }"
    [ "${_wgg_fields[2]:-}" = "$_wgg_group" ] || continue
    _wgg_pid=${_wgg_record%%/stat:*}
    _wg_members[$_wgg_group]+=" ${_wgg_pid##*/}"
  done
  for _wgg_pid in ${_wg_members[$_wgg_group]}; do
    _wgg_read=0
    process_start "$_wgg_pid" _wgg_start || _wgg_read=$?
    [ "$_wgg_read" = 1 ] || return 0
  done
  return 1
}

# _wait_gone_free TARGET returns 0 when TARGET, `port:N` or `lock:FILE`, is free as the top of this
# file says, and 1 when it is held or cannot be read. A lock is read by taking it and dropping it at
# once, on a descriptor of its own.
_wait_gone_free() {
  local _wgf_listening _wgf_fd _wgf_status
  case $1 in
    port:*)
      _wgf_listening=$(ss -Hltn "sport = :${1#port:}" 2>/dev/null) || return 1
      [ -z "$_wgf_listening" ]
      ;;
    lock:*)
      [ -e "${1#lock:}" ] || return 0
      { exec {_wgf_fd}<"${1#lock:}"; } 2>/dev/null || return 1
      _wgf_status=0
      flock -n "$_wgf_fd" 2>/dev/null || _wgf_status=1
      exec {_wgf_fd}<&-
      return "$_wgf_status"
      ;;
    *) return 1 ;;
  esac
}

# _wait_gone_deadline WHO DEADLINE VAR assigns VAR the DEADLINE in microseconds since the epoch. It
# fails, naming WHO and DEADLINE on stderr, when DEADLINE is no time in seconds since the epoch at or
# after 2001-09-09 (1000000000), whole or with up to six decimals: a caller that passes a count of
# seconds from now gets this line rather than a wait that ends at once.
_wait_gone_deadline() {
  local _wgd_fraction
  if [[ $2 =~ ^([0-9]+)(\.([0-9]{1,6}))?$ ]] && [ "${BASH_REMATCH[1]}" -ge 1000000000 ]; then
    _wgd_fraction=${BASH_REMATCH[3]}000000
    printf -v "$3" '%s' "$((10#${BASH_REMATCH[1]} * 1000000 + 10#${_wgd_fraction:0:6}))"
    return 0
  fi
  echo "$1: the deadline '$2' is no time in seconds since the epoch (\$((EPOCHSECONDS + 10)) is ten seconds from now)" >&2
  return 1
}

# process_start PID VAR assigns VAR the start time of the process PID names, in clock ticks since
# boot (proc(5), stat field 22), and returns 0 while that process is there; 1 when it is gone (no
# /proc/PID, or a zombie, stat field 3, whose task directory lists no thread but the main one); and
# 2 when /proc/PID is there but its stat could not be read or holds no start time. A caller that
# records a pid with its start time can tell later whether the pid still names that process. The
# command name, field 2, is parenthesised and may hold spaces, a `) ` or a newline, since a process
# names itself, so the stat is read whole (to its end, not its first line) and cut after its last
# `) `; the fields after it hold none of those. It is the one reader of a start and of this rule:
# lib/process-tree.sh's proc_start and lib/leftovers.sh's leftover_started read through it, each
# with wait_gone_proc set to the host's /proc for its own call.
process_start() {
  local _wgr_stat=
  local -a _wgr_tasks
  # read -d '' ends with 1 at the end of a file that holds no NUL, which a stat never does: only
  # a read that got nothing is no read.
  { IFS= read -r -d '' _wgr_stat <"$wait_gone_proc/$1/stat"; } 2>/dev/null || :
  if [ -z "$_wgr_stat" ]; then
    [ -e "$wait_gone_proc/$1" ] && return 2
    return 1
  fi
  read -r -a _wgr_stat <<<"${_wgr_stat##*) }"
  [ -n "${_wgr_stat[19]:-}" ] || return 2
  if [ "${_wgr_stat[0]}" = Z ]; then
    # The main thread has exited. With no other thread listed (or the directory gone, which leaves
    # the pattern itself, one word), the process holds nothing but its pid.
    _wgr_tasks=("$wait_gone_proc/$1/task/"*)
    [ "${#_wgr_tasks[@]}" -gt 1 ] || return 1
  fi
  printf -v "$2" '%s' "${_wgr_stat[19]}"
}

# read_until DEADLINE ARRAY PATTERN PATH... is a walk held to its caller's bound: it reads every PATH
# (every process's /proc stat, say, or the environment of a few) by one grep a batch of PATHs, under
# timeout, so the reads end at DEADLINE (an absolute time, as wait_gone reads one), and assigns the
# array ARRAY one element `PATH:RECORD` for each NUL-ended record of a PATH that matches the grep
# pattern PATTERN (an empty one matches every record): a stat is one record, read whole, since a
# process names itself and a name may hold a newline; an environment is one record a variable. It
# reads in the C locale and as text, so a name that holds a byte that is no UTF-8 is read like any
# other. A PATH that cannot be read, or whose process ended during the walk, gives no record. So a
# /proc entry that never answers its reader (the stat of a process whose task is stuck) holds its
# caller only until DEADLINE. Returns 0 once every PATH was read; 124 when DEADLINE came first, or
# had passed, with ARRAY empty, for the caller to name its bound in its own terms; 2 for a DEADLINE
# wait_gone would refuse; and the status of xargs for any other failure, with ARRAY empty too. The
# locals' names are ones no caller passes.
read_until() {
  local _ru_deadline_us _ru_left _ru_status=0
  _wait_gone_deadline read_until "$1" _ru_deadline_us || return 2
  local -n _ru_records=$2
  local _ru_pattern=$3
  shift 3
  _ru_records=()
  [ "$#" -gt 0 ] || return 0
  _ru_left=$((_ru_deadline_us - ${EPOCHREALTIME//[!0-9]/}))
  [ "$_ru_left" -gt 0 ] || return 124
  printf -v _ru_left '%d.%06d' "$((_ru_left / 1000000))" "$((_ru_left % 1000000))"
  # timeout leads a process group of its own and ends xargs and every grep at DEADLINE: 124, or 137
  # when one needed the SIGKILL a second later. xargs ends with 123 when a grep found no record or
  # met a file it could not read, which is no failure of the walk.
  mapfile -d '' -t _ru_records < <(printf '%s\0' "$@" |
    LC_ALL=C timeout -k 1 "$_ru_left" xargs -0 grep -asHz -e "$_ru_pattern" -- || exit)
  wait "$!" || _ru_status=$?
  case $_ru_status in
    0 | 123) return 0 ;;
    124 | 137) _ru_status=124 ;;
  esac
  _ru_records=()
  return "$_ru_status"
}

# wait_gone_signal names the command kill_gone and stop_gone send each signal with, as
# `"$wait_gone_signal" SIGNAL TARGET...`, given every TARGET that is a pid or a process group and
# never a port or a lock: _wait_gone_kill, a plain kill, unless the caller names its own (a local of
# its function): lib/process-tree.sh's stop_pid sends through freeze_if, held to the start it
# recorded for each pid. A target already gone is no failure.
wait_gone_signal=_wait_gone_kill
_wait_gone_kill() {
  local _wgk_signal=$1
  shift
  # no-wait: kill_gone and stop_gone, through _wait_gone_send, wait for every target next (wait_gone).
  kill -s "$_wgk_signal" -- "$@" 2>/dev/null || true
}

# _wait_gone_send SIGNAL TARGET... sends SIGNAL with wait_gone_signal to each TARGET that is a pid or
# a process group, the one rule kill_gone and stop_gone share: a port or a lock is waited for, never
# signalled, and an empty TARGET is skipped. It sends nothing when no TARGET is a process. It sets
# wait_gone_signalled to the number of TARGETs it sent SIGNAL to, 0 when none was a process: the
# fact stop_gone decides by, and that kill_gone words its naming from.
_wait_gone_send() {
  local _wgs_signal=$1 _wgs_target
  shift
  local -a _wgs_processes=()
  for _wgs_target in "$@"; do
    case $_wgs_target in
      '' | port:* | lock:*) ;;
      *) _wgs_processes+=("$_wgs_target") ;;
    esac
  done
  wait_gone_signalled=${#_wgs_processes[@]}
  [ "$wait_gone_signalled" -gt 0 ] || return 0
  "$wait_gone_signal" "$_wgs_signal" "${_wgs_processes[@]}"
}

# kill_gone SIGNAL DEADLINE TARGET... sends SIGNAL to every TARGET that is a pid or a process group
# (_wait_gone_send), waits until DEADLINE for every TARGET to be gone (wait_gone), the ports and
# locks among them free, and returns 0 once they are. Each one still there at DEADLINE is named on
# stderr with its command line (or what holds the port), the deadline and the time since the signal
# (wait_gone_name), which says so when no TARGET was a process and so no signal went; and it
# returns 1; 2 for a DEADLINE wait_gone refuses. A caller whose next step needs the targets gone
# fails when it returns 1; a cleanup that goes on whatever it finds adds `|| true` and says why.
kill_gone() {
  local _kg_signal=$1 _kg_deadline=$2 _kg_deadline_us _kg_sent _kg_status _kg_signalled
  shift 2
  _wait_gone_deadline kill_gone "$_kg_deadline" _kg_deadline_us || return 2
  _kg_sent=${EPOCHREALTIME//[!0-9]/}
  _wait_gone_send "$_kg_signal" "$@"
  _kg_signalled=$wait_gone_signalled
  wait_gone "$_kg_deadline" "$@" && return 0
  _kg_status=$?
  [ "$_kg_status" = 1 ] || return "$_kg_status"
  # The words for people alone, made here from the count: nothing decides by them.
  if [ "$_kg_signalled" -gt 0 ]; then
    wait_gone_name "SIG$_kg_signal" "$_kg_sent"
  else
    wait_gone_name "SIG$_kg_signal, sent to no process (no target was one)" "$_kg_sent"
  fi
  return 1
}

# stop_gone GRACE DEADLINE TARGET... is a stop: SIGTERM to every TARGET that is a pid or a process
# group (_wait_gone_send), and a wait of up to GRACE whole seconds from the call, counted to the
# microsecond and never past DEADLINE, for every TARGET to be gone (the ports and locks among them
# free); then SIGKILL to the processes still there and a wait for every TARGET until DEADLINE
# (kill_gone), so the two waits share the caller's one bound. A process in the middle of a write (a
# pane's token refresh, a server's drain) gets the whole grace to finish before anything is killed.
# With no TARGET a process (lib/process-tree.sh's stop_pid of an empty PID with its ports), it
# signals nothing and waits for the ports and locks until DEADLINE, as kill_gone names such a wait.
# Returns 0 once every TARGET is gone, 0 too when none is given; 1 when one is still there at
# DEADLINE, named on stderr as kill_gone names it; 2 for a DEADLINE wait_gone refuses.
stop_gone() {
  local _sg_grace=$1 _sg_deadline=$2 _sg_deadline_us _sg_term_end
  shift 2
  _wait_gone_deadline stop_gone "$_sg_deadline" _sg_deadline_us || return 2
  _sg_term_end=$((${EPOCHREALTIME//[!0-9]/} + _sg_grace * 1000000))
  [ "$_sg_term_end" -le "$_sg_deadline_us" ] || _sg_term_end=$_sg_deadline_us
  _wait_gone_send TERM "$@"
  if [ "$wait_gone_signalled" = 0 ]; then
    kill_gone KILL "$_sg_deadline" "$@"
    return
  fi
  wait_gone "${_sg_term_end:0:-6}.${_sg_term_end: -6}" "$@" && return 0
  kill_gone KILL "$_sg_deadline" "${wait_gone_left[@]}"
}

# wait_gone_name SIGNAL SENT names, on stderr, each target wait_gone left, with the deadline it
# reached and the seconds since SENT (EPOCHREALTIME's microseconds, its separator taken out) that
# SIGNAL was sent: `pid N (COMMAND) is still in the process table at its deadline, 2.0 s after its
# SIGKILL`, `process group N still has a process ...`, `port N is still listened on (WHAT ss NAMES)
# ...`, `lock FILE is still held ...`, or, for an entry /proc holds but cannot read, `pid N:
# /proc/N/stat could not be read, so it is not known to be gone ...`.
wait_gone_name() {
  local _wgn_signal=$1 _wgn_sent=$2 _wgn_target _wgn_what _wgn_tenths _wgn_start
  _wgn_tenths=$(((${EPOCHREALTIME//[!0-9]/} - _wgn_sent) / 100000))
  for _wgn_target in ${wait_gone_left[@]+"${wait_gone_left[@]}"}; do
    if [[ $_wgn_target == port:* ]]; then
      # ss -p reads every process's descriptors to name the holder: held to 5 s, as a name is all
      # it adds once the wait is over, and a read that ran past that names no holder.
      _wgn_what=$(timeout -k 1 5 ss -Hltnp "sport = :${_wgn_target#port:}" 2>&1 | tr -s ' \n' ' ') || _wgn_what=
      _wgn_what="port ${_wgn_target#port:} is still listened on (${_wgn_what% })"
    elif [[ $_wgn_target == lock:* ]]; then
      _wgn_what="lock ${_wgn_target#lock:} is still held"
    elif [ "${_wgn_target#-}" != "$_wgn_target" ]; then
      _wgn_what="process group ${_wgn_target#-} still has a process"
    elif ! process_start "$_wgn_target" _wgn_start && [ -e "$wait_gone_proc/$_wgn_target" ]; then
      _wgn_what="pid $_wgn_target: $wait_gone_proc/$_wgn_target/stat could not be read, so it is not known to be gone"
    else
      _wgn_what=$(tr '\0' ' ' 2>/dev/null <"$wait_gone_proc/$_wgn_target/cmdline") || _wgn_what=
      _wgn_what="pid $_wgn_target (${_wgn_what% }) is still in the process table"
    fi
    echo "$_wgn_what at its deadline, $((_wgn_tenths / 10)).$((_wgn_tenths % 10)) s after its $_wgn_signal" >&2
  done
}
