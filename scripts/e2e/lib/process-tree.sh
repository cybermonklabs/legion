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

# children_of PID prints PID's children, one a line, read from PID itself, every thread's list
# (/proc/<pid>/task/*/children), since a Go process forks from whichever thread runs the call. A
# PID that has gone has none.
children_of() {
  local list
  local -a more
  for list in /proc/"$1"/task/*/children; do
    more=()
    { read -r -a more <"$list"; } 2>/dev/null || :
    [ "${#more[@]}" = 0 ] || printf '%s\n' "${more[@]}"
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
    for child in $(children_of "$pid"); do
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

# child_of PID prints PID's one child (children_of), and fails naming what it found when PID has
# none or more than one.
child_of() {
  local -a children
  mapfile -t children < <(children_of "$1")
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
  for child in $(children_of "$1"); do
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
