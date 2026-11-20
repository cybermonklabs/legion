#!/usr/bin/env bash
# Red test: the order search of the shell stacks (scripts/e2e/lib/sweep-order.ts) reads a start as
# clean where `set -e` ends it before its removal of dead stacks' Postgres, and a `set +e` that bash
# never applies to the start's own shell comes first.
#
# R3: "The next start of a test stack removes each Postgres of a stack that it can show dead." Bash
# keeps `set -e` per shell: a subshell, a command substitution, a function whose body is in
# parentheses, each command of a pipeline of more than one and a list in the background run in a
# shell of their own, and a `set +e` there leaves the start's shell as it was; a `set +e` in a
# branch, a list or a loop that does not run changes nothing. The search fails safe: a state that
# one path may leave on is on after the paths join.
#
# Each case is a driver under `set -Eeuo pipefail` with the form, then `test ! -e "$LOCK"`, then
# the removal, which the shared runner (lib/order-search-cases.sh) runs with the lock held and
# without it, and then searches; the search must name the check's own line. Controls: the check
# written `[ ! -e "$LOCK" ] || exit 1` must be found, and a `set +e` in both branches of an `if`,
# which bash always applies, must make the start reach its removal with the lock held and read as
# clean; else UNKNOWN.
#
#   usage: order-search-errexit-scope.test.sh TREE     (TREE: a checkout's root)
# Exit 0: each form is found. Exit 1: a form the run shows refusing reads as clean. Exit 2: UNKNOWN.
set -u
. "$(dirname "$0")/order-search-cases.sh"
order_search_setup "${1:?usage: order-search-errexit-scope.test.sh TREE}"
shebang='#!/usr/bin/env bash'
options='set -Eeuo pipefail'
check='test ! -e "$LOCK"'
order_search_case control-explicit refuses found:1 "$shebang" "$options" '[ ! -e "$LOCK" ] || exit 1'
order_search_case control-off-in-both-branches reaches clean "$shebang" "$options" \
  $'if [ -n "${NEVER_SET:-}" ]; then set +e; else set +e; fi\n'"$check"
order_search_case branch-that-does-not-run refuses found:2 "$shebang" "$options" \
  $'if false; then set +e; fi\n'"$check"
order_search_case list-that-does-not-run refuses found:2 "$shebang" "$options" $'true || set +e\n'"$check"
order_search_case loop-that-does-not-run refuses found:2 "$shebang" "$options" \
  $'while false; do set +e; done\n'"$check"
order_search_case case-arm-that-does-not-run refuses found:2 "$shebang" "$options" \
  $'case "${NEVER_SET:-}" in never) set +e ;; esac\n'"$check"
order_search_case subshell refuses found:2 "$shebang" "$options" $'( set +e )\n'"$check"
order_search_case command-substitution refuses found:2 "$shebang" "$options" $'saved=$(set +e)\n'"$check"
order_search_case function-in-parentheses refuses found:3 "$shebang" "$options" \
  $'relax() ( set +e )\nrelax\n'"$check"
order_search_case pipeline refuses found:2 "$shebang" "$options" $'set +e | cat\n'"$check"
order_search_case background refuses found:2 "$shebang" "$options" $'set +e &\n'"$check"
order_search_end "a set +e that bash never applies to the start"
