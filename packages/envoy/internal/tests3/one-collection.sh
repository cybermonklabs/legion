#!/bin/sh
# One collection for every object a SeaweedFS server stores, grown before anything is written,
# and the checks that say it held. This is the one implementation of that rule, and both places
# that start this store run it: the shard job of .github/workflows/dispatch-browser.yaml, for the
# browser suite's file store, and tests3.Start (seaweedfs.go beside this file, which embeds it),
# inside the Go tests' SeaweedFS container. So the order of its steps, its waits and its checks
# cannot drift apart between the two.
#
#   one-collection.sh prepare <collection> <server log>   once the S3 port answers, before any write
#   one-collection.sh check <collection> <server log>     after writes
#
# It drives the `weed server` on this machine's default ports (master 127.0.0.1:9333, filer
# 127.0.0.1:8888) with `weed shell` and `wget`, which both images carry (busybox's in the
# SeaweedFS image), and reads that server's log from the file named. It stays POSIX: the
# SeaweedFS image's sh is busybox's, and the shard job runs its steps with `sh -e`. On a failure
# it prints a `::error::` line naming what failed, what it read, and the end of the server's log,
# and exits 1.
#
# WHY. SeaweedFS keeps a bucket's objects in a collection of the bucket's own name and makes that
# collection's seven volumes when the bucket's first object is written. That growth races the
# volume server's heartbeat: the master can read a heartbeat collected before the collection's
# first volume existed, take the volume back and drop the collection's layout while the write
# waits on it. The write then fails after 10 s. When the filer gives up on it before the master
# answers, every later write to the store fails at once ("Connection wait stopped: context
# canceled" in the log) until the store is restarted; when the master answers first, a retry of
# the write can succeed. Run 44292015929 met the first outcome in shard 2: the first object of
# dispatch-e2e-files-worker-2 was a row's upload, Dispatch answered it 502
# FILE_STORE_UNAVAILABLE, and the row and its retry each waited 45 s for a Download link that
# could not come. The race is rare, not absent: 1 of the 1,066 growths of the 265 shard legs of
# 200 ci.yaml runs, read from their record artifacts on 2026-11-18 (10:02 to 13:06Z). A growth
# that no write waits on survives it, since the next heartbeat lists the volume again. So no
# write may grow a collection.
#
# EVERY WAIT HAS A BOUND, AND EVERY BOUND AND CHECK FAILS BY NAME. The script asks the store
# things in two helpers only, shell and master, each under a bound of its own, and it waits in
# one only, within, which ends by its seconds; every check and every wait fails by a message of
# its own. TestEveryWaitAndCheckOfTheScriptHasACase (waits_test.go) parses this file as POSIX
# shell, reads every command it runs wherever it runs it (a function, a substitution, a trap,
# within's and timeout's command), and fails on a request outside those helpers or under no bound
# above 0, a wait outside within or inside another wait, a sleep but `sleep 1`, a command outside
# the list of those that end by themselves (its scriptCommands, with tail only as
# `tail -n <lines> [--] <file>`, a file a variable gives after --), a function that runs itself,
# is defined twice or takes a listed name, a within whose command runs within, a deadline or
# seconds set but within's own, arithmetic but within's deadline, a within that does not take its
# arguments as its first two statements, a set of the arguments, an exit outside fail and usage, a
# return but "$status", a test nothing acts on, a message no case wants, or shell it cannot read.
#
# The log lines this reads are SeaweedFS 3.97's, which .github/ci-images.yaml pins by digest for
# both images; whoever moves that row reads them again.
set -eu

usage() {
  echo "::error::usage: one-collection.sh prepare|check <collection> <server log>; it was given: $*" >&2
  exit 1
}
[ $# -eq 3 ] || usage "$@"
step=$1
collection=$2
log=$3
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fail <message> [file...]: the message as an error, each file read, the end of the server's log.
fail() {
  echo "::error::$1"
  shift
  for file in "$@"; do
    echo "--- $(basename "$file")"
    cat "$file"
  done
  echo "--- the last 200 lines of $log"
  tail -n 200 -- "$log" 2>/dev/null || echo "(no log at $log)"
  exit 1
}

# shell <file> <command>...: one `weed shell` running the commands, its output into file. It runs
# under timeout(1), which both images carry (busybox's in the SeaweedFS image), and each one runs
# after registered below has seen a volume server, where it is quick: in each of CI run
# 44425048628's four shard legs every shell took under 0.2 s by the master's log, and with the
# store held to a tenth of a CPU on a dev box the slowest took 1.8 s. A shell still running at 60 s
# is talking to a store that stopped answering, and 60 s fails it by name a minute in, rather than
# at the shard job's 22 minutes or go test's 20. It is ended by KILL: weed shell catches TERM and
# exits 0, and busybox's timeout answers its program's own exit, so a shell ended by TERM read as
# one that succeeded in the SeaweedFS image.
shell() {
  out=$1
  shift
  printf '%s\n' "$@" | timeout -s KILL 60 weed shell -master=127.0.0.1:9333 -filer=127.0.0.1:8888 > "$out" 2>&1 ||
    fail "weed shell could not run" "$out"
}

# master <path> <file>: one request to the master's HTTP port, its answer and any error into file.
# The master answers each of these within a second or so. The grow of step 2 is the slowest: it
# answered within 0.012 s in CI's four shard legs, within 1.011 s in 75 server logs of this
# script's runs on a dev box at loads up to 665, and within 0.4 s with the store held to a tenth of
# a CPU. So 10 s ends a request to a master that took the connection and does not answer, by KILL
# as for a shell, and the file says so; the wait that sent it tries again until its own seconds
# run out.
master() {
  timeout -s KILL 10 wget -q -O - "http://127.0.0.1:9333$1" > "$2" 2>&1 || {
    status=$?
    echo "(no answer: exit $status; 137 is the 10 s bound ending it)" >> "$2"
    return "$status"
  }
}

# within <seconds> <message> <file> <command>...: command, run a second apart until it succeeds,
# and a failure by message, with file read, once that many seconds have passed without a success.
# It counts seconds, not tries, because a try can take as long as its own bound: 60 tries of a
# request bounded at 10 s can take 11 minutes, and on a store that took each connection and never
# answered they held prepare past 330 s with no line. A try under way when the seconds run out
# ends within its own bound after them. `within 0` tries once.
within() {
  seconds=$1 message=$2 seen=$3
  shift 3
  deadline=$(( $(date +%s) + seconds ))
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "$message" "$seen"
    sleep 1
  done
}

# 1. A volume server registered with the master, which the master's /dir/status names by its
# "Url". A `weed shell` that connects before one has registered waits for ever: the master sends a
# new client one message for each volume server it knows (master_grpc_server.go's KeepConnected),
# so with none it sends nothing, and the shell prints dots until it is killed. So every step that
# runs a shell reads this first: registered <seconds> reads it for up to that many seconds.
# prepare waits 60 s, as a step of its own, which no cut of another step removes. In 1,172 starts
# of this store read on a dev box, none held back on purpose, the volume server registered before
# the S3 port answered or at most 0.52 s after it; held back 10 s after the port, it registered 10
# to 11 s after; held back 30 s, prepare passed in 32 s, and in 36 s with the store held to a tenth
# of a CPU. So a minute without one is a volume server that did not start, and a master that
# answers nothing ends this wait by name 71 s in at most: its minute, a second's pause and one
# request's 10 s, well inside runScript's deadline (seaweedfs.go). check reads it once, since
# prepare saw it: a master that does not answer, or that lists no volume server, is a store that
# stopped during the run, and it fails by name at once.
listed() {
  master /dir/status "$work/status.txt" && grep -q '"Url"' "$work/status.txt"
}
registered() {
  within "$1" "the master lists no volume server, so the store cannot take a write" "$work/status.txt" listed
}

# 2. The collection's seven volumes, made by `/vol/grow` before anything is written. It runs
# before the rule of step 3 exists: the filer writes a file of its own log about a minute after it
# starts, and once the rule is in force that file goes into the collection too, so the collection
# must hold its volumes before the rule can send any write to it. It names the collection itself,
# so it needs no rule. It is retried because a volume server started with `-volume.max=0`, as both
# are, registers with room for no volume: it learns the volume size limit from the master's first
# answer, works out its room from it, and only then reports it (store.go's MaybeAdjustVolumeMax),
# and until then the master answers 406. It reports it at once, or failing that in its next
# heartbeat, 5 s later (constants.VolumePulseSeconds): 10 s of tries, a second apart, cover two of
# those. In the 33 server logs of the first runs of this script, the first grow after step 1
# succeeded every time.
grow() {
  within 10 "the master grew no volumes for $collection" "$work/grow.txt" master "/vol/grow?collection=$collection&count=7" "$work/grow.txt"
}

# 3. The rule: every path into the collection, so each bucket and the filer's own files share
# it. Then a directory under /etc/seaweedfs, made after the rule, which step 5 waits for.
configure() {
  marker="rule-in-force-$$"
  shell "$work/rule.txt" "fs.configure -locationPrefix=/ -collection=$collection -apply" "fs.mkdir /etc/seaweedfs/$marker"
}

# 4. The rule, read back. `weed shell` exits 0 when a command inside it fails, and fs.configure
# returns success on a flag it cannot parse, so neither says whether the rule was stored. The
# filer's configuration as fs.configure prints it must hold the rule instead.
read_back() {
  shell "$work/configuration.txt" fs.configure
  tr -d ' \t\n' < "$work/configuration.txt" | grep -qF "{\"locationPrefix\":\"/\",\"collection\":\"$collection\"," ||
    fail "the filer holds no rule putting every path into $collection" "$work/rule.txt" "$work/configuration.txt"
}

# 5. The rule in force. fs.configure stores the rule and returns; the filer puts it in force only
# when it processes that change, in order with every change under /etc/seaweedfs, and it logs
# each one (filer_on_meta_event.go's "procesing", spelt so). Measured on a dev box, that came
# 0.2 s after a readiness write that had started at once, so the write grew a collection of its
# bucket's name. Once the marker's line is logged, the rule before it is in force.
wait_in_force() {
  within 60 "the filer did not put the rule for $collection in force within a minute" "$work/rule.txt" grep -q "procesing.*$marker" "$log"
}

# collection.list into collections.txt: one line per collection, `collection:"<name>"` and then
# its counts, after the shell's prompt.
list() {
  shell "$work/collections.txt" collection.list
}

# alone <pattern>: collections.txt lists one collection, and its line matches pattern.
alone() {
  [ "$(grep -c 'collection:"' "$work/collections.txt")" = 1 ] && grep -q "$1" "$work/collections.txt"
}

# 6. The collection alone, with its volumes, before any write. Before anything is written a
# collection can exist only because step 2 made it, so a grow that is missing, aimed at another
# collection or made nothing fails here, on the plain store, and not only on the rare run where
# the race comes.
alone_before_writes() {
  list
  alone "collection:\"$collection\"[[:space:]]*volumeCount:[1-9]" ||
    fail "before the first write the store holds no volumes of $collection alone" "$work/collections.txt"
}

# After writes: no write grew storage, and the collection is still the store's only one. The
# master logs `volume grow &{...}` (master_grpc_server_volume.go) only for a growth that an
# assign asked for because the layout had no writable volume, which is a write waiting on storage
# being made; the grow of step 2 logs `volumeGrowHandler received` instead. A log that is missing
# or empty is a failure, not a pass: nothing could be read from it. The log is read before the
# store is asked anything, so a growth is named even when the store has stopped since.
check() {
  [ -s "$log" ] || fail "the server's log $log is missing or empty, so no growth can be read from it"
  if grep 'volume grow &' "$log" > "$work/growths.txt"; then
    fail "a write grew the store's storage: the master grew a collection for a write that found no writable volume" "$work/growths.txt"
  fi
  registered 0
  list
  alone "collection:\"$collection\"" ||
    fail "the store holds a collection other than $collection, so a write grew one" "$work/collections.txt"
}

prepare() {
  registered 60
  grow
  configure
  read_back
  wait_in_force
  alone_before_writes
}

case $step in
  prepare) prepare ;;
  check) check ;;
  *) usage "$@" ;;
esac
