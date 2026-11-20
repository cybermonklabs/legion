#!/usr/bin/env bash
# Red test: the order search of the shell stacks (scripts/e2e/lib/sweep-order.ts) reads a start as
# clean where errexit ends it before its removal of dead stacks' Postgres, in two forms it does not
# read:
#   1. errexit turned on by the script's first line, `#!/bin/bash -e` or `#!/usr/bin/env -S bash -e`,
#      with no `set -e`, before a bare `test ! -e <lock>`;
#   2. an arithmetic command whose operand is a command substitution, `(( $(stat …) == 0 ))`, under
#      `set -euo pipefail`.
#
# R3: "The next start of a test stack removes each Postgres of a stack that it can show dead." Item 5
# of round 14: "Under set -e (and set -o errexit, and a trap … ERR that exits), every command whose
# failure is not handled ends the start, so count each such command before the removal as a
# refusal unless it is shown to read only the start's own inputs." Decision 14: the search "fails
# safe". The search's own rule names `[`, `[[`, `test` with no test of a file, and the like, as
# input-only; neither form here is one of them.
#
# Each case is a driver that the shared runner (lib/order-search-cases.sh) runs as an executable, so
# the first line's -e counts, with the lock held and without it, and then searches. Control: the
# same check written `[ ! -e "$LOCK" ] || exit 1` must be FOUND, else UNKNOWN.
#
#   usage: order-search-errexit-shebang-arith.test.sh TREE     (TREE: a checkout's root)
# Exit 0: each form is found. Exit 1: a form the run shows refusing reads as clean. Exit 2: UNKNOWN.
set -u
. "$(dirname "$0")/order-search-cases.sh"
order_search_setup "${1:?usage: order-search-errexit-shebang-arith.test.sh TREE}"
check='test ! -e "$LOCK"'
order_search_case control-explicit refuses found '#!/usr/bin/env bash' '' \
  $'set -euo pipefail\n[ ! -e "$LOCK" ] || exit 1'
order_search_case shebang-bash-e refuses found '#!/bin/bash -e' '' "$check"
order_search_case shebang-env-S-bash-e refuses found '#!/usr/bin/env -S bash -e' '' "$check"
order_search_case arith-command-substitution refuses found '#!/usr/bin/env bash' '' \
  $'set -euo pipefail\n(( $(stat -c %s "$LOCK" 2>/dev/null || echo 0) == 0 ))'
order_search_end "errexit from a first line or an arithmetic command"
