#!/usr/bin/env bash
# The driver of the check c-tree-census of the issue tree's Verification document: the issue's
# census, zero differences in both directions for every projection this child writes, over a
# corpus half of which the earlier release wrote.
#
# It empties the database DATABASE_URL names (a database on this machine whose name ends
# `_upgrade`, which it drops and creates again) and refuses to run, with a FAIL line, where that
# database's own collation orders 'a' after 'B' (`select 'a' < 'B'` false): such a database cannot
# tell byte order from its collation, so a rank read without `collate "C"` would pass here. It boots
# the release BASELINE_SHA names on it through lib-baseline.sh, signs DEV_SIGNIN_LOGIN in (default
# alice), creates PROJECT, and writes the base half through that release's API: an issue whose
# spec, verification-md, plan-md and notes.md each hold the shapes seed-corpus.sh writes (headings
# followed by paragraphs, a nested list, a list whose last item is empty, a typed block holding a
# code block and a table, a quote holding a list) beside requirements, checks that serve them and
# plan tasks; an image; comments citing blocks, a person's comment and messages; an answered ask and
# an open one; results recorded on its checks; a child issue whose check serves a requirement of
# its parent and one of its own; and an issue blocked by the first. It records the block ids and
# their parents, the ask identities, the citation targets and each check_results row, byte for
# byte, and stops that server. It boots this head's (the stamped envoy-dispatch on PATH) over the
# same rows, with the hold settings lib-tree.sh gives every upgrade boot, waits for the backfill as
# the Verification preamble states it (lib-tree.sh's tree_wait_backfill), and writes the head half,
# the same shapes through this head's API, beside a criterion a person writes under a comment of
# its own issue. The head half's child holds no requirement of its own, and its check serves its
# parent's alone: an issue this head creates under an open parent that holds requirements is a
# view on that parent's spec, and a requirement written on it is refused 409
# REQUIREMENT_BELONGS_TO_ANCESTOR.
#
# The census, each half reported apart. For every document of every issue of PROJECT: the blocks
# the server's blocks read answers and the block nodes the store holds (each node whose current
# in_document names the document) are equal, id for id, and so are the nodes and the
# document_blocks rows that project them. Each node's current part_of names the node the rendering
# draws it inside, the parent its path (GET /api/v1/artifacts/{id}/blocks/{block_id}) names, as
# pmdoc.ValidDocument.BlockDescendantIDs walks the tree; an authored node's names its parent
# attribute's target, else its first serves target of its own issue, else its issue; a top-level
# block of a role document (spec, verification-md, plan-md, the tree's own fragment) hangs under
# the issue and an uploaded document's under that document's node. Each row's parent_block_id is
# the block its tuple names, where that is a block of the row's artifact, and none otherwise; a
# difference prints the node, the parent the row holds and the parent the tuple gives. The
# siblings of one document under one parent stand in the order of their source rows by rank, byte
# for byte, and in the tree read's walk, which two reads answer alike; the top-level blocks of an
# issue's documents stand in one run per document. Each half's issues are then reported in two
# classes, those that have converted and those that have not, each with its documents and their
# document_blocks rows, so the rows of the unconverted issues stand as a class of their own.
#
# It takes the same two-sided count for check_results against the result nodes, the rows the
# earlier release wrote read byte for byte as captured and each still listed under its check by GET
# /api/v1/issues/{key}/results; issues.parent_key against the child_of tuples; issue_links against
# the needs tuples between issues; artifacts and artifact_versions against the artifact, query and
# document version nodes; components, component_depends and issue_component_members against the
# component nodes and their part_of, depends_on and affects tuples; and node_search, its rows of
# the nodes that hold text against the text values and its document rows against the live
# documents that hold markdown. It prints each side's count and the symmetric difference, and reads `envoy-dispatch census
# --store`'s line of each of those kinds at zero differences each way and its `census: versions`
# line at `tree_mismatch=0` and `tree_mismatch_repaired=0` (lib-tree.sh's tree_census_versions),
# failing and printing that line otherwise, and prints the census's own class of the rows of issues
# that have not converted, its `unstated_rows` lines, which it compares with nothing. It asserts it
# read more than zero issues and that each half holds more than zero rows.
#
# Inputs: DATABASE_URL, PROJECT, BASELINE_SHA (or BASELINE_BINARY), DISPATCH_TOKEN (the agent
# bearer every server here accepts), DEV_SIGNIN_LOGIN (optional, default alice), and envoy-dispatch
# on PATH. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1; a missing input exits 2. It prints
# no SKIP line, and its last line is `PASS: issue-tree-graph.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<name>_upgrade PROJECT=CORE \
         DISPATCH_TOKEN=e2e-token BASELINE_SHA=<commit> [DEV_SIGNIN_LOGIN=alice] \
         bash e2e/acceptance/issue-tree-graph.sh
       (run from packages/dispatch, with the stamped envoy-dispatch on PATH; DATABASE_URL names a
       database on this machine whose name ends _upgrade, which this run drops and creates again)
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

require_env DATABASE_URL PROJECT
require_commands cmp comm curl diff grep head jq paste psql sed sort tail tr wc
readonly driver=issue-tree-graph.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
# The kinds of `envoy-dispatch census --store` that hold the projections this check reads: the
# blocks and their rank, text and room rows; the results; the issue relations; the artifacts and
# their versions; and the component tables.
readonly -a census_kinds=(block block_rank block_text room_block check_result issue_parent issue_link
  artifact version component_parent component_depends issue_component_member)

work_dir
mkdir "$work/capture" "$work/base" "$work/head"
stamp="$(date +%s)-$$"
readonly stamp
sections=0
base_issues=()
head_issues=()
cleanup() {
  local code=$?
  tree_stop_head
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- helpers -------------------------------------------------------------------------------------

# issue_tree_graph_keys ISSUE…: the issues as a SQL array literal, in keys_sql.
issue_tree_graph_keys() {
  local key list=""
  for key in "$@"; do
    list+=",'$key'"
  done
  keys_sql="array[${list#,}]::text[]"
}

# issue_tree_graph_write_half SECTION HALF: writes one half of the corpus through the server at
# $base, and keeps its issues in HALF_issues (base_issues or head_issues).
issue_tree_graph_write_half() {
  local section="$1" half="$2" shapes shapes_spec verification plan notes child blocked comment
  {
    printf 'The %s half of the tree census.\n\n## Requirements\n\n' "$half"
    printf ':::requirement{#r-shapes}\nEvery shape the census reads is in the corpus.\n:::\n\n'
    printf ':::requirement{#r-citations}\nComments and documents cite blocks.\n:::\n\n'
    printf '## Decisions\n\n:::decision{#d-store}\nThe corpus lives in the pod'"'"'s own test database.\n:::\n\n'
    corpus_shapes spec "$half-spec"
  } >"$work/$half-spec.md"
  corpus_issue "$section" "The $half half of the tree census" "$work/$half-spec.md"
  shapes="$corpus_key" shapes_spec="$corpus_spec"
  tree_key "$section" "the shapes issue's key" "$shapes"
  corpus_revise "$section" "$shapes_spec" spec 2
  {
    printf 'How the %s half is proved.\n\n' "$half"
    printf ':::check{#c-shapes serves="dispatch://%s/spec#b-r-shapes" surface="api" run="bash e2e/acceptance/issue-tree-graph.sh" negative_control="Skip the parent update and the census counts the row."}\nThe census finds every shape.\n:::\n\n' "$shapes"
    printf ':::check{#c-both serves="dispatch://%s/spec#b-r-citations dispatch://%s/spec#b-r-shapes" surface="api" run="bash e2e/acceptance/issue-tree-graph.sh" negative_control="Drop a serves target and the parent moves."}\nOne check serves two requirements.\n:::\n\n' "$shapes" "$shapes"
    corpus_shapes verification.md "$half-verification"
  } >"$work/$half-verification.md"
  corpus_upload "$section" "$shapes" verification.md "$work/$half-verification.md"
  verification="$corpus_doc"
  corpus_revise "$section" "$verification" verification.md 2
  {
    printf 'How the %s half is built.\n\n' "$half"
    printf ':::plan_task{#t-seed serves="dispatch://%s/spec#b-r-shapes"}\nWrite the shapes.\n:::\n\n' "$shapes"
    printf ':::plan_task{#t-cite serves="dispatch://%s/spec#b-r-citations" needs="dispatch://%s/artifact/plan-md#b-t-seed"}\nCite the blocks.\n:::\n\n' "$shapes" "$shapes"
    corpus_shapes plan.md "$half-plan"
  } >"$work/$half-plan.md"
  corpus_upload "$section" "$shapes" plan.md "$work/$half-plan.md"
  plan="$corpus_doc"
  {
    printf '# Notes\n\nThe corpus meets dispatch://%s/spec#b-r-shapes, as dispatch://%s/artifact/verification-md#b-c-shapes proves.\n\n' "$shapes" "$shapes"
    corpus_shapes notes.md "$half-notes"
  } >"$work/$half-notes.md"
  corpus_upload "$section" "$shapes" notes.md "$work/$half-notes.md"
  notes="$corpus_doc"
  corpus_revise "$section" "$notes" notes.md 2
  corpus_image "$section" "$shapes" diagram.png
  corpus_blocks "$section" "$notes"
  corpus_comment agent "$section" "$shapes" \
    "dispatch://$shapes/spec#b-r-shapes is proved by dispatch://$shapes/artifact/verification-md#b-c-shapes."
  corpus_comment person "$section" "$shapes" \
    "Which shapes does dispatch://$shapes/artifact/plan-md#b-t-seed write?"$'\n\n'"The notes say why: dispatch://$shapes/artifact/notes-md#b-$corpus_paragraph."
  comment="$corpus_item"
  corpus_comment agent "$section" "$shapes" "Every one of them." "$comment" b2
  corpus_message agent "$section" "$shapes" "The census reads dispatch://$shapes/artifact/plan-md#b-t-cite."
  corpus_message person "$section" "$shapes" "Is every shape written?"
  corpus_ask "$section" "$shapes" "Which store holds the $half half?"
  corpus_answer "$section" "$corpus_item" "Postgres, the database the pod runs." Postgres
  corpus_ask "$section" "$shapes" "Which store holds the next corpus?"
  corpus_wait_settled "$section"
  corpus_result "$section" "$shapes" c-shapes pass "$corpus_commit_one" tester
  corpus_result "$section" "$shapes" c-both fail "$corpus_commit_one" implementer
  corpus_result "$section" "$shapes" c-both pass "$corpus_commit_two" implementer
  # The earlier release's child holds a requirement of its own, which its check serves beside its
  # parent's. This head's child holds none: born under an open parent that holds requirements, it
  # is a view on that parent's spec, and POST /api/v1/issues refuses a spec that holds a
  # requirement (409 REQUIREMENT_BELONGS_TO_ANCESTOR); its check serves its parent's alone, and
  # hangs under the child issue, since it serves no requirement of its own issue.
  {
    [[ "$half" != base ]] || printf ':::requirement{#r-child}\nThe child holds a requirement of its own.\n:::\n\n'
    corpus_shapes spec "$half-child-spec"
  } >"$work/$half-child-spec.md"
  corpus_issue "$section" "A child in the $half half" "$work/$half-child-spec.md" "$shapes"
  child="$corpus_key"
  tree_key "$section" "the child's key" "$child"
  {
    if [[ "$half" == base ]]; then
      printf ':::check{#c-child serves="dispatch://%s/spec#b-r-shapes dispatch://%s/spec#b-r-child" surface="cli" run="bash e2e/acceptance/issue-tree-graph.sh" negative_control="Serve the parent alone and the check hangs under the issue."}\nThe child proves its parent'"'"'s shapes and its own requirement.\n:::\n\n' \
        "$shapes" "$child"
    else
      printf ':::check{#c-child serves="dispatch://%s/spec#b-r-shapes" surface="cli" run="bash e2e/acceptance/issue-tree-graph.sh" negative_control="Serve a requirement of the child and the check hangs under it."}\nThe child proves its parent'"'"'s shapes.\n:::\n\n' \
        "$shapes"
    fi
    corpus_shapes verification.md "$half-child-verification"
  } >"$work/$half-child-verification.md"
  corpus_upload "$section" "$child" verification.md "$work/$half-child-verification.md"
  corpus_wait_settled "$section"
  corpus_result "$section" "$child" c-child pass "$corpus_commit_one" tester
  printf 'An issue the %s shapes issue blocks.\n\n:::requirement{#r-blocked}\nIt waits.\n:::\n' "$half" >"$work/$half-blocked-spec.md"
  corpus_issue "$section" "An issue blocked in the $half half" "$work/$half-blocked-spec.md" "" "$shapes"
  blocked="$corpus_key"
  tree_key "$section" "the blocked issue's key" "$blocked"
  corpus_wait_settled "$section"
  if [[ "$half" == base ]]; then
    base_issues=("$shapes" "$child" "$blocked")
  else
    head_issues=("$shapes" "$child" "$blocked")
  fi
  half_shapes="$shapes"
  pass "$section" "wrote the $half half: $shapes (with its four documents, an image, comments, messages, asks and results), its child $child, and $blocked, which $shapes blocks"
}

# issue_tree_graph_document SECTION HALF ID REF SLUG PRIMARY KIND ISSUE: gathers, into
# $work/HALF/ID.json, what the census reads of one document: its blocks as the blocks read answers
# them, each block's rendering parent as its path names it (the nearest block above it that holds
# an id, none at the top), each block node the store holds of it (its current in_document names
# the document) and each document_blocks row that projects one (a row whose document it is, as
# refs' BlockDocumentSQL reads a row's document: the one its issue_room_blocks row names, else the
# row's own artifact), with the parent the row holds and the address of the row's artifact.
#
# An issue's tree artifact is read through the text read: the blocks read and the path read are a
# document's alone (400 NOT_DOCUMENT for an artifact of kind tree), and the text read takes the
# tree artifact as it takes a document (api's readsAsDocument) and answers, with ?blocks=true, the
# blocks of its own fragment with their ranges. Its blocks' rendering parents are read off those
# ranges: the nearest block before a block whose range holds it, a block of no bytes holding none.
issue_tree_graph_document() {
  local section="$1" half="$2" id="$3" ref="$4" slug="$5" primary="$6" kind="$7" issue="$8"
  local address top block parent dir="$work/$2"
  if [[ "$primary" == t ]]; then
    address="dispatch://$issue/spec"
  else
    address="dispatch://$issue/artifact/$slug"
  fi
  if [[ "$primary" == t || "$kind" == tree || "$slug" == verification-md || "$slug" == plan-md ]]; then
    top="issue:$issue"
  else
    top="artifact:$ref"
  fi
  : >"$dir/$id.parents"
  if [[ "$kind" == tree ]]; then
    tree_send GET "/api/v1/artifacts/$id/text?blocks=true"
    tree_want 200 "$section" "read the text of $ref with its blocks"
    jq -c '[.blocks[] | {id, type, from, attrs: ([.attributes[]? | {(.name): .value}] | add // {})}]' "$work/body" >"$dir/$id.blocks" ||
      fail "$section" "read the blocks of $ref" "no list of blocks: $(body)"
    jq -r '.blocks as $blocks | range(0; $blocks | length) as $i | $blocks[$i] as $b
      | ([$blocks[:$i][] | select(.from <= $b.from and $b.to <= .to and .to > .from)] | last | .id // "") as $parent
      | "\($b.id)\t\($parent)"' "$work/body" >"$dir/$id.parents" ||
      fail "$section" "read where each block stands in $ref" "no ranges in: $(body)"
  else
    tree_send GET "/api/v1/artifacts/$id/blocks"
    tree_want 200 "$section" "read the blocks of $ref"
    jq -c '[.[] | {id, type, from, attrs: ([.attributes[]? | {(.name): .value}] | add // {})}]' "$work/body" >"$dir/$id.blocks" ||
      fail "$section" "read the blocks of $ref" "no list of blocks: $(body)"
    while IFS= read -r block; do
      tree_send GET "/api/v1/artifacts/$id/blocks/$(q "$block")"
      tree_want 200 "$section" "read where $block stands in $ref"
      parent="$(jq -r '[.path[:-1][] | select(.id != "") | .id] | last // ""' "$work/body")" ||
        fail "$section" "read where $block stands in $ref" "no path in: $(body)"
      printf '%s\t%s\n' "$block" "$parent" >>"$dir/$id.parents"
    done < <(jq -r '.[].id' "$dir/$id.blocks")
  fi
  query "$section" "read the block nodes of $ref" "
    select split_part(c.from_id, '#', 2) || '|' || c.from_id
    from (
      select distinct on (t.from_id) t.from_id, t.asserted
      from graph_tuples t
      where t.from_kind = 'block' and t.kind = 'in_document' and t.to_kind = 'artifact' and t.to_id = '$ref'
      order by t.from_id, t.seq desc
    ) c
    join graph_nodes n on n.kind = 'block' and n.id = c.from_id and n.state <> 'gone'
    where c.asserted"
  printf '%s\n' "$answer" >"$dir/$id.nodes"
  query "$section" "read the document_blocks rows of $ref" "
    select d.block_id || '|' || coalesce(d.node_address, '') || '|' || coalesce(d.parent_block_id, '') || '|' || a.ref_key
    from document_blocks d join artifacts a on a.id = d.artifact_id
    where coalesce((select r.document_id from issue_room_blocks r
                    where r.tree_artifact_id = d.artifact_id and r.block_id = d.block_id), d.artifact_id) = '$id'"
  printf '%s\n' "$answer" >"$dir/$id.rows"
  jq -n --arg id "$id" --arg ref "$ref" --arg address "$address" --arg issue "$issue" --arg top "$top" \
    --slurpfile blocks "$dir/$id.blocks" --rawfile parents "$dir/$id.parents" \
    --rawfile nodes "$dir/$id.nodes" --rawfile rows "$dir/$id.rows" '
    def lines($text): $text | split("\n") | map(select(. != ""));
    {id: $id, ref: $ref, address: $address, issue: $issue, top: $top, blocks: $blocks[0],
     parents: (lines($parents) | map(split("\t") | {(.[0]): (.[1] // "")}) | add // {}),
     nodes: (lines($nodes) | map(split("|") | {(.[0]): .[1]}) | add // {}),
     rows: (lines($rows) | map(split("|") | {(.[0]): {node: .[1], parent: .[2], artifact: .[3]}}) | add // {})}' \
    >"$dir/$id.json"
}

# issue_tree_graph_walk SECTION ISSUE FILE: the block nodes of ISSUE's tree read with no view, in
# the walk's order across every page (lib-tree.sh's tree_read_all), one id a line, into FILE.
issue_tree_graph_walk() {
  tree_read_all "$1" "$work/walk.json" "$2"
  jq -r '.nodes[] | select(.kind == "block") | .id' "$work/walk.json" >"$3"
}

# issue_tree_graph_sides SECTION HALF NAME ROWS NODES: the two-sided count of the projection NAME
# over HALF: the rows ROWS reads against the store's side NODES reads, each one key a line. It
# prints the count on each side and the symmetric difference, and fails on any difference.
issue_tree_graph_sides() {
  local section="$1" half="$2" name="$3" rows nodes only_rows only_nodes
  query "$section" "read the $name rows of the $half half" "$4"
  printf '%s\n' "$answer" | grep -v '^$' | LC_ALL=C sort -u >"$work/$half.$name.rows" || true
  query "$section" "read the store's side of $name in the $half half" "$5"
  printf '%s\n' "$answer" | grep -v '^$' | LC_ALL=C sort -u >"$work/$half.$name.nodes" || true
  rows="$(wc -l <"$work/$half.$name.rows")" nodes="$(wc -l <"$work/$half.$name.nodes")"
  only_rows="$(LC_ALL=C comm -23 "$work/$half.$name.rows" "$work/$half.$name.nodes" | head -n 10 | paste -sd ' ' -)"
  only_nodes="$(LC_ALL=C comm -13 "$work/$half.$name.rows" "$work/$half.$name.nodes" | head -n 10 | paste -sd ' ' -)"
  printf 'projection %s %s: rows=%s store=%s rows_without_the_store=%s store_without_a_row=%s\n' "$half" "$name" \
    "$rows" "$nodes" "$(LC_ALL=C comm -23 "$work/$half.$name.rows" "$work/$half.$name.nodes" | wc -l)" \
    "$(LC_ALL=C comm -13 "$work/$half.$name.rows" "$work/$half.$name.nodes" | wc -l)"
  [[ -z "$only_rows" && -z "$only_nodes" ]] ||
    fail "$section" "$name of the $half half reads zero differences in both directions" \
      "rows the store lacks: ${only_rows:-none}; the store's without a row: ${only_nodes:-none}"
  pass "$section" "$name of the $half half: $rows rows and $nodes in the store, zero differences in both directions"
}

# The current relation out of a node, as a statement reads one (graphstore.CurrentTupleRead): the
# newest tuple per (node, kind, far end), kept while that newest one asserts. issue_tree_graph_links
# FROM_KIND KIND TO_KIND FILTER: `from>to` for each current tuple of KIND from a node of FROM_KIND to
# one of TO_KIND whose from_id FILTER (SQL over t.from_id) admits.
issue_tree_graph_links() {
  links_sql="
    select c.from_id || '>' || c.to_id from (
      select distinct on (t.from_id, t.to_kind, t.to_id) t.from_id, t.to_kind, t.to_id, t.asserted
      from graph_tuples t
      where t.from_kind = '$1' and t.kind = '$2' and t.to_kind = '$3' and $4
      order by t.from_id, t.to_kind, t.to_id, t.seq desc
    ) c where c.asserted"
}

# The census the driver takes of one half's documents, over the JSON issue_tree_graph_census
# gathers: {docs, part_of, rank, walk}. It prints one line per difference, then one `count` line
# per count it took.
readonly census_program='
  . as $h
  | ($h.docs | map({key: .address, value: .}) | from_entries) as $by
  | def node_of($address):
      ($address | split("#b-")) as $part
      | if ($part | length) == 2 and $by[$part[0]] != null then ($by[$part[0]].nodes[$part[1]] // null) else null end;
    def issue_of($address): $address | ltrimstr("dispatch://") | split("/")[0];
    def authored: . as $t | ["criterion", "requirement", "check", "plan_task"] | index($t) != null;
    def want($d; $b):
      if ($b.type | authored) then
        if ($b.attrs.parent // "") != "" then "names:" + ($b.attrs.parent | split("/") | last | split("#")[0])
        else
          ([($b.attrs.serves // "") | split(" ")[] | select(. != "" and issue_of(.) == $d.issue)] | first) as $serves
          | if $serves != null then "block:" + (node_of($serves) // ("unresolved " + $serves))
            else "issue:" + $d.issue end
        end
      elif ($d.parents[$b.id] // "") != "" then "block:" + ($d.nodes[$d.parents[$b.id]] // ("unresolved " + $d.parents[$b.id]))
      else $d.top end;
    def placed($want; $have):
      ($have | length) == 1
      and (if ($want | startswith("names:")) then ($have[0] | contains($want | ltrimstr("names:"))) else $have[0] == $want end);
    [ $h.docs[] as $d
      | ($d.blocks | map(.id)) as $read
      | ($d.nodes | keys) as $held
      | ($d.rows | keys) as $rows
      | (($read - $held)[] | "node \($d.ref)#\(.) is in the blocks read and the store holds no node of it"),
        (($held - $read)[] | "node \($d.ref)#\(.) is a node of the store the blocks read does not answer"),
        (($held - $rows)[] | "node \($d.ref)#\(.) is a node no document_blocks row projects"),
        (($rows - $held)[] | "node \($d.ref)#\(.) is a document_blocks row of no node"),
        ($d.blocks[] as $b
         | ($d.nodes[$b.id] // null) as $node
         | select($node != null)
         | want($d; $b) as $want
         | ($h.part_of[$node] // []) as $have
         | (if placed($want; $have) then empty
            else "parent \($node), a \($b.type): the store places it under \($have | join(" and ") | if . == "" then "nothing" else . end), and the rendering and its author under \($want)" end),
           (($d.rows[$b.id] // null) as $row
            | select($row != null)
            | (($have[0] // "") | if startswith("block:" + $row.artifact + "#") then ltrimstr("block:" + $row.artifact + "#") else "" end) as $tuple
            | if $row.parent == $tuple then empty
              else "projection \($node): its document_blocks row holds the parent \($row.parent | if . == "" then "none" else . end), and its part_of tuple gives \($have[0] // "none")" end)),
        ($d.blocks | to_entries | map(select($d.nodes[.value.id] != null)
           | {at: .key, id: .value.id, node: $d.nodes[.value.id], parent: want($d; .value)})
         | group_by(.parent)[] | select(length > 1) as $siblings
         | ($siblings | map(.node)) as $source
         | ($siblings | map($h.rank[.node] // null)) as $ranks
         | if ($ranks | index(null)) != null then "rank under \($siblings[0].parent): \($siblings[$ranks | index(null)].node) holds no rank"
           else
             ($siblings | sort_by($h.rank[.node]) | map(.node)) as $by_rank
             | ($h.walk[$d.issue] // []) as $walk
             | ($source | map(. as $n | $walk | index($n)) | map(select(. != null))) as $walked
             | (if $by_rank == $source then empty
                else "rank under \($siblings[0].parent): by rank \($by_rank | join(", ")), in the source rows \($source | join(", "))" end),
               (if $walked == ($walked | sort) then empty
                else "walk under \($siblings[0].parent): the tree read walks \($source | join(", ")) at \($walked | map(tostring) | join(", "))" end)
           end)
    ] as $differences
    | [ $h.walk | to_entries[] as $issue
        | [ $h.docs[] | select(.issue == $issue.key and .top == ("issue:" + $issue.key)) as $d
            | $d.blocks[] | select(($d.parents[.id] // "") == "" and (.type | authored | not))
            | {node: $d.nodes[.id], doc: $d.id} | select(.node != null) ] as $tops
        | [ $issue.value[] as $n | $tops[] | select(.node == $n) | .doc ]
        | reduce .[] as $doc ([]; if (last // null) == $doc then . else . + [$doc] end)
        | select(length != (unique | length))
        | "band under issue:\($issue.key): the top-level blocks of its documents interleave in the walk (runs of \(join(", ")))"
      ] as $bands
    | ($differences + $bands)[],
      "count documents=\($h.docs | length)",
      "count blocks_read=\([$h.docs[].blocks[]] | length)",
      "count block_nodes=\([$h.docs[].nodes | keys[]] | length)",
      "count document_blocks_rows=\([$h.docs[].rows | keys[]] | length)",
      "count parents_read=\([$h.docs[] | .nodes as $n | .blocks[] | select($n[.id] != null)] | length)",
      "count ranked_siblings=\([$h.docs[] | .nodes as $n | .blocks[] | select($n[.id] != null and $h.rank[$n[.id]] != null)] | length)"'

# issue_tree_graph_census SECTION HALF ISSUE…: the census of one half's documents
# (census_program), its differences printed, failing on any.
issue_tree_graph_census() {
  local section="$1" half="$2" key id ref slug primary kind issue dir="$work/$2"
  shift 2
  issue_tree_graph_keys "$@"
  query "$section" "list the documents of the $half half" "
    select a.id::text || '|' || a.ref_key || '|' || a.slug || '|' || a.is_primary::text::char(1) || '|' || a.kind || '|' || a.issue_key
    from artifacts a
    where a.issue_key = any($keys_sql) and a.kind in ('doc', 'tree') and a.retired_at is null
      and not exists (select 1 from graph_nodes q where q.kind = 'artifact' and q.id = a.ref_key and q.supertag = 'query'
                      and q.attributes->>'query' = 'decisions')
    order by a.issue_key, a.created_at, a.id"
  printf '%s\n' "$answer" >"$dir/documents"
  while IFS='|' read -r id ref slug primary kind issue; do
    [[ -n "$id" ]] || continue
    issue_tree_graph_document "$section" "$half" "$id" "$ref" "$slug" "$primary" "$kind" "$issue"
  done <"$dir/documents"
  # Every far end, of any kind: the part_of of a block names a block, an issue, an artifact or a
  # conversation node.
  query "$section" "read the current part_of of each block node of the $half half" "
    select c.from_id || '|' || c.to_kind || ':' || c.to_id from (
      select distinct on (t.from_id, t.to_kind, t.to_id) t.from_id, t.to_kind, t.to_id, t.asserted
      from graph_tuples t
      where t.from_kind = 'block' and t.kind = 'part_of' and t.to_kind <> 'value' and split_part(t.from_id, '/', 1) = any($keys_sql)
      order by t.from_id, t.to_kind, t.to_id, t.seq desc
    ) c where c.asserted"
  printf '%s\n' "$answer" >"$dir/part_of"
  query "$section" "read the current rank of each block node of the $half half" "
    select c.from_id || '|' || c.to_id from (
      select distinct on (t.from_id) t.from_id, t.to_id, t.asserted
      from graph_tuples t
      where t.from_kind = 'block' and t.kind = 'rank' and t.to_kind = 'value' and split_part(t.from_id, '/', 1) = any($keys_sql)
      order by t.from_id, t.seq desc
    ) c where c.asserted"
  printf '%s\n' "$answer" >"$dir/rank"
  : >"$dir/walks"
  for key in "$@"; do
    issue_tree_graph_walk "$section" "$key" "$dir/$key.walk"
    issue_tree_graph_walk "$section" "$key" "$dir/$key.walk-again"
    cmp -s "$dir/$key.walk" "$dir/$key.walk-again" ||
      fail "$section" "two reads of $key's tree give one order" "$(diff "$dir/$key.walk" "$dir/$key.walk-again" | head -n 20)"
    jq -Rn --arg k "$key" '{($k): [inputs | select(. != "")]}' <"$dir/$key.walk" >>"$dir/walks"
  done
  pass "$section" "two reads of each tree of the $half half ($*) give one order"
  jq -n --slurpfile walks "$dir/walks" --rawfile part_of "$dir/part_of" --rawfile rank "$dir/rank" '
    def lines($text): $text | split("\n") | map(select(. != ""));
    {docs: [inputs],
     part_of: (lines($part_of) | map(split("|")) | group_by(.[0]) | map({key: .[0][0], value: map(.[1])}) | from_entries),
     rank: (lines($rank) | map(split("|")) | map({key: .[0], value: (.[1:] | join("|"))}) | from_entries),
     walk: ($walks | add // {})}' "$dir"/*.json >"$dir/census.json" 2>"$work/jq" ||
    fail "$section" "gather the census of the $half half" "jq: $(<"$work/jq")"
  jq -r "$census_program" "$dir/census.json" >"$dir/census.out" 2>"$work/jq" ||
    fail "$section" "take the census of the $half half" "jq: $(<"$work/jq")"
  grep '^count ' "$dir/census.out" | sed "s/^count /count $half /"
  if grep -v '^count ' "$dir/census.out" >"$dir/differences"; then
    fail "$section" "the $half half's nodes, parents and ranks read zero differences in both directions" \
      "$(wc -l <"$dir/differences") differences: $(head -n 20 "$dir/differences" | paste -sd ';' -)"
  fi
  grep -q '^count document_blocks_rows=[1-9]' "$dir/census.out" ||
    fail "$section" "the $half half holds rows" "its census read none: $(grep '^count ' "$dir/census.out" | paste -sd ' ' -)"
  pass "$section" "the $half half's blocks, nodes and rows agree id for id, every parent and every rank stands, and the walk keeps the source order"
}

# issue_tree_graph_projections SECTION HALF ISSUE…: the two-sided counts of the issue-keyed
# projections over one half.
issue_tree_graph_projections() {
  local section="$1" half="$2"
  shift 2
  issue_tree_graph_keys "$@"
  issue_tree_graph_sides "$section" "$half" check_results \
    "select id::text from check_results where issue_key = any($keys_sql)" "
      select n.id from graph_nodes n
      where n.kind = 'result' and n.state <> 'gone' and exists (
        select 1 from graph_tuples t where t.from_kind = 'result' and t.from_id = n.id and t.kind = 'result_of'
          and split_part(t.to_id, '/', 1) = any($keys_sql))"
  issue_tree_graph_links issue child_of issue "t.from_id = any($keys_sql)"
  issue_tree_graph_sides "$section" "$half" issues.parent_key \
    "select key || '>' || parent_key from issues where parent_key is not null and key = any($keys_sql)" "$links_sql"
  issue_tree_graph_links issue needs issue "t.from_id = any($keys_sql)"
  issue_tree_graph_sides "$section" "$half" issue_links \
    "select issue_key || '>' || target_key from issue_links where kind = 'blocked_by' and issue_key = any($keys_sql)" "$links_sql"
  issue_tree_graph_sides "$section" "$half" artifacts \
    "select ref_key from artifacts where issue_key = any($keys_sql) and retired_at is null" "
      select id from graph_nodes where kind = 'artifact' and state <> 'gone' and split_part(id, '/', 1) = any($keys_sql)"
  issue_tree_graph_sides "$section" "$half" artifact_versions "
      select a.ref_key || '@v' || v.number from artifact_versions v join artifacts a on a.id = v.artifact_id
      where a.issue_key = any($keys_sql) and a.retired_at is null" "
      select id from graph_nodes where kind = 'version' and state <> 'gone' and split_part(id, '/', 1) = any($keys_sql)"
  issue_tree_graph_links issue affects component "t.from_id = any($keys_sql)"
  issue_tree_graph_sides "$section" "$half" issue_component_members \
    "select issue_key || '>' || project_key || '/' || component_id from issue_component_members where issue_key = any($keys_sql)" "$links_sql"
}

# issue_tree_graph_classes SECTION HALF ISSUE…: the half's issues in two classes, each reported
# apart: those that have converted (an issue_trees row holds the issue) and those that have not,
# whose rows are the class the store census counts by itself (graphcensus's unconvertedUnstated,
# printed as `unstated_rows`, the store section). For each class it prints its issues, their
# documents and the document_blocks rows of those documents, a room's rows under its tree artifact.
issue_tree_graph_classes() {
  local section="$1" half="$2" class converted issues count documents rows
  shift 2
  issue_tree_graph_keys "$@"
  for class in converted unconverted; do
    converted=false
    [[ "$class" == unconverted ]] || converted=true
    query "$section" "read the $class issues of the $half half and their rows" "
      with held as (
        select i.key,
               (select count(*) from artifacts a
                where a.issue_key = i.key and a.kind in ('doc', 'tree') and a.retired_at is null) as documents,
               (select count(*) from document_blocks d join artifacts a on a.id = d.artifact_id
                where a.issue_key = i.key) as block_rows
        from issues i
        where i.key = any($keys_sql) and exists (select 1 from issue_trees t where t.issue_key = i.key) = $converted)
      select coalesce(string_agg(key, ' ' order by key), '') || '|' || count(*) || '|'
          || coalesce(sum(documents), 0) || '|' || coalesce(sum(block_rows), 0)
      from held"
    IFS='|' read -r issues count documents rows <<<"$answer"
    printf 'class %s %s: issues=%s (%s) documents=%s document_blocks_rows=%s\n' \
      "$half" "$class" "$count" "${issues:-none}" "$documents" "$rows"
  done
  pass "$section" "the $half half's issues and rows, reported in two classes: converted and not converted"
}

# --- setup: the earlier release over an emptied database --------------------------------------

section=setup
tree_reset_database "$database_url" '?*_upgrade' "$section"
query "$section" "read how this database orders 'a' and 'B'" "select ('a' < 'B')::text"
[[ "$answer" == true ]] ||
  fail "$section" "the database's own collation is not byte order, so a rank read without collate \"C\" shows" \
    "select 'a' < 'B' reads $answer on $(env -u PGHOSTADDR psql "$database_url" -Atqc "select datcollate from pg_database where datname = current_database()" 2>&1): such a database cannot tell byte order from its own collation"
pass "$section" "select 'a' < 'B' reads true: this database's collation is not byte order"
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-tree-graph-$stamp"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The census of the issue tree"
sections=$((sections + 1))

# --- base: the half the earlier release writes --------------------------------------------------

section=base
issue_tree_graph_write_half "$section" base
issue_tree_graph_keys "${base_issues[@]}"
readonly base_keys_sql="$keys_sql"
# What the earlier release created: each block id with the parent its row holds, each ask's
# identity, each citation's target, and each check_results row byte for byte.
query "$section" "record the blocks and their parents" "
    select a.ref_key || '#' || d.block_id || ' parent=' || coalesce(d.parent_block_id, '')
    from document_blocks d join artifacts a on a.id = d.artifact_id where a.issue_key = any($base_keys_sql)
    order by 1"
printf '%s\n' "$answer" >"$work/capture/blocks"
query "$section" "record the asks" "select id::text || ' ' || issue_key from asks where issue_key = any($base_keys_sql) order by 1"
printf '%s\n' "$answer" >"$work/capture/asks"
query "$section" "record the citations" "select from_kind || ':' || from_id || '>' || to_kind || ':' || to_id from refs order by 1"
printf '%s\n' "$answer" >"$work/capture/refs"
query "$section" "record the check results" "
    select row_to_json(r)::text from check_results r where r.issue_key = any($base_keys_sql) order by r.id"
printf '%s\n' "$answer" >"$work/capture/check_results"
query "$section" "record which check each result is recorded on" "
    select r.id::text || '|' || r.issue_key || '|' || r.check_block_id from check_results r where r.issue_key = any($base_keys_sql) order by r.id"
printf '%s\n' "$answer" >"$work/capture/result_checks"
printf 'recorded %s blocks, %s asks, %s citations and %s results the earlier release wrote\n' \
  "$(grep -c . "$work/capture/blocks" || true)" "$(grep -c . "$work/capture/asks" || true)" \
  "$(grep -c . "$work/capture/refs" || true)" "$(grep -c . "$work/capture/check_results" || true)"
baseline_stop_server
sections=$((sections + 1))

# --- upgrade: this head over the earlier release's rows -----------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
# Every id the earlier release's index held stands. This head's index holds rows beside them: a
# list item is a block of its own to this head's renderer (pmdoc's renderer.list records each
# item's range), which indexes it at the id the item held already, where the earlier release's
# index held no row of an item. So each row the upgrade added must be a list item's, and a row of
# any other type that the earlier release's index did not hold fails here.
query "$section" "read the blocks the earlier release created, after the upgrade" "
    select a.ref_key || '#' || d.block_id || ' ' || d.type
    from document_blocks d join artifacts a on a.id = d.artifact_id where a.issue_key = any($base_keys_sql)
    order by 1"
printf '%s\n' "$answer" >"$work/blocks.after"
sed 's/ parent=.*$//' "$work/capture/blocks" | grep . | LC_ALL=C sort >"$work/blocks.before" ||
  fail "$section" "the earlier release created block ids" "the capture holds none"
sed 's/ [^ ]*$//' "$work/blocks.after" | grep . | LC_ALL=C sort >"$work/blocks.after.ids" || true
LC_ALL=C comm -23 "$work/blocks.before" "$work/blocks.after.ids" >"$work/blocks.lost"
[[ ! -s "$work/blocks.lost" ]] ||
  fail "$section" "every block id the earlier release created stands after the upgrade" \
    "lost: $(head -n 20 "$work/blocks.lost" | paste -sd ';' -)"
grep -v ' list_item$' "$work/blocks.after" | sed 's/ [^ ]*$//' | grep . | LC_ALL=C sort >"$work/blocks.after.other" || true
LC_ALL=C comm -13 "$work/blocks.before" "$work/blocks.after.other" >"$work/blocks.added"
[[ ! -s "$work/blocks.added" ]] ||
  fail "$section" "each row the upgrade added to the earlier release's index is a list item's" \
    "added, of another type: $(grep -F -f "$work/blocks.added" "$work/blocks.after" | head -n 20 | paste -sd ';' -)"
printf 'the upgrade keeps the %s block ids the earlier release indexed and indexes %s list items beside them\n' \
  "$(wc -l <"$work/blocks.before")" "$(LC_ALL=C comm -13 "$work/blocks.before" "$work/blocks.after.ids" | wc -l)"
want_value "$section" "every ask the earlier release created stands after the upgrade" "$(<"$work/capture/asks")" \
  "select coalesce(string_agg(id::text || ' ' || issue_key, E'\n' order by id::text || ' ' || issue_key), '') from asks where issue_key = any($base_keys_sql)"
query "$section" "read the citations after the upgrade" "select from_kind || ':' || from_id || '>' || to_kind || ':' || to_id from refs order by 1"
printf '%s\n' "$answer" >"$work/refs.after"
comm -23 <(grep . "$work/capture/refs" | LC_ALL=C sort) <(grep . "$work/refs.after" | LC_ALL=C sort) >"$work/refs.lost" || true
[[ ! -s "$work/refs.lost" ]] ||
  fail "$section" "every citation the earlier release created still names its target" "lost: $(head -n 10 "$work/refs.lost" | paste -sd ';' -)"
pass "$section" "the earlier release's block ids, asks and citations stand after the upgrade"
sections=$((sections + 1))

# --- head: the half this head writes --------------------------------------------------------------

section=head
issue_tree_graph_write_half "$section" head
head_shapes="$half_shapes"
readonly head_shapes
tree_read agent "$head_shapes"
tree_want 200 "$section" "read $head_shapes's tree"
tree_field "$section" "$head_shapes's tree artifact" '.tree'
head_tree="$tree_value"
readonly head_tree
tree_person_send POST "/api/v1/issues/$head_shapes/comments" "$(jq -cn '{body: "The person wants the census to read a criterion."}')"
tree_want 201 "$section" "$person_login comments on $head_shapes"
tree_field "$section" "the comment's id" .id
criterion_comment="$tree_value"
readonly criterion_comment
tree_person_send POST "/api/v1/artifacts/$head_tree/edits" "$(jq -cn --arg p "dispatch://$head_shapes/comment/$criterion_comment#b-b1" \
  '{ops: [{op: "insert", after: "end", markdown: (":::criterion{#k-census parent=\"" + $p + "\"}\nThe census reads a criterion under a comment.\n:::\n")}]}')"
tree_want 200 "$section" "$person_login writes a criterion under the comment into $head_shapes's tree"
corpus_wait_settled "$section"
tree_wait_backfill "$section"
pass "$section" "$person_login wrote a criterion under a comment of $head_shapes"
sections=$((sections + 1))

# --- census: the blocks, each half apart ----------------------------------------------------------

section=census
query "$section" "count the issues of $project" "select count(*) from issues where project_key = '$project'"
issues_read="$answer"
((issues_read > 0)) || fail "$section" "the census reads more than zero issues" "$project holds $issues_read"
pass "$section" "the census reads $issues_read issues of $project: base ${base_issues[*]}, head ${head_issues[*]}"
issue_tree_graph_census "$section" base "${base_issues[@]}"
issue_tree_graph_census "$section" head "${head_issues[@]}"
issue_tree_graph_classes "$section" base "${base_issues[@]}"
issue_tree_graph_classes "$section" head "${head_issues[@]}"
sections=$((sections + 1))

# --- projections: the two-sided count of each projection, each half apart ------------------------

section=projections
issue_tree_graph_projections "$section" base "${base_issues[@]}"
issue_tree_graph_projections "$section" head "${head_issues[@]}"
# The check_results rows the earlier release wrote, byte for byte, each still listed under its
# check.
query "$section" "read the check results the earlier release wrote" "
    select row_to_json(r)::text from check_results r where r.issue_key = any($base_keys_sql) order by r.id"
printf '%s\n' "$answer" >"$work/check_results.after"
cmp -s "$work/capture/check_results" "$work/check_results.after" ||
  fail "$section" "each check_results row the earlier release wrote reads byte for byte as captured" \
    "$(diff "$work/capture/check_results" "$work/check_results.after" | head -n 10)"
[[ -n "$(grep . "$work/capture/check_results")" ]] ||
  fail "$section" "the earlier release wrote check_results rows" "the capture holds none"
while IFS='|' read -r result issue check; do
  [[ -n "$result" ]] || continue
  tree_send GET "/api/v1/issues/$issue/results"
  tree_want 200 "$section" "read the results of $issue"
  jq -e --arg id "$result" --arg check "$check" \
    'any(.. | objects | select((.results? | type) == "array"); ([.results[].id] | index($id)) != null and ([.[] | strings] | index($check)) != null)' \
    "$work/body" >/dev/null ||
    fail "$section" "the result $result the earlier release wrote is still listed under its check $check" "GET /api/v1/issues/$issue/results: $(body)"
done <"$work/capture/result_checks"
pass "$section" "each check_results row the earlier release wrote reads byte for byte as captured, and its check still lists it"
# The component tables, which name a project and no issue, and node_search, against the whole
# store. node_search holds two kinds of row (R17, migration 20261125160000_issue_tree_node_search):
# one for each node that holds text, which is held to the text values, and one for each live
# document that holds a version with markdown (node_kind `artifact`), whose vector is that
# version's and which no text value stands behind, held to those documents, as the store census's
# own two kinds read the table (graphcensus' node_search and document_search_node).
issue_tree_graph_sides "$section" project components \
  "select project_key || '/' || id from components where project_key = '$project'" \
  "select id from graph_nodes where kind = 'component' and state <> 'gone' and split_part(id, '/', 1) = '$project'"
issue_tree_graph_links component part_of component "split_part(t.from_id, '/', 1) = '$project'"
issue_tree_graph_sides "$section" project components.parent \
  "select project_key || '/' || id || '>' || project_key || '/' || parent from components where parent is not null and project_key = '$project'" \
  "$links_sql"
issue_tree_graph_links component depends_on component "split_part(t.from_id, '/', 1) = '$project'"
issue_tree_graph_sides "$section" project component_depends \
  "select project_key || '/' || from_id || '>' || project_key || '/' || to_id from component_depends where project_key = '$project'" \
  "$links_sql"
issue_tree_graph_sides "$section" store node_search \
  "select node_kind || ':' || node_address from node_search where node_kind <> 'artifact'" "
    select c.from_kind || ':' || c.from_id from (
      select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
      from graph_tuples t
      where t.kind = 'text' and t.to_kind = 'value'
      order by t.from_kind, t.from_id, t.seq desc
    ) c
    join graph_nodes n on n.kind = c.from_kind and n.id = c.from_id and n.state <> 'gone'
    where c.asserted"
issue_tree_graph_sides "$section" store node_search.documents \
  "select node_address from node_search where node_kind = 'artifact'" "
    select a.ref_key from artifacts a
    join graph_nodes n on n.kind = 'artifact' and n.id = a.ref_key and n.state <> 'gone'
    where a.kind = 'doc' and a.retired_at is null
      and (select v.search from artifact_versions v
           where v.artifact_id = a.id and v.markdown is not null order by v.number desc limit 1) is not null"
sections=$((sections + 1))

# --- store: envoy-dispatch census --store over both halves ----------------------------------------

section=store
tree_store_census "$section"
tree_census_versions "$section" "the census reads tree_mismatch=0 and tree_mismatch_repaired=0" "" "$work/census.out"
for kind in "${census_kinds[@]}"; do
  line="$(grep -E "^census: store kind=$kind (nodes|tuples)=" "$work/census.out" || true)"
  [[ -n "$line" ]] || fail "$section" "the store census reads the kind $kind" "it prints no line of it: $(grep -E '^census: store' "$work/census.out" | head -n 5 | paste -sd ';' -)"
  printf '%s\n' "$line"
  [[ "$line" =~ rows_without_a_(node|tuple)=([0-9,]+)\ (node|tuple)s_without_a_row=([0-9,]+) ]] ||
    fail "$section" "read the census line of $kind" "$line"
  [[ "${BASH_REMATCH[2]}" == 0 && "${BASH_REMATCH[4]}" == 0 ]] ||
    fail "$section" "the census of $kind reads zero differences in both directions" \
      "$line; $(grep -E "^census: store kind=$kind difference at" "$work/census.out" | head -n 10 | paste -sd ';' -)"
done
pass "$section" "the store census reads zero differences in both directions for ${census_kinds[*]}"
# The rows of documents of issues that have not converted which no pass of this build's rule has
# stated, which the census counts as a class of their own and compares with nothing.
grep -E '^census: store kind=[a-z_]+ unstated_rows=' "$work/census.out" >"$work/unstated" ||
  fail "$section" "the store census counts the rows of issues that have not converted as a class of their own" \
    "it prints no 'census: store kind=<kind> unstated_rows=' line: $(grep -E '^census: store kind=block_text' "$work/census.out" | head -n 5 | paste -sd ';' -)"
cat "$work/unstated"
pass "$section" "the store census reports the rows of issues that have not converted as a class of their own: $(sed -E 's/^census: store kind=([a-z_]+) unstated_rows=([0-9,]+).*/\1 \2/' "$work/unstated" | paste -sd ',' -)"
sections=$((sections + 1))

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
