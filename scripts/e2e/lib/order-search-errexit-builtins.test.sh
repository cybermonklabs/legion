#!/usr/bin/env bash
# Red test: the order search of the shell stacks (scripts/e2e/lib/sweep-order.ts) reads every way
# bash turns errexit on or off as bash does, or names the command it cannot read; it never reads
# such a command as clean.
#
# R3: "The next start of a test stack removes each Postgres of a stack that it can show dead."
# Decision 14: the search "fails safe". Bash turns errexit on with `set`, with `shopt -o`, which
# sets the options of `set -o` (bash(1), shopt: "-o Restricts the values of optname to be those
# defined for the -o option to the set builtin"), through `builtin` and `command`, which run the
# builtin itself, through `eval`, which runs its words as commands of the same shell, and with a
# name it reads once quotes are removed (`\set`, `'set'`); `local -` makes a function's options its
# own, which bash gives back when the function returns (bash(1), local: "If name is '-', the set of
# shell options is made local to the function in which local is invoked"); a trap on ERR, which
# bash reads in any case, ends the start as errexit does. A command whose words the search cannot
# know (`eval "$x"`, `set $opts`, a file it does not read sourced, a command named by an
# expansion, an alias once `expand_aliases` is on) it names as one it cannot read.
#
# Each case is a driver that the shared runner (lib/order-search-cases.sh) runs with the lock held
# and without it, and then searches; bash's own run shows whether the form ends the start before
# its removal. Controls: `set -o errexit` must be FOUND, and a driver that sets no option must read
# clean, else UNKNOWN.
#
#   usage: order-search-errexit-builtins.test.sh TREE     (TREE: a checkout's root)
# Exit 0: each form is read as bash runs it. Exit 1: a form is not. Exit 2: UNKNOWN.
set -u
. "$(dirname "$0")/order-search-cases.sh"
order_search_setup "${1:?usage: order-search-errexit-builtins.test.sh TREE}"
shebang='#!/usr/bin/env bash'
check='test ! -e "$LOCK"'
strict='set -Eeuo pipefail'
on() { # NAME FORM: FORM turns errexit on, and the check after it ends the start.
  order_search_case "$1" refuses found:2 "$shebang" '' "$2"$'\n'"$check"
}
order_search_case control-set-o-errexit refuses found:2 "$shebang" '' $'set -o errexit\n'"$check"
order_search_case control-no-option reaches clean "$shebang" '' $':\n'"$check"
on shopt-s-o-errexit 'shopt -s -o errexit'
on shopt-so-errexit 'shopt -so errexit'
on shopt-o-s-errexit 'shopt -o -s errexit'
on shopt-os-errexit 'shopt -os errexit'
on builtin-set-e 'builtin set -e'
on command-set-e 'command set -e'
on command-p-set-e 'command -p set -e'
on command-builtin-set-e 'command builtin set -e'
on backslash-set-e '\set -e'
on quoted-set-e "'set' -e"
on quoted-option-e 'set "-e"'
on eval-double-quoted 'eval "set -e"'
on eval-single-quoted "eval 'set -e'"
on eval-words 'eval set -e'
on trap-err-lower-case "trap 'exit 3' err"
on builtin-trap-err "builtin trap 'exit 3' ERR"
# `local -`: bash gives the function's caller back its options, so a `set +e` after it holds only
# in the function, and the start keeps `set -e`; one in a branch that runs is read where it may run.
order_search_case local-dash-keeps-set-e refuses found:3 "$shebang" "$strict" \
  $'relax() { local -; set +e; }\nrelax\n'"$check"
order_search_case local-dash-among-names refuses found:3 "$shebang" "$strict" \
  $'relax() { local a - b; set +e; }\nrelax\n'"$check"
order_search_case local-dash-in-a-branch refuses found:3 "$shebang" "$strict" \
  $'relax() { if true; then local -; fi; set +e; }\nrelax\n'"$check"
order_search_case control-function-set-plus-e reaches clean "$shebang" "$strict" \
  $'relax() { set +e; }\nrelax\n'"$check"
order_search_case local-dash-gives-back-off reaches clean "$shebang" '' \
  $'tighten() { local -; set -e; }\ntighten\n'"$check"
# What the search cannot read it names, and the start ends before its removal at each of them.
order_search_case eval-of-an-expansion refuses unread:2 "$shebang" '' \
  $'opts=\'set -e\'\neval "$opts"\n'"$check"
order_search_case set-of-an-expansion refuses unread:2 "$shebang" '' $'opts=-e\nset $opts\n'"$check"
order_search_case shopt-of-an-expansion refuses unread:2 "$shebang" '' \
  $'option=errexit\nshopt -s -o "$option"\n'"$check"
order_search_case trap-of-an-expansion refuses unread:2 "$shebang" '' \
  $'action=\'exit 3\'\ntrap "$action" ERR\n'"$check"
order_search_case command-named-by-an-expansion refuses unread:2 "$shebang" '' \
  $'builtin=set\n"$builtin" -e\n'"$check"
order_search_case source-of-a-file-not-read refuses unread:2 "$shebang" '' \
  $'printf \'set -e\\n\' >"$root/options.sh"\n. "$root/options.sh"\n'"$check"
order_search_case alias-after-expand-aliases refuses unread:1 "$shebang" '' \
  $'shopt -s expand_aliases\nalias tighten=\'set -e\'\ntighten\n'"$check"
order_search_end "the ways bash turns errexit on or off"
