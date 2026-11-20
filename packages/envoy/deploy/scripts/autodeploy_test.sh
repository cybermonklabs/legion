#!/usr/bin/env bash
# The deployer's suite: runs autodeploy.sh against the stand-ins of autodeploy_test/fixture.sh, one
# group of cases per file of autodeploy_test/, each group a concern of the deployer:
#
#   rollback.sh  a failed deploy's rollback, the schema-set refusal, the stop first, the migration
#                key's poll and holder, and the database credential's split;
#   registry.sh  the pull's retry and the registry's manifest reads;
#   deadline.sh  the bound each call of a pass is given, against the pass's deadline;
#   loop.sh      the commit order, the record of failed deploys, the continuous loop, the settings
#                and the end of a --once run.
#
# With no argument it runs every group, in that order; with arguments, the groups they name. Each
# group counts its cases, and a case that fails stops the suite, naming itself.
set -euo pipefail

suite=$(cd "$(dirname "$0")" && pwd)/autodeploy_test
groups=(rollback registry deadline loop)
if (($# > 0)); then
  groups=("$@")
fi
# shellcheck source=packages/envoy/deploy/scripts/autodeploy_test/fixture.sh
source "$suite/fixture.sh"
counts=()
for group in "${groups[@]}"; do
  if [[ ! -f "$suite/$group.sh" ]]; then
    printf 'no case group %s in %s\n' "$group" "$suite" >&2
    exit 2
  fi
  group_cases=0
  # shellcheck source=/dev/null
  source "$suite/$group.sh"
  if ((group_cases == 0)); then
    printf 'the case group %s ran no case, so it proves nothing\n' "$group" >&2
    exit 1
  fi
  counts+=("$group $group_cases")
done
printf 'autodeploy %s: %s cases: PASS\n' "$(IFS=,; printf '%s' "${counts[*]}" | sed 's/,/, /g')" "$cases"
