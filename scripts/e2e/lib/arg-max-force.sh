# shellcheck shell=bash
# arg_max_force ENTRY ROOM SCRATCH OLD_FORM puts the calling shell under the force where a row proves
# that a walk of every process does not hand the proc root's entries to a program as its arguments.
# Sourced by a test's shell, never by a proof.
#
# The walk under test reads `$proc_root`, a proc root the row made of its own processes
# (scripts/e2e/lib/proc-root.ts), never the host's /proc, so the row's time is its own and not the
# host's process count. A 512 KiB stack makes ARG_MAX 128 KiB, and PADDING, exported, fills the
# environment to ROOM bytes short of the largest that still lets a program of no arguments start,
# measured here. An exec handed every $proc_root/<pid>/ENTRY as an argument then fails with E2BIG, as
# one does unforced at about 80,000 processes, while an exec of a few arguments still starts. A root
# whose entries together are under twice ROOM gets more first: entries of its own at pids above the
# kernel's pid_max, which no process can hold, each an empty `environ` and empty `fd` and `cwd`
# directories, so they name no process and no socket of any walk. They are counted in the shell and
# made with one mkdir, so the force costs a row the same few programs however many it needs.
# must pass runs here: OLD_FORM, the command line the walk replaced, run through eval, must fail
# with E2BIG, and a program of no arguments must still start. SCRATCH is a directory the caller
# owns, where the control keeps its stderr for the time of a read. Any failure of the force exits
# the shell with 1 and a line naming it.
arg_max_force() {
  local entry=$1 room=$2 scratch=$3 old=$4 true_bin lo=0 hi=131072 mid err line unreal have n pad
  local -a made=() dirs=()
  [ -n "${proc_root:-}" ] && [ -d "$proc_root" ] || {
    echo "arg_max_force: no proc root of the row's own (proc_root) to force" >&2
    exit 1
  }
  ulimit -s 512
  [ "$(getconf ARG_MAX)" = 131072 ] || {
    echo "ARG_MAX is $(getconf ARG_MAX), not 131072, under a 512 KiB stack" >&2
    exit 1
  }
  unreal=$(</proc/sys/kernel/pid_max)
  have=$(arg_max_force_glob "$entry")
  # Each new entry adds its path ($proc_root/<pid>/ENTRY), its terminating zero and its pointer.
  while [ "$have" -lt $((2 * room)) ]; do
    unreal=$((unreal + 1))
    made+=("$unreal")
    have=$((have + ${#proc_root} + ${#unreal} + ${#entry} + 2 + 9))
  done
  if [ "${#made[@]}" -gt 0 ]; then
    for n in "${made[@]}"; do dirs+=("$proc_root/$n/fd" "$proc_root/$n/cwd"); done
    mkdir -p "${dirs[@]}"
    for n in "${made[@]}"; do : >"$proc_root/$n/environ"; done
  fi
  true_bin=$(type -P true)
  # Each padding is built in the shell (printf -v), so a step of the search starts one program.
  while [ $((hi - lo)) -gt 64 ]; do
    mid=$(((lo + hi) / 2))
    printf -v pad "%${mid}s" ""
    if PADDING=$pad "$true_bin" 2>/dev/null; then lo=$mid; else hi=$mid; fi
  done
  [ "$lo" -gt $((room + 4096)) ] || {
    echo "no room for the padding: a program starts with only $lo bytes of it" >&2
    exit 1
  }
  export PADDING
  printf -v PADDING "%$((lo - room))s" ""
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

# arg_max_force_glob ENTRY prints the bytes every $proc_root/<pid>/ENTRY takes as arguments: the
# path, its terminating zero and its pointer.
arg_max_force_glob() {
  local a n=0
  for a in "$proc_root"/[0-9]*/"$1"; do [ ! -e "$a" ] || n=$((n + ${#a} + 9)); done
  echo "$n"
}
