#!/usr/bin/env bash
# Drives check c-tree-page of the issue tree's Verification document: the tree read of an issue
# the earlier release wrote, once this head has converted it, and the spec query it answers from
# the same route. It serves the spec's requirements r-queries, r-one-tree and r-one-room, and the
# root's r-document-tree.
#
# It runs over infra_1253_upgrade, the database the Verification preamble gives the drivers that
# read rows an earlier release wrote. It refuses to run, with a FAIL line, where the database
# answers `select 'a' < 'B'` false: such a database's own collation orders as bytes do, so a read
# that ordered ranks by it could not be told from one that orders them byte for byte. It then
# empties that database and boots the base-commit server on it through lib-baseline.sh
# (BASELINE_SHA or BASELINE_BINARY), signs the person in, creates the project PROJECT and, through
# that server's API, writes one issue with no parent that is not done: a spec of two requirement
# blocks, a Verification document of two check blocks, each serving one of them, and a plan of one
# plan node serving the first. Each document holds a prose paragraph beside its typed blocks. The
# requirements and the checks are written in the reverse of their ids' order, so the rank order
# the walk answers differs from the order of their node ids. It records each typed block's id,
# text and `serves` targets as the earlier release's blocks read answers them, and stops that
# server.
#
# It boots this head's server over the same database (lib-tree.sh's tree_boot_head, with the hold
# settings of the preamble), waits for the backfill and for the newest build announcement to name
# this head's commit, and runs `envoy-dispatch convert-issues --project PROJECT`. Then, through
# GET /api/v1/issues/{key}/tree:
#   tree      one read answers converted true, each requirement, each check and the plan node once,
#             each with its id, its supertag, its parent and its rank; each node's parent is the
#             issue's node or another node of the answer and stands before it; siblings stand in
#             increasing rank, compared byte for byte, the unranked after them;
#   serves    the answer holds the serves tuples among those nodes, and the walk of them from each
#             requirement reaches the checks and the plan node whose serves names it, the set the
#             earlier release wrote;
#   again     a second read answers the same nodes in the same order;
#   spec      ?view=spec answers the two requirements, each the query's, in the order of the first
#             read, each with its parent's id, supertag and first line;
#   query     the issue read, GET /api/v1/issues/{key}, holds the spec query node, which states the
#             types it takes, those R14 gives the spec query (callout, criterion, requirement), and
#             the subtree it takes them from, the issue's tree node;
#   words     one word of a check and one of the plan node, both written before the conversion,
#             change through the issue's room, and the tree read answers both new words.
#
# Inputs: DATABASE_URL (infra_1253_upgrade, a loopback database whose name begins infra_1253_,
# which this driver drops and creates again), PROJECT (CORE), DISPATCH_TOKEN (e2e-token, the bearer
# both servers accept), BASELINE_SHA or BASELINE_BINARY (lib-baseline.sh's), DEV_SIGNIN_LOGIN (the
# person signed in before the project's create names them, `alice` where it is unset), and the
# stamped `envoy-dispatch` of this head on PATH. Each unmet input is exit 2.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: issue-tree-spec-query.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/infra_1253_upgrade?sslmode=disable \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE [DEV_SIGNIN_LOGIN=alice] \
         {BASELINE_SHA=<commit> | BASELINE_BINARY=<path>} bash e2e/acceptance/issue-tree-spec-query.sh
       (run from packages/dispatch, with this head's stamped envoy-dispatch on PATH; the driver
       drops and creates the database DATABASE_URL names, whose name begins infra_1253_)
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

readonly driver=issue-tree-spec-query.sh
require_env DATABASE_URL PROJECT
require_commands curl jq psql
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
loopback_database_url "$database_url" || {
  printf '%s: DATABASE_URL names no single loopback database: this driver drops and creates it\n' "$driver" >&2
  exit 2
}

# The types R14 gives the spec query, in the order its definition names them.
readonly spec_query_takes='["callout","criterion","requirement"]'
# The words the room's write changes, one in the check c-zulu and one in the plan node t-only.
readonly check_before="The tree read answers every requirement once." check_after="The tree read returns every requirement once."
readonly plan_before="Write the walk that answers every requirement once." plan_after="Build the walk that answers every requirement once."

work_dir
sections=0
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

# issue_tree_spec_query_section NAME: starts the section NAME, which the closing line counts.
issue_tree_spec_query_section() {
  sections=$((sections + 1))
  printf -- '--- section %s\n' "$1"
}

# issue_tree_spec_query_expect FILE SECTION WHAT WANT JQ-ARGS…: lib.sh's expect over the answer
# kept in FILE rather than over the last one, which it puts back as $work/body first.
issue_tree_spec_query_expect() {
  local file="$1"
  shift
  cp "$file" "$work/body"
  expect "$@"
}

# issue_tree_spec_query_empty_database: drops and creates again the database DATABASE_URL names,
# so the base release's own Store.Migrate writes its schema and this driver's issue is all it
# holds. It drops only a loopback database whose name begins infra_1253_, connecting to the
# `postgres` database of the same server, as seed-decisions.sh's seed_reset_database drops its own.
issue_tree_spec_query_empty_database() {
  local url="$database_url" scheme authority name query
  [[ "$url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] || {
    printf '%s: DATABASE_URL is no postgres:// URL naming a database\n' "$driver" >&2
    exit 2
  }
  scheme="${BASH_REMATCH[1]}" authority="${BASH_REMATCH[3]}" name="${BASH_REMATCH[4]}" query="${BASH_REMATCH[5]}"
  [[ "$name" =~ ^infra_1253_[a-z0-9_]+$ ]] || {
    printf '%s: DATABASE_URL names the database %s: this driver drops and creates only one whose name begins infra_1253_\n' "$driver" "$name" >&2
    exit 2
  }
  baseline_psql "${scheme}${authority}/postgres${query}" -q \
    -c "drop database if exists \"$name\" with (force)" -c "create database \"$name\"" >"$work/reset" 2>&1 ||
    fail setup "drop and create the database $name" "psql: $(<"$work/reset")"
  pass setup "dropped and created the database $name empty"
}

# issue_tree_spec_query_record SECTION ISSUE SLUG: the typed blocks of the document SLUG of ISSUE
# as the earlier release's blocks read answers them (GET .../artifacts/{slug}/text?blocks=true):
# each requirement, check and plan node's id, type, text - its markdown, the byte range the read
# gives it - and the block ids its serves names, appended to $work/written.json. The fixture's
# markdown is ASCII, so jq's slice by character is the slice by byte.
issue_tree_spec_query_record() {
  local section="$1" issue="$2" slug="$3"
  tree_send GET "/api/v1/issues/$issue/artifacts/$slug/text?blocks=true"
  tree_want 200 "$section" "the earlier release reads the blocks of $slug of $issue"
  jq -e '.blocks | type == "array"' "$work/body" >/dev/null ||
    fail "$section" "the earlier release reads the blocks of $slug of $issue" "no blocks: $(body)"
  jq -c --arg slug "$slug" '.markdown as $m | [.blocks[]
    | select(.type == "requirement" or .type == "check" or .type == "plan_task")
    | ($m[.from:.to]) as $text
    | {block_id: .id, type, slug: $slug, text: $text,
       serves: ((([$text | capture("serves=\"(?<s>[^\"]*)\"")][0].s) // "")
         | [splits(" +") | select(. != "") | sub("^.*#b-"; "")])}]' "$work/body" >"$work/recorded.json"
  jq -c -s 'add' "$work/written.json" "$work/recorded.json" >"$work/written.next"
  mv "$work/written.next" "$work/written.json"
  while IFS= read -r line; do
    printf 'written %s\n' "$line"
  done < <(jq -c '.[] | {block_id, type, serves, text}' "$work/recorded.json")
}

# issue_tree_spec_query_tree_checks SECTION FILE WHAT: the order rules of the tree read in FILE:
# each node of the tree but the issue's own stands under a parent that is the issue's node or a
# node of the answer, which stands before it; under each parent the ranked siblings stand in
# increasing rank, compared as jq compares strings, by code point, which is byte order for UTF-8,
# and the unranked after them. A failure names the parent and the two siblings in the order read.
issue_tree_spec_query_tree_checks() {
  local section="$1" file="$2" what="$3" broken
  broken="$(jq -r --arg key "$key" '
    .nodes as $nodes
    | ([$nodes | to_entries[] | {key: "\(.value.kind):\(.value.id)", value: .key}] | from_entries) as $at
    | ($nodes | to_entries
       | map(select(.value.role != "context" and .value.parent != null)
             | {i: .key, node: "\(.value.kind):\(.value.id)", parent: "\(.value.parent.kind):\(.value.parent.id)",
                rank: .value.rank})) as $placed
    | ([$placed[] | select(($at[.parent] // null) == null and .parent != "issue:\($key)")
        | "\(.node) stands under \(.parent), which is neither the issue node issue:\($key) nor a node of the answer"]
       + [$placed[] | select(($at[.parent] // null) != null and $at[.parent] > .i)
        | "\(.node) at \(.i) stands before its parent \(.parent) at \($at[.parent])"]
       + [$placed | group_by(.parent)[] | sort_by(.i) as $siblings
          | ($siblings | map(select(.rank != null))) as $ranked
          | ([range(0; ($ranked | length) - 1) as $j
              | select(($ranked[$j].rank < $ranked[$j + 1].rank) | not)
              | "under \($ranked[$j].parent) the siblings \($ranked[$j].node) (rank \($ranked[$j].rank)) and \($ranked[$j + 1].node) (rank \($ranked[$j + 1].rank)) stand in that order, out of rank order"]
             + [($siblings | map(select(.rank == null)) | first) as $loose
                | ($ranked | last) as $latest
                | select($loose != null and $latest != null and $loose.i < $latest.i)
                | "under \($latest.parent) the unranked sibling \($loose.node) stands before the ranked \($latest.node)"])[]])
    | .[]' "$file")" || fail "$section" "$what" "jq could not read the answer: $(head -c 600 "$file")"
  [[ -z "$broken" ]] || fail "$section" "$what" "${broken//$'\n'/; }"
  pass "$section" "$what"
}

# --- setup -------------------------------------------------------------------------------------

issue_tree_spec_query_section setup
issue_tree_spec_query_empty_database
query setup "read whether the database's own collation orders 'a' before 'B'" "select 'a' < 'B'"
[[ "$answer" == t ]] ||
  fail setup "the database's collation tells byte order from its own" \
    "select 'a' < 'B' reads '$answer': this database orders as bytes do, so a read that ordered ranks by its collation could not be told from one that orders them byte for byte; give the driver a database whose collation is linguistic"
pass setup "select 'a' < 'B' reads true: the database's collation is not byte order"

baseline_start
# The bearer both servers accept, printed nowhere, inherited by no child process.
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-tree-spec-query-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The issue tree's spec query"

# --- the earlier release writes the issue ----------------------------------------------------------

issue_tree_spec_query_section earlier
printf '[]\n' >"$work/written.json"
cat >"$work/spec.md" <<'SPEC'
The summary paragraph of the issue whose tree this check reads.

:::requirement{#r-zulu}
The tree holds every requirement once.
:::

:::requirement{#r-alpha}
The tree orders siblings by rank.
:::
SPEC
corpus_issue earlier "The tree of one issue, read as one" "$work/spec.md"
key="$corpus_key"
[[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail earlier "create the issue" "the key $key is no issue key"
readonly key
cat >"$work/verification.md" <<VERIFICATION
How the tree read is proved.

:::check{#c-zulu serves="dispatch://$key/spec#b-r-zulu" surface="api" run="bash e2e/acceptance/issue-tree-spec-query.sh" when="before_merge" negative_control="Answer the nodes in id order and the siblings come back out of rank order."}
$check_before
:::

:::check{#c-alpha serves="dispatch://$key/spec#b-r-alpha" surface="api" run="bash e2e/acceptance/issue-tree-spec-query.sh" when="before_merge" negative_control="Drop the rank from a node and the order check names it."}
The tree read answers siblings in rank order.
:::
VERIFICATION
corpus_upload earlier "$key" verification.md "$work/verification.md"
verification="$corpus_doc" verification_slug="$corpus_slug"
readonly verification verification_slug
cat >"$work/plan.md" <<PLAN
How the tree read is built.

:::plan_task{#t-only serves="dispatch://$key/spec#b-r-zulu"}
$plan_before
:::
PLAN
corpus_upload earlier "$key" plan.md "$work/plan.md"
plan="$corpus_doc" plan_slug="$corpus_slug"
readonly plan plan_slug
corpus_wait_settled earlier
pass earlier "the earlier release wrote $key: its spec, $verification_slug ($verification) and $plan_slug ($plan)"
issue_tree_spec_query_record earlier "$key" spec
issue_tree_spec_query_record earlier "$key" "$verification_slug"
issue_tree_spec_query_record earlier "$key" "$plan_slug"
issue_tree_spec_query_expect "$work/written.json" earlier \
  "the earlier release's blocks read answers the two requirements, the two checks and the plan node" \
  '[["c-alpha","check"],["c-zulu","check"],["r-alpha","requirement"],["r-zulu","requirement"],["t-only","plan_task"]]' \
  '[.[] | [.block_id, .type]] | sort'
issue_tree_spec_query_expect "$work/written.json" earlier "the earlier release wrote the serves the fixture names" \
  '{"c-alpha":["r-alpha"],"c-zulu":["r-zulu"],"t-only":["r-zulu"]}' \
  '[.[] | select(.type != "requirement") | {key: .block_id, value: .serves}] | from_entries'
baseline_stop_server
pass earlier "the earlier release's server stopped; its database stays"

# --- this head converts the issue -----------------------------------------------------------------

issue_tree_spec_query_section upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill upgrade
tree_wait_announcement upgrade
tree_convert upgrade
printf '%s\n' "$(tail -n 20 "$work/out")"

# --- the tree read ---------------------------------------------------------------------------------

issue_tree_spec_query_section tree
tree_read agent "$key"
tree_want 200 tree "read the tree of $key"
cp "$work/body" "$work/tree.json"
issue_tree_spec_query_expect "$work/tree.json" tree "$key reads converted true on one page" '[true,null]' '[.converted, .cursor]'
# Each typed block the earlier release wrote, answered once by its block id, with its id, its
# supertag (its type), its parent and its rank.
missing="$(jq -r --slurpfile written "$work/written.json" '
  . as $read | $written[0][] as $block
  | [$read.nodes[] | select(.block_id == $block.block_id and (.issue // "") == "")] as $held
  | if ($held | length) != 1 then "\($block.block_id) is answered \($held | length) times"
    elif ($held[0].id | type) != "string" or $held[0].id == "" then "\($block.block_id) carries no id"
    elif $held[0].supertag != $block.type then "\($block.block_id) carries the supertag \($held[0].supertag), want \($block.type)"
    elif $held[0].parent == null or ($held[0].parent.id // "") == "" then "\($block.block_id) carries no parent"
    elif ($held[0].rank | type) != "string" then "\($block.block_id) carries the rank \($held[0].rank | tojson), want one"
    else empty end' "$work/tree.json")"
[[ -z "$missing" ]] || fail tree "the read answers each requirement, check and the plan node once, with its id, supertag, parent and rank" "${missing//$'\n'/; }"
pass tree "the read answers each requirement, check and the plan node once, with its id, supertag, parent and rank"
while IFS= read -r line; do
  printf 'read %s\n' "$line"
done < <(jq -c --slurpfile written "$work/written.json" '($written[0] | map(.block_id)) as $ids
  | .nodes | to_entries[] | select((.value.block_id // "") as $b | $ids | index($b))
  | {at: .key, block_id: .value.block_id, id: .value.id, supertag: .value.supertag,
     parent: "\(.value.parent.kind):\(.value.parent.id)", rank: .value.rank}' "$work/tree.json")
issue_tree_spec_query_tree_checks tree "$work/tree.json" \
  "each node stands under the issue's node or a node of the answer, after it, and siblings stand in increasing rank, compared byte for byte"

# --- the serves tuples -----------------------------------------------------------------------------

issue_tree_spec_query_section serves
walked="$(jq -c '
  ([.nodes[] | {key: "\(.kind):\(.id)", value: (.block_id // "")}] | from_entries) as $block
  | .tuples as $tuples
  | [.nodes[] | select(.supertag == "requirement") | "\(.kind):\(.id)" as $requirement
     | {key: .block_id,
        value: ([$tuples[] | select(.kind == "serves" and "\(.to.kind):\(.to.id)" == $requirement)
                 | $block["\(.from.kind):\(.from.id)"] // "(a node outside the answer)"] | sort)}]
  | from_entries' "$work/tree.json")" ||
  fail serves "walk the serves tuples from each requirement" "jq could not read the answer: $(head -c 600 "$work/tree.json")"
wanted="$(jq -c '. as $all | [.[] | select(.type == "requirement") | .block_id as $r
  | {key: $r, value: ([$all[] | select(.serves | index($r)) | .block_id] | sort)}] | from_entries' "$work/written.json")"
printf 'serves walked from each requirement: %s\n' "$walked"
printf 'serves the earlier release wrote, by requirement: %s\n' "$wanted"
[[ "$(jq -cS . <<<"$walked")" == "$(jq -cS . <<<"$wanted")" ]] ||
  fail serves "the walk of the serves tuples from each requirement reaches the set the earlier release wrote" \
    "it reaches $walked, want $wanted"
pass serves "the walk of the serves tuples from each requirement reaches the set the earlier release wrote"

# --- a second read ---------------------------------------------------------------------------------

issue_tree_spec_query_section again
tree_read agent "$key"
tree_want 200 again "read the tree of $key a second time"
first="$(jq -c '[.nodes[] | "\(.kind):\(.id)"]' "$work/tree.json")"
second="$(jq -c '[.nodes[] | "\(.kind):\(.id)"]' "$work/body")"
[[ "$first" == "$second" ]] ||
  fail again "a second read answers the same nodes in the same order" "the first read $first, the second $second"
pass again "a second read answers the same $(jq 'length' <<<"$first") nodes in the same order"

# --- the spec view ---------------------------------------------------------------------------------

issue_tree_spec_query_section spec
tree_read agent "$key" spec
tree_want 200 spec "read ?view=spec of $key"
cp "$work/body" "$work/spec-view.json"
order="$(jq -c '[.nodes[] | select(.supertag == "requirement") | .block_id]' "$work/tree.json")"
issue_tree_spec_query_expect "$work/spec-view.json" spec \
  "?view=spec answers the two requirements, each the query's, in the order the first read gave them" \
  "$(jq -c '[.[] | [., "query"]]' <<<"$order")" '[.nodes[] | select(.supertag == "requirement") | [.block_id, .role]]'
issue_tree_spec_query_expect "$work/spec-view.json" spec \
  "each requirement of ?view=spec carries its parent's id, supertag and first line" '[]' \
  '[.nodes[] | select(.supertag == "requirement")
    | select(.parent == null or (.parent.id // "") == "" or (.parent.supertag // "") == "" or (.parent.first_line // "") == "")
    | {block_id, parent}]'

# --- the spec query node ---------------------------------------------------------------------------

issue_tree_spec_query_section query
tree_send GET "/api/v1/issues/$key"
tree_want 200 query "read the issue $key"
cp "$work/body" "$work/issue.json"
jq -e '([.queries[]? | select(.query == "spec")] + [.artifacts[]? | select(.primary == true)]) | length > 0' \
  "$work/issue.json" >/dev/null ||
  fail query "the issue read holds the spec query node" "it holds no query node and no primary document: $(head -c 600 "$work/issue.json")"
jq -c '([.queries[]? | select(.query == "spec")] + [.artifacts[]? | select(.primary == true)]) | first' \
  "$work/issue.json" >"$work/spec-query.json"
printf 'the spec query node as the issue read answers it: %s\n' "$(head -c 600 "$work/spec-query.json")"
issue_tree_spec_query_expect "$work/spec-query.json" query \
  "the spec query node states the types R14 gives the spec query and the subtree they come from, the issue's tree node" \
  "$(jq -cn --argjson t "$spec_query_takes" --arg s "issue:$key" '{subtree: $s, takes: $t}')" \
  '{takes: (.takes | if type == "string" then split(" ") else . end), subtree}'

# --- a write of the issue's room ---------------------------------------------------------------

issue_tree_spec_query_section words
corpus_edit words "change one word of the check c-zulu" "$verification" \
  "$(jq -cn --arg f "$check_before" --arg w "$check_after" '[{op: "replace", find: $f, with: $w}]')"
corpus_edit words "change one word of the plan node t-only" "$plan" \
  "$(jq -cn --arg f "$plan_before" --arg w "$plan_after" '[{op: "replace", find: $f, with: $w}]')"
tree_read agent "$key"
tree_want 200 words "read the tree of $key after the words changed"
expect words "the tree read answers the check's new word and the plan node's new word" '[true,true]' \
  --arg c "$check_after" --arg p "$plan_after" \
  '[([.nodes[] | select(.block_id == "c-zulu") | .text | contains($c)] | any),
    ([.nodes[] | select(.block_id == "t-only") | .text | contains($p)] | any)]'

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
