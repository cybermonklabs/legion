# shellcheck shell=bash
# arg_max_force ENTRY ROOM SCRATCH OLD_FORM puts the calling shell under the force where a row proves
# that a walk of every process does not hand the machine's /proc entries to a program as its
# arguments. Sourced by a test's shell, never by a proof.
#
# A 512 KiB stack makes ARG_MAX 128 KiB, and PADDING, exported, fills the environment to ROOM bytes
# short of the largest that still lets a program of no arguments start, measured here. An exec
# handed every /proc/<pid>/ENTRY as an argument then fails with E2BIG, as one does unforced at about
# 80,000 processes, while an exec of a few arguments still starts. A machine with too few processes
# for every entry together to outgrow twice ROOM gets more first: sleeps this starts, recorded by
# pid and ended when the shell exits (an EXIT trap of its own, so the caller sets none). The control
# the force must pass runs here: OLD_FORM, the command line the walk replaced, run through eval, must
# fail with E2BIG, and a program of no arguments must still start. SCRATCH is a directory the caller
# owns, where the control keeps its stderr for the time of a read. Any failure of the force exits
# the shell with 1 and a line naming it.
arg_max_force() {
  local entry=$1 room=$2 scratch=$3 old=$4 true_bin lo=0 hi=131072 mid err line
  ulimit -s 512
  [ "$(getconf ARG_MAX)" = 131072 ] || {
    echo "ARG_MAX is $(getconf ARG_MAX), not 131072, under a 512 KiB stack" >&2
    exit 1
  }
  force_sleepers=()
  trap arg_max_force_end EXIT
  while [ "$(arg_max_force_glob "$entry")" -lt $((2 * room)) ]; do
    [ "${#force_sleepers[@]}" -lt 5000 ] || {
      echo "5000 more processes still leave every /proc/<pid>/$entry under twice $room bytes" >&2
      exit 1
    }
    for _ in $(seq 50); do
      sleep 300 >/dev/null 2>&1 &
      force_sleepers+=("$!")
    done
  done
  true_bin=$(type -P true)
  while [ $((hi - lo)) -gt 64 ]; do
    mid=$(((lo + hi) / 2))
    if PADDING=$(printf "%${mid}s" "") "$true_bin" 2>/dev/null; then lo=$mid; else hi=$mid; fi
  done
  [ "$lo" -gt $((room + 4096)) ] || {
    echo "no room for the padding: a program starts with only $lo bytes of it" >&2
    exit 1
  }
  export PADDING
  PADDING=$(printf "%${lo}s" "")
  PADDING=${PADDING:room}
  "$true_bin" || {
    echo "a program of no arguments does not start under the force" >&2
    exit 1
  }
  err=$(mktemp "$scratch/force.XXXXXX")
  if eval "$old" >/dev/null 2>"$err"; then
    echo "the old form started: the force is not in force" >&2
    exit 1
  fi
  { IFS= read -r line <"$err"; } || line=
  rm -f -- "$err"
  [[ $line == *"Argument list too long"* ]] || {
    echo "the old form failed otherwise: $line" >&2
    exit 1
  }
}

# arg_max_force_glob ENTRY prints the bytes every /proc/<pid>/ENTRY takes as arguments: the path, its
# terminating zero and its pointer.
arg_max_force_glob() {
  local a n=0
  for a in /proc/[0-9]*/"$1"; do n=$((n + ${#a} + 9)); done
  echo "$n"
}

# arg_max_force_end ends the sleeps arg_max_force started, by the pids it recorded.
arg_max_force_end() {
  [ "${#force_sleepers[@]}" = 0 ] || { kill "${force_sleepers[@]}" && wait "${force_sleepers[@]}"; } 2>/dev/null || :
}
