#!/usr/bin/env bash
# The check c-shared-id-census: the instance-wide count of block ids that stand in two or more of
# the three documents of one issue that move into its room, published before the first move, and
# the move that makes each such id unique (spec requirement r-move-safe).
#
# It empties the database DATABASE_URL names (infra_1253_upgrade on this machine, dropped and
# created again), boots the release before this branch on it through lib-baseline.sh (BASELINE_SHA
# or BASELINE_BINARY), and writes through that server's API two issues, each carrying a spec, a
# verification.md and a plan.md that hold the shapes the tree census reads: the collision issue,
# whose verification.md holds a check and whose plan.md holds a plan task both under the block id
# r1 (address #b-r1), and a second issue with no id of its own written twice. It stops that server,
# boots this head over the same database with the hold of R6's step, and waits for the backfill.
#
# Then, after the upgrade and before the conversion, it takes the count twice, printing each with
# the database's time: once from the stored rows, keyed as document_blocks keys them on
# (artifact_id, block_id), over the spec, the verification-md and the plan-md of every issue of the
# instance, and once from the blocks the server reads back, GET /api/v1/artifacts/{id}/blocks of
# each of those documents. It prints each issue and each id behind a count above zero, with the
# documents that hold it, and the source of every row it counted: the version that last changed
# the block, which the earlier release wrote before this head booted and which holds no
# version_renders row of this head's writer. The two counts must name the same ids of the same
# issues; the published count must hold the collision issue and r1; and a count taken over no issue
# is refused. A count above zero is lawful stored data and fails nothing. It runs the .census.sql
# of the migration that makes block ids unique within an issue (20261117045607_issue_tree_rooms),
# the single select the repository runs before a deploy, and reads zero.
#
# It then waits for this head's build announcement, converts with `envoy-dispatch convert-issues
# --project PROJECT`, and reads every block id of the collision issue's tree once, through every
# page of GET /api/v1/issues/{key}/tree: no id stands on two nodes (where one does, it prints the
# issue, the id and both nodes, and fails), one r1 keeps its id, the other holds a fresh id, and
# the old address of each, the verification-md's and the plan-md's, resolves through its moved_to
# tuple (one asserted tuple from each, which moved_to_reach and GET /api/v1/references?to= follow)
# to a node the tree read holds.
#
# Inputs: DATABASE_URL (infra_1253_upgrade on this machine), DISPATCH_TOKEN (e2e-token), PROJECT
# (CORE), DEV_SIGNIN_LOGIN (optional: the person the project's create names as its default assignee,
# alice where unset, as the Verification preamble signs her in), BASELINE_SHA or BASELINE_BINARY,
# and `envoy-dispatch` on PATH, built stamped with this head's commit.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. The last line is `PASS: issue-tree-census.sh complete <N> sections`. Every
# check is `cmd || fail …`, never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1253_upgrade DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         [DEV_SIGNIN_LOGIN=alice] BASELINE_SHA=<the commit of the release this branch lands after> \
         bash e2e/acceptance/issue-tree-census.sh
       (run from packages/dispatch, with envoy-dispatch on PATH built stamped with this head's
       commit; DATABASE_URL names infra_1253_upgrade on this machine, which this run drops and
       creates again)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL DISPATCH_TOKEN PROJECT
require_commands curl date diff jq psql sort
readonly driver=issue-tree-census.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
session="issue-tree-census-$(date +%s)-$$"
readonly session
# The census of the migration that makes block ids unique within an issue, which the repository
# runs before a deploy.
census_sql="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../envoy/internal/dispatch/store/migrations" && pwd)/20261117045607_issue_tree_rooms.census.sql" || {
  printf '%s: cannot find packages/envoy/internal/dispatch/store/migrations from %s\n' "$driver" "$(dirname -- "${BASH_SOURCE[0]}")" >&2
  exit 2
}
readonly census_sql
[[ -r "$census_sql" ]] || {
  printf '%s: the census %s cannot be read\n' "$driver" "$census_sql" >&2
  exit 2
}
# The three documents of an issue that move into its room: the spec, and the documents a role
# predicate names verification-md and plan-md (documentRolePredicate in api/artifacts.go).
readonly role_documents_sql="select a.id, a.issue_key, a.slug, a.ref_key from artifacts a
  where a.issue_key is not null and a.kind = 'doc' and a.retired_at is null
    and (a.is_primary or a.role in ('verification', 'plan')
         or (a.role is null and a.slug in ('verification-md', 'plan-md')))"

work_dir
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  tree_stop_head
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# issue_tree_census_empty_database: drops and creates the database database_url names, so the
# earlier release's Store.Migrate writes its schema and the corpus is its only content. It takes
# only infra_1253_upgrade on this machine, the database of this issue's upgrade drivers, and
# refuses every other with status 2.
issue_tree_census_empty_database() {
  local scheme="" authority="" name="" query=""
  # The URL's parts are taken before loopback_database_url runs, whose own match replaces
  # BASH_REMATCH.
  if [[ "$database_url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]]; then
    scheme="${BASH_REMATCH[1]}" authority="${BASH_REMATCH[3]}" name="${BASH_REMATCH[4]}" query="${BASH_REMATCH[5]}"
  fi
  if [[ -z "$name" ]] || ! loopback_database_url "$database_url"; then
    printf '%s: DATABASE_URL names no database on this machine; the run drops and creates infra_1253_upgrade alone\n' "$driver" >&2
    exit 2
  fi
  [[ "$name" == infra_1253_upgrade ]] || {
    printf '%s: DATABASE_URL names the database %s; the run drops and creates infra_1253_upgrade alone\n' "$driver" "$name" >&2
    exit 2
  }
  baseline_psql "${scheme}${authority}/postgres${query}" -q \
    -c "drop database if exists \"$name\" with (force)" -c "create database \"$name\"" \
    >"$work/empty-database" 2>&1 || fail setup "drop and create the database $name" "psql: $(<"$work/empty-database")"
  printf '%s: the database %s was dropped and created empty\n' "$driver" "$name"
}

# issue_tree_census_tree SECTION FILE ISSUE: the agent's tree read of ISSUE with no view (lib-tree.sh's
# tree_read), every page of it followed through its cursor, as one answer in FILE: the first page's
# members and every page's nodes and tuples.
issue_tree_census_tree() {
  local section="$1" file="$2" issue="$3" cursor="" pages=0
  : >"$work/pages"
  while true; do
    tree_read agent "$issue" "" "" "$cursor"
    tree_want 200 "$section" "read the tree of $issue"
    cat "$work/body" >>"$work/pages"
    pages=$((pages + 1))
    cursor="$(jq -r '.cursor // ""' "$work/body")"
    [[ -n "$cursor" ]] || break
    ((pages < 500)) || fail "$section" "read the tree of $issue" "its cursor did not end after $pages pages"
  done
  jq -s '.[0] + {nodes: [.[].nodes[]?], tuples: [.[].tuples[]?]}' "$work/pages" >"$file" ||
    fail "$section" "read the tree of $issue" "the pages do not join: $(head -c 600 "$work/pages")"
  printf 'read the tree of %s: %s pages, %s nodes\n' "$issue" "$pages" "$(jq '.nodes | length' "$file")"
}

# issue_tree_census_count NAME VALUE AT: prints one count with the database time it was taken at.
issue_tree_census_count() { printf 'count %s=%s at %s\n' "$1" "$2" "$3"; }

# --- setup and the corpus, through the earlier release ----------------------------------------------

issue_tree_census_empty_database
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The block ids the census reads"

section=corpus
# issue_tree_census_issue TITLE PREFIX VERIFICATION_ID PLAN_ID: an issue whose spec, verification.md
# and plan.md hold the shapes, with a requirement r-PREFIX, a check VERIFICATION_ID serving it and a
# plan task PLAN_ID serving it; sets corpus_key, and issue_tree_census_verification and
# issue_tree_census_plan to the ids of its verification.md and its plan.md.
issue_tree_census_verification="" issue_tree_census_plan=""
issue_tree_census_issue() {
  local title="$1" prefix="$2" check="$3" task="$4" key
  {
    printf 'The %s issue of the census.\n\n:::requirement{#r-%s}\nThe %s issue holds one requirement.\n:::\n\n' "$prefix" "$prefix" "$prefix"
    corpus_shapes spec "$prefix-spec"
  } >"$work/$prefix-spec.md"
  corpus_issue "$section" "$title" "$work/$prefix-spec.md"
  key="$corpus_key"
  [[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$section" "create the issue \"$title\"" "the key $key is no issue key"
  {
    printf 'How the %s issue is proved.\n\n:::check{#%s serves="dispatch://%s/spec#b-r-%s" surface="cli" run="bash e2e/acceptance/issue-tree-census.sh" negative_control="Take the fresh id out and two nodes share one id."}\nThe %s issue is proved.\n:::\n\n' \
      "$prefix" "$check" "$key" "$prefix" "$prefix"
    corpus_shapes verification.md "$prefix-verification"
  } >"$work/$prefix-verification.md"
  corpus_upload "$section" "$key" verification.md "$work/$prefix-verification.md"
  issue_tree_census_verification="$corpus_doc"
  corpus_revise "$section" "$corpus_doc" verification.md 2
  {
    printf 'How the %s issue is built.\n\n:::plan_task{#%s serves="dispatch://%s/spec#b-r-%s"}\nBuild the %s issue.\n:::\n\n' \
      "$prefix" "$task" "$key" "$prefix" "$prefix"
    corpus_shapes plan.md "$prefix-plan"
  } >"$work/$prefix-plan.md"
  corpus_upload "$section" "$key" plan.md "$work/$prefix-plan.md"
  issue_tree_census_plan="$corpus_doc"
  corpus_revise "$section" "$corpus_doc" plan.md 2
  corpus_key="$key"
}
issue_tree_census_issue "An issue whose verification.md and plan.md share the block id r1" collision r1 r1
readonly collision="$corpus_key" collision_verification="$issue_tree_census_verification" collision_plan="$issue_tree_census_plan"
pass "$section" "wrote $collision, whose verification.md holds the check r1 and whose plan.md holds the plan task r1"
issue_tree_census_issue "An issue whose documents share no id of their own" clean c-clean t-clean
readonly clean="$corpus_key"
pass "$section" "wrote $clean, whose check and plan task hold ids of their own"
corpus_wait_settled "$section"
baseline_stop_server

# --- upgrade --------------------------------------------------------------------------------------

section=upgrade
query "$section" "read the time before this head boots" "select clock_timestamp()"
readonly booted_after="$answer"
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"

# --- census: the count, from the stored rows and from the blocks the server reads back ------------

section=census
query "$section" "count the issues whose documents the census reads" \
  "select clock_timestamp(), count(distinct issue_key) from ($role_documents_sql) d"
IFS='|' read -r at issues <<<"$answer"
issue_tree_census_count census_issues "$issues" "$at"
((issues > 0)) || fail "$section" "the census reads at least one issue" "it read $issues issues: a count taken over no issue is refused"
readonly shared_sql="with role_documents as ($role_documents_sql)
  select d.issue_key, b.block_id, string_agg(d.slug, ',' order by d.slug) as documents
  from document_blocks b join role_documents d on d.id = b.artifact_id
  group by d.issue_key, b.block_id
  having count(distinct b.artifact_id) >= 2"
query "$section" "count the block ids that stand in two or more of the three documents of one issue, from document_blocks" \
  "select clock_timestamp(), count(*) from ($shared_sql) shared"
IFS='|' read -r at stored_count <<<"$answer"
query "$section" "list the block ids that stand in two or more of the three documents of one issue, from document_blocks" \
  "select coalesce(string_agg(issue_key || ' ' || block_id || ' ' || documents, E'\n' order by issue_key, block_id), '') from ($shared_sql) shared"
printf '%s\n' "$answer" | sed '/^$/d' | LC_ALL=C sort >"$work/shared.stored"
issue_tree_census_count shared_block_ids_stored "$stored_count" "$at"
while IFS= read -r line; do
  printf 'shared id (stored rows): issue %s block %s (#b-%s) in %s\n' "${line%% *}" "$(cut -d' ' -f2 <<<"$line")" \
    "$(cut -d' ' -f2 <<<"$line")" "$(cut -d' ' -f3 <<<"$line")"
done <"$work/shared.stored"
grep -qx "$collision r1 [^ ]*" "$work/shared.stored" ||
  fail "$section" "the published count holds $collision and b-r1" "the stored rows count $stored_count ids: $(tr '\n' ';' <"$work/shared.stored")"
pass "$section" "the published count, from the stored rows, holds $collision and b-r1 among its $stored_count ids"
# The source of every row it counted: the version that last changed the block.
query "$section" "read the source of every row the census counted" "
  with role_documents as ($role_documents_sql),
  shared as (
    select d.issue_key, b.block_id from document_blocks b join role_documents d on d.id = b.artifact_id
    group by d.issue_key, b.block_id having count(distinct b.artifact_id) >= 2
  )
  select coalesce(string_agg(
    d.issue_key || ' ' || b.block_id || ' ' || d.slug || ' v' || b.changed_version || ' written ' ||
      coalesce(v.created_at::text, 'by no version') ||
      case when v.created_at is null then ' NOT-THE-EARLIER-RELEASE'
           when v.created_at >= '$booted_after'::timestamptz then ' after this head booted NOT-THE-EARLIER-RELEASE'
           else ' by the earlier release, before this head booted' end ||
      case when r.artifact_id is null then '' else ' with a version_renders row NOT-THE-EARLIER-RELEASE' end,
    E'\n' order by d.issue_key, b.block_id, d.slug), '')
  from shared s
  join role_documents d on d.issue_key = s.issue_key
  join document_blocks b on b.artifact_id = d.id and b.block_id = s.block_id
  left join artifact_versions v on v.artifact_id = b.artifact_id and v.number = b.changed_version
  left join version_renders r on r.artifact_id = b.artifact_id and r.number = b.changed_version"
printf '%s\n' "$answer" | sed '/^$/d; s/^/source: /'
[[ "$answer" != *NOT-THE-EARLIER-RELEASE* ]] ||
  fail "$section" "every row the census counted is one the earlier release wrote" \
    "$(grep 'NOT-THE-EARLIER-RELEASE' <<<"$answer" | head -n 5 | tr '\n' ';')"
pass "$section" "every row the census counted is one the earlier release wrote, before this head booted at $booted_after"
# The same count from the blocks the server reads back.
query "$section" "list the documents the census reads" \
  "select coalesce(string_agg(issue_key || ' ' || slug || ' ' || id, E'\n' order by issue_key, slug), '') from ($role_documents_sql) d"
: >"$work/blocks.read"
documents=0
while read -r issue slug id; do
  [[ -n "$id" ]] || continue
  tree_send GET "/api/v1/artifacts/$id/blocks"
  tree_want 200 "$section" "read the blocks of $slug of $issue"
  jq -r --arg i "$issue" --arg s "$slug" '.[] | "\($i) \(.id) \($s)"' "$work/body" >>"$work/blocks.read" ||
    fail "$section" "read the blocks of $slug of $issue" "no list of blocks: $(body)"
  documents=$((documents + 1))
done <<<"$answer"
LC_ALL=C sort -u "$work/blocks.read" |
  jq -R -s -r 'split("\n") | map(select(. != "") | split(" ")) | group_by(.[0] + " " + .[1])
    | map(select((map(.[2]) | unique | length) >= 2) | "\(.[0][0]) \(.[0][1]) \(map(.[2]) | unique | join(","))") | .[]' |
  LC_ALL=C sort >"$work/shared.read"
query "$section" "read the time of the count from the blocks read" "select clock_timestamp()"
issue_tree_census_count shared_block_ids_read_back "$(wc -l <"$work/shared.read")" "$answer"
printf 'the blocks read of %s documents\n' "$documents"
differs="$(diff "$work/shared.stored" "$work/shared.read")" || true
[[ -z "$differs" ]] ||
  fail "$section" "the count from the stored rows and the count from the blocks read agree exactly" \
    "they differ (< stored rows, > blocks read): $(tr '\n' ';' <<<"$differs")"
pass "$section" "the count from the stored rows and the count from the blocks the server reads back agree: $stored_count ids"
query "$section" "run the census of 20261117045607_issue_tree_rooms" "$(<"$census_sql")"
census_answer="$answer"
query "$section" "read the time of the migration census" "select clock_timestamp()"
issue_tree_census_count block_ids_twice_under_a_tree "$census_answer" "$answer"
[[ "$census_answer" == 0 ]] ||
  fail "$section" "the migration census reads zero block ids twice under one tree artifact" "it reads $census_answer"
pass "$section" "the migration census reads zero block ids twice under one tree artifact"
# The two old addresses of r1, each as a node id (`<ref_key>#r1`, text.TargetID) and as a citation
# writes it.
query "$section" "read the old addresses of r1" \
  "select (select ref_key from artifacts where id = '$collision_verification') || '#r1 ' ||
          (select ref_key from artifacts where id = '$collision_plan') || '#r1'"
read -r verification_r1 plan_r1 <<<"$answer"
readonly old_addresses=("$verification_r1 dispatch://$collision/artifact/verification-md#b-r1"
  "$plan_r1 dispatch://$collision/artifact/plan-md#b-r1")

# --- convert --------------------------------------------------------------------------------------

section=convert
tree_wait_announcement "$section"
tree_convert "$section"
sed 's/^/  /' "$work/out"

# --- ids: every block id of the issue's tree once ---------------------------------------------------

section=ids
issue_tree_census_tree "$section" "$work/collision.json" "$collision"
cp "$work/collision.json" "$work/body"
expect "$section" "$collision reads converted: true" true '.converted'
twice="$(jq -r --arg k "$collision" '[.nodes[] | select(.kind == "block" and ((.issue // $k) == $k))]
  | group_by(.block_id) | map(select(length > 1) | "issue \($k) block id \(.[0].block_id) (#b-\(.[0].block_id)) stands on \(length) nodes: \(map(.id) | join(", "))")
  | join("; ")' "$work/collision.json")"
[[ -z "$twice" ]] || fail "$section" "every block id of the tree of $collision stands on one node" "$twice"
blocks="$(jq --arg k "$collision" '[.nodes[] | select(.kind == "block" and ((.issue // $k) == $k))] | length' "$work/collision.json")"
pass "$section" "each of the $blocks block ids of the tree of $collision stands on one node"
expect "$section" "one node of the tree of $collision keeps the id r1" 1 --arg k "$collision" \
  '[.nodes[] | select(.kind == "block" and ((.issue // $k) == $k) and .block_id == "r1")] | length'
reached=()
for pair in "${old_addresses[@]}"; do
  read -r old old_address <<<"$pair"
  query "$section" "read the moved_to tuples from $old" "
    select count(*) || ' ' || coalesce((select t.to_kind || ' ' || t.to_id from graph_tuples t
      where t.from_kind = 'block' and t.from_id = '$old' and t.kind = 'moved_to' and t.asserted
      order by t.seq desc limit 1), 'none none')
    from graph_tuples where from_kind = 'block' and from_id = '$old' and kind = 'moved_to' and asserted"
  read -r tuples to_kind to_id <<<"$answer"
  [[ "$tuples" == 1 && "$to_kind" == block ]] ||
    fail "$section" "one moved_to tuple records the move of $old" "graph_tuples holds $tuples asserted from it, the newest to $to_kind $to_id"
  query "$section" "follow the moves of $old" "select coalesce((select kind || ' ' || id from moved_to_reach('$old')), 'none')"
  [[ "$answer" == "block $to_id" ]] ||
    fail "$section" "the old address $old resolves through its moved_to tuple" "moved_to_reach reaches $answer, the tuple names block $to_id"
  jq -e --arg id "$to_id" '[.nodes[] | select(.kind == "block" and .id == $id)] | length == 1' "$work/collision.json" >/dev/null ||
    fail "$section" "the old address $old resolves to a node of the tree of $collision" "its moved_to names $to_id, which the tree read holds no node of"
  tree_send GET "/api/v1/references?to=$(q "$old_address")"
  tree_want 200 "$section" "resolve the old address $old_address"
  expect "$section" "the old address $old_address resolves through its moved_to tuple to $to_id" \
    "$(jq -cn --arg v "$to_id" '$v')" '.node.id'
  reached+=("$to_id")
done
[[ "${reached[0]}" != "${reached[1]}" ]] ||
  fail "$section" "the two r1 reach two nodes" "both reach ${reached[0]}"
kept=0
for id in "${reached[@]}"; do
  [[ "$id" != *"#r1" ]] || kept=$((kept + 1))
done
[[ "$kept" == 1 ]] ||
  fail "$section" "one r1 keeps its id and the other holds a fresh one" "the two reach ${reached[*]}"
pass "$section" "one r1 keeps its id and the other holds a fresh one: the two reach ${reached[*]}"

printf 'PASS: %s complete 6 sections\n' "$driver"
