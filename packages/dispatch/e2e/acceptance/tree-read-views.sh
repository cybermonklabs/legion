#!/usr/bin/env bash
# Drives the driver half of check c-tree-read-views of the issue tree's Verification document: the
# tree read, GET /api/v1/issues/{key}/tree, with no view and with its four views - spec,
# verification, plan and decisions - which are filters of the issue's one graph, each answering its
# own kinds and a fixed context out of it. It serves the spec's requirement r-queries and the
# root's r-document-tree. Every read of a tree goes through that route.
#
# It runs against the `bun run dispatch:local` server of the Verification preamble, over
# infra_1253, as the agent bearer and as the people the dev sign-in signs in: DEV_SIGNIN_LOGIN
# (alice), who writes where a person writes, and bob, who reads. The issues it seeds:
#   A  born converted: a comment thread alice wrote; a criterion alice wrote into the issue's tree
#      under the thread's first block; a requirement r1 under that criterion; an agent's decision d1
#      with its shaped tuple to r1 and the reading g1 under it; r1's constrained_by to a constraint
#      of the catalogue (the source of the `inversion` lens kind, which is not the `pre-mortem`
#      kind's source); a check c1 serving r1 with one result; plan tasks t0 and t1 serving r1, t1
#      needing t0; an ask a1 alice answered, with an agent's reading g2 under it; and alice's
#      approval of the Verification query.
#   B  a child of A, born converted and holding no requirement: a check cc1 and plan tasks t0 and t1
#      that serve A's r1, t1 needing B's own t0 and sharing its block id with A's t1, the check with
#      one result; a design block in B's own tree; and a constraint nothing names.
#   C  not converted (created while a server_processes row that lists no issue_room is live, which
#      the driver writes and takes back through DATABASE_URL, as the Go tests' holdIssuesUnconverted
#      does): a requirement set proposed and a check set overtaken through the /state route.
# What it asserts, section by section, is the check's text in its order; each section's comment
# names what it reads. A failure names the node, the tuple or the route it found wrong.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (the agent bearer, read
# here and never printed), PROJECT (CORE), DEV_SIGNIN_LOGIN (alice) and DATABASE_URL (infra_1253,
# the database that server serves, which psql reads and, for C's create and the author-only lens of
# the last section, writes as the Go tests of the same rules write it). Each unmet input is exit 2.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: tree-read-views.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/infra_1253?sslmode=disable \
         DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice \
         bash e2e/acceptance/tree-read-views.sh
       (run from packages/dispatch against `bun run dispatch:local`; DATABASE_URL names the
       database that server serves)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

readonly driver=tree-read-views.sh
require_env DATABASE_URL DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands awk cp curl date grep jq mktemp psql seq
read_bearer
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
base="${DISPATCH_URL%/}"
readonly base
# shellcheck disable=SC2031 # the run's own DATABASE_URL
database_url="$DATABASE_URL"
readonly database_url
loopback_database_url "$database_url" || {
  printf '%s: DATABASE_URL names no single loopback database\n' "$driver" >&2
  exit 2
}
readonly login="$DEV_SIGNIN_LOGIN" reader=bob
stamp="$(date +%s)-$$"
readonly stamp
readonly session="tree-read-views-$stamp"
# The checkout's packages directory, whose sources the caller search reads.
packages_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly packages_dir

# The words the driver writes and changes.
readonly requirement_words="The list orders the asks by urgency." requirement_changed="The list always orders the asks by urgency."
readonly decision_words="We order the list by urgency." decision_changed="We order the list by urgency, then by age."
readonly check_words="The check that proves the order." check_changed="The check that proves the urgent order."
readonly quote="order the list by urgency"
readonly retraction_reason="The run named the wrong commit."
readonly hidden_verdict="The order holds only if the urgency is set, alice thinks." hidden_finding="An ask with no urgency sorts last."

work_dir
sections=0
keys=()
held_process=""
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  local closing
  if [[ -n "$held_process" ]]; then
    env -u PGHOSTADDR psql "$database_url" -X -q -c "delete from server_processes where instance = '$held_process'::uuid" >/dev/null 2>&1 ||
      printf '%s: could not delete the server_processes row %s\n' "$driver" "$held_process" >&2
  fi
  for closing in "${keys[@]}"; do
    tree_send PATCH "/api/v1/issues/$closing" "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] || printf '%s: could not close %s: HTTP %s\n' "$driver" "$closing" "$status" >&2
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- the driver's own helpers ----------------------------------------------------------------

# tree_read_views_section NAME: starts the section NAME, which the closing line counts.
tree_read_views_section() {
  sections=$((sections + 1))
  printf -- '--- section %s\n' "$1"
}

# tree_read_views_expect FILE SECTION WHAT WANT JQ-ARGS…: lib.sh's expect over the answer kept in
# FILE rather than over the last one, which it puts back as $work/body first.
tree_read_views_expect() {
  local file="$1"
  shift
  cp "$file" "$work/body"
  expect "$@"
}

# tree_read_views_get SECTION AS PATH FILE: GET PATH as AS - `agent`, the agent bearer; or a login
# lib.sh's sign_in signed in, with its cookie - answered 200, kept in FILE.
tree_read_views_get() {
  local section="$1" as="$2" path="$3" file="$4"
  if [[ "$as" == agent ]]; then
    tree_send GET "$path"
  else
    request "cookie:$as" GET "$path"
    tree_last_request="GET $path as $as"
  fi
  tree_want 200 "$section" "read $path as $as"
  cp "$work/body" "$file"
}

# tree_read_views_trees SECTION PATH FILE: GET PATH as the agent with `Dispatch-Trees: 1`, the
# header under which a read answers each display tree (text_tree, body_tree), answered 200, kept in
# FILE. lib-tree.sh's tree_request sends no header but the bearer's.
tree_read_views_trees() {
  local section="$1" path="$2" file="$3"
  tree_last_request="GET $path (Dispatch-Trees: 1)"
  status="$(curl -sS -X GET -o "$work/body" -w '%{http_code}' -H "@$work/auth" -H 'Dispatch-Trees: 1' \
    "$base$path" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  tree_want 200 "$section" "read $path with its display trees"
  cp "$work/body" "$file"
}

# tree_read_views_node SECTION FILE BLOCK_ID [ISSUE]: the one node of the read in FILE whose block
# id is BLOCK_ID and which belongs to ISSUE (the issue read where ISSUE is empty): its store kind and
# id in node_kind and node_id. A read holding none, or more than one, fails naming the block.
tree_read_views_node() {
  local section="$1" file="$2" block="$3" issue="${4:-}" held
  held="$(jq -c --arg b "$block" --arg i "$issue" '[.nodes[] | select(.block_id == $b and (.issue // "") == $i) | {kind, id}]' "$file")"
  [[ "$(jq 'length' <<<"$held")" == 1 ]] ||
    fail "$section" "the read holds the block $block${issue:+ of $issue} once" "it holds $held"
  node_kind="$(jq -r '.[0].kind' <<<"$held")"
  node_id="$(jq -r '.[0].id' <<<"$held")"
}

# tree_read_views_edit_person SECTION WHAT DOCUMENT OPS: alice applies the edit ops OPS to DOCUMENT,
# as seed-corpus.sh's corpus_edit applies them as the agent.
tree_read_views_edit_person() {
  tree_person_send POST "/api/v1/artifacts/$3/edits" "$(jq -cn --argjson ops "$4" '{ops: $ops}')"
  tree_want 200 "$1" "$2"
  jq -e '.changed == true' "$work/body" >/dev/null || fail "$1" "$2" "the edit changed nothing: $(body)"
}

# tree_read_views_opener SECTION ISSUE SLUG BLOCK: the opening line of the typed block BLOCK as the
# document SLUG of ISSUE now reads, in tree_value, which an edit then finds by its words.
tree_read_views_opener() {
  tree_send GET "/api/v1/issues/$2/artifacts/$3/text"
  tree_want 200 "$1" "read $3 of $2"
  tree_value="$(jq -r '.markdown' "$work/body" | grep -m1 -E "^:{3,}[a-z_]+\\{#$4[ }]")" ||
    fail "$1" "find the opening line of $4 in $3 of $2" "$(body)"
}

# tree_read_views_comment SECTION ISSUE BODY [ANCHOR]: alice comments on ISSUE, anchored by the JSON
# ANCHOR where given, answered 201; its id in tree_value.
tree_read_views_comment() {
  tree_person_send POST "/api/v1/issues/$2/comments" "$(jq -cn --arg b "$3" --argjson a "${4:-null}" \
    '{body: $b} + (if $a == null then {} else {anchor: $a} end)')"
  tree_want 201 "$1" "$login comments on $2"
  tree_field "$1" "$login comments on $2" .id
}

# tree_read_views_version SECTION DOCUMENT: the newest version of DOCUMENT and its digest, as GET
# /api/v1/artifacts/{id} answers them, in newest_version and newest_digest.
tree_read_views_version() {
  tree_send GET "/api/v1/artifacts/$2"
  tree_want 200 "$1" "read the document $2"
  tree_field "$1" "the newest version of $2" '[.versions[].number] | max'
  newest_version="$tree_value"
  tree_field "$1" "the digest of the newest version of $2" "[.versions[] | select(.number == $newest_version) | .digest][0] // empty"
  newest_digest="$tree_value"
}

# tree_read_views_create SECTION TITLE FILE [PARENT]: the agent creates an issue of `project` whose
# spec is FILE's markdown, under PARENT where given (seed-corpus.sh's corpus_issue), closed when the
# run ends.
tree_read_views_create() {
  corpus_issue "$1" "$2" "$3" "${4:-}"
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
  keys+=("$corpus_key")
}

# tree_read_views_ask_id SECTION ISSUE BLOCK: the id of the ask the block BLOCK of ISSUE's documents
# opened (GET /api/v1/issues/{key}/asks?state=all), in tree_value.
tree_read_views_ask_id() {
  tree_send GET "/api/v1/issues/$2/asks?state=all"
  tree_want 200 "$1" "list the asks of $2"
  tree_field "$1" "the ask of the block $3 of $2" "[.[] | select(.block_id == \"$3\") | .id] | if length == 1 then .[0] else empty end"
}

# --- setup -------------------------------------------------------------------------------------

tree_read_views_section setup
tree_init
tree_sign_in setup "$login"
sign_in setup "$login" " (for a read or write that names its cookie)"
sign_in setup "$reader" " (the reader an author-only node is kept from)"
seeded_catalogue
pre_mortem_source="$(jq -r '[.lens_kinds[] | select(.id == "pre-mortem") | .sources[0].id][0] // empty' "$work/catalogue.json")"
named_constraint="$(jq -r '[.lens_kinds[] | select(.id == "inversion") | .sources[0].id][0] // empty' "$work/catalogue.json")"
named_constraint_ref="$(jq -r '[.lens_kinds[] | select(.id == "inversion") | .sources[0].ref][0] // empty' "$work/catalogue.json")"
[[ -n "$pre_mortem_source" && -n "$named_constraint" && "$named_constraint" != "$pre_mortem_source" ]] ||
  fail setup "the catalogue names a source constraint of pre-mortem and of inversion" "$(head -c 600 "$work/catalogue.json")"
[[ -n "$named_constraint_ref" ]] || named_constraint_ref="dispatch://constraint/$named_constraint"
readonly pre_mortem_source named_constraint named_constraint_ref
pass setup "the requirement's constraint is inversion's source $named_constraint; pre-mortem's source is $pre_mortem_source"

# --- the seed ------------------------------------------------------------------------------------

tree_read_views_section seed
cat >"$work/spec-a.md" <<SPEC
The summary paragraph: we $quote, said the summary, before the decision says it again.

:::ask{#a1 urgency="med" multiple="false"}
Which order does the list take?

- Urgency: The most urgent first
- Age: The oldest first
:::
SPEC
tree_read_views_create seed "The tree read and its views" "$work/spec-a.md"
readonly key="$corpus_key" spec="$corpus_spec"
tree_read_views_get seed agent "/api/v1/issues/$key/tree" "$work/a-born.json"
tree="$(jq -r '.tree // empty' "$work/a-born.json")"
[[ -n "$tree" ]] || fail seed "$key is born converted with its tree" "$(head -c 600 "$work/a-born.json")"
readonly tree
tree_read_views_get seed agent "/api/v1/artifacts/$tree" "$work/tree-artifact.json"
tree_slug="$(jq -r '.slug' "$work/tree-artifact.json")"
readonly tree_slug

# The thread alice wrote, timed by the driver's clock on each side of its write.
written_before="$(date +%s)"
corpus_comment person seed "$key" "We want the urgent asks first."$'\n\n'"And we want them quickly."
written_after="$(date +%s)"
readonly head="$corpus_item" written_before written_after
corpus_comment person seed "$key" "And the oldest after them." "$head"
readonly head_reply="$corpus_item"
tree_read_views_get seed agent "/api/v1/comments/$head/blocks" "$work/head-blocks.json"
head_block="$(jq -r '.blocks[0].ref // empty' "$work/head-blocks.json")"
[[ -n "$head_block" ]] || fail seed "read the first block of the comment $head" "$(head -c 600 "$work/head-blocks.json")"
readonly head_block
readonly criterion_words="I want the urgent asks first."
tree_read_views_edit_person seed "$login writes the criterion k1 into the tree of $key" "$tree" "$(jq -cn --arg p "$head_block" --arg w "$criterion_words" \
  '[{op: "insert", after: "end", markdown: (":::criterion{#k1 parent=\"" + $p + "\"}\n" + $w + "\n:::")}]')"
tree_read_views_edit_person seed "$login writes the requirement r1 under the criterion" "$spec" "$(jq -cn \
  --arg p "dispatch://$key/artifact/$tree_slug#b-k1" --arg w "$requirement_words" \
  '[{op: "insert", after: "end", markdown: (":::requirement{#r1 parent=\"" + $p + "\"}\n" + $w + "\n:::")}]')"
readonly shaped_pointer="dispatch://$key/spec#b-r1"
corpus_edit seed "an agent records the decision d1, which shaped r1, with the reading g1" "$spec" "$(jq -cn --arg s "$shaped_pointer" --arg w "$decision_words" \
  '[{op: "insert", after: "end", markdown: ("::::decision{#d1 shaped=\"" + $s + "\"}\n" + $w + "\n\n:::reading{#g1}\nThe agent reads it as urgency first and age second.\n:::\n::::")}]')"
cat >"$work/verification-a.md" <<VERIFICATION
How the order is proved.

:::check{#c1 serves="$shaped_pointer" surface="cli" run="bash e2e/acceptance/tree-read-views.sh" when="before_merge" negative_control="Answer the spec view with no context and the decision is missing."}
$check_words
:::
VERIFICATION
corpus_upload seed "$key" verification.md "$work/verification-a.md"
readonly verification="$corpus_doc"
cat >"$work/plan-a.md" <<PLAN
How the order is built.

:::plan_task{#t0 serves="$shaped_pointer"}
Sort the list by urgency.
:::

:::plan_task{#t1 serves="$shaped_pointer" needs="dispatch://$key/artifact/plan-md#b-t0"}
Show the sorted list.
:::
PLAN
corpus_upload seed "$key" plan.md "$work/plan-a.md"
readonly plan="$corpus_doc"
corpus_wait_settled seed
corpus_result seed "$key" c1 pass "$corpus_commit_one" tester
readonly first_result="$tree_value"
tree_read_views_ask_id seed "$key" a1
readonly answered_ask="$tree_value"
corpus_answer seed "$answered_ask" "Urgency, then age." Urgency
# The agent's reading g2 under the answered ask: the agent writes the spec again with the ask's block
# holding it, as a writer rewrites a document around a block.
tree_send GET "/api/v1/issues/$key/artifacts/spec/text"
tree_want 200 seed "read the spec of $key"
jq -r '.markdown' "$work/body" | awk -v words="The agent reads the answer as urgency before age." '
  !done && /^:::ask\{#a1[ }]/ { inside = 1; print ":" $0; next }
  inside && $0 == ":::" { print ""; print ":::reading{#g2}"; print words; print ":::"; print "::::"; inside = 0; done = 1; next }
  { print }' >"$work/spec-a-reading.md"
grep -q '^:::reading{#g2}$' "$work/spec-a-reading.md" || fail seed "write the reading g2 under the ask a1" "the spec holds no ask block a1: $(head -c 600 "$work/spec-a-reading.md")"
corpus_upload seed "$key" spec "$work/spec-a-reading.md"
corpus_wait_settled seed
corpus_review seed "$verification" approved
# The pointer from r1 to the catalogue's constraint.
tree_read_views_get seed agent "/api/v1/issues/$key/tree" "$work/a-seeded.json"
tree_read_views_node seed "$work/a-seeded.json" r1
readonly requirement_node="$node_id"
tree_person_send POST "/api/v1/issues/$key/nodes/$(q "$requirement_node")/pointers" \
  "$(jq -cn --arg t "$named_constraint_ref" '{kind: "constrained_by", action: "append", to: $t}')"
tree_want 200 seed "$login points r1 at the constraint $named_constraint"
pass seed "seeded $key: the thread $head with the reply $head_reply, k1, r1 ($requirement_node), d1 and g1, c1 with the result $first_result, t0 and t1, a1 ($answered_ask) with g2, and r1's constraint"

# The child B, a view of A: its check and its two plan tasks serve A's r1; its t1 shares A's t1's
# block id and needs its own t0.
printf "The child's design.\n" >"$work/spec-b.md"
tree_read_views_create seed "A child that proves its parent's requirement" "$work/spec-b.md" "$key"
readonly child="$corpus_key"
cat >"$work/verification-b.md" <<VERIFICATION
How the child proves its parent.

:::check{#cc1 serves="$shaped_pointer" surface="cli" run="bash e2e/acceptance/tree-read-views.sh" when="before_merge" negative_control="Leave the child's check out and the parent reads a gap."}
The child's check proves the parent's order.
:::
VERIFICATION
corpus_upload seed "$child" verification.md "$work/verification-b.md"
cat >"$work/plan-b.md" <<PLAN
How the child builds its part.

:::plan_task{#t0 serves="$shaped_pointer"}
The child writes the urgency field.
:::

:::plan_task{#t1 serves="$shaped_pointer" needs="dispatch://$child/artifact/plan-md#b-t0"}
The child fills the urgency field.
:::
PLAN
corpus_upload seed "$child" plan.md "$work/plan-b.md"
corpus_wait_settled seed
corpus_result seed "$child" cc1 pass "$corpus_commit_one" tester
readonly child_result="$tree_value"
tree_read_views_get seed agent "/api/v1/issues/$child/tree" "$work/b-born.json"
child_tree="$(jq -r '.tree // empty' "$work/b-born.json")"
[[ -n "$child_tree" ]] || fail seed "$child is born converted with its tree" "$(head -c 600 "$work/b-born.json")"
readonly child_tree
readonly design_words="The child's design block: the urgency field comes first."
tree_read_views_edit_person seed "$login gives $child a design block" "$child_tree" \
  "$(jq -cn --arg w "$design_words" '[{op: "insert", after: "end", markdown: $w}]')"
tree_send POST "/api/v1/issues/$child/constraints" "$(jq -cn --argjson a "$(tree_actor)" \
  '{standard: "A constraint nothing names", reference: "section 0", actor: $a}')"
tree_want 201 seed "write a constraint nothing names"
tree_field seed "write a constraint nothing names" .id
readonly unnamed_constraint="$tree_value"
corpus_wait_settled seed
pass seed "seeded the child $child: cc1 with the result $child_result, t0 and t1, a design block, and the constraint $unnamed_constraint nothing names"

# --- converted, the tree artifact, view_of -----------------------------------------------------------

tree_read_views_section converted
tree_read_views_get converted agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" converted "$key, born converted, reads converted true, its tree, and view_of null" \
  "$(jq -cn --arg t "$tree" '[true, $t, null]')" '[.converted, .tree, .view_of]'
for document in "$spec" "$verification" "$plan"; do
  tree_read_views_get converted agent "/api/v1/artifacts/$document" "$work/document.json"
  tree_read_views_expect "$work/document.json" converted "the tree of $key is the room the artifact read answers for $document" \
    "\"$tree\"" '.room'
done
tree_read_views_get converted agent "/api/v1/artifacts/$tree" "$work/tree-read.json"
tree_read_views_get converted agent "/api/v1/artifacts/$tree/text" "$work/tree-text.json"
tree_read_views_expect "$work/tree-text.json" converted "GET /api/v1/artifacts/{tree}/text answers the criterion written into $key" 'true' \
  --arg w "$criterion_words" '.markdown | contains($w)'
tree_read_views_get converted agent "/api/v1/issues/$key" "$work/a-issue.json"
tree_read_views_expect "$work/a-issue.json" converted "the issue read's documents do not name the tree artifact" '[]' \
  --arg t "$tree" '[.artifacts[]? | select(.id == $t or .kind == "tree") | .id]'
tree_read_views_get converted agent "/api/v1/issues/$child/tree" "$work/b.json"
tree_read_views_expect "$work/b.json" converted "the child $child, which holds no requirement, reads view_of $key" "\"$key\"" '.view_of'
tree_person_send POST "/api/v1/issues/$child/artifacts/spec/edits" \
  "$(jq -cn '{ops: [{op: "insert", after: "end", markdown: ":::requirement{#cr9}\nThe child'"'"'s own requirement.\n:::"}]}')"
tree_want 409 converted "a requirement written on $child is refused"
tree_read_views_expect "$work/body" converted "the refusal is REQUIREMENT_BELONGS_TO_ANCESTOR naming $key" '[true,true]' \
  --arg k "$key" '[.code == "REQUIREMENT_BELONGS_TO_ANCESTOR", (.error | contains($k))]'

# --- the spec view's context -----------------------------------------------------------------------

tree_read_views_section spec-context
tree_read_views_node spec-context "$work/a.json" d1
readonly decision_node="$node_id"
tree_read_views_get spec-context agent "/api/v1/issues/$key/tree?view=spec" "$work/a-spec.json"
held="$(jq -c --arg d "$decision_node" --arg r "$requirement_node" '[([.nodes[] | select(.id == $d) | .role] == ["context"]),
  ([.tuples[] | select(.kind == "shaped" and .from.id == $d and .to.id == $r)] | length == 1)]' "$work/a-spec.json")"
[[ "$held" == '[true,true]' ]] ||
  fail spec-context "?view=spec answers the decision whose shaped names the requirement as context, with the tuple" \
    "the decision $decision_node and its shaped tuple to the requirement $requirement_node are missing (context held, tuple held: $held)"
pass spec-context "?view=spec answers the decision $decision_node, whose shaped names $requirement_node, as context with the tuple"
tree_read_views_expect "$work/a-spec.json" spec-context "?view=spec answers the constraint r1's constrained_by names as context, with the tuple" \
  '[["context"],1]' --arg c "$named_constraint" --arg r "$requirement_node" \
  '[[.nodes[] | select(.kind == "constraint" and .id == $c) | .role],
    ([.tuples[] | select(.kind == "constrained_by" and .from.id == $r and .to.id == $c)] | length)]'
tree_read_views_expect "$work/a-spec.json" spec-context "?view=spec answers the criterion and the requirement as the query's" \
  '[["criterion","query"],["requirement","query"]]' \
  '[.nodes[] | select(.block_id == "k1" or .block_id == "r1") | [.supertag, .role]] | sort'

# --- the catalogue's context -----------------------------------------------------------------------

tree_read_views_section catalogue
tree_person_send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$requirement_node" \
  '{on: $on, kind: "pre-mortem", visibility: "project", findings: ["The urgency may be unset."]}')"
tree_want 201 catalogue "$login invokes a pre-mortem on r1"
tree_field catalogue "$login invokes a pre-mortem on r1" .id
readonly lens="$tree_value"
tree_read_views_get catalogue agent "/api/v1/issues/$key/tree" "$work/a-lens.json"
tree_read_views_expect "$work/a-lens.json" catalogue "the read with no view answers the lens as the tree's node" '["tree"]' \
  --arg l "$lens" '[.nodes[] | select(.kind == "lens" and .id == $l) | .role]'
missing=""
[[ "$(jq -c --arg l "$lens" '[[.nodes[] | select(.kind == "lens_kind" and .id == "pre-mortem") | .role],
  ([.tuples[] | select(.kind == "has_kind" and .from.id == $l and .to.kind == "lens_kind" and .to.id == "pre-mortem")] | length)]' \
  "$work/a-lens.json")" == '[["context"],1]' ]] || missing+=" the pre-mortem lens_kind node with the lens's has_kind tuple;"
[[ "$(jq -c --arg c "$named_constraint" --arg r "$requirement_node" '[[.nodes[] | select(.kind == "constraint" and .id == $c) | .role],
  ([.tuples[] | select(.kind == "constrained_by" and .from.id == $r and .to.id == $c)] | length)]' \
  "$work/a-lens.json")" == '[["context"],1]' ]] || missing+=" the constraint $named_constraint with r1's constrained_by tuple;"
[[ -z "$missing" ]] || fail catalogue "the read with no view answers the catalogue nodes its tuples name as context" "missing:${missing%;}"
pass catalogue "the read with no view answers the pre-mortem lens_kind and the constraint $named_constraint as context, each with its tuple"
tree_read_views_expect "$work/a-lens.json" catalogue "the read answers no other lens_kind and no other constraint" '[]' \
  --arg c "$named_constraint" '[.nodes[] | select((.kind == "lens_kind" and .id != "pre-mortem") or (.kind == "constraint" and .id != $c)) | "\(.kind):\(.id)"]'
for absent in "$pre_mortem_source:the pre-mortem kind's source constraint" "$unnamed_constraint:a constraint nothing names"; do
  id="${absent%%:*}" what="${absent#*:}"
  ! grep -qF -- "$id" "$work/a-lens.json" ||
    fail catalogue "the read answers $what nowhere, since a context node brings in no further context" "it names $id: $(head -c 600 "$work/a-lens.json")"
  pass catalogue "the read answers $what ($id) nowhere"
done

# --- the plan view, the verification view, the coverage and the tree of a parent ----------------

tree_read_views_section parent
tree_read_views_get parent agent "/api/v1/issues/$key/tree?view=plan" "$work/a-plan.json"
tree_read_views_expect "$work/a-plan.json" parent "?view=plan answers the parent's plan tasks and the child's, each marked with its issue" \
  "$(jq -cn --arg c "$child" '[["",  "t0"],["", "t1"],[$c, "t0"],[$c, "t1"]]')" \
  '[.nodes[] | select(.supertag == "plan_task") | [(.issue // ""), .block_id]] | sort'
tree_read_views_expect "$work/a-plan.json" parent "?view=plan answers them in one order, each with its position, needs and size" '[true,true]' \
  '[.nodes[] | select(.supertag == "plan_task")] as $tasks
   | [($tasks | all(.plan != null and (.plan | has("position") and has("needs") and has("size")))),
      ([$tasks[] | .plan.position] as $p | ($p | all(. != null)) and $p == ($p | sort))]'
tree_read_views_get parent agent "/api/v1/issues/$key/tree?view=verification" "$work/a-verification.json"
tree_read_views_expect "$work/a-verification.json" parent \
  "?view=verification answers the child's check through serves, marked with the child, with the child issue as its parent, and its result" \
  "$(jq -cn --arg c "$child" --arg r "$child_result" '[[$c, "issue", $c], [$r]]')" \
  --arg r "$child_result" '[([.nodes[] | select(.block_id == "cc1") | [.issue, .parent.kind, .parent.id]] | first),
    [.nodes[] | select(.kind == "result" and .id == $r) | .id]]'
tree_read_views_expect "$work/a-verification.json" parent \
  "?view=verification answers the parent's check and its result as the query's and the requirement above them as context" \
  '[["check","query"],["requirement","context"],["result","query"]]' --arg r "$first_result" \
  '[.nodes[] | select((.block_id == "c1" and (.issue // "") == "") or (.block_id == "r1" and (.issue // "") == "") or (.kind == "result" and .id == $r))
    | [(if .kind == "result" then "result" else .supertag end), .role]] | sort'
tree_read_views_get parent agent "/api/v1/issues/$key/coverage" "$work/a-coverage.json"
tree_read_views_expect "$work/a-coverage.json" parent "the parent's coverage reads r1 as covered" '[true,[]]' \
  '[.requirements[] | select(.block_id == "r1")] | first | [(.checks >= 1), [.gaps[]? | select(contains("no check"))]]'
tree_read_views_get parent agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" parent \
  "the parent's read with no view answers the child's plan tasks through serves, marked with the child, with the child issue as their parent" \
  "$(jq -cn --arg c "$child" '[[$c, "issue", $c, "t0"], [$c, "issue", $c, "t1"]]')" --arg c "$child" \
  '[.nodes[] | select(.supertag == "plan_task" and .issue == $c) | [.issue, .parent.kind, .parent.id, .block_id]] | sort'
# The two plan tasks t1, one of each issue, each named by its own id: the child's needs tuple and the
# parent's each join the plan task of their own issue.
tree_read_views_node parent "$work/a.json" t0
parent_t0="$node_id"
tree_read_views_node parent "$work/a.json" t1
parent_t1="$node_id"
tree_read_views_node parent "$work/a.json" t0 "$child"
child_t0="$node_id"
tree_read_views_node parent "$work/a.json" t1 "$child"
child_t1="$node_id"
readonly parent_t0 parent_t1 child_t0 child_t1
[[ "$parent_t1" != "$child_t1" ]] ||
  fail parent "the two plan tasks that share the block id t1 answer different ids" "both answer the id $parent_t1"
needs="$(jq -c '[.tuples[] | select(.kind == "needs") | [.from.id, .to.id]] | sort' "$work/a.json")"
want_needs="$(jq -cn --arg a "$parent_t1" --arg b "$parent_t0" --arg c "$child_t1" --arg d "$child_t0" '[[$a, $b], [$c, $d]] | sort')"
[[ "$(jq -c --argjson w "$want_needs" '[.[] | select(. as $t | $w | any(. == $t))] | sort' <<<"$needs")" == "$want_needs" ]] ||
  fail parent "each needs tuple names its ends by their own ids, so the child's tuple is told from the parent's" \
    "the shared block id t1 reads as one plan task's: the needs tuples are $needs, want $want_needs among them"
serves="$(jq -c --arg r "$requirement_node" --arg a "$parent_t1" --arg c "$child_t1" \
  '[.tuples[] | select(.kind == "serves" and .to.id == $r and (.from.id == $a or .from.id == $c)) | .from.id] | sort' "$work/a.json")"
[[ "$serves" == "$(jq -cn --arg a "$parent_t1" --arg c "$child_t1" '[$a, $c] | sort')" ]] ||
  fail parent "each serves tuple of the two plan tasks t1 names its from end by its own id" \
    "the shared block id t1: the serves tuples to $requirement_node from them are $serves"
pass parent "the plan tasks t1 of $key ($parent_t1) and of $child ($child_t1) share the block id and answer their own tuples"

# --- each view of the one graph ----------------------------------------------------------------

tree_read_views_section views
tree_read_views_get views agent "/api/v1/issues/$key/tree?view=decisions" "$work/a-decisions.json"
tree_read_views_get views agent "/api/v1/issues/$key/tree?view=spec" "$work/a-spec.json"
jq -c '[.nodes[] | "\(.kind):\(.id)"]' "$work/a.json" >"$work/a-nodes.json"
for view in spec verification plan decisions; do
  file="$work/a-$view.json"
  # A node reaches a view only through the graph: every node a view answers, the read with no view
  # answers.
  tree_read_views_expect "$file" views "?view=$view answers no node the read with no view does not" '[]' \
    --slurpfile graph "$work/a-nodes.json" '[.nodes[] | "\(.kind):\(.id)" | select(. as $n | $graph[0] | index($n) == null)]'
  tree_read_views_expect "$file" views "each node ?view=$view answers as the query's carries its parent's id, supertag and first line" '[]' \
    '[.nodes[] | select(.role == "query")
      | select(.parent == null or (.parent.id // "") == "" or (.parent.supertag // "") == "" or (.parent.first_line // "") == "")
      | "\(.kind):\(.id)"]'
done
tree_read_views_expect "$work/a-spec.json" views "?view=spec answers as the query's only criteria, requirements and the memo callout" '[]' \
  '[.nodes[] | select(.role == "query" and ((.supertag == "criterion" or .supertag == "requirement" or .supertag == "callout") | not)) | "\(.kind):\(.id)"]'
tree_read_views_expect "$work/a-verification.json" views "?view=verification answers as the query's only checks and results" '[]' \
  '[.nodes[] | select(.role == "query" and ((.kind == "result" or .supertag == "check") | not)) | "\(.kind):\(.id)"]'
tree_read_views_expect "$work/a-plan.json" views "?view=plan answers as the query's only plan nodes" '[]' \
  '[.nodes[] | select(.role == "query" and ((.supertag == "plan_task" or .supertag == "plan_role") | not)) | "\(.kind):\(.id)"]'
tree_read_views_expect "$work/a-decisions.json" views "?view=decisions answers as the query's only asks, decisions and readings" '[]' \
  '[.nodes[] | select(.role == "query" and ((.kind == "ask" or .supertag == "decision" or .supertag == "reading") | not)) | "\(.kind):\(.id)"]'
tree_read_views_expect "$work/a-decisions.json" views "?view=decisions answers the answered ask a1 with its answer, the decision d1, and g1 under d1" \
  "$(jq -cn --arg a "$answered_ask" --arg d "$decision_node" '[$a, "Urgency, then age.", $d, $d]')" \
  --arg a "$answered_ask" --arg d "$decision_node" \
  '[([.nodes[] | select(.kind == "ask" and .id == $a) | .id] | first), ([.nodes[] | select(.kind == "ask" and .id == $a) | .attributes.answer] | first),
    ([.nodes[] | select(.id == $d and .supertag == "decision") | .id] | first), ([.nodes[] | select(.block_id == "g1") | .parent.id] | first)]'
reading="$(jq -c --arg a "$answered_ask" '[.nodes[] | select(.block_id == "g2" and .supertag == "reading" and .role == "query" and .parent.kind == "ask" and .parent.id == $a) | .id]' \
  "$work/a-decisions.json")"
reading_id="$(jq -r '[.nodes[] | select(.block_id == "g2") | .id][0] // "g2"' "$work/a.json")"
[[ "$(jq 'length' <<<"$reading")" == 1 ]] ||
  fail views "?view=decisions answers the agent's reading under the person's answered ask, once, the query's, under the ask's node" \
    "it answers no reading $reading_id under the ask $answered_ask: $reading"
pass views "?view=decisions answers the reading $reading_id under the answered ask $answered_ask"

# --- each node's members ---------------------------------------------------------------------------

tree_read_views_section members
tree_read_views_trees members "/api/v1/issues/$key/tree" "$work/a-trees.json"
tree_read_views_expect "$work/a-trees.json" members "every node of the read with no view carries visibility and made_at" '[]' \
  '[.nodes[] | select((has("visibility") and has("made_at")) | not) | "\(.kind):\(.id)"]'
tree_read_views_expect "$work/a-trees.json" members "the comment thread reads project and the time the driver wrote it, within one second" '[true]' \
  --arg h "$head" --argjson from "$written_before" --argjson to "$written_after" \
  '[.nodes[] | select(.kind == "comment" and .id == $h)
    | .visibility == "project" and ((.made_at | sub("\\.[0-9]+"; "") | fromdateiso8601) as $t | $t >= $from - 1 and $t <= $to + 1)]'
tree_read_views_expect "$work/a-trees.json" members "the lens reads the visibility the driver wrote it with" '["project"]' \
  --arg l "$lens" '[.nodes[] | select(.kind == "lens" and .id == $l) | .visibility]'
tree_read_views_expect "$work/a-trees.json" members "each node of the tree but an issue's whose text is not empty carries text_tree" '[]' \
  '[.nodes[] | select(.role == "tree" and .kind != "issue" and (.text // "") != "" and (.text_tree == null)) | "\(.kind):\(.id)"]'
tree_read_views_trees members "/api/v1/issues/$key/artifacts/spec/blocks" "$work/a-spec-blocks.json"
tree_read_views_trees members "/api/v1/issues/$key/comments" "$work/a-comments.json"
tree_read_views_trees members "/api/v1/issues/$key/decisions" "$work/a-decisions-read.json"
for display in "r1:requirement:$(jq -c '[.[]? | select(.id == "r1") | .text_tree][0]' "$work/a-spec-blocks.json")" \
  "$head:comment:$(jq -c --arg h "$head" '[.. | objects | select(.id? == $h and has("body_tree")) | .body_tree][0]' "$work/a-comments.json")" \
  "d1:decision:$(jq -c '[.decisions[]? | select(.block_id == "d1") | .text_tree][0]' "$work/a-decisions-read.json")"; do
  block="${display%%:*}" rest="${display#*:}"
  kind="${rest%%:*}" want="${rest#*:}"
  if [[ "$kind" == comment ]]; then
    got="$(jq -c --arg h "$block" '[.nodes[] | select(.kind == "comment" and .id == $h) | .text_tree][0]' "$work/a-trees.json")"
  else
    got="$(jq -c --arg b "$block" '[.nodes[] | select(.block_id == $b and (.issue // "") == "") | .text_tree][0]' "$work/a-trees.json")"
  fi
  [[ "$want" != null && "$got" == "$want" ]] ||
    fail members "the $kind $block answers the text_tree its own read answers, byte for byte" "the tree read answers $got, its own read $want"
  pass members "the $kind $block answers the text_tree its own read answers, byte for byte"
done
tree_read_views_expect "$work/a.json" members "the read with no view carries the serves, shaped, needs and constrained_by tuples among its nodes" \
  '["constrained_by","needs","serves","shaped"]' \
  '[.tuples[] | .kind | select(. == "serves" or . == "shaped" or . == "needs" or . == "constrained_by")] | unique'

# --- a version of the spec query -------------------------------------------------------------------

tree_read_views_section version
tree_read_views_version version "$spec"
readonly tied_version="$newest_version"
tree_read_views_opener version "$key" spec d1
opener="$tree_value"
corpus_edit version "retract the shaped tuple of d1" "$spec" \
  "$(jq -cn --arg f "$opener" '[{op: "replace", find: $f, with: "::::decision{#d1}"}]')"
tree_read_views_get version agent "/api/v1/issues/$key/tree?view=spec&v=$tied_version" "$work/a-spec-then.json"
tree_read_views_get version agent "/api/v1/issues/$key/tree?view=spec" "$work/a-spec-now.json"
for read in "then:$work/a-spec-then.json:1" "now:$work/a-spec-now.json:0"; do
  name="${read%%:*}" rest="${read#*:}"
  file="${rest%:*}" want="${rest##*:}"
  tree_read_views_expect "$file" version "?view=spec $name (version $tied_version, then the head) holds the shaped tie from d1 to r1 $want times" "$want" \
    --arg d "$decision_node" --arg r "$requirement_node" '[.tuples[] | select(.kind == "shaped" and .from.id == $d and .to.id == $r)] | length'
done
tree_send GET "/api/v1/issues/$key/tree?view=spec&v=$((tied_version + 1000))"
tree_want 404 version "a version the spec query does not have is refused"
tree_read_views_expect "$work/body" version "a version the spec query does not have answers QUERY_VERSION_NOT_FOUND" '"QUERY_VERSION_NOT_FOUND"' '.code'
tree_read_views_opener version "$key" spec d1
opener="$tree_value"
corpus_edit version "write the shaped tuple of d1 again" "$spec" \
  "$(jq -cn --arg f "$opener" --arg s "$shaped_pointer" '[{op: "replace", find: $f, with: ($f | sub("}$"; " shaped=\"" + $s + "\"}"))}]')"

# --- comments, the decision's review state and its Keep ------------------------------------------

tree_read_views_section comments
tree_read_views_comment comments "$key" "Does urgency come before age?" \
  "$(jq -cn --arg a "$spec" --arg q "$quote" '{artifact: $a, quote: $q, block_id: "d1"}')"
readonly anchored="$tree_value"
tree_read_views_comment comments "$key" "The first of two comments on the issue."
readonly earlier_comment="$tree_value"
later_comment=""
for attempt in $(seq 1 40); do
  tree_read_views_comment comments "$key" "The second of two comments on the issue, written again ($attempt) until its id sorts below the first's."
  if [[ "$(jq -rn --arg a "$tree_value" --arg b "$earlier_comment" '$a < $b')" == true ]]; then
    later_comment="$tree_value"
    break
  fi
done
[[ -n "$later_comment" ]] || fail comments "write a later comment whose id sorts below $earlier_comment" "40 writes found none"
readonly later_comment
tree_read_views_get comments agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" comments "the comment anchored inside d1's paragraph by its block stands under the decision" \
  "\"$decision_node\"" --arg c "$anchored" '[.nodes[] | select(.kind == "comment" and .id == $c) | .parent.id][0]'
order="$(jq -r --arg k "$key" --arg a "$earlier_comment" --arg b "$later_comment" '
  (.nodes | to_entries | map(select(.value.parent != null and .value.parent.kind == "issue" and .value.parent.id == $k))) as $children
  | ([$children[] | select(.value.rank != null) | .key] | max // -1) as $ranked
  | ([$children[] | select(.value.kind == "comment" and .value.id == $a) | .key][0] // -1) as $first
  | ([$children[] | select(.value.kind == "comment" and .value.id == $b) | .key][0] // -1) as $second
  | if $first < 0 or $second < 0 then "missing: the comment \($a) at \($first) or \($b) at \($second)"
    elif $first > $second then "the comments \($b) and \($a) come back in the reverse of the order they were written"
    elif $first < $ranked then "the comment \($a) stands before a ranked child of the issue"
    else "" end' "$work/a.json")"
[[ -z "$order" ]] || fail comments "the read answers the two comments after the issue's ranked children, in the order they were written" "$order"
pass comments "the read answers $earlier_comment and then $later_comment, after the issue's ranked children, in the order they were written"
tree_read_views_get comments agent "/api/v1/issues/$key/tree?view=decisions" "$work/a-decisions.json"
state="$(jq -c --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision | {waiting_review, kept, replaced}][0]' "$work/a-decisions.json")"
[[ "$state" != null ]] || fail comments "?view=decisions answers d1 with its review state" "it answers no decision $decision_node"
tree_read_views_expect "$work/a.json" comments "the read with no view answers d1's review state as ?view=decisions does" "$(jq -cS . <<<"$state")" \
  --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision | {waiting_review, kept, replaced}][0]'
sum="$(jq -r --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision.text_sha256][0] // empty' "$work/a.json")"
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || fail comments "the read answers d1's text_sha256" "$(head -c 600 "$work/a.json")"
tree_person_send POST "/api/v1/artifacts/$spec/blocks/d1/keep" "$(jq -cn --arg s "$sum" '{text_sha256: $s}')"
[[ "$status" == 200 || "$status" == 201 ]] || fail comments "$login keeps d1 through the Keep route" "$tree_last_request answered HTTP $status: $(body)"
tree_read_views_get comments agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" comments "the next read answers d1 kept by $login" "\"$login\"" \
  --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision.kept.kept_by][0]'
tree_read_views_get comments agent "/api/v1/issues/$key/decisions" "$work/a-decisions-read.json"
# Every member the decisions read answers for d1, equal on the decision node of the read with no view.
members="$(jq -r --slurpfile read "$work/a-decisions-read.json" --arg d "$decision_node" '
  ([$read[0].decisions[] | select(.block_id == "d1")][0]) as $want
  | ([.nodes[] | select(.id == $d) | .decision][0] // {}) as $got
  | [$want | keys_unsorted[] as $k | select(($got | has($k)) | not) | "\($k) is missing"]
    + [$want | keys_unsorted[] as $k | select(($got | has($k)) and $got[$k] != $want[$k]) | "\($k) reads \($got[$k] | tojson), want \($want[$k] | tojson)"]
    + (if $want.links != null and ($want.links | length) > 0 and ([$got.links[]? | has("stale")] | all | not) then ["the link'"'"'s stale is missing"] else [] end)
  | .[]' "$work/a.json")"
[[ -z "$members" ]] ||
  fail comments "the read with no view answers on d1 every member GET /api/v1/issues/{key}/decisions answers for it, each equal" \
    "the decision $decision_node: ${members//$'\n'/; }"
pass comments "the read with no view answers on d1 every member the decisions read answers for it, each equal"
decided="$(jq -c --arg d "$decision_node" '[.tuples[] | select(.kind == "decided_by" and .from.id == $d) | .to] | first // empty' "$work/a.json")"
[[ -n "$decided" ]] || fail comments "the read answers d1's decided_by tuple" "it holds none: $(jq -c '[.tuples[] | select(.kind == "decided_by")]' "$work/a.json")"
tree_read_views_expect "$work/a.json" comments "the read answers the node d1's decided_by names as context" '["context"]' \
  --argjson e "$decided" '[.nodes[] | select(.kind == $e.kind and .id == $e.id) | .role]'

# --- a result against the words it was recorded for ---------------------------------------------

tree_read_views_section results
tree_read_views_node results "$work/a.json" c1
readonly check_node="$node_id"
corpus_result results "$key" c1 pass "$corpus_commit_two" implementer
readonly result="$tree_value"
tree_read_views_get results agent "/api/v1/issues/$key/tree" "$work/a.json"
check_sum="$(jq -r --arg c "$check_node" '[.nodes[] | select(.id == $c) | .check.text_sha256][0] // empty' "$work/a.json")"
check_version="$(jq -r --arg c "$check_node" '[.nodes[] | select(.id == $c) | .check.changed_version][0] // empty' "$work/a.json")"
[[ -n "$check_sum" && -n "$check_version" ]] || fail results "the check c1 answers its text_sha256 and changed_version" "$(head -c 600 "$work/a.json")"
tree_read_views_expect "$work/a.json" results "the result answers its verdict, retracted null and check_sha256 equal to the check's text_sha256" \
  "$(jq -cn --arg s "$check_sum" '["pass", null, $s]')" --arg r "$result" \
  '[.nodes[] | select(.kind == "result" and .id == $r) | .result | [.verdict, .retracted, .check_sha256]][0]'
corpus_edit results "change the check c1's words" "$verification" \
  "$(jq -cn --arg f "$check_words" --arg w "$check_changed" '[{op: "replace", find: $f, with: $w}]')"
tree_read_views_get results agent "/api/v1/issues/$key/tree" "$work/a.json"
moved="$(jq -c --arg c "$check_node" '[.nodes[] | select(.id == $c) | .check | [.text_sha256, .changed_version]][0]' "$work/a.json")"
[[ "$(jq -r '.[0]' <<<"$moved")" != "$check_sum" && "$(jq -r '.[1]' <<<"$moved")" != "$check_version" ]] ||
  fail results "the check's text_sha256 and changed_version move after its words change" \
    "the check $check_node answers $moved, as before the change ($check_sum, version $check_version)"
tree_read_views_expect "$work/a.json" results "the result's check_sha256 stays the sum it was recorded against" "\"$check_sum\"" \
  --arg r "$result" '[.nodes[] | select(.kind == "result" and .id == $r) | .result.check_sha256][0]'
tree_person_send POST "/api/v1/results/$result/retract" "$(jq -cn --arg r "$retraction_reason" '{reason: $r}')"
[[ "$status" == 200 || "$status" == 201 ]] || fail results "$login retracts the result $result" "$tree_last_request answered HTTP $status: $(body)"
tree_read_views_get results agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" results "the retracted result answers retracted with the reason" "\"$retraction_reason\"" \
  --arg r "$result" '[.nodes[] | select(.kind == "result" and .id == $r) | .result.retracted.reason][0]'

# --- a link's two facts, and the digest the Verification approval records ----------------------

tree_read_views_section links
# tree_read_views_link WHAT CHANGED STALE: the decision node's one link reads changed_since_tie
# CHANGED and stale STALE, and answers the link as GET /api/v1/issues/{key}/decisions answers it,
# member for member.
tree_read_views_link() {
  tree_read_views_get links agent "/api/v1/issues/$key/tree" "$work/a.json"
  tree_read_views_get links agent "/api/v1/issues/$key/decisions" "$work/a-decisions-read.json"
  tree_read_views_expect "$work/a.json" links "$1: d1's link reads changed_since_tie $2 and stale $3" "[$2,$3]" \
    --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision.links[0] | .changed_since_tie, .stale]'
  tree_read_views_expect "$work/a.json" links "$1: d1's link is answered as the decisions read answers it, member for member" \
    "$(jq -cS '[.decisions[] | select(.block_id == "d1") | .links][0]' "$work/a-decisions-read.json")" \
    --arg d "$decision_node" '[.nodes[] | select(.id == $d) | .decision.links][0]'
}
tree_read_views_link "with the tie written" false false
tree_read_views_version links "$verification"
readonly approved_digest="$newest_digest"
corpus_edit links "change the words of r1, which d1's shaped names" "$spec" \
  "$(jq -cn --arg f "$requirement_words" --arg w "$requirement_changed" '[{op: "replace", find: $f, with: $w}]')"
tree_read_views_link "after r1's words changed" true false
tree_read_views_version links "$verification"
[[ "$newest_digest" == "$approved_digest" ]] ||
  fail links "a change to a context requirement's words leaves the Verification digest unchanged" \
    "the digest moved from $approved_digest to $newest_digest (version $newest_version)"
pass links "a change to a context requirement's words leaves the Verification digest $approved_digest unchanged"
corpus_result links "$child" cc1 fail "$corpus_commit_two" implementer
tree_read_views_version links "$verification"
[[ "$newest_digest" == "$approved_digest" ]] ||
  fail links "a second result on the child's check leaves the Verification digest unchanged" \
    "the digest moved from $approved_digest to $newest_digest (version $newest_version)"
pass links "a second result on the child's check leaves the Verification digest unchanged"
corpus_edit links "change d1's own words" "$spec" \
  "$(jq -cn --arg f "$decision_words" --arg w "$decision_changed" '[{op: "replace", find: $f, with: $w}]')"
tree_read_views_link "after d1's own words changed" true true

# --- an answered ask that shaped the requirement ----------------------------------------------------

tree_read_views_section ask
corpus_edit ask "an agent writes the ask a3 into the spec" "$spec" "$(jq -cn \
  '[{op: "insert", after: "end", markdown: ":::ask{#a3 urgency=\"low\" multiple=\"false\"}\nWhich age breaks a tie?\n\n- Oldest: The oldest first\n- Newest: The newest first\n:::"}]')"
corpus_wait_settled ask
tree_read_views_ask_id ask "$key" a3
readonly shaping_ask="$tree_value"
corpus_answer ask "$shaping_ask" "The oldest breaks a tie." Oldest
tree_read_views_opener ask "$key" spec a3
opener="$tree_value"
tree_read_views_edit_person ask "$login gives a3 a shaped tuple to r1" "$spec" \
  "$(jq -cn --arg f "$opener" --arg s "$shaped_pointer" '[{op: "replace", find: $f, with: ($f | sub("}$"; " shaped=\"" + $s + "\"}"))}]')"
tree_read_views_get ask agent "/api/v1/issues/$key/tree?view=spec" "$work/a-spec.json"
tree_read_views_expect "$work/a-spec.json" ask "?view=spec answers the answered ask a3 as context, with its shaped tuple to r1" '[["context"],1]' \
  --arg a "$shaping_ask" --arg r "$requirement_node" \
  '[[.nodes[] | select(.kind == "ask" and .id == $a) | .role], ([.tuples[] | select(.kind == "shaped" and .from.id == $a and .to.id == $r)] | length)]'

# --- anchors ---------------------------------------------------------------------------------------

tree_read_views_section anchors
tree_read_views_comment anchors "$key" "Is the order always by urgency?" \
  "$(jq -cn --arg a "$spec" '{artifact: $a, quote: "orders the asks", block_id: "r1"}')"
readonly on_requirement="$tree_value"
tree_read_views_get anchors agent "/api/v1/issues/$key/tree" "$work/a.json"
tree_read_views_expect "$work/a.json" anchors "a comment whose anchor names the converted spec, a quote and r1's block_id hangs under r1" \
  "\"$requirement_node\"" --arg c "$on_requirement" '[.nodes[] | select(.kind == "comment" and .id == $c) | .parent.id][0]'
readonly refused_body="A comment on words the decision does not hold."
tree_person_send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg b "$refused_body" --arg a "$spec" \
  '{body: $b, anchor: {artifact: $a, quote: "words the decision never held", block_id: "d1"}}')"
tree_want 400 anchors "a comment whose anchor names d1 with a quote d1 does not hold is refused"
tree_read_views_expect "$work/body" anchors "the refusal is INVALID_ANCHOR naming block_id" '[true,true]' \
  '[.code == "INVALID_ANCHOR", (.error | contains("block_id"))]'
tree_read_views_get anchors agent "/api/v1/issues/$key/comments" "$work/a-comments.json"
tree_read_views_expect "$work/a-comments.json" anchors "the issue holds no such comment" '0' \
  --arg b "$refused_body" '[.. | objects | select(.body? == $b)] | length'

# --- a run of the command bar -------------------------------------------------------------------

tree_read_views_section run
request "cookie:$login" POST /api/v1/going/requests "$(jq -cn --arg p "$project" '{words: "Which plan holds the order?", project: $p}')"
tree_last_request="POST /api/v1/going/requests as $login"
tree_want 201 run "$login starts a run of the command bar"
tree_field run "$login starts a run of the command bar" .id
readonly run="$tree_value"
request "cookie:$login" POST "/api/v1/issues/$key/comments" '{"body": "The run says the plan holds the order."}' "$run"
tree_last_request="POST /api/v1/issues/$key/comments as $login under the run $run"
tree_want 201 run "$login comments on $key under the run's header"
tree_field run "$login comments on $key under the run's header" .id
readonly run_comment="$tree_value"
tree_read_views_get run "$login" "/api/v1/issues/$key/tree" "$work/a-alice.json"
held="$(jq -c --arg r "$run" --arg c "$run_comment" --arg k "$key" '[[.nodes[] | select(.kind == "interaction" and .id == $r) | .role],
  ([.tuples[] | select(.kind == "changed_by_run" and .from.kind == "comment" and .from.id == $c and .to.id == $r)] | length),
  ([.tuples[] | select(.kind == "changed_by_run" and .from.kind == "issue" and .from.id == $k and .to.id == $r)] | length)]' "$work/a-alice.json")"
[[ "$held" == '[["context"],1,0]' ]] ||
  fail run "$login's read answers the run's interaction as context, with the comment's changed_by_run and none from the issue" \
    "the run $run reads $held (interaction roles, the comment's tuples, the issue's tuples)"
pass run "$login's read answers the run $run as context with the comment's changed_by_run tuple, and the issue holds none"
tree_read_views_get run "$reader" "/api/v1/issues/$key/tree" "$work/a-bob.json"
tree_read_views_expect "$work/a-bob.json" run "$reader's read answers the comment and neither the tuple nor the interaction" '[1,0]' \
  --arg r "$run" --arg c "$run_comment" '[([.nodes[] | select(.kind == "comment" and .id == $c)] | length),
    ([.nodes[] | select(.id == $r)] + [.tuples[] | select(.to.id == $r or .from.id == $r)] | length)]'
! grep -qF -- "$run" "$work/a-bob.json" || fail run "$reader's answer names the run nowhere" "it names $run"

# --- an issue that has not converted ------------------------------------------------------------

tree_read_views_section unconverted
query unconverted "hold a live process that lists no issue room" "
  insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at)
  select gen_random_uuid(), signing_keys, schema_version, schema_creatable, now() + interval '1 hour'
  from server_processes order by heartbeat_at desc limit 1
  returning instance::text"
held_process="$answer"
[[ "$held_process" =~ ^[0-9a-f-]{36}$ ]] || fail unconverted "hold a live process that lists no issue room" "psql answered '$held_process'"
cat >"$work/spec-c.md" <<'SPEC'
A paragraph of the unconverted spec's prose.

:::requirement{#rc}
The unconverted issue holds one requirement.
:::
SPEC
tree_read_views_create unconverted "An issue that has not converted" "$work/spec-c.md"
readonly unconverted="$corpus_key" unconverted_spec="$corpus_spec"
query unconverted "take the held process back" "delete from server_processes where instance = '$held_process'::uuid returning 1"
held_process=""
cat >"$work/verification-c.md" <<VERIFICATION
A paragraph of the unconverted Verification document's prose.

:::check{#cc serves="dispatch://$unconverted/spec#b-rc" surface="cli" run="bash e2e/acceptance/tree-read-views.sh"}
The unconverted check proves its requirement.
:::
VERIFICATION
corpus_upload unconverted "$unconverted" verification.md "$work/verification-c.md"
readonly unconverted_verification="$corpus_doc" unconverted_slug="$corpus_slug"
corpus_wait_settled unconverted
for set in "$unconverted/spec#rc:proposed" "$unconverted/$unconverted_slug#cc:overtaken"; do
  tree_person_send POST "/api/v1/issues/$unconverted/nodes/$(q "${set%:*}")/state" "$(jq -cn --arg s "${set##*:}" '{state: $s}')"
  tree_want 200 unconverted "$login sets ${set%:*} ${set##*:}"
done
tree_read_views_trees unconverted "/api/v1/issues/$unconverted/tree" "$work/c.json"
tree_read_views_expect "$work/c.json" unconverted "$unconverted reads converted false and tree null" '[false,null]' '[.converted, .tree]'
missing="$(jq -r '[.nodes[] | .block_id] as $held | ["rc", "cc"][] | select(. as $b | $held | index($b) == null)' "$work/c.json")"
[[ -z "$missing" ]] || fail unconverted "the read with no view answers the typed blocks of an issue that has not converted" \
  "the blocks ${missing//$'\n'/ and } are missing"
tree_read_views_node unconverted "$work/c.json" rc
readonly unconverted_requirement="$node_id"
for typed in "rc|spec|$unconverted_spec|proposed|issue|$unconverted" "cc|$unconverted_slug|$unconverted_verification|overtaken|block|$unconverted_requirement"; do
  IFS='|' read -r block slug document state parent_kind parent_id <<<"$typed"
  tree_read_views_trees unconverted "/api/v1/issues/$unconverted/artifacts/$slug/blocks" "$work/c-blocks.json"
  position="$(jq --arg b "$block" '[.[] | .id] | index($b)' "$work/c-blocks.json")"
  want="$(jq -cn --arg s "$state" --arg pk "$parent_kind" --arg pi "$parent_id" --arg d "$document" --arg slug "$slug" \
    --argjson p "$position" --argjson tt "$(jq -c --arg b "$block" '[.[] | select(.id == $b) | .text_tree][0]' "$work/c-blocks.json")" \
    '{count: 1, state: $s, parent: [$pk, $pi], visibility: "project", made_at: true, text_tree: $tt, document: {artifact_id: $d, slug: $slug, position: $p}}')"
  tree_read_views_expect "$work/c.json" unconverted \
    "$block of $unconverted is answered once, under the parent R1 gives it, with its state, visibility, made_at, text_tree and document" \
    "$(jq -cS . <<<"$want")" --arg b "$block" \
    '[.nodes[] | select(.block_id == $b)] as $held | $held[0]
     | {count: ($held | length), state, parent: [.parent.kind, .parent.id], visibility, made_at: (.made_at != null), text_tree,
        document: (.document // null | if . == null then null else {artifact_id, slug, position} end)}'
done

# --- a node whose parent the reader may not see ------------------------------------------------

tree_read_views_section visibility
tree_person_send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$requirement_node" --arg v "$hidden_verdict" --arg f "$hidden_finding" \
  '{on: $on, kind: "pre-mortem", verdict: $v, findings: [$f]}')"
tree_want 201 visibility "$login invokes the pre-mortem lens her risk stands under"
tree_field visibility "$login invokes the pre-mortem lens her risk stands under" .id
readonly hidden_lens="$tree_value"
tree_person_send POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "dispatch://lens/$hidden_lens" \
  '{lens: $l, heading: "The urgency field is left unset.", early_warning: "Asks sort last.", prevention: "Require the field."}')"
tree_want 201 visibility "$login writes a project risk under that lens"
tree_field visibility "$login writes a project risk under that lens" .id
readonly risk="$tree_value"
# The store refuses a project node under an author-only one, so the lens and its finding are
# reserved for alice once the risk stands, as TestTheTreeReadLiftsAVisibleNodeAboveAParentItsReaderMayNotSee
# reserves its lens.
query visibility "reserve the lens $hidden_lens and its finding for $login" "
  with reserved as (
    update graph_nodes set visibility = 'author'
    where (kind = 'lens' and id = '$hidden_lens') or (kind = 'block' and id like 'lens/' || '$hidden_lens' || '#%')
    returning 1)
  select count(*) from reserved"
[[ "$answer" -ge 2 ]] || fail visibility "reserve the lens and its finding for $login" "the update reserved $answer nodes"
pass visibility "reserved the lens $hidden_lens and its finding for $login; the risk $risk stays project"
tree_read_views_get visibility "$login" "/api/v1/issues/$key/tree" "$work/a-alice.json"
tree_read_views_expect "$work/a-alice.json" visibility "read as $login, the risk's parent is the lens" "\"$hidden_lens\"" \
  --arg r "$risk" '[.nodes[] | select(.kind == "risk" and .id == $r) | .parent.id][0]'
tree_read_views_get visibility "$reader" "/api/v1/issues/$key/tree" "$work/a-bob.json"
tree_read_views_expect "$work/a-bob.json" visibility "read as $reader, the risk stands under the requirement, after its own children" \
  "$(jq -cn --arg r "$requirement_node" '[$r, true]')" --arg risk "$risk" --arg r "$requirement_node" \
  '(.nodes | to_entries) as $all
   | ([$all[] | select(.value.kind == "risk" and .value.id == $risk)][0]) as $lifted
   | [$lifted.value.parent.id,
      ([$all[] | select(.value.parent != null and .value.parent.id == $r and .value.id != $risk) | .key] | all(. < $lifted.key))]'
for hidden in "$hidden_lens" "$hidden_verdict" "$hidden_finding"; do
  ! grep -qF -- "$hidden" "$work/a-bob.json" ||
    fail visibility "$reader's answer names nothing of the hidden lens" "it names $hidden: no node, parent or tuple may"
done
pass visibility "$reader's answer names neither the hidden lens's id, its has_kind or part_of tuple, its verdict nor its finding's text"
tree_read_views_expect "$work/a-bob.json" visibility "the pre-mortem lens_kind is in $reader's answer once, as the context of the visible lens" \
  '["context"]' '[.nodes[] | select(.kind == "lens_kind" and .id == "pre-mortem") | .role]'

# --- the decisions read through this release ------------------------------------------------------

tree_read_views_section decisions-read
tree_read_views_get decisions-read agent "/api/v1/issues/$key/decisions" "$work/a-decisions-read.json"
tree_read_views_get decisions-read agent "/api/v1/issues/$key/tree?view=decisions" "$work/a-decisions.json"
tree_read_views_expect "$work/a-decisions.json" decisions-read \
  "GET /api/v1/issues/{key}/decisions answers the decision nodes of ?view=decisions, each with the same review state" \
  "$(jq -cS '[.decisions[] | {block_id, waiting_review, kept: (.kept != null), replaced}] | sort_by(.block_id)' "$work/a-decisions-read.json")" \
  '[.nodes[] | select(.decision != null) | .decision | {block_id, waiting_review, kept: (.kept != null), replaced}] | sort_by(.block_id)'
# No caller of that route is left in the page, the client or the daemon: a search that finds a
# planted caller in a scratch copy of one file first, so a search that matched nothing cannot read as
# a clean tree. A caller is a line of a source file that names the route's path, the issue's key a
# parameter of it, and is no comment; a test's file, which stands a fake route up and calls none, is
# no caller.
readonly caller_pattern='issues/(\{[a-zA-Z_]+\}|\$\{[^}]+\}|:[a-zA-Z_]+|%[sv])/decisions([?"'"'"'`]|$)'
tree_read_views_callers() {
  grep -rnE --include='*.ts' --include='*.tsx' --include='*.go' --exclude-dir=__tests__ \
    --exclude='*.test.ts' --exclude='*.test.tsx' --exclude='*_test.go' -- "$caller_pattern" "$@" |
    grep -vE '^[^:]+:[0-9]+:[[:space:]]*(//|\*|/\*)' || true
}
scratch="$(mktemp -d "$work/callers.XXXXXX")"
cp "$packages_dir/envoy-client/src/dispatch-owner.ts" "$scratch/dispatch-owner.ts"
printf '\nexport const planted = (key: string) => fetch(`/api/v1/issues/${key}/decisions`);\n' >>"$scratch/dispatch-owner.ts"
planted="$(tree_read_views_callers "$scratch")"
[[ -n "$planted" ]] || fail decisions-read "the caller search finds a planted caller of the decisions route" "it found none in $scratch"
printf 'planted caller found: %s\n' "$planted"
pass decisions-read "the caller search finds the planted caller in a scratch copy of dispatch-owner.ts"
callers="$(tree_read_views_callers "$packages_dir/dispatch/web/src" "$packages_dir/envoy-client/src" "$packages_dir/daemon")"
[[ -z "$callers" ]] ||
  fail decisions-read "packages/dispatch/web/src, packages/envoy-client/src and packages/daemon hold no caller of GET /api/v1/issues/{key}/decisions" \
    "${callers//$'\n'/; }"
pass decisions-read "packages/dispatch/web/src, packages/envoy-client/src and packages/daemon hold no caller of GET /api/v1/issues/{key}/decisions"

# --- the child's spec view ------------------------------------------------------------------------

tree_read_views_section child
tree_read_views_get child agent "/api/v1/issues/$child/tree?view=spec" "$work/b-spec.json"
tree_read_views_expect "$work/b-spec.json" child "the child's ?view=spec answers its design block as context" '["context"]' \
  --arg w "$design_words" '[.nodes[] | select((.text // "" | sub("\\s+$"; "")) == $w) | .role]'
tree_read_views_expect "$work/b-spec.json" child "the child's ?view=spec answers the ancestor's requirement its check serves as context" \
  "$(jq -cn --arg k "$key" '[["context", $k]]')" --arg r "$requirement_node" '[.nodes[] | select(.id == $r) | [.role, .issue]]'

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
