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
# Sourced, never run, by lib/rig.sh and by stage2-tmux-supervision.sh, which sources no rig. It
# needs nothing of its caller's.

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
  { read -r stat <"/proc/$pid/stat"; } 2>/dev/null || return 1
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
# that is gone. A host with no /proc (macOS) asks ps and pgrep. The files are read with `read`:
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
proc_stat_field() {
  local stat
  local -a fields
  if [ -d /proc/self ]; then
    { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 0
    read -r -a fields <<<"${stat##*) }"
    printf '%s\n' "${fields[$2 - 3]}"
  else
    ps -o "$3=" -p "$1" 2>/dev/null | tr -d ' '
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
  [ "$(proc_stat_field "$1" 3 state)" != Z ] || return 0
  proc_stat_field "$1" 22 lstart
}
# proc_children asks pgrep, which walks every process for its parent, where /proc cannot answer: a
# host with no /proc (macOS), and a kernel built without CONFIG_PROC_CHILDREN, whose /proc has no
# task/<tid>/children file at all, so the read below would print no child of a process that has
# children and stop_tree would leave them running.
proc_children() {
  local list
  local -a more
  if [ ! -d /proc/self ] || { [ -d "/proc/$1" ] && [ ! -e "/proc/$1/task/$1/children" ]; }; then
    pgrep -P "$1" || true
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

# process_args PID NAME reads PID's arguments into the array NAME from the NUL-delimited
# /proc/PID/cmdline, in this shell: no pipe stands between the read and the check, so nothing can
# end early on it. It fails when the entry cannot be read or holds no argument (the process has
# gone).
process_args() {
  local -n _args=$2
  { mapfile -d '' -t _args <"/proc/$1/cmdline"; } 2>/dev/null && [ "${#_args[@]}" -gt 0 ]
}
# env_of PID NAME prints every value PID's environment gives NAME, one a line, read in this shell
# from the NUL-delimited /proc/PID/environ with no pipe between the read and the check: a
# `tr | grep -q` stops reading at its match, tr's SIGPIPE is the pipeline's status under pipefail,
# and a negated check reads that as absent. It fails with 2 when the environment cannot be read or
# is empty (the process has exited), which is never the same as a clean one.
env_of() {
  local entry
  local -a entries=()
  { mapfile -d '' -t entries <"/proc/$1/environ"; } 2>/dev/null && [ "${#entries[@]}" -gt 0 ] || return 2
  for entry in "${entries[@]}"; do
    case $entry in "$2="*) printf '%s\n' "${entry#*=}" ;; esac
  done
}
# env_has PID NAME succeeds when PID's environment names NAME, with any value or none, fails with 1
# when it does not, and with 2 when it cannot be read (env_of). A check that a variable is absent
# holds only on status 1.
env_has() {
  local entry
  local -a entries=()
  { mapfile -d '' -t entries <"/proc/$1/environ"; } 2>/dev/null && [ "${#entries[@]}" -gt 0 ] || return 2
  for entry in "${entries[@]}"; do
    case $entry in "$2="*) return 0 ;; esac
  done
  return 1
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
# that started at START (proc_start) and CHECK, a command given PID as its last word, succeeds: it
# stops PID first (SIGSTOP), so the process cannot end and its pid pass to another between the
# check and the signal, checks, signals, and thaws it (SIGCONT) unless SIGNAL is KILL or STOP. On a
# mismatch it thaws PID and fails, sending nothing; it fails too when PID cannot be signalled
# (signalable) or stopped. A process of another user cannot be stopped at all, and one of this
# user's that took the pid stands still only for the check.
freeze_if() {
  local pid=$1 start=$2 signal=$3
  shift 3
  signalable "$pid" || return 1
  kill -STOP "$pid" 2>/dev/null || return 1
  if [ "$(proc_start "$pid")" != "$start" ] || { [ "$#" -gt 0 ] && ! "$@" "$pid"; }; then
    kill -CONT "$pid" 2>/dev/null || true
    return 1
  fi
  kill -"$signal" "$pid" 2>/dev/null || true
  case $signal in KILL | STOP) ;; *) kill -CONT "$pid" 2>/dev/null || true ;; esac
}

# names_dir DIR NAME… PID succeeds when PID's environment, read whole in this shell with no pipe,
# gives one of the NAMEs the value DIR or a path under DIR; fails with 1 when none has that value,
# and with 2 when the environment cannot be read or is empty (the process has gone). PID comes last,
# so freeze_if can run it as a CHECK.
names_dir() {
  local dir=$1 pid=${!#} entry name
  local -a names=("${@:2:$# - 2}") entries=()
  { mapfile -d '' -t entries <"/proc/$pid/environ"; } 2>/dev/null && [ "${#entries[@]}" -gt 0 ] || return 2
  for entry in "${entries[@]}"; do
    for name in "${names[@]}"; do
      case $entry in "$name=$dir" | "$name=$dir"/*) return 0 ;; esac
    done
  done
  return 1
}

# procs_naming DIR NAME… prints `PID START` (proc_start) for each process whose environment gives
# one of the NAMEs the value DIR or a path under it (names_dir), this shell itself left out. It reads
# every process's environment, since a process that left every tree the run recorded, a
# double-forked daemon the kernel gave to pid 1, is one of them: one grep narrows the environments to
# those holding a NAME=DIR anywhere, and each is then read whole, variable by variable. The shell's
# glob hands the entries to grep through xargs, which splits them into calls that fit ARG_MAX: as
# grep's own arguments, past about 80,000 processes, grep could not start. The start is read before
# and after the environment, and a process that ended or whose start moved between is left out.
# xargs exits 123 when a batch's grep exits 1 or 2 (no match in it, or an environment this user may
# not read, another user's, or whose process ended during the walk), which is no error here. Any
# other status is xargs's own, a batch it could not start or a grep a signal ended (125), and like a
# machine with no /proc to read it makes procs_naming print why and fail with 2, so no caller reads
# an unread machine as one with nothing left.
procs_naming() {
  local dir=$1 p pid before status=0 name
  local -a names=("${@:2}") naming_patterns=() naming_files=()
  [ -r /proc/self/environ ] || {
    echo "procs_naming: there is no /proc/<pid>/environ to read, so the processes naming $dir were not read" >&2
    return 2
  }
  for name in "${names[@]}"; do naming_patterns+=(-e "$name=$dir"); done
  # `|| exit` keeps xargs's status for the wait below, and keeps a caller's ERR trap (`set -E`) from
  # firing inside the substitution for the batches every walk has with no match.
  mapfile -t naming_files < <(printf '%s\0' /proc/[0-9]*/environ | xargs -0 grep -lzsF "${naming_patterns[@]}" || exit)
  wait "$!" || status=$?
  case $status in
    0 | 123) ;;
    *)
      echo "procs_naming: xargs exited $status reading /proc/<pid>/environ, so the processes naming $dir were not read" >&2
      return 2
      ;;
  esac
  for p in ${naming_files[@]+"${naming_files[@]}"}; do
    pid=${p#/proc/}
    pid=${pid%/environ}
    [ "$pid" != "$$" ] && [ "$pid" != "$BASHPID" ] || continue
    before=$(proc_start "$pid")
    [ -n "$before" ] || continue
    names_dir "$dir" "${names[@]}" "$pid" || continue
    [ "$(proc_start "$pid")" = "$before" ] || continue
    printf '%s %s\n' "$pid" "$before"
  done
}

# run_marks are the variables that mark a process as one of a stage proof's run, each naming the
# run's scratch directory: LEGION_E2E_RUN, which the run exports once its transcript runs, so every
# process it starts after carries it, and XDG_CONFIG_HOME, for the processes whose environment the
# daemon builds from its allow-list (a pane's, paneEnvAllowList in
# packages/daemon/internal/runtime/tmux/environment.go), which carries no variable of the run's own
# but has its XDG directories moved under the daemon's state directory, which the run puts under its
# scratch directory. A person's shell working in that directory, or a `tail -f` of a file there,
# carries neither, whatever its working directory or command line, so no sweep ends it.
run_marks=(LEGION_E2E_RUN XDG_CONFIG_HOME)

# marked_processes DIR prints `PID START` for every process still alive that carries the mark of the
# run whose scratch directory is DIR (run_marks, procs_naming). It fails with 2, saying why, when
# the walk could not read the machine's processes, and when the caller has not marked its run:
# LEGION_E2E_RUN in its own environment is not DIR.
marked_processes() {
  [ "${LEGION_E2E_RUN:-}" = "$1" ] || {
    echo "marked_processes: the run under $1 has not marked its processes (LEGION_E2E_RUN is '${LEGION_E2E_RUN:-}')" >&2
    return 2
  }
  procs_naming "$1" "${run_marks[@]}"
}

# stop_marked_processes DIR [GRACE] ends every process of the run under DIR (marked_processes), and
# no other: each is signalled only through freeze_if, while it is the process the walk read and
# still carries the run's mark, so a pid the kernel gave to another process since is thawed and left.
# GRACE, in seconds (default 0), gives each SIGTERM first and that long for all of them to exit: a
# pane's Oh My Pi in the middle of a token refresh gets the time to write the rotated token back to
# the operator's database before it goes, where a plain SIGKILL could leave it a refresh token that
# is already spent. A process still alive after GRACE, or every one with no GRACE, gets SIGKILL. It
# prints why and fails with 2 when the run's processes could not be read, having signalled nothing.
stop_marked_processes() {
  local dir=$1 grace=${2:-0} pairs pid start i
  local -a sent=() alive=()
  pairs=$(marked_processes "$dir") || {
    echo "stop_marked_processes: stopped no process of the run under $dir, since none could be named" >&2
    return 2
  }
  [ -n "$pairs" ] || return 0
  if [ "$grace" -gt 0 ]; then
    while read -r pid start; do
      freeze_if "$pid" "$start" TERM names_dir "$dir" "${run_marks[@]}" && sent+=("$pid $start")
    done <<<"$pairs"
    alive=("${sent[@]}")
    for ((i = 0; i < grace * 5 && ${#alive[@]} > 0; i++)); do
      sleep 0.2
      sent=("${alive[@]}")
      alive=()
      for pid in "${sent[@]}"; do
        [ "$(proc_start "${pid%% *}")" != "${pid#* }" ] || alive+=("$pid")
      done
    done
    [ "${#alive[@]}" -gt 0 ] || return 0
    pairs=$(printf '%s\n' "${alive[@]}")
  fi
  while read -r pid start; do
    freeze_if "$pid" "$start" KILL names_dir "$dir" "${run_marks[@]}" || true
  done <<<"$pairs"
  return 0
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
    { read -r stat <"/proc/$pid/stat"; } 2>/dev/null || continue
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
  local top=$1 exe want pid child seen hit
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
  while [ "${#pending[@]}" -gt 0 ]; do
    pid=${pending[0]}
    pending=("${pending[@]:1}")
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
