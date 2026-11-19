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
# are read in this shell (mapfile), and the working directories of the processes neither names a
# run in are read by one `find` for them all, so the walk starts no program for each process. A
# proc root that lists no process at all (an empty tmpfs where /proc should be) is reported through
# `fail`, naming it, since a walk that read nothing found no leftover only because it saw nothing.
refuse_leftovers() {
  local prefix=$1 proc=${proc_root:-/proc} name owner created p path target since boot ticks i
  local -a leftover_list=() entries=() args=() environ=() undecided=() cwd_links=()
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
    # shellcheck disable=SC2185 # find reads its paths from stdin (-files0-from -), not from argv
    mapfile -d '' -t cwd_links < <(printf '%s\0' "${undecided[@]}" |
      LC_ALL=C find -files0-from - -maxdepth 0 -printf '%p\0%l/\0' 2>/dev/null || :)
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
