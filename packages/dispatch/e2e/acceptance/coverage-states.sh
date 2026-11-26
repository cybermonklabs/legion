#!/usr/bin/env bash
# The check c-coverage-states: the coverage read says what proves a requirement and what does
# not, it answers from the serves tuples and from no second copy, and a Verification document past
# the block bound writes its checks and their serves tuples at once. It runs against the server of
# `bun run dispatch:local`, on the database that server serves, and seeds every issue it reads.
#
#   S1 It creates an issue whose spec holds five requirements, r1 to r5, and writes its
#      Verification document: no check serves r1, c2 serves r2 and holds no result, c3 serves r3
#      and holds a fail, c4 serves r4 and holds a pass recorded before the driver edits c4's words,
#      and c5 serves r5 and holds a pass against its words as they stand.
#   S2 GET /api/v1/issues/{key}/coverage reads r1 as a gap, r2, r3 and r4 as unproven, and r5 as
#      proved. Where r3 or r4 reads proved, the failure names both requirements and the verdict and
#      text sum of the result the read counted for each.
#   S3 It retracts r5's result through POST /api/v1/results/{id}/retract, with a reason, and reads
#      r5 unproven, the result's graph_nodes row and its check_results row unchanged, and the reason
#      appended as a value of the result's `retracted` attribute.
#   S4 It retracts c2's serves tuple directly in graph_tuples and reads the coverage answer lose
#      that pairing on its next read; it appends the tuple again and reads the pairing back.
#   S5 It deletes c2 from the Verification document and reads r2 uncovered again.
#   S6 It creates a second issue whose spec holds two requirements, and writes its Verification
#      document of more than 2,000 blocks, which the write leaves to the reconciliation
#      (deferBlockIndexBlocks). Right after the write it reads both checks listed under their
#      requirements by the coverage read, each check's serves tuple in the store, and owed_documents
#      of the coverage read and of the tree read naming that document and its newest version; where
#      no check of the document is there, the failure names the document. It waits until
#      document_block_versions.version of that document is not null, every 2 s for at most 300 s,
#      then reads both checks in tree order in GET /api/v1/issues/{key}/tree, each with its serves
#      tuple, and no owed_documents in either read.
#
# Inputs: DATABASE_URL (the database the server serves, on this machine: the driver reads the store
# through psql and writes S4's two tuples there), DISPATCH_URL (the server), DISPATCH_TOKEN or
# DISPATCH_TOKEN_FILE (the agent bearer, read here and never printed) and PROJECT (an existing
# project key). Each unmet input is exit 2. The agent bearer writes every issue, document and
# result, and closes the issues when the run ends, also when a check fails.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line. The
# last line is `PASS: coverage-states.sh complete 6 sections`. Every check is `cmd || fail …`,
# never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE bash e2e/acceptance/coverage-states.sh
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
require_commands curl jq psql
# shellcheck disable=SC2031 # the run's own DATABASE_URL
loopback_database_url "$DATABASE_URL" || {
  printf 'coverage-states.sh: DATABASE_URL names no single loopback host; the driver writes tuples into the store it names\n' >&2
  usage
  exit 2
}
read_bearer

readonly driver=coverage-states.sh
# shellcheck disable=SC2031 # the caller's DISPATCH_URL; only lib.sh's `cli` exports another
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2031 # the run's own DATABASE_URL
readonly database_url="$DATABASE_URL"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="coverage-states-$stamp"
# The blocks the large Verification document holds past its two checks: more than the 2,000 a
# write indexes in its own request (deferBlockIndexBlocks in docs/reference_excerpts.go).
readonly filler_blocks=2100
# The reason S3's retraction gives, which the result's retracted value holds.
readonly retract_reason="coverage-states.sh retracts r5's pass: its run proved nothing $stamp"

work_dir
tree_init

keys=()
cleanup() {
  local code=$? issue
  trap - EXIT INT TERM
  for issue in "${keys[@]}"; do
    close_issue "$issue"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# coverage_states_serves SECTION CHECK REQUIREMENT: the one current serves tuple out of the check
# node CHECK names the requirement node REQUIREMENT.
coverage_states_serves() {
  tree_far_ends "$1" block serves "$2"
  [[ "$answer" == "block:$3" ]] ||
    fail "$1" "the store holds the serves tuple from $2 to $3" "the current serves tuples out of $2 name '$answer'"
  pass "$1" "the store holds the serves tuple from $2 to $3, and no other out of it"
}

# coverage_states_coverage SECTION ISSUE: reads GET /api/v1/issues/{key}/coverage of ISSUE, kept
# in $work/coverage.json.
coverage_states_coverage() {
  tree_send GET "/api/v1/issues/$(q "$2")/coverage"
  tree_want 200 "$1" "read the coverage of $2"
  cp "$work/body" "$work/coverage.json"
}

# coverage_states_states SECTION ISSUE WANT: the coverage read of ISSUE answers, for each of its
# requirements by block id, the coverage member WANT names, a JSON object of block id to state.
# Where r3 or r4 reads proved, the failure names both requirements and the verdict and the text sum
# of the result the read counted for each, held in result_verdict and result_sum by block id.
coverage_states_states() {
  local section="$1" issue="$2" want="$3" got counted="" requirement state
  coverage_states_coverage "$section" "$issue"
  got="$(jq -cS '[.requirements[] | {key: .block_id, value: (.coverage // "no coverage member")}] | from_entries' \
    "$work/coverage.json")" || fail "$section" "read the coverage of $issue" "unreadable: $(body)"
  for requirement in r3 r4; do
    state="$(jq -r --arg r "$requirement" '.[$r] // ""' <<<"$got")"
    [[ "$state" != proved ]] ||
      counted+="${counted:+; }$requirement reads proved, counting the $requirement check's result ${result_id[$requirement]:-} of verdict ${result_verdict[$requirement]:-} recorded against the text sum ${result_sum[$requirement]:-}"
  done
  [[ -z "$counted" ]] ||
    fail "$section" "a fail and a pass against words the check no longer holds are no proof" \
      "$counted; the coverage of $issue answers $got, want $want; proof: $(jq -c '[.requirements[] | {block_id, proof}]' "$work/coverage.json")"
  [[ "$got" == "$(jq -cS . <<<"$want")" ]] ||
    fail "$section" "the coverage of $issue reads each requirement as its proof stands" \
      "it answers $got, want $(jq -cS . <<<"$want"); proof: $(jq -c '[.requirements[] | {block_id, proof}]' "$work/coverage.json")"
  pass "$section" "the coverage of $issue reads $got"
}

# coverage_states_proof SECTION ISSUE REQUIREMENT WANT: the coverage read last kept lists, under
# the requirement REQUIREMENT, the check nodes WANT names, a JSON list of store ids, once each.
coverage_states_proof() {
  local got
  got="$(jq -cS --arg r "$3" '[.requirements[] | select(.block_id == $r) | (.proof // [])[] | .check] | sort' \
    "$work/coverage.json")" || fail "$1" "read the proof of $3" "unreadable: $(head -c 600 "$work/coverage.json")"
  [[ "$got" == "$(jq -cS 'sort' <<<"$4")" ]] ||
    fail "$1" "the coverage of $2 lists under $3 the checks $4" \
      "it lists $got: $(jq -c --arg r "$3" '[.requirements[] | select(.block_id == $r)]' "$work/coverage.json")"
  pass "$1" "the coverage of $2 lists under $3 the checks $got"
}

# coverage_states_row SECTION WHAT SQL NAME=VALUE…: the md5 of the one row SQL reads, in $answer,
# so a row is compared whole before and after a write.
coverage_states_row() {
  with_vars "$@"
  [[ -n "$answer" ]] || fail "$1" "$2" "no row"
}

declare -A result_id=() result_verdict=() result_sum=()
readonly commit="$corpus_commit_one"

# --- S1: five requirements, and what serves each --------------------------------------------------

cat >"$work/spec.md" <<'SPEC'
The spec whose five requirements the coverage read tells apart.

:::requirement{#r1}
The first requirement has no check at all.
:::

:::requirement{#r2}
The second requirement has a check that holds no result.
:::

:::requirement{#r3}
The third requirement has a check whose newest result failed.
:::

:::requirement{#r4}
The fourth requirement has a check whose pass was recorded against words it no longer holds.
:::

:::requirement{#r5}
The fifth requirement has a check that passed against the words it holds.
:::
SPEC
corpus_issue S1 "Coverage states check $stamp" "$work/spec.md"
readonly key="$corpus_key"
keys+=("$key")
[[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail S1 "create the issue" "the key $key is no issue key"
pass S1 "created $key in $project, whose spec holds r1 to r5"

{
  printf 'How the five requirements of %s are proved.\n\n' "$key"
  for number in 2 3 4 5; do
    printf ':::check{#c%d serves="dispatch://%s/spec#b-r%d" surface="api" run="bash e2e/acceptance/coverage-states.sh" negative_control="Count any result as proof and r%d reads otherwise."}\nThe check of requirement %d holds these words.\n:::\n\n' \
      "$number" "$key" "$number" "$number" "$number"
  done
} >"$work/verification.md"
corpus_upload S1 "$key" verification.md "$work/verification.md"
readonly verification="$corpus_doc"
tree_send GET "/api/v1/issues/$key/artifacts/verification-md/blocks"
tree_want 200 S1 "read the blocks of $key's verification.md"
expect S1 "verification.md of $key holds the checks c2 to c5" '["check","check","check","check"]' \
  '[.[] | select(.id == "c2" or .id == "c3" or .id == "c4" or .id == "c5") | .type]'

# coverage_states_record SECTION CHECK VERDICT REQUIREMENT: records a result of CHECK and keeps its
# id, its verdict and the text sum it was recorded against under REQUIREMENT.
coverage_states_record() {
  corpus_result "$1" "$key" "$2" "$3" "$commit" tester
  result_id[$4]="$tree_value"
  result_verdict[$4]="$(jq -r '.result.verdict' "$work/body")"
  result_sum[$4]="$(jq -r '.result.check_sha256' "$work/body")"
  pass "$1" "recorded ${result_verdict[$4]} ${result_id[$4]} of $2 against the text sum ${result_sum[$4]}"
}
coverage_states_record S1 c3 fail r3
coverage_states_record S1 c4 pass r4
corpus_edit S1 "edit the words of c4 after its result" "$verification" \
  "$(jq -cn '[{op: "replace", find: "The check of requirement 4 holds these words.", with: "The check of requirement 4 holds other words now."}]')"
coverage_states_record S1 c5 pass r5

tree_block_node S1 "$verification" c4
[[ -n "$tree_block_node_id" ]] || fail S1 "read the node of c4" "no document_blocks row holds c4 of $verification"
with_vars S1 "read the text sum c4 holds now" \
  "select text_sha256 from document_blocks where node_id = :'node'" node="$tree_block_node_id"
[[ -n "$answer" && "$answer" != "${result_sum[r4]}" ]] ||
  fail S1 "c4 holds words other than the ones its pass was recorded against" \
    "its text sum is '$answer', its pass's ${result_sum[r4]}"
pass S1 "c4 holds the text sum $answer, and its pass names ${result_sum[r4]}"

# --- S2: a gap, three unproven, one proved ----------------------------------------------------------

coverage_states_states S2 "$key" '{"r1":"gap","r2":"unproven","r3":"unproven","r4":"unproven","r5":"proved"}'

# --- S3: a retracted pass is no result ---------------------------------------------------------------

readonly retracted="${result_id[r5]}"
[[ "$retracted" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] ||
  fail S3 "read the id of r5's result" "$retracted is no uuid"
coverage_states_row S3 "read the node of $retracted" \
  "select md5(row(n.*)::text) from graph_nodes n where n.kind = 'result' and n.id = :'id'" id="$retracted"
readonly node_before="$answer"
coverage_states_row S3 "read the check_results row of $retracted" \
  "select md5(row(c.*)::text) from check_results c where c.id = :'id'::uuid" id="$retracted"
readonly row_before="$answer"
tree_send POST "/api/v1/results/$retracted/retract" \
  "$(jq -cn --arg r "$retract_reason" --argjson a "$(tree_actor)" '{reason: $r, actor: $a}')"
tree_want 201 S3 "retract $retracted"
pass S3 "retracted $retracted, r5's pass, with a reason"
coverage_states_states S3 "$key" '{"r1":"gap","r2":"unproven","r3":"unproven","r4":"unproven","r5":"unproven"}'
coverage_states_row S3 "read the node of $retracted again" \
  "select md5(row(n.*)::text) from graph_nodes n where n.kind = 'result' and n.id = :'id'" id="$retracted"
[[ "$answer" == "$node_before" ]] ||
  fail S3 "the result node $retracted is unchanged by its retraction" "its row read $node_before before and $answer after"
pass S3 "the result node $retracted is unchanged by its retraction"
coverage_states_row S3 "read the check_results row of $retracted again" \
  "select md5(row(c.*)::text) from check_results c where c.id = :'id'::uuid" id="$retracted"
[[ "$answer" == "$row_before" ]] ||
  fail S3 "the check_results row of $retracted is unchanged by its retraction" "it read $row_before before and $answer after"
pass S3 "the check_results row of $retracted is unchanged by its retraction"
with_vars S3 "read the retracted values of $retracted" \
  "select count(*) filter (where to_id = :'reason') || '|' || count(*) from graph_tuples
   where from_kind = 'result' and from_id = :'id' and kind = 'retracted' and to_kind = 'value' and asserted" \
  id="$retracted" reason="$retract_reason"
[[ "$answer" == "1|1" ]] ||
  fail S3 "the result node $retracted holds its reason as the one value of its retracted attribute" \
    "of its retracted values, those holding the reason and all of them read $answer, want 1|1"
pass S3 "the result node $retracted holds its reason as the one value of its retracted attribute"

# --- S4: the answer follows the serves tuple ---------------------------------------------------------

tree_block_node S4 "$verification" c2
readonly c2_node="$tree_block_node_id"
[[ -n "$c2_node" ]] || fail S4 "read the node of c2" "no document_blocks row holds c2 of $verification"
tree_send GET "/api/v1/issues/$key"
tree_want 200 S4 "read $key"
tree_field S4 "read $key" .primary_artifact_id
readonly spec="$tree_value"
tree_block_node S4 "$spec" r2
readonly r2_node="$tree_block_node_id"
[[ -n "$r2_node" ]] || fail S4 "read the node of r2" "no document_blocks row holds r2 of $spec"
coverage_states_serves S4 "$c2_node" "$r2_node"
coverage_states_coverage S4 "$key"
coverage_states_proof S4 "$key" r2 "$(jq -cn --arg c "$c2_node" '[$c]')"

# coverage_states_state_serves SECTION ASSERTED: appends to graph_tuples, with no event, the serves
# tuple from c2 to r2, asserted or retracted as ASSERTED says, as the store records any fact: a
# row, never an update.
coverage_states_state_serves() {
  with_vars "$1" "append the serves tuple from $c2_node to $r2_node, asserted $2" \
    "insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
     values ('block', :'from', 'serves', 'block', :'to', clock_timestamp(), :'who'::jsonb, 0, :'asserted'::boolean)
     returning seq" \
    from="$c2_node" to="$r2_node" who="$(tree_actor)" asserted="$2"
  pass "$1" "appended to graph_tuples the serves tuple from $c2_node to $r2_node, asserted $2, at seq $answer"
}
coverage_states_state_serves S4 false
coverage_states_coverage S4 "$key"
coverage_states_proof S4 "$key" r2 '[]'
coverage_states_states S4 "$key" '{"r1":"gap","r2":"gap","r3":"unproven","r4":"unproven","r5":"unproven"}'
coverage_states_state_serves S4 true
coverage_states_coverage S4 "$key"
coverage_states_proof S4 "$key" r2 "$(jq -cn --arg c "$c2_node" '[$c]')"
coverage_states_states S4 "$key" '{"r1":"gap","r2":"unproven","r3":"unproven","r4":"unproven","r5":"unproven"}'

# --- S5: a deleted check serves nothing --------------------------------------------------------------

corpus_edit S5 "delete c2 from $key's verification.md" "$verification" \
  "$(jq -cn '[{op: "delete", block: "c2"}]')"
pass S5 "deleted c2 from $key's verification.md"
coverage_states_states S5 "$key" '{"r1":"gap","r2":"gap","r3":"unproven","r4":"unproven","r5":"unproven"}'
coverage_states_proof S5 "$key" r2 '[]'

# --- S6: a Verification document past the block bound ------------------------------------------------

cat >"$work/large-spec.md" <<'SPEC'
The spec whose two requirements a Verification document past the block bound proves.

:::requirement{#r1}
The first requirement of the large document's issue.
:::

:::requirement{#r2}
The second requirement of the large document's issue.
:::
SPEC
corpus_issue S6 "Coverage states check, a Verification document past the block bound $stamp" "$work/large-spec.md"
readonly large="$corpus_key" large_spec="$corpus_spec"
keys+=("$large")
[[ "$large" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail S6 "create the issue" "the key $large is no issue key"
{
  printf 'How the two requirements of %s are proved.\n\n' "$large"
  printf ':::check{#cb1 serves="dispatch://%s/spec#b-r1" surface="api" run="bash e2e/acceptance/coverage-states.sh" negative_control="Skip the typed pass and the check is not read."}\nThe first check of the large document.\n:::\n\n' "$large"
  printf ':::check{#cb2 serves="dispatch://%s/spec#b-r2" surface="api" run="bash e2e/acceptance/coverage-states.sh" negative_control="Skip the typed pass and the check is not read."}\nThe second check of the large document.\n:::\n\n' "$large"
  corpus_filler verification.md "$filler_blocks"
} >"$work/large-verification.md"
corpus_upload S6 "$large" verification.md "$work/large-verification.md"
readonly large_verification="$corpus_doc"
corpus_version S6 "$large_verification"
readonly large_version="$tree_value"
pass S6 "wrote $large's verification.md, $large_verification, at version $large_version: two checks and $filler_blocks blocks more"

# Right after the write, with no wait.
tree_block_node S6 "$large_verification" cb1
readonly cb1_node="$tree_block_node_id"
tree_block_node S6 "$large_verification" cb2
readonly cb2_node="$tree_block_node_id"
[[ -n "$cb1_node" && -n "$cb2_node" ]] ||
  fail S6 "the write of verification.md $large_verification of $large writes its checks at once" \
    "right after the write no document_blocks row holds cb1 ('$cb1_node') or cb2 ('$cb2_node') of $large_verification"
tree_block_node S6 "$large_spec" r1
readonly lr1_node="$tree_block_node_id"
tree_block_node S6 "$large_spec" r2
readonly lr2_node="$tree_block_node_id"
[[ -n "$lr1_node" && -n "$lr2_node" ]] ||
  fail S6 "read the requirement nodes of $large" "no document_blocks row holds r1 ('$lr1_node') or r2 ('$lr2_node') of $large_spec"
coverage_states_coverage S6 "$large"
listed="$(jq -c '[(.requirements // [])[] | (.proof // [])[] | .check] | sort' "$work/coverage.json")" ||
  fail S6 "read the coverage of $large" "unreadable: $(head -c 600 "$work/coverage.json")"
[[ "$listed" == "$(jq -cn --arg a "$cb1_node" --arg b "$cb2_node" '[$a, $b] | sort')" ]] ||
  fail S6 "right after the write the coverage read lists the checks of verification.md $large_verification of $large" \
    "it lists $listed: $(head -c 600 "$work/coverage.json")"
coverage_states_proof S6 "$large" r1 "$(jq -cn --arg c "$cb1_node" '[$c]')"
coverage_states_proof S6 "$large" r2 "$(jq -cn --arg c "$cb2_node" '[$c]')"
coverage_states_serves S6 "$cb1_node" "$lr1_node"
coverage_states_serves S6 "$cb2_node" "$lr2_node"
owed_entry="$(jq -cn --arg a "$large_verification" --argjson v "$large_version" '{artifact_id: $a, version: $v}')"
readonly owed_entry
jq -e --argjson o "$owed_entry" 'any((.owed_documents // [])[]; . == $o)' "$work/coverage.json" >/dev/null ||
  fail S6 "right after the write the coverage read names $large_verification owed at version $large_version" \
    "its owed_documents are $(jq -c '.owed_documents' "$work/coverage.json")"
pass S6 "right after the write the coverage read names $large_verification owed at version $large_version"
tree_read agent "$large"
tree_want 200 S6 "read the tree of $large"
jq -e --argjson o "$owed_entry" 'any((.owed_documents // [])[]; . == $o)' "$work/body" >/dev/null ||
  fail S6 "right after the write the tree read names $large_verification owed at version $large_version" \
    "its owed_documents are $(jq -c '.owed_documents' "$work/body")"
pass S6 "right after the write the tree read names $large_verification owed at version $large_version"

deadline=$((SECONDS + tree_wait_seconds))
while :; do
  with_vars S6 "read the indexed version of $large_verification" \
    "select clock_timestamp() || '|' || coalesce((select coalesce(version::text, 'null') from document_block_versions where artifact_id = :'doc'::uuid), 'no row')" \
    doc="$large_verification"
  printf 'document_block_versions.version of %s at %s\n' "$large_verification" "${answer/|/ is }"
  [[ "${answer#*|}" == null || "${answer#*|}" == "no row" ]] || break
  ((SECONDS < deadline)) ||
    fail S6 "the reconciliation indexes $large_verification within $tree_wait_seconds s" "at ${answer%%|*} its version is ${answer#*|}"
  sleep "$tree_poll_seconds"
done
pass S6 "the reconciliation indexed $large_verification at version ${answer#*|}"

tree_read agent "$large"
tree_want 200 S6 "read the tree of $large once it is indexed"
expect S6 "the tree read of $large answers cb1 and cb2 in tree order" \
  "$(jq -cn --arg a "$cb1_node" --arg b "$cb2_node" '[$a, $b]')" \
  '[.nodes[] | select(.kind == "block" and .supertag == "check" and (.issue // "") == "") | .id]'
expect S6 "the tree read of $large answers each check's serves tuple" \
  "$(jq -cnS --arg a "$cb1_node" --arg b "$cb2_node" --arg r "$lr1_node" --arg s "$lr2_node" '[[$a, $r], [$b, $s]]')" \
  '[.tuples[] | select(.kind == "serves") | [.from.id, .to.id]] | sort'
expect S6 "the tree read of $large names no owed document" null '.owed_documents'
coverage_states_coverage S6 "$large"
coverage_states_proof S6 "$large" r1 "$(jq -cn --arg c "$cb1_node" '[$c]')"
coverage_states_proof S6 "$large" r2 "$(jq -cn --arg c "$cb2_node" '[$c]')"
jq -e '.owed_documents == null' "$work/coverage.json" >/dev/null ||
  fail S6 "the coverage read of $large names no owed document" "its owed_documents are $(jq -c '.owed_documents' "$work/coverage.json")"
pass S6 "the coverage read of $large names no owed document"

pass "$driver" "complete 6 sections"
