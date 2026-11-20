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

# proc_walk_complaint, which reads the stderr of a walk of every process, and proc_walk_bound, the
# bound of such a walk (lib/process-tree.sh).
# shellcheck source-path=SCRIPTDIR source=process-tree.sh
declare -F proc_walk_complaint proc_walk_bound >/dev/null || . "${BASH_SOURCE[0]%/*}/process-tree.sh"

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

# refuse_leftovers PREFIX fails, naming each one and the command that removes it, when a container
# or a process of an earlier PREFIX run outlived its driver. The caller's `fail` reports it. It
# reads the processes of `${proc_root:-/proc}`: the host's /proc, unless a test points proc_root at
# a proc root of its own (links to the /proc entries of the processes it started,
# scripts/e2e/lib/proc-root.ts), since a walk of the host's whole /proc holds the test to the host's
# processes, an earlier run's among them. A process names its run by the first of its command line,
# its environment and its working directory that holds /tmp/<PREFIX>.<driver pid>.: the first two
# are read by one grep for each xargs batch of them all, and the working directories of the
# processes neither names a run in by one `find` for them all, so the walk starts no program for
# each process, and each of the two is held to its bound (proc_walk_bound, lib/process-tree.sh): a
# /proc entry that never answers its reader, which held this check for ever, ends it there, named.
# A proc root that lists no process at all (an empty tmpfs where /proc should be), a walk past its
# bound, and a grep or a `find` that failed, a signal ended or that is too old to read -files0-from
# (findutils before 4.9), are reported through `fail`, naming them, with status 2: a walk that read
# nothing found no leftover only because it saw nothing. A working directory this user may not read,
# another user's process's, or one whose process ended during the walk, is no complaint
# (proc_walk_complaint).
refuse_leftovers() {
  local prefix=$1 proc=${proc_root:-/proc} name owner created p path target since boot ticks i
  local err complaint status=0 bound record
  local -a leftover_list=() entries=() args=() undecided=() cwd_links=() named=() files=()
  local -A owner_of=()
  boot=$(awk '$1 == "btime" { print $2 }' /proc/stat)
  ticks=$(getconf CLK_TCK)
  while read -r name; do
    owner=${name##*-}
    case $owner in '' | *[!0-9]*) continue ;; esac
    created=$(docker inspect -f '{{.Created}}' "$name" 2>/dev/null) || continue
    since=$((($(date -d "$created" +%s) - boot) * ticks))
    leftover_owner "$owner" "$since" || leftover_list+=("container $name (docker rm -f -v $name)")
  done < <(docker ps -a --filter "name=^$prefix-" --format '{{.Names}}')
  entries=("$proc"/[0-9]*)
  [ -e "${entries[0]}" ] || {
    fail "refuse_leftovers: $proc lists no process, so whether an earlier $prefix run left processes running was not read"
    return 2
  }
  # A pid is its namespace's own: a process in another pid namespace (an agent box) names a driver
  # pid that means nothing here, and one of another user's reads as nothing. The test is a stat of
  # the namespace's link, which no process can hold.
  for p in "${entries[@]}"; do
    [ "$p/ns/pid" -ef /proc/self/ns/pid ] || continue
    files+=("$p/cmdline" "$p/environ")
  done
  bound=$(proc_walk_bound "${#entries[@]}")
  # Each process's command line, then its environment: grep prints each file's matches in the
  # order it was handed them, so the first record of a process is its command line's where it has
  # one. `|| exit` keeps the walk's status for the wait below, and keeps a caller's ERR trap
  # (`set -E`) from firing inside the substitution for the batches every walk has with no match.
  # timeout leads a process group of its own, ends every grep at the bound, and exits 124 then.
  if [ "${#files[@]}" -gt 0 ]; then
    mapfile -d '' -t named < <(printf '%s\0' "${files[@]}" |
      timeout -k 5 "$bound" xargs -0 grep -sHozE "/tmp/$prefix\\.[0-9]+\\." || exit)
    wait "$!" || status=$?
    case $status in
      0 | 123) ;;
      124)
        fail "refuse_leftovers: the walk of ${#entries[@]} entries of $proc had not ended after ${bound}s (proc_walk_bound), so whether an earlier $prefix run left processes running was not read"
        return 2
        ;;
      *)
        fail "refuse_leftovers: xargs exited $status reading the command lines and environments under $proc, so whether an earlier $prefix run left processes running was not read"
        return 2
        ;;
    esac
  fi
  for record in ${named[@]+"${named[@]}"}; do
    path=${record%%:/tmp/*}
    p=${path%/*}
    [ -z "${owner_of[$p]+set}" ] || continue
    if [[ ${record#"$path":} =~ ^/tmp/$prefix\.([0-9]+)\. ]]; then owner_of[$p]=${BASH_REMATCH[1]}; fi
  done
  for ((i = 0; i < ${#files[@]}; i += 2)); do
    p=${files[i]%/cmdline}
    [ -n "${owner_of[$p]+set}" ] || undecided+=("$p/cwd")
  done
  status=0
  if [ "${#undecided[@]}" -gt 0 ]; then
    err=$(mktemp "${TMPDIR:-/tmp}/refuse-leftovers.XXXXXX") || {
      fail "refuse_leftovers: no file to keep find's complaints in, so the working directories under $proc were not read"
      return 2
    }
    # shellcheck disable=SC2185 # find reads its paths from stdin (-files0-from -), not from argv
    # `|| exit` keeps find's status for the wait below, and keeps a caller's ERR trap (`set -E`) from
    # firing inside the substitution for the unreadable entries every walk meets. timeout ends find
    # at the bound with a TERM and exits 124 then; a find a signal ended of itself passes its status.
    mapfile -d '' -t cwd_links < <(printf '%s\0' "${undecided[@]}" |
      LC_ALL=C timeout -k 5 "$bound" find -files0-from - -maxdepth 0 -printf '%p\0%l/\0' 2>"$err" || exit)
    wait "$!" || status=$?
    complaint=
    case $status in
      124) complaint="the walk of ${#undecided[@]} entries had not ended after ${bound}s (proc_walk_bound)" ;;
      0) ;;
      *) complaint=$(proc_walk_complaint "$err" "$proc") || complaint= ;;
    esac
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
