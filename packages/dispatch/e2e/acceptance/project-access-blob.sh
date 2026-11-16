#!/usr/bin/env bash
# c-blob-unchanged (dispatch://INFRA-3862, verification v17; plan task t-a-22): with no private
# project, every caller reads at this build, byte for byte, what it reads at the merge base.
#
# It builds the merge base and this checkout through scripts/e2e/lib/two-builds.sh and starts each
# on a database of its own, made here on the server DATABASE_URL names and dropped when the run
# ends, so the two seeds cannot see each other and nothing the caller's database holds is touched.
# It seeds both builds alike, through the routes both builds have: one public project PUB and one
# PRIV, public too, made by carol, each with an issue, a comment, an ask, a message, an issue
# document and a project document; three people who sign in at the dev sign-in route, every one a
# full member; a personal token of each, which names no project, since the merge base has no list
# to name one in; and the shared agent token. Then, as each of the seven callers in turn, it makes
# `POST /api/v1/projects` and reads the fixed list below from both builds, and compares the status
# and the bytes of each pair of answers.
#
# What it masks, each named, and nothing else:
#   - every id the seed writes new: each object's id as its create answered it, each personal
#     token's id, plaintext and prefix, and each document's spec id, recorded under one label per
#     object (`remember`), so the same object reads as the same label in both builds;
#   - every time the run wrote: an RFC 3339 instant between the run's start and the moment the
#     answer is masked, and each duration `durations` names, a number of seconds or milliseconds
#     measured from such an instant or by the request itself.
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
# DATABASE_URL only for the server it names. --base-commit names the merge base; left out, it is
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

# free_port prints a port the kernel chose and nothing holds. Another process can take it before
# the server binds it; two_builds_prerequisites refuses a port that answers, so a run that loses
# that race fails at once rather than talking to a stranger.
free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}
base_port=$(free_port)
head_port=$(free_port)
envoy_port=$(free_port)
# The databases this run makes, named once two_builds_setup has armed the teardown, and dropped
# by blob_cleanup only when made_databases says the create ran.
base_database_url=''
head_database_url=''
made_databases=()
two_builds_setup 'project access blob e2e' "${forwarded[@]}"

# blob_cleanup stops both servers, drops the databases this run made, and hands over to
# two-builds' cleanup, which stops the rest and exits with the run's status.
blob_cleanup() {
  local rc=$1 name
  stop_pid "$head_pid"
  stop_pid "$base_pid"
  head_pid=''
  base_pid=''
  for name in "${made_databases[@]}"; do
    psql "$database_url" -X -q -c "drop database if exists \"$name\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop database $name; drop it by hand" >&2
  done
  cleanup "$rc"
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
mkdir -p "$work/cookies" "$work/auth" "$work/answers"
pass

# own_database VAR BUILD makes the database BUILD serves, on the server DATABASE_URL names, and
# assigns VAR its connection string. The name carries this shell's pid and random bytes, so two
# runs on one server never meet. It runs in this shell, not in a substitution, so made_databases
# holds every database it made for blob_cleanup to drop.
own_database() {
  local name _url
  name="project_access_blob_$$_$(openssl rand -hex 4)_$2"
  run_database_url _url DATABASE_URL "$database_url" "$name" "this run creates and drops a database there" ||
    fail "$_url"
  psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c "create database \"$name\"" >/dev/null ||
    fail "could not create database $name"
  made_databases+=("$name")
  psql "$_url" -X -q -v ON_ERROR_STOP=1 -c 'create extension if not exists vector' >/dev/null ||
    fail "could not install the vector extension in $name, which only the cluster's master role can"
  printf -v "$1" '%s' "$_url"
}

begin databases
own_database base_database_url base
own_database head_database_url head
note "two databases of this run's own: ${made_databases[*]}"
pass

begin fake-envoy
two_builds_fake_envoy
pass

two_builds_identity

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

# The seed's people, each a full member, and the seven callers, in the order verification v17
# names their kinds: the three people at a browser, a personal token of each and the shared token.
readonly alice=alice@example.com bob=bob@outside.example carol=carol@example.com
readonly callers=(alice bob carol alice-token bob-token carol-token shared)
readonly session=project-access-blob

url_of() { if [ "$1" = base ]; then printf '%s' "$base_url"; else printf '%s' "$head_url"; fi; }
email_of() {
  case "$1" in
    alice*) printf '%s' "$alice" ;;
    bob*) printf '%s' "$bob" ;;
    carol*) printf '%s' "$carol" ;;
  esac
}

# send BUILD CALLER METHOD PATH [BODY]: one request to BUILD as CALLER, the status in $status and
# the body in $work/body. A person sends their session cookie and the build's origin, as a browser
# does; a token sends its bearer from a header file, so no argument list holds it.
send() {
  local build=$1 caller=$2 method=$3 path=$4 body=${5:-} url
  local -a credential data=()
  url=$(url_of "$build")
  case "$caller" in
    alice | bob | carol) credential=(-b "$work/cookies/$build-$caller" -H "Origin: $url") ;;
    *) credential=(-H "@$work/auth/$build-$caller") ;;
  esac
  [ -z "$body" ] || data=(-H 'Content-Type: application/json' --data-binary "$body")
  status=$(curl -sS -o "$work/body" -w '%{http_code}' --max-time "$call_seconds" -X "$method" \
    "${credential[@]}" "${data[@]}" "$url$path" 2>"$work/curl") || status="curl failed: $(<"$work/curl")"
}

# expect_status CODE WHAT: the last answer is CODE.
expect_status() {
  [ "$status" = "$1" ] || fail "$2: HTTP $status, want $1: $(head -c 600 "$work/body")"
}

# remember BUILD LABEL VALUE records VALUE, an id BUILD's seed wrote new, under LABEL: the mask
# reads it as <LABEL>, and a path in the read list names it as {LABEL}.
remember() {
  [ -n "$3" ] && [ "$3" != null ] || fail "the seed of the $1 build read no $2"
  printf '%s\t%s\n' "$2" "$3" >>"$work/ids-$1.tsv"
}
recalled() { awk -F '\t' -v label="$2" '$1 == label { print $2; exit }' "$work/ids-$1.tsv"; }

# actor: the shared token's writes name the session that makes them; a person's and a personal
# token's name none, since the credential is the writer.
with_actor() {
  if [ "$1" = shared ]; then
    jq -c --arg s "$session" '. + {actor: {kind: "session", id: $s}}' <<<"$2"
  else
    printf '%s' "$2"
  fi
}

# seed BUILD writes the seed into BUILD, in one order, so both builds hold the same rows.
seed() {
  local build=$1 person project key minted
  : >"$work/ids-$build.tsv"
  printf 'Authorization: Bearer %s\n' "$agent_token" >"$work/auth/$build-shared"
  for person in alice bob carol; do
    sign_in_as "$build" "$person"
  done
  for project in PUB PRIV; do
    send "$build" carol POST /api/v1/projects "$(jq -cn --arg k "$project" '{key: $k, name: ("Project " + $k)}')"
    expect_status 201 "$build: carol makes $project"
  done
  for person in alice bob carol; do
    send "$build" "$person" POST /api/v1/me/agent-tokens "$(jq -cn --arg n "$person's token" '{name: $n}')"
    expect_status 201 "$build: $person mints a token"
    minted=$(jq -r .token "$work/body")
    printf 'Authorization: Bearer %s\n' "$minted" >"$work/auth/$build-$person-token"
    remember "$build" "$person token" "$minted"
    remember "$build" "$person token id" "$(jq -r .id "$work/body")"
    remember "$build" "$person token prefix" "$(jq -r .prefix "$work/body")"
  done
  for project in PUB PRIV; do
    send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" \
      '{project: $p, title: ("The issue of " + $p), spec: "# Spec\n\nThe first paragraph.\n"}')"
    expect_status 201 "$build: carol opens the issue of $project"
    key=$(jq -r .key "$work/body")
    [ "$key" = "$project-1" ] || fail "$build: the issue of $project is $key, not $project-1"
    send "$build" carol GET "/api/v1/issues/$key/summary"
    expect_status 200 "$build: carol reads $key"
    remember "$build" "$project spec" "$(jq -r '.documents[] | select(.primary) | .id' "$work/body")"
    send "$build" alice POST "/api/v1/issues/$key/comments" '{"body":"A remark from alice."}'
    expect_status 201 "$build: alice comments on $key"
    remember "$build" "$project comment" "$(jq -r .id "$work/body")"
    send "$build" bob POST "/api/v1/issues/$key/asks" '{"question":"Does bob see this?"}'
    expect_status 201 "$build: bob asks on $key"
    remember "$build" "$project ask" "$(jq -r .id "$work/body")"
    send "$build" carol POST "/api/v1/issues/$key/messages" '{"body":"A message from carol."}'
    expect_status 201 "$build: carol writes a message on $key"
    remember "$build" "$project message" "$(jq -r .id "$work/body")"
    send "$build" carol POST "/api/v1/issues/$key/artifacts" '{"name":"notes.md","content":"# Notes\n\nCarol'"'"'s notes.\n"}'
    expect_status 201 "$build: carol uploads a document to $key"
    remember "$build" "$project notes" "$(jq -r .artifact.id "$work/body")"
    send "$build" carol POST "/api/v1/projects/$project/artifacts" '{"name":"Playbook.md","content":"# Playbook\n\nHow we work.\n"}'
    expect_status 201 "$build: carol uploads a document to $project"
    remember "$build" "$project playbook" "$(jq -r .artifact.id "$work/body")"
  done
}

# sign_in_as BUILD PERSON signs PERSON in at BUILD's dev sign-in route, which records them in
# `people` as a real sign-in does, and keeps their session cookie for send.
sign_in_as() {
  local build=$1 person=$2 answer
  answer=$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$call_seconds" -c "$work/cookies/$build-$person" \
    "$(url_of "$build")/auth/_dev/signin?login=$(email_of "$person")") || answer=000
  if [ "$answer" != 302 ] || ! grep -q dsession "$work/cookies/$build-$person"; then
    fail "$build: the dev sign-in of $person answered $answer with no session cookie"
  fi
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
  "/api/v1/artifacts/{{P} notes}"
  "/api/v1/artifacts/{{P} playbook}"
)

# resolve BUILD PATH prints PATH with each {LABEL} replaced by the id BUILD's seed recorded under
# it, URL-encoded as one path or query component.
resolve() {
  local build=$1 path=$2 label value
  while [[ "$path" =~ \{([^{}]+)\} ]]; do
    label=${BASH_REMATCH[1]}
    value=$(recalled "$build" "$label")
    [ -n "$value" ] || fail "the seed of the $build build recorded no $label"
    path=${path//"{$label}"/$(jq -rn --arg v "$value" '$v|@uri')}
  done
  printf '%s' "$path"
}

# The durations the mask reads as <duration>, by their JSON member: each is a time the run wrote,
# measured again at every read, so no two reads of the same row agree on it.
#   took_ms       how long a search took (GET /api/v1/search)
#   age_seconds   how old an open ask is (GET /api/v1/asks/open)
readonly durations='["took_ms","age_seconds"]'

# mask BUILD FILE prints FILE with every id BUILD's seed recorded replaced by <LABEL>, every
# RFC 3339 instant the run wrote by <time>, and every duration by <duration>, byte for byte
# otherwise: jq reads the body as one raw string and writes it back as it was.
mask() {
  local build=$1 file=$2
  jq -Rrj --slurp \
    --rawfile ids "$work/ids-$build.tsv" \
    --argjson durations "$durations" \
    --argjson from "$run_epoch" \
    --argjson to "$(date +%s)" '
      def literal: gsub("(?<c>[.*+?^$|(){}\\[\\]\\\\/])"; "\\\(.c)");
      def instant:
        (sub("\\.[0-9]+"; "") | sub("Z$"; "+00:00")) as $t
        | ($t[0:19] + "Z" | fromdateiso8601)
          - (($t[19:20] + "1" | tonumber) * (($t[20:22] | tonumber) * 3600 + ($t[23:25] | tonumber) * 60));
      def written: (try instant catch null) as $at | $at != null and $at >= $from - 1 and $at <= $to + 1;
      reduce ($ids | split("\n")[] | select(. != "") | split("\t")) as $id (.;
        gsub($id[1] | literal; "<" + $id[0] + ">"))
      | gsub("(?<t>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2}))";
          if (.t | written) then "<time>" else .t end)
      | reduce $durations[] as $member (.; gsub("\"\($member)\":[0-9]+"; "\"\($member)\":<duration>"))' "$file"
}

compared=0
differed=0
# first_difference A B prints the 1-based offset of the first byte at which the files A and B
# differ, as cmp names it; past the end of the shorter file, that is one past its length. It
# returns 1, printing nothing, when they are the same.
first_difference() {
  local said
  said=$(cmp "$1" "$2" 2>&1) && return 1
  case "$said" in
    *'is empty'*) echo 1 ;;
    *EOF*) echo $(($(sed -nE 's/.*after (byte|char) ([0-9]+).*/\2/p' <<<"$said") + 1)) ;;
    *) sed -nE 's/.*differ: (byte|char) ([0-9]+),.*/\2/p' <<<"$said" ;;
  esac
}

# compare CALLER METHOD PATH [BODY]: the base build's answer and this build's, to the same request
# made as CALLER, are the same status and, masked, the same bytes. A pair that differs is written
# to $work/differences, which the end of the run reads.
compare() {
  local caller=$1 method=$2 path=$3 body=${4:-} build at target
  for build in base head; do
    target=$(resolve "$build" "$path")
    send "$build" "$caller" "$method" "$target" "$body"
    printf '%s' "$status" >"$work/answers/$build.status"
    mask "$build" "$work/body" >"$work/answers/$build.body"
  done
  compared=$((compared + 1))
  printf '%s\n' "$(<"$work/answers/head.status")" >>"$work/statuses"
  if [ "$(<"$work/answers/base.status")" != "$(<"$work/answers/head.status")" ]; then
    differed=$((differed + 1))
    printf '%s %s %s: the base build answered HTTP %s, this build HTTP %s\n   base: %s\n   head: %s\n' \
      "$caller" "$method" "$path" "$(<"$work/answers/base.status")" "$(<"$work/answers/head.status")" \
      "$(head -c 400 "$work/answers/base.body")" "$(head -c 400 "$work/answers/head.body")" >>"$work/differences"
  elif at=$(first_difference "$work/answers/base.body" "$work/answers/head.body"); then
    differed=$((differed + 1))
    printf '%s %s %s (HTTP %s): the answers differ at byte %s\n   base: %s\n   head: %s\n' \
      "$caller" "$method" "$path" "$(<"$work/answers/head.status")" "$at" \
      "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$work/answers/base.body" | head -c 240)" \
      "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$work/answers/head.body" | head -c 240)" >>"$work/differences"
  fi
}

# no_difference fails the check when any answer so far differed, printing every one.
no_difference() {
  [ "$differed" -eq 0 ] || fail "$differed of $compared answers differ:
$(<"$work/differences")"
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
# The statuses the answers came back with, so a run whose every answer is the same refusal reads
# as that rather than as a pass over the seed.
note "$compared answers compared, each the same status and the same masked bytes at both builds;" \
  "by this build's status: $(sort "$work/statuses" | uniq -c | awk '{ printf "%s%s %s", sep, $2, $1; sep = ", " }')"
pass
