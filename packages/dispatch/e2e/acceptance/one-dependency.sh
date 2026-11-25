#!/usr/bin/env bash
# Check c-one-dependency (the issue tree's spec, requirements r-one-dependency and r-plan-size):
# waiting is one relation. An issue waiting on an issue and a plan node waiting on a plan node are
# both `needs` tuples of the store, each written by its one writer and read back through every
# projection of it, over rows the earlier release wrote too.
#
# It runs over infra_1253_upgrade, as the Verification preamble states it for a driver that reads
# rows an earlier release wrote:
#   setup    empties the database DATABASE_URL names (a database on this machine whose name begins
#            infra_1253_upgrade), boots the release before through lib-baseline.sh's baseline_start
#            (BASELINE_SHA, or BASELINE_BINARY), signs DEV_SIGNIN_LOGIN in, and creates PROJECT;
#   before   through that server's API writes issue A and issue B blocked by A, and captures B's
#            issue_links row and the issue read that release answers, then stops it;
#   upgrade  boots this head (the stamped envoy-dispatch on PATH, through lib-tree.sh's
#            tree_boot_head, held), which migrates those rows, and waits for the backfill
#            (tree_wait_backfill);
#   E        the issue_links row the earlier release wrote answers as a `needs` tuple from B to A,
#            and the row stands as it was captured;
#   seed     writes parent P, whose plan holds t-p1 (size 45m) and t-p2 (size 2h), its children C,
#            whose plan holds t-c1 (size 1h30m, needs P's t-p1) and t-c2 (no size), and S, a sibling
#            of C, and has C wait on S through PATCH /api/v1/issues/{key};
#   halves   both waits read back as `needs` tuples of the store, each from the node that waits to
#            the node it waits on; a read that finds either missing fails with both reads side by
#            side;
#   project  issue_links holds the issue pair as a row, the plan node's markdown holds its `needs`
#            attribute naming its target, and graph_edges names the issue wait `blocked_by`, as a
#            process of the release before reads it, and the plan wait `needs`; a graph_edges read
#            that names the issue wait otherwise fails naming the two issues and the kind it read;
#   refs     GET /api/v1/references?from=<C> answers the issue wait as `needs`, and so do
#            ?kind=needs and ?kind=blocked_by;
#   retract  PATCH C {needs: []} retracts the wait, and the next read, with no pass run, finds the
#            issue_links row gone and the tuple retracted;
#   cli      `dispatch issue-update --issue C --blocked-by S`, which sends needs and blocked_by
#            with the same targets, appends one `needs` tuple, and the row stands again;
#   reach    creates CORE2 (POST /api/v1/projects, default_assignee the signed-in person), and the
#            agent bearer, which names no project, writes a `needs` from S to an issue of CORE2,
#            which is taken (a refusal fails naming both issues and their projects); a set above
#            contracts.MaxIssueBlockers is 400 NEEDS_INPUT under needs and 400 BLOCKED_BY_INPUT under
#            blocked_by; needs and blocked_by naming the same targets append one tuple for each;
#            two lists that differ are 400, naming both; C waiting on its parent P is 409
#            DEPENDENCY_CYCLE; a plan task of C needing a plan task of an issue outside C's tree is
#            400 INVALID_MARKDOWN; and a plan task in a second document of C needing C's own t-c1
#            is taken, its `needs` tuple read back;
#   sizes    t-p1, t-p2 and t-c1 read back 45, 120 and 90 minutes through the node's attributes
#            (the tree read's plan view) and through `dispatch read --ref`, each with its text as
#            written; t-c2 holds no size at all; 2d, 1.5h, 0m, -1h and 10001m are each refused 400
#            INVALID_MARKDOWN, and 10000m is taken.
# The Go halves of the check (TestNeedsCrossesAProjectAndRefusesAProjectScopedCaller and
# TestTheDaemonReadsNeedsAndFallsBackToBlockedBy) are the check's own steps, not this driver's.
#
# Inputs: DATABASE_URL (infra_1253_upgrade, emptied here), DISPATCH_TOKEN (the agent bearer both
# servers accept, e2e-token), PROJECT (CORE), DEV_SIGNIN_LOGIN (optional, the person, `alice` by
# default), BASELINE_SHA or BASELINE_BINARY and LEGION_WORKSPACE as lib-baseline.sh reads them, and
# on PATH envoy-dispatch (this head, stamped), bun (the dispatch client), curl, jq, psql, go and
# git. Each unmet input is exit 2. Each passing check prints `PASS: <section> <what>`; the first
# failing one prints `FAIL: <section> <what>: <observed>` and exits 1. The last line is
# `PASS: one-dependency.sh complete 12 sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && DATABASE_URL=$INFRA_1253_UPGRADE_DATABASE_URL DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE [DEV_SIGNIN_LOGIN=alice] BASELINE_SHA=<commit> bash e2e/acceptance/one-dependency.sh
       (DATABASE_URL names infra_1253_upgrade on this machine, which this driver empties; the
       stamped envoy-dispatch of this head is on PATH)
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
require_commands bun curl envoy-dispatch jq psql
readonly driver=one-dependency.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
# Not readonly: one_dependency_issue points seed-corpus.sh's corpus_issue, which reads it, at the
# second project for one call.
project="$PROJECT"
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
readonly other_project="${PROJECT}2"
# The cap on an issue's waits, as the contracts generate it.
cap_line="$(grep -E '^const MaxIssueBlockers = [0-9]+$' \
  "$(dirname "${BASH_SOURCE[0]}")/../../../envoy/internal/contracts/generated.go")" || {
  printf '%s: no `const MaxIssueBlockers = <n>` in packages/envoy/internal/contracts/generated.go\n' "$driver" >&2
  exit 2
}
readonly max_blockers="${cap_line##* }"

# one_dependency_reset_database: drops the database DATABASE_URL names and creates it empty, so the
# release before's own Store.Migrate writes its schema, connecting to the `postgres` database of
# the same server with the same role. It drops only a database on this machine whose name begins
# infra_1253_upgrade, the database the upgrade drivers of this issue alone use, and refuses every
# other with status 2: never infra_1253, which `bun run dispatch:local` serves.
one_dependency_reset_database() {
  local scheme authority name query host
  [[ "$database_url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] || {
    printf '%s: DATABASE_URL is no postgres:// URL naming a database\n' "$driver" >&2
    exit 2
  }
  scheme="${BASH_REMATCH[1]}" authority="${BASH_REMATCH[3]}" name="${BASH_REMATCH[4]}" query="${BASH_REMATCH[5]}"
  host="${authority##*@}"
  if [[ "$host" == \[* ]]; then host="${host%%]*}]"; else host="${host%%:*}"; fi
  case "$host" in
    127.0.0.1 | localhost | '[::1]') ;;
    *)
      printf '%s: DATABASE_URL is on %s, not this machine; this driver empties only this pod'"'"'s own infra_1253_upgrade\n' "$driver" "$host" >&2
      exit 2
      ;;
  esac
  [[ "$name" =~ ^infra_1253_upgrade[a-z0-9_]*$ ]] || {
    printf '%s: DATABASE_URL names %s; this driver empties only a database whose name begins infra_1253_upgrade\n' "$driver" "$name" >&2
    exit 2
  }
  baseline_psql "${scheme}${authority}/postgres${query}" -q \
    -c "drop database if exists \"$name\" with (force)" -c "create database \"$name\"" >"$work/reset" 2>&1 ||
    fail setup "drop and create the database $name" "psql: $(<"$work/reset")"
  printf 'database %s dropped and created empty\n' "$name"
}

# one_dependency_issue SECTION TITLE [PARENT [BLOCKED_BY [PROJECT]]]: the agent creates an issue of
# PROJECT (by default the run's) holding a spec of one paragraph, under PARENT and blocked by
# BLOCKED_BY where given, through the member the release before reads (seed-corpus.sh's
# corpus_issue); sets created_key.
created_key=""
one_dependency_issue() {
  printf 'The issue "%s" waits or is waited on.\n' "$2" >"$work/spec.md"
  if [[ -n "${5:-}" ]]; then
    project="$5" corpus_issue "$1" "$2" "$work/spec.md" "${3:-}" "${4:-}"
  else
    corpus_issue "$1" "$2" "$work/spec.md" "${3:-}" "${4:-}"
  fi
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] ||
    fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
  created_key="$corpus_key"
}

# one_dependency_upload SECTION ISSUE NAME FILE [STATUS]: the agent uploads FILE as the document
# NAME of ISSUE and the answer is STATUS, 201 by default: then through seed-corpus.sh's
# corpus_upload, which names the token of a version it replaces, and it sets uploaded_id to the
# artifact's id; any other status is a refused first version.
uploaded_id=""
one_dependency_upload() {
  local section="$1" issue="$2" name="$3" file="$4" want="${5:-201}"
  if [[ "$want" == 201 ]]; then
    corpus_upload "$section" "$issue" "$name" "$file"
    uploaded_id="$corpus_doc"
    return 0
  fi
  jq -cn --arg n "$name" --rawfile c "$file" --argjson a "$(tree_actor)" '{name: $n, content: $c, actor: $a}' \
    >"$work/upload.json"
  tree_send POST "/api/v1/issues/$issue/artifacts" "@$work/upload.json"
  tree_want "$want" "$section" "upload $name to $issue"
}

# one_dependency_patch SECTION WHAT ISSUE BODY STATUS: the agent patches ISSUE with the members of
# BODY, a JSON object, and the answer is STATUS.
one_dependency_patch() {
  tree_send PATCH "/api/v1/issues/$3" "$(jq -cn --argjson b "$4" --argjson a "$(tree_actor)" '$b + {actor: $a}')"
  tree_want "$5" "$1" "$2"
}

# one_dependency_node SECTION REF: the store node the block reference REF names, as the reference
# read resolves it (GET /api/v1/references?to=, through moved_to, the issue room and the address);
# sets node_kind and node_id.
node_kind=""
node_id=""
one_dependency_node() {
  tree_send GET "/api/v1/references?to=$(q "$2")"
  tree_want 200 "$1" "read the node $2 names"
  tree_field "$1" "read the node $2 names" .node.kind
  node_kind="$tree_value"
  tree_field "$1" "read the node $2 names" .node.id
  node_id="$tree_value"
}

# one_dependency_needs SECTION FROM_KIND FROM_ID TO_KIND TO_ID: the needs tuples of the store from
# the one node to the other: whether the newest asserts the wait (needs_current, 1 or 0) and how many
# rows assert it (needs_rows), read with the database's clock (needs_at).
needs_current=""
needs_rows=""
needs_at=""
one_dependency_needs() {
  with_vars "$1" "read the needs tuples from $2 $3 to $4 $5" "
    select clock_timestamp(),
           coalesce((select asserted::int from graph_tuples
                     where from_kind = :'fk' and from_id = :'fi' and kind = 'needs' and to_kind = :'tk' and to_id = :'ti'
                     order by seq desc limit 1), 0),
           (select count(*) from graph_tuples
            where from_kind = :'fk' and from_id = :'fi' and kind = 'needs' and to_kind = :'tk' and to_id = :'ti' and asserted)" \
    "fk=$2" "fi=$3" "tk=$4" "ti=$5"
  IFS='|' read -r needs_at needs_current needs_rows <<<"$answer"
}

# one_dependency_waits SECTION FROM_KIND FROM_ID TO_KIND TO_ID: the newest needs tuple from the one
# node to the other asserts the wait.
one_dependency_waits() {
  one_dependency_needs "$@"
  [[ "$needs_current" == 1 ]] ||
    fail "$1" "a needs tuple runs from $2 $3 to $4 $5" \
      "at $needs_at the newest asserts none ($needs_rows rows assert it)"
  pass "$1" "a needs tuple runs from $2 $3 to $4 $5 ($needs_rows rows assert it)"
}

# one_dependency_edges SECTION FROM_KIND FROM_ID TO_KIND TO_ID: the kinds graph_edges answers between
# the two nodes, comma-separated, `(none)` for none, in edge_kinds.
edge_kinds=""
one_dependency_edges() {
  with_vars "$1" "read graph_edges from $2 $3 to $4 $5" "
    select coalesce(string_agg(kind, ',' order by kind), '(none)') from graph_edges
    where from_kind = :'fk' and from_id = :'fi' and to_kind = :'tk' and to_id = :'ti'" \
    "fk=$2" "fi=$3" "tk=$4" "ti=$5"
  edge_kinds="$answer"
}

# one_dependency_links SECTION ISSUE TARGET: the issue_links rows of ISSUE waiting on TARGET, in
# links_rows.
links_rows=""
one_dependency_links() {
  with_vars "$1" "read the issue_links rows of $2 on $3" "
    select count(*) from issue_links where issue_key = :'i' and kind = 'blocked_by' and target_key = :'t'" \
    "i=$2" "t=$3"
  links_rows="$answer"
}

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

# --- setup: the release before over an empty database ----------------------------------------------

section=setup
one_dependency_reset_database
baseline_start
# The bearer both servers accept, the run's DISPATCH_TOKEN; printed nowhere, inherited by no child.
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="one-dependency-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_cli_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The one dependency relation"

# --- before: the rows the earlier release writes ---------------------------------------------------

section=before
one_dependency_issue "$section" "The issue the release before's wait names"
before_target="$created_key"
one_dependency_issue "$section" "The issue the release before has wait" "" "$before_target"
before_waiter="$created_key"
readonly before_target before_waiter
query "$section" "capture the issue_links row the release before wrote" "
  select coalesce(string_agg(issue_key || ' ' || kind || ' ' || target_key, ';' order by target_key), '(none)')
  from issue_links where issue_key = '$before_waiter'"
readonly before_row="$answer"
[[ "$before_row" == "$before_waiter blocked_by $before_target" ]] ||
  fail "$section" "the release before writes one issue_links row for $before_waiter's wait" "it holds: $before_row"
pass "$section" "captured the issue_links row the release before wrote: $before_row"
tree_send GET "/api/v1/issues/$before_waiter"
tree_want 200 "$section" "read $before_waiter through the release before"
expect "$section" "the release before answers $before_waiter blocked by $before_target" \
  "[\"$before_target\"]" '.blocked_by'
baseline_stop_server
pass "$section" "stopped the release before"

# --- upgrade: this head over those rows -----------------------------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"

# --- E: the earlier release's wait is a needs tuple -------------------------------------------------

section=E
one_dependency_waits "$section" issue "$before_waiter" issue "$before_target"
one_dependency_links "$section" "$before_waiter" "$before_target"
[[ "$links_rows" == 1 ]] ||
  fail "$section" "the issue_links row the release before wrote stands after the upgrade" \
    "$links_rows rows of $before_waiter on $before_target, captured: $before_row"
pass "$section" "the issue_links row $before_row the release before wrote stands, and answers as a needs tuple"
tree_send GET "/api/v1/issues/$before_waiter"
tree_want 200 "$section" "read $before_waiter through this head"
expect "$section" "this head answers $before_waiter's wait as needs and as blocked_by" \
  "$(jq -cn --arg t "$before_target" '[[$t], [$t]]')" '[.needs, .blocked_by]'

# --- seed: the parent, its children, their plans and the issue wait --------------------------------

section=seed
one_dependency_issue "$section" "The parent whose plan the child's plan waits on"
parent="$created_key"
one_dependency_issue "$section" "The child that waits" "$parent"
child="$created_key"
one_dependency_issue "$section" "The sibling the child waits on" "$parent"
sibling="$created_key"
readonly parent child sibling
readonly p1_ref="dispatch://$parent/artifact/plan-md#b-t-p1" p2_ref="dispatch://$parent/artifact/plan-md#b-t-p2"
readonly c1_ref="dispatch://$child/artifact/plan-md#b-t-c1" c2_ref="dispatch://$child/artifact/plan-md#b-t-c2"
readonly p1_text="Write the parser the child reads." p2_text="Ship the parser behind its flag."
readonly c1_text="Read what the parser writes." c2_text="Report what the reader found."
{
  printf 'How the parent is built.\n\n'
  printf ':::plan_task{#t-p1 size="45m"}\n%s\n:::\n\n' "$p1_text"
  printf ':::plan_task{#t-p2 size="2h"}\n%s\n:::\n' "$p2_text"
} >"$work/parent-plan.md"
one_dependency_upload "$section" "$parent" plan.md "$work/parent-plan.md"
pass "$section" "uploaded $parent's plan of two nodes, t-p1 and t-p2"
{
  printf 'How the child is built.\n\n'
  printf ':::plan_task{#t-c1 size="1h30m" needs="%s"}\n%s\n:::\n\n' "$p1_ref" "$c1_text"
  printf ':::plan_task{#t-c2}\n%s\n:::\n' "$c2_text"
} >"$work/child-plan.md"
# The cross-issue needs from the child's plan node to the parent's is taken (R10).
one_dependency_upload "$section" "$child" plan.md "$work/child-plan.md"
readonly child_plan="$uploaded_id"
pass "$section" "uploaded $child's plan of two nodes, t-c1 needing $p1_ref across the two issues"
one_dependency_patch "$section" "$child waits on its sibling $sibling through the issue route" "$child" \
  "$(jq -cn --arg s "$sibling" '{needs: [$s]}')" 200
pass "$section" "$child waits on $sibling through PATCH /api/v1/issues/$child"

# --- halves: both waits are needs tuples -----------------------------------------------------------

section=halves
one_dependency_node "$section" "$c1_ref"
c1_kind="$node_kind" c1_id="$node_id"
one_dependency_node "$section" "$p1_ref"
p1_kind="$node_kind" p1_id="$node_id"
readonly c1_kind c1_id p1_kind p1_id
one_dependency_needs "$section" issue "$child" issue "$sibling"
issue_read="issue wait $child -> $sibling: newest asserts $needs_current, $needs_rows rows assert it, at $needs_at"
issue_current="$needs_current"
one_dependency_needs "$section" "$c1_kind" "$c1_id" "$p1_kind" "$p1_id"
plan_read="plan wait $c1_kind $c1_id -> $p1_kind $p1_id: newest asserts $needs_current, $needs_rows rows assert it, at $needs_at"
[[ "$issue_current" == 1 && "$needs_current" == 1 ]] ||
  fail "$section" "both waits are needs tuples of the store" "$issue_read | $plan_read"
pass "$section" "both waits are needs tuples: $issue_read | $plan_read"

# --- project: the projections of the one relation ---------------------------------------------------

section=project
one_dependency_links "$section" "$child" "$sibling"
[[ "$links_rows" == 1 ]] ||
  fail "$section" "issue_links holds the pair $child, $sibling" "it holds $links_rows rows of $child on $sibling"
pass "$section" "issue_links holds the pair $child, $sibling as one row"
tree_send GET "/api/v1/artifacts/$child_plan/text"
tree_want 200 "$section" "read $child's plan"
tree_field "$section" "read $child's plan" .markdown
# The opening line of t-c1, whatever order the renderer writes its attributes in.
opening="$(grep -m 1 -E '^:::plan_task\{#t-c1[ }]' <<<"$tree_value")" || opening=""
[[ "$opening" == *"needs=\"$p1_ref\""* ]] ||
  fail "$section" "the plan node t-c1's markdown holds its needs attribute naming $p1_ref" \
    "its opening line reads '${opening:-(none)}' in: $(head -c 600 <<<"$tree_value")"
pass "$section" "t-c1's markdown holds needs=\"$p1_ref\": $opening"
one_dependency_edges "$section" issue "$child" issue "$sibling"
[[ "$edge_kinds" == blocked_by ]] ||
  fail "$section" "graph_edges names the wait of $child on $sibling blocked_by, as the release before reads it" \
    "graph_edges answers $child -> $sibling as: $edge_kinds"
pass "$section" "graph_edges names the issue wait $child -> $sibling blocked_by"
one_dependency_edges "$section" "$c1_kind" "$c1_id" "$p1_kind" "$p1_id"
[[ ",$edge_kinds," == *,needs,* ]] ||
  fail "$section" "graph_edges names the plan wait needs" \
    "graph_edges answers $c1_kind $c1_id -> $p1_kind $p1_id as: $edge_kinds"
pass "$section" "graph_edges names the plan wait $c1_id -> $p1_id needs"

# --- refs: the references read answers the issue wait as needs --------------------------------------

section=refs
for filter in "" needs blocked_by; do
  tree_send GET "/api/v1/references?from=$(q "dispatch://$child")${filter:+&kind=$filter}"
  tree_want 200 "$section" "read the references from $child${filter:+ of kind $filter}"
  expect "$section" "GET /api/v1/references?from=dispatch://$child${filter:+&kind=$filter} answers the wait on $sibling as needs" \
    1 --arg s "$sibling" '[.edges[] | select(.kind == "needs" and .direction == "out" and .node.kind == "issue" and .node.id == $s)] | length'
done

# --- retract: the issue route retracts the wait, and the row goes with it --------------------------

section=retract
one_dependency_patch "$section" "$child retracts its wait through the issue route" "$child" '{"needs": []}' 200
one_dependency_links "$section" "$child" "$sibling"
[[ "$links_rows" == 0 ]] ||
  fail "$section" "the next read after the retraction finds the issue_links row gone" \
    "it finds $links_rows rows of $child on $sibling, with no pass run"
one_dependency_needs "$section" issue "$child" issue "$sibling"
[[ "$needs_current" == 0 ]] ||
  fail "$section" "the retraction retracts the needs tuple" "at $needs_at the newest still asserts it"
readonly retracted_rows="$needs_rows"
pass "$section" "the retraction leaves no issue_links row and a retracted tuple, with no pass run"

# --- cli: dispatch issue-update --blocked-by appends one needs tuple --------------------------------

section=cli
run "$section" "dispatch issue-update --issue $child --blocked-by $sibling" \
  issue-update --issue "$child" --blocked-by "$sibling"
one_dependency_needs "$section" issue "$child" issue "$sibling"
[[ "$needs_current" == 1 && "$needs_rows" == $((retracted_rows + 1)) ]] ||
  fail "$section" "dispatch issue-update --blocked-by appends one needs tuple" \
    "at $needs_at the newest asserts $needs_current and $needs_rows rows assert the wait, $retracted_rows before it: $(<"$work/out")"
one_dependency_links "$section" "$child" "$sibling"
[[ "$links_rows" == 1 ]] || fail "$section" "the CLI's wait stands as one issue_links row" "$links_rows rows"
pass "$section" "dispatch issue-update --blocked-by, sending needs and blocked_by alike, appended one needs tuple"

# --- reach: what each writer takes and refuses -----------------------------------------------------

section=reach
tree_create_project "$section" "$other_project" "A second project the wait reaches"
one_dependency_issue "$section" "An issue of the second project" "" "" "$other_project"
other="$created_key"
readonly other
tree_send PATCH "/api/v1/issues/$sibling" \
  "$(jq -cn --arg o "$other" --argjson a "$(tree_actor)" '{needs: [$o], actor: $a}')"
[[ "$status" == 200 ]] ||
  fail "$section" "the agent bearer, which names no project, writes a needs across projects" \
    "PATCH $sibling (project $project) needing $other (project $other_project) answered HTTP $status: $(body)"
one_dependency_waits "$section" issue "$sibling" issue "$other"
pass "$section" "the agent bearer's needs from $sibling ($project) to $other ($other_project) is taken"
too_many="$(jq -cn --arg o "$other" --argjson n "$((max_blockers + 1))" '[range($n) | $o]')"
for member in needs blocked_by; do
  code=NEEDS_INPUT
  [[ "$member" == needs ]] || code=BLOCKED_BY_INPUT
  one_dependency_patch "$section" "$((max_blockers + 1)) targets under $member are refused" "$sibling" \
    "$(jq -cn --arg m "$member" --argjson l "$too_many" '{($m): $l}')" 400
  expect "$section" "$((max_blockers + 1)) targets under $member, above contracts.MaxIssueBlockers ($max_blockers), are refused $code" \
    "\"$code\"" .code
done
one_dependency_issue "$section" "An issue that names both members" "$parent"
both="$created_key"
readonly both
one_dependency_patch "$section" "$both names needs and blocked_by with the same targets" "$both" \
  "$(jq -cn --arg s "$sibling" --arg o "$other" '{needs: [$s, $o], blocked_by: [$o, $s]}')" 200
for target in "$sibling" "$other"; do
  one_dependency_needs "$section" issue "$both" issue "$target"
  [[ "$needs_current" == 1 && "$needs_rows" == 1 ]] ||
    fail "$section" "needs and blocked_by naming the same targets append one needs tuple for $target" \
      "at $needs_at the newest asserts $needs_current and $needs_rows rows assert it"
done
pass "$section" "needs and blocked_by naming the same targets appended one needs tuple for each of $sibling and $other"
one_dependency_patch "$section" "needs and blocked_by naming different issues are refused" "$both" \
  "$(jq -cn --arg s "$sibling" --arg o "$other" '{needs: [$s], blocked_by: [$o]}')" 400
expect "$section" "the refusal of two lists that differ names both" true \
  --arg s "$sibling" --arg o "$other" '(.error | contains("needs [" + $s + "]")) and (.error | contains("blocked_by [" + $o + "]"))'
one_dependency_patch "$section" "$child waiting on its parent $parent is a cycle" "$child" \
  "$(jq -cn --arg p "$parent" '{needs: [$p]}')" 409
expect "$section" "$child waiting on its parent $parent through the relation and the parent chain is refused DEPENDENCY_CYCLE" \
  '"DEPENDENCY_CYCLE"' .code
one_dependency_issue "$section" "An issue outside the child's tree"
outside="$created_key"
readonly outside
printf ':::plan_task{#t-u1}\nA task no issue of the child'"'"'s tree holds.\n:::\n' >"$work/outside-plan.md"
one_dependency_upload "$section" "$outside" plan.md "$work/outside-plan.md"
printf ':::plan_task{#t-c9 needs="dispatch://%s/artifact/plan-md#b-t-u1"}\nWait on a task outside the tree.\n:::\n' \
  "$outside" >"$work/reach.md"
one_dependency_upload "$section" "$child" reach.md "$work/reach.md" 400
expect "$section" "a plan task of $child needing a plan task of $outside, outside its tree, is refused INVALID_MARKDOWN naming $outside" \
  true --arg o "$outside" '.code == "INVALID_MARKDOWN" and (.error | contains($o))'
printf ':::plan_task{#t-c3 needs="%s"}\nCheck the reader once it runs.\n:::\n' "$c1_ref" >"$work/steps.md"
one_dependency_upload "$section" "$child" steps.md "$work/steps.md"
pass "$section" "a plan task of $child's second document, steps.md, needing $child's own t-c1 is taken"
one_dependency_node "$section" "dispatch://$child/artifact/steps-md#b-t-c3"
one_dependency_waits "$section" "$node_kind" "$node_id" "$c1_kind" "$c1_id"

# --- sizes: a plan node's size in minutes -----------------------------------------------------------

section=sizes
for row in "$parent t-p1 $p1_ref 45" "$parent t-p2 $p2_ref 120" "$child t-c1 $c1_ref 90" "$child t-c2 $c2_ref -"; do
  read -r issue block ref minutes <<<"$row"
  case "$block" in
    t-p1) words="$p1_text" ;;
    t-p2) words="$p2_text" ;;
    t-c1) words="$c1_text" ;;
    *) words="$c2_text" ;;
  esac
  tree_read agent "$issue" plan
  tree_want 200 "$section" "read $issue's plan view"
  jq -e --arg b "$block" '[.nodes[] | select(.block_id == $b)] | length == 1' "$work/body" >/dev/null ||
    fail "$section" "$issue's plan view holds $block once" "$(body)"
  expect "$section" "$block's text reads as its author wrote it" "$(jq -cn --arg w "$words" '$w')" \
    --arg b "$block" '.nodes[] | select(.block_id == $b) | .text'
  if [[ "$minutes" == - ]]; then
    expect "$section" "$block holds no size at all, absent rather than present and empty" false \
      --arg b "$block" '.nodes[] | select(.block_id == $b) | .attributes | has("size")'
  else
    expect "$section" "$block's size reads $minutes minutes through the node's attributes" "\"$minutes\"" \
      --arg b "$block" '.nodes[] | select(.block_id == $b) | .attributes.size'
  fi
  run "$section" "dispatch read --ref $ref" read --ref "$ref"
  grep -qF "$words" "$work/out" ||
    fail "$section" "dispatch read --ref $ref prints $block's text" "$(<"$work/out")"
  if [[ "$minutes" == - ]]; then
    ! grep -qiE '(^|[^a-z_])size([^a-z_]|$)' "$work/out" ||
      fail "$section" "dispatch read --ref $ref prints no size for $block" "$(<"$work/out")"
  else
    grep -qiE "(^|[^a-z_])size([^a-z_0-9]+)$minutes([^0-9]|\$)" "$work/out" ||
      fail "$section" "dispatch read --ref $ref prints $block's size, $minutes minutes" "$(<"$work/out")"
  fi
  pass "$section" "dispatch read --ref $ref prints $block's text and its size ($minutes minutes, - for none)"
done
one_dependency_issue "$section" "The issue whose plan task sizes are probed"
probe="$created_key"
readonly probe
for size in 2d 1.5h 0m -1h 10001m 10000m; do
  printf ':::plan_task{#t-size size="%s"}\nA task of the size %s.\n:::\n' "$size" "$size" >"$work/sizes.md"
  if [[ "$size" == 10000m ]]; then
    one_dependency_upload "$section" "$probe" sizes.md "$work/sizes.md"
    pass "$section" "a plan task of size $size is taken"
  else
    one_dependency_upload "$section" "$probe" sizes.md "$work/sizes.md" 400
    expect "$section" "a plan task of size $size is refused INVALID_MARKDOWN" '"INVALID_MARKDOWN"' .code
  fi
done

printf 'PASS: %s complete 12 sections\n' "$driver"
