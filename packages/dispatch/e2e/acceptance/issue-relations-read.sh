#!/usr/bin/env bash
# The check c-issue-relations-read: every relation between issues is a tuple with one writer, read
# back in one read, and the issue read answers its query nodes. It runs against the server of
# `bun run dispatch:local`, on the database that server serves, and seeds every issue it reads.
#
#   R1 It creates, in PROJECT, a parent P, a sibling S, an issue D and a third issue T holding an
#      agent's comment and message; a project of the run's own holding an issue with a comment;
#      and the issue I under P, waiting on S, whose duplicate_of is D, written through the
#      duplicate_of field of POST /api/v1/issues.
#   R2 Through PATCH /api/v1/issues/{key} it sets I's filed_from to the comment's canonical
#      reference, and then sends five values the field refuses: an issue key, the comment's block
#      reference (#b-), a comment of the other project, a comment reference whose uuid no row holds
#      and a block of a spec. Each is 400 naming filed_from, and I's filed_from still names the
#      comment after each. A 200 fails naming the value that was taken.
#   R3 GET /api/v1/references?from=<I> answers child_of P, needs S, duplicate_of D and filed_from
#      the comment, one edge of each kind, each at its target.
#   R4 `dispatch read --issue I` prints the same four, once each, in its Links: section.
#   R5 `dispatch issue --duplicate-of I` creates I2, and `dispatch issue-update --filed-from` sets
#      its filed_from to the message's canonical reference; the store holds I2's current
#      duplicate_of tuple at I and its filed_from tuple at the message.
#   R6 PATCH /api/v1/issues/{I2} {needs: [S]} writes a dependency: the store holds the needs tuple
#      from I2 to S and issue_links holds the row (I2, blocked_by, S). A failure prints both reads.
#   R7 The issue read of I, which is converted, answers in verification_artifact_id and
#      plan_artifact_id the artifact ids of its verification and plan query nodes, and its spec as
#      its primary document, the store holding a query node for each of its four queries. While a
#      server process that lists no issue_room capability is live, an issue is born unconverted:
#      the driver stands one in (a copy of the server's own row of server_processes, with no
#      capabilities and a heartbeat of its own, removed again before the section ends) and
#      creates U, whose issue read answers in those two fields the ids of its role documents,
#      neither of them a query node.
#   R8 It searches packages/dispatch/web/src, packages/envoy-client/src and packages/daemon for a
#      reader of either field, after finding a reader it plants in a scratch copy of one file, and
#      finds none.
#
# Inputs: DATABASE_URL (the database the server serves, on this machine: the driver reads the store
# through psql and stands R7's process row there), DISPATCH_URL (the server), DISPATCH_TOKEN or
# DISPATCH_TOKEN_FILE (the agent bearer, read here and never printed) and PROJECT (an existing
# project key). `bun` runs the dispatch CLI of this checkout. Each unmet input is exit 2. The agent
# bearer writes every issue, comment and message, and closes the issues when the run ends, also
# when a check fails; the other project stays, since no route removes a project.
#
# It must run alone on its server: no other driver, test or person may write to that server while
# it runs. For the few seconds R7's stand-in process row is live, every issue that server creates
# is born unconverted, whoever creates it, so another run's issue would not be converted as that
# run expects; and another run that stood in a process row of its own, or waited on the fleet's
# build announcement, would read this row as a live process of an earlier build.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line. The
# last line is `PASS: issue-relations-read.sh complete 8 sections`. Every check is `cmd || fail …`,
# never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE bash e2e/acceptance/issue-relations-read.sh
       (run from packages/dispatch against `bun run dispatch:local`; DATABASE_URL names the
       database that server serves, on this machine; DISPATCH_TOKEN_FILE may stand in for
       DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL DISPATCH_URL PROJECT
require_commands bun curl grep jq psql
# shellcheck disable=SC2031 # the run's own DATABASE_URL
loopback_database_url "$DATABASE_URL" || {
  printf 'issue-relations-read.sh: DATABASE_URL names no single loopback host; the driver writes a process row into the store it names\n' >&2
  usage
  exit 2
}
read_bearer

readonly driver=issue-relations-read.sh
# shellcheck disable=SC2031 # the caller's DISPATCH_URL; only lib.sh's `cli` exports another
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2031 # the run's own DATABASE_URL
readonly database_url="$DATABASE_URL"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="issue-relations-read-$stamp"
# The checkout's root, whose three trees R8 searches.
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
readonly repo_root

work_dir
tree_init
tree_cli_init

keys=()
hold_instance=""
hold_pid=""
cleanup() {
  local code=$? closing
  trap - EXIT INT TERM
  issue_relations_read_release_room
  for closing in "${keys[@]}"; do
    close_issue "$closing"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# issue_relations_read_create SECTION TITLE JSON: the agent creates an issue of `project` titled
# TITLE, the members of the JSON object JSON added to the create, forced past the near-copy check;
# sets issue_relations_read_key and adds it to the issues the run closes.
issue_relations_read_create() {
  tree_send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$2" --argjson more "$3" --argjson a "$(tree_actor)" \
    '{project: $p, title: $t, spec: ("The issue \"" + $t + "\" of the relations check."), force: true, actor: $a} + $more')"
  tree_want 201 "$1" "create the issue \"$2\""
  tree_field "$1" "create the issue \"$2\"" .key
  [[ "$tree_value" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "create the issue \"$2\"" "the key $tree_value is no issue key"
  issue_relations_read_key="$tree_value"
  keys+=("$tree_value")
}

# issue_relations_read_filed_from SECTION ISSUE WANT: the issue read of ISSUE answers filed_from
# WANT.
issue_relations_read_filed_from() {
  tree_send GET "/api/v1/issues/$2"
  tree_want 200 "$1" "read $2"
  expect "$1" "$2's filed_from names $3" "$(jq -cn --arg f "$3" '$f')" '.filed_from'
}

# issue_relations_read_hold_room SECTION: stands in a live server process of a build that lists
# no issue_room capability, so an issue created meanwhile is born unconverted (fleetServesIssueRoom
# in api/issue_create.go): a copy of the server's own row of server_processes under an instance of
# its own, its capabilities null, as a process of the release before writes them, and a heartbeat
# the driver stamps every 2 s, well inside the 15 s live window (processes.LiveWindow).
issue_relations_read_hold_room() {
  query "$1" "make an instance id" "select gen_random_uuid()"
  hold_instance="$answer"
  psql_run "$1" "stand in a live process that lists no issue_room" -v instance="$hold_instance" <<'SQL'
begin;
create temp table held on commit drop as
  select * from server_processes where heartbeat_at > now() - interval '15 seconds' order by started_at limit 1;
update held set instance = :'instance'::uuid, capabilities = null, started_at = now(), heartbeat_at = now();
insert into server_processes select * from held;
commit;
SQL
  (
    while :; do
      env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -X -q -At \
        -c "update server_processes set heartbeat_at = now() where instance = '$hold_instance'" >/dev/null 2>&1 || true
      sleep 2
    done
  ) &
  hold_pid=$!
  want_value "$1" "a live process lists no issue_room" 1 \
    "select count(*) from server_processes where instance = '$hold_instance' and capabilities is null and heartbeat_at > now() - interval '15 seconds'"
}

# issue_relations_read_release_room: removes the process issue_relations_read_hold_room stood in,
# its heartbeat first; safe to call twice and when none stands. A failure is printed on stderr.
issue_relations_read_release_room() {
  if [[ -n "$hold_pid" ]]; then
    kill "$hold_pid" 2>/dev/null || true
    wait "$hold_pid" 2>/dev/null || true
    hold_pid=""
  fi
  if [[ -n "$hold_instance" ]]; then
    env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -X -q -At \
      -c "delete from server_processes where instance = '$hold_instance'" >/dev/null 2>"$work/release" ||
      printf '%s: could not remove the process row %s: %s\n' "$driver" "$hold_instance" "$(<"$work/release")" >&2
    hold_instance=""
  fi
}

# issue_relations_read_readers DIR…: prints each reader of the issue read's verification_artifact_id
# or plan_artifact_id under DIR, as file:line:text: a line of a TypeScript or Go file that is no
# test and no comment and reads either field as a member (`.field`, `?.field`), names it in quotes
# (a Pick of the type, a Go json tag) or destructures it, leaving out the results read's own member
# of that name (`results.verification_artifact_id`, GET /api/v1/issues/{key}/results).
issue_relations_read_readers() {
  grep -rnE --include='*.ts' --include='*.tsx' --include='*.go' \
    --exclude='*.test.ts' --exclude='*.test.tsx' --exclude='*_test.go' --exclude-dir=__tests__ \
    --exclude-dir=node_modules \
    '\??\.(verification_artifact_id|plan_artifact_id)\b|"(verification_artifact_id|plan_artifact_id)[",]|\b(verification_artifact_id|plan_artifact_id)\b[^:]*\}[[:space:]]*=' \
    "$@" 2>/dev/null |
    grep -vE '^[^:]+:[0-9]+:[[:space:]]*(//|/?\*|#)' |
    grep -vE 'results(\.data)?\??\.verification_artifact_id' || true
}

# --- R1: the issues, the items and the relations of the create -------------------------------------

issue_relations_read_create R1 "Relations check: the parent $stamp" '{}'
readonly parent="$issue_relations_read_key"
issue_relations_read_create R1 "Relations check: the sibling it waits on $stamp" '{}'
readonly sibling="$issue_relations_read_key"
issue_relations_read_create R1 "Relations check: the issue it repeats $stamp" '{}'
readonly repeated="$issue_relations_read_key"
issue_relations_read_create R1 "Relations check: the third issue, holding the items $stamp" '{}'
readonly third="$issue_relations_read_key"
corpus_comment agent R1 "$third" "The comment the relations check's issue is filed from."
readonly comment_id="$corpus_item"
readonly comment_ref="dispatch://$third/comment/$comment_id"
corpus_message agent R1 "$third" "The message the relations check's second issue is filed from."
readonly message_id="$corpus_item"
readonly message_ref="dispatch://$third/message/$message_id"
pass R1 "created $parent, $sibling, $repeated and $third, which holds the comment $comment_ref and the message $message_ref"

with_vars R1 "read the default assignee of $project" \
  "select coalesce((select default_assignee from projects where key = :'p'), '')" p="$project"
[[ -n "$answer" ]] || fail R1 "read the default assignee of $project" "the project names none"
another_project R1 "$answer"
tree_send POST /api/v1/issues "$(jq -cn --arg p "$other_project" --argjson a "$(tree_actor)" \
  '{project: $p, title: "Relations check: an issue of another project", force: true, actor: $a}')"
tree_want 201 R1 "create an issue of $other_project"
tree_field R1 "create an issue of $other_project" .key
readonly foreign="$tree_value"
keys+=("$foreign")
corpus_comment agent R1 "$foreign" "A comment of another project, which no issue of $project is filed from."
readonly foreign_ref="dispatch://$foreign/comment/$corpus_item"
pass R1 "created $foreign in $other_project, holding the comment $foreign_ref"

cat >"$work/issue-spec.md" <<'SPEC'
The issue whose relations the check reads back in one read.

:::requirement{#r1}
The issue reads its relations back as tuples.
:::
SPEC
tree_send POST /api/v1/issues "$(jq -cn --arg p "$project" --rawfile s "$work/issue-spec.md" --arg parent "$parent" \
  --arg needs "$sibling" --arg d "$repeated" --argjson a "$(tree_actor)" \
  '{project: $p, title: "Relations check: the issue whose relations are read", spec: $s, parent: $parent,
    needs: [$needs], duplicate_of: $d, force: true, actor: $a}')"
tree_want 201 R1 "create the issue under $parent, waiting on $sibling, a duplicate of $repeated"
tree_field R1 "create the issue" .key
# Not readonly: seed-corpus.sh's and lib-tree.sh's writers and reads take a local of that name.
issue="$tree_value"
keys+=("$issue")
pass R1 "created $issue under $parent, waiting on $sibling, its duplicate_of $repeated written by the create"

# --- R2: filed_from takes a conversation item of the project, and nothing else ---------------------

tree_send PATCH "/api/v1/issues/$issue" "$(jq -cn --arg f "$comment_ref" --argjson a "$(tree_actor)" '{filed_from: $f, actor: $a}')"
tree_want 200 R2 "set $issue's filed_from to $comment_ref"
issue_relations_read_filed_from R2 "$issue" "$comment_ref"
query R2 "make a comment id no row holds" \
  "select u::text from gen_random_uuid() u where not exists (select 1 from comments where id = u)"
readonly unheld_id="$answer"
[[ -n "$unheld_id" ]] || fail R2 "make a comment id no row holds" "the store holds the id it made"
refused_values=(
  "$third"
  "$comment_ref#b-b1"
  "$foreign_ref"
  "dispatch://$third/comment/$unheld_id"
  "dispatch://$issue/spec#b-r1"
)
refused_words=(
  "an issue key"
  "the comment's block reference"
  "a comment of another project"
  "a comment reference no row holds"
  "a block of a spec"
)
for index in "${!refused_values[@]}"; do
  value="${refused_values[$index]}"
  tree_send PATCH "/api/v1/issues/$issue" "$(jq -cn --arg f "$value" --argjson a "$(tree_actor)" '{filed_from: $f, actor: $a}')"
  [[ "$status" != 2?? ]] ||
    fail R2 "filed_from refuses ${refused_words[$index]}" \
      "PATCH /api/v1/issues/$issue answered HTTP $status for filed_from $value, want 400 naming filed_from: $(body)"
  tree_want 400 R2 "filed_from refuses ${refused_words[$index]}, $value"
  jq -e '.error | type == "string" and contains("filed_from")' "$work/body" >/dev/null ||
    fail R2 "the refusal of $value names filed_from" "it reads $(body)"
  pass R2 "filed_from refuses ${refused_words[$index]}, $value, with 400 naming filed_from"
  issue_relations_read_filed_from R2 "$issue" "$comment_ref"
done

# --- R3: one read answers the four relations -------------------------------------------------------

# The node each relation's edge names: an issue by its key, the comment by its uuid; each beside
# its canonical reference.
declare -A relation_id=([child_of]="$parent" [needs]="$sibling" [duplicate_of]="$repeated" [filed_from]="$comment_id")
declare -A relation_ref=([child_of]="dispatch://$parent" [needs]="dispatch://$sibling" [duplicate_of]="dispatch://$repeated" [filed_from]="$comment_ref")
declare -A relation_node=([child_of]=issue [needs]=issue [duplicate_of]=issue [filed_from]=comment)
readonly relations=(child_of needs duplicate_of filed_from)
tree_send GET "/api/v1/references?from=$(q "dispatch://$issue")"
tree_want 200 R3 "read the relations of $issue"
cp "$work/body" "$work/references.json"
for kind in "${relations[@]}"; do
  got="$(jq -c --arg k "$kind" '[.edges[] | select(.kind == $k) | {kind: .node.kind, id: .node.id, ref: (.node.ref // null)}]' \
    "$work/references.json")" || fail R3 "read the $kind edges of $issue" "unreadable: $(head -c 600 "$work/references.json")"
  jq -e --arg n "${relation_node[$kind]}" --arg i "${relation_id[$kind]}" --arg r "${relation_ref[$kind]}" \
    'length == 1 and .[0].kind == $n and (.[0].id == $i or .[0].ref == $r)' <<<"$got" >/dev/null ||
    fail R3 "GET /api/v1/references?from=dispatch://$issue answers $kind once, at ${relation_ref[$kind]}" "its $kind edges are $got"
  pass R3 "GET /api/v1/references?from=dispatch://$issue answers $kind once, at ${relation_ref[$kind]}"
done

# --- R4: dispatch read --issue prints them under Links: --------------------------------------------

run R4 "read $issue" read --issue "$issue"
awk 'found && /^- / { print; next } found { exit } /^Links:$/ { found = 1 }' "$work/out" >"$work/links"
[[ -s "$work/links" ]] || fail R4 "dispatch read --issue $issue prints a Links: section" "it prints: $(<"$work/out")"
for kind in "${relations[@]}"; do
  target="${relation_id[$kind]}"
  of_kind="$(grep -c -- "^- $kind " "$work/links" || true)"
  at_target="$(grep -cE -- "^- $kind ${relation_node[$kind]} (dispatch://([^ ]*/)?)?$target \\(" "$work/links" || true)"
  [[ "$of_kind" == 1 && "$at_target" == 1 ]] ||
    fail R4 "the Links: section of $issue prints $kind once, at ${relation_ref[$kind]}" \
      "it prints $of_kind $kind lines, $at_target of them at the target: $(<"$work/links")"
  pass R4 "the Links: section of $issue prints $kind once, at ${relation_ref[$kind]}"
done

# --- R5: the CLI's two flags write tuples ------------------------------------------------------------

printf 'A second issue the relations check writes through the CLI.\n' >"$work/duplicate-spec.md"
run R5 "create a duplicate of $issue" issue --project "$project" --title "Relations check: a duplicate of $issue" \
  --duplicate-of "$issue" --spec-file "$work/duplicate-spec.md" --force
[[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
  fail R5 "create a duplicate of $issue" "no issue key in: $(<"$work/out")"
readonly duplicate="${BASH_REMATCH[1]}"
keys+=("$duplicate")
pass R5 "dispatch issue --duplicate-of $issue created $duplicate"
run R5 "set $duplicate's filed_from" issue-update --issue "$duplicate" --filed-from "$message_ref"
pass R5 "dispatch issue-update --filed-from $message_ref wrote $duplicate"
tree_far_ends R5 issue duplicate_of "$duplicate"
[[ "$answer" == "issue:$issue" ]] ||
  fail R5 "the store holds $duplicate's duplicate_of tuple at $issue" "its current duplicate_of tuples name '$answer'"
pass R5 "the store holds $duplicate's duplicate_of tuple at $issue"
tree_far_ends R5 issue filed_from "$duplicate"
[[ "$answer" == "message:$message_id" ]] ||
  fail R5 "the store holds $duplicate's filed_from tuple at the message $message_id" "its current filed_from tuples name '$answer'"
pass R5 "the store holds $duplicate's filed_from tuple at the message $message_id"

# --- R6: a dependency written through the issue route is a needs tuple, and issue_links its row ------

tree_send PATCH "/api/v1/issues/$duplicate" "$(jq -cn --arg s "$sibling" --argjson a "$(tree_actor)" '{needs: [$s], actor: $a}')"
tree_want 200 R6 "make $duplicate wait on $sibling"
tree_far_ends R6 issue needs "$duplicate"
readonly tuples_read="$answer"
with_vars R6 "read the issue_links rows of $duplicate" \
  "select coalesce(string_agg(issue_key || ' ' || kind || ' ' || target_key, ',' order by target_key), '')
   from issue_links where issue_key = :'k'" k="$duplicate"
readonly links_read="$answer"
[[ "$tuples_read" == "issue:$sibling" && "$links_read" == "$duplicate blocked_by $sibling" ]] ||
  fail R6 "the dependency $duplicate -> $sibling is a needs tuple, and issue_links holds its row" \
    "issue_links holds '$links_read'; the current needs tuples out of $duplicate name '$tuples_read'; want '$duplicate blocked_by $sibling' beside 'issue:$sibling'"
pass R6 "the dependency $duplicate -> $sibling is the needs tuple 'issue:$sibling', and issue_links holds '$links_read'"

# --- R7: the issue read answers its query nodes ------------------------------------------------------

printf 'How %s is proved.\n\n:::check{#c1 serves="dispatch://%s/spec#b-r1"}\nThe relations read back.\n:::\n' \
  "$issue" "$issue" >"$work/verification.md"
corpus_upload R7 "$issue" verification.md "$work/verification.md"
printf 'How %s is built.\n' "$issue" >"$work/plan.md"
corpus_upload R7 "$issue" plan.md "$work/plan.md"
tree_read agent "$issue"
tree_want 200 R7 "read the tree of $issue"
expect R7 "$issue is converted" true '.converted'
with_vars R7 "read the query nodes of $issue" \
  "select coalesce(json_object_agg(n.attributes->>'query', a.id::text)::text, '{}')
   from artifacts a join graph_nodes n on n.kind = 'artifact' and n.id = a.ref_key and n.supertag = 'query'
   where a.issue_key = :'k'" k="$issue"
queries="$answer"
readonly queries
jq -e '(keys | sort) == ["decisions", "plan", "spec", "verification"]' <<<"$queries" >/dev/null ||
  fail R7 "the store holds a query node for each of $issue's four queries" "it holds $queries"
pass R7 "the store holds a query node for each of $issue's four queries: $queries"
tree_send GET "/api/v1/issues/$issue"
tree_want 200 R7 "read $issue"
expect R7 "the issue read of $issue answers its query nodes in primary_artifact_id, verification_artifact_id and plan_artifact_id" \
  "$(jq -cS '{primary: .spec, verification: .verification, plan: .plan}' <<<"$queries")" \
  '{primary: .primary_artifact_id, verification: .verification_artifact_id, plan: .plan_artifact_id}'

issue_relations_read_hold_room R7
issue_relations_read_create R7 "Relations check: an issue born while a process lists no issue room $stamp" '{}'
readonly unconverted="$issue_relations_read_key"
printf 'How %s is proved.\n' "$unconverted" >"$work/verification.md"
corpus_upload R7 "$unconverted" verification.md "$work/verification.md"
printf 'How %s is built.\n' "$unconverted" >"$work/plan.md"
corpus_upload R7 "$unconverted" plan.md "$work/plan.md"
tree_read agent "$unconverted"
tree_want 200 R7 "read the tree of $unconverted"
expect R7 "$unconverted, born while a process listed no issue room, is not converted" false '.converted'
with_vars R7 "read the role documents of $unconverted" \
  "select json_build_object(
     'verification', (select id::text from artifacts where issue_key = :'k' and role = 'verification'),
     'plan', (select id::text from artifacts where issue_key = :'k' and role = 'plan'),
     'queries', (select count(*) from artifacts a join graph_nodes n on n.kind = 'artifact' and n.id = a.ref_key
                 and n.supertag = 'query' where a.issue_key = :'k' and a.role in ('verification', 'plan')))::text" \
  k="$unconverted"
roles="$answer"
readonly roles
jq -e '.verification != null and .plan != null and .queries == 0' <<<"$roles" >/dev/null ||
  fail R7 "$unconverted holds a verification and a plan role document, neither a query node" "it holds $roles"
tree_send GET "/api/v1/issues/$unconverted"
tree_want 200 R7 "read $unconverted"
expect R7 "the issue read of $unconverted answers its role documents in verification_artifact_id and plan_artifact_id" \
  "$(jq -cS '{verification, plan}' <<<"$roles")" '{verification: .verification_artifact_id, plan: .plan_artifact_id}'
readonly stood_in="$hold_instance"
issue_relations_read_release_room
want_value R7 "the process $stood_in the driver stood in is gone" 0 \
  "select count(*) from server_processes where instance = '$stood_in'"

# --- R8: no page, CLI or daemon reads either field ---------------------------------------------------

mkdir "$work/planted"
readonly planted_from="$repo_root/packages/envoy-client/src/dispatch-commands/issues.ts"
[[ -r "$planted_from" ]] || fail R8 "read the file a reader is planted in" "$planted_from cannot be read"
cp "$planted_from" "$work/planted/issues.ts"
printf '\nexport const plantedReader = (issue: { plan_artifact_id: string }) => issue.plan_artifact_id;\n' >>"$work/planted/issues.ts"
planted="$(issue_relations_read_readers "$work/planted")"
[[ "$planted" == *plantedReader* ]] ||
  fail R8 "the search finds the reader planted in a scratch copy of $planted_from" "it finds '$planted'"
pass R8 "the search finds the reader planted in a scratch copy of $planted_from"
searched=()
for tree in packages/dispatch/web/src packages/envoy-client/src packages/daemon; do
  [[ -d "$repo_root/$tree" ]] || fail R8 "search $tree" "$repo_root/$tree is no directory"
  searched+=("$repo_root/$tree")
done
readers="$(issue_relations_read_readers "${searched[@]}")"
[[ -z "$readers" ]] ||
  fail R8 "no page, CLI or daemon reads verification_artifact_id or plan_artifact_id of the issue read" "$readers"
pass R8 "no file of packages/dispatch/web/src, packages/envoy-client/src or packages/daemon reads verification_artifact_id or plan_artifact_id of the issue read"

pass "$driver" "complete 8 sections"
