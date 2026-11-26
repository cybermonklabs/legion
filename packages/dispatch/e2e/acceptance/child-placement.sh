#!/usr/bin/env bash
# The check c-child-placement: a child issue hangs under the words that filed it, through the move
# of R7, and the placement and the child_of relation move on their own. It runs against the server
# of `bun run dispatch:local`, on the database that server serves, and seeds every issue it reads.
#
#   P1 As the person DEV_SIGNIN_LOGIN signs in, it creates the issue A and writes a comment on it,
#      creates under A the child C1 filed from that comment and the child C2 with no placement, and
#      creates the issue B outside A's tree.
#   P2 The child C1 holds child_of and no part_of, so A's tree read answers its placement_seq as the
#      seq of its current child_of tuple, which the driver reads from graph_tuples beside it. It
#      places C1 under the comment through PATCH /api/v1/issues/{A}/nodes/{C1's store address} with
#      the body {parent} and an If-Match naming that placement_seq, sent with the person's cookie,
#      and the same PATCH with the same tag again answers 412 PRECONDITION_FAILED and moves nothing,
#      since the move wrote C1's first part_of. A's tree read answers C1 under the comment, and C2
#      under A at the seq of C2's current child_of.
#   P3 It moves C1 under B by writing its child_of (PATCH /api/v1/issues/{C1} {parent: B}). B's tree
#      read answers C1 under B, the store holds no current part_of tuple from C1 to the comment,
#      and A's tree read answers C2 under A and C1 nowhere. Where C1 still stands under the
#      comment, the failure names C1, the comment and B.
#
# Inputs: DATABASE_URL (the database the server serves, on this machine, which psql reads),
# DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (the agent bearer, read here and
# never printed), PROJECT (an existing project key) and DEV_SIGNIN_LOGIN (the login the server's
# dev sign-in route signs in: `alice` on `bun run dispatch:local`). Each unmet input is exit 2.
# The person writes every issue, the comment and both moves; the agent bearer closes the issues
# when the run ends, also when a check fails.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line. The
# last line is `PASS: child-placement.sh complete 3 sections`. Every check is `cmd || fail …`,
# never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice \
         bash e2e/acceptance/child-placement.sh
       (run from packages/dispatch against `bun run dispatch:local`, whose dev sign-in route signs
       DEV_SIGNIN_LOGIN in; DATABASE_URL names the database that server serves, on this machine;
       DISPATCH_TOKEN_FILE may stand in for DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands curl jq psql
# shellcheck disable=SC2031 # the run's own DATABASE_URL
loopback_database_url "$DATABASE_URL" || {
  printf 'child-placement.sh: DATABASE_URL names no single loopback host; the driver reads the store it names\n' >&2
  usage
  exit 2
}
read_bearer

readonly driver=child-placement.sh
# shellcheck disable=SC2031 # the caller's DISPATCH_URL; only lib.sh's `cli` exports another
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2031 # the run's own DATABASE_URL
readonly database_url="$DATABASE_URL"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="child-placement-$stamp"

work_dir
tree_init

keys=()
cleanup() {
  local code=$? index
  trap - EXIT INT TERM
  for ((index = ${#keys[@]} - 1; index >= 0; index--)); do
    close_issue "${keys[$index]}"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# child_placement_create SECTION TITLE JSON: the person creates an issue of `project` titled TITLE,
# the members of the JSON object JSON added to the create, forced past the near-copy check; sets
# child_placement_key and adds it to the issues the run closes.
child_placement_create() {
  tree_person_send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$2" --argjson more "$3" \
    '{project: $p, title: $t, spec: ("The issue \"" + $t + "\" of the child placement check."), force: true} + $more')"
  tree_want 201 "$1" "$tree_person creates the issue \"$2\""
  tree_field "$1" "create the issue \"$2\"" .key
  tree_key "$1" "create the issue \"$2\"" "$tree_value"
  child_placement_key="$tree_value"
  keys+=("$tree_value")
}

# child_placement_node SECTION ISSUE KIND ID: the node of kind KIND and id ID that the tree read
# last kept answers, as one JSON object, in child_placement_found; empty where it answers none.
child_placement_node() {
  child_placement_found="$(jq -c --arg k "$3" --arg i "$4" 'first(.nodes[] | select(.kind == $k and .id == $i)) // empty' \
    "$work/tree.json")" || fail "$1" "read the tree of $2" "unreadable: $(head -c 600 "$work/tree.json")"
}

# child_placement_tree SECTION ISSUE: the person reads ISSUE's tree, kept in $work/tree.json.
child_placement_tree() {
  tree_read person "$2"
  tree_want 200 "$1" "read the tree of $2"
  cp "$work/body" "$work/tree.json"
}

# child_placement_under SECTION ISSUE CHILD KIND ID: the tree read of ISSUE last kept answers the
# child issue CHILD under the node of kind KIND and id ID.
child_placement_under() {
  child_placement_node "$1" "$2" issue "$3"
  [[ -n "$child_placement_found" ]] || fail "$1" "the tree read of $2 answers $3" "it answers no node of $3"
  jq -e --arg k "$4" --arg i "$5" '.parent.kind == $k and .parent.id == $i' <<<"$child_placement_found" >/dev/null ||
    fail "$1" "the tree read of $2 answers $3 under $4 $5" "it answers $3 under $(jq -c '.parent' <<<"$child_placement_found")"
  pass "$1" "the tree read of $2 answers $3 under $4 $5"
}

# child_placement_seqs SECTION KIND ISSUE: the seqs of the current KIND tuples out of the issue node
# ISSUE - for each far end, the newest tuple naming it, kept while it asserts - sorted and joined by
# commas, in $answer.
child_placement_seqs() {
  with_vars "$1" "read the seqs of the current $2 tuples out of $3" \
    "select coalesce(string_agg(t.seq::text, ',' order by t.seq), '')
     from (select distinct on (to_kind, to_id) seq, asserted from graph_tuples
           where from_kind = 'issue' and from_id = :'issue' and kind = :'kind' and to_kind <> 'value'
           order by to_kind, to_id, seq desc) t
     where t.asserted" \
    issue="$3" kind="$2"
}

# child_placement_child_of_tag SECTION ISSUE CHILD: the child issue CHILD holds no current part_of
# and one current child_of, and the tree read of ISSUE last kept answers CHILD's placement_seq as
# that child_of's seq (R8), which it sets in child_placement_tag. Where the read answers null or
# another number, the failure names the child, the answer and the seq it wants.
child_placement_child_of_tag() {
  local child_of seq
  tree_node_far "$1" "issue/$3" part_of
  [[ -z "$answer" ]] || fail "$1" "$3 holds no part_of before a move places it" "its current part_of tuples name '$answer'"
  child_placement_seqs "$1" child_of "$3"
  [[ "$answer" =~ ^[0-9]+$ ]] || fail "$1" "$3 holds one current child_of" "its current child_of tuples have the seqs '$answer'"
  child_of="$answer"
  child_placement_node "$1" "$2" issue "$3"
  [[ -n "$child_placement_found" ]] || fail "$1" "the tree read of $2 answers $3" "it answers no node of $3"
  seq="$(jq -r '.placement_seq | if . == null then "null" else tostring end' <<<"$child_placement_found")"
  [[ "$seq" == "$child_of" ]] ||
    fail "$1" "the tree read of $2 answers the placement_seq of $3, which holds no part_of, as the seq of its current child_of, $child_of" \
      "it answers the child $3 at placement_seq $seq: $child_placement_found"
  child_placement_tag="$child_of"
  pass "$1" "the tree read of $2 answers $3, which holds no part_of, at placement_seq $child_of, the seq of its current child_of"
}

# --- P1: the issue, the comment that files its child, the two children and the other parent ---------

tree_sign_in P1 "$DEV_SIGNIN_LOGIN"
child_placement_create P1 "Child placement check: the first issue $stamp" '{}'
readonly first="$child_placement_key"
corpus_comment person P1 "$first" "Which child takes the placement work? File one for it."
readonly comment_id="$corpus_item"
tree_uuid P1 "comment on $first" "$comment_id"
readonly comment_ref="dispatch://$first/comment/$comment_id"
pass P1 "created $first, on which $tree_person wrote the comment $comment_ref"
child_placement_create P1 "Child placement check: the child filed from the comment $stamp" \
  "$(jq -cn --arg p "$first" --arg f "$comment_ref" '{parent: $p, filed_from: $f}')"
readonly placed="$child_placement_key"
expect P1 "$placed stands under $first, filed from $comment_ref" \
  "$(jq -cn --arg p "$first" --arg f "$comment_ref" '[$p, $f]')" '[.parent, .filed_from]'
child_placement_create P1 "Child placement check: the child with no placement $stamp" \
  "$(jq -cn --arg p "$first" '{parent: $p}')"
readonly unplaced="$child_placement_key"
expect P1 "$unplaced stands under $first" "$(jq -cn --arg p "$first" '$p')" '.parent'
child_placement_create P1 "Child placement check: the second parent $stamp" '{}'
readonly second="$child_placement_key"
pass P1 "created $placed and $unplaced under $first, and $second outside its tree"

# --- P2: the child is placed under the words that filed it ---------------------------------------

child_placement_tree P2 "$first"
child_placement_node P2 "$first" comment "$comment_id"
[[ -n "$child_placement_found" ]] || fail P2 "the tree read of $first answers the comment $comment_id" "it answers no node of it"
child_placement_child_of_tag P2 "$first" "$placed"
readonly placement_seq="$child_placement_tag"
place_body="$(jq -cn --arg c "comment/$comment_id" '{parent: $c}')"
readonly place_body
tree_node_send PATCH person "$first" "issue/$placed" "$placement_seq" "$place_body" ""
tree_want 200 P2 "$tree_person places $placed under the comment $comment_id with the tag of its child_of"
expect P2 "the move of $placed answers the comment as its parent" true \
  --arg c "$comment_id" '.parent | type == "string" and endswith($c)'
tree_field P2 "the move of $placed answers its new placement_seq" .placement_seq
[[ "$tree_value" =~ ^[0-9]+$ && "$tree_value" != "$placement_seq" ]] ||
  fail P2 "the move of $placed answers a new placement_seq" "it answers $tree_value, and the tag it took was $placement_seq"
readonly moved_seq="$tree_value"
tree_node_send PATCH person "$first" "issue/$placed" "$placement_seq" "$place_body" ""
tree_want 412 P2 "the same move of $placed again, with the tag of its child_of the move made stale"
expect P2 "the repeat of the move of $placed answers PRECONDITION_FAILED" '"PRECONDITION_FAILED"' .code
tree_node_far P2 "issue/$placed" part_of
[[ "$answer" == "comment/$comment_id" ]] ||
  fail P2 "the store holds the one part_of tuple from $placed to the comment $comment_id" "its current part_of tuples name '$answer'"
child_placement_seqs P2 part_of "$placed"
[[ "$answer" == "$moved_seq" ]] ||
  fail P2 "the refused repeat moves nothing: $placed stands at placement_seq $moved_seq" "its current part_of tuples have the seqs '$answer'"
pass P2 "the store holds the one part_of tuple from $placed to the comment $comment_id, at the seq $moved_seq the move answered"
child_placement_tree P2 "$first"
child_placement_under P2 "$first" "$placed" comment "$comment_id"
child_placement_under P2 "$first" "$unplaced" issue "$first"
child_placement_child_of_tag P2 "$first" "$unplaced"

# --- P3: child_of moves the child, and the placement under the old words goes ----------------------

tree_person_send PATCH "/api/v1/issues/$placed" "$(jq -cn --arg p "$second" '{parent: $p}')"
tree_want 200 P3 "$tree_person moves $placed under $second through its child_of"
expect P3 "$placed stands under $second" "$(jq -cn --arg p "$second" '$p')" '.parent'
# child_placement_off_comment SECTION ISSUE: where the tree read of ISSUE last kept answers the
# child under the comment of its old parent, the failure names the child, the comment and the new
# parent.
child_placement_off_comment() {
  child_placement_node "$1" "$2" issue "$placed"
  [[ -z "$child_placement_found" ]] ||
    ! jq -e --arg c "$comment_id" '.parent.kind == "comment" and .parent.id == $c' <<<"$child_placement_found" >/dev/null ||
    fail "$1" "a move of child_of takes $placed off the comment of its old parent" \
      "the tree read of $2 answers the child $placed under the comment $comment_id of $first, where its new parent $second is to hold it"
}
child_placement_tree P3 "$first"
child_placement_off_comment P3 "$first"
child_placement_tree P3 "$second"
child_placement_off_comment P3 "$second"
child_placement_under P3 "$second" "$placed" issue "$second"
tree_node_far P3 "issue/$placed" part_of
[[ ",$answer," != *",comment/$comment_id,"* ]] ||
  fail P3 "the move of $placed under $second retracts its part_of to the comment $comment_id" \
    "the child $placed still holds its part_of to the comment $comment_id of $first under its new parent $second: its current part_of tuples name '$answer'"
pass P3 "the move of $placed under $second retracted its part_of to the comment $comment_id: its current part_of tuples name '${answer:-none}'"
child_placement_tree P3 "$first"
child_placement_under P3 "$first" "$unplaced" issue "$first"
child_placement_node P3 "$first" issue "$placed"
[[ -z "$child_placement_found" ]] ||
  fail P3 "the tree read of $first no longer answers $placed" "it answers $child_placement_found"
pass P3 "the tree read of $first no longer answers $placed"

pass "$driver" "complete 3 sections"
