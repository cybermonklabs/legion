#!/usr/bin/env bash
# c-blob-unchanged (private projects, verification v17): with no private project, every caller
# reads at this build, byte for byte, what it reads at the merge base.
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
# reads the fixed list below from both builds, search's meaning leg among them, and compares the
# status, the headers and the bytes of each pair of answers; and it compares the frames the live
# socket sends each caller at both builds while carol comments on each project.
#
# What it masks, each named, and nothing else (access-lib.sh's mask):
#   - every id the seed writes new: each object's id as its create answered it, each document's
#     spec id and first block, and each personal token's plaintext, id and prefix, recorded under
#     one label per object (`remember`), so the same object reads as the same label in both builds;
#   - every time the run wrote: an RFC 3339 instant between the run's start and the moment the
#     answer is masked, and each duration access-lib.sh's `durations` names;
#   - the values of the headers access-lib.sh's masked_headers names, each line kept.
# Issue keys, project keys, slugs, names, versions and event numbers are the seed's own and are
# compared as they are.
#
# `GET /api/v1` is left out of the list. The route index gains a row for each route a change
# adds, and the routes golden (packages/envoy/internal/dispatch/api/testdata/routes.txt, held by
# TestRouteIndexListsEveryRouteSortedByPathThenMethod) and `go run ./cmd/dispatch openapi --check`
# hold the index and every route's description instead. So the named list of additions is empty:
# every field private projects add to an answer is omitempty at its default, and any difference
# fails. The run reads the whole list before it fails, and names, for each answer that differs,
# the caller, the route and the first byte that differs, with both answers around it.
#
# Usage, from packages/dispatch:
#
#   DATABASE_URL=<a loopback PostgreSQL this run may create databases on> \
#     bash e2e/acceptance/project-access-blob.sh \
#     [--base-commit <rev> | --base-branch <ref>] [--control private-pub | --control private-header]
#
# It needs no harness: the run starts its own servers and its own fake Envoy, and reads
# DATABASE_URL only for the server it names, whose database must hold the vector extension (the
# harness's does: its server migrated it). --base-commit names the merge base; left out, it is
# HEAD's merge base with --base-branch's ref, the branch the change is built on, origin/main when
# the run names none, chosen as base_branch_merge_base says. A change stacked on another branch
# names that branch, so the base holds every route the branch it is built on holds. --control
# private-pub is the negative control of the bodies: it makes PUB private in the head's database
# after the seed, and the run must fail naming a route. --control private-header is the negative
# control of the headers: it starts the head with DISPATCH_PRIVATE_PROJECTS=PRIV, so every answer
# says `Dispatch-Private-Projects: 1` where the declared addition is `0`, and the run must fail
# naming the header.
#
# The ports are the kernel's, each chosen as its service starts and held by that service from the
# choice on. The work directory, under TMPDIR, holds the base tree, both binaries, every process's
# log and every answer: removed when the run passes, kept and printed when it fails.
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

# This driver's own arguments, read before two_builds_setup, which takes --base-commit alone.
control=''
base_branch=origin/main
forwarded=()
while [ $# -gt 0 ]; do
  case "$1" in
    --control)
      [ $# -ge 2 ] || fail "--control takes the control to apply: private-pub or private-header"
      control=$2
      shift 2
      ;;
    --base-branch)
      [ $# -ge 2 ] || fail "--base-branch takes the ref of the branch the change is built on"
      base_branch=$2
      shift 2
      ;;
    *)
      forwarded+=("$1")
      shift
      ;;
  esac
done
case "$control" in
  '' | private-pub | private-header) ;;
  *) fail "unknown control '$control': the controls are private-pub and private-header" ;;
esac

# Every port is one the kernel chooses as its service starts (two-builds.sh's empty ports): the
# fake Envoy binds port 0 and reports it, and each build serves on a socket held from the kernel's
# choice until it serves on it, so no other process can take one in between.
base_port='' head_port='' envoy_port=''
two_builds_setup 'project access blob e2e' "${forwarded[@]}"
# Both servers and the fake Envoy stop before the databases the servers serve are dropped;
# two-builds' cleanup stops the rest and exits with the run's status.
blob_cleanup() {
  stop_all "$head_pid" "$base_pid" "$fake_envoy_pid"
  head_pid='' base_pid='' fake_envoy_pid=''
  drop_own_databases
  cleanup "$1"
}
trap 'blob_cleanup "$?"' EXIT
trap 'blob_cleanup 130' INT
trap 'blob_cleanup 143' TERM

# base_branch_merge_base TARGET prints the one merge base of HEAD and TARGET this run compares
# against: 0 with it, 1 when there is none, and 2 with UNKNOWN on stderr when it cannot say
# which. A history that merged both ways (a criss-cross: the base branch merged main, and so did
# this stack) has more than one merge base, and `git merge-base` alone answers whichever it finds
# first, which can be main's commit, below every change the base branch holds, so every route of
# the base branch would differ. So every merge base is read (`--all`), and of several the newest
# on TARGET's first-parent line, the commits the base branch itself was at, is taken. A commit
# stands at one place on that line, so two candidates cannot share the newest place; none on the
# line is UNKNOWN, naming every candidate, rather than a guess.
base_branch_merge_base() {
  local target=$1 candidate line position best='' best_position=''
  local -a candidates
  mapfile -t candidates < <(git -C "$root" merge-base --all HEAD "$target")
  [ ${#candidates[@]} -gt 0 ] || return 1
  if [ ${#candidates[@]} -eq 1 ]; then
    printf '%s\n' "${candidates[0]}"
    return 0
  fi
  line=$(git -C "$root" rev-list --first-parent "$target")
  for candidate in "${candidates[@]}"; do
    position=$(awk -v c="$candidate" '$0 == c { print NR; exit }' <<<"$line")
    if [ -n "$position" ] && { [ -z "$best_position" ] || [ "$position" -lt "$best_position" ]; }; then
      best=$candidate best_position=$position
    fi
  done
  if [ -z "$best" ]; then
    echo "UNKNOWN: HEAD and $target have ${#candidates[@]} merge bases (${candidates[*]}), and none of them is on $target's first-parent line" >&2
    return 2
  fi
  printf '%s\n' "$best"
}

if [ -z "$base_commit" ]; then
  target=$base_branch
  chose=0
  base_commit=$(base_branch_merge_base "$target") || chose=$?
  case "$chose" in
    0) note "the merge base is HEAD's with $target: $base_commit" ;;
    2) fail "the merge base of HEAD with $target is UNKNOWN (the line above names every candidate): name the base with --base-commit" ;;
    *) fail "no merge base of HEAD with $target: fetch it, or name the base with --base-commit" ;;
  esac
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
# The listener both builds read lists three sessions, so GET /api/v1/agents answers rows and not
# an empty list, in which no field of a row could differ: one whose credential lists PUB and which
# holds a role, one whose credential lists PRIV, and one the listener names no projects for, as a
# listener from before the list sends it. The first two follow their project's first issue, so
# each issue has a subscriber.
sessions='[{"session_id":"ses-pub-session","title":"Works on PUB","projects":["PUB"],"roles":["legion-planner"],"capabilities":["aside","btw","steer"]}'
sessions+=',{"session_id":"ses-priv-session","title":"Works on PRIV","projects":["PRIV"],"capabilities":["aside","btw","steer"]}'
sessions+=',{"session_id":"ses-no-list","title":"Names no project","capabilities":["btw"]}]'
curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$envoy_port/__fixture/sessions" \
  -d "$sessions" >/dev/null || fail "the fake Envoy refused its sessions"
interests='[{"session_id":"ses-pub-session","topics":["notifications.dispatch.issue.PUB-1.>"]}'
interests+=',{"session_id":"ses-priv-session","topics":["notifications.dispatch.issue.PRIV-1.>"]}]'
curl -fsS --max-time 5 -X PUT -H 'Content-Type: application/json' "http://127.0.0.1:$envoy_port/__fixture/interests" \
  -d "$interests" >/dev/null || fail "the fake Envoy refused its interests"
note "the fake Envoy lists ses-pub-session (PUB, a role), ses-priv-session (PRIV) and ses-no-list (no list), the first two following their project's first issue"
pass

two_builds_identity

# Both builds run with the test hooks on and the dev embedder, which calls no model service, so
# search answers its meaning leg beside its keyword leg, each read once both builds have settled
# their meaning index (await_index_settled). Each is given no port, so start_server holds one and
# sets base_port and head_port.
begin servers
database_url=$base_database_url start_server base "$work/bin/dispatch-base" '' \
  DISPATCH_TEST_HOOKS=1 DISPATCH_DEV_EMBEDDER=1
head_private=()
[ "$control" != private-header ] || head_private=(DISPATCH_PRIVATE_PROJECTS=PRIV)
database_url=$head_database_url start_server head "$work/bin/dispatch-head" '' \
  DISPATCH_TEST_HOOKS=1 DISPATCH_DEV_EMBEDDER=1 "${head_private[@]}"
build_url[base]=http://127.0.0.1:$base_port
build_url[head]=http://127.0.0.1:$head_port
await_start base "$base_pid" 120 "the base build to answer /healthz" curl -fsS --max-time 2 "${build_url[base]}/healthz"
await_start head "$head_pid" 120 "this build to answer /healthz" curl -fsS --max-time 2 "${build_url[head]}/healthz"
note "the base build serves ${build_url[base]}, this build ${build_url[head]}"
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

# created BUILD WHAT: the last write answered 200, 201 or 204, else the seed fails naming WHAT.
created() {
  [[ "$status" == 20[014] ]] || fail "$1: $2: HTTP $status $(head -c 300 "$work/body")"
}

# spec_ask_opened BUILD KEY: KEY's asks on BUILD hold the one its spec's `spec_done` block opened,
# whose id it sets as spec_ask. The block's ask is opened behind the write of the spec.
spec_ask_opened() {
  send "$1" carol GET "/api/v1/issues/$2/asks"
  [ "$status" = 200 ] || return 1
  spec_ask=$(jq -r '[.[] | select(.question == "Is the spec done?") | .id][0] // empty' "$work/body")
  [ -n "$spec_ask" ]
}

# The seed's writes that deliver to a session, each "ROUTE:LABEL", the object PROJECT's label
# "PROJECT LABEL" names under /api/v1/ROUTE: the shared token's mention, carol's message to the
# session, alice's replies to the two asks, carol's reply to the seed's comment and alice's reply
# to the seed's message, each of the last four naming the session too.
readonly sends=("comments:mention" "messages:targeted" "comments:ask reply" "comments:choice reply" "comments:comment reply" "messages:message reply")

# delivery_settled BUILD PROJECT ITEM: on BUILD, PROJECT's send ITEM (a "ROUTE:LABEL" of sends)
# holds a delivery attempt, and every attempt is settled (no longer pending). A reply is read
# through its thread, which the route may answer with the thread's head first, so the send's own
# row is found by its id. A delivery settles behind its write and touches its object then, so the
# seed waits for each before it writes on, and each build writes in one order.
delivery_settled() {
  local id
  id=$(recalled "$1" "$2 ${3#*:}")
  send "$1" carol GET "/api/v1/${3%%:*}/$id"
  [ "$status" = 200 ] && jq -e --arg id "$id" '[.. | objects | select(.id == $id) | .deliveries[]?.state]
    | length > 0 and all(. != "pending")' "$work/body" >/dev/null
}

# settle BUILD PROJECT ITEM waits, bounded, for delivery_settled.
settle() {
  until_true 120 "the $1 build to settle the delivery of $2's ${3#*:}" delivery_settled "$1" "$2" "$3"
}

# remember_envelopes BUILD remembers the envelope id the listener gave each settled attempt of the
# seed's sends on BUILD, as "PROJECT LABEL envelope <n>". Both builds send through the one fake
# listener, which numbers its envelopes in the order it takes them (envelope-<n>), so the same
# attempt carries another id at each build. The longest ids are remembered first: the mask
# replaces each as text, and envelope-1 is a part of envelope-12.
remember_envelopes() {
  local build=$1 project item label value
  : >"$work/envelopes"
  for project in PUB PRIV; do
    for item in "${sends[@]}"; do
      send "$build" carol GET "/api/v1/${item%%:*}/$(recalled "$build" "$project ${item#*:}")"
      expect_status 200 "$build: carol reads the $project ${item#*:}"
      jq -r --arg label "$project ${item#*:} envelope" --arg id "$(recalled "$build" "$project ${item#*:}")" \
        '[.. | objects | select(.id == $id) | .deliveries[]? | .envelope_id | select(. != null)] | unique | to_entries[]
         | "\(.value)\t\($label) \(.key + 1)"' "$work/body" >>"$work/envelopes"
    done
  done
  while IFS=$'\t' read -r value label; do
    remember "$build" "$label" "$value"
  done < <(awk -F '\t' '{ print length($1) "\t" $0 }' "$work/envelopes" | sort -t $'\t' -k1,1nr | cut -f2-)
}

# decisions_linked BUILD KEY: KEY's decisions on BUILD hold the section's link to its spec's block
# and its ask, which the store writes behind the decisions document.
decisions_linked() {
  send "$1" carol GET "/api/v1/issues/$2/decisions"
  [ "$status" = 200 ] && jq -e 'any(.decisions[]; (.links | length > 0) and (.asks | length > 0))' "$work/body" >/dev/null
}

map_repository() {
  send "$1" carol PUT "/api/v1/settings/repo-projects/$2" "$(jq -cn --arg p "$3" '{project: $p}')"
  created "$1" "carol maps $2 to $3"
}

# seed_lists BUILD writes, through the routes both builds have, a row into every list an answer
# read below holds, which the seed above leaves empty, so that no field of such a row can differ
# unseen (the lists check): a priority and the work under it, in flight and parked; each first
# issue's labels, link, blocker, child (with a link of its own), parent and design gate; each
# project's gated steps; an ask with options left open and followed, the seed's ask given options,
# followed and answered with one; replies to both asks, to the seed's comment and to its message,
# each naming the session; a comment of the shared token's that mentions the session, and a
# message to it; an issue whose spec holds a question with options, and a decisions document
# whose section names that question and its block; and a broadcast. Each send is settled before
# the next write (settle).
seed_lists() {
  local build=$1 project key ask edited_at index block
  send "$build" carol POST /api/v1/priorities '{"id":"blob-work","title":"The blob work"}'
  created "$build" "carol writes the priority blob-work"
  # A repository mapping of each project, so GET /api/v1/settings/repo-projects answers a row of
  # each, which the route answers only for a project its reader sees.
  map_repository "$build" acme/pub-map PUB
  map_repository "$build" acme/priv-map PRIV
  for project in PUB PRIV; do
    key=$project-1
    send "$build" carol PUT "/api/v1/projects/$project/gated-steps" '{"steps":["decisions","criteria","spec"]}'
    created "$build" "carol publishes $project's gated steps"
    send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" --arg k "$key" \
      '{project: $p, title: ("A child of " + $k), parent: $k, advances: ["blob-work"]}')"
    created "$build" "carol opens $project-2 under $key"
    remember "$build" "$project-2 spec" "$(jq -r .primary_artifact_id "$work/body")"
    # The create takes no external link, so the child is linked as it is parked.
    send "$build" carol PATCH "/api/v1/issues/$project-2" "$(jq -cn --arg p "$project" \
      '{status: "backlog", external_links: [{url: ("https://example.com/" + $p + "/child"), kind: "doc"}]}')"
    created "$build" "carol parks $project-2 and links it"
    send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" --arg k "$key" '{project: $p, title: ("A blocker of " + $k)}')"
    created "$build" "carol opens $project-3"
    remember "$build" "$project-3 spec" "$(jq -r .primary_artifact_id "$work/body")"
    send "$build" carol PATCH "/api/v1/issues/$project-3" '{"status":"in_progress"}'
    created "$build" "carol starts $project-3"
    send "$build" carol PATCH "/api/v1/issues/$key" "$(jq -cn --arg p "$project" \
      '{labels: ["blob"], blocked_by: [($p + "-3")], advances: ["blob-work"],
        external_links: [{url: ("https://example.com/" + $p), kind: "doc"}]}')"
    created "$build" "carol labels $key, links it, blocks it on $project-3 and has it advance blob-work"
    send "$build" carol PUT "/api/v1/issues/$key/design-gates" "$(jq -cn --arg a "$(recalled "$build" "$project spec")" \
      '{gates: [{step: "spec", artifact_id: $a}]}')"
    created "$build" "carol publishes $key's design gates"
    send "$build" carol POST "/api/v1/issues/$key/asks" '{"question":"Which way?","options":[{"label":"This way"},{"label":"That way"}]}'
    created "$build" "carol asks on $key with options"
    remember "$build" "$project choice" "$(jq -r .id "$work/body")"
    # The seed's ask is given options as it is reworded, so it is answered with one; the choice
    # keeps its own, so its edit's previous version holds them.
    send "$build" carol PATCH "/api/v1/asks/$(recalled "$build" "$project ask")" \
      '{"question":"Is this still the way?","options":[{"label":"This way"},{"label":"That way"}]}'
    created "$build" "carol rewords the $project ask and gives it options"
    # The answer below names the wording it answers, the one this edit wrote.
    edited_at=$(jq -c '.edited_at' "$work/body")
    send "$build" carol PATCH "/api/v1/asks/$(recalled "$build" "$project choice")" '{"question":"Which way now?"}'
    created "$build" "carol rewords the $project choice"
    for ask in "$project ask" "$project choice"; do
      send "$build" carol PUT "/api/v1/asks/$(recalled "$build" "$ask")/followers/ses-pub-session"
      created "$build" "carol has ses-pub-session follow the $ask"
      send "$build" alice POST "/api/v1/issues/$key/comments" "$(jq -cn --arg a "$(recalled "$build" "$ask")" \
        '{body: "A reply to the ask, for the PUB session.", ask_id: $a, mentions: [{target: "session:ses-pub-session"}], delivery: "btw"}')"
      created "$build" "alice replies to the $ask, mentioning ses-pub-session"
      remember "$build" "$ask reply" "$(jq -r .id "$work/body")"
      settle "$build" "$project" "comments:${ask#"$project "} reply"
    done
    send "$build" bob POST "/api/v1/asks/$(recalled "$build" "$project ask")/answer" \
      "$(jq -cn --argjson e "$edited_at" '{selected: ["This way"], text: "Yes, it is.", expected_edited_at: $e}')"
    created "$build" "bob answers the $project ask with This way"
    send "$build" carol POST "/api/v1/issues/$key/comments" "$(jq -cn --arg c "$(recalled "$build" "$project comment")" --arg p "$project" \
      '{body: ("A reply, naming dispatch://" + $p + "/artifact/playbook-md."), reply_to: $c,
        mentions: [{target: "session:ses-pub-session"}], delivery: "btw"}')"
    created "$build" "carol replies to the $project comment, mentioning ses-pub-session"
    remember "$build" "$project comment reply" "$(jq -r .id "$work/body")"
    settle "$build" "$project" "comments:comment reply"
    send "$build" shared POST "/api/v1/issues/$key/comments" "$(with_actor shared \
      '{"body":"For the PUB session.","mentions":[{"target":"session:ses-pub-session"}],"delivery":"btw"}')"
    created "$build" "the shared token comments on $key, mentioning ses-pub-session"
    remember "$build" "$project mention" "$(jq -r .id "$work/body")"
    settle "$build" "$project" "comments:mention"
    send "$build" alice POST "/api/v1/issues/$key/messages" "$(jq -cn --arg m "$(recalled "$build" "$project message")" \
      '{body: "A reply to carol, for the PUB session.", in_reply_to: $m, target: "session:ses-pub-session", delivery: "btw"}')"
    created "$build" "alice replies to the $project message, to ses-pub-session"
    remember "$build" "$project message reply" "$(jq -r .id "$work/body")"
    settle "$build" "$project" "messages:message reply"
    send "$build" carol POST "/api/v1/issues/$key/messages" '{"body":"For the PUB session.","target":"session:ses-pub-session","delivery":"btw"}'
    created "$build" "carol sends ses-pub-session a message on $key"
    remember "$build" "$project targeted" "$(jq -r .id "$work/body")"
    settle "$build" "$project" "messages:targeted"
    # A question written as a block of an issue's spec is one of its open questions (decisions),
    # beside each section of its decisions document. It is written into the spec the issue opens
    # with: an edit of a spec appends artifact.human_edited once the spec has been quiet for a
    # window, in each build's own time, so a later write could take its event id at one build only.
    send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" \
      '{project: $p, title: "An issue with a question",
        spec: "# Spec\n\n:::ask{#spec_done urgency=\"med\" multiple=\"false\"}\nIs the spec done?\n\n- Yes: It is.\n- No: Not yet.\n:::\n"}')"
    created "$build" "carol opens $project-4 with a question in its spec"
    remember "$build" "$project-4 spec" "$(jq -r .primary_artifact_id "$work/body")"
    until_true 60 "the $build build to open the ask $project-4's spec writes" spec_ask_opened "$build" "$project-4"
    remember "$build" "$project spec ask" "$spec_ask"
    send "$build" carol GET "$(resolve "$build" "/api/v1/artifacts/{$project-4 spec}/blocks")"
    expect_status 200 "$build: carol reads the blocks of $project-4's spec"
    index=0
    while IFS= read -r block; do
      grep -q -P "\t$block\$" "$work/ids-$build.tsv" && continue
      index=$((index + 1))
      remember "$build" "$project-4 block $index" "$block"
    done < <(jq -r '.[].id' "$work/body")
    # The section names the question and its block, as decisions_test.go's decisions do, so it
    # holds an ask and a link to a block of the spec.
    send "$build" carol POST "/api/v1/issues/$project-4/artifacts" "$(jq -cn --arg p "$project" --arg a "$spec_ask" \
      '{name: "decisions.md", content: ("# Decisions\n\n## The way\n\nWe go this way. Decided in dispatch://" + $p + "-4/ask/" + $a
        + ". Shapes dispatch://" + $p + "-4/spec#b-spec%5Fdone.\n")}')"
    created "$build" "carol writes $project-4's decisions"
    remember "$build" "$project decisions" "$(jq -r .artifact.id "$work/body")"
    until_true 60 "the $build build to link $project-4's decision" decisions_linked "$build" "$project-4"
    # The decision's text_sha256 is the sum of its text as stored, which names this build's own
    # ask id, so it is remembered as the ids are (decisions_linked read the decisions last).
    remember "$build" "$project decision sum" "$(jq -r '.decisions[0].text_sha256' "$work/body")"
    send "$build" carol GET "$(resolve "$build" "/api/v1/artifacts/{$project decisions}/blocks")"
    expect_status 200 "$build: carol reads the blocks of $project-4's decisions"
    index=0
    while IFS= read -r block; do
      index=$((index + 1))
      remember "$build" "$project decisions block $index" "$block"
    done < <(jq -r '.[].id' "$work/body")
    # A parent above the first issue, so an event of it names its ancestors.
    send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" --arg k "$key" '{project: $p, title: ("A parent of " + $k)}')"
    created "$build" "carol opens $project-5"
    remember "$build" "$project-5 spec" "$(jq -r .primary_artifact_id "$work/body")"
    send "$build" carol PATCH "/api/v1/issues/$key" "$(jq -cn --arg p "$project" '{parent: ($p + "-5")}')"
    created "$build" "carol puts $key under $project-5"
  done
  send "$build" carol POST /api/v1/broadcasts '{"body":"A broadcast.","delivery":"btw","session_ids":["ses-pub-session"]}'
  created "$build" "carol broadcasts to ses-pub-session"
  remember "$build" broadcast "$(jq -r .id "$work/body")"
  # Each recipient's copy is a message of its own, with an id the answer names.
  index=0
  while IFS= read -r block; do
    grep -q -P "\t$block\$" "$work/ids-$build.tsv" && continue
    index=$((index + 1))
    remember "$build" "broadcast copy $index" "$block"
  done < <(jq -r '.. | strings | select(test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$work/body")
  remember_envelopes "$build"
}

begin seed
seed base
seed head
seed_lists base
seed_lists head
if [ "$control" = private-pub ]; then
  psql "$head_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PUB'" >/dev/null ||
    fail "the control could not make PUB private in this build's database"
  note "control: PUB is private in this build's database"
fi
note "both builds hold PUB-1 and PRIV-1 with a comment, an ask, a message and two documents each"
pass

# The fixed list, read as every caller. {LABEL} is the id the build's seed recorded under LABEL,
# so each build is asked for its own object; the route is named as written here. The repository
# mappings (GET /api/v1/settings/repo-projects) are on it: the route answers only the rows of
# projects its reader sees, which with every project public is the answer it gave before, and
# map_repository writes a row of each project at both builds, so the list is never empty and is
# never named in unfillable_lists. The third search and the results read write a phrase, so each
# answer's query.operators holds a row. They name no OR: in a run with `"first paragraph" OR
# remark`, PRIV-1's comment and its spec were answered in one order at one build and in the other
# at the other, every caller alike. Each search but the one keywords_only names runs both legs,
# keyword and meaning.
readonly reads=(
  /auth/whoami
  /api/v1/whoami
  /api/v1/projects
  /api/v1/users
  /api/v1/me/agent-tokens
  /api/v1/settings/project-default-assignees
  /api/v1/inbox
  /api/v1/me/answers
  /api/v1/me/replies
  /api/v1/me/state
  /api/v1/events/log
  "/api/v1/search?q=paragraph"
  "/api/v1/search?q=remark"
  "/api/v1/search?q=%22first%20paragraph%22"
  "/api/v1/search/results?q=%22first%20paragraph%22&keywords_only=true"
  /api/v1/agents
  /api/v1/broadcasts
  /api/v1/priorities
  /api/v1/signins
  /api/v1/settings/repo-projects
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
  "/api/v1/issues/{P}-4/decisions"
  "/api/v1/references?to=dispatch://{P}-1"
  "/api/v1/comments/{{P} comment}"
  "/api/v1/comments/{{P} mention}"
  "/api/v1/asks/{{P} ask}"
  "/api/v1/asks/{{P} choice}"
  "/api/v1/messages/{{P} message}"
  "/api/v1/messages/{{P} targeted}"
  "/api/v1/artifacts/{{P} spec}"
  "/api/v1/artifacts/{{P} spec}/blocks"
  "/api/v1/artifacts/{{P} notes}"
  "/api/v1/artifacts/{{P} playbook}"
)

# The headers are compared as access-lib.sh's same_answers compares them. The one declared
# addition: this build answers `Dispatch-Private-Projects: 0` (KF7) on every answer, which a base
# from before it does not send, so it must be on this build's answer, and it is dropped from both
# builds' headers before they are compared: a base that already holds this change sends it too,
# and only the exact `: 0` line is dropped, so a base or a build that sends `: 1` still differs.
readonly declared_header='dispatch-private-projects: 0'

# record_lists PATH writes, for every list in this build's last answer to PATH (each JSON array,
# the answer itself included), one `ROUTE<tab>WHERE<tab>LENGTH` line to $work/lists.tsv. ROUTE is
# PATH's template: each seeded object's label ({PUB comment}), issue key (PUB-1), project key and
# person's login is written {}, and the query is dropped, so a list is one list of its route
# whichever object the route was read for: the mention's deliveries fill the comments route's
# .comment.deliveries, which the seed's own comment, sent to no one, leaves empty. WHERE names the
# list by its members with `[]` for each row of a list above it, as `.[].roles`. An answer that is
# no JSON holds no list. The lists check reads the file.
record_lists() {
  local route
  route=$(sed -E 's/\?.*//; s/\{[^}]*\}/{}/g; s#/(PUB|PRIV)-[0-9]+#/{}#g; s#/projects/(PUB|PRIV)(/|$)#/projects/{}\2#; s#/people/[^/]+/#/people/{}/#' <<<"$1")
  jq -r --arg route "$route" 'path(.. | arrays) as $at
    | [$route, ($at | map(if type == "number" then "[]" else "." + tostring end) | join("") | if . == "" then "." else . end),
       (getpath($at) | length | tostring)] | @tsv' "$work/answers/head.raw" >>"$work/lists.tsv" 2>/dev/null || true
}

# compare CALLER METHOD PATH [BODY]: the base build's answer and this build's, to the same request
# made as CALLER, are the same status, the same headers but the declared addition and, masked, the
# same bytes (same_answers).
compare() {
  local caller=$1 method=$2 path=$3 body=${4:-} build target
  for build in base head; do
    target=$(resolve "$build" "$path")
    send "$build" "$caller" "$method" "$target" "$body"
    keep_answer "$build" "$build"
  done
  same_answers "$caller $method $path" base head '' "$declared_header"
  [ "$method" != GET ] || record_lists "$path"
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
# A build pages its event log only up to its safe horizon (listEventLog, sinceHorizon), and a
# build can answer a read before that horizon has passed what the seed wrote: one run read [] from
# the merge base's log where this build listed the seed's events. So each build is read only once
# its log, as carol reads it, holds the newest event its database holds after the projects above,
# the project.created of a public project. Likewise the windows of every document, comment,
# message and ask, and the vectors of everything meaning search ranks, are written behind the
# write, in each build's own time: two runs in five read `"embedding":"pending"` at the merge base
# where this build still read `"none"`. So both builds are read only once each has settled its
# meaning index: no item owed its windows, and nothing its coverage counts lacking or failed
# (access-lib.sh's await_index_settled). A build that never settles fails the wait, naming it and
# what it still waits on. The seed settled every delivery before it wrote on, so the newest event
# is read after them.
await_index_settled "$index_settle_seconds" base head
for build in base head; do
  url_var=${build}_database_url
  newest=$(psql "${!url_var}" -X -tAc 'select coalesce(max(id), 0) from events') ||
    fail "$build: could not read the newest event of its database"
  [ "$newest" -gt 0 ] || fail "$build: its database holds no event after the seed"
  until_true 120 "the $build build's event log to reach its newest event, $newest" event_log_reaches "$build" carol "$newest"
done
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
# A run that compared no answer has not passed: the fixed list or the callers held none.
[ "$compared" -gt 0 ] || fail "no answer was compared: the fixed list or the callers held none"
note "$compared answers compared, each the same status and the same masked bytes at both builds;" \
  "by this build's status: $(statuses)"
pass

# A list that is empty in every answer compares no row, so a field this build added to its rows
# would pass unseen: on 2026-11-19 a `projects` member on every agents row passed while the
# listener listed no session. So every list of every answer read above, by route and by where in
# the answer it stands, must hold a row in at least one caller's answer, or be named in
# unfillable_lists with why no seed can fill it. Each key is `ROUTE<tab>WHERE`, each half a shell
# pattern, in which a list's `[]` is written `\[\]`. A list a key names that holds a row fails too,
# and so does a key that names no list read, so the table cannot keep a reason that stopped being
# true.
declare -A unfillable_lists=(
  ["/api/v1/inbox	\[\].thread.answers"]="the Inbox lists open asks alone, and an answer closes its ask"
  ["/api/v1/*	\[\].payload.deliveries"]="a comment's or message's event carries it as it was written, before any delivery attempt exists: comment_create.go writes its deliveries as [], and createStoredMessage publishes its event before the send"
  ["/api/v1/priorities	.work_under_no_priority.issues\[\].advances"]="the first fault's rows advance no live priority, so their advances is the empty list a zero advancesScan answers (priority_queries.go, priorityFaultRow.targets)"
  ["*	*components.ids"]="a component id must name a component of its project's architecture model, which is read from GitHub, and this run starts no GitHub"
  ["*	*components.unknown"]="a component the model does not hold is refused at the write (400 COMPONENTS_INPUT), so an issue names none"
)
# unfillable KEY-VAR ROUTE WHERE sets KEY-VAR to the unfillable_lists key whose patterns match
# ROUTE and WHERE, and fails when none does.
unfillable() {
  local key
  for key in "${!unfillable_lists[@]}"; do
    # shellcheck disable=SC2053 # each half of the key is a pattern
    if [[ "$2" == ${key%%	*} && "$3" == ${key#*	} ]]; then
      printf -v "$1" '%s' "$key"
      return 0
    fi
  done
  return 1
}
begin lists
[ -s "$work/lists.tsv" ] || fail "no answer held a list, so the reads above compared no row"
awk -F '\t' '{ key = $1 "\t" $2; if (!(key in most) || $3 + 0 > most[key]) most[key] = $3 + 0 }
  END { for (key in most) print key "\t" most[key] }' "$work/lists.tsv" | sort >"$work/lists-most.tsv"
empty='' stale='' matched_key=''
declare -A key_matched=()
while IFS=$'\t' read -r route where most; do
  if unfillable matched_key "$route" "$where"; then
    key_matched[$matched_key]=1
    [ "$most" -eq 0 ] ||
      stale+="$route at $where holds $most rows, though unfillable_lists says ${unfillable_lists[$matched_key]}"$'\n'
  elif [ "$most" -eq 0 ]; then
    empty+="$route at $where"$'\n'
  fi
done <"$work/lists-most.tsv"
for key in "${!unfillable_lists[@]}"; do
  [ -n "${key_matched[$key]+set}" ] ||
    stale+="unfillable_lists names ${key/	/ at }, and no list read above is it"$'\n'
done
[ -z "$empty$stale" ] || fail "$(printf '%s' "$empty" | grep -c .) lists were empty in every caller's answer, so no row of theirs was compared:
$empty${stale:+and unfillable_lists is wrong:
$stale}"
note "$(wc -l <"$work/lists-most.tsv") lists read, each holding a row in at least one caller's answer but those unfillable_lists names (${#unfillable_lists[@]} patterns)"
pass

# The live socket, last, since it writes: each caller subscribes with no resume at both builds at
# once, and while both sockets are open carol comments on PUB-1 and then on PRIV-1 at each build.
# Each build's answer is the frames it sent the caller (socket-access.ts live, which prints each
# event's id as <event>), each comment's id remembered under one label at both builds, and the two
# answers must be the same. With every project public every caller is sent both comments, so an
# answer at this build that holds no event fails: the compare must see the socket send something.
begin sockets
declare -A live_pid=()
# live_settled READY PID: the live probe PID has written its ready file READY, or has exited.
live_settled() { [ -e "$1" ] || ! kill -0 "$2" 2>/dev/null; }
for caller in "${callers[@]}"; do
  for build in base head; do
    rm -f "$work/live-ready-$build" "$work/live-written-$build"
    bun "$(dirname "$0")/socket-access.ts" live "${build_url[$build]}" "$work/auth/$build-$caller" \
      "$work/live-ready-$build" "$work/live-written-$build" >"$work/live-body-$build" 2>&1 &
    live_pid[$build]=$!
  done
  # A probe that ends before it is ready, as one whose upgrade is refused does (it prints
  # `http <status> <body>` and writes no ready file), ends the wait at once: its output is its
  # answer, kept and compared as any other.
  for build in base head; do
    until_true 10 "$caller's live socket on the $build build to start, or its probe to end" \
      live_settled "$work/live-ready-$build" "${live_pid[$build]}"
  done
  for build in base head; do
    for project in PUB PRIV; do
      send "$build" carol POST "/api/v1/issues/$project-1/comments" "$(jq -cn --arg p "$project" '{body: ("A live remark on " + $p + ".")}')"
      expect_status 201 "$build: carol comments on $project-1 while $caller's live socket is open"
      remember "$build" "live $project comment for $caller" "$(jq -r .id "$work/body")"
    done
  done
  for build in base head; do
    : >"$work/live-written-$build"
  done
  for build in base head; do
    status=socket
    wait "${live_pid[$build]}" || status=error
    cp "$work/live-body-$build" "$work/body"
    keep_answer "$build" "$build"
  done
  problem=''
  grep -q '"type":"event"' "$work/answers/head.body" ||
    problem="$caller /ws/live: this build sent no event of carol's comments, so the compare saw nothing: $(head -c 300 "$work/answers/head.body")"
  judge "$(<"$work/answers/head.status")" "$problem"
  same_answers "$caller /ws/live" base head
done
no_difference
note "every caller is sent the same frames at both builds"
pass
