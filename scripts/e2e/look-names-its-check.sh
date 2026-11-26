#!/usr/bin/env bash
# A production check's look report through the plugin's executePhaseOp and real daemon client.
# The Go driver boots the whole daemon over a Dispatch this checkout builds, on databases of
# the caller's PostgreSQL server and a nats-server of its own. It walks one issue to its merge
# under production_check: tester, and takes look 1 in the tester's session. It runs these checks:
#   control-client-look-unnamed: a report without check_opened_at gets 409
#     CHECK_LOOK_NOT_SERVED; legion state --json still shows look 1 open;
#   client-look-named: the time comes from the task the fake runtime received. A pending
#     carrying that check_opened_at and wake_at answers {}, and legion state --json shows the
#     check still open, lookOpen false, and nextWakeAt equal to the time the report named.
# Run on Linux with go, git, jj, bun after bun install, awk, python3 and a loopback
# LEGION_TEST_PG_DSN: bash scripts/e2e/look-names-its-check.sh.
# LOOK_NAMES_ITS_CHECK_EVIDENCE_DIR keeps the proof's own log and evidence directory.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
command -v bun >/dev/null || { echo 'FAIL prerequisites: bun is not on PATH' >&2; exit 1; }
[ -d "$root/node_modules" ] || { echo 'FAIL prerequisites: run bun install at the checkout root' >&2; exit 1; }
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'a look names its check' look-names-its-check TestLookNamesItsCheckLive "${LOOK_NAMES_ITS_CHECK_EVIDENCE_DIR:-}"
