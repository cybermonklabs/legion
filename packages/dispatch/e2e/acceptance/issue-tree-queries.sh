#!/usr/bin/env bash
# Drives check c-queries of the issue tree's Verification document: the four queries of an issue -
# Spec, Verification, Plan and Decisions - and the tree read, each compared, on an issue the earlier
# release wrote and this head converted, with the read it replaces as the earlier release answered
# it. It serves the spec's requirements r-queries and r-result-node, and the root's
# r-verification-edges.
#
# It runs over infra_1253_upgrade, the database of the drivers that read rows an earlier release
# wrote. It empties that database and boots the base-commit server on it through lib-baseline.sh
# (BASELINE_SHA or BASELINE_BINARY), signs the person in, creates the project PROJECT and, through
# that server's API, writes one issue: a spec holding a memo callout, three requirements, an open
# ask, and three decisions - one marked replaced, one the person keeps through the Keep route
# (POST /api/v1/artifacts/{id}/blocks/{block_id}/keep) and one waiting review - beside prose; a
# Verification document of two checks, each with its serves, surface, run, when and
# negative_control, beside prose; results recorded on both checks; and, after a first coverage read
# that holds no "has no plan task" line, a plan of two plan tasks, one needing the other, beside
# prose. Before the move it takes, from the earlier release: GET /api/v1/issues/{key}/coverage, GET
# /api/v1/issues/{key}/results, GET /api/v1/issues/{key}/artifacts/{slug}/text?blocks=true for the
# spec, verification-md and plan-md, and GET /api/v1/issues/{key}/decisions, which must hold an
# entry in open_questions, an entry in decisions, and a decision in each of the three review states.
# It stops that server, boots this head's over the same database (lib-tree.sh's tree_boot_head, with
# the hold settings), waits for the backfill and for the newest build announcement to name this
# head's commit, and runs `envoy-dispatch convert-issues --project PROJECT`.
#
# On the converted issue it reads the same coverage and results and compares every field the earlier
# release answered, entry for entry and word for word; the coverage read must answer as many
# requirements as before, or the driver prints the answer and fails; the line `r-backup has no plan
# task` stands, since the issue holds a plan. It then compares each query with the reads it replaces,
# block by block, keyed by block id, each block's markdown as the earlier blocks read answered it
# (its byte range of the markdown served, the fixture being ASCII):
#   spec          ?view=spec answers the memo callout and each requirement, as the query's, with that
#                 markdown;
#   verification  ?view=verification answers each check, as the query's, with that markdown, its
#                 surface, run, when and negative_control values, and its serves;
#   plan          ?view=plan answers each plan task, as the query's, with that markdown, its serves and
#                 its needs;
#   tree          the read with no view answers each of those blocks once and no prose block of the
#                 three documents;
#   decisions     ?view=decisions answers each entry of the earlier read's open_questions, by its ask
#                 id, with the same question and options, and each of its decisions, by its
#                 block_id, with the same heading, text, kind, replaced, kept and waiting_review; it
#                 may answer more entries than the earlier read did.
# serves and needs are read off the tuples of the read with no view, each end named by the block id
# of the node it names. Every difference is printed with the query and the block or entry it is in,
# and any one fails the run.
#
# Inputs: DATABASE_URL (infra_1253_upgrade, a loopback database whose name begins infra_1253_,
# which this driver drops and creates again), PROJECT (CORE), DISPATCH_TOKEN (e2e-token, the bearer
# both servers accept), DEV_SIGNIN_LOGIN (alice, who approves nothing here, answers nothing, and
# keeps the one decision), BASELINE_SHA or BASELINE_BINARY (lib-baseline.sh's), and the stamped
# `envoy-dispatch` of this head on PATH. Each unmet input is exit 2.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: issue-tree-queries.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/infra_1253_upgrade?sslmode=disable \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice \
         {BASELINE_SHA=<commit> | BASELINE_BINARY=<path>} bash e2e/acceptance/issue-tree-queries.sh
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

readonly driver=issue-tree-queries.sh
require_env DATABASE_URL PROJECT DEV_SIGNIN_LOGIN
require_commands curl jq psql
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"
loopback_database_url "$database_url" || {
  printf '%s: DATABASE_URL names no single loopback database: this driver drops and creates it\n' "$driver" >&2
  exit 2
}

# The typed blocks of a role document; every other block of one is prose.
readonly typed_blocks='["callout","ask","requirement","plan_task","check","decision","reading","criterion"]'
# The roles whose documents the comparison reads, by the slug each holds on the issue.
readonly compared_slugs=(spec verification-md plan-md)

work_dir
sections=0
differences=0
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

# issue_tree_queries_section NAME: starts the section NAME, which the closing line counts.
issue_tree_queries_section() {
  sections=$((sections + 1))
  printf -- '--- section %s\n' "$1"
}

# issue_tree_queries_empty_database: drops and creates again the database DATABASE_URL names, so
# the base release's own Store.Migrate writes its schema and this driver's issue is all it holds. It
# drops only a loopback database whose name begins infra_1253_, connecting to the `postgres`
# database of the same server, as seed-decisions.sh's seed_reset_database drops its own.
issue_tree_queries_empty_database() {
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

# issue_tree_queries_keep SECTION PATH FILE: GET PATH as the agent, answered 200, kept in FILE.
issue_tree_queries_keep() {
  tree_send GET "$2"
  tree_want 200 "$1" "read $2"
  cp "$work/body" "$3"
}

# issue_tree_queries_differences SECTION WHAT EARLIER LATER: every field of the answer in EARLIER
# that the answer in LATER does not hold the same, entry for entry and word for word, each printed
# as one line naming WHAT and its path. A field EARLIER does not hold is not compared, so a member a
# later release adds reads as no difference. Each one found adds to `differences`.
issue_tree_queries_differences() {
  local section="$1" what="$2" earlier="$3" later="$4" found line count=0
  found="$(jq -rn --slurpfile e "$earlier" --slurpfile l "$later" '
    def diffs($e; $l; $path):
      if ($e | type) == "object" then
        if ($l | type) != "object" then ["\($path): \($e | tojson | .[0:300]) became \($l | tojson | .[0:300])"]
        else [$e | keys_unsorted[] as $k | diffs($e[$k]; $l[$k]; "\($path).\($k)")[]] end
      elif ($e | type) == "array" then
        if ($l | type) != "array" then ["\($path): \($e | tojson | .[0:300]) became \($l | tojson | .[0:300])"]
        elif ($e | length) != ($l | length) then
          ["\($path): \($e | length) entries became \($l | length): earlier \($e | tojson | .[0:600]), now \($l | tojson | .[0:600])"]
        else [range(0; $e | length) as $i | diffs($e[$i]; $l[$i]; "\($path)[\($i)]")[]] end
      elif $e == $l then []
      else ["\($path): \($e | tojson) became \($l | tojson)"] end;
    diffs($e[0]; $l[0]; "") | .[]')" || fail "$section" "$what" "jq could not compare $earlier with $later"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    printf 'DIFFERENCE: %s at %s\n' "$what" "$line"
    count=$((count + 1))
  done <<<"$found"
  differences=$((differences + count))
  if ((count == 0)); then
    pass "$section" "$what answers every field the earlier release answered, the same"
  fi
}

# issue_tree_queries_report SECTION WHAT LINES: prints each line of LINES as a difference of WHAT
# and adds them to `differences`; with none, WHAT passes.
issue_tree_queries_report() {
  local section="$1" what="$2" lines="$3" line count=0
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    printf 'DIFFERENCE: %s: %s\n' "$what" "$line"
    count=$((count + 1))
  done <<<"$lines"
  differences=$((differences + count))
  if ((count == 0)); then
    pass "$section" "$what"
  fi
}

# --- setup -------------------------------------------------------------------------------------

issue_tree_queries_section setup
issue_tree_queries_empty_database
baseline_start
# The bearer both servers accept, printed nowhere, inherited by no child process.
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-tree-queries-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The issue tree's queries"

# --- the earlier release writes the issue ----------------------------------------------------------

issue_tree_queries_section earlier
cat >"$work/spec.md" <<'SPEC'
:::callout{#memo kind="note" title=""}
The issue holds four queries over one graph. Each query is read as the earlier release read it. The comparison prints every difference.
:::

The summary paragraph of the issue the queries are compared on.

## Requirements

:::requirement{#r-store}
The store holds every block once.
:::

:::requirement{#r-format}
The format reads back word for word.
:::

:::requirement{#r-backup}
The backup holds the store.
:::

## Questions

:::ask{#q-open urgency="med" multiple="false"}
Which store holds the backup?

- Postgres: The database the pod runs.
- Files: A directory beside the server.
:::

## Decisions

A paragraph of prose between the questions and the decisions.

:::decision{#d-replaced replaced="true"}
The store was a directory of files.
:::

:::decision{#d-kept}
The store is the database the pod runs.
:::

:::decision{#d-waiting}
The format is the markdown the renderer writes.
:::
SPEC
corpus_issue earlier "The queries of one issue, compared" "$work/spec.md"
key="$corpus_key" spec="$corpus_spec"
[[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail earlier "create the issue" "the key $key is no issue key"
readonly key spec
cat >"$work/verification.md" <<VERIFICATION
How the queries are proved.

The checks below compare each query with the read it replaces.

:::check{#c-store serves="dispatch://$key/spec#b-r-store" surface="api" run="bash e2e/acceptance/issue-tree-queries.sh" when="before_merge" negative_control="Leave a block out of the store and the comparison names it."}
The comparison finds every block of the store once.
:::

:::check{#c-format serves="dispatch://$key/spec#b-r-format dispatch://$key/spec#b-r-store" surface="cli" run="dispatch read --issue $key" when="after_merge" negative_control="Change a word and the comparison prints both."}
The comparison reads the format back word for word.
:::
VERIFICATION
corpus_upload earlier "$key" verification.md "$work/verification.md"
readonly verification="$corpus_doc"
pass earlier "the earlier release wrote $key, its spec $spec and its Verification document $verification"
corpus_result earlier "$key" c-store pass "$corpus_commit_one" tester
corpus_result earlier "$key" c-format fail "$corpus_commit_two" implementer
corpus_wait_settled earlier

# The coverage before the issue holds a plan: no requirement reads "has no plan task".
issue_tree_queries_keep earlier "/api/v1/issues/$key/coverage" "$work/coverage-unplanned.json"
expect earlier "before the plan the earlier coverage read answers has_plan false and no line has no plan task" '[false,[]]' \
  '[.has_plan, [(.gaps[]?, .requirements[].gaps[]?) | select(contains("has no plan task"))]]'

cat >"$work/plan.md" <<PLAN
How the queries are built.

A paragraph of prose before the plan's tasks.

:::plan_task{#t-store serves="dispatch://$key/spec#b-r-store"}
Write the store.
:::

:::plan_task{#t-format serves="dispatch://$key/spec#b-r-format" needs="dispatch://$key/artifact/plan-md#b-t-store"}
Write the format.
:::
PLAN
corpus_upload earlier "$key" plan.md "$work/plan.md"
readonly plan="$corpus_doc"
pass earlier "the earlier release wrote the plan $plan of $key"
corpus_wait_settled earlier

# The person keeps one decision through the Keep route, with the sum the earlier read answers for it.
issue_tree_queries_keep earlier "/api/v1/issues/$key/decisions" "$work/decisions-unkept.json"
kept_sum="$(jq -r '[.decisions[]? | select(.block_id == "d-kept") | .text_sha256][0] // ""' "$work/decisions-unkept.json")"
[[ "$kept_sum" =~ ^[0-9a-f]{64}$ ]] ||
  fail earlier "the earlier decisions read answers the decision d-kept with its text_sha256" "$(head -c 600 "$work/decisions-unkept.json")"
tree_person_send POST "/api/v1/artifacts/$spec/blocks/d-kept/keep" "$(jq -cn --arg s "$kept_sum" '{text_sha256: $s}')"
[[ "$status" == 200 || "$status" == 201 ]] ||
  fail earlier "$person_login keeps the decision d-kept" "$tree_last_request answered HTTP $status: $(body)"
pass earlier "$person_login kept the decision d-kept through the Keep route"
corpus_wait_settled earlier

# --- the reads before the move ------------------------------------------------------------------

issue_tree_queries_section before
issue_tree_queries_keep before "/api/v1/issues/$key/coverage" "$work/coverage-before.json"
issue_tree_queries_keep before "/api/v1/issues/$key/results" "$work/results-before.json"
issue_tree_queries_keep before "/api/v1/issues/$key/decisions" "$work/decisions-before.json"
for slug in "${compared_slugs[@]}"; do
  issue_tree_queries_keep before "/api/v1/issues/$key/artifacts/$slug/text?blocks=true" "$work/text-$slug.json"
  jq -e '.blocks | type == "array"' "$work/text-$slug.json" >/dev/null ||
    fail before "the earlier release reads the blocks of $slug" "no blocks: $(head -c 600 "$work/text-$slug.json")"
done
cp "$work/coverage-before.json" "$work/body"
expect before "once the issue holds a plan the earlier coverage read answers the line r-backup has no plan task, once" '1' \
  '[(.gaps[]?, .requirements[].gaps[]?) | select(contains("has no plan task") and contains("r-backup"))] | unique | length'
expect before "the earlier coverage read answers the three requirements in the spec's order" '["r-store","r-format","r-backup"]' \
  '[.requirements[].block_id]'
cp "$work/decisions-before.json" "$work/body"
expect before "the earlier decisions read holds an entry in open_questions, an entry in decisions, and a decision replaced, one kept and one waiting review" \
  '[true,true,true,true,true]' \
  '[(.open_questions | length > 0), (.decisions | length > 0), ([.decisions[] | select(.replaced == true)] | length > 0),
    ([.decisions[] | select(.kept != null)] | length > 0), ([.decisions[] | select(.waiting_review == true)] | length > 0)]'
printf 'earlier decisions read: %s\n' "$(jq -c '{open_questions: [.open_questions[] | {id, question}],
  decisions: [.decisions[] | {block_id, kind, replaced, kept: (.kept != null), waiting_review}]}' "$work/decisions-before.json")"

# Each typed block and each prose block of the three documents, keyed by block id, with its
# markdown as the earlier read answered it: the slice of its markdown its byte range names, the
# fixture being ASCII, so a slice by character is a slice by byte. A typed block's attributes are
# read off its opening line; serves and needs as the block ids they name. Prose is a block of no
# typed type that no typed block holds.
for slug in "${compared_slugs[@]}"; do
  jq -c --arg slug "$slug" --argjson typed "$typed_blocks" '
    .markdown as $m
    | [.blocks[] | select(.type as $t | $typed | index($t)) | [.from, .to]] as $ranges
    | .blocks[]
    | ($m[.from:.to] | sub("\\s+$"; "")) as $text
    | (.type as $t | $typed | index($t) != null) as $isTyped
    | . as $block
    | select($isTyped or ([$ranges[] | select(.[0] <= $block.from and $block.from < .[1])] | length == 0))
    | ($text | split("\n")[0]) as $opening
    | ([$opening | capture("(?<name>[a-z_]+)=\"(?<value>(?:[^\"\\\\]|\\\\.)*)\""; "g") | {key: .name, value}] | from_entries) as $attributes
    | {slug: $slug, block_id: .id, type, typed: $isTyped, text: $text, attributes: $attributes,
       serves: [($attributes.serves // "") | splits(" +") | select(. != "") | sub("^.*#b-"; "")],
       needs: [($attributes.needs // "") | splits(" +") | select(. != "") | sub("^.*#b-"; "")]}' \
    "$work/text-$slug.json"
done | jq -s -c '.' >"$work/blocks-before.json"
while IFS= read -r line; do
  printf 'earlier block %s\n' "$line"
done < <(jq -c '.[] | {slug, block_id, type, typed, serves, needs}' "$work/blocks-before.json")
cp "$work/blocks-before.json" "$work/body"
expect before "the earlier documents hold the memo, the three requirements, the two checks and the two plan tasks beside prose" \
  '[["callout","memo"],["check","c-format"],["check","c-store"],["plan_task","t-format"],["plan_task","t-store"],["requirement","r-backup"],["requirement","r-format"],["requirement","r-store"],true,true,true]' \
  '[(.[] | select(.type == "callout" or .type == "requirement" or .type == "check" or .type == "plan_task") | [.type, .block_id])] | sort
   + [([.[] | select(.slug == "spec" and (.typed | not))] | length > 0),
      ([.[] | select(.slug == "verification-md" and (.typed | not))] | length > 0),
      ([.[] | select(.slug == "plan-md" and (.typed | not))] | length > 0)]'
baseline_stop_server
pass before "the earlier release's server stopped; its database stays"

# --- this head converts the issue -----------------------------------------------------------------

issue_tree_queries_section upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill upgrade
tree_wait_announcement upgrade
tree_convert upgrade
printf '%s\n' "$(tail -n 20 "$work/out")"
issue_tree_queries_keep upgrade "/api/v1/issues/$key/tree" "$work/tree.json"
cp "$work/tree.json" "$work/body"
expect upgrade "$key reads converted true on one page" '[true,null]' '[.converted, .cursor]'

# --- coverage and results ------------------------------------------------------------------------

issue_tree_queries_section coverage
issue_tree_queries_keep coverage "/api/v1/issues/$key/coverage" "$work/coverage-after.json"
[[ "$(jq '.requirements | length' "$work/coverage-after.json")" == "$(jq '.requirements | length' "$work/coverage-before.json")" ]] ||
  fail coverage "the coverage read of the converted issue answers the requirements the earlier read did" \
    "it answers $(head -c 1200 "$work/coverage-after.json")"
issue_tree_queries_differences coverage "GET /api/v1/issues/$key/coverage" "$work/coverage-before.json" "$work/coverage-after.json"
cp "$work/coverage-after.json" "$work/body"
expect coverage "the line r-backup has no plan task stands on the converted issue, which holds a plan" '1' \
  '[(.gaps[]?, .requirements[].gaps[]?) | select(contains("has no plan task") and contains("r-backup"))] | unique | length'
issue_tree_queries_keep coverage "/api/v1/issues/$key/results" "$work/results-after.json"
issue_tree_queries_differences coverage "GET /api/v1/issues/$key/results" "$work/results-before.json" "$work/results-after.json"

# --- the four queries and the tree read -----------------------------------------------------------

issue_tree_queries_section queries
for view in spec verification plan decisions; do
  issue_tree_queries_keep queries "/api/v1/issues/$key/tree?view=$view" "$work/view-$view.json"
done
# The relation each typed block holds, by the block ids of its two ends, off the one graph: the read
# with no view, which holds every requirement, check and plan task of the issue.
jq -c '([.nodes[] | {key: "\(.kind):\(.id)", value: (.block_id // "")}] | from_entries) as $block
  | [.tuples[] | select(.kind == "serves" or .kind == "needs")
     | {kind, from: ($block["\(.from.kind):\(.from.id)"] // "\(.from.kind):\(.from.id)"),
        to: ($block["\(.to.kind):\(.to.id)"] // "\(.to.kind):\(.to.id)")}]' "$work/tree.json" >"$work/relations.json"

# issue_tree_queries_compare VIEW FILE SELECT WHAT: each earlier block SELECT picks is answered by
# the read in FILE once, as the query's where VIEW is a view, with the earlier markdown, its values
# and its relations; prints every difference.
issue_tree_queries_compare() {
  local view="$1" file="$2" select="$3" what="$4" found
  found="$(jq -r --arg view "$view" --slurpfile blocks "$work/blocks-before.json" --slurpfile relations "$work/relations.json" '
    . as $read
    | $blocks[0][] | select('"$select"') as $block
    | [$read.nodes[] | select(.block_id == $block.block_id and (.issue // "") == "")] as $held
    | if ($held | length) != 1 then
        "block \($block.block_id): answered \($held | length) times, want once; the earlier read answered \($block.text | tojson)"
      else $held[0] as $node
        | (if $view != "" and $node.role != "query" then "block \($block.block_id): answered as \($node.role), want the query'"'"'s" else empty end),
          (if ($node.text | sub("\\s+$"; "")) != $block.text
           then "block \($block.block_id): the earlier read answered \($block.text | tojson), this one \($node.text | tojson)" else empty end),
          (if $block.type == "check" then
             (["surface", "run", "when", "negative_control"][] as $name
              | select(($node.attributes[$name] // null) != ($block.attributes[$name] // null))
              | "block \($block.block_id): \($name) the earlier read answered \($block.attributes[$name] | tojson), this one \($node.attributes[$name] | tojson)")
           else empty end),
          (if $block.type == "check" or $block.type == "plan_task" then
             ([$relations[0][] | select(.kind == "serves" and .from == $block.block_id) | .to] | sort) as $serves
             | select($serves != ($block.serves | sort))
             | "block \($block.block_id): serves the earlier read answered \($block.serves | sort | tojson), this one \($serves | tojson)"
           else empty end),
          (if $block.type == "plan_task" then
             ([$relations[0][] | select(.kind == "needs" and .from == $block.block_id) | .to] | sort) as $needs
             | select($needs != ($block.needs | sort))
             | "block \($block.block_id): needs the earlier read answered \($block.needs | sort | tojson), this one \($needs | tojson)"
           else empty end)
      end' "$file")" || fail queries "$what" "jq could not read $file"
  issue_tree_queries_report queries "$what" "$found"
}

issue_tree_queries_compare spec "$work/view-spec.json" '.slug == "spec" and (.type == "requirement" or (.type == "callout" and .block_id == "memo"))' \
  "?view=spec answers the memo callout and each requirement as the earlier spec read did"
issue_tree_queries_compare verification "$work/view-verification.json" '.type == "check"' \
  "?view=verification answers each check with its serves, surface, run, when and negative_control as the earlier verification-md read did"
issue_tree_queries_compare plan "$work/view-plan.json" '.type == "plan_task"' \
  "?view=plan answers each plan task with its serves and needs as the earlier plan-md read did"
issue_tree_queries_compare "" "$work/tree.json" \
  '.type == "requirement" or .type == "check" or .type == "plan_task" or (.type == "callout" and .block_id == "memo")' \
  "the tree read with no view answers each block of the three queries once, as the earlier reads did"

# No prose block of the three documents: none by its block id, and none by its markdown.
prose="$(jq -r --slurpfile blocks "$work/blocks-before.json" '
  . as $read
  | [$blocks[0][] | select(.typed | not)] as $prose
  | [$read.nodes[] | . as $node
     | $prose[] | select($node.block_id == .block_id or (($node.text // "") | sub("\\s+$"; "")) == .text)
     | "prose block \(.block_id) of \(.slug), a \(.type): \(.text | tojson), answered as \($node.kind):\($node.id)"]
  | .[]' "$work/tree.json")" || fail queries "read the prose of the tree read" "jq could not read $work/tree.json"
issue_tree_queries_report queries "the tree read with no view answers no prose block of the spec, verification-md or plan-md" "$prose"

# The Decisions query against the earlier decisions read's two members, entry for entry over the
# entries that read answered.
decided="$(jq -r --slurpfile earlier "$work/decisions-before.json" '
  . as $read
  | ([$earlier[0].open_questions[] as $ask
      | [$read.nodes[] | select(.kind == "ask" and .id == $ask.id)] as $held
      | if ($held | length) != 1 then "open question \($ask.id) (\($ask.question | tojson)): answered \($held | length) times, want once"
        else $held[0] as $node
          | (if $node.text != $ask.question then "open question \($ask.id): question \($ask.question | tojson) became \($node.text | tojson)" else empty end),
            (if ($node.ask.options // null) != $ask.options
             then "open question \($ask.id): options \($ask.options | tojson) became \($node.ask.options // null | tojson)" else empty end)
        end]
     + [$earlier[0].decisions[] as $decision
      | [$read.nodes[] | select(.decision != null and .decision.block_id == $decision.block_id)] as $held
      | if ($held | length) != 1 then "decision \($decision.block_id): answered \($held | length) times, want once"
        else $held[0].decision as $now
          | (["heading", "text", "kind", "replaced", "waiting_review"][] as $name
             | select(($now[$name] | tojson) != ($decision[$name] | tojson))
             | "decision \($decision.block_id): \($name) the earlier read answered \($decision[$name] | tojson), this one \($now[$name] | tojson)"),
            (($decision.kept | if . == null then null else {kept_by, kept_at} end) as $was
             | ($now.kept | if . == null then null else {kept_by, kept_at} end) as $is
             | select(($was | tojson) != ($is | tojson))
             | "decision \($decision.block_id): kept the earlier read answered \($was | tojson), this one \($is | tojson)")
        end])
  | .[]' "$work/view-decisions.json")" || fail queries "read ?view=decisions" "jq could not read $work/view-decisions.json"
issue_tree_queries_report queries \
  "?view=decisions answers each open question and each decision the earlier decisions read answered, with the same members" "$decided"

((differences == 0)) ||
  fail queries "every query and read of the converted issue answers what the earlier release answered" \
    "$differences differences, each printed above on a DIFFERENCE line"
pass queries "every query and read of the converted issue answers what the earlier release answered"

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
