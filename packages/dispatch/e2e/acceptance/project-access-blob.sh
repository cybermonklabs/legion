#!/usr/bin/env bash
# c-blob-unchanged (private projects, verification v17): with no private project, every caller
# reads at this build, byte for byte, what it reads at the merge base, on each route the fixed
# lists below name and on the live socket's event feed; the routes it does not compare, each with
# why, are named below under "Not compared".
#
# It builds the merge base and this checkout through scripts/e2e/lib/two-builds.sh and starts each
# on a database of its own, made here on the server DATABASE_URL names and dropped when the run
# ends, so the two seeds cannot see each other and nothing the caller's database holds is touched.
# It seeds both builds alike, through the routes both builds have (access-lib.sh): three people
# who sign in at the dev sign-in route, every one a full member; one public project PUB and one
# PRIV, public too, made by carol, each with an issue whose spec is a document with blocks, a
# comment, an ask, a message, an issue document and a project document; a personal token of each
# person, which names no project, since the merge base has no list to name one in; the shared
# agent token; and, written straight into each build's database, Delivery configured, with pull
# requests and runs (seed_delivery), and a repository mapping of each project (map_repository).
# Then, as each of the seven callers in turn, it makes `POST /api/v1/projects`, makes,
# renames and deletes a Delivery session group, and reads the fixed list below from both builds,
# search's meaning leg among them, and compares the status, the headers and the bytes of each pair
# of answers; and it compares the frames the live socket's event feed sends each caller at both
# builds while carol comments on each project.
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
# hold the index and every route's description instead. So the change itself declares no addition
# to a body: every field private projects add to an answer is omitempty at its default (its one
# declared addition is a header, declared_header below). The one list of differences a body may
# hold is main_additions, below: a member main added after the commit the base is pinned to, each
# entry naming main's commit, the route and the member's path, and each held to that commit being
# in this build and not in the base, and to admitting at least one answer (access-lib.sh's
# main_additions_in_force and main_additions_report). Any other difference fails.
# The run reads the whole list before it fails, and names, for each answer that differs, the
# caller, the route and the first byte that differs, with both answers around it.
#
# Not compared, each with why:
#   - `GET /api/v1`, above.
#   - The live socket's Delivery views (the delivery hub: timeline, measures and pipeline
#     subscriptions on /ws/live) and its agent view (a session's messages): socket-access.ts
#     subscribes the event feed alone. Their GET routes are on the fixed list, and the hub's
#     snapshots and deltas are held to those routes' answers by the api package's tests
#     (delivery_live_hub_test.go, delivery_live_hub_access_test.go, live_agent_test.go).
#   - `GET /api/v1/access-groups`: this change pages it, answering `{groups, next}` where the
#     merge base answered the array of groups, so no byte of the two can match; its pages are held
#     by access_groups_test.go.
#   - What the GitHub App's routes answer with an App key: this run starts no GitHub and gives
#     neither build a key, so `GET /api/v1/github/app-installation/{owner}/{repo}` and the proxy
#     (`GET /api/github/rest/...`) are compared as each caller is answered at a deployment with no
#     App key, the refusal both answer before any repository is read. Whether the App is installed
#     on a repository, and what the proxy forwards, are held by github_app_installation_test.go
#     and the githubapi package's tests, and by project-access.sh's proxy section.
#   - A route this build serves that the base does not: the base has no answer to hold it to, only
#     its router's no-route 404. A route is new when this build's own route index (`GET /api/v1`)
#     lists it and the base's does not, never by an answer's status or body, since a hidden
#     object's 404 is a 404 too and a served route's gate answers the no-route body on purpose; a
#     hidden object stands at a route the base lists, and a route neither index lists, as
#     /auth/whoami and the GitHub proxy, is compared. Each new route is read at neither build and
#     named once on a line `SKIP new route: METHOD PATTERN`, and the reads section counts them.
#     A base whose index cannot be read is UNKNOWN (exit 2), never a skip (access-lib.sh's
#     read_route_indexes and new_route). A change that merges in another line's routes, which its
#     base lacks, names that line's commit with --base-commit to be compared against it.
# The lists check reads JSON arrays only: a member that is null or absent in every answer (an
# object no seed fills, an omitempty member) is compared as that null or absence, and holds no row.
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
# naming the header. It exits 0 on a pass, 1 on a failure, and 2 when a build's route index
# cannot be read (UNKNOWN: whether a route is new cannot be told).
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

# Main's changes after the commit the base is pinned to, each a member main added to an answer on
# the fixed list (access-lib.sh's main_additions, which says how an entry is written and held). The
# list is empty: the base the checks name holds main's changes up to the main it holds, and every
# answer then is the same at both builds. An entry is added only for a member main adds after that
# base, and is removed once the base holds its commit, where it would admit nothing. An entry
# written as
#   $'<40-hex commit>\tGET /api/v1/issues/{key}\t.design_gates[].holds\tnull'
# admits each answer to an issue whose design gates differ from the base's only by a `holds`
# member, null at this build.
readonly main_additions=()

begin prerequisites
two_builds_prerequisites "--base-commit names the merge base" require_jq
main_additions_in_force "$root" "$base_sha"
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
# session, alice's replies to the two asks, carol's reply to the seed's comment, alice's reply to
# the seed's message and carol's follow-up to her message, each of the last five naming the
# session too.
readonly sends=("comments:mention" "messages:targeted" "comments:ask reply" "comments:choice reply" "comments:comment reply" "messages:message reply" "messages:follow-up")

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
# seed's sends on BUILD, as "PROJECT LABEL envelope <n>", and of each copy of its broadcast, as
# "broadcast copy <n> envelope <m>", once that copy's delivery has settled. Both builds send
# through the one fake listener, which numbers its envelopes in the order it takes them
# (envelope-<n>), so the same attempt carries another id at each build. The longest ids are
# remembered first: the mask replaces each as text, and envelope-1 is a part of envelope-12.
remember_envelopes() {
  local build=$1 project item label value index=1
  : >"$work/envelopes"
  for project in PUB PRIV; do
    for item in "${sends[@]}"; do
      envelopes_of "$build" "$project" "$item"
    done
  done
  while [ -n "$(recalled "$build" "broadcast copy $index")" ]; do
    settle "$build" broadcast "messages:copy $index"
    envelopes_of "$build" broadcast "messages:copy $index"
    index=$((index + 1))
  done
  while IFS=$'\t' read -r value label; do
    remember "$build" "$label" "$value"
  done < <(awk -F '\t' '{ print length($1) "\t" $0 }' "$work/envelopes" | sort -t $'\t' -k1,1nr | cut -f2-)
}

# envelopes_of BUILD OWNER ITEM writes to $work/envelopes, one `ID<tab>LABEL` line each, the
# envelope ids of the attempts of OWNER's send ITEM (a "ROUTE:LABEL", read as delivery_settled
# reads it), each labelled "OWNER LABEL envelope <n>".
envelopes_of() {
  local id
  id=$(recalled "$1" "$2 ${3#*:}")
  send "$1" carol GET "/api/v1/${3%%:*}/$id"
  expect_status 200 "$1: carol reads the $2 ${3#*:}"
  jq -r --arg label "$2 ${3#*:} envelope" --arg id "$id" \
    '[.. | objects | select(.id == $id) | .deliveries[]? | .envelope_id | select(. != null)] | unique | to_entries[]
     | "\(.value)\t\($label) \(.key + 1)"' "$work/body" >>"$work/envelopes"
}

# decisions_linked BUILD KEY: KEY's decisions on BUILD hold the section's link to its spec's block
# and its ask, which the store writes behind the decisions document.
decisions_linked() {
  send "$1" carol GET "/api/v1/issues/$2/decisions"
  [ "$status" = 200 ] && jq -e 'any(.decisions[]; (.links | length > 0) and (.asks | length > 0))' "$work/body" >/dev/null
}

# map_repository BUILD REPO PROJECT writes REPO's mapping to PROJECT straight into BUILD's database,
# as the row that PUT /api/v1/settings/repo-projects/{owner}/{repo} wrote for carol's cookie and a
# body of {project} alone, before #4457 (85f90626) removed it (api/repo_projects.go,
# putRepoProject, at 5d01144423): not its project's default, renamed to nothing, created by carol
# as her cookie's writes record her, and created now. A build that holds #4457 serves no write of
# the mappings, its PUT answering 404, while GET /api/v1/settings/repo-projects still answers the
# rows, so the seed writes them the same way at both builds whatever routes each serves. The PUT
# also appended a settings.repo_project.updated event, which this write does not, at either build.
map_repository() {
  local url_var=${1}_database_url actor
  actor=$(jq -cn --arg id "$carol" '{kind: "user", id: $id, proven: true}')
  psql "${!url_var}" -X -q -v ON_ERROR_STOP=1 -v repo="$2" -v project="$3" -v actor="$actor" \
    >/dev/null <<'SQL' || fail "$1: could not write the mapping of $2 to $3"
insert into repo_projects (repo, project, is_default, created_by)
values (:'repo', :'project', false, :'actor'::jsonb)
SQL
}

# seed_lists BUILD writes, through the routes both builds have (and the repository mappings
# straight into its database, map_repository), a row into every list an answer read below holds,
# which the seed above leaves empty, so that no field of such a row can differ unseen (the lists
# check): a priority and the work under it, in flight and parked; each first
# issue's labels, link, blocker, child (with a link of its own), parent, design gate and role
# holder; each project's gated steps; an ask with options left open and followed, the seed's ask
# given options, followed and answered with one; replies to both asks, to the seed's comment and
# to its message, each naming the session, the reply to the seed's comment naming the other
# project's first issue too, so each first issue's references hold a row in linked_from, which
# lists only a node outside the issue's own content that links into it; a comment of the shared
# token's that mentions the session, and a message to it, which the session answers and carol
# follows up; an issue whose spec holds a question with options, and a decisions document whose
# section names that question and its block; an open P0 issue of each project with no owner
# (Delivery's unowned_p0); a broadcast; and a Delivery session group. Each send is settled before
# the next write (settle).
seed_lists() {
  local build=$1 project key ask edited_at index block other
  send "$build" carol POST /api/v1/priorities '{"id":"blob-work","title":"The blob work"}'
  created "$build" "carol writes the priority blob-work"
  # A repository mapping of each project, so GET /api/v1/settings/repo-projects answers a row of
  # each, which the route answers only for a project its reader sees.
  map_repository "$build" acme/pub-map PUB
  map_repository "$build" acme/priv-map PRIV
  for project in PUB PRIV; do
    key=$project-1
    other=PRIV
    [ "$project" = PUB ] || other=PUB
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
    # P0, open and with no claim or route, so the Delivery measures list it as a P0 with no owner.
    send "$build" carol PATCH "/api/v1/issues/$project-3" '{"status":"in_progress","priority":0}'
    created "$build" "carol starts $project-3 at P0"
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
    send "$build" carol POST "/api/v1/issues/$key/comments" "$(jq -cn --arg c "$(recalled "$build" "$project comment")" --arg p "$project" --arg o "$other-1" \
      '{body: ("A reply, naming dispatch://" + $p + "/artifact/playbook-md and dispatch://" + $o + "."), reply_to: $c,
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
    # ses-pub-session answers that message's attempt, as its agent does, and carol follows it up to
    # the session, so the session's targeted messages list replies, one of them delivered; the
    # shared token writes as the session.
    send "$build" shared POST "/api/v1/messages/$(recalled "$build" "$project targeted")/reply" \
      '{"actor":{"kind":"session","id":"ses-pub-session"},"attempt":1,"body":"It is noted."}'
    created "$build" "ses-pub-session answers carol's message on $key"
    remember "$build" "$project targeted answer" "$(jq -r .id "$work/body")"
    send "$build" carol POST "/api/v1/issues/$key/messages" "$(jq -cn --arg m "$(recalled "$build" "$project targeted")" \
      '{body: "One more thing, for the PUB session.", in_reply_to: $m, target: "session:ses-pub-session", delivery: "btw"}')"
    created "$build" "carol follows up her message to ses-pub-session on $key"
    remember "$build" "$project follow-up" "$(jq -r .id "$work/body")"
    settle "$build" "$project" "messages:follow-up"
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
    # The Legion daemon of the project names the session that holds the first issue's implementer
    # role, as it publishes a claim; the shared token writes as that daemon's session.
    send "$build" shared PUT "/api/v1/issues/$key/role-holders" "$(jq -cn --arg p "$project" \
      '{holders: [{role: "implementer", session: ("ses-" + ($p | ascii_downcase) + "-session")}],
        actor: {kind: "session", id: ("legion-daemon:" + $p)}}')"
    created "$build" "the $project daemon names $key's implementer"
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
  # A session group the timeline lists, holding the session no project names.
  send "$build" carol POST /api/v1/delivery/session-groups '{"name":"Blob group","sessions":["ses-no-list"]}'
  created "$build" "carol groups ses-no-list"
  remember "$build" "session group" "$(jq -r .id "$work/body")"
}

# seed_delivery BUILD configures Delivery on BUILD and writes the rows its intake and reconcile
# would have written, in delivery-seed.ts's shape, straight into the build's database, since this
# run gives neither build an App key (the settings route answers 409 DELIVERY_SETTINGS_ACCESS
# without one) and starts no GitHub. The one delivery_settings row, as scripts/e2e's verification
# driver writes it, makes the pipeline, the measures and the timeline answer their arrays rather
# than 404 DELIVERY_NOT_CONFIGURED; the rows fill them: three pull requests of the population, one
# attributed to each project's first issue and one, a rework, to no issue; two deploy runs on main
# that ship the first two, and a failed one after the third; and a failed and a passing PR-checks
# run; each run with its jobs. Every instant is a whole number of hours before the run's start, so
# both builds hold the same rows, compared as written (the mask reads only the instants the run
# wrote after its start).
seed_delivery() {
  local build=$1 url_var=${1}_database_url hours
  local -a at=()
  for hours in 2 3 4 5 6 7 8 9 10 19 20 21 22 27 28 29 30 192 200; do
    at[hours]=$(date -u -d "@$((run_epoch - hours * 3600))" +%Y-%m-%dT%H:%M:%SZ)
  done
  psql "${!url_var}" -X -q -v ON_ERROR_STOP=1 >/dev/null <<SQL || fail "$build: could not write the Delivery rows"
insert into delivery_settings (
  singleton, deploy_repo, deploy_workflow_path, production_job_names, pr_checks_workflow_path,
  population_authors, excluded_repos, updated_by
) values (
  true, 'acme/pub-repo', '.github/workflows/deploy.yml', array['release'], '.github/workflows/ci.yml',
  array['octocat'], array[]::text[], '{"kind":"system","id":"blob-seed"}'
);
-- The fourth pull request was merged eight days before the run, before the timeline's default
-- seven-day window, and had not shipped by its start, so the timeline lists it as waiting. The
-- first names the session that wrote it, which the timeline lists as its session.
insert into delivery_pull_requests (
  repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
) values
  ('acme/pub-repo', 1, 'feat: PUB''s shipped change', 'https://github.com/acme/pub-repo/pull/1', 'octocat',
   '${at[30]}', '${at[29]}', 12, 3, false, 'PUB-1', array['ses-pub-session'], false),
  ('acme/pub-repo', 2, 'feat: PRIV''s shipped change', 'https://github.com/acme/pub-repo/pull/2', 'octocat',
   '${at[22]}', '${at[21]}', 7, 2, false, 'PRIV-1', array[]::text[], false),
  ('acme/pub-repo', 3, 'fix: a waiting rework', 'https://github.com/acme/pub-repo/pull/3', 'octocat',
   '${at[10]}', '${at[9]}', 4, 1, true, null, array[]::text[], false),
  ('acme/pub-repo', 4, 'feat: PUB''s change merged before the window', 'https://github.com/acme/pub-repo/pull/4', 'octocat',
   '${at[200]}', '${at[192]}', 5, 0, false, 'PUB-1', array[]::text[], false);
-- The slowest-jobs table ranks a PR-checks job only with measures.SlowestMinResults (50) timed
-- results in the shown days: fifty passing runs of 'build / unit', 40 to 89 hours before the run.
insert into delivery_runs (
  repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
)
select 'acme/pub-repo', 800 + i, 'pr_checks', 3, 'blob' || (800 + i),
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i),
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i),
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i),
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i) + make_interval(mins => 10 + i),
  'success', 'https://github.com/acme/pub-repo/actions/runs/' || (800 + i), null, 'pull_request'
from generate_series(0, 49) as i;
insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
select 'acme/pub-repo', 800 + i, 'build / unit',
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i),
  to_timestamp(${run_epoch}) - make_interval(hours => 40 + i) + make_interval(mins => 10 + i),
  'success'
from generate_series(0, 49) as i;
insert into delivery_runs (
  repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
) values
  ('acme/pub-repo', 500, 'deploy', null, 'blob500', '${at[29]}', '${at[28]}', '${at[28]}', '${at[27]}', 'success',
   'https://github.com/acme/pub-repo/actions/runs/500', 'main', 'push'),
  ('acme/pub-repo', 501, 'deploy', null, 'blob501', '${at[21]}', '${at[20]}', '${at[20]}', '${at[19]}', 'success',
   'https://github.com/acme/pub-repo/actions/runs/501', 'main', 'push'),
  ('acme/pub-repo', 502, 'deploy', null, 'blob502', '${at[9]}', '${at[8]}', '${at[8]}', '${at[7]}', 'failure',
   'https://github.com/acme/pub-repo/actions/runs/502', 'main', 'push'),
  ('acme/pub-repo', 700, 'pr_checks', 3, 'blob700', '${at[10]}', '${at[6]}', '${at[6]}', '${at[5]}', 'failure',
   'https://github.com/acme/pub-repo/actions/runs/700', null, 'pull_request'),
  ('acme/pub-repo', 701, 'pr_checks', 3, 'blob701', '${at[10]}', '${at[4]}', '${at[4]}', '${at[3]}', 'success',
   'https://github.com/acme/pub-repo/actions/runs/701', null, 'pull_request');
insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) values
  ('acme/pub-repo', 500, 'build', '${at[28]}', '${at[28]}', 'success'),
  ('acme/pub-repo', 500, 'release', '${at[28]}', '${at[27]}', 'success'),
  ('acme/pub-repo', 501, 'build', '${at[20]}', '${at[20]}', 'success'),
  ('acme/pub-repo', 501, 'release', '${at[20]}', '${at[19]}', 'success'),
  ('acme/pub-repo', 502, 'build', '${at[8]}', '${at[8]}', 'success'),
  ('acme/pub-repo', 502, 'integration tests', '${at[8]}', '${at[7]}', 'failure'),
  ('acme/pub-repo', 502, 'release', null, null, 'skipped'),
  ('acme/pub-repo', 700, 'build / unit', '${at[6]}', '${at[5]}', 'failure'),
  ('acme/pub-repo', 700, 'lint', '${at[6]}', '${at[6]}', 'success'),
  ('acme/pub-repo', 701, 'build / unit', '${at[4]}', '${at[3]}', 'success'),
  ('acme/pub-repo', 701, 'lint', '${at[4]}', '${at[4]}', 'success');
SQL
}

begin seed
seed base
seed head
seed_lists base
seed_lists head
seed_delivery base
seed_delivery head
if [ "$control" = private-pub ]; then
  psql "$head_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PUB'" >/dev/null ||
    fail "the control could not make PUB private in this build's database"
  note "control: PUB is private in this build's database"
fi
note "both builds hold PUB-1 and PRIV-1 with a comment, an ask, a message and two documents each"
pass

begin route-index
# Each build's own route index, read as carol: a route this build's index lists and the base's does
# not is read at neither build below, and named on a line `SKIP new route: METHOD PATTERN` the first
# time a read reaches it (access-lib.sh's new_route); every other route is compared. An index that
# cannot be read is UNKNOWN, exit 2, and no route is skipped.
read_route_indexes
note "the base build's route index lists $(wc -l <"$work/route-index-base") routes, this build's $(wc -l <"$work/route-index-head")"
pass

# The fixed list, read as every caller. {LABEL} is the id the build's seed recorded under LABEL,
# so each build is asked for its own object; the route is named as written here. The repository
# mappings (GET /api/v1/settings/repo-projects) are on it: the route answers only the rows of
# projects its reader sees, which with every project public is the answer it gave before, and
# map_repository writes a row of each project into each build's database, so the list is never
# empty and is never named in unfillable_lists. The third search and the results read write a
# phrase, so each answer's query.operators holds a row. They name no OR: in a run with
# `"first paragraph" OR remark`, PRIV-1's comment and its spec were answered in one order at one
# build and in the other at the other, every caller alike. Each search but the one keywords_only
# names runs both legs, keyword and meaning.
#
# The routes this change rewrote are on it too. Delivery is configured and holds rows at both builds
# (seed_delivery), so its three reads answer their arrays, not 404 DELIVERY_NOT_CONFIGURED. Its
# pipeline is read at the run's start (`at`), so its seven days are one set at both builds even
# when a read falls past midnight; its measures and timeline read their default window, whose ends
# the mask reads as <time>, and the timeline lists the seed's pull requests and session group. A
# session's targeted messages and what reached it are read for both sessions the projects name,
# PUB's holding the seed's messages and replies. The
# GitHub App's installation lookup and the proxy are read for the repository PUB is mapped to, as
# a deployment with no App key answers them (see "Not compared" above).
pipeline_at=$(date -u -d "@$run_epoch" +%Y-%m-%dT%H:%M:%SZ)
readonly pipeline_at
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
  "/api/v1/delivery/pipeline?at=$pipeline_at"
  /api/v1/delivery/measures
  /api/v1/delivery/timeline
  /api/v1/agents/ses-pub-session/messages
  /api/v1/agents/ses-pub-session/answers
  /api/v1/agents/ses-priv-session/messages
  /api/v1/agents/ses-priv-session/answers
  /api/v1/github/app-installation/acme/pub-map
  /api/github/rest/repos/acme/pub-map/pulls/1
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
  "/api/v1/issues/{P}-1/role-holders"
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
# access-lib.sh's compare, which every read below goes through, reads it.
readonly declared_header='dispatch-private-projects: 0'

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

begin session-groups
# Each caller, in the callers' order, makes a Delivery session group of ses-priv-session, renames
# it and deletes it, on both builds, each answer compared like any read. The group's id is new at
# each build, so it is remembered under one label at both before its answer is kept. A make that
# either build refuses leaves nothing to rename or delete, and its refusal is the compare.
for caller in "${callers[@]}"; do
  # A route only this build serves is held to nothing at the base (access-lib.sh's new_route).
  if new_route POST /api/v1/delivery/session-groups; then
    continue
  fi
  for build in base head; do
    send "$build" "$caller" POST /api/v1/delivery/session-groups \
      "$(with_actor "$caller" "$(jq -cn --arg c "$caller" '{name: ("Group of " + $c), sessions: ["ses-priv-session"]}')")"
    [ "$status" != 201 ] || remember "$build" "group of $caller" "$(jq -r .id "$work/body")"
    keep_answer "$build" "$build"
  done
  same_answers "$caller POST /api/v1/delivery/session-groups" base head '' "$declared_header" \
    "POST /api/v1/delivery/session-groups"
  [ -n "$(recalled base "group of $caller")" ] && [ -n "$(recalled head "group of $caller")" ] || continue
  compare "$caller" PATCH "/api/v1/delivery/session-groups/{group of $caller}" \
    "$(with_actor "$caller" "$(jq -cn --arg c "$caller" '{name: ("Renamed by " + $c)}')")"
  compare "$caller" DELETE "/api/v1/delivery/session-groups/{group of $caller}" "$(with_actor "$caller" '{}')"
done
no_difference
note "every caller's session group answers are the same at both builds"
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
# Each main addition in force names the answers it admitted, and one that admitted none fails the
# run once the differences, if any, are printed. The reads are the last compares an entry can
# reach: the sockets below are no route's answer.
main_additions_report
no_difference
main_additions_used
# A run that compared no answer has not passed: the fixed list or the callers held none.
[ "$compared" -gt 0 ] || fail "no answer was compared: the fixed list or the callers held none"
note "$compared answers compared, each the same status and the same masked bytes at both builds" \
  "but the members of any main addition admitted above;" "by this build's status: $(statuses)"
note "$new_routes new routes skipped, $skipped_requests requests to them read at neither build (each route named on its SKIP line above)"
pass

# A list that is empty in every answer compares no row, so a field this build added to its rows
# would pass unseen: on 2026-11-19 a `projects` member on every agents row passed while the
# listener listed no session. So every list of every answer read above, by route and by where in
# the answer it stands, must hold a row in at least one caller's answer, or be named in
# unfillable_lists with why no seed can fill it. Each key is `ROUTE<tab>WHERE`, each half a shell
# pattern, in which a list's `[]` is written `\[\]`. A list a key names that holds a row fails too,
# and so does a key that names no list read, so the table cannot keep a reason that stopped being
# true. It checks arrays only: an object member, or a nullable one, that is null or absent in
# every caller's answer holds no row and is not named here, and is compared only as that null or
# absence (the header's "Not compared").
declare -A unfillable_lists=(
  ["/api/v1/inbox	\[\].thread.answers"]="the Inbox lists open asks alone, and an answer closes its ask"
  ["/api/v1/*	\[\].payload.deliveries"]="a comment's or message's event carries it as it was written, before any delivery attempt exists: comment_create.go writes its deliveries as [], and createStoredMessage publishes its event before the send"
  ["/api/v1/priorities	.work_under_no_priority.issues\[\].advances"]="the first fault's rows advance no live priority, so their advances is the empty list a zero advancesScan answers (priority_queries.go, priorityFaultRow.targets)"
  ["*	*components.ids"]="a component id must name a component of its project's architecture model, which is read from GitHub, and this run starts no GitHub"
  ["*	*components.unknown"]="a component the model does not hold is refused at the write (400 COMPONENTS_INPUT), so an issue names none"
  ["/api/v1/delivery/timeline	.prs\[\].components"]="a pull request's components are those its issue names, each a component of its project's architecture model, which is read from GitHub, and this run starts no GitHub"
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
  # The written file names both comments, which every caller is sent with every project public,
  # PRIV-1's last: the probe reads until the feed has sent every event its log lets the caller
  # read of them (socket-access.ts), however long an open transaction holds the feed back, and the
  # feed sends in id order, so anything it sends of the writes before the last comment has come by
  # then. The file is written whole and then moved into place, so the probe reads no part of it.
  for build in base head; do
    printf '%s\n' "$(recalled "$build" "live PUB comment for $caller")" "$(recalled "$build" "live PRIV comment for $caller")" \
      >"$work/live-written-$build.part"
    mv "$work/live-written-$build.part" "$work/live-written-$build"
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
run_end
