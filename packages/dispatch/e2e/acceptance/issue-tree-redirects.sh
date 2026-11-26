#!/usr/bin/env bash
# The check c-redirects: no citation of a moved node dangles, and no migration rewrites a pointer
# (spec requirement r-move-safe).
#
# It empties the database DATABASE_URL names (infra_1253_upgrade on this machine, dropped and
# created again) and boots the release before this branch on it through lib-baseline.sh
# (BASELINE_SHA or BASELINE_BINARY). Through that server's API it writes the main issue, whose
# Verification document (two versions) holds the check c-target and the ask block a-verify, and
# whose notes.md cites dispatch://<main>/artifact/verification-md#b-c-target (a refs row and a
# block_refs row from the citation) and the address dispatch://<main>/artifact/notes-md#b-q-moved;
# an ask with block_artifact_id set on that document (a-verify's row); a comment whose anchor
# points at the document, at a quote of c-target; and a result on c-target (its check_results row
# holds the check_artifact_id and check_block_id pair). On a second issue, whose closure the
# closure leg reads (an issue's closure leaves its own documents out), it writes a comment citing
# c-target, a comment citing notes-md#b-q-moved, and one prose citation of a block no version
# holds, dispatch://<main>/artifact/verification-md#b-c-nowhere.
#
# Through that server it records what each resolver answers for the citation of c-target: the
# reference read (GET /api/v1/references?to=, blockReferenceNode in api/references.go), the same
# reference pinned to the Verification document's newest version (`@v<n>`), the block-link read
# (GET /api/v1/block-links?from=<notes.md>&to=<verification.md>, refs.BlockLinks), the second
# issue's reference closure (GET /api/v1/issues/{key}/references, refs.Closure), and the backlink
# read of the same address; and the five pointer columns, refs.to_id, block_refs.to_id,
# asks.block_artifact_id, comments.anchor and the check_results pair. It stops that server, boots
# this head over the same database with the hold of R6's step, and waits for the backfill. Before
# the move it reads the five pointer columns as it captured them, so no migration rewrote one.
#
# The release before this branch writes no decisions document, so no move of the restore of the
# decisions step can be written through it: the driver appends, through psql, one moved_to tuple
# of the shape that restore writes (graphstore.TupleMovedTo, from a block address to a block, with
# the time of the move) from notes-md#q-moved to the node of a-verify, and resolves the second
# issue's citation of it through each resolver before the first node moves.
#
# Then `envoy-dispatch census --store`: its `census: dangling_citations moved=<n> returning=<n>`
# line, its `unresolved_citations n=<n>` line, which reads 1 or more (the planted citation of
# c-nowhere), and its `census: versions …` line, which must read tree_mismatch=0 and
# tree_mismatch_repaired=0, or the run fails printing that line. It waits for this head's build
# announcement and converts with `envoy-dispatch convert-issues --project PROJECT`, which moves
# the Verification document's blocks into the issue's tree, and reads the citation again four ways:
# each resolver reaches the block at its new address, the far end of the one moved_to tuple that
# records the move (no second row for it), and answers the text captured before the upgrade; the
# backlink read answers the same links. Each of the five pointers is a tuple of the store after the
# move, from its owner to the node's new home, and each projection column names that home. Last
# `census --store` again, after the wait for every document's block index: the dangling line and
# `n` read as before the move, and the versions line reads tree_mismatch=0 and
# tree_mismatch_repaired=0.
#
# Inputs: DATABASE_URL (infra_1253_upgrade on this machine), DISPATCH_TOKEN (e2e-token), PROJECT
# (CORE), DEV_SIGNIN_LOGIN (optional: the person the project's create names as its default assignee,
# alice where unset, as the Verification preamble signs her in), BASELINE_SHA or BASELINE_BINARY,
# and `envoy-dispatch` on PATH, built stamped with this head's commit.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. The last line is `PASS: issue-tree-redirects.sh complete <N> sections`.
# Every check is `cmd || fail …`, never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1253_upgrade DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         [DEV_SIGNIN_LOGIN=alice] BASELINE_SHA=<the commit of the release this branch lands after> \
         bash e2e/acceptance/issue-tree-redirects.sh
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
require_commands curl date diff grep jq psql sed
readonly driver=issue-tree-redirects.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
session="issue-tree-redirects-$(date +%s)-$$"
readonly session
# The words of c-target that the anchored comment quotes and every resolver answers.
readonly target_words="The redirect target check holds its words."

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

# issue_tree_redirects_resolve SECTION FILE NODE: what each resolver answers for the citation of
# c-target, written to FILE as one JSON object: `reference` and `pinned`, the node GET
# /api/v1/references?to= answers for the address and for the address pinned to version
# $pinned_version (its id and its text); `backlinks`, the edges that read answers (each edge's kind
# and its other end's kind, and the ids of the other ends); `block_links`, the links GET
# /api/v1/block-links answers from notes.md to the block whose node id is NODE (its id and its
# text); `closure`, the second issue's links to that node (its id, its name and its text).
issue_tree_redirects_resolve() {
  local section="$1" file="$2" node="$3"
  tree_send GET "/api/v1/references?to=$(q "$target_address")"
  tree_want 200 "$section" "resolve $target_address through the reference read"
  cp "$work/body" "$work/resolve.reference"
  tree_send GET "/api/v1/references?to=$(q "$target_address@v$pinned_version")"
  tree_want 200 "$section" "resolve $target_address@v$pinned_version through the reference read pinned to a version"
  cp "$work/body" "$work/resolve.pinned"
  tree_send GET "/api/v1/block-links?from=$(q "dispatch://$main/artifact/notes-md")&to=$(q "dispatch://$main/artifact/verification-md")"
  tree_want 200 "$section" "read the block links from notes.md to verification.md of $main"
  cp "$work/body" "$work/resolve.links"
  tree_send GET "/api/v1/issues/$other/references"
  tree_want 200 "$section" "read the reference closure of $other"
  cp "$work/body" "$work/resolve.closure"
  jq -n --arg node "$node" --slurpfile reference "$work/resolve.reference" --slurpfile pinned "$work/resolve.pinned" \
    --slurpfile links "$work/resolve.links" --slurpfile closure "$work/resolve.closure" '{
      reference: ($reference[0].node | {id, text: (.text // "")}),
      pinned: ($pinned[0].node | {id, text: (.text // "")}),
      backlinks: {kinds: ([$reference[0].edges[] | "\(.kind) \(.node.kind)"] | sort),
                  ids: ([$reference[0].edges[] | .node.id] | sort)},
      block_links: [$links[0].links[] | select(.to.id == $node) | {id: .to.id, text: (.to.text // "")}],
      closure: [$closure[0].links[] | select(.node.id == $node) | {id: .node.id, name, text: (.node.text // "")}]
    }' >"$file" || fail "$section" "read what each resolver answers for $target_address" \
    "the answers do not read: $(head -c 300 "$work/resolve.reference")"
  printf 'resolvers of %s: %s\n' "$target_address" "$(jq -c . "$file")"
}

# issue_tree_redirects_pointers SECTION FILE: the five pointer columns, one line each, in FILE:
# the refs and block_refs rows of the citations of c-target, by the owners refs_owners and
# block_refs_owners name, and the rows of the ask, the anchored comment and the result, by their
# ids.
issue_tree_redirects_pointers() {
  query "$1" "read the five pointer columns" "
    select concat_ws(E'\n',
      (select string_agg('refs ' || from_kind || ' ' || from_id || ' ' || to_kind || ' ' || to_id, E'\n' order by from_kind, from_id)
         from refs where (from_kind, from_id) in ($refs_owners)),
      (select string_agg('block_refs ' || artifact_id || ' ' || block_id || ' ' || from_node || ' ' || to_kind || ' ' || to_id, E'\n' order by artifact_id, block_id)
         from block_refs where (artifact_id, block_id) in ($block_refs_owners)),
      (select 'asks ' || id || ' ' || coalesce(block_artifact_id::text, 'null') || ' ' || coalesce(block_id, 'null') from asks where id = '$ask'),
      (select 'comments ' || id || ' ' || coalesce(anchor::text, 'null') from comments where id = '$anchored'),
      (select 'check_results ' || id || ' ' || coalesce(check_artifact_id::text, 'null') || ' ' || coalesce(check_block_id, 'null')
         from check_results where id = '$result'))"
  printf '%s\n' "$answer" >"$2"
}

# issue_tree_redirects_census SECTION: `envoy-dispatch census --store` over the database, which
# exits 0 or 1 (a refusal, whose reasons are other classes than the lines read here); its
# dangling_citations line in `dangling`, its unresolved_citations count in `unresolved`; and its
# `census: versions` line read by lib-tree.sh's tree_census_versions, which fails printing that
# line unless it reads tree_mismatch=0 and tree_mismatch_repaired=0.
issue_tree_redirects_census() {
  local section="$1" line
  tree_envoy_dispatch_status census --store
  [[ "$tree_envoy_status" == 0 || "$tree_envoy_status" == 1 ]] ||
    fail "$section" "envoy-dispatch census --store is taken" "it exited $tree_envoy_status: $(tail -n 20 "$work/out")"
  cp "$work/out" "$work/census.$section"
  grep -E '^census: dangling_citations|unresolved_citations' "$work/census.$section" | sed 's/^/  /' || true
  line="$(grep -m 1 '^census: dangling_citations ' "$work/census.$section")" ||
    fail "$section" "census --store prints its dangling_citations line" "it prints none: $(tail -n 20 "$work/census.$section")"
  [[ "$line" =~ moved=([0-9,]+)\ returning=([0-9,]+) ]] ||
    fail "$section" "census --store prints its dangling_citations line" "the line reads: $line"
  dangling="moved=${BASH_REMATCH[1]} returning=${BASH_REMATCH[2]}"
  line="$(grep -m 1 -E 'unresolved_citations n=[0-9,]+' "$work/census.$section")" ||
    fail "$section" "census --store prints its unresolved_citations line" "it prints none: $(tail -n 20 "$work/census.$section")"
  [[ "$line" =~ unresolved_citations\ n=([0-9,]+) ]] ||
    fail "$section" "census --store prints its unresolved_citations line" "the line reads: $line"
  unresolved="${BASH_REMATCH[1]//,/}"
  tree_census_versions "$section" "each run of census --store reads tree_mismatch=0 and tree_mismatch_repaired=0" "" \
    "$work/census.$section"
  pass "$section" "census --store reads dangling_citations $dangling and unresolved_citations n=$unresolved"
}

# issue_tree_redirects_tuple SECTION WHAT FROM_KIND FROM_ID TO_ID: a tuple the store holds now from
# the node FROM_KIND FROM_ID to the node TO_ID: the newest tuple of some kind between the two
# asserts.
issue_tree_redirects_tuple() {
  query "$1" "read the tuples from $3 $4 to $5" "
    select count(*) from (
      select distinct on (t.kind) t.kind, t.asserted from graph_tuples t
      where t.from_kind = '$3' and t.from_id = '$4' and t.to_id = '$5'
      order by t.kind, t.seq desc
    ) newest where asserted"
  ((answer > 0)) || fail "$1" "$2 is a tuple of the store" "no current tuple runs from $3 $4 to $5"
}

# --- setup and the corpus, through the earlier release ----------------------------------------------

tree_reset_database "$database_url" infra_1253_upgrade setup
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The citations the move keeps"

section=corpus
printf 'The main issue, whose Verification document moves.\n\n:::requirement{#r-main}\nThe main issue keeps every citation.\n:::\n' \
  >"$work/main-spec.md"
corpus_issue "$section" "The issue whose Verification document moves into its tree" "$work/main-spec.md"
readonly main="$corpus_key"
[[ "$main" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$section" "create the main issue" "the key $main is no issue key"
{
  printf 'How the main issue is proved.\n\n'
  printf ':::check{#c-target serves="dispatch://%s/spec#b-r-main" surface="cli" run="bash e2e/acceptance/issue-tree-redirects.sh" negative_control="Leave a moved_to tuple out and the citation dangles."}\n%s\n:::\n\n' \
    "$main" "$target_words"
  printf ':::ask{#a-verify urgency="low"}\nWhich store does the target check read?\n:::\n\n'
  corpus_shapes verification.md main-verification
} >"$work/main-verification.md"
corpus_upload "$section" "$main" verification.md "$work/main-verification.md"
readonly verification="$corpus_doc"
corpus_revise "$section" "$verification" verification.md 2
printf '# Notes\n\nThe notes cite dispatch://%s/artifact/verification-md#b-c-target.\n\nThe moved question was dispatch://%s/artifact/notes-md#b-q-moved.\n' \
  "$main" "$main" >"$work/main-notes.md"
corpus_upload "$section" "$main" notes.md "$work/main-notes.md"
readonly notes="$corpus_doc"
corpus_wait_settled "$section"
corpus_version "$section" "$verification"
readonly pinned_version="$tree_value"
query "$section" "read the ask of the block a-verify" \
  "select coalesce((select id::text from asks where block_artifact_id = '$verification' and block_id = 'a-verify'), '')"
readonly ask="$answer"
[[ -n "$ask" ]] || fail "$section" "the ask block a-verify of verification.md has its ask row" "asks holds none"
tree_send POST "/api/v1/issues/$main/comments" "$(jq -cn --arg v "$verification" --arg q "$target_words" --argjson a "$(tree_actor)" \
  '{body: "The quoted check is the one that moves.", anchor: {artifact: $v, quote: $q}, actor: $a}')"
tree_want 201 "$section" "write a comment anchored at a quote of c-target"
tree_field "$section" "write a comment anchored at a quote of c-target" .id
readonly anchored="$tree_value"
corpus_result "$section" "$main" c-target pass "$corpus_commit_one" tester
readonly result="$tree_value"
printf 'An issue whose comments cite the main issue.\n' >"$work/other-spec.md"
corpus_issue "$section" "The issue whose closure reads the citation" "$work/other-spec.md"
readonly other="$corpus_key"
corpus_comment agent "$section" "$other" "The check that moves is dispatch://$main/artifact/verification-md#b-c-target."
readonly cite_comment="$corpus_item"
corpus_comment agent "$section" "$other" "The moved question is dispatch://$main/artifact/notes-md#b-q-moved."
corpus_comment agent "$section" "$other" "A citation of dispatch://$main/artifact/verification-md#b-c-nowhere, which no version holds."
corpus_wait_settled "$section"
query "$section" "read the node ids of the cited blocks" \
  "select (select ref_key from artifacts where id = '$verification') || ' ' || (select ref_key from artifacts where id = '$notes')"
read -r verification_ref notes_ref <<<"$answer"
readonly target_node="$verification_ref#c-target" ask_node="$verification_ref#a-verify" moved_node="$notes_ref#q-moved"
readonly target_address="dispatch://$main/artifact/verification-md#b-c-target"
readonly moved_address="dispatch://$main/artifact/notes-md#b-q-moved"
pass "$section" "wrote $main (verification.md $verification at version $pinned_version, notes.md $notes, the ask $ask, the anchored comment $anchored, the result $result) and $other (the citation $cite_comment)"
# The owners of the refs and block_refs rows of the citations of c-target, as the earlier release
# wrote them: each pointer read after this one reads these rows by their owner, wherever they
# point.
query "$section" "read the owners of the refs rows of the citations of c-target" \
  "select coalesce(string_agg('(' || quote_literal(from_kind) || ', ' || quote_literal(from_id) || ')', ', '), '')
   from refs where to_id = '$target_node'"
[[ -n "$answer" ]] || fail "$section" "the earlier release wrote the refs pointer of a citation of c-target" "refs holds no row to $target_node"
readonly refs_owners="$answer"
query "$section" "read the owners of the block_refs rows of the citations of c-target" \
  "select coalesce(string_agg('(' || quote_literal(artifact_id::text) || '::uuid, ' || quote_literal(block_id) || ')', ', '), '')
   from block_refs where to_id = '$target_node'"
[[ -n "$answer" ]] || fail "$section" "the earlier release wrote the block_refs pointer of a citation of c-target" "block_refs holds no row to $target_node"
readonly block_refs_owners="$answer"
issue_tree_redirects_resolve "$section" "$work/resolved.captured" "$target_node"
issue_tree_redirects_pointers "$section" "$work/pointers.captured"
sed 's/^/captured pointer: /' "$work/pointers.captured"
for pointer in refs block_refs asks comments check_results; do
  grep -q "^$pointer " "$work/pointers.captured" ||
    fail "$section" "the earlier release wrote the $pointer pointer" "the rows hold none: $(tr '\n' ';' <"$work/pointers.captured")"
done
jq -e --arg n "$target_node" --arg w "$target_words" \
  '[.reference, .pinned, .block_links[0], .closure[0]] | all(. != null and .id == $n and ((.text | contains($w)) or ((.name // "") | contains($w))))' \
  "$work/resolved.captured" >/dev/null ||
  fail "$section" "each resolver of the earlier release answers c-target at $target_node with its words" "$(<"$work/resolved.captured")"
pass "$section" "captured what each resolver answers for $target_address and the five pointer columns, through the earlier release"
baseline_stop_server

# --- upgrade: before the first move ------------------------------------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
issue_tree_redirects_pointers "$section" "$work/pointers.upgraded"
differs="$(diff "$work/pointers.captured" "$work/pointers.upgraded")" || true
[[ -z "$differs" ]] ||
  fail "$section" "no migration rewrote a pointer: the five columns read as captured" "they differ (< captured, > now): $(tr '\n' ';' <<<"$differs")"
pass "$section" "no migration rewrote a pointer: the five pointer columns read as captured"
issue_tree_redirects_resolve "$section" "$work/resolved.upgraded" "$target_node"
jq -e --arg n "$target_node" --arg w "$target_words" \
  '[.reference, .pinned, .block_links[0], .closure[0]] | all(. != null and .id == $n and ((.text | contains($w)) or ((.name // "") | contains($w))))' \
  "$work/resolved.upgraded" >/dev/null ||
  fail "$section" "every resolver answers c-target at $target_node with its words before the move" "$(<"$work/resolved.upgraded")"
pass "$section" "every resolver answers c-target at $target_node with its words before the move"
# One move of the restore's shape, which the release before cannot write, resolved through each
# resolver before the first node of this child moves.
query "$section" "append a moved_to tuple of the restore's shape from $moved_node to $ask_node" "
  insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
  values ('block', '$moved_node', 'moved_to', 'block', '$ask_node', clock_timestamp(),
          jsonb_build_object('kind', 'session', 'id', '$session'), 0)
  returning seq"
pass "$section" "appended the moved_to tuple $answer from $moved_node to $ask_node"
tree_send GET "/api/v1/references?to=$(q "$moved_address")"
tree_want 200 "$section" "resolve $moved_address through the reference read"
expect "$section" "the reference read resolves the moved address $moved_address to $ask_node" "$(jq -cn --arg v "$ask_node" '$v')" '.node.id'
tree_send GET "/api/v1/block-links?from=$(q "dispatch://$main/artifact/notes-md")&to=$(q "dispatch://$main/artifact/verification-md")"
tree_want 200 "$section" "read the block links from notes.md to verification.md of $main"
expect "$section" "the block-link read resolves the moved address to $ask_node" true --arg n "$ask_node" '[.links[] | select(.to.id == $n)] | length >= 1'
tree_send GET "/api/v1/issues/$other/references"
tree_want 200 "$section" "read the reference closure of $other"
expect "$section" "the closure of $other resolves the moved address to $ask_node" true --arg n "$ask_node" '[.links[] | select(.node.id == $n)] | length >= 1'
issue_tree_redirects_census "$section"
readonly dangling_before="$dangling" unresolved_before="$unresolved"
((unresolved_before >= 1)) ||
  fail "$section" "unresolved_citations reads 1 or more before the move, the planted citation of c-nowhere among them" "it reads $unresolved_before"
pass "$section" "unresolved_citations reads $unresolved_before before the move, the planted citation of c-nowhere among them"

# --- convert ------------------------------------------------------------------------------------------

section=convert
tree_wait_announcement "$section"
tree_convert "$section"
sed 's/^/  /' "$work/out"
query "$section" "read the moved_to rows from $target_node" "
  select count(*) || ' ' || coalesce((select to_kind || ' ' || to_id || ' ' || at from graph_tuples
    where from_kind = 'block' and from_id = '$target_node' and kind = 'moved_to' and asserted order by seq desc limit 1), 'none none none')
  from graph_tuples where from_kind = 'block' and from_id = '$target_node' and kind = 'moved_to'"
read -r rows to_kind new_node moved_at <<<"$answer"
[[ "$rows" == 1 && "$to_kind" == block ]] ||
  fail "$section" "one moved_to tuple records the move of $target_node" "graph_tuples holds $rows rows from it, the newest asserted to $to_kind $new_node"
pass "$section" "one moved_to tuple records the move of $target_node to $new_node at $moved_at, and no second row"
query "$section" "read the new home of a-verify and the main issue's tree" "
  select coalesce((select to_id from graph_tuples where from_kind = 'block' and from_id = '$ask_node' and kind = 'moved_to'
                   and asserted order by seq desc limit 1), 'none')
         || ' ' || coalesce((select artifact_id::text from issue_trees where issue_key = '$main'), 'none')"
read -r new_ask_node tree_artifact <<<"$answer"
[[ "$new_ask_node" != none && "$tree_artifact" != none ]] ||
  fail "$section" "the move gives a-verify a new home in the tree of $main" "a-verify moved to $new_ask_node, and the issue's tree is $tree_artifact"
readonly new_node new_ask_node tree_artifact

# --- redirects: the citation after the move -------------------------------------------------------------

section=redirects
issue_tree_redirects_resolve "$section" "$work/resolved.moved" "$new_node"
for leg in reference pinned block_links closure; do
  # The one answer of each resolver for the node: the node itself, or the one link to it.
  pick=".$leg"
  [[ "$leg" == reference || "$leg" == pinned ]] || pick=".$leg | if length == 1 then .[0] else null end"
  moved="$(jq -c "$pick" "$work/resolved.moved")"
  captured="$(jq -c "$pick" "$work/resolved.captured")"
  [[ "$(jq -r '.id // "none"' <<<"$moved")" == "$new_node" ]] ||
    fail "$section" "the $leg resolver reaches c-target at its new address $new_node" "it answers $moved"
  [[ "$(jq -c 'del(.id)' <<<"$moved")" == "$(jq -c 'del(.id)' <<<"$captured")" ]] ||
    fail "$section" "the $leg resolver answers the text captured before the upgrade" "it answers $moved, captured $captured"
  pass "$section" "the $leg resolver reaches c-target at $new_node with the text captured before the upgrade"
done
[[ "$(jq -c '.backlinks.kinds' "$work/resolved.moved")" == "$(jq -c '.backlinks.kinds' "$work/resolved.captured")" ]] ||
  fail "$section" "the backlink read of $target_address answers the links it answered before the upgrade" \
    "it answers $(jq -c '.backlinks' "$work/resolved.moved"), captured $(jq -c '.backlinks' "$work/resolved.captured")"
jq -e --arg c "$cite_comment" '.backlinks.ids | any(endswith($c))' "$work/resolved.moved" >/dev/null ||
  fail "$section" "the backlink read of $target_address answers the comment $cite_comment" "it answers $(jq -c '.backlinks' "$work/resolved.moved")"
pass "$section" "the backlink read of $target_address answers the links it answered before the upgrade"

# --- pointers: each a tuple after the move, and its projection naming the node's new home ------------

section=pointers
issue_tree_redirects_pointers "$section" "$work/pointers.moved"
sed 's/^/pointer after the move: /' "$work/pointers.moved"
readonly new_block="${new_node##*#}" new_ask_block="${new_ask_node##*#}"
while read -r table rest; do
  case "$table" in
    refs)
      read -r from_kind from_id to_kind to_id <<<"$rest"
      [[ "$to_kind $to_id" == "block $new_node" ]] ||
        fail "$section" "refs.to_id of $from_kind $from_id names the node's new home" "it names $to_kind $to_id, want block $new_node"
      issue_tree_redirects_tuple "$section" "the citation of $from_kind $from_id" "$from_kind" "$from_id" "$new_node"
      ;;
    block_refs)
      read -r _ _ from_node to_kind to_id <<<"$rest"
      [[ "$to_kind $to_id" == "block $new_node" ]] ||
        fail "$section" "block_refs.to_id of $from_node names the node's new home" "it names $to_kind $to_id, want block $new_node"
      issue_tree_redirects_tuple "$section" "the citation of block $from_node" block "$from_node" "$new_node"
      ;;
    asks)
      read -r id block_artifact block_id <<<"$rest"
      [[ "$block_artifact $block_id" == "$tree_artifact $new_ask_block" ]] ||
        fail "$section" "asks.block_artifact_id of $id names the new home of a-verify" \
          "it names $block_artifact $block_id, want $tree_artifact $new_ask_block"
      issue_tree_redirects_tuple "$section" "the ask $id's block" ask "$id" "$new_ask_node"
      ;;
    comments)
      read -r id anchor <<<"$rest"
      [[ "$(jq -r '.artifact_id + " " + (.block_id // "")' <<<"$anchor")" == "$tree_artifact $new_block" ]] ||
        fail "$section" "comments.anchor of $id names the new home of c-target" "it reads $anchor, want artifact_id $tree_artifact and block_id $new_block"
      issue_tree_redirects_tuple "$section" "the anchor of the comment $id" comment "$id" "$new_node"
      ;;
    check_results)
      read -r id check_artifact check_block <<<"$rest"
      [[ "$check_artifact $check_block" == "$tree_artifact $new_block" ]] ||
        fail "$section" "the check_results pair of $id names the new home of c-target" \
          "it reads $check_artifact $check_block, want $tree_artifact $new_block"
      issue_tree_redirects_tuple "$section" "the result $id's check" result "$id" "$new_node"
      ;;
    *) fail "$section" "read the five pointer columns" "a line names no pointer: $table $rest" ;;
  esac
  pass "$section" "the $table pointer is a tuple of the store and its projection names the node's new home"
done <"$work/pointers.moved"
for pointer in refs block_refs asks comments check_results; do
  grep -q "^$pointer " "$work/pointers.moved" ||
    fail "$section" "the $pointer pointer stands after the move" "the rows hold none: $(tr '\n' ';' <"$work/pointers.moved")"
done

# --- census: the counters after the move ---------------------------------------------------------------

section=census
tree_wait_backfill "$section"
issue_tree_redirects_census "$section"
[[ "$dangling" == "$dangling_before" ]] ||
  fail "$section" "the dangling_citations line reads after the move as before it" "it reads $dangling, and read $dangling_before before the move"
[[ "$unresolved" == "$unresolved_before" ]] ||
  fail "$section" "unresolved_citations reads after the move as before it, the planted citation still in it" \
    "it reads n=$unresolved, and read n=$unresolved_before before the move"
pass "$section" "no citation the move touched dangles: dangling_citations $dangling and unresolved_citations n=$unresolved, as before the move"

printf 'PASS: %s complete 7 sections\n' "$driver"
