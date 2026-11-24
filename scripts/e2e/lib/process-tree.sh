# shellcheck shell=bash
# How a live proof knows a process it started: by the pid it had when it started, read from what
# started it, and never by a walk of the machine's /proc for its name, its command line or its
# working directory. Such a walk meets every other user's and every other run's processes (a shared
# devbox holds tens of thousands, and each Oh My Pi there starts a daemon broker child with the same
# argv[0]); on a devbox at load 469 one walk of 29,613 entries took 504 s; and a read of one entry
# through a pipe ends early on the pipe's consumer, which pipefail reports as 141 and the walk reads
# as no such process. A process's own name proves nothing either: an omp_invocation, a wrapper or
# `exec -a` gives argv[0] any name, so a process is told by the program the kernel records it runs
# (/proc/<pid>/exe).
#
# Sourced, never run, by lib/rig.sh, by lib/leftovers.sh when no script has sourced it yet (for
# proc_walk_complaint), by stage2-tmux-supervision.sh, which sources no rig, and by
# packages/dispatch/e2e/acceptance/replace-blocks.sh. It needs nothing of its caller's.

# record_pid_sh is the POSIX sh a tmux pane's command runs as
# `sh -c "$record_pid_sh" record-pid FILE COMMAND…`: it writes its own pid and its start time (stat
# field 22, clock ticks since boot, which exec keeps) to FILE, whole, through a rename, then execs
# COMMAND, so FILE names COMMAND's own process from before COMMAND's first instruction. The start
# time is read in a function, whose positional parameters are its own, so COMMAND's stay whole. It
# holds no single quote, so a single-quoted word of a pane's command line carries it.
# shellcheck disable=SC2016,SC2034 # expanded by the pane's shell; the sourcing script's panes run it
record_pid_sh='started() { set -- ${1##*) }; printf "%s" "${20}"; }; read -r stat </proc/$$/stat && printf "%s %s\n" "$$" "$(started "$stat")" >"$1.tmp" && mv -f -- "$1.tmp" "$1" && shift && exec "$@"'

# recorded_pid FILE EXE prints the pid FILE names (record_pid_sh) while that very process runs EXE:
# the pid's start time is the one FILE holds, so a pid the kernel has since given to another
# process, of EXE or of any other program, is not it, and it runs EXE by the kernel's record
# (/proc/<pid>/exe, which a zombie no longer has). It fails otherwise: before the pane wrote FILE,
# once the process has ended, and for a reused pid. It is also the poll that waits for the process
# to start or end.
recorded_pid() {
  local pid start stat want seen
  local -a fields
  { read -r pid start <"$1"; } 2>/dev/null || return 1
  case $pid in '' | *[!0-9]*) return 1 ;; esac
  case $start in '' | *[!0-9]*) return 1 ;; esac
  stat=
  { IFS= read -r -d '' stat <"/proc/$pid/stat"; } 2>/dev/null || :
  [ -n "$stat" ] || return 1
  read -r -a fields <<<"${stat##*) }"
  [ "${fields[19]-}" = "$start" ] || return 1
  want=$(realpath -e -- "$2" 2>/dev/null || :)
  seen=$(readlink "/proc/$pid/exe" 2>/dev/null || :)
  [ -n "$want" ] && [ "$seen" = "$want" ] || return 1
  printf '%s\n' "$pid"
}

# proc_parent PID and proc_group PID print PID's parent pid and process group, and proc_children
# PID prints each of PID's children, one a line. Each reads that one process: fields 4 and 5 of
# /proc/PID/stat, read past the last `) ` because the command name between the parentheses may hold
# both, and every thread's list of children (/proc/PID/task/*/children), since a Go process forks
# from whichever thread runs the call. None reads every process on the machine as `ps -p` and
# `pgrep -P` do: procps-ng's `ps -o args= -p PID` opened /proc/<pid>/stat 12,564 times for one pid
# at about 13,000 processes, about a second on a loaded devbox. Each prints nothing for a process
# that is gone, and succeeds, on either path: a caller under `set -e` assigns the answer bare. A host
# with no /proc (macOS) asks ps and pgrep. The files are read with `read`:
# under `set -e`, bash 5.2 ends the shell when `$(<FILE)` cannot open FILE, even on the left of `||`.
#
# proc_children's list is complete only while the children are stopped or frozen:
# proc_tid_children(5) calls this interface less reliable than a walk of the numbered directories
# of /proc, because when a child the read has passed then exits, the kernel goes on by position in
# the list and can leave a surviving child out. `pgrep -P`, which stop_tree called before, walks
# those directories and reads each process's parent, so it does not meet that case; each form can
# still miss a child that its walk has passed. stop_tree stops the parent first, so it starts no new
# child, and it does not stop the children. So this changes what the list can miss, and neither
# form is the frozen read that the man page asks for.
# The stat is read whole (`read -d ''`, whose status is 1 at the end of a file that holds no NUL,
# which is no error, so `|| :` keeps a caller's ERR trap quiet) and cut after its last `) `: a
# process names itself, and a name that holds `) ` or a newline is then still read past, where a
# read of the first line alone took a forged parent and no start.
proc_stat_field() {
  local stat=
  local -a fields
  if [ -d /proc/self ]; then
    { IFS= read -r -d '' stat <"/proc/$1/stat"; } 2>/dev/null || :
    [ -n "$stat" ] || return 0
    read -r -a fields <<<"${stat##*) }"
    printf '%s\n' "${fields[$2 - 3]}"
  else
    ps -o "$3=" -p "$1" 2>/dev/null | tr -d ' ' || :
  fi
}
proc_parent() { proc_stat_field "$1" 4 ppid; }
proc_group() { proc_stat_field "$1" 5 pgid; }
# proc_start PID prints PID's start time, field 22 of /proc/PID/stat: clock ticks since boot, which
# the kernel sets once, at the fork, and exec keeps, so a pid and its start name one process even
# once the kernel has given the pid to another. It prints nothing for a process that is gone, a
# zombie included, since a zombie runs nothing any more. A host with no /proc asks ps for the start
# as a date (lstart).
proc_start() {
  local stat=
  local -a fields
  if [ ! -d /proc/self ]; then
    [ "$(proc_stat_field "$1" 3 state)" != Z ] || return 0
    proc_stat_field "$1" 22 lstart
    return 0
  fi
  { IFS= read -r -d '' stat <"/proc/$1/stat"; } 2>/dev/null || :
  [ -n "$stat" ] || return 0
  read -r -a fields <<<"${stat##*) }"
  [ "${fields[0]}" != Z ] || return 0
  printf '%s\n' "${fields[19]}"
}

# proc_unreadable WHO WHAT prints "WHO: /proc has no self, so WHAT" and succeeds when this host's
# /proc cannot answer for a process: on Linux a /proc with no `self` (an empty tmpfs mounted there),
# where a pid reads as gone whether or not its process runs, and where pgrep reads nothing too and
# exits 1 as it does for a process with no children. A host with no /proc at all (macOS) is not
# unreadable: it is answered by ps and pgrep.
proc_unreadable() {
  [ ! -d /proc/self ] || return 1
  case ${OSTYPE:-} in
    linux*)
      echo "$1: /proc has no self, so $2" >&2
      return 0
      ;;
  esac
  return 1
}

# proc_children asks pgrep, which walks every process for its parent, where /proc cannot answer: a
# host with no /proc (macOS), and a kernel built without CONFIG_PROC_CHILDREN, whose /proc has no
# task/<tid>/children file at all, so the read below would print no child of a process that has
# children and stop_tree would leave them running. A Linux /proc that cannot be read
# (proc_unreadable), and pgrep's own failure, any status above 1, make it print why and fail with 2,
# so no caller reads either as no children.
proc_children() {
  local list status=0
  local -a more
  ! proc_unreadable proc_children "the children of $1 were not read" || return 2
  if [ ! -d /proc/self ] || { [ -d "/proc/$1" ] && [ ! -e "/proc/$1/task/$1/children" ]; }; then
    pgrep -P "$1" || status=$?
    [ "$status" -le 1 ] || {
      echo "proc_children: pgrep -P $1 exited $status, so the children of $1 were not read" >&2
      return 2
    }
    return 0
  fi
  for list in /proc/"$1"/task/*/children; do
    more=()
    { read -r -a more <"$list"; } 2>/dev/null || :
    [ "${#more[@]}" = 0 ] || printf '%s\n' "${more[@]}"
  done
}

# signalable PID is whether PID can name one process the run started: a number without a leading
# zero other than 1, this shell, and this shell's process group leader (proc_group, which reads this
# shell's own /proc entry alone). 0, however many digits spell it, and a negative number name a
# whole process group, so `kill -STOP 0` would stop the caller with everything else in its group and
# a later `kill -KILL 0` end them all; no pid is written with a leading zero. A refusal is printed,
# since a caller that hands one over has a bug.
signalable() {
  local pid=$1
  case "$pid" in
    '' | *[!0-9]* | 0* | 1) ;;
    *)
      [ "$pid" != "$$" ] && [ "$pid" != "$BASHPID" ] &&
        [ "$pid" != "$(proc_group "$$")" ] && return 0
      ;;
  esac
  echo "refused to signal pid '$pid': it is not one process this run started" >&2
  return 1
}

# started_at maps each pid a proof recorded (record_start) to the start time it had then. A pid is
# recorded where the shell learns it, right after its `&` (`$!`) or its own pid file: the kernel gives
# a pid to another process only once its process has ended and been reaped, so that read meets the
# process itself, or nothing once it has gone, unless in the moment between it ended, bash reaped it
# and another process took the pid. A pid whose process had already ended, or was a zombie, when
# its start was read holds `gone` (start_process records each service so): there is no process to
# signal, and the pid may be anyone's by then, so stop_pid and signal_group send it nothing and say
# nothing. A second source of this file keeps what is recorded.
declare -gA started_at

# record_start PID records PID's start time in started_at, for stop_pid to hold PID to, and fails
# with 1 when PID has no live process to read it from.
record_start() {
  local start
  start=$(proc_start "$1")
  [ -n "$start" ] || return 1
  started_at[$1]=$start
}

# record_pid_file FILE VAR reads the pid and the start a process wrote of itself before it ran
# (record_pid_sh), assigns the pid to VAR and records the start with it (started_at), and fails
# until FILE is there whole. Both are the process's own, so a signal held to them reaches nothing
# that took the pid since, however late FILE is read: a cleanup that reads it after the process
# ended holds the pid to the process's start, not to a stranger's.
record_pid_file() {
  local pid start
  { read -r pid start <"$1"; } 2>/dev/null || return 1
  case $pid in '' | *[!0-9]*) return 1 ;; esac
  case $start in '' | *[!0-9]*) return 1 ;; esac
  started_at[$pid]=$start
  printf -v "$2" '%s' "$pid"
}

# stop_pid PID ends PID, which the proof recorded with record_start (start_process records each
# service): SIGTERM, then up to 10 s for it to exit, then SIGKILL, each sent through freeze_if held to
# the recorded start, so a pid whose process ended, was reaped and passed to another process since,
# another user's or another run's, gets no signal. On this box the pid counter wraps in about seven
# minutes, so a daemon that crashed before the cleanup leaves its pid to a stranger well within a
# run. A pid with no recorded start is a caller's bug: it is named and left alone. A pid recorded
# `gone` (started_at) is left alone with no word. It is best effort: a failed cleanup must never
# obscure the check that failed, so it always succeeds.
stop_pid() {
  local pid=${1:-} start i
  [ -n "$pid" ] || return 0
  start=${started_at[$pid]:-}
  [ -n "$start" ] || {
    echo "stop_pid: no start time is recorded for pid $pid (record_start), so it was not signalled" >&2
    return 0
  }
  [ "$start" != gone ] || return 0
  signalable "$pid" || return 0
  freeze_if "$pid" "$start" TERM || return 0
  for i in $(seq 1 50); do
    [ "$(proc_start "$pid")" = "$start" ] || return 0
    sleep 0.2
  done
  freeze_if "$pid" "$start" KILL || true
  return 0
}

# signal_group PID SIGNAL sends SIGNAL to the process group PID leads, PID being a group leader the
# proof recorded (record_start), only while PID is still that process: it stops the leader first
# (freeze_if … STOP, held to the recorded start), so the leader cannot exit and leave its group id to
# another group before the signal lands, then signals the group and thaws the leader. A leader with
# no recorded start, one recorded `gone`, or one that has gone since, is left alone, and so is what
# is left of its group: with no leader to hold to its start, nothing tells that group from another,
# so a signal_group after stop_pid of the same leader sends nothing. A rig that must end a member
# that outlived its leader reads the leader's descendants while it is held stopped and ends each by
# its start (the Envoy session-states stop_agent), or runs the run's sweep (stop_marked_processes),
# as the Envoy session-states and clean-session-stop cleanups do. It always succeeds.
signal_group() {
  local pid=${1:-} start
  [ -n "$pid" ] || return 0
  start=${started_at[$pid]:-}
  [ -n "$start" ] && [ "$start" != gone ] || return 0
  signalable "$pid" || return 0
  freeze_if "$pid" "$start" STOP || return 0
  kill "-$2" -- "-$pid" 2>/dev/null || true
  kill -CONT "$pid" 2>/dev/null || true
  return 0
}

# process_args PID NAME reads PID's arguments into the array NAME from the NUL-delimited
# /proc/PID/cmdline, in this shell: no pipe stands between the read and the check, so nothing can
# end early on it. It fails when the entry cannot be read or holds no argument (the process has
# gone).
process_args() {
  local -n _args=$2
  { mapfile -d '' -t _args <"/proc/$1/cmdline"; } 2>/dev/null && [ "${#_args[@]}" -gt 0 ]
}
# process_environ PID NAME reads PID's environment into the array NAME, as process_args reads its
# arguments, and fails when it cannot be read or is empty (the process has gone).
process_environ() {
  local -n _environ=$2
  { mapfile -d '' -t _environ <"/proc/$1/environ"; } 2>/dev/null && [ "${#_environ[@]}" -gt 0 ]
}
# env_of PID NAME prints every value PID's environment gives NAME, one a line, read in this shell
# from the NUL-delimited /proc/PID/environ with no pipe between the read and the check: a
# `tr | grep -q` stops reading at its match, tr's SIGPIPE is the pipeline's status under pipefail,
# and a negated check reads that as absent. It fails with 2 when the environment cannot be read or
# is empty (the process has exited), which is never the same as a clean one. Like every reader of an
# environment here, it turns the shell's xtrace off for itself (`local -`), so a run under `bash -x`
# does not write the environment's other values, a provider key among them, into its transcript.
env_of() {
  local - entry
  local -a entries=()
  set +x
  process_environ "$1" entries || return 2
  for entry in "${entries[@]}"; do
    case $entry in "$2="*) printf '%s\n' "${entry#*=}" ;; esac
  done
}
# env_has PID NAME succeeds when PID's environment names NAME, with any value or none, fails with 1
# when it does not, and with 2 when it cannot be read (env_of). A check that a variable is absent
# holds only on status 1.
env_has() {
  local - entry
  local -a entries=()
  set +x
  process_environ "$1" entries || return 2
  for entry in "${entries[@]}"; do
    case $entry in "$2="*) return 0 ;; esac
  done
  return 1
}
# env_length PID NAME prints the length in bytes of the first value PID's environment gives NAME, and
# 0 when it gives none, and fails with 2 when the environment cannot be read (env_of): a check of a
# secret's presence reads its length without the value ever standing in a variable of its caller.
# The count is in the C locale (a local LC_ALL), so a value of multibyte characters counts its bytes.
env_length() {
  local - entry LC_ALL=C
  local -a entries=()
  set +x
  process_environ "$1" entries || return 2
  for entry in "${entries[@]}"; do
    case $entry in "$2="*)
      entry=${entry#*=}
      printf '%s\n' "${#entry}"
      return 0
      ;;
    esac
  done
  printf '0\n'
}
# environ_laid_out PID succeeds once PID's environment holds a variable, and fails until then. Linux
# names the program an exec starts (/proc/PID/exe) before it has laid out that program's arguments
# and environment: in Linux 7.0, begin_new_exec sets the exe and installs the new memory
# (fs/exec.c), load_elf_binary then maps the program, and create_elf_tables lays out the arguments,
# sets the environment's bounds to an empty range, lays out the environment and only then sets its
# end (fs/binfmt_elf.c). /proc/PID/cmdline reads nothing until those bounds are first set, and
# /proc/PID/environ reads nothing until that end is (fs/proc/base.c). On a devbox at load 1065, a
# poll saw an exe naming the program and an empty environment in 91 of 200 starts of a copy of
# sleep, for up to 62 ms. So a caller that found PID by the program it runs (launched, a wait on
# /proc/PID/exe) waits on this before it reads PID's environment: in that window env_of, env_has
# and env_length answer 2, an environment that could not be read, for a process that will run with
# a full one. Read the exe first: before that exec, PID runs another program (env, a shell) with an
# environment of its own. It fails, too, for a process that has gone, and for one started with an
# empty environment, which no wait makes readable.
environ_laid_out() {
  local -a entries=()
  process_environ "$1" entries
}

# incarnation_pid INCARNATION prints the pid of the tmux runtime's incarnation `<pid>:<start ticks>`
# (a claim's .locator.incarnation, packages/daemon/internal/runtime/tmux/tmux.go) while that pid is
# still the process that started at those ticks (proc_start), and fails otherwise: a malformed
# incarnation, a process that has ended, and a pid the kernel has since given to another process.
# A pane is never known by its pid alone, as the daemon itself never knows it.
incarnation_pid() {
  local pid=${1%%:*} ticks=${1#*:}
  case $1 in *:*) ;; *) return 1 ;; esac
  case $pid in '' | 0* | *[!0-9]*) return 1 ;; esac
  case $ticks in '' | *[!0-9]*) return 1 ;; esac
  [ "$(proc_start "$pid")" = "$ticks" ] || return 1
  printf '%s\n' "$pid"
}

# freeze_if PID START SIGNAL [CHECK…] signals PID with SIGNAL only while it is still the process
# that started at START (proc_start) and CHECK, a command given PID as its last word, succeeds. It
# reads both before it sends anything, and sends nothing to a pid that fails either: a process of
# this user's that took a pid the walk read gets no signal, unless the pid passes to it between
# that first read and the SIGSTOP below, a window no read closes without a pidfd; then it gets the
# SIGSTOP, the second read finds the mismatch, and it is thawed. Then it stops PID (SIGSTOP), so
# the process cannot end and its pid pass to another between the check and the signal, reads both
# again, signals, and thaws it (SIGCONT) unless SIGNAL is KILL or STOP. On a mismatch at the second
# read it sends nothing more, and thaws PID only when this stop is what stopped it: a process that
# was stopped already (state T or t at the first read, a job its shell stopped) stays stopped. It
# fails too when PID cannot be signalled (signalable) or stopped; a process of another user cannot
# be stopped at all. It fails with 2, sending nothing more, where CHECK failed with 2 (a check that
# could not read PID, as held_names_dir answers for a read no exec let it make), and with 1 on any
# other refusal.
freeze_if() {
  local pid=$1 start=$2 signal=$3 was checked=0
  shift 3
  signalable "$pid" || return 1
  [ "$(proc_start "$pid")" = "$start" ] || return 1
  [ "$#" = 0 ] || "$@" "$pid" || checked=$?
  [ "$checked" = 0 ] || return "$((checked == 2 ? 2 : 1))"
  was=$(proc_stat_field "$pid" 3 state)
  kill -STOP "$pid" 2>/dev/null || return 1
  [ "$(proc_start "$pid")" = "$start" ] || checked=1
  [ "$checked" != 0 ] || [ "$#" = 0 ] || "$@" "$pid" || checked=$?
  if [ "$checked" != 0 ]; then
    case $was in T | t) ;; *) kill -CONT "$pid" 2>/dev/null || true ;; esac
    return "$((checked == 2 ? 2 : 1))"
  fi
  kill -"$signal" "$pid" 2>/dev/null || true
  case $signal in KILL | STOP) ;; *) kill -CONT "$pid" 2>/dev/null || true ;; esac
}

# names_dir DIR NAME… PID succeeds when PID's environment, read whole in this shell with no pipe,
# gives one of the NAMEs the value DIR or a path under DIR; fails with 1 when none has that value,
# and with 2 when the environment cannot be read or is empty (the process has gone). PID comes last,
# so freeze_if can run it as a CHECK. It runs with xtrace off (env_of). An empty DIR is refused with
# 2: `NAME=` followed by any absolute path would match it, so every process carrying NAME would be
# the run's.
names_dir() {
  local - dir=$1 pid=${!#} entry name
  local -a names=("${@:2:$# - 2}") entries=()
  set +x
  [ -n "$dir" ] || {
    echo "names_dir: no run directory given, so no process is the run's" >&2
    return 2
  }
  process_environ "$pid" entries || return 2
  for entry in "${entries[@]}"; do
    for name in "${names[@]}"; do
      case $entry in "$name=$dir" | "$name=$dir"/*) return 0 ;; esac
    done
  done
  return 1
}

# held_names_dir DIR NAME… PID answers what names_dir answers for PID, from a read of PID's
# environment that falls within one exec, as procs_naming's own reads do: it reads the environment
# between two reads of PID's bounds (exec_state), and takes the answer only when both show the same
# bounds, set. A read across an exec comes back empty, or cut short at a page with the mark further
# on, and the sweep that took one such read for an answer took a marked process for an unmarked one
# and left it running (a reviewer's forcing: 3 of 25 sweeps). While PID is inside one exec it waits
# (every 10 ms, up to procs_naming_hold_seconds, 300), and while its bounds move under each read it
# reads again (up to procs_naming_exec_seconds, 10); past either, or when this user cannot open
# PID's environment any more, it says why and fails with 2, so a check that could not read PID is
# never taken for one that found no mark. An ended PID, and bounds set and equal after the exec (an
# environment of no variable), answer 1. Where the kernel shows no bounds (bounds_shown) each read
# is taken as it comes. PID comes last, so freeze_if can run it as a CHECK.
held_names_dir() {
  local pid=${!#} before held again found shown hold='' deadline=''
  before=$(proc_start "$pid")
  [ -n "$before" ] || return 1
  bounds_shown shown
  while :; do
    exec_state "$pid" "$before" held "$shown"
    case $held in
      gone | empty*) return 1 ;;
      exec)
        hold=${hold:-$((SECONDS + ${procs_naming_hold_seconds:-300}))}
        [ "$SECONDS" -lt "$hold" ] || {
          echo "held_names_dir: process $pid stayed inside one exec for ${procs_naming_hold_seconds:-300} s, so whether it carries the run's mark was not checked" >&2
          return 2
        }
        sleep 0.01
        continue
        ;;
    esac
    found=0
    names_dir "$@" || found=$?
    exec_state "$pid" "$before" again "$shown"
    [ "$again" != "$held" ] || [ "$found" = 2 ] || return "$found"
    [ "$again" != gone ] || return 1
    { : <"/proc/$pid/environ"; } 2>/dev/null || {
      echo "held_names_dir: the environment of process $pid can no longer be read, so whether it carries the run's mark was not checked" >&2
      return 2
    }
    deadline=${deadline:-$((SECONDS + ${procs_naming_exec_seconds:-10}))}
    [ "$SECONDS" -lt "$deadline" ] || {
      echo "held_names_dir: process $pid gave no read of its environment within one exec for ${procs_naming_exec_seconds:-10} s, so whether it carries the run's mark was not checked" >&2
      return 2
    }
    sleep 0.01
  done
}

# proc_descendants PID [START] prints `PID START` for every process below PID, children first, then
# theirs: each read from its parent's own /proc entry (proc_children) with its start time
# (proc_start), so a later signal can hold it to the process the read saw. Below a stopped PID the
# parents run, and one can reap a child, whose pid a stranger can then take, at any point of the
# walk. So each parent is held to its process, not to its pid: with START, PID's start must read
# START before its children are read and again after each child's parent check, or the walk below
# PID ends there; each child is taken only while its parent is PID, with its start the same before
# and after that check, and its own children are walked held to that start. A stranger that took a
# reaped pid, and the stranger's children, are left out. A process that ends during the read is left
# out. Without START, PID itself is not checked: a caller that has PID stopped passes its start all
# the same.
proc_descendants() {
  local parent=$1 held=${2:-} child start children
  [ -z "$held" ] || [ "$(proc_start "$parent")" = "$held" ] || return 0
  children=$(proc_children "$parent") || return 2
  for child in $children; do
    start=$(proc_start "$child")
    [ -n "$start" ] || continue
    [ "$(proc_parent "$child")" = "$parent" ] && [ "$(proc_start "$child")" = "$start" ] || continue
    [ -z "$held" ] || [ "$(proc_start "$parent")" = "$held" ] || return 0
    printf '%s %s\n' "$child" "$start"
    proc_descendants "$child" "$start" || return 2
  done
}

# kill_frozen PID ends PID, which the caller has stopped and held to its start time and its mark
# (freeze_if … STOP), with every process under it: each child goes with its own tree (stop_tree,
# held to PID as its parent, which a stopped PID cannot lose), then PID itself (SIGKILL). PID's own
# parent is not read: it can exit and leave PID to another between two reads, and a stop_tree held
# to the parent read first would then leave PID stopped and alive. When PID's children, or a
# child's, could not be read (proc_children), PID is killed all the same and it fails with 2.
kill_frozen() {
  local pid=$1 child children status=0
  children=$(proc_children "$pid") || status=2
  for child in $children; do stop_tree "$child" "$pid" || status=2; done
  kill -KILL "$pid" 2>/dev/null || true
  return "$status"
}

# kill_held DIR NAME… reads `PID START` lines on stdin, as a walk of the run's processes prints them
# (procs_naming), and ends each with everything under it (kill_frozen) once freeze_if has stopped it
# while it is still the process that started at START and its environment still gives one of the
# NAMEs the value DIR or a path under it (held_names_dir, a read within one exec). A pid that passed
# to another process since the walk gets no signal, and a blank line is skipped. The run's sweep
# (stop_marked_processes, with run_marks) and the skill-scenario rig's stop_env_processes (with
# SKILL_SCENARIO_RUN) end their walks' processes through it, so the loop and its rows are written
# once. It fails with 2 when a tree's processes could not all be read (kill_frozen), and when a
# process's check could not read it (freeze_if's 2), which is left running and named.
kill_held() {
  local dir=$1 pid start status=0 held
  shift
  while read -r pid start; do
    [ -n "$pid" ] || continue
    held=0
    freeze_if "$pid" "$start" STOP held_names_dir "$dir" "$@" || held=$?
    if [ "$held" = 2 ]; then
      echo "kill_held: process $pid of the run under $dir could not be checked, so it was left running" >&2
      status=2
      continue
    fi
    [ "$held" = 0 ] || continue
    kill_frozen "$pid" || status=2
  done
  return "$status"
}

# stop_tree PID [PARENT] stops PID and every process under it. A watcher is a loop whose kubectl, jq
# or sleep outlives the loop when only the loop is stopped; the loop is frozen first, so it starts
# nothing new while its children go. PID is signalled only while its parent is PARENT (default this
# shell), so a pid the watcher left and another process reused is never hit. An empty PID is a
# watcher that never started: nothing to stop. The parent and the children are read from PID's own
# /proc entry (proc_parent and proc_children), never from every process on the machine.
# proc_children prints nothing for a process with no children, which is no error: under a driver's
# `set -E` a failing command would run the ERR trap inside the substitution, and a trap that prints
# there would hand its words over as child pids (the `0` of an `exit 0` among them). On a Linux
# /proc that cannot be read (proc_unreadable), where a pid's parent reads as none whether or not the
# process runs, it prints why and fails with 2, and so it does when a child's could not be read.
stop_tree() {
  local pid=${1:-} parent=${2:-$$}
  [ -n "$pid" ] || return 0
  ! proc_unreadable stop_tree "the parent of $pid was not read" || return 2
  signalable "$pid" || return 0
  [ "$(proc_parent "$pid")" = "$parent" ] || return 0
  kill -STOP "$pid" 2>/dev/null || return 0
  kill_frozen "$pid"
}

# proc_walk_complaint ERRFILE PROC prints the first line of a walk of PROC's stderr that is neither an
# entry this user may not read, another user's process's, nor one whose process ended during the
# walk, and fails when every line is one of those two: what a walk of every process meets on a
# busy machine and no error of its own. It reads a find's lines (`find: 'PROC/<pid>…': …`) and a
# grep's of an environment (`grep: PROC/<pid>/environ: …`), for which a process that ended between
# the path's lookup and the open reads as ESRCH (`No such process`), the kernel's answer for an
# environ whose task has gone. A walk that could not start (E2BIG, a find that is not there, or
# one too old to read -files0-from) leaves a line of its own or the shell's, which is a complaint.
# listening_holders (lib/rig.sh) and refuse_leftovers (lib/leftovers.sh) read their find's stderr
# with it, and procs_naming its grep's.
proc_walk_complaint() {
  local line rest
  while IFS= read -r line; do
    rest=${line#*find: \'"$2"/}
    [ "$rest" = "$line" ] || [[ ! $rest =~ ^[0-9]+(/[^\']*)?\':\ (Permission\ denied|No\ such\ file\ or\ directory)$ ]] || continue
    rest=${line#*grep: "$2"/}
    [ "$rest" = "$line" ] || [[ ! $rest =~ ^[0-9]+/environ:\ (Permission\ denied|No\ such\ file\ or\ directory|No\ such\ process)$ ]] || continue
    printf '%s\n' "$line"
    return 0
  done <"$1"
  return 1
}

# proc_nspid PID NAME assigns to NAME the pids of PID's NSpid line in /proc/PID/status, from the pid
# namespace /proc shows down to PID's own, one space between, or nothing where that file cannot be
# read (PID has gone) or the kernel writes no such line (Linux before 4.1, gVisor). It reads in this
# shell, so `proc_nspid self NAME` reads this shell's own.
proc_nspid() {
  local -a nspid_words=()
  local nspid_out='' nspid_word
  { while read -r -a nspid_words; do [ "${nspid_words[0]-}" != NSpid: ] || break; nspid_words=(); done <"/proc/$1/status"; } 2>/dev/null || :
  for nspid_word in "${nspid_words[@]:1}"; do nspid_out+="${nspid_out:+ }$nspid_word"; done
  printf -v "$2" '%s' "$nspid_out"
}

# ns_init_adopts PID succeeds when PID, a process older than this shell, is the init of a pid
# namespace below this shell's (its NSpid, proc_nspid, names more than one pid, the last 1) that may
# hold an orphan of the run, and fails otherwise. procs_naming walks only from a shell in the pid
# namespace /proc shows, so the init of this shell's own is pid 1 there, which each walk judges
# itself. The run's orphans reach the init of a namespace below it only through a process of the run
# that entered it (nsenter, a setns), and entering it takes privilege over its owner, which reading
# its namespace links takes too (`/proc/PID/ns/pid`): the kernel shows them (ptrace_may_access, in
# its read mode) only to a process whose fsuid and fsgid equal PID's real, effective and saved uid
# and gid while PID is dumpable, or to one that holds CAP_SYS_PTRACE in PID's user namespace, as in
# one this user made. So an init whose links this user cannot read is one whose namespace the run
# could not enter: a rootful container's docker-init, below which only the container runtime puts
# processes (`docker exec`), with the environment the runtime gives them, which holds the run's
# mark only where a caller copied it in by name (`docker exec -e LEGION_E2E_RUN`). Tester4784-3 at
# fe4ead44bb: another session's not-dumpable perl that docker-init adopted failed every walk with 2.
# Such an init holds none of the run's orphans, and a process whose first older parent it is cannot
# carry the mark. It may hold them, and this succeeds, where this user may read its links, and
# where this kernel shows no namespace links at all. Not covered: a privileged entry into a
# namespace this user cannot enter that keeps the run's mark (`sudo -E nsenter`, `docker exec -e`
# with the mark) and leaves a not-dumpable process there; no script of this repository does so.
ns_init_adopts() {
  local ns_pids
  proc_nspid "$1" ns_pids
  [ "${ns_pids% *}" != "$ns_pids" ] && [ "${ns_pids##* }" = 1 ] || return 1
  [ -e "/proc/$1/ns/pid" ] && return 0
  [ -e /proc/self/ns/pid ] || return 0
  return 1
}

# unread_may_carry PID SHELL_START ANCESTOR… succeeds when PID, a process of this user whose
# environment this user cannot read, may carry the mark of the run this shell runs, and fails when
# it cannot. The kernel gives a process's environ to root while the process is not dumpable: one
# that set PR_SET_DUMPABLE to 0 (ssh-agent and gpg-agent do at their start), one whose credentials
# changed (a login session's process that tailscaled or sshd started as root), and one with no
# memory left (a zombie); its /proc/PID directory, its stat and its status stay its own user's.
# SHELL_START is the start (proc_start) of this shell, $$, the run's own, and each ANCESTOR is one
# of this shell's whole line of parents up to its top, as PID:START (proc_parent and proc_start;
# procs_naming refuses a line it could not read to the top), so a pid another process has taken
# since matches none. A process that started in the same clock tick as this shell is taken for one
# that started after it, and a parent of the same tick for a younger one: the refusal is the side a
# tie falls on. A process of the run is one this shell started, or one started for it, so it
# started after this shell. Its parent is the process that started it, or, once that one ended,
# the nearest subreaper above it, or pid 1, which adopted it: each parent read up from it is one of
# its ancestors, and for a process of the run every one below this shell, or below the ANCESTOR
# that adopted it, started after this shell, so the first one older than this shell is an
# ANCESTOR, or the init of this shell's pid namespace: a shell that entered a namespace from outside
# (`docker exec`, `kubectl exec`, a step of a GitHub Actions `container:` job) has no parent there
# (its parent reads 0), and the run's orphans go to the namespace's init. A subreaper that is none
# of the ANCESTORs adopts none of the run's processes, since the kernel gives an orphan only to a
# subreaper above its own dead parent. So PID cannot carry the mark when it runs nothing (it has
# ended or is a zombie), when it started before this shell (a devbox's own ssh-agent, gpg-agent or
# forward), and when the first of its parents older than this shell is none of the ANCESTORs and
# no init of a pid namespace that may hold the run's orphans (pid 1 here, or the init of a namespace
# below this shell's, as ns_init_adopts judges it): a process that ran outside the run before it,
# below which nothing inherited the run's environment (another session's tool, a login session's
# process, one that `docker exec` left in a rootful container's namespace). Any other may: a child
# of the run, one double-forked away from it to a reaper, as an ssh-agent the run started is, and
# one whose line of parents could not be read to its end (a parent that ended during the read),
# unless it has ended since. A process of another session that double-forked away during the run
# (a gpg-agent started for a new homedir), or that the init of this shell's own namespace adopted,
# reads as one
# of the run's: its parent is then a reaper too, and nothing readable tells the two apart, on a
# devbox often enough to fail a long run's teardown (the tester of aec1fb77ff saw 13 of 60 walks
# exit 2 on such processes). A shell that is a child subreaper (lib/subreaper.py) is judged by
# run_descendant instead, which tells them apart. Each process on the way is read once, its parent
# and its start from one read of its stat (as proc_start reads it), in this shell: a devbox holds
# dozens of this user's zombies and not-dumpable processes, and a subshell for each read doubled the
# time of a walk of its /proc.
unread_may_carry() {
  local pid=$1 shell=$2 at parent=$1 start='' stat ancestor i
  local -a fields
  shift 2
  for ((i = 0; i < 4096; i++)); do
    stat=
    { IFS= read -r -d '' stat <"/proc/$parent/stat"; } 2>/dev/null || :
    [ -n "$stat" ] || break
    read -r -a fields <<<"${stat##*) }"
    [ "${fields[0]}" != Z ] || break
    at=${fields[19]}
    if [ -z "$start" ]; then
      start=$at
      [ "$start" -ge "$shell" ] || return 1
    elif [ "$at" -lt "$shell" ]; then
      for ancestor; do [ "$ancestor" != "$parent:$at" ] || return 0; done
      [ "$parent" != 1 ] || return 0
      ns_init_adopts "$parent" && return 0
      return 1
    fi
    parent=${fields[1]}
  done
  [ -n "$start" ] && [ "$(proc_start "$pid")" = "$start" ]
}

# run_descendant PID SHELL_START succeeds when PID, a process of this user whose environment this
# user cannot read, may carry the mark of the run this shell runs, where this shell, $$, is a child
# subreaper (lib/subreaper.py, which names it in LEGION_E2E_SUBREAPER as `PID START`), and fails
# when it cannot.
# Every process of the run then stays below this shell for its whole life: one whose parent ends
# goes to the nearest subreaper above that parent, which is this shell or one below it, never pid 1
# or a reaper above this shell; except one in another pid namespace, which goes to that namespace's
# init however far above it the subreaper is (a reviewer's forcing: the run nsenter'd a namespace
# older than the shell, its sh exited, and that namespace's init adopted the run's not-dumpable
# orphan). So PID may carry the mark when its line of parents reaches this shell itself (its pid
# and SHELL_START, the start proc_start reads) before a process older than this shell, or meets
# first an older process that is the init of a pid namespace below this shell's and that may hold
# the run's orphans (ns_init_adopts), and one whose line could not be read to there (a parent that
# ended during the read) may, unless it has ended since. PID cannot when it runs nothing (it has
# ended or is a zombie), when it started before this shell, and when its line meets first another
# process older than this shell, this namespace's pid 1 among them, and an init whose namespace
# the run could not enter (a rootful container's docker-init): a process another session left
# behind during the run, however young. Each process on the way is read once, as unread_may_carry
# reads it, in this shell.
run_descendant() {
  local pid=$1 shell=$2 at parent=$1 start='' stat i
  local -a fields
  for ((i = 0; i < 4096; i++)); do
    stat=
    { IFS= read -r -d '' stat <"/proc/$parent/stat"; } 2>/dev/null || :
    [ -n "$stat" ] || break
    read -r -a fields <<<"${stat##*) }"
    [ "${fields[0]}" != Z ] || break
    at=${fields[19]}
    if [ -z "$start" ]; then
      start=$at
      [ "$start" -ge "$shell" ] || return 1
    elif [ "$parent" = "$$" ] && [ "$at" = "$shell" ]; then
      return 0
    elif [ "$at" -lt "$shell" ]; then
      [ "$parent" != 1 ] || return 1
      ns_init_adopts "$parent" && return 0
      return 1
    fi
    parent=${fields[1]}
  done
  [ -n "$start" ] && [ "$(proc_start "$pid")" = "$start" ]
}

# exec_state PID START NAME [SHOWN] assigns to NAME what one read of /proc/PID/stat says of PID's
# environment while PID is still the process that started at START (proc_start): `gone` once it has
# ended, is a zombie, or its pid is another's; `exec` inside an exec; `empty START` for bounds set
# and equal, an environment of no variable; and `set START-END` for one with variables, its two
# bounds (fields 50 and 51, env_start and env_end). An exec gives the process new memory, all of
# whose bounds read 0, then sets env_start and env_end both to the start of the variables, counts
# them, and sets env_end to their end, and only after that sets start_code (field 26). So a read is
# `exec` while env_start or start_code reads 0: of 100,000 reads of a process re-exec'ing itself,
# 116 found the bounds set and equal, and all 116 found start_code 0; a process held inside that
# count (SCHED_IDLE on a busy CPU) stays there as long as it is held, and a walk that took it for
# `empty` named nothing of it. The two bounds come from the one read, so they are one moment's, and
# an exec places them anew (address space randomization places each new stack elsewhere). A read of
# /proc/PID/environ reads the memory the process had when the file was opened, and comes back empty,
# or cut short, when an exec ends that memory during the read; so the same `set` read before the
# open and after the read holds that read to one exec's memory, whole. The kernel shows these
# fields to a reader that may read the process's memory, as this user may read its own dumpable
# process's. A kernel that shows none of them, as gVisor shows all of them as 0, cannot tell an exec
# from its end: a caller that found so (bounds_shown) passes SHOWN empty, and a live process then
# reads `set -`, so each read of its environment is taken as it comes. It runs in this shell and
# assigns, never prints: a subshell for each read took longer than an exec of sh, and two reads
# around one environment never met one exec.
exec_state() {
  local stat=
  local -a fields
  { IFS= read -r -d '' stat <"/proc/$1/stat"; } 2>/dev/null || :
  if [ -z "$stat" ]; then
    printf -v "$3" '%s' gone
    return 0
  fi
  read -r -a fields <<<"${stat##*) }"
  if [ "${fields[0]}" = Z ] || [ "${fields[19]}" != "$2" ]; then
    printf -v "$3" '%s' gone
  elif [ "$#" -ge 4 ] && [ -z "$4" ]; then
    printf -v "$3" '%s' 'set -'
  elif [ "${fields[47]:-0}" = 0 ] || [ "${fields[23]:-0}" = 0 ]; then
    printf -v "$3" '%s' exec
  elif [ "${fields[48]:-}" = "${fields[47]}" ]; then
    printf -v "$3" 'empty %s' "${fields[47]}"
  else
    printf -v "$3" 'set %s-%s' "${fields[47]}" "${fields[48]}"
  fi
}

# bounds_shown NAME assigns 1 to NAME when this kernel shows a process's memory bounds in its stat
# (exec_state), and nothing when it does not: this shell runs, has an environment and is in no exec,
# so its own env_start and start_code read 0 only where the kernel shows them all as 0, as gVisor
# (4.19.0-gvisor) does.
bounds_shown() {
  local stat=
  local -a fields
  { IFS= read -r -d '' stat <"/proc/$BASHPID/stat"; } 2>/dev/null || :
  read -r -a fields <<<"${stat##*) }"
  if [ "${fields[47]:-0}" != 0 ] && [ "${fields[23]:-0}" != 0 ]; then
    printf -v "$1" '%s' 1
  else
    printf -v "$1" '%s' ''
  fi
}

# procs_naming DIR NAME… prints `PID START` (proc_start) for each process whose environment gives
# one of the NAMEs the value DIR or a path under it (names_dir), this shell itself left out. It reads
# the environment of every process of `${proc_root:-/proc}`: the host's /proc, unless a test points
# proc_root at a proc root of its own (links to the /proc entries of the processes it started,
# scripts/e2e/lib/proc-root.ts), since a walk of the host's whole /proc holds the test's time to the
# host's process count. Every process is read since a process that left every tree the run
# recorded, a double-forked daemon the kernel gave to pid 1, is one of them: one grep narrows the
# environments to those holding a NAME=DIR anywhere, and each is then read whole, variable by
# variable. The shell's glob lists the entries, the shell's own test keeps those this user may read,
# and xargs hands them to grep in calls that fit ARG_MAX: as grep's own arguments, past about
# 80,000 processes, grep could not start. Another user's environment, which a walk of a shared
# machine meets by the thousand, is no process of a run, and each would add a complaint below. One
# of this user's that the kernel keeps from this user (/proc/<pid> is this user's and its environ
# root's), or that grep could not read once the test had passed it (a process that made itself not
# dumpable between), is judged once grep is done (unread_may_carry): one that may carry the run's
# mark makes procs_naming name it and fail with 2, since whether it carries the mark was not
# checked, and one that cannot is passed, a zombie or a devbox's ssh-agent from before this shell
# among them. The start is read before and after the environment, and a process that ended or whose
# start moved between is left out. A process grep matched, and one whose read by grep may have
# fallen inside an exec (the bound where its environment starts, read from the stat before and after
# grep, read 0 or moved), is read whole between two reads of its bounds, and the read decides only
# when both show the same bounds, set (exec_state): a read across an exec comes back empty or cut
# short. So a marked process caught inside an exec is still named, and one with no such read at the
# walk's bound of 10 s makes procs_naming say so and fail with 2. Where the kernel places a new
# stack where the old one was (address space randomization off), an exec that began and ended
# between two reads of the bounds is not seen. xargs exits 123 both for a batch whose command exits
# 1 and for one whose command exits 2, and grep exits 1 for no match in its batch, as most batches
# of a busy machine have, and 2 for an error. So each batch runs grep under a sh of its own (batch
# below), which turns grep's no match into 0, passes grep's complaints on, and, for a grep that
# failed and said nothing, says so itself: every failed batch leaves a line of its own, whatever
# another batch printed, and a 123 is always a grep's error. A grep a signal ended ends its sh with
# that signal, which xargs reads as 125. A grep's complaint about an environment this user may not
# read or whose process ended during the walk is no error (proc_walk_complaint), this user's judged
# as above. Any other complaint, and any other status, xargs's own (a batch it could not start, a
# grep a signal ended, 125), like a proc root that lists no process at all (an empty tmpfs where
# /proc should be, or one whose pid entries this user cannot see), make procs_naming print why and
# fail with 2, so no caller reads an unread machine as one with nothing left. So does a shell in a
# pid namespace below the one /proc shows (its own NSpid, read from /proc/self, names more than one
# pid: a shell that `nsenter --pid` put in a namespace without mounting its /proc): there $$ is its
# pid in its own namespace, which /proc numbers as another process, and every read the walk makes of
# this shell and its line of parents would read that other process.
procs_naming() {
  local dir=$1 proc=${proc_root:-/proc} p pid before status=0 name err empties complaint line rest shell comm unread=
  local child child_start at lost='' lineage='' found held again shown walk_snap hold deadline=
  # $0 is the batch's name and "$@" the patterns (-e PATTERN pairs) and the batch's entries. grep's
  # matches go out on fd 3, the walk's own output, and its complaints on stderr. Around grep the
  # batch reads the bound where each entry's environment starts (env_start, field 50 of its stat;
  # NAMING_SNAP) and puts out on fd 4 each entry whose bound read 0 or moved between the two reads
  # (NAMING_MOVED): its process was inside an exec then, which gives it memory whose environment
  # bounds read 0 until the exec is done and whose stack, so its environment, the kernel places
  # anew (address space randomization), and grep's read of it may have come back empty. An awk of
  # the bounds that fails (either read, or their comparison) leaves the batch no list of the entries
  # an exec met, so the batch says which and exits 2, as for a grep that failed: a walk that took
  # its empty list for "none moved" read clean (a reviewer's forcing, status 0, named []). The list
  # of failed reads starts empty in the batch itself, so no value from the caller's environment
  # reads as one.
  local batch='awk_failed=
before=$(awk "$NAMING_SNAP" "$@" 2>/dev/null) || awk_failed="the bounds read before grep (awk exited $?)"
said=$(grep -lzF "$@" 2>&1 >&3 3>&- 4>&-)
status=$?
after=$(awk "$NAMING_SNAP" "$@" 2>/dev/null) || awk_failed="${awk_failed:+$awk_failed, }the bounds read after grep (awk exited $?)"
{ printf "%s\n" "$before"; echo --; printf "%s\n" "$after"; } | awk -F "\t" "$NAMING_MOVED" >&4 ||
  awk_failed="${awk_failed:+$awk_failed, }the bounds compared (awk exited $?)"
[ -z "$said" ] || printf "%s\n" "$said" >&2
[ -z "$awk_failed" ] || echo "awk failed at $awk_failed" >&2
case $status in
  0 | 1) [ -n "$awk_failed" ] || exit 0 ;;
  2) [ -n "$said" ] || echo "grep exited 2 and printed nothing" >&2 ;;
  *)
    [ "$status" -le 128 ] || kill "-$((status - 128))" "$$"
    echo "grep exited $status" >&2
    ;;
esac
exit 2'
  # One line an entry, the entry and its process's env_start, a tab between, or 0 while its exec is
  # still filling in its bounds (start_code, field 26, still 0: exec_state); none for a process gone.
  # The -e PATTERN pairs among the batch's arguments are passed over. Fields after the command name
  # start at the state, field 3, so field 26 is the 24th and field 50 the 48th.
  local snap='BEGIN {
  for (i = 1; i < ARGC; i++) {
    if (ARGV[i] == "-e") { i++; continue }
    s = ARGV[i]; sub(/environ$/, "stat", s); line = ""
    if ((getline line < s) > 0) { sub(/^.*\) /, "", line); n = split(line, v, " "); print ARGV[i] "\t" (n >= 48 && v[24] != 0 ? v[48] : 0) }
    close(s)
  }
  exit
}'
  local moved='!past { if ($0 == "--") { past = 1; next } start[$1] = $2; next }
($1 in start) && (start[$1] != $2 || start[$1] == 0 || $2 == 0) { print $1 }'
  # Where the kernel shows no bounds (bounds_shown), every entry would read 0: no entry is flagged,
  # and only grep's matches are read whole.
  bounds_shown shown
  walk_snap=$snap
  [ -n "$shown" ] || walk_snap='BEGIN { exit }'
  local -a names=("${@:2}") naming_patterns=() naming_files=() empty_files=() listed=() entries=() unread_pids=() ancestors=()
  local -A unread_seen=() named_pids=() grepped=()
  # An empty DIR would match `NAME=` followed by any absolute path (names_dir).
  [ -n "$dir" ] || {
    echo "procs_naming: no run directory given, so no process was named" >&2
    return 2
  }
  local own_nspid
  proc_nspid self own_nspid
  [ "${own_nspid% *}" = "$own_nspid" ] || {
    echo "procs_naming: this shell is pid $$ in a pid namespace below the one /proc shows (NSpid $own_nspid), so its reads of /proc are of other processes and the processes naming $dir were not read" >&2
    return 2
  }
  listed=("$proc"/[0-9]*/environ)
  [ -e "${listed[0]}" ] || {
    echo "procs_naming: $proc lists no process's environ, so the processes naming $dir were not read" >&2
    return 2
  }
  for p in "${listed[@]}"; do
    if [ -r "$p" ]; then
      entries+=("$p")
    elif [ -O "${p%/environ}" ]; then
      pid=${p%/environ}
      pid=${pid##*/}
      unread_seen[$pid]=1
      unread_pids+=("$pid")
    fi
  done
  # This shell's own environment is always one of them on a /proc: none read is no answer.
  [ "${#entries[@]}" -gt 0 ] || {
    echo "procs_naming: $proc lists no environ this user may read, so the processes naming $dir were not read" >&2
    return 2
  }
  for name in "${names[@]}"; do naming_patterns+=(-e "$name=$dir"); done
  err=$(mktemp -d "${TMPDIR:-/tmp}/procs-naming.XXXXXX") || {
    echo "procs_naming: no directory to keep grep's complaints in, so the processes naming $dir were not read" >&2
    return 2
  }
  empties=$err/empty err=$err/said
  # `|| exit "$?"` keeps xargs's status for the wait below, and keeps a caller's ERR trap (`set -E`)
  # from firing inside the substitution for an entry whose process ended during the walk. The status
  # is passed out by value: a bare `exit` in a process or command substitution that a trap runs exits
  # with the status from before the trap (bash 5.2; a `( … )` subshell keeps its own), so inside a
  # teardown's EXIT trap a failed xargs read 0 after a run that passed (the walk took a batch it lost
  # for one that matched nothing) and 1 after one that failed (the teardown failed on a benign 123:
  # Tester4784-3's `xargs exited 1 … xargs printed nothing`).
  # The C locale keeps grep's complaints in the words proc_walk_complaint reads.
  mapfile -t naming_files < <(printf '%s\0' "${entries[@]}" |
    NAMING_SNAP=$walk_snap NAMING_MOVED=$moved LC_ALL=C xargs -0 sh -c "$batch" procs_naming "${naming_patterns[@]}" 3>&1 2>"$err" 4>"$empties" || exit "$?")
  wait "$!" || status=$?
  mapfile -t empty_files <"$empties"
  complaint=
  if [ "$status" != 0 ]; then
    complaint=$(proc_walk_complaint "$err" "$proc") || complaint=
    # A 123 whose every line is an entry each walk of a busy machine meets is an answer. Each failed
    # batch printed a line, so a 123 with none lost its complaints, and any other status is none.
    [ -n "$complaint" ] || [ "$status" != 123 ] || [ -s "$err" ] || complaint="its complaints were lost"
    [ -n "$complaint" ] || [ "$status" = 123 ] || complaint="xargs printed nothing"
    # An environment of this user's that the test passed and grep could not read is judged below.
    while IFS= read -r line; do
      rest=${line#*grep: "$proc"/}
      [[ $rest =~ ^([0-9]+)/environ:\ Permission\ denied$ ]] || continue
      [ ! -O "$proc/${BASH_REMATCH[1]}" ] || [ -n "${unread_seen[${BASH_REMATCH[1]}]:-}" ] || {
        unread_seen[${BASH_REMATCH[1]}]=1
        unread_pids+=("${BASH_REMATCH[1]}")
      }
    done <"$err"
  fi
  rm -rf -- "${err%/said}"
  if [ -n "$complaint" ]; then
    echo "procs_naming: xargs exited $status reading $proc/<pid>/environ: $complaint, so the processes naming $dir were not read" >&2
    return 2
  fi
  # A process grep matched, and one whose read by grep may have fallen inside an exec (its
  # environment's bound read 0 or moved around grep), is read whole (names_dir) between two reads
  # of its bounds (exec_state), and the read decides only when both say `set` and the same: a read
  # of /proc/<pid>/environ reads the memory the process had at the open, and an exec gives it new
  # memory before the bounds of its arguments and environment, so a read across one comes back
  # empty, or cut short at a page where the mark sits further on, and the walk named nothing of a
  # marked process in its exec (34 of 385 reads of a process re-exec'ing itself came back empty;
  # the tester of b285526b59 had 4 of 1,000 cut short). While it is inside one exec (`exec`, its
  # bounds 0 or its start_code not yet set: an exec sets the bounds equal for a moment, and a walk
  # that took that for an environment of no variable missed a process re-exec'ing itself in 3 of
  # 1,000 walks, and a process held there every time) it is waited for, every 10 ms, whatever the
  # walk's bound and whatever its lineage: every exec ends, and a process a loaded machine holds
  # inside one (10.8 s under the exec-window forcing) is no process that cannot be read. Only one
  # held inside one exec for procs_naming_hold_seconds (300) makes the walk print why and fail with
  # 2, never pass. Bounds set and equal after that (`empty`) end the reads: no mark is in an
  # environment of no variable. Where the kernel shows no bounds (bounds_shown), each read decides
  # as it comes. A process whose execs come faster than a read (its bounds moved during each) is
  # read again up to procs_naming_exec_seconds (10) for every such process together. Past it, one
  # grep did not match is judged as an unreadable environment is (naming_may_carry), and one that
  # cannot carry the run's mark is passed; for any other the walk prints why and fails with 2. One
  # the kernel keeps from this user (it cannot be opened, as a process of this user in another user
  # namespace's environment cannot; its stat then shows the bounds as 0 as well) is no exec: it goes
  # to the judgment of this user's unreadable environments below, once. A process listed twice is
  # printed once.
  for p in ${naming_files[@]+"${naming_files[@]}"}; do
    pid=${p%/environ}
    grepped[${pid##*/}]=1
  done
  for p in ${naming_files[@]+"${naming_files[@]}"} ${empty_files[@]+"${empty_files[@]}"}; do
    pid=${p%/environ}
    pid=${pid##*/}
    [ "$pid" != "$$" ] && [ "$pid" != "$BASHPID" ] && [ -z "${named_pids[$pid]:-}" ] || continue
    before=$(proc_start "$pid")
    [ -n "$before" ] || continue
    hold=
    while :; do
      { : <"/proc/$pid/environ"; } 2>/dev/null || {
        [ ! -O "/proc/$pid" ] || [ -n "${unread_seen[$pid]:-}" ] || {
          unread_seen[$pid]=1
          unread_pids+=("$pid")
        }
        break
      }
      exec_state "$pid" "$before" held "$shown"
      case $held in
        gone) break ;;
        exec)
          hold=${hold:-$((SECONDS + ${procs_naming_hold_seconds:-300}))}
          [ "$SECONDS" -lt "$hold" ] || {
            echo "procs_naming: process $pid stayed inside one exec for ${procs_naming_hold_seconds:-300} s, so whether it carries the run's mark was not checked and the processes naming $dir were not read" >&2
            return 2
          }
          sleep 0.01
          continue
          ;;
        empty*) break ;;
        *)
          found=0
          names_dir "$dir" "${names[@]}" "$pid" || found=$?
          exec_state "$pid" "$before" again "$shown"
          if [ "$again" = "$held" ] && [ "$found" != 2 ]; then
            [ "$found" != 0 ] || {
              named_pids[$pid]=1
              printf '%s %s\n' "$pid" "$before"
            }
            break
          fi
          ;;
      esac
      deadline=${deadline:-$((SECONDS + ${procs_naming_exec_seconds:-10}))}
      [ "$SECONDS" -lt "$deadline" ] || {
        if [ -z "${grepped[$pid]:-}" ]; then
          naming_lineage || return 2
          naming_may_carry "$pid" || break
        fi
        echo "procs_naming: process $pid gave no read of its environment within one exec for ${procs_naming_exec_seconds:-10} s, so whether it carries the run's mark was not checked and the processes naming $dir were not read" >&2
        return 2
      }
      sleep 0.01
    done
  done
  if [ "${#unread_pids[@]}" -gt 0 ]; then
    naming_lineage || return 2
    for pid in "${unread_pids[@]}"; do
      naming_may_carry "$pid" || continue
      comm=
      { read -r comm <"/proc/$pid/comm"; } 2>/dev/null || :
      echo "procs_naming: process $pid (${comm//[[:cntrl:]]/ }) is this user's, started after this shell, and its environment cannot be read, so it may carry the run's mark and the processes naming $dir were not checked" >&2
      unread=1
    done
    [ -z "$unread" ] || return 2
  fi
}

# naming_lineage, run inside procs_naming with its locals, reads once what the judgment of whether
# a process of this user may carry the run's mark needs (naming_may_carry): this shell's start
# (shell), and, unless this shell is a child subreaper (lib/subreaper.py names it in
# LEGION_E2E_SUBREAPER as `PID START`, which a child process with a pid of its own, a process that
# took this pid later, and a refused mark, `PID START refused`, do not match; run_descendant needs
# nothing more), its whole line of parents to its top (a parent of 0) as PID:START
# (ancestors), each parent held to a start no later than its child's: a line cut short, by a parent
# that ended and was reaped during the read or whose pid another process took, would take a reaper
# above the cut for a stranger and pass a process of the run (unread_may_carry). Such a line, like a
# shell whose start it cannot read, makes it print why and fail, so the walk is then no answer.
naming_lineage() {
  [ -z "$lineage" ] || return 0
  lineage=1
  shell=$(proc_start "$$")
  [ -n "$shell" ] || lost="this shell $$"
  if [ "${LEGION_E2E_SUBREAPER:-}" != "$$ $shell" ]; then
    child=$$ child_start=$shell
    while [ -z "$lost" ]; do
      p=$(proc_parent "$child")
      [ "$p" != 0 ] || break
      at=
      [ -z "$p" ] || at=$(proc_start "$p")
      if [ -z "$at" ] || [ "$at" -gt "$child_start" ] || [ "${#ancestors[@]}" -ge 4096 ]; then
        lost="the parent ${p:-(none)} of $child"
        break
      fi
      ancestors+=("$p:$at")
      child=$p child_start=$at
    done
  fi
  [ -z "$lost" ] || {
    echo "procs_naming: this shell's line of parents could not be read to its end, at $lost, which ended or whose pid another process took, so this user's processes whose environment could not be read were not checked and the processes naming $dir were not read" >&2
    return 2
  }
}

# naming_may_carry PID, run inside procs_naming once naming_lineage has read, succeeds when PID, a
# process of this user whose environment the walk could not read, may carry the run's mark:
# run_descendant from a shell that is a child subreaper, unread_may_carry from any other.
naming_may_carry() {
  if [ "${LEGION_E2E_SUBREAPER:-}" = "$$ $shell" ]; then
    run_descendant "$1" "$shell"
  else
    unread_may_carry "$1" "$shell" ${ancestors[@]+"${ancestors[@]}"}
  fi
}

# run_marks are the variables that mark a process as one of a stage proof's run, each naming the
# run's scratch directory: LEGION_E2E_RUN, which the run exports once its transcript runs, so every
# program it execs after carries it, and XDG_CONFIG_HOME, for the processes whose environment the
# daemon builds from its allow-list (a pane's, paneEnvAllowList in
# packages/daemon/internal/runtime/tmux/environment.go), which carries no variable of the run's own
# but has its XDG directories moved under the daemon's state directory, which the run puts under its
# scratch directory. A person's shell working in that directory, or a `tail -f` of a file there,
# carries neither, whatever its working directory or command line, so no sweep ends it. Nor does a
# fork of the driver that execs nothing, a background subshell, group or function (`( … ) &`,
# `pane_watcher &`): its /proc/<pid>/environ is the driver's environment at the driver's own exec,
# which an `export` does not rewrite, so no sweep sees it, and each is stopped by the pid it had
# when it started, and ends of itself once the driver has gone where its loop reads `kill -0 "$$"`.
# They are: claude-backend-tmux.sh's, stage3-devbox-workflow.sh's and stage3-4b13b-acceptance.sh's
# pane_watcher (watcher_pid), stage3-4b13b-acceptance.sh's instructor (instructor_pid),
# stage4b-sandbox-tree.sh's watch_raw pod and event watches (watch_pid, events_pid), its node-memory
# and interests samplers (sampler_pid, interests_pid) and its pod_shape_watcher (shape_pid),
# lib/github-apps.sh's token refresher (app_token_refresher) and lib/bridge-tunnel.sh's watchdog
# (bridge_watchdog_pid).
run_marks=(LEGION_E2E_RUN XDG_CONFIG_HOME)

# marked_processes DIR prints `PID START` for every process still alive that carries the mark of the
# run whose scratch directory is DIR (run_marks, procs_naming). It fails with 2, saying why, when
# the walk could not read the machine's processes, and when the caller has not marked its run:
# DIR is empty, or LEGION_E2E_RUN in its own environment is not DIR.
marked_processes() {
  [ -n "$1" ] || {
    echo "marked_processes: no run directory given, so no process of a run was named" >&2
    return 2
  }
  [ "${LEGION_E2E_RUN:-}" = "$1" ] || {
    echo "marked_processes: the run under $1 has not marked its processes (LEGION_E2E_RUN is '${LEGION_E2E_RUN:-}')" >&2
    return 2
  }
  procs_naming "$1" "${run_marks[@]}"
}

# stop_marked_processes DIR [GRACE] ends every process of the run under DIR (marked_processes), with
# everything under each, and no other: each is stopped only through freeze_if, while it is the
# process the walk read and still carries the run's mark, and then ended with its tree
# (kill_frozen), so a child that cleared its environment, and so carries no mark, goes with the
# process that started it. A pid the kernel gave to another process since gets no signal. GRACE, in
# seconds (default 0), gives each SIGTERM first and that long for all of them to exit: a pane's Oh
# My Pi in the middle of a token refresh gets the time to write the rotated token back to the
# operator's database before it goes, where a plain SIGKILL could leave it a refresh token that is
# already spent. Each one's children are read while it is stopped for its TERM, and theirs below,
# under running parents, each parent held to its start and each child to its parent and its own
# start (proc_descendants), so a child it leaves behind when it exits is still ended, by its own
# start time, and a stranger that took a reaped pid below is not. A process still alive after
# GRACE, or every one with no GRACE, is ended with its tree. It prints why and fails with 2 when the
# run's processes could not be read, having signalled nothing, fails with 2 after its signals when
# a process's tree below it could not be read (proc_children), and fails with 2, naming it and
# leaving it running, when a process's check could not read its mark (held_names_dir, a read within
# one exec, as kill_held's).
stop_marked_processes() {
  local dir=$1 grace=${2:-0} pairs pid start i entry status=0 found held
  local -a sent=() alive=() below=()
  pairs=$(marked_processes "$dir") || {
    echo "stop_marked_processes: stopped no process of the run under $dir, since none could be named" >&2
    return 2
  }
  [ -n "$pairs" ] || return 0
  if [ "$grace" -gt 0 ]; then
    while read -r pid start; do
      held=0
      freeze_if "$pid" "$start" STOP held_names_dir "$dir" "${run_marks[@]}" || held=$?
      if [ "$held" = 2 ]; then
        echo "stop_marked_processes: process $pid of the run under $dir could not be checked, so it was left running" >&2
        status=2
        continue
      fi
      [ "$held" = 0 ] || continue
      found=$(proc_descendants "$pid" "$start") || status=2
      [ -z "$found" ] || mapfile -t -O "${#below[@]}" below <<<"$found"
      kill -TERM "$pid" 2>/dev/null || true
      kill -CONT "$pid" 2>/dev/null || true
      sent+=("$pid $start")
    done <<<"$pairs"
    alive=(${sent[@]+"${sent[@]}"})
    for ((i = 0; i < grace * 5 && ${#alive[@]} > 0; i++)); do
      sleep 0.2
      sent=("${alive[@]}")
      alive=()
      for entry in "${sent[@]}"; do
        [ "$(proc_start "${entry%% *}")" != "${entry#* }" ] || alive+=("$entry")
      done
    done
    pairs=
    [ "${#alive[@]}" = 0 ] || pairs=$(printf '%s\n' "${alive[@]}")
  fi
  kill_held "$dir" "${run_marks[@]}" <<<"$pairs" || status=2
  for entry in ${below[@]+"${below[@]}"}; do
    freeze_if "${entry%% *}" "${entry#* }" KILL || true
  done
  return "$status"
}

# proc_describe PID... prints one line for each PID that runs: its pid, parent, process group,
# session, start time and command line, in the fields of `ps -o pid,ppid,pgid,sid,lstart,args -p`,
# read from that process alone and from /proc/stat, whose btime turns a start into a clock time.
# The command line's words are joined by spaces, and each control character becomes a space, which
# keeps one process on one line; procps-ng prints a control character as `?` instead, so a line of
# this and a line of ps are not the same bytes. A PID that is gone prints nothing, as ps prints no
# line for it. A host with no /proc asks ps.
proc_describe() {
  local pid stat boot ticks IFS=' '
  local -a fields words
  if [ ! -d /proc/self ]; then
    IFS=,
    ps -o pid,ppid,pgid,sid,lstart,args -p "$*" 2>&1 || true
    return 0
  fi
  boot=$(awk '$1 == "btime" { print $2 }' /proc/stat)
  ticks=$(getconf CLK_TCK)
  for pid in "$@"; do
    stat=
    { IFS= read -r -d '' stat <"/proc/$pid/stat"; } 2>/dev/null || :
    [ -n "$stat" ] || continue
    read -r -a fields <<<"${stat##*) }"
    words=()
    { mapfile -d '' -t words <"/proc/$pid/cmdline"; } 2>/dev/null || true
    words=(${words[@]+"${words[@]//[[:cntrl:]]/ }"})
    printf '%s ppid %s pgid %s sid %s started %s: %s\n' "$pid" "${fields[1]}" "${fields[2]}" \
      "${fields[3]}" "$(date -d "@$((boot + fields[19] / ticks))" '+%a %b %e %T %Y')" "${words[*]-}"
  done
}

# launched PID EXE… prints the one process below PID, in the process tree PID started, that runs
# one of the EXEs by the kernel's record (/proc/<pid>/exe), whatever name it was called by: below a
# pane, past the `sh -c` that runs its command as a child, the `legion worker-shim` and the `mise x`
# that execs, down to the agent. The walk goes below no process that runs an EXE, so the agent's own
# children (its daemon broker, its tools, an Oh My Pi a Claude Code runs) are none of it, and it
# passes every other process on the way, among them a helper a shim runs beside its agent
# (agent-secrets' keygen and renew, internal/shim/agentsecrets.go), which a walk that took only
# single children would stop at. A child that has forked and not yet exec'd still runs its parent's
# program, so it is never taken for the agent. Each EXE is a native program (native_program): a
# script's process runs its interpreter. It prints why and fails when no process there runs an
# EXE, naming each process it passed and the program it runs, and when more than one does: it
# picks none.
launched() {
  local top=$1 exe want pid child seen hit next=0
  local -a programs=() pending matches=() walked=()
  shift
  for exe in "$@"; do
    want=$(native_program "$exe") || {
      printf '%s\n' "$want"
      return 1
    }
    programs+=("$want")
  done
  pending=("$top")
  while [ "$next" -lt "${#pending[@]}" ]; do
    pid=${pending[next]}
    next=$((next + 1))
    for child in $(proc_children "$pid"); do
      seen=$(readlink "/proc/$child/exe" 2>/dev/null || :)
      hit=
      for want in "${programs[@]}"; do [ "$seen" != "$want" ] || hit=1; done
      if [ -n "$hit" ]; then
        matches+=("$child")
      else
        pending+=("$child")
        walked+=("$child ${seen:-(unreadable)}")
      fi
    done
  done
  case ${#matches[@]} in
    1) printf '%s\n' "${matches[0]}" ;;
    0)
      printf 'no process below %s runs %s' "$top" "${programs[*]}"
      [ "${#walked[@]}" = 0 ] || printf '; below it: %s' "$(IFS=,; printf '%s' "${walked[*]}")"
      printf '\n'
      return 1
      ;;
    *)
      printf '%s processes below %s run %s: %s\n' "${#matches[@]}" "$top" "${programs[*]}" "${matches[*]}"
      return 1
      ;;
  esac
}

# native_program FILE prints FILE's real path when it is a program the kernel runs as itself, and
# fails saying why otherwise: a file that is not there; a script (`#!`), whose process runs its
# interpreter and, behind a wrapper such as a dotfiles shim, ends in another program the script
# picks; and a mise shim (a link to mise under another name), which execs the version mise picks
# for the directory. No process ever runs such a FILE by the kernel's record.
native_program() {
  local path magic=
  path=$(realpath -e -- "$1" 2>/dev/null) || {
    printf '%s is no program on disk\n' "$1"
    return 1
  }
  { IFS= read -r -n 2 magic <"$path"; } 2>/dev/null || :
  if [ "$magic" = '#!' ]; then
    printf '%s is a script, not the program the process that runs it runs: name the binary it ends in\n' "$1"
    return 1
  fi
  if [ "${path##*/}" = mise ] && [ "${1##*/}" != mise ]; then
    printf '%s is a mise shim, which execs the version mise picks: name that binary (mise which %s)\n' "$1" "${1##*/}"
    return 1
  fi
  printf '%s\n' "$path"
}

# first_native NAME prints the real path of the first NAME on PATH that is the program itself
# (native_program), past every wrapper script and mise shim, as a devbox's dotfiles wrappers find
# the binary they hand a call to; each entry of `type -ap` is one path, spaces and all. It fails,
# naming each candidate it passed, when no NAME on PATH is one.
first_native() {
  local native_candidate native_found
  local -a native_candidates=() native_refusals=()
  mapfile -t native_candidates < <(type -ap -- "$1")
  for native_candidate in ${native_candidates[@]+"${native_candidates[@]}"}; do
    if native_found=$(native_program "$native_candidate"); then
      printf '%s\n' "$native_found"
      return 0
    fi
    native_refusals+=("$native_found")
  done
  printf 'no %s on PATH is the program itself' "$1"
  [ "${#native_refusals[@]}" = 0 ] || printf ': %s' "$(IFS=';'; printf '%s' "${native_refusals[*]}")"
  printf '\n'
  return 1
}

# child_of PID prints PID's one child (proc_children), and fails naming what it found when PID has
# none or more than one.
child_of() {
  local -a children
  mapfile -t children < <(proc_children "$1")
  case ${#children[@]} in
    1) printf '%s\n' "${children[0]}" ;;
    0)
      printf 'process %s has no child\n' "$1"
      return 1
      ;;
    *)
      printf 'process %s has %s children: %s\n' "$1" "${#children[@]}" "${children[*]}"
      return 1
      ;;
  esac
}

# child_running PID EXE prints the one child of PID itself, not a process further down, that runs
# EXE by the kernel's record (native_program), and fails naming PID's children and what each runs
# when none or more than one does: a pane's `legion worker-shim` is its process's child that runs
# the run's legion, and a Claude Code pane's `legion claude-bridge` is that shim's.
child_running() {
  local want child seen
  local -a hits=() walked=()
  want=$(native_program "$2") || {
    printf '%s\n' "$want"
    return 1
  }
  for child in $(proc_children "$1"); do
    seen=$(readlink "/proc/$child/exe" 2>/dev/null || :)
    walked+=("$child ${seen:-(unreadable)}")
    [ "$seen" != "$want" ] || hits+=("$child")
  done
  if [ "${#hits[@]}" = 1 ]; then
    printf '%s\n' "${hits[0]}"
    return 0
  fi
  printf '%s children of %s run %s; its children: %s\n' "${#hits[@]}" "$1" "$want" "$(IFS=,; printf '%s' "${walked[*]:-none}")"
  return 1
}

# omp_program PIN prints the program a Legion process started from this shell's environment runs
# for `omp_invocation: mise x PIN -- omp`: LEGION_OMP_PATH's real path when the environment sets
# it, since it outranks the invocation (ResolveInvocation, packages/daemon/internal/omplaunch), and
# PIN's own bin/omp otherwise, refused when it is a script (native_program). A rig whose daemon runs
# with LEGION_OMP_PATH unset names PIN's bin/omp itself.
omp_program() {
  if [ -n "${LEGION_OMP_PATH:-}" ]; then
    native_program "$LEGION_OMP_PATH"
  else
    native_program "$(mise where "$1")/bin/omp"
  fi
}
