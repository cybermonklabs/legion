#!/usr/bin/env bash
# A stage driver's check that it is not starting beside what an earlier run of the same stage left
# running. A driver that dies with no chance to clean up (SIGKILL, a crashed host) leaves its daemon,
# its listener, its agents' processes and its containers behind, and a later run would otherwise
# start beside them without a word. Everything a run starts carries the pid of the driver that
# started it: each container is named <prefix>-<role>-<pid>, and every process of the run holds its
# work directory, /tmp/<prefix>.<pid>.<random>, in its argv, its working directory or its
# environment (an agent only as its working directory, a bridge only through the state directories
# it is handed). A run of the same stage that is still alive owns its own, and two lanes may run a
# stage at once, so only what a dead driver started is a leftover.
#
# A Postgres is the exception, and no judge of this file's: every shell stack's Postgres container
# carries the mark every test stack's Postgres carries (packages/dispatch/e2e/
# harness-postgres-marks.ts), and the run's start removes, before it starts its own, each Postgres
# whose mark names a process that is gone, the browser pool's among them, by the one removal each
# start of a stack runs (packages/dispatch/e2e/postgres-mark.ts), the same way on a dev box and in
# CI. Each needs bun, and a start with no bun on PATH fails with a line that names it:
#   remove_leftover_postgres     removes each Postgres of a stack that is gone, leaves each other,
#                                and names on stderr each it leaves, one line per kind.
#   postgres_mark RUN CONTAINER  prints the --label value that marks CONTAINER as the Postgres of
#                                the run whose work directory is RUN, owned by this shell ($$).
# refuse_leftovers leaves a marked container to that removal.

_postgres_mark_cli=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/packages/dispatch/e2e/postgres-mark.ts

# _postgres_mark ARGS... runs the command with ARGS, or fails, naming bun, where there is no bun.
_postgres_mark() {
  command -v bun >/dev/null || {
    echo "lib/leftovers.sh: bun is required: the mark and the removal of a stack's Postgres run through bun $_postgres_mark_cli" >&2
    return 1
  }
  bun "$_postgres_mark_cli" "$@"
}

remove_leftover_postgres() {
  _postgres_mark sweep
}

postgres_mark() {
  _postgres_mark label "$$" "$1" "$2"
}

# proc_walk_complaint, which reads the stderr of a walk of every process (lib/process-tree.sh).
# shellcheck source-path=SCRIPTDIR source=process-tree.sh
declare -F proc_walk_complaint >/dev/null || . "${BASH_SOURCE[0]%/*}/process-tree.sh"

# leftover_started PID prints the process's start time, in clock ticks since boot (proc(5), stat
# field 22), and fails for a pid with no process or a zombie. The stat is read whole, in this
# shell, and cut after its last `) `: the command name, field 2, is parenthesised and may hold
# spaces, a `) ` or a newline; the fields after it hold none.
leftover_started() {
  local stat=
  local -a stat_fields
  { IFS= read -r -d '' stat <"/proc/$1/stat"; } 2>/dev/null || :
  [ -n "$stat" ] || return 1
  read -r -a stat_fields <<<"${stat##*) }"
  [ "${stat_fields[0]}" != Z ] || return 1
  printf '%s\n' "${stat_fields[19]}"
}

# leftover_owner OWNER SINCE: OWNER is a live process that started no later than SINCE (clock ticks
# since boot), so it is the driver that started what SINCE dates, not a later process given its pid.
leftover_owner() {
  local start
  start=$(leftover_started "$1") || return 1
  [ "$start" -le "$2" ]
}

# leftover_now_us VAR sets VAR to the wall clock in microseconds since the epoch: from bash's own
# EPOCHREALTIME, with no process started, so two reads are microseconds apart, or from date.
leftover_now_us() {
  if [ -n "${EPOCHREALTIME:-}" ]; then printf -v "$1" '%s' "${EPOCHREALTIME//[^0-9]/}"; else printf -v "$1" '%s' "$(date +%s%6N)"; fi
}

# leftover_boot_ns prints the earliest moment the machine can have booted, in nanoseconds since
# the epoch, and it is that moment to within a millisecond. /proc/uptime counts the time since the
# boot in hundredths of a second, and the clock of a process's start counts it in ticks; btime of
# /proc/stat is a whole second. So the wall clock is read at a step of the count, when the time
# since the boot is exactly what the count says: the step to U fell between the start of the read
# that still said less and the end of the read that says U, and the boot is U before that start.
# A step that the scheduler stretches past a millisecond is read again, up to 50 times; past that
# the boot is the earliest of the last, wider, window.
leftover_boot_ns() {
  local up last='' before last_before='' after tries=0
  while :; do
    leftover_now_us before
    read -r up _ </proc/uptime
    leftover_now_us after
    up=${up//[^0-9]/}
    if [ -n "$last" ] && [ "$up" != "$last" ]; then
      if [ $((10#$after - 10#$last_before)) -le 1000 ] || [ "$tries" -ge 50 ]; then
        printf '%s\n' "$(((10#$last_before - 10#$up * 10000) * 1000))"
        return 0
      fi
      tries=$((tries + 1))
    fi
    last=$up last_before=$before
  done
}

# refuse_leftovers PREFIX fails, naming each one and the command that removes it, when a container
# or a process of an earlier PREFIX run outlived its driver. The caller's `fail` reports it. It
# reads the processes of `${proc_root:-/proc}`: the host's /proc, unless a test points proc_root at
# a proc root of its own (links to the /proc entries of the processes it started,
# scripts/e2e/lib/proc-root.ts), since a walk of the host's whole /proc holds the test to the host's
# processes, an earlier run's among them. A process names its run by the first of its command line,
# its environment and its working directory that holds /tmp/<PREFIX>.<driver pid>.: the first two
# are read in this shell (mapfile), and the working directories of the processes neither names a
# run in are read by one `find` for them all, so the walk starts no program for each process. A
# proc root that lists no process at all (an empty tmpfs where /proc should be), and a `find` that
# failed, a signal ended or that is too old to read -files0-from (findutils before 4.9), are
# reported through `fail`, naming them, with status 2: a walk that read nothing found no leftover
# only because it saw nothing. A working directory this user may not read, another user's
# process's, or one whose process ended during the walk, is no complaint (proc_walk_complaint).
#
# A container's name ends in its driver's pid, or in that pid and the run's 16 random hex digits
# (<prefix>-<role>-<pid>-<random>, unique where two PID namespaces share a pid). A container that
# carries the Postgres mark is the removal's (remove_leftover_postgres, above), and no leftover here.
#
# A container's owner is the live process of its pid that started no later than the container was
# made, compared at the precision both clocks give: the owner's start in clock ticks since the
# boot, and Docker's creation in nanoseconds, turned into ticks since the boot (leftover_boot_ns).
# Where the two fall in one tick the owner counts as alive: a driver that made its container a few
# milliseconds after it started is never a leftover, and a later process given a dead driver's pid
# is taken for it only where it started in the tick (10 ms at 100 a second) its container was made.
refuse_leftovers() {
  local prefix=$1 proc=${proc_root:-/proc} name mark owner created p path target since boot tick i
  local err complaint status=0
  local -a leftover_list=() entries=() args=() environ=() undecided=() cwd_links=()
  local -A owner_of=()
  boot=$(leftover_boot_ns)
  tick=$((1000000000 / $(getconf CLK_TCK)))
  while IFS='|' read -r name mark; do
    [ -z "$mark" ] || continue
    [[ $name =~ -([0-9]+)(-[0-9a-f]{16})?$ ]] || continue
    owner=${BASH_REMATCH[1]}
    created=$(docker inspect -f '{{.Created}}' "$name" 2>/dev/null) || continue
    created=$(date -d "$created" +%s%N) || continue
    # A container made before this boot has no owner among the processes of this boot.
    since=-1
    [ "$created" -lt "$boot" ] || since=$(((created - boot) / tick))
    leftover_owner "$owner" "$since" || leftover_list+=("container $name (docker rm -f -v $name)")
  done < <(docker ps -a --filter "name=^$prefix-" --format '{{.Names}}|{{.Label "dev.legion.dispatch-e2e.postgres-mark"}}')
  entries=("$proc"/[0-9]*)
  [ -e "${entries[0]}" ] || {
    fail "refuse_leftovers: $proc lists no process, so whether an earlier $prefix run left processes running was not read"
    return 2
  }
  for p in "${entries[@]}"; do
    # A pid is its namespace's own: a process in another pid namespace (an agent box) names a
    # driver pid that means nothing here, and one of another user's reads as nothing.
    [ "$p/ns/pid" -ef /proc/self/ns/pid ] || continue
    # A process that ends between the listing and the reads has nothing left to read; it is no
    # leftover. Each read is guarded, so the caller's ERR trap never sees one fail.
    args=() environ=()
    { mapfile -d '' -t args <"$p/cmdline"; } 2>/dev/null || :
    { mapfile -d '' -t environ <"$p/environ"; } 2>/dev/null || :
    if [[ "${args[*]-} ${environ[*]-}" =~ /tmp/$prefix\.([0-9]+)\. ]]; then
      owner_of[$p]=${BASH_REMATCH[1]}
    else
      undecided+=("$p/cwd")
    fi
  done
  if [ "${#undecided[@]}" -gt 0 ]; then
    err=$(mktemp "${TMPDIR:-/tmp}/refuse-leftovers.XXXXXX") || {
      fail "refuse_leftovers: no file to keep find's complaints in, so the working directories under $proc were not read"
      return 2
    }
    # shellcheck disable=SC2185 # find reads its paths from stdin (-files0-from -), not from argv
    # `|| exit` keeps find's status for the wait below, and keeps a caller's ERR trap (`set -E`) from
    # firing inside the substitution for the unreadable entries every walk meets.
    mapfile -d '' -t cwd_links < <(printf '%s\0' "${undecided[@]}" |
      LC_ALL=C find -files0-from - -maxdepth 0 -printf '%p\0%l/\0' 2>"$err" || exit)
    wait "$!" || status=$?
    complaint=
    [ "$status" = 0 ] || complaint=$(proc_walk_complaint "$err" "$proc") || complaint=
    # GNU find exits 0 or 1 of its own accord; any other status is a find that a signal or the shell
    # ended, and what it printed before is no answer, even when it printed no complaint.
    [ "$status" -le 1 ] || complaint=${complaint:-"find ended with status $status and printed nothing"}
    rm -f -- "$err"
    if [ -n "$complaint" ]; then
      fail "refuse_leftovers: find exited $status reading the working directories under $proc: $complaint"
      return 2
    fi
    for ((i = 0; i + 1 < ${#cwd_links[@]}; i += 2)); do
      path=${cwd_links[i]} target=${cwd_links[i + 1]}
      [[ $target =~ /tmp/$prefix\.([0-9]+)\. ]] || continue
      owner_of[${path%/cwd}]=${BASH_REMATCH[1]}
    done
  fi
  for p in "${!owner_of[@]}"; do
    owner=${owner_of[$p]}
    since=$(leftover_started "${p##*/}") || continue
    leftover_owner "$owner" "$since" && continue
    args=()
    { mapfile -d '' -t args <"$p/cmdline"; } 2>/dev/null || :
    target="${args[*]-}"
    leftover_list+=("process ${p##*/}, ${target:0:100} (kill ${p##*/})")
  done
  [ "${#leftover_list[@]}" -eq 0 ] && return 0
  fail "an earlier $prefix run whose driver is gone left these running: $(printf '%s; ' "${leftover_list[@]}")remove them and run again"
}
