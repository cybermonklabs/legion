#!/usr/bin/env bash
# The live proof that text an older build of Dispatch writes gets this build's reference grammar
# with no edit and no command run: a bare issue key, which a build that predates this release
# indexes as nothing at all. Two real `envoy-dispatch` binaries run against one PostgreSQL — the
# one built from the commit `--base-commit` names, and the one built from this checkout — the way
# a rolling release serves both at once, and what the older build writes and edits while the newer
# one runs shows in its target's "Referenced by" once the older build has stopped.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/dispatch-two-builds.sh --base-commit <rev>
#
# DATABASE_URL is required and must name a loopback host: the run empties its `public` schema and
# keeps the extensions it holds, so the base build migrates it from empty and this build migrates
# it forward, as a deployment's upgrade does. --base-commit is required and names the commit the
# base build is built from, which is the merge base with the default branch for a change under
# review; its tree is taken with `git archive` into the run's own work directory, so the checkout
# is never touched.
# The ports are TWO_BUILDS_BASE_PORT (19071), TWO_BUILDS_HEAD_PORT (19072) and
# TWO_BUILDS_ENVOY_PORT (19073), each refused when something already answers on it. The work
# directory, under TMPDIR, holds the base tree, both binaries and every process's log: removed
# when the run passes, kept and printed when it fails.
# The harness this proof and dispatch-search-two-builds.sh run on — the argument, the
# prerequisites, the build of both binaries, the reset, the fake Envoy and the start of each
# server — is scripts/e2e/lib/two-builds.sh, which both source; what is below is this proof's
# own seed and cases.
#
# The three cases, in the order the run makes them:
#
#   base-alone     the base build alone writes a comment citing an issue by its bare key, and
#                  that issue's "Referenced by" stays empty for the whole bounded wait: the base
#                  build's own grammar reads no bare key, which is this proof's negative control.
#   overlap        this build starts beside it and migrates the schema forward; the base build,
#                  still serving, writes a second such comment and edits the first. Neither is
#                  referenced by anything the base build indexed.
#   drained        the base build stops, and with no command run this build's own reconciliation
#                  drains the queue those writes enqueued: both comments show in the target's
#                  "Referenced by".
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh, which lib/two-builds.sh sources, reads of its caller: start_process and
# await_start log under $evidence/logs.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/two-builds.sh
. "$root/scripts/e2e/lib/two-builds.sh"

# This proof's own ports. two_builds_setup reads this script's --base-commit and DATABASE_URL
# before it drops every DISPATCH_*, ENVOY_*, NATS_*, PGHOSTADDR, PGSERVICE and PGSERVICEFILE the
# shell inherited, and arms the teardown, whose last line carries the label given here.
base_port=${TWO_BUILDS_BASE_PORT:-19071}
head_port=${TWO_BUILDS_HEAD_PORT:-19072}
envoy_port=${TWO_BUILDS_ENVOY_PORT:-19073}
two_builds_setup 'dispatch two builds e2e' "$@"

# references_hold TARGET SOURCE... reads the target's "Referenced by" (GET
# /api/v1/references?to=TARGET, whose edges are the incoming ones) and answers 0 when exactly
# the references SOURCE names point at it, in any order, 1 when others do, and 2 when the read
# itself failed — which a bounded wait takes for "not yet" and a direct caller for a failure.
#
# A source is named by the item it is. This release indexes a text from the block that holds it,
# so an edge made by a comment's citation names `<comment>#b-<block>`; which block of a comment
# carried the citation is not what this proof is about, so the fragment is dropped and a source
# citing the target from two of its own blocks counts once.
references_hold() {
  local target=$1
  shift
  local wanted found answer
  wanted=$(printf '%s\n' "$@" | sort -u)
  answer=$(call GET "/api/v1/references?to=$target") || return 2
  found=$(printf '%s' "$answer" |
    bun -e 'let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { for (const edge of JSON.parse(data).edges ?? []) console.log((edge.node?.ref ?? "").replace(/#b-.*$/, "")); });' |
    sort -u) || return 2
  references_found=$found
  [ "$found" = "$wanted" ]
}

# How far apart two block-index reconciliation ticks are when nothing is owed, read from the
# constant the loop runs on (internal/dispatch/docs, BlockIndexReconcileInterval) rather than
# guessed, so the drain below waits exactly as long as this build can take and no longer. A
# declaration this does not recognise fails the run here, rather than leaving a number of this
# script's own to go stale beside the code. It is this proof's own prerequisite, which the
# shared block runs after the checks the two proofs share.
reconcile_seconds=''
read_reconcile_interval() {
  local block_index=$root/packages/envoy/internal/dispatch/docs/block_index.go
  reconcile_seconds=$(sed -n \
    's/^[[:space:]]*BlockIndexReconcileInterval = \([0-9]\{1,\}\) \* time\.Second$/\1/p' "$block_index")
  [[ "$reconcile_seconds" =~ ^[1-9][0-9]*$ ]] ||
    fail "could not read BlockIndexReconcileInterval from $block_index"
  note "block-index reconciliation every ${reconcile_seconds}s"
}

begin prerequisites
two_builds_prerequisites \
  "--base-commit is required: it names the commit the base build is built from (the merge base with the default branch)" \
  read_reconcile_interval
pass

begin build
two_builds_build dispatch-two-builds
pass

begin reset-database
two_builds_reset_database
pass

begin fake-envoy
two_builds_fake_envoy
pass

two_builds_identity

begin base-build
start_server base "$work/bin/dispatch-base" "$base_port"
await_start base "$base_pid" 120 "the base build to answer /healthz" \
  curl -fsS --max-time 2 "$base_url/healthz"
note "the base build serves $base_url on the schema it migrated"
pass

begin seed
target_url=$base_url
actor='"actor":{"kind":"session","id":"two-builds"}'
# CORE is a person's, not the session's: this build's migration fills every project's default
# assignee from the person holding most of its issues or, failing that, from the person whose
# project.created event made it, each a `people` row
# (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), and stops on a
# project neither names. Nobody holds CORE's issues, so its creator is what the migration fills
# it from: the person sign_in records in `people` at the base build's dev sign-in, whose cookie
# makes the project.created actor {kind: user, id: <their email>}. The body names no
# default_assignee, a member a base build from before it refuses (INVALID_JSON), and no actor,
# since the cookie is the writer.
core_owner=core-owner@example.com
sign_in "$core_owner" || fail "the base build did not sign $core_owner in"
call_person POST /api/v1/projects '{"key":"CORE","name":"Core"}' >/dev/null ||
  fail "the base build refused the project"
note "the base build made CORE as $core_owner, signed in at its dev sign-in"
target_key=$(call POST /api/v1/issues "{\"project\":\"CORE\",\"title\":\"Warm the cache\",$actor}" | json_field key) ||
  fail "the base build refused the target issue"
host_key=$(call POST /api/v1/issues "{\"project\":\"CORE\",\"title\":\"Reference host\",$actor}" | json_field key) ||
  fail "the base build refused the host issue"
[ "$target_key" = "CORE-1" ] || fail "the first issue is $target_key, not CORE-1"
note "the base build made $target_key and $host_key"
# The citation is a bare issue key, which only this release's grammar reads.
written=$(call POST "/api/v1/issues/$host_key/comments" "{\"body\":\"Blocked on $target_key today.\",$actor}" | json_field id) ||
  fail "the base build refused the comment"
# Every issue is referenced by its own spec, a structural edge no text makes, so it stands in
# every reading of the target's "Referenced by" below.
spec_edge="dispatch://$target_key/spec"
note "the base build wrote the comment $written"
pass

begin base-alone
# The base build's own index holds nothing for a bare key, and nothing else is running to drain
# the queue, so the target's "Referenced by" stays empty for the whole wait. This is the control:
# without this release the citation never lands.
waited=0
while [ "$waited" -lt 45 ]; do
  held=0
  references_hold "dispatch://$target_key" "$spec_edge" || held=$?
  case "$held" in
    0) ;;
    1) fail "the base build indexed the bare key it cannot read: after ${waited}s $target_key is referenced by $(echo "$references_found" | tr '\n' ' ')" ;;
    *) fail "the base build's read of $target_key's references failed after ${waited}s" ;;
  esac
  sleep 5
  waited=$((waited + 5))
done
note "after ${waited}s under the base build alone, $target_key is referenced by nothing"
pass

begin head-build
start_server head "$work/bin/dispatch-head" "$head_port"
await_start head "$head_pid" 120 "this build to answer /healthz" \
  curl -fsS --max-time 2 "$head_url/healthz"
kill -0 "$base_pid" 2>/dev/null || fail "the base build exited when this build migrated the schema"
note "this build serves $head_url on the schema it migrated forward, beside the base build"
pass

begin overlap
# The older build, still serving, writes a second comment citing the same issue by its bare key
# (an edit of the first is the other half of the same case, and Dispatch admits a comment edit
# from a person alone, while the one cookie this run holds is CORE's creator's, taken for the seed
# alone). Neither write is indexed with this release's grammar — the build making them does not
# have it — and both are enqueued by the triggers this build's migration installed.
target_url=$base_url
second=$(call POST "/api/v1/issues/$host_key/comments" "{\"body\":\"Also blocked on $target_key.\",$actor}" | json_field id) ||
  fail "the base build refused the second comment"
kill -0 "$base_pid" 2>/dev/null || fail "the base build exited while it was writing"
note "the base build wrote $second while this build was serving"
pass

# What a timed-out drain prints before it fails: the target's "Referenced by" as the last read
# answered, and how many rows still owe this build's grammar. A drain that ran and wrote
# something else is a different failure from one that never ran, and a timeout naming neither is
# unactionable.
report_drain() {
  note "the last read of $target_key's references: $(printf '%s' "${references_found:-}" | tr '\n' ' ')"
  note "rows still owed this build's grammar: $(psql "$database_url" -X -tAc \
    'select count(*) from refs_grammar_owed' 2>&1 | tr -d '\n')"
}

begin drained
# The older build stops, and nothing else is asked of this one: no rebuild command, no edit. It
# is waited out rather than signalled and left: a process still running holds the block-index
# lease for the length of its own pass (a session advisory lock, which Postgres drops with the
# session), and a process still writing would add to what this build owes an index.
stop_pid "$base_pid"
kill -0 "$base_pid" 2>/dev/null && fail "the base build is still running after it was stopped"
base_pid=''
target_url=$head_url
# With the base build gone, the only thing between the writes it left and the target's
# "Referenced by" is this build's own reconciliation: the next tick, at most one interval away,
# and that tick's own pass. Two intervals is that bound with a whole interval to spare, and it is
# the interval's own, so a release that changes the cadence changes this wait with it.
timeout_hook=report_drain
until_true $((reconcile_seconds * 2)) "$target_key to be referenced by both comments the base build wrote" \
  references_hold "dispatch://$target_key" "$spec_edge" \
  "dispatch://$host_key/comment/$written" "dispatch://$host_key/comment/$second"
timeout_hook=''
note "$target_key is referenced by $written and $second, with no command run"
pass
