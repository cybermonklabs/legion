#!/usr/bin/env bash
# The driver half of the check c-child-view: a child issue is a view on its ancestor's spec. It
# runs against the server of `bun run dispatch:local`, on the database that server serves, and
# seeds every issue it reads; the check's four Go tests run before it.
#
#   V1 It creates a parent P whose spec holds two requirements, r1 and r2, and under it the first
#      child C, whose spec holds no requirement and whose one check, c1, serves r1. C's tree read
#      answers view_of P. P's ?view=verification answers c1 through its serves tuple, marked with
#      C; C's ?view=spec answers r1, drawn from P, and no other requirement, and the store holds no
#      requirement block of C.
#   V2 It writes a requirement block into C's spec through each write of the API that can carry
#      one: an upload of the spec (POST /api/v1/issues/{key}/artifacts), both edit routes
#      (POST /api/v1/artifacts/{id}/edits, POST /api/v1/issues/{key}/artifacts/{slug}/edits), and
#      POST /api/v1/issues creating a second child of P whose spec holds one. Each is 409
#      REQUIREMENT_BELONGS_TO_ANCESTOR naming P; where a write is taken, the failure names C, the
#      write and the requirement it wrote. No second child is created, and C reads unchanged: its
#      spec at the same version and bytes, and no requirement block of it in the store.
#   V3 P's coverage reads r1 served by C's check alone and r2 served by nothing. It writes P's
#      Verification document, whose check pc2 serves r2, and records a pass on C's check: P's
#      verification query stands at the same version, with the same artifact_version_text_sha256,
#      once the drain of both issues' queries ends. It records a pass on pc2, and P's coverage reads
#      both requirements proved.
#   V4 It creates an issue D, closes it, creates a child E under it, writes a requirement block into
#      E's spec through POST /api/v1/artifacts/{id}/edits, and reads the write taken: E is no view
#      and its spec holds the requirement.
#
# Inputs: DATABASE_URL (the database the server serves, on this machine, which psql reads),
# DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (the agent bearer, read here and
# never printed) and PROJECT (an existing project key). Each unmet input is exit 2. The agent
# bearer writes every issue, document and result, and closes the issues when the run ends, also
# when a check fails.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line. The
# last line is `PASS: issue-tree-child-view.sh complete 4 sections`. Every check is `cmd || fail …`,
# never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE bash e2e/acceptance/issue-tree-child-view.sh
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
require_commands curl jq psql sha256sum
# shellcheck disable=SC2031 # the run's own DATABASE_URL
loopback_database_url "$DATABASE_URL" || {
  printf 'issue-tree-child-view.sh: DATABASE_URL names no single loopback host; the driver reads the store it names\n' >&2
  usage
  exit 2
}
read_bearer

readonly driver=issue-tree-child-view.sh
# shellcheck disable=SC2031 # the caller's DISPATCH_URL; only lib.sh's `cli` exports another
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2031 # the run's own DATABASE_URL
readonly database_url="$DATABASE_URL"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="issue-tree-child-view-$stamp"
readonly commit="$corpus_commit_one"
readonly refusal_code=REQUIREMENT_BELONGS_TO_ANCESTOR

work_dir
tree_init

keys=()
cleanup() {
  local code=$? index
  trap - EXIT INT TERM
  for ((index = ${#keys[@]} - 1; index >= 0; index--)); do
    [[ -z "${keys[$index]}" ]] || close_issue "${keys[$index]}"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# issue_tree_child_view_issue SECTION TITLE FILE [PARENT]: seed-corpus.sh's corpus_issue, the key
# held to an issue key's shape, since it goes into SQL, and added to the issues the run closes.
issue_tree_child_view_issue() {
  corpus_issue "$@"
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
  keys+=("$corpus_key")
}

# issue_tree_child_view_block_node SECTION DOCUMENT BLOCK: the store address of block BLOCK of the
# document whose artifact id is DOCUMENT, as its document_blocks row names it, read in the room the
# document is edited in (its document_rooms row's room, else the document itself), in
# issue_tree_child_view_node; empty where no row holds the block.
issue_tree_child_view_block_node() {
  with_vars "$1" "read the node of $3 of $2" \
    "select coalesce((select b.node_id from document_blocks b
       where b.artifact_id = coalesce((select r.room from document_rooms r where r.artifact_id = :'doc'::uuid), :'doc'::uuid)
         and b.block_id = :'block'), '')" \
    doc="$2" block="$3"
  issue_tree_child_view_node="$answer"
}

# issue_tree_child_view_requirement_rows SECTION DOCUMENT: the block ids of the requirement blocks
# the store holds in the room the document DOCUMENT is edited in (its document_rooms row's room,
# else the document itself): of the spec of a converted issue, every requirement block of any of
# its issue's documents. Sorted and joined by commas, in $answer.
issue_tree_child_view_requirement_rows() {
  with_vars "$1" "read the requirement blocks of the room of $2" \
    "select coalesce(string_agg(b.block_id, ',' order by b.block_id), '')
     from document_blocks b
     where b.artifact_id = coalesce((select r.room from document_rooms r where r.artifact_id = :'doc'::uuid), :'doc'::uuid)
       and b.type = 'requirement'" \
    doc="$2"
}

# issue_tree_child_view_drained SECTION ISSUE…: waits until no query of the issues ISSUE… is owed
# a version (issue_query_marks, which the drain empties once it writes them), every 2 s for at most
# 300 s, as lib-tree.sh's waits poll.
issue_tree_child_view_drained() {
  local section="$1" deadline=$((SECONDS + tree_wait_seconds)) list
  shift
  list="{$(IFS=,; printf '%s' "$*")}"
  while :; do
    with_vars "$section" "read the queries owed a version" \
      "select clock_timestamp() || '|' || count(*) from issue_query_marks where issue_key = any(:'keys'::text[])" keys="$list"
    [[ "${answer#*|}" != 0 ]] || break
    ((SECONDS < deadline)) ||
      fail "$section" "the queries of $* are drained within $tree_wait_seconds s" "at ${answer%%|*} ${answer#*|} issues still owe a version"
    sleep "$tree_poll_seconds"
  done
  pass "$section" "no query of $* is owed a version"
}

# issue_tree_child_view_record SECTION ISSUE CHECK: seed-corpus.sh's corpus_result of a pass of
# CHECK on ISSUE.
issue_tree_child_view_record() {
  corpus_result "$1" "$2" "$3" pass "$commit" tester
  pass "$1" "recorded the pass $tree_value of $3 on $2"
}

# issue_tree_child_view_query SECTION DOCUMENT: the newest version of the query node DOCUMENT and the
# digest artifact_version_text_sha256 answers for it, `<version>|<digest>`, in $answer.
issue_tree_child_view_query() {
  with_vars "$1" "read the newest version of $2 and its digest" \
    "select v.number || '|' || coalesce(artifact_version_text_sha256(v.artifact_id, v.number), '')
     from artifact_versions v where v.artifact_id = :'doc'::uuid order by v.number desc limit 1" doc="$2"
  [[ "$answer" =~ ^[0-9]+\|[0-9a-f]{64}$ ]] || fail "$1" "read the newest version of $2 and its digest" "it reads '$answer'"
}

# issue_tree_child_view_refused SECTION WRITE REQUIREMENT: the last answer refused WRITE, which
# carried the requirement block REQUIREMENT into the child's spec, 409 REQUIREMENT_BELONGS_TO_ANCESTOR
# naming the parent. A write that was taken fails naming the child, the write and the requirement.
issue_tree_child_view_refused() {
  [[ "$status" != 2?? ]] ||
    fail "$1" "$2 on $child is refused $refusal_code" \
      "$child took the requirement $3 through $2: $tree_last_request answered HTTP $status: $(body)"
  tree_want 409 "$1" "$2 on $child is refused $refusal_code"
  jq -e --arg c "$refusal_code" --arg p "$parent" '.code == $c and (.error | type == "string" and contains($p))' \
    "$work/body" >/dev/null || fail "$1" "$2 on $child is refused $refusal_code naming $parent" "it answers $(body)"
  pass "$1" "$2 carrying the requirement $3 into $child's spec is refused 409 $refusal_code naming $parent"
}

# issue_tree_child_view_requirement ID WORDS: one requirement block, as markdown.
issue_tree_child_view_requirement() { printf ':::requirement{#%s}\n%s\n:::\n' "$1" "$2"; }

# --- V1: the parent, and its first child, a view on its spec --------------------------------------

cat >"$work/parent-spec.md" <<'SPEC'
The parent whose spec its children are views on.

:::requirement{#r1}
The first requirement of the parent, which its child's check serves.
:::

:::requirement{#r2}
The second requirement of the parent, which a check of its own serves.
:::
SPEC
issue_tree_child_view_issue V1 "Child view check: the parent $stamp" "$work/parent-spec.md"
# Not readonly: seed-corpus.sh's corpus_issue takes a local of that name.
parent="$corpus_key"
readonly parent_spec="$corpus_spec"
printf 'The first child of %s holds the design of its part. Its checks serve the parent'"'"'s words.\n' "$parent" \
  >"$work/child-spec.md"
issue_tree_child_view_issue V1 "Child view check: the first child $stamp" "$work/child-spec.md" "$parent"
readonly child="$corpus_key" child_spec="$corpus_spec"
printf 'How %s proves its part.\n\n:::check{#c1 serves="dispatch://%s/spec#b-r1" surface="api" run="bash e2e/acceptance/issue-tree-child-view.sh" negative_control="Let the child hold the requirement and the check serves its own."}\nThe first child proves the first requirement of its parent.\n:::\n' \
  "$child" "$parent" >"$work/child-verification.md"
corpus_upload V1 "$child" verification.md "$work/child-verification.md"
pass V1 "created $parent, whose spec holds r1 and r2, and its child $child, whose check c1 serves $parent's r1"

tree_read agent "$child"
tree_want 200 V1 "read the tree of $child"
expect V1 "$child is a view on $parent" "$(jq -cn --arg p "$parent" '$p')" '.view_of'
expect V1 "$child's check c1 hangs from $child, since it serves no requirement of its own issue" \
  "$(jq -cn --arg c "$child" '[{kind: "issue", id: $c}]')" \
  '[.nodes[] | select(.supertag == "check" and .block_id == "c1") | {kind: .parent.kind, id: .parent.id}]'
issue_tree_child_view_block_node V1 "$parent_spec" r1
readonly r1_node="$issue_tree_child_view_node"
[[ -n "$r1_node" ]] || fail V1 "read the node of $parent's r1" "no document_blocks row holds r1 of $parent_spec"
tree_read agent "$parent" verification
tree_want 200 V1 "read the verification view of $parent"
cp "$work/body" "$work/parent-verification.json"
c1_node="$(jq -r --arg c "$child" 'first((.nodes // [])[] | select(.supertag == "check" and .block_id == "c1" and .issue == $c) | .id) // empty' \
  "$work/parent-verification.json")" || fail V1 "read the verification view of $parent" "unreadable: $(body)"
[[ -n "$c1_node" ]] ||
  fail V1 "the verification view of $parent answers $child's check c1, marked with $child" \
    "its checks are $(jq -c '[.nodes[] | select(.supertag == "check") | {id, block_id, issue}]' "$work/parent-verification.json")"
readonly c1_node
pass V1 "the verification view of $parent answers $child's check $c1_node, marked with $child"
jq -e --arg c "$c1_node" --arg r "$r1_node" 'any(.tuples[]; .kind == "serves" and .from.id == $c and .to.id == $r)' \
  "$work/parent-verification.json" >/dev/null ||
  fail V1 "the verification view of $parent answers $c1_node through its serves tuple to $r1_node" \
    "its serves tuples are $(jq -c '[.tuples[] | select(.kind == "serves")]' "$work/parent-verification.json")"
pass V1 "the verification view of $parent answers $c1_node through its serves tuple to $r1_node"
tree_read agent "$child" spec
tree_want 200 V1 "read the spec view of $child"
expect V1 "the spec view of $child answers $parent's r1 and no other requirement" \
  "$(jq -cn --arg p "$parent" '[{block_id: "r1", issue: $p}]')" \
  '[.nodes[] | select(.supertag == "requirement") | {block_id, issue: (.issue // null)}]'
issue_tree_child_view_requirement_rows V1 "$child_spec"
[[ -z "$answer" ]] || fail V1 "the store holds no requirement block of $child's spec" "it holds $answer"
pass V1 "the store holds no requirement block of $child's spec"

# --- V2: every write through the API refuses a requirement on the view ----------------------------

tree_send GET "/api/v1/artifacts/$child_spec/text"
tree_want 200 V2 "read $child's spec"
jq -j '.markdown' "$work/body" >"$work/child-spec-now.md"
before_version="$(jq -r '.version' "$work/body")"
child_token="$(jq -r '.token' "$work/body")"
before_sum="$(sha256sum "$work/child-spec-now.md")"
before_sum="${before_sum%% *}"
readonly before_version child_token before_sum

{
  cat "$work/child-spec-now.md"
  printf '\n'
  issue_tree_child_view_requirement r-upload "The child states a requirement of its own through an upload."
} >"$work/upload-spec.md"
tree_send POST "/api/v1/issues/$child/artifacts" "$(jq -cn --rawfile c "$work/upload-spec.md" --arg t "$child_token" \
  --argjson a "$(tree_actor)" '{name: "spec.md", content: $c, precondition: {document: $t}, actor: $a}')"
issue_tree_child_view_refused V2 "an upload of the spec (POST /api/v1/issues/$child/artifacts)" r-upload

tree_send POST "/api/v1/artifacts/$child_spec/edits" "$(jq -cn \
  --arg m "$(issue_tree_child_view_requirement r-edit-id "The child states a requirement of its own through an edit by id.")" \
  --argjson a "$(tree_actor)" '{ops: [{op: "insert", markdown: $m, after: "end"}], actor: $a}')"
issue_tree_child_view_refused V2 "an edit by id (POST /api/v1/artifacts/$child_spec/edits)" r-edit-id

tree_send POST "/api/v1/issues/$child/artifacts/spec/edits" "$(jq -cn \
  --arg m "$(issue_tree_child_view_requirement r-edit-slug "The child states a requirement of its own through an edit by slug.")" \
  --argjson a "$(tree_actor)" '{ops: [{op: "insert", markdown: $m, after: "end"}], actor: $a}')"
issue_tree_child_view_refused V2 "an edit by slug (POST /api/v1/issues/$child/artifacts/spec/edits)" r-edit-slug

{
  printf 'A second child of %s whose spec states a requirement.\n\n' "$parent"
  issue_tree_child_view_requirement r-create "The second child states a requirement of its own at its create."
} >"$work/second-spec.md"
readonly second_title="Child view check: a second child holding a requirement $stamp"
tree_send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$second_title" --rawfile s "$work/second-spec.md" \
  --arg parent "$parent" --argjson a "$(tree_actor)" '{project: $p, title: $t, spec: $s, parent: $parent, force: true, actor: $a}')"
[[ "$status" != 201 ]] || keys+=("$(jq -r '.key' "$work/body")")
issue_tree_child_view_refused V2 "the create of a second child of $parent (POST /api/v1/issues)" r-create
tree_send GET "/api/v1/issues?parent=$(q "$parent")"
tree_want 200 V2 "list the children of $parent"
expect V2 "$parent holds $child and no second child" "$(jq -cn --arg c "$child" '[$c]')" '[.[].key]'

tree_send GET "/api/v1/artifacts/$child_spec/text"
tree_want 200 V2 "read $child's spec again"
after_version="$(jq -r '.version' "$work/body")"
jq -j '.markdown' "$work/body" >"$work/child-spec-after.md"
after_sum="$(sha256sum "$work/child-spec-after.md")"
after_sum="${after_sum%% *}"
[[ "$after_version" == "$before_version" && "$after_sum" == "$before_sum" ]] ||
  fail V2 "$child's spec is unchanged" "it stood at version $before_version, sha256 $before_sum, and stands at version $after_version, sha256 $after_sum"
pass V2 "$child's spec is unchanged: version $after_version, sha256 $after_sum"
issue_tree_child_view_requirement_rows V2 "$child_spec"
[[ -z "$answer" ]] || fail V2 "the store holds no requirement block of $child's spec" "it holds $answer"
pass V2 "the store holds no requirement block of $child's spec"
tree_read agent "$child" spec
tree_want 200 V2 "read the spec view of $child again"
expect V2 "the spec view of $child still answers $parent's r1 and no other requirement" \
  "$(jq -cn --arg p "$parent" '[{block_id: "r1", issue: $p}]')" \
  '[.nodes[] | select(.supertag == "requirement") | {block_id, issue: (.issue // null)}]'

# --- V3: the parent's coverage is all, and its record holds no node of the child -------------------

tree_send GET "/api/v1/issues/$parent/coverage"
tree_want 200 V3 "read the coverage of $parent"
expect V3 "the coverage of $parent reads r1 served by $child's check alone and r2 served by nothing" \
  "$(jq -cnS --arg c "$c1_node" --arg k "$child" '{r1: {checks: 1, proof: [{check: $c, issue: $k}]}, r2: {checks: 0, proof: []}}')" \
  '[.requirements[] | {key: .block_id, value: {checks, proof: [(.proof // [])[] | {check, issue}]}}] | from_entries'
expect V3 "the coverage of $parent names r2's missing check among its gaps" true 'any(.gaps[]; . == "r2 has no check")'

printf 'How %s proves its own part.\n\n:::check{#pc2 serves="dispatch://%s/spec#b-r2" surface="api" run="bash e2e/acceptance/issue-tree-child-view.sh" negative_control="Leave r2 unserved and the parent is not covered."}\nThe parent proves its second requirement.\n:::\n' \
  "$parent" "$parent" >"$work/parent-verification.md"
corpus_upload V3 "$parent" verification.md "$work/parent-verification.md"
readonly parent_verification="$corpus_doc"
issue_tree_child_view_drained V3 "$parent" "$child"
issue_tree_child_view_query V3 "$parent_verification"
readonly query_before="$answer"
pass V3 "$parent's verification query $parent_verification stands at version ${query_before%%|*}, digest ${query_before#*|}"
issue_tree_child_view_record V3 "$child" c1
issue_tree_child_view_drained V3 "$parent" "$child"
issue_tree_child_view_query V3 "$parent_verification"
[[ "$answer" == "$query_before" ]] ||
  fail V3 "a pass on $child's check leaves $parent's verification query at its version and digest" \
    "it stood at ${query_before/|/ with the digest } and stands at ${answer/|/ with the digest }"
pass V3 "a pass on $child's check leaves $parent's verification query at version ${answer%%|*}, digest ${answer#*|}"
issue_tree_child_view_record V3 "$parent" pc2
tree_send GET "/api/v1/issues/$parent/coverage"
tree_want 200 V3 "read the coverage of $parent again"
expect V3 "every requirement of $parent is covered by a passed check" '{"r1":"proved","r2":"proved"}' \
  '[.requirements[] | {key: .block_id, value: (.coverage // "no coverage member")}] | from_entries'

# --- V4: a child under a done ancestor stands on its own --------------------------------------------

{
  printf 'An issue that is done before its child states its work.\n\n'
  issue_tree_child_view_requirement rd1 "The done issue's requirement."
} >"$work/done-spec.md"
issue_tree_child_view_issue V4 "Child view check: an issue that is done $stamp" "$work/done-spec.md"
readonly done_issue="$corpus_key"
printf 'A child of %s, which is done before this child states its work.\n' "$done_issue" >"$work/own-spec.md"
issue_tree_child_view_issue V4 "Child view check: a child of an issue that is done $stamp" "$work/own-spec.md" "$done_issue"
readonly own="$corpus_key" own_spec="$corpus_spec"
tree_send PATCH "/api/v1/issues/$done_issue" "$(jq -cn --argjson a "$(tree_actor)" '{status: "done", actor: $a}')"
tree_want 200 V4 "close $done_issue"
expect V4 "$done_issue is done" '"done"' '.status'
# The run closed it, so its cleanup does not close it again.
open_keys=()
for issue in "${keys[@]}"; do
  [[ "$issue" == "$done_issue" ]] || open_keys+=("$issue")
done
keys=("${open_keys[@]}")
tree_send POST "/api/v1/artifacts/$own_spec/edits" "$(jq -cn \
  --arg m "$(issue_tree_child_view_requirement r-own "The child of a done issue states a requirement of its own.")" \
  --argjson a "$(tree_actor)" '{ops: [{op: "insert", markdown: $m, after: "end"}], actor: $a}')"
[[ "$status" == 200 ]] ||
  fail V4 "an edit carrying the requirement r-own into $own's spec is taken" "$tree_last_request answered HTTP $status: $(body)"
expect V4 "the edit carrying the requirement r-own into $own's spec is taken" true '.changed'
tree_send GET "/api/v1/issues/$own/artifacts/spec/blocks"
tree_want 200 V4 "read the blocks of $own's spec"
expect V4 "$own's spec holds the requirement r-own" '["requirement"]' '[.[] | select(.id == "r-own") | .type]'
tree_read agent "$own"
tree_want 200 V4 "read the tree of $own"
expect V4 "$own, a child of the done issue $done_issue, is no view" null '.view_of'

pass "$driver" "complete 4 sections"
