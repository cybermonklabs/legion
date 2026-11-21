#!/usr/bin/env bash
# The rig of dispatch://INFRA-4425's checks K2, K3 and K4, and of dispatch://INFRA-4799's C2: every
# Dispatch client a Legion deployment runs, writing with a token the secrets broker issued, and a
# tool's reads through `dispatch proxy`, against a real broker, a real Dispatch and the real
# clients. The driver
# (packages/daemon/internal/daemon/dispatch_broker_live_test.go, built with `-tags e2e`) starts the
# broker's own test rig on PostgreSQL, builds and starts the real Dispatch server from this
# checkout's packages/envoy on a database of its own behind a recording proxy, and drives the
# daemon's own Dispatch client, the real `dispatch` command (packages/envoy-client/bin/dispatch.ts
# under bun) and scripts/e2e/dispatch-broker-clients.ts, which holds the client that command
# builds and the client the pi-envoy plugin builds:
#   - K2, TestTheDaemonWritesAsItselfOnlyInItsProject: under dispatch_token_broker the daemon's
#     client writes on an issue of its own project and on an issue of another, an issue message
#     on each and on the other a status and its step, rating, plan and role holder records, each
#     accepted under DPoP; one whose proof names an origin outside DISPATCH_PROOF_ORIGINS is
#     refused 401 BROKER_TOKEN_PROOF_INVALID; and `dispatch whoami` in a role's environment acts
#     for that role's person and names the project its token names. Without the setting both
#     messages carry Bearer and are accepted, and whoami runs under the shared token.
#   - K4, TestARoleWritesEveryKindWithItsOwnToken: with a role's token of its project, every write
#     kind a role makes, through the dispatch command and through the plugin's client, three
#     writes of the command on another project's issue, and with the daemon's token every method
#     of the daemon's client, each answered 2xx; with a token of another project every write on
#     the first project's objects is accepted too, but the daemon script's step record, its rating
#     and its plan are refused 403 STEP_WRITER and its role holders 403 ROLE_HOLDER_FORBIDDEN,
#     since that token's session is no daemon's, and an upload into another session's
#     conversation 403 SESSION_SCOPE.
#   - K3, TestEveryClientRenewsAndStopsOnARefusal, with LEGION_E2E_TOKEN_RENEWAL=1 and in place of
#     the two above: the three long-lived clients write over three lifetimes of a 420 s token,
#     renewing it with no restart, and stop with the broker's refusal once their pod's token is
#     one the broker refuses for good, sending Dispatch nothing. It takes about fifteen minutes.
#   - C2, TestTheProxyServesAToolRunWithOneTradeForEachRenewal, with LEGION_E2E_PROXY=1 and alone,
#     in place of every test above, K3 included: `dispatch proxy` in a role's environment runs a
#     Python standard-library client over its unix socket for 255 s of a 420 s token. The client
#     reads back a file's bytes, posts one comment twice under one Idempotency-Key and makes at
#     least 2,000 requests, each read answered 200, and the proxy trades at most 1 + T / (L - 300) times,
#     the count its exit line names; without DISPATCH_TOKEN_BROKER it exits 2 and runs nothing.
#     It takes about five minutes past the rig's start.
# Each test writes its tables to TMPDIR and logs the file's path; `go test -v` prints each test's
# own `--- PASS` or `--- FAIL` line, which is the result, since a skip prints `ok` too.
#
# Run it from a checkout on a Linux host with go, bun, psql and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/dispatch-broker.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL every database of the run is made on:
# the broker's schema (BROKER_TEST_DATABASE_URL, which this script sets to it) and Dispatch's
# database (its role must be allowed to create and drop databases, and the server must offer the
# pgvector extension, which Dispatch's migrations create). That server must be this machine's:
# lib/loopback-dsn.sh's require_loopback_dsn refuses a host that is not 127.0.0.1, localhost or
# ::1 with exit 2, before anything builds or starts. This script starts no database. It runs
# `bun install` at the checkout's root when node_modules is missing, and builds with GOCACHE when
# it is set, else a cache under TMPDIR. Nothing reaches a deployed broker, Dispatch or GitHub.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
refuse() { echo "FAIL prerequisites: $*" >&2; exit 2; }
fail() { echo "FAIL prerequisites: $*" >&2; exit 1; }

[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the broker and Dispatch run on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
for tool in bun go psql python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
# lib/loopback-dsn.sh's require_loopback_dsn says why its callers clear these three, and why the
# call comes after the tool check above.
# shellcheck source-path=SCRIPTDIR source=lib/loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
# require_loopback_dsn assigns db_host the host libpq reads, or its own refusal line.
db_host=''
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops databases there" ||
  refuse "$db_host"
echo "PostgreSQL at $db_host (LEGION_TEST_PG_DSN, not printed); BROKER_TEST_DATABASE_URL is set to it"
if [ ! -d "$root/node_modules" ]; then
  echo "$root/node_modules is missing; running bun install at the checkout's root"
  (cd "$root" && bun install) || fail "bun install at $root failed"
fi

export BROKER_TEST_DATABASE_URL=$LEGION_TEST_PG_DSN
run='^(TestTheDaemonWritesAsItselfOnlyInItsProject|TestARoleWritesEveryKindWithItsOwnToken)$'
if [ "${LEGION_E2E_TOKEN_RENEWAL:-}" = 1 ]; then
  run='^TestEveryClientRenewsAndStopsOnARefusal$'
fi
if [ "${LEGION_E2E_PROXY:-}" = 1 ]; then
  run='^TestTheProxyServesAToolRunWithOneTradeForEachRenewal$'
fi
cd "$root/packages/daemon"
GOCACHE=${GOCACHE:-${TMPDIR:-/tmp}/go-build} GOGC=20 GOMAXPROCS=2 \
  go test -p=1 -tags e2e -count=1 -timeout 30m -v -run "$run" ./internal/daemon/
