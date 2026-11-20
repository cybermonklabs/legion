#!/usr/bin/env bash
# Red test: the order search of the shell stacks (scripts/e2e/lib/sweep-order.ts) reads a start as
# clean where `set -e` ends it before its removal of dead stacks' Postgres.
#
# R3: "The next start of a test stack removes each Postgres of a stack that it can show dead." The
# search says it "reads a refusal by what it does, not by its name". Under `set -e` (every driver of
# scripts/e2e runs `set -euo pipefail` or `set -Eeuo pipefail`), a command that fails ends the
# start, as `exit 1` does: `flock -n 9` on a lock a dead run's pane holds, `mkdir` of a fixed
# directory a dead run left, `[ ! -e <file a dead run left> ]`, and `cd` or `pushd` to a directory
# that an input names and a dead run took away, with `--` or not, alone or in a command
# substitution. The search counts `exit`, `return` and helpers that always exit, not such a command.
#
# Each case is a driver that sources the tree's real lib/leftovers.sh, runs one check of the lock,
# which a process of the shared runner (lib/order-search-cases.sh) holds as a dead run's pane
# would, and then removes dead stacks' Postgres. The runner runs it with the lock held, where the
# start must end before the removal, and without it, where it must reach the removal, and then
# searches. Controls: the same check with an explicit `|| exit 1` must be FOUND, and a `cd` to a
# place of the driver's own checkout, which bash runs it from, must reach the removal both times and
# read clean, else UNKNOWN.
#
#   usage: order-search-set-e-refusal.test.sh TREE     (TREE: a checkout's root)
# Exit 0: every form is found. Exit 1: a form the run shows refusing reads as clean. Exit 2: UNKNOWN.
set -u
. "$(dirname "$0")/order-search-cases.sh"
order_search_setup "${1:?usage: order-search-set-e-refusal.test.sh TREE}"
shebang='#!/usr/bin/env bash'
options='set -euo pipefail'
order_search_case control-explicit-exit refuses found "$shebang" "$options" '[ ! -e "$LOCK" ] || exit 1'
order_search_case control-cd-to-its-own-checkout reaches clean "$shebang" "$options" 'cd "$root/scripts"'
order_search_case set-e-test refuses found "$shebang" "$options" 'test ! -e "$LOCK"'
order_search_case set-e-flock refuses found "$shebang" "$options" 'exec 9>>"$LOCK"; flock -n 9'
order_search_case set-e-mkdir refuses found "$shebang" "$options" 'mkdir "$LOCK.d"'
order_search_case set-e-cd refuses found:1 "$shebang" "$options" 'cd "$LOCK.free"'
order_search_case set-e-cd-after-dashes refuses found:1 "$shebang" "$options" 'cd -- "$LOCK.free"'
order_search_case set-e-pushd refuses found:1 "$shebang" "$options" 'pushd "$LOCK.free"'
order_search_case set-e-cd-in-a-substitution refuses found:1 "$shebang" "$options" \
  'free=$(cd "$LOCK.free" && pwd)'
order_search_end "the refusals that set -e makes"
