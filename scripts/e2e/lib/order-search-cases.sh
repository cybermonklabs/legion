#!/usr/bin/env bash
# The one runner of the order search's cases that bash itself backs (order-search-*.test.sh). Each
# case plants a driver in a tree of its own, with the tree's real lib/leftovers.sh; RUNS it with a
# lock held and then without it, so bash shows whether the case's form ends the start before its
# removal; and then runs the tree's search (lib/sweep-order.ts) over the planted tree and reads
# what the search says. A test script sources this file, calls `order_search_setup TREE`, then
# `order_search_case` once for each case, and then `order_search_end WHAT`, which prints PASS or
# FAIL. The cases are the scripts' own data; how a case is planted, run and read is here alone.
#
#   order_search_case NAME RUN WANT FIRST BEFORE AFTER
#     NAME    the case's name, and its tree's. A case whose name starts `control-` is a control:
#             where it does not do what it wants, the plant or the search is broken, and the script
#             says UNKNOWN and exits 2 at once.
#     RUN     refuses: with the lock held the start ends before its removal, and without the lock
#             it reaches its removal; reaches: it reaches its removal both times.
#     WANT    found      the search exits 1 and names a refusal (FOUND);
#             found:N    the same, and it names one at the Nth line of AFTER;
#             clean      the search exits 0 and names no refusal;
#             unread:N   the search exits 2 and names the Nth line of AFTER as a command it cannot
#                        read (UNREAD), which it never reads as clean.
#     FIRST   the driver's first line: `#!/usr/bin/env bash`, or one that gives bash `-e`.
#     BEFORE  the lines between FIRST and the driver's source of lib/leftovers.sh, one per line.
#     AFTER   the lines after that source and before the removal: the case's form, then the check
#             that ends the start where its lock is held.
#
# The lock is a file holding a line, beside a directory of the same name with `.d` after it, which a
# process of the runner holds with flock(1), as a dead run's pane would: `test ! -e "$LOCK"`,
# `mkdir "$LOCK.d"`, `flock -n 9` on it and `(( $(stat -c %s "$LOCK") == 0 ))` each fail while it
# is held. While it is free, a directory `$LOCK.free` is there in its place, so that a case can need
# a directory that a dead run's state takes away: `cd "$LOCK.free"` fails while the lock is held.
# The driver gets the lock's path in $LOCK.
#
# Each run of a planted driver is bash, lib/leftovers.sh and a few commands, with no sweep, since
# the planted tree holds no postgres-mark.ts: well under a second at a load of 500 on a shared Linux
# machine on 2026-11-18. Each is bounded at 30 s, the lock holder's start at 30 s and each search
# at 60 s, so a case takes at most `order_search_case_s` (150 s), which sweep-order.test.ts counts
# for each case of a script in that script's row limit. A run without the lock that its bound ends
# is UNKNOWN, never a pass.
set -u

# What one case may take at most, in seconds: two driver runs, the holder's start and the search.
order_search_case_s=150

# order_search_setup TREE: the tree whose search, lib/leftovers.sh and its process-tree.sh sibling
# the cases use; a scratch directory of this run's own (mktemp -d), removed on exit, with the lock
# in it.
order_search_setup() {
  order_search_tree=${1:?usage: order_search_setup TREE}
  order_search=$order_search_tree/scripts/e2e/lib/sweep-order.ts
  [ -r "$order_search" ] && [ -r "$order_search_tree/scripts/e2e/lib/leftovers.sh" ] &&
    [ -r "$order_search_tree/scripts/e2e/lib/process-tree.sh" ] || {
    echo "UNKNOWN: no search, leftovers.sh or process-tree.sh in $order_search_tree"
    exit 2
  }
  command -v bun >/dev/null || {
    echo "UNKNOWN: no bun on PATH"
    exit 2
  }
  order_search_work=$(mktemp -d)
  trap 'order_search_release; rm -rf -- "$order_search_work"' EXIT
  order_search_lock=$order_search_work/held.lock
  order_search_holder=
  order_search_bad=0
}

# order_search_hold: removes the free state's directory, makes the lock and its directory and starts
# the process that holds it, and waits until it holds it. Records the holder's pid, which only this
# runner signals.
order_search_hold() {
  rm -rf -- "$order_search_lock.free"
  printf 'held\n' >"$order_search_lock"
  mkdir -p "$order_search_lock.d"
  rm -f -- "$order_search_lock.taken"
  (
    exec 9>>"$order_search_lock"
    flock 9
    : >"$order_search_lock.taken"
    exec sleep 120
  ) &
  order_search_holder=$!
  local waited=0
  until [ -e "$order_search_lock.taken" ]; do
    if [ "$waited" -ge 300 ]; then
      echo "UNKNOWN: the lock's holder did not take it within 30 s"
      exit 2
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
}

# order_search_release: ends the lock's holder, by the pid the runner recorded, removes the lock,
# its directory and its mark, and makes the free state's directory.
order_search_release() {
  if [ -n "${order_search_holder:-}" ]; then
    kill "$order_search_holder" 2>/dev/null
    wait "$order_search_holder" 2>/dev/null
    order_search_holder=
  fi
  rm -rf -- "${order_search_lock:?}" "$order_search_lock.d" "$order_search_lock.taken"
  mkdir -p -- "$order_search_lock.free"
}

# order_search_case NAME RUN WANT FIRST BEFORE AFTER: one case, as the header says.
order_search_case() {
  local name=$1 run=$2 want=$3 first=$4 before=$5 after=$6
  local t=$order_search_work/$name line
  local lines=("$first")
  mkdir -p "$t/scripts/e2e/lib"
  cp "$order_search_tree/scripts/e2e/lib/leftovers.sh" "$order_search_tree/scripts/e2e/lib/process-tree.sh" "$t/scripts/e2e/lib/"
  if [ -n "$before" ]; then
    while IFS= read -r line; do lines+=("$line"); done <<<"$before"
  fi
  lines+=('root=$(cd "$(dirname "$0")/../.." && pwd)' '. "$root/scripts/e2e/lib/leftovers.sh"')
  # The line of the driver that the Nth line of AFTER is: N after this offset.
  local offset=${#lines[@]}
  while IFS= read -r line; do lines+=("$line"); done <<<"$after"
  lines+=('echo "removal reached"' 'remove_leftover_postgres')
  printf '%s\n' "${lines[@]}" >"$t/scripts/e2e/driver.sh"
  chmod +x "$t/scripts/e2e/driver.sh"
  local blocked free src
  order_search_hold
  blocked=$(LOCK=$order_search_lock timeout 30 "$t/scripts/e2e/driver.sh" 2>&1 | grep -c 'removal reached')
  order_search_release
  free=$(LOCK=$order_search_lock timeout 30 "$t/scripts/e2e/driver.sh" 2>&1 | grep -c 'removal reached')
  local wanted_blocked=0
  [ "$run" = reaches ] && wanted_blocked=1
  if [ "$blocked" != "$wanted_blocked" ] || [ "$free" != 1 ]; then
    echo "UNKNOWN: $name does not $run as planted (reached its removal: with the lock $blocked, without it $free)"
    exit 2
  fi
  timeout 60 bun "$order_search" "$t" >"$t.out" 2>&1
  src=$?
  local at=${want#*:} says
  [ "$at" != "$want" ] && at=$((offset + at))
  local shown="scripts/e2e/driver.sh"
  case $want in
    found) [ "$src" = 1 ] && grep -q '^FOUND' "$t.out" ;;
    found:*) [ "$src" = 1 ] && grep -q "^FOUND .*$shown:$at\b" "$t.out" ;;
    clean) [ "$src" = 0 ] && ! grep -q '^FOUND' "$t.out" ;;
    unread:*) [ "$src" = 2 ] && grep -q "^UNREAD $shown: $shown:$at\b" "$t.out" ;;
    *) echo "UNKNOWN: $name wants $want, which the runner does not read"; exit 2 ;;
  esac
  local held=$?
  says=$(grep -m1 -E '^(FOUND|UNREAD|UNKNOWN)' "$t.out" | cut -c1-240)
  if [ "$held" = 0 ]; then
    echo "${want%%:*}: $name ($run with the lock held; the search exits $src: ${says:-$(grep '^COUNT' "$t.out")})"
  elif [[ $name == control-* ]]; then
    echo "UNKNOWN: the control $name wants $want, and the search exits $src: $(head -c 300 "$t.out")"
    exit 2
  else
    echo "MISSED: $name: bash $run as planted, and the search wants $want but exits $src: ${says:-$(grep '^COUNT' "$t.out")}"
    order_search_bad=$((order_search_bad + 1))
  fi
}

# order_search_end WHAT: PASS where every case did what it wants, else FAIL naming how many did not.
order_search_end() {
  if [ "$order_search_bad" -gt 0 ]; then
    echo "FAIL: $order_search_bad case(s) of $1 that the search does not read as bash runs them"
    exit 1
  fi
  echo "PASS: every form is found"
}
