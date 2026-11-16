#!/usr/bin/env bash
# c-blob-unchanged (dispatch://INFRA-3862, verification v17; plan task t-a-22): with no private
# project, every caller reads at this build, byte for byte, what it reads at the merge base.
#
# It builds the merge base and this checkout through scripts/e2e/lib/two-builds.sh and starts each
# on a database of its own, made here on the server DATABASE_URL names and dropped when the run
# ends, so the two seeds cannot see each other and nothing the caller's database holds is touched.
# It seeds both builds alike, through the routes both builds have (access-lib.sh): three people
# who sign in at the dev sign-in route, every one a full member; one public project PUB and one
# PRIV, public too, made by carol, each with an issue whose spec is a document with blocks, a
# comment, an ask, a message, an issue document and a project document; a personal token of each
# person, which names no project, since the merge base has no list to name one in; and the shared
# agent token. Then, as each of the seven callers in turn, it makes `POST /api/v1/projects` and
# reads the fixed list below from both builds, and compares the status and the bytes of each pair
# of answers.
#
# What it masks, each named, and nothing else (access-lib.sh's mask):
#   - every id the seed writes new: each object's id as its create answered it, each document's
#     spec id and first block, and each personal token's plaintext, id and prefix, recorded under
#     one label per object (`remember`), so the same object reads as the same label in both builds;
#   - every time the run wrote: an RFC 3339 instant between the run's start and the moment the
#     answer is masked, and each duration access-lib.sh's `durations` names.
# Issue keys, project keys, slugs, names, versions and event numbers are the seed's own and are
# compared as they are.
#
# `GET /api/v1` is left out of the list. The route index gains a row for each route this issue
# adds, and the routes golden (packages/envoy/internal/dispatch/api/testdata/routes.txt, held by
# TestRouteIndexLists) and `go run ./cmd/dispatch openapi --check` hold the index and every route's
# description instead. So the named list of additions is empty: every field this issue adds is
# omitempty at its default, and any difference fails. The run reads the whole list before it
# fails, and names, for each answer that differs, the caller, the route and the first byte that
# differs, with both answers around it.
#
# Usage, from packages/dispatch:
#
#   DATABASE_URL=<a loopback PostgreSQL this run may create databases on> \
#     bash e2e/acceptance/with-harness.sh bash e2e/acceptance/project-access-blob.sh \
#     [--base-commit <rev>] [--control private-pub]
#
# The harness is not needed: the run starts its own servers and its own fake Envoy, and reads
# DATABASE_URL only for the server it names, whose database must hold the vector extension (the
# harness's does: its server migrated it). --base-commit names the merge base; left out, it is
# `git merge-base` of HEAD with origin/legion/INFRA-1149, the branch this stack is built on, or
# with origin/main once that branch is gone. --control private-pub is the negative control: it
# makes PUB private in the head's database after the seed, and the run must fail naming a route.
#
# The ports are the kernel's, chosen at the start. The work directory, under TMPDIR, holds the
# base tree, both binaries, every process's log and every answer: removed when the run passes,
# kept and printed when it fails.
set -euo pipefail

root=$(cd "$(dirname "$0")/../../../.." && pwd)
started=$SECONDS
# The run's start, the earliest instant an answer can hold that the run wrote.
run_epoch=$(date +%s)
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
# await_start log under $evidence/logs, and a timed-out until_true runs timeout_hook, which this
# driver leaves empty.
evidence=''
# shellcheck disable=SC2034 # read by lib/rig.sh's until_true
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=../../../../scripts/e2e/lib/two-builds.sh
. "$root/scripts/e2e/lib/two-builds.sh"
# shellcheck source-path=SCRIPTDIR source=access-lib.sh
. "$(dirname "$0")/access-lib.sh"

# This driver's own argument, read before two_builds_setup, which takes --base-commit alone.
control=''
forwarded=()
while [ $# -gt 0 ]; do
  case "$1" in
    --control)
      [ $# -ge 2 ] || fail "--control takes the control to apply: private-pub"
      control=$2
      shift 2
      ;;
    *)
      forwarded+=("$1")
      shift
      ;;
  esac
done
case "$control" in
  '' | private-pub) ;;
  *) fail "unknown control '$control': the one control is private-pub" ;;
esac

base_port=$(free_port)
head_port=$(free_port)
envoy_port=$(free_port)
two_builds_setup 'project access blob e2e' "${forwarded[@]}"
# Both servers stop before the databases they serve are dropped; two-builds' cleanup stops the
# rest and exits with the run's status.
blob_cleanup() {
  stop_pid "$head_pid"
  stop_pid "$base_pid"
  head_pid=''
  base_pid=''
  drop_own_databases
  cleanup "$1"
}
trap 'blob_cleanup "$?"' EXIT
trap 'blob_cleanup 130' INT
trap 'blob_cleanup 143' TERM

if [ -z "$base_commit" ]; then
  target=origin/legion/INFRA-1149
  git -C "$root" rev-parse --verify --quiet "$target^{commit}" >/dev/null || target=origin/main
  base_commit=$(git -C "$root" merge-base "$target" HEAD) ||
    fail "no merge base of HEAD with $target: fetch it, or name the base with --base-commit"
  note "the merge base is HEAD's with $target"
fi

# This driver's own prerequisite, beside the ones the two proofs share: it reads and masks every
# answer with jq.
require_jq() { command -v jq >/dev/null || fail "jq is not on PATH"; }

begin prerequisites
two_builds_prerequisites "--base-commit names the merge base" require_jq
pass

begin build
two_builds_build project-access-blob
pass

begin databases
base_database_url=''
head_database_url=''
own_database base_database_url base
own_database head_database_url head
note "two databases of this run's own: ${made_databases[*]}"
pass

begin fake-envoy
two_builds_fake_envoy
pass

two_builds_identity
build_url[base]=$base_url
build_url[head]=$head_url

# Both builds run with the test hooks on and no embedder, so search answers its keyword leg alone
# and no answer waits on a model call or on a queue that drains in its own time.
begin servers
database_url=$base_database_url start_server base "$work/bin/dispatch-base" "$base_port" \
  DISPATCH_TEST_HOOKS=1 DISPATCH_TEST_EMBEDDER_OFF=1
database_url=$head_database_url start_server head "$work/bin/dispatch-head" "$head_port" \
  DISPATCH_TEST_HOOKS=1 DISPATCH_TEST_EMBEDDER_OFF=1
await_start base "$base_pid" 120 "the base build to answer /healthz" curl -fsS --max-time 2 "$base_url/healthz"
await_start head "$head_pid" 120 "this build to answer /healthz" curl -fsS --max-time 2 "$head_url/healthz"
note "the base build serves $base_url, this build $head_url"
pass

# The seven callers, in the order verification v17 names their kinds: the three people at a
# browser, a personal token of each and the shared token.
readonly callers=(alice bob carol alice-token bob-token carol-token shared)

# seed BUILD writes the seed into BUILD, in one order, so both builds hold the same rows. Every
# person is a full member and both projects are public, the deployment R9 speaks of.
seed() {
  local build=$1 person
  seed_people "$build"
  make_project "$build" PUB
  make_project "$build" PRIV
  for person in alice bob carol; do
    mint_token "$build" "$person" "$person-token"
  done
  seed_objects "$build" PUB alice
  seed_objects "$build" PRIV bob
}

begin seed
seed base
seed head
if [ "$control" = private-pub ]; then
  psql "$head_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PUB'" >/dev/null ||
    fail "the control could not make PUB private in this build's database"
  note "control: PUB is private in this build's database"
fi
note "both builds hold PUB-1 and PRIV-1 with a comment, an ask, a message and two documents each"
pass

# The fixed list, read as every caller. {LABEL} is the id the build's seed recorded under LABEL,
# so each build is asked for its own object; the route is named as written here.
readonly reads=(
  /auth/whoami
  /api/v1/whoami
  /api/v1/projects
  /api/v1/users
  /api/v1/me/agent-tokens
  /api/v1/settings/project-default-assignees
  /api/v1/settings/repo-projects
  /api/v1/inbox
  /api/v1/me/answers
  /api/v1/me/replies
  /api/v1/me/state
  /api/v1/events/log
  "/api/v1/search?q=paragraph"
  "/api/v1/search?q=remark"
  /api/v1/agents
  /api/v1/broadcasts
  /api/v1/priorities
  /api/v1/signins
  "/api/v1/people/$carol/nodes"
  "/api/v1/people/$alice/nodes"
)
readonly project_reads=(
  "/api/v1/issues?project={P}"
  "/api/v1/asks/open?project={P}"
  "/api/v1/projects/{P}/artifacts"
  "/api/v1/projects/{P}/artifacts/playbook-md"
  "/api/v1/issues/{P}-1"
  "/api/v1/issues/{P}-1/summary"
  "/api/v1/issues/{P}-1/comments"
  "/api/v1/issues/{P}-1/asks"
  "/api/v1/issues/{P}-1/messages"
  "/api/v1/issues/{P}-1/artifacts"
  "/api/v1/issues/{P}-1/artifacts/notes-md"
  "/api/v1/issues/{P}-1/events"
  "/api/v1/issues/{P}-1/collaborators"
  "/api/v1/issues/{P}-1/subscribers"
  "/api/v1/issues/{P}-1/references"
  "/api/v1/issues/{P}-1/decisions"
  "/api/v1/references?to=dispatch://{P}-1"
  "/api/v1/comments/{{P} comment}"
  "/api/v1/asks/{{P} ask}"
  "/api/v1/messages/{{P} message}"
  "/api/v1/artifacts/{{P} spec}"
  "/api/v1/artifacts/{{P} spec}/blocks"
  "/api/v1/artifacts/{{P} notes}"
  "/api/v1/artifacts/{{P} playbook}"
)

# compare CALLER METHOD PATH [BODY]: the base build's answer and this build's, to the same request
# made as CALLER, are the same status and, masked, the same bytes (same_answers).
compare() {
  local caller=$1 method=$2 path=$3 body=${4:-} build target
  for build in base head; do
    target=$(resolve "$build" "$path")
    send "$build" "$caller" "$method" "$target" "$body"
    keep_answer "$build" "$build"
  done
  same_answers "$caller $method $path" base head
}

begin create-projects
# Each caller makes a project of its own, in the callers' order, on both builds, and the two
# answers are compared like any read: POST /api/v1/projects's answer is on the fixed list.
for caller in "${callers[@]}"; do
  key=$(tr -d '-' <<<"${caller^^}" | cut -c1-8)
  compare "$caller" POST /api/v1/projects "$(with_actor "$caller" "$(jq -cn --arg k "$key" '{key: $k, name: ("Made by " + $k)}')")"
done
no_difference
note "every caller's project answer is the same at both builds"
pass

begin reads
for caller in "${callers[@]}"; do
  for path in "${reads[@]}"; do
    compare "$caller" GET "$path"
  done
  for project in PUB PRIV; do
    for path in "${project_reads[@]}"; do
      compare "$caller" GET "${path//\{P\}/$project}"
    done
  done
done
no_difference
note "$compared answers compared, each the same status and the same masked bytes at both builds;" \
  "by this build's status: $(statuses)"
pass
