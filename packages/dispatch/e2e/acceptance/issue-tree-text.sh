#!/usr/bin/env bash
# The second driver of the check c-reparent-and-state of dispatch://INFRA-1253: the text rules of
# the move route, PATCH /api/v1/issues/{key}/nodes/{id} (R7), on an issue that has not converted.
# The tree is the source and the Markdown a rendering of it: where Markdown can show a moved
# block's new place the move writes its text there, in one version of the document that holds the
# block, and nests a block only under a container with no words of its own, a callout or a list
# item; under a block whose content is its own words the text stays flat and the tree alone holds
# the place.
#
# An issue this head creates while it alone serves its database is born converted (R5, R6), so the
# driver empties the database DATABASE_URL names (a database on this machine whose name ends
# `_upgrade`, which it drops and creates again), boots the release BASELINE_SHA names on it through
# lib-baseline.sh, signs DEV_SIGNIN_LOGIN in and creates PROJECT there, and boots this head's server
# beside it (tree_boot_head, with the settings that hold R6's conversion step), each on a port
# held-port.py holds. It waits for the backfill as the Verification preamble states. While both
# processes are live it creates the issue through this head's POST /api/v1/issues, with an empty
# spec, and adds its Verification document and its plan by a title alone through this head's POST
# /api/v1/issues/{key}/artifacts: the release before lists no issue_room, so the issue is not born
# converted, and each document keeps a room of its own. It writes no block while the release
# before is live, and then stops it. Through this head alone it writes every block a case moves
# and makes every move. Before the first block and after the last move it reads no document_rooms
# row of the issue.
#
# A case that writes a version - rewritten, gained, dropped, list, order, callout, list paragraph -
# has an approval request open on the document it writes before the move, and reads after it one
# version more of that document, its artifact.version, the request naming the new version, the
# version before still rendering the bytes it rendered before, and the move in both the new stored
# version and the head render (GET /api/v1/artifacts/{id}/text), and the text_parent and text_rank
# the version's index pass wrote:
#
#   rewritten  a requirement whose parent= names K1 moves under K2, and its text names K2;
#   gained     a check that names no parent, under its requirement by serves, moves under another
#              requirement: the Verification document's new version gains parent= naming it, and
#              the spec takes no version;
#   dropped    a requirement that names and serves K1 moves under a trade-off lens on K2, which no
#              attribute names: its text holds no parent=, and the tree read answers it under the lens;
#   list       a list item moves under the item above it in its list, and stands one level deeper;
#   order      a paragraph moves after its sibling, and the text holds it there;
#   callout    a paragraph moves under a callout X, which does not open the spec: the text holds it
#              as X's last block, and its stored part_of names X;
#   list paragraph  a paragraph moves under a list item that holds one paragraph: the text holds it
#              in the item after its first paragraph, and its stored part_of names the item.
#
# The cases that write no version:
#
#   decision   alice keeps D, her decision, with the text_sha256 the decisions read answers, and
#              the agent moves a paragraph under it: the move is taken, the text stays flat - the
#              paragraph outside D, which holds what it held - its stored part_of names D, and
#              D reads the same text_sha256, kept by alice, waiting on no review;
#   table      one row for each of a requirement, a criterion, an ask block and a reading of the
#              spec, a check of the Verification document, a plan task of the plan and a callout C
#              inside a decision D2: a paragraph of the block's document moves under the block, the
#              move is taken, writes no version and leaves the head render byte for byte as read
#              before, the paragraph's stored part_of names the block, the block's token is the
#              one read before, and for C, D2's text_sha256 is too;
# The tree read returns none of these moved prose paragraphs of role documents (R2, R8).
#   emptied    the only paragraph of a top-level callout Y, the only paragraph of a callout Y2 in a
#              list item, and the first paragraph of a list item M, each moved under the issue after
#              a top-level block before them, are each 409 CONTAINER_WOULD_EMPTY naming the
#              container and its kind, and change neither the spec's version count, the store's
#              tuple count nor the spec's room's doc_updates rows, each paragraph still under its
#              container.
#
#   stored     After the cases, an edit that changes another block's words alone runs a later index
#              pass, after which every block a case moved reads the placement stored and the part_of
#              its move wrote: the dropped requirement stays under the lens.
#
# Inputs: DATABASE_URL (a database on this machine whose name ends _upgrade, which this run drops
# and creates again), PROJECT, DEV_SIGNIN_LOGIN, DISPATCH_TOKEN (the agent bearer every server here
# accepts), BASELINE_SHA (or BASELINE_BINARY, and BASELINE_EXTRA_ENV and LEGION_WORKSPACE as
# lib-baseline.sh's head comment states them), and the stamped envoy-dispatch of this head on PATH.
# It takes no DISPATCH_URL: it boots both servers itself. A missing input exits 2.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line, and
# its last line is `PASS: issue-tree-text.sh complete <n> cases <m> moves`. Every check is
# `cmd || fail …`, never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=$INFRA_1253_UPGRADE_DATABASE_URL DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice BASELINE_SHA=<commit> bash e2e/acceptance/issue-tree-text.sh
       (run from packages/dispatch with the stamped envoy-dispatch of this head on PATH;
       DATABASE_URL names a database on this machine whose name ends _upgrade, which this run
       drops and creates again; BASELINE_BINARY may stand in for BASELINE_SHA)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"

require_env DATABASE_URL PROJECT DEV_SIGNIN_LOGIN
require_commands awk cmp curl grep head jq psql
command -v envoy-dispatch >/dev/null || {
  printf 'issue-tree-text.sh: envoy-dispatch is not on PATH: build ./cmd/dispatch of this head, stamped with its commit\n' >&2
  usage
  exit 2
}
readonly driver=issue-tree-text.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"

work_dir
stamp="$(date +%s)-$$"
readonly stamp
cases=0
moves=0
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

# --- the vocabulary of this driver ------------------------------------------------------------------

# tt_database_parts: database_url split into the server it names (database_server, its URL with
# the `postgres` database in place of its own) and its database's name (database_name). It refuses
# a URL whose database is not on this machine or whose name does not end `_upgrade`, the database
# the Verification preamble gives the upgrade drivers.
tt_database_parts() {
  loopback_database_url "$database_url" ||
    tree_refuse "DATABASE_URL names no single loopback postgres database: this driver drops and creates it"
  [[ "$database_url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] ||
    tree_refuse "DATABASE_URL is no postgres:// URL naming a database"
  database_server="${BASH_REMATCH[1]}${BASH_REMATCH[3]}/postgres${BASH_REMATCH[5]}"
  database_name="${BASH_REMATCH[4]}"
  [[ "$database_name" =~ ^[a-z0-9_]+_upgrade$ ]] ||
    tree_refuse "DATABASE_URL names the database $database_name: this driver drops and creates only a database whose name ends _upgrade"
}

# tt_empty_database: drops the database database_url names and creates it empty, so the earlier
# release's own Store.Migrate writes its schema and this run's rows are its only content.
tt_empty_database() {
  baseline_psql "$database_server" -q \
    -c "drop database if exists \"$database_name\" with (force)" -c "create database \"$database_name\"" >"$work/reset" 2>&1 ||
    fail setup "empty the database $database_name" "psql: $(<"$work/reset")"
  pass setup "dropped the database $database_name and created it empty"
}

# tt_rooms SECTION WHEN: the issue holds no document_rooms row, read at WHEN.
tt_rooms() {
  with_vars "$1" "read the document_rooms rows of $key" \
    "select count(*) from document_rooms r join artifacts a on a.id = r.artifact_id where a.issue_key = :'key'" key="$key"
  [[ "$answer" == 0 ]] || fail "$1" "the issue of the text cases holds no document_rooms row $2" "the issue $key of the text cases holds a document_rooms row, want none"
  pass "$1" "$key holds no document_rooms row $2"
}

# tt_quiet SECTION: waits, at most 120 s, until no document of the issue owes a settlement or its
# block index, so a version count and a pass's facts read what the moves left.
tt_quiet() {
  local deadline=$((SECONDS + 120))
  while true; do
    with_vars "$1" "read what the documents of $key still owe" \
      "select (select count(*) from doc_settlements_pending p join artifacts a on a.id = p.artifact_id where a.issue_key = :'key')
            + (select count(*) from document_block_versions v join artifacts a on a.id = v.artifact_id
               where a.issue_key = :'key' and (v.version is null or v.store_owed_at is not null))" key="$key"
    [[ "$answer" != 0 ]] || return 0
    ((SECONDS < deadline)) || fail "$1" "the documents of $key settle and take their block index" "after 120 s they still owe $answer"
    sleep 1
  done
}

# tt_patch AS KEY NODE TAG BODY: PATCH /api/v1/issues/KEY/nodes/NODE as AS (agent or person), NODE
# percent-encoded as one segment, with the JSON BODY and `If-Match: "TAG"`. The answer is in
# $work/body and `status`, as tree_request leaves them.
tt_patch() {
  local as="$1" key="$2" node="$3" tag="$4" body="$5"
  local -a auth
  case "$as" in
    agent) auth=(-H "@$work/auth") ;;
    person) auth=(-b "$work/jar" -H "Origin: $(tree_origin)") ;;
    *) tree_refuse "tt_patch sends as agent or person, not '$as'" ;;
  esac
  tree_last_request="PATCH /api/v1/issues/$key/nodes/$node If-Match \"$tag\" $body"
  status="$(curl -sS -X PATCH -o "$work/body" -w '%{http_code}' "${auth[@]}" -H "If-Match: \"$tag\"" \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "$base/api/v1/issues/$(q "$key")/nodes/$(q "$node")" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# tt_store ADDRESS: the store's kind and id of the node at ADDRESS, in tt_kind and tt_id: a block's
# address holds a `#` and is the block node's id; every other address is `<kind>/<id>`.
tt_store() {
  if [[ "$1" == *"#"* ]]; then
    tt_kind=block tt_id="$1"
  else
    tt_kind="${1%%/*}" tt_id="${1#*/}"
  fi
}

# tt_far SECTION ADDRESS TUPLE: the far ends of the current TUPLE tuples out of the node at ADDRESS -
# a block's id, every other node `<kind>/<id>` - joined by commas, in answer.
tt_far() {
  tt_store "$2"
  with_vars "$1" "read the $3 of $2" \
    "select coalesce(string_agg(case when h.to_kind = 'block' then h.to_id else h.to_kind || '/' || h.to_id end, ',' order by h.to_kind, h.to_id), '')
     from (select distinct on (to_kind, to_id) to_kind, to_id, asserted from graph_tuples
           where from_kind = :'kind' and from_id = :'id' and kind = :'tuple' and to_kind <> 'value'
           order by to_kind, to_id, seq desc) h where h.asserted" \
    kind="$tt_kind" id="$tt_id" tuple="$3"
}

# tt_value SECTION ADDRESS ATTRIBUTE: the current value of ATTRIBUTE of the node at ADDRESS, empty
# where it holds none, in answer.
tt_value() {
  tt_store "$2"
  with_vars "$1" "read the $3 of $2" \
    "select coalesce((select case when asserted then to_id end from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = :'attribute' and to_kind = 'value'
       order by seq desc limit 1), '')" \
    kind="$tt_kind" id="$tt_id" attribute="$3"
}

# tt_tag SECTION ADDRESS: the placement_seq of the node at ADDRESS, the seq of its current part_of, as
# the tree read answers it, in tt_placed.
tt_tag() {
  tt_store "$2"
  with_vars "$1" "read the placement_seq of $2" \
    "select coalesce((select h.seq from (select distinct on (to_kind, to_id) seq, asserted from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = 'part_of' and to_kind <> 'value'
       order by to_kind, to_id, seq desc) h where h.asserted order by h.seq desc limit 1)::text, '')" \
    kind="$tt_kind" id="$tt_id"
  [[ -n "$answer" ]] || fail "$1" "read the placement_seq of $2" "it holds no part_of"
  tt_placed="$answer"
}

# tt_body SECTION AS PARENT [AFTER]: the body of a move under PARENT, after AFTER where it is given,
# with the agent's actor where AS is agent, in answer.
tt_body() {
  answer="$(jq -cn --arg p "$3" --arg a "${4:-}" --argjson actor "$(tree_actor)" --arg as "$2" \
    '{parent: $p} + (if $a == "" then {} else {after: $a} end) + (if $as == "agent" then {actor: $actor} else {} end)')"
}

# The blocks a case moved, each with the parent its move wrote, which the stored section reads after
# a later pass.
declare -a stored_nodes=() stored_parents=()

# tt_move SECTION AS KEY ADDRESS PARENT [AFTER]: AS moves ADDRESS under PARENT (after AFTER, where it
# is given) with the If-Match of its placement_seq, and wants 200 naming the parent and a
# placement_seq; the move joins the blocks the stored section reads.
tt_move() {
  local section="$1" as="$2" key="$3" address="$4" parent="$5" after="${6:-}" code
  tt_tag "$section" "$address"
  tt_body "$section" "$as" "$parent" "$after"
  tt_patch "$as" "$key" "$address" "$tt_placed" "$answer"
  if [[ "$status" != 200 ]]; then
    code="$(jq -r '.code // ""' "$work/body" 2>/dev/null)" || code=""
    fail "$section" "the move of $address under $parent" "the move of $address under $parent answered $status${code:+ $code}, want 200: $(body)"
  fi
  expect "$section" "the move of $address answers its parent" "\"$parent\"" .parent
  jq -e '.placement_seq | numbers' "$work/body" >/dev/null ||
    fail "$section" "the move of $address answers its placement_seq" "PATCH of $address answered without placement_seq for the next If-Match: $(body)"
  moves=$((moves + 1))
  stored_nodes+=("$address")
  stored_parents+=("$parent")
}

# tt_write SECTION WHAT DOC MARKDOWN: the person appends MARKDOWN to the document DOC through the edit
# route; it must change it.
tt_write() {
  tree_person_send POST "/api/v1/artifacts/$3/edits" "$(jq -cn --arg m "$4" '{ops: [{op: "insert", after: "end", markdown: $m}]}')"
  tree_want 200 "$1" "$2"
  jq -e '.changed == true' "$work/body" >/dev/null || fail "$1" "$2" "the edit changed nothing: $(body)"
  pass "$1" "$2"
}

# tt_block SECTION DOC TYPE WORDS: the block of type TYPE in the text of DOC whose text holds WORDS:
# its node id in tt_node and its block id in tt_block_id.
tt_block() {
  with_vars "$1" "read the $3 of $2 that holds '$4'" \
    "select coalesce((select d.node_id || '|' || d.block_id from document_blocks d
       where d.artifact_id = :'doc'::uuid and d.type = :'type' and strpos(d.excerpt, :'words') > 0
       order by d.block_id limit 1), '')" doc="$2" type="$3" words="$4"
  [[ -n "$answer" ]] || fail "$1" "read the $3 of $2 that holds '$4'" "no $3 of $2 holds it"
  tt_node="${answer%%|*}" tt_block_id="${answer#*|}"
}

# tt_typed SECTION DOC ID: the node id of the block ID of DOC, in tt_node.
tt_typed() {
  with_vars "$1" "read the node of block $3 of $2" \
    "select coalesce((select node_id from document_blocks where artifact_id = :'doc'::uuid and block_id = :'id' limit 1), '')" doc="$2" id="$3"
  [[ -n "$answer" ]] || fail "$1" "read the node of block $3 of $2" "no block $3 of $2 holds a node"
  tt_node="$answer"
}

# tt_version SECTION DOC: the newest version of DOC, its number in tt_number and its stored markdown
# in $work/version.md.
tt_version() {
  with_vars "$1" "read the newest version of $2" "select coalesce(max(number), 0) from artifact_versions where artifact_id = :'doc'::uuid" doc="$2"
  tt_number="$answer"
  with_vars "$1" "read the markdown of version $tt_number of $2" \
    "select coalesce((select markdown from artifact_versions where artifact_id = :'doc'::uuid and number = :'n'::int), '')" doc="$2" n="$tt_number"
  printf '%s\n' "$answer" >"$work/version.md"
}

# tt_head SECTION DOC: the head render of DOC, the canonical markdown GET /api/v1/artifacts/{id}/text
# answers, in $work/head.md.
tt_head() {
  tree_person_send GET "/api/v1/artifacts/$2/text"
  tree_want 200 "$1" "read the head render of $2"
  jq -r '.markdown' "$work/body" >"$work/head.md"
}

# tt_render SECTION DOC NUMBER FILE: what the one renderer answers for version NUMBER of DOC
# (GET /api/v1/artifacts/{id}/versions/{number}?download=true), in FILE.
tt_render() {
  tree_person_send GET "/api/v1/artifacts/$2/versions/$3?download=true"
  tree_want 200 "$1" "read version $3 of $2"
  cp "$work/body" "$4"
}

# tt_request SECTION DOC: the agent, naming its session, opens an approval request on DOC, as an
# agent asks a person to approve its document; its id in tt_ask.
tt_request() {
  tree_send POST "/api/v1/artifacts/$2/approval-requests" "$(jq -cn --argjson a "$(tree_actor)" '{actor: $a}')"
  [[ "$status" == 201 || "$status" == 200 ]] || fail "$1" "open an approval request on $2" "it answered $status: $(body)"
  tree_field "$1" "open an approval request on $2" .ask.id
  tt_ask="$tree_value"
  pass "$1" "opened the approval request $tt_ask on $2"
}

# tt_before SECTION DOC: what a move that writes a version of DOC is read against: the newest version
# in tt_from and what the renderer answers for it in $work/before-render.
tt_before() {
  tt_quiet "$1"
  tt_version "$1" "$2"
  tt_from="$tt_number"
  tt_render "$1" "$2" "$tt_from" "$work/before-render"
}

# tt_after SECTION DOC ASK WHAT: WHAT, a move, wrote one version of DOC since tt_before: its
# artifact.version stands, the request ASK names it, and the version before renders the bytes it
# rendered before. The new version's stored markdown is left in $work/new.md and the head render in
# $work/head.md.
tt_after() {
  tt_quiet "$1"
  tt_version "$1" "$2"
  ((tt_number == tt_from + 1)) ||
    fail "$1" "$4 writes one version of $2" "$4 left $2 at version $tt_number, want $((tt_from + 1))"
  cp "$work/version.md" "$work/new.md"
  with_vars "$1" "read the artifact.version of version $tt_number of $2" \
    "select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = :'doc'
       and coalesce((payload->'version'->>'number')::int, (payload->>'version')::int) = :'n'::int" doc="$2" n="$tt_number"
  [[ "$answer" == 1 ]] || fail "$1" "version $tt_number of $2 appends its artifact.version" "it appended $answer artifact.version events, want 1"
  with_vars "$1" "read the version the request $3 names of $2" \
    "select coalesce((select d->>'version' from asks a, jsonb_array_elements(a.approval->'documents') d
       where a.id = :'ask'::uuid and d->>'artifact_id' = :'doc' limit 1), '')" ask="$3" doc="$2"
  [[ "$answer" == "$tt_number" ]] ||
    fail "$1" "the request on $2 names the version $4 wrote" "the request $3 on $2 names version $answer after $4, want $tt_number"
  tt_render "$1" "$2" "$tt_from" "$work/after-render"
  cmp -s "$work/before-render" "$work/after-render" ||
    fail "$1" "version $tt_from of $2 still renders its original bytes" "after $4 it renders $(head -c 300 "$work/after-render")"
  tt_head "$1" "$2"
  pass "$1" "$4 wrote version $tt_number of $2, with its artifact.version, the request naming it, and version $tt_from rendering as before"
}

# tt_container FILE OPENER: the lines of the container block of FILE whose opening line, after its
# fence of colons, begins with OPENER (`callout{#tx`), from that line to its closing fence - a line
# of as many colons, whatever its indent - on stdout; nothing where FILE holds no such line.
tt_container() {
  awk -v opener="$2" '
    !found {
      line = $0
      sub(/^ */, "", line)
      if (match(line, /^:+/) && RLENGTH >= 3 && index(substr(line, RLENGTH + 1), opener) == 1) {
        found = 1
        fence = substr(line, 1, RLENGTH)
        print
        next
      }
    }
    found {
      print
      closing = $0
      gsub(/^ +| +$/, "", closing)
      if (closing == fence) exit
    }
  ' "$1"
}

# tt_line FILE WORDS: the first line of FILE that holds WORDS, on stdout; nothing for none.
tt_line() {
  awk -v words="$2" 'index($0, words) { print; exit }' "$1"
}

# tt_tree_parent SECTION VIEW NODE: the parent the person's tree read of the issue, with ?view=VIEW
# where it is not empty, answers for NODE, in answer; empty where it answers NODE under none.
tt_tree_parent() {
  tree_read person "$key" "$2"
  tree_want 200 "$1" "read the tree of $key${2:+ ?view=$2}"
  answer="$(jq -r --arg n "$3" '[.nodes[] | select(.id == $n) | (.parent.id // "")][0] // ""' "$work/body")"
}

# tt_stored_prose SECTION VIEW NODE PARENT: NODE stands under PARENT in the store, but the tree
# read omits it because it is prose of a role document (R2, R8).
tt_stored_prose() {
  tt_far "$1" "$3" part_of
  [[ "$answer" == "$4" ]] ||
    fail "$1" "the moved paragraph's placement" "$3 holds a stored part_of naming $answer, want $4"
  tree_read person "$key" "$2"
  tree_want 200 "$1" "read the tree of $key${2:+ ?view=$2}"
  jq -e --arg n "$3" 'all(.nodes[]; .id != $n)' "$work/body" >/dev/null ||
    fail "$1" "the tree read leaves out role-document prose" "the tree read answers $3, want no node"
}

# tt_decision SECTION NODE: the decisions read of the issue's entry for the decision NODE: its
# text_sha256 in tt_sum, the login its Keep names in tt_kept (empty for none) and its waiting_review
# in tt_waiting.
tt_decision() {
  tree_person_send GET "/api/v1/issues/$key/decisions"
  tree_want 200 "$1" "read the decisions of $key"
  jq -e --arg n "$2" 'any(.entries[]; .node.id == $n)' "$work/body" >/dev/null ||
    fail "$1" "the decisions read of $key answers $2" "it holds no entry of $2: $(body)"
  tt_sum="$(jq -r --arg n "$2" '[.entries[] | select(.node.id == $n)][0].text_sha256' "$work/body")"
  tt_kept="$(jq -r --arg n "$2" '[.entries[] | select(.node.id == $n)][0].kept.kept_by // ""' "$work/body")"
  tt_waiting="$(jq -r --arg n "$2" '[.entries[] | select(.node.id == $n)][0].waiting_review' "$work/body")"
}

# tt_token SECTION DOC BLOCK: the token GET /api/v1/artifacts/{id}/blocks answers for BLOCK of DOC,
# in answer.
tt_token() {
  tree_person_send GET "/api/v1/artifacts/$2/blocks"
  tree_want 200 "$1" "read the blocks of $2"
  answer="$(jq -r --arg b "$3" '[.[] | select(.id == $b)][0].token // ""' "$work/body")"
  [[ -n "$answer" ]] || fail "$1" "the blocks of $2 answer a token of $3" "they hold no block $3: $(body)"
}

# The markdown alice writes into the spec, the Verification document and the plan: every block a
# case moves, each found by its words or its id.
tt_spec_markdown() {
  local spec="dispatch://$key/spec#b-"
  cat <<MD
:::criterion{#tk1}
The first criterion of the text cases.
:::

:::criterion{#tk2}
The second criterion of the text cases.
:::

:::requirement{#tr1 parent="${spec}tk1"}
The requirement whose parent the move rewrites.
:::

:::requirement{#tr2 serves="${spec}tk1" parent="${spec}tk1"}
The requirement whose parent the move drops.
:::

:::requirement{#tr3}
The requirement the checks serve.
:::

- The first item of the moving list.
- The second item of the moving list, which moves under the first.

The first paragraph of the order case.

The second paragraph of the order case.

:::callout{#tx kind="note" title=""}
The callout's own paragraph.
:::

The paragraph that moves into the callout.

:::decision{#td}
The decision alice keeps.
:::

The paragraph the agent moves under the decision.

- The list item that holds one paragraph.

The paragraph that moves into the list item.

:::requirement{#trq}
The requirement of the table's row.
:::

A paragraph that moves under the requirement of the table.

:::criterion{#tkq}
The criterion of the table's row.
:::

A paragraph that moves under the criterion of the table.

:::ask{#taq}
Which store do the table's writers take?
:::

A paragraph that moves under the ask of the table.

::::decision{#tdr}
The decision whose reading the table moves under.

:::reading{#tgr}
The agent reads the decision as one store.
:::
::::

A paragraph that moves under the reading of the table.

::::decision{#td2}
The decision that holds the callout of the table.

:::callout{#tc kind="note" title=""}
The callout inside the decision.
:::
::::

A paragraph that moves under the callout inside the decision.

A top-level paragraph before the emptied containers.

:::callout{#ty kind="note" title=""}
Y holds this one paragraph.
:::

- The first paragraph of the item that holds Y2.

  :::callout{#ty2 kind="note" title=""}
  Y2 holds this one paragraph.
  :::
- M's first paragraph.

  M's second paragraph.
MD
}

tt_verification_markdown() {
  local serves="dispatch://$key/spec#b-tr3"
  cat <<MD
How $key is checked.

:::check{#tc1 serves="$serves" surface="api" run="go test ./..." negative_control="Undo the move."}
The check that gains a parent.
:::

:::check{#tc2 serves="$serves" surface="api" run="go test ./..." negative_control="Undo the move."}
The check of the table's row.
:::

A paragraph of the Verification document that moves under its check.
MD
}

tt_plan_markdown() {
  cat <<MD
The plan of $key.

:::plan_task{#tt1}
The plan task of the table's row.
:::

A paragraph of the plan that moves under its plan task.
MD
}

# --- setup: the release before over an emptied database, and this head beside it -------------------

section=setup
tt_database_parts
tt_empty_database
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-tree-text-$stamp"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The text of a move"
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
cases=$((cases + 1))

# --- issue: created while the release before is live, so it is not born converted -------------------

section=issue
tree_person_send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "The text of a move $stamp" '{project: $p, title: $t, force: true}')"
tree_want 201 "$section" "create the issue of the text cases through this head"
tree_field "$section" "the key of the issue of the text cases" .key
key="$tree_value"
[[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$section" "create the issue of the text cases" "the key $key is no issue key"
# Not readonly: the move helpers take a local `key`, which a readonly name refuses.
tree_field "$section" "the spec of $key" .primary_artifact_id
readonly spec_doc="$tree_value"
tree_read person "$key"
tree_want 200 "$section" "read the tree of $key"
expect "$section" "$key, created beside a process that lists no issue_room, is not converted" false .converted
tt_upload_empty() {
  tree_person_send POST "/api/v1/issues/$key/artifacts" "$(jq -cn --arg n "$2" '{name: $n}')"
  tree_want 201 "$1" "add $2 to $key by a title alone"
  tree_field "$1" "add $2 to $key by a title alone" .artifact.id
}
tt_upload_empty "$section" verification.md
readonly verification_doc="$tree_value"
tt_upload_empty "$section" plan.md
readonly plan_doc="$tree_value"
tt_rooms "$section" "before the first block"
baseline_stop_server
pass "$section" "the release before stopped; this head alone serves the database"
cases=$((cases + 1))

# --- blocks: every block a case moves, written through this head alone -------------------------------

section=blocks
tt_write "$section" "alice writes the blocks of the text cases into the spec of $key" "$spec_doc" "$(tt_spec_markdown)"
tt_write "$section" "alice writes the checks into the Verification document of $key" "$verification_doc" "$(tt_verification_markdown)"
tt_write "$section" "alice writes the plan task into the plan of $key" "$plan_doc" "$(tt_plan_markdown)"
tt_quiet "$section"
tt_request "$section" "$spec_doc"
readonly spec_ask="$tt_ask"
tt_request "$section" "$verification_doc"
readonly verification_ask="$tt_ask"
tt_typed "$section" "$spec_doc" tk1
readonly tk1="$tt_node"
tt_typed "$section" "$spec_doc" tk2
readonly tk2="$tt_node"
tt_typed "$section" "$spec_doc" tr1
readonly tr1="$tt_node"
tt_typed "$section" "$spec_doc" tr2
readonly tr2="$tt_node"
tt_typed "$section" "$verification_doc" tc1
readonly tc1="$tt_node"
cases=$((cases + 1))

# --- rewritten: the parent= of a requirement names its new parent ------------------------------------

section=rewritten
tt_far "$section" "$tr1" text_parent
text_before="$answer"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$tr1" "$tk2"
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $tr1 under $tk2"
for file in "$work/new.md" "$work/head.md"; do
  line="$(tt_line "$file" "requirement{#tr1")"
  [[ "$line" != *"#b-tk1\""* ]] || fail "$section" "the text of $tr1 names its new parent" "the new spec version of $key still names the old parent $tk1: $line"
  [[ "$line" == *"#b-tk2\""* ]] || fail "$section" "the text of $tr1 names its new parent" "the new spec version of $key holds no parent= naming $tk2: $line"
done
tt_far "$section" "$tr1" text_parent
[[ "$answer" == "$text_before" ]] || fail "$section" "the text of $tr1 stays where it stood" "its text_parent moved from $text_before to $answer"
tt_value "$section" "$tr1" text_rank
[[ -n "$answer" ]] || fail "$section" "$tr1 holds a text_rank" "it holds none after its version's pass"
pass "$section" "the text of $tr1 names $tk2, and its text_parent and text_rank stand"
cases=$((cases + 1))

# --- gained: a check that named no parent gains one, in a version of its own document --------------

section=gained
tt_version "$section" "$spec_doc"
spec_version="$tt_number"
tt_before "$section" "$verification_doc"
tt_move "$section" person "$key" "$tc1" "$tr1"
tt_after "$section" "$verification_doc" "$verification_ask" "the move of $tc1 under $tr1"
for file in "$work/new.md" "$work/head.md"; do
  line="$(tt_line "$file" "check{#tc1")"
  [[ "$line" == *"parent=\"dispatch://$key/spec#b-tr1\""* ]] ||
    fail "$section" "the text of $tc1 gains parent= naming $tr1" "the new version of $key holds no parent= for $tc1, want $tr1: $line"
done
tt_version "$section" "$spec_doc"
[[ "$tt_number" == "$spec_version" ]] || fail "$section" "the spec takes no version" "the move of $tc1 wrote version $tt_number of the spec, $spec_version before"
tt_value "$section" "$tc1" text_rank
[[ -n "$answer" ]] || fail "$section" "$tc1 holds a text_rank" "it holds none after its version's pass"
pass "$section" "the Verification document's new version names $tr1 in the parent= of $tc1, and the spec stands at version $spec_version"
cases=$((cases + 1))

# --- dropped: under a lens no attribute names, the text drops the attribute ----------------------------

section=dropped
tree_person_send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "dispatch://$key/spec#b-tk2" '{on: $on, kind: "trade-off"}')"
tree_want 201 "$section" "invoke a trade-off on $tk2"
tree_field "$section" "the lens on $tk2" .id
readonly lens="lens/$tree_value"
tt_far "$section" "$tr2" text_parent
text_before="$answer"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$tr2" "$lens"
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $tr2 under $lens"
for file in "$work/new.md" "$work/head.md"; do
  line="$(tt_line "$file" "requirement{#tr2")"
  [[ "$line" != *"parent="* ]] || fail "$section" "the text of $tr2 holds no parent=" "the new version of $key still names a parent for $tr2, which no attribute can name under a lens: $line"
  [[ "$line" == *"serves="* ]] || fail "$section" "the text of $tr2 keeps its serves" "the new version of $key lost the serves of $tr2: $line"
done
tt_tree_parent "$section" "" "$tr2"
[[ "$answer" == "${lens#lens/}" || "$answer" == "$lens" ]] || fail "$section" "the tree read answers $tr2 under the lens" "$tr2 stands under '$answer' in the tree read, want $lens"
tt_far "$section" "$tr2" text_parent
[[ "$answer" == "$text_before" ]] || fail "$section" "the text of $tr2 stays flat where it stood" "its text_parent moved from $text_before to $answer"
pass "$section" "the text of $tr2 names no parent and is flat, and the tree read answers it under $lens"
cases=$((cases + 1))

# --- list: a list item nests under the item above it --------------------------------------------------

section=list
tt_block "$section" "$spec_doc" list_item "The first item of the moving list"
readonly first_item="$tt_node"
tt_block "$section" "$spec_doc" list_item "The second item of the moving list"
readonly second_item="$tt_node"
# tt_depth FILE: how many levels deep the second item of the moving list stands in FILE, its indent
# in steps of two spaces, in answer.
tt_depth() {
  answer="$(awk -v words="The second item of the moving list" 'index($0, words) { match($0, /^ */); print int(RLENGTH / 2); exit }' "$1")"
  [[ -n "$answer" ]] || answer=-1
}
tt_head "$section" "$spec_doc"
tt_depth "$work/head.md"
depth_before="$answer"
tt_far "$section" "$second_item" text_parent
text_before="$answer"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$second_item" "$first_item"
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $second_item under $first_item"
for file in "$work/new.md" "$work/head.md"; do
  tt_depth "$file"
  [[ "$answer" == "$((depth_before + 1))" ]] ||
    fail "$section" "the text of $second_item stands one level deeper" "list item $second_item at depth $answer, want $((depth_before + 1))"
done
tt_far "$section" "$second_item" text_parent
[[ "$answer" != "$text_before" ]] || fail "$section" "the text_parent of $second_item follows its text" "it still names $text_before"
pass "$section" "the text of $second_item stands at depth $((depth_before + 1)), under $first_item, and its text_parent is $answer"
cases=$((cases + 1))

# --- order: a block moves among its siblings, and the text holds it in its new place ------------------

section=order
tt_block "$section" "$spec_doc" paragraph "The first paragraph of the order case"
readonly first_paragraph="$tt_node"
tt_block "$section" "$spec_doc" paragraph "The second paragraph of the order case"
readonly second_paragraph="$tt_node"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$first_paragraph" "issue/$key" "$second_paragraph"
expect "$section" "the move of $first_paragraph answers the sibling it now follows" "\"$second_paragraph\"" .after
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $first_paragraph after $second_paragraph"
for file in "$work/new.md" "$work/head.md"; do
  text="$(<"$file")"
  [[ "$text" == *"The second paragraph of the order case"*"The first paragraph of the order case"* ]] ||
    fail "$section" "the text holds $first_paragraph after $second_paragraph" "the new version of $key holds $first_paragraph before $second_paragraph"
done
tt_value "$section" "$first_paragraph" text_rank
moved_rank="$answer"
tt_value "$section" "$second_paragraph" text_rank
second_rank="$answer"
# Ranks compare byte for byte, as every rank does (R1), so the store compares them, not the shell's
# locale.
with_vars "$section" "compare the text_rank of $first_paragraph with that of $second_paragraph" \
  "select coalesce((select :'moved'::text collate \"C\" > :'other'::text collate \"C\" and :'moved' <> ''), false)" \
  moved="$moved_rank" other="$second_rank"
[[ "$answer" == t ]] ||
  fail "$section" "the text_rank of $first_paragraph follows its text" "it reads '$moved_rank', and that of $second_paragraph '$second_rank'"
pass "$section" "the text holds $first_paragraph after $second_paragraph, and its text_rank $moved_rank stands above $second_rank"
cases=$((cases + 1))

# --- callout: a paragraph nests in a callout as its last block -----------------------------------------

section=callout
tt_block "$section" "$spec_doc" paragraph "The paragraph that moves into the callout"
readonly callout_paragraph="$tt_node"
tt_typed "$section" "$spec_doc" tx
readonly callout="$tt_node"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$callout_paragraph" "$callout"
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $callout_paragraph under $callout"
for file in "$work/new.md" "$work/head.md"; do
  last="$(tt_container "$file" "callout{#tx" | awk '{ previous = line; line = $0 } END { print previous }')"
  [[ "$last" == *"The paragraph that moves into the callout"* ]] ||
    fail "$section" "the text nests $callout_paragraph in $callout" "the new version of $key holds $callout_paragraph outside $callout, want inside it"
done
tt_stored_prose "$section" spec "$callout_paragraph" "$callout"
tt_far "$section" "$callout_paragraph" text_parent
[[ "$answer" == "$callout" ]] || fail "$section" "the text_parent of $callout_paragraph is $callout" "it names '$answer'"
pass "$section" "the text holds $callout_paragraph as the last block of $callout; both part_of and text_parent name $callout"
cases=$((cases + 1))

# --- decision: under a decision, whose content is its own words, the text stays flat -----------------

section=decision
tt_typed "$section" "$spec_doc" td
readonly decision="$tt_node"
tt_block "$section" "$spec_doc" paragraph "The paragraph the agent moves under the decision"
readonly decision_paragraph="$tt_node"
tt_decision "$section" "$decision"
readonly decision_sum="$tt_sum"
tree_person_send POST "/api/v1/artifacts/$spec_doc/blocks/td/keep" "$(jq -cn --arg s "$decision_sum" '{text_sha256: $s}')"
tree_want 200 "$section" "alice keeps the decision $decision"
tt_quiet "$section"
tt_version "$section" "$spec_doc"
decision_version="$tt_number"
tt_container "$work/version.md" "decision{#td" >"$work/decision-version-before"
tt_head "$section" "$spec_doc"
tt_container "$work/head.md" "decision{#td" >"$work/decision-head-before"
tt_move "$section" agent "$key" "$decision_paragraph" "$decision"
tt_quiet "$section"
tt_version "$section" "$spec_doc"
[[ "$tt_number" == "$decision_version" ]] ||
  fail "$section" "the move under $decision writes no version" "the move of $decision_paragraph under $decision wrote version $tt_number of the spec, $decision_version before"
tt_head "$section" "$spec_doc"
for render in version head; do
  tt_container "$work/$render.md" "decision{#td" >"$work/decision-after"
  if grep -qF "The paragraph the agent moves under the decision" "$work/decision-after"; then
    fail "$section" "the text holds $decision_paragraph outside $decision" "the new version of $key holds $decision_paragraph inside $decision, want outside it"
  fi
  cmp -s "$work/decision-$render-before" "$work/decision-after" ||
    fail "$section" "$decision holds the blocks it held, in the $render render" "it reads $(head -c 300 "$work/decision-after")"
done
tt_stored_prose "$section" spec "$decision_paragraph" "$decision"
tt_decision "$section" "$decision"
[[ "$tt_sum" == "$decision_sum" && "$tt_kept" == "$person_login" && "$tt_waiting" == false ]] ||
  fail "$section" "$decision reads the text it was kept at" "the decision $decision reads text_sha256 $tt_sum after the move, want $decision_sum, kept by '$tt_kept', waiting_review $tt_waiting"
pass "$section" "the agent's move of $decision_paragraph under $decision is taken, the text stays flat, and $decision stays kept by $tt_kept at $tt_sum"
cases=$((cases + 1))

# --- list paragraph: a paragraph nests in a list item after its first paragraph -----------------------

section="list-paragraph"
tt_block "$section" "$spec_doc" list_item "The list item that holds one paragraph"
readonly list_item="$tt_node"
tt_block "$section" "$spec_doc" paragraph "The paragraph that moves into the list item"
readonly list_paragraph="$tt_node"
tt_before "$section" "$spec_doc"
tt_move "$section" person "$key" "$list_paragraph" "$list_item"
tt_after "$section" "$spec_doc" "$spec_ask" "the move of $list_paragraph under $list_item"
for file in "$work/new.md" "$work/head.md"; do
  text="$(<"$file")"
  [[ "$text" == *"- The list item that holds one paragraph."$'\n\n'"  The paragraph that moves into the list item."* ]] ||
    fail "$section" "the text nests $list_paragraph in $list_item" "the new version of $key holds $list_paragraph outside $list_item, want inside it after its first paragraph"
done
tt_stored_prose "$section" spec "$list_paragraph" "$list_item"
tt_far "$section" "$list_paragraph" text_parent
[[ "$answer" == "$list_item" ]] || fail "$section" "the text_parent of $list_paragraph is $list_item" "it names '$answer'"
pass "$section" "the text holds $list_paragraph in $list_item after its first paragraph; both part_of and text_parent name $list_item"
cases=$((cases + 1))

# --- table: under a block whose content is its own words, the text stays flat --------------------------

section=table
tt_typed "$section" "$spec_doc" td2
readonly table_decision="$tt_node"
for row in \
  "requirement|$spec_doc|trq|A paragraph that moves under the requirement of the table|spec" \
  "criterion|$spec_doc|tkq|A paragraph that moves under the criterion of the table|spec" \
  "ask|$spec_doc|taq|A paragraph that moves under the ask of the table|spec" \
  "reading|$spec_doc|tgr|A paragraph that moves under the reading of the table|spec" \
  "check|$verification_doc|tc2|A paragraph of the Verification document that moves under its check|verification" \
  "plan_task|$plan_doc|tt1|A paragraph of the plan that moves under its plan task|plan" \
  "callout|$spec_doc|tc|A paragraph that moves under the callout inside the decision|spec"; do
  IFS='|' read -r row_type doc block words view <<<"$row"
  tt_typed "$section" "$doc" "$block"
  block_node="$tt_node"
  tt_block "$section" "$doc" paragraph "$words"
  paragraph="$tt_node"
  tt_quiet "$section"
  tt_version "$section" "$doc"
  row_version="$tt_number"
  tt_head "$section" "$doc"
  cp "$work/head.md" "$work/row-head"
  tt_token "$section" "$doc" "$block"
  row_token="$answer"
  row_sum=""
  if [[ "$block" == tc ]]; then
    tt_decision "$section" "$table_decision"
    row_sum="$tt_sum"
  fi
  tt_move "$section" person "$key" "$paragraph" "$block_node"
  tt_quiet "$section"
  tt_version "$section" "$doc"
  [[ "$tt_number" == "$row_version" ]] ||
    fail "$section" "row $row_type writes no version" "row $row_type: the move of $paragraph wrote version $tt_number of $doc, want none"
  tt_head "$section" "$doc"
  cmp -s "$work/row-head" "$work/head.md" ||
    fail "$section" "row $row_type leaves the head render as it was" "row $row_type: the move of $paragraph changed the head render of $doc"
  tt_stored_prose "$section (row $row_type)" "$view" "$paragraph" "$block_node"
  tt_token "$section" "$doc" "$block"
  [[ "$answer" == "$row_token" ]] || fail "$section" "row $row_type: the block's token stands" "row $row_type: $block_node reads token $answer, want $row_token"
  if [[ -n "$row_sum" ]]; then
    tt_decision "$section" "$table_decision"
    [[ "$tt_sum" == "$row_sum" ]] ||
      fail "$section" "row $row_type: the decision's text_sha256 stands" "row $row_type: $table_decision reads text_sha256 $tt_sum, want $row_sum"
  fi
  pass "$section" "row $row_type: the move of $paragraph under $block_node is taken, writes no version, and leaves the text and the block's token as they were"
done
cases=$((cases + 1))

# --- emptied: a move that would leave a container with no block of its own is refused -----------------

section=emptied
tt_block "$section" "$spec_doc" paragraph "A top-level paragraph before the emptied containers"
readonly before_containers="$tt_node"
tt_typed "$section" "$spec_doc" ty
readonly callout_y="$tt_node"
tt_typed "$section" "$spec_doc" ty2
readonly callout_y2="$tt_node"
tt_block "$section" "$spec_doc" list_item "M's first paragraph"
readonly item_m="$tt_node" item_m_id="$tt_block_id"
tt_block "$section" "$spec_doc" paragraph "Y holds this one paragraph"
readonly of_y="$tt_node"
tt_block "$section" "$spec_doc" paragraph "Y2 holds this one paragraph"
readonly of_y2="$tt_node"
tt_block "$section" "$spec_doc" paragraph "M's first paragraph"
readonly of_m="$tt_node"
tt_quiet "$section"
# tt_counts SECTION: the spec's version count, the store's tuple count and the count of the
# doc_updates rows of the spec's room, its own document, in answer.
tt_counts() {
  with_vars "$1" "count the spec's versions, the store's tuples and the spec's room updates" \
    "select (select count(*) from artifact_versions where artifact_id = :'doc'::uuid) || '|' || (select count(*) from graph_tuples)
            || '|' || (select count(*) from doc_updates where artifact_id = :'doc'::uuid)" doc="$spec_doc"
}
for out in "$of_y|$callout_y|ty|callout" "$of_y2|$callout_y2|ty2|callout" "$of_m|$item_m|$item_m_id|list item"; do
  IFS='|' read -r paragraph container id kind <<<"$out"
  tt_far "$section" "$paragraph" part_of
  [[ "$answer" == "$container" ]] || fail "$section" "$paragraph stands under $container" "it stands under '$answer' before its move"
  tt_counts "$section"
  counts_before="$answer"
  tt_tag "$section" "$paragraph"
  tt_body "$section" person "issue/$key" "$before_containers"
  tt_patch person "$key" "$paragraph" "$tt_placed" "$answer"
  code="$(jq -r '.code // ""' "$work/body" 2>/dev/null)" || code=""
  [[ "$status" == 409 && "$code" == CONTAINER_WOULD_EMPTY ]] ||
    fail "$section" "the move of $paragraph out of $container is refused" "the move of $paragraph out of $container answered $status${code:+ $code}, want 409 CONTAINER_WOULD_EMPTY: $(body)"
  jq -e --arg k "$kind" --arg i "#$id" '.error | contains($k) and contains($i)' "$work/body" >/dev/null ||
    fail "$section" "the refusal names the $kind $container" "it reads: $(body)"
  tt_counts "$section"
  [[ "$answer" == "$counts_before" ]] ||
    fail "$section" "the refused move stores nothing" "the move of $paragraph out of $container changed the spec's versions, the store's tuples and the room's updates from $counts_before to $answer, want no change"
  tt_far "$section" "$paragraph" part_of
  [[ "$answer" == "$container" ]] ||
    fail "$section" "the refused move keeps $paragraph under $container" "the move of $paragraph out of $container changed its part_of to $answer, want no change"
  pass "$section" "the move of $paragraph out of the $kind $container answers 409 CONTAINER_WOULD_EMPTY naming it, and stores nothing"
done
cases=$((cases + 1))

# --- stored: a later pass keeps every place a move wrote ------------------------------------------------

section=stored
tree_person_send POST "/api/v1/artifacts/$spec_doc/edits" \
  '{"ops": [{"op": "replace", "find": "The first criterion of the text cases.", "with": "The first criterion of the text cases, reworded."}]}'
tree_want 200 "$section" "alice changes the words of $tk1 alone"
tt_quiet "$section"
for index in "${!stored_nodes[@]}"; do
  node="${stored_nodes[$index]}"
  want="${stored_parents[$index]}"
  tt_far "$section" "$node" part_of
  part_of="$answer"
  tt_value "$section" "$node" placement
  [[ "$answer" == stored && "$part_of" == "$want" ]] ||
    fail "$section" "the next index pass keeps $node where its move put it" "$node holds the placement '$answer' and part_of $part_of after the next index pass, want stored under $want"
done
pass "$section" "after the next index pass every one of the ${#stored_nodes[@]} moved blocks holds the placement stored and the part_of its move wrote"
tt_rooms "$section" "after the last move"
cases=$((cases + 1))

printf 'PASS: %s complete %d cases %d moves\n' "$driver" "$cases" "$moves"
