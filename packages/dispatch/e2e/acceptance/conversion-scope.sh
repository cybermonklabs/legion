#!/usr/bin/env bash
# The check c-conversion-scope: the conversion touches the issues it must and leaves the rest
# exactly as they are (spec requirements r-conversion-scope and r-child-view).
#
# It empties the database DATABASE_URL names (a database on this machine named
# infra_1253_upgrade, the one this issue's upgrade drivers share, dropped and created again), boots
# the release before this branch on it through lib-baseline.sh (BASELINE_SHA, or BASELINE_BINARY,
# as its head comment states them), and writes four issues through that server's API, so that
# every document the conversion reads is one the earlier release wrote:
#   - a `done` issue, with its spec (a requirement and an agent's decision holding a reading, the
#     decision node the release before makes of a section), its verification.md and its plan.md,
#     one ask a person answered and one comment thread a person wrote, and no decisions document;
#   - an in-flight parent, with a discussion (a person's comment of two blocks, an agent's reply to
#     its second block and the person's reply, and an ask the person answered) and three
#     documents. Its spec holds four requirements whose `From:` lines cite one ask of the parent,
#     one comment of it, one ask of the unrelated `done` issue, and that unrelated ask first and
#     the parent's comment second; its verification.md holds one check serving each of them; a
#     person approved its spec;
#   - a first child of the parent, whose spec holds no requirement of its own, with one comment an
#     agent wrote and no ask;
#   - a second child of the parent, whose spec holds a requirement of its own.
# The person's writes (the parent's discussion, the done issue's thread and the approval) are
# DEV_SIGNIN_LOGIN's, through the dev sign-in; every other write is the agent bearer's.
#
# It records the digest of every version of every document of the four issues, stops that server,
# boots this head over the same database with the hold of R6's step (lib-tree.sh's tree_boot_head),
# waits for the backfill and for this head's build announcement, and runs
# `envoy-dispatch convert-issues --project PROJECT`. Then:
#   - done: the done issue's documents are byte for byte as captured, every version of each, and
#     its room count (document_rooms rows of its documents and its issue_trees row) is the one read
#     before the conversion; each changed document is printed with the issue key and its name, and
#     the run fails. Its ?view=decisions answers its decision nodes, their readings and their review
#     state as the earlier release's decisions read answered them, and its tree read answers
#     `converted: false`, its three document ids, its ask and the thread the person wrote in;
#   - redraw: the parent reads `converted: true`; its discussion is a graph (the person's comment,
#     both replies under it, and the ask); every requirement hangs where its `From:` line puts it,
#     under the first conversation node of the parent the line cites, else under the issue, and the
#     count of requirements so redrawn equals the count of requirements before, else the run prints
#     each requirement that hangs elsewhere, by its id, and fails; every check hangs under the
#     requirement it serves; each requirement and check keeps its author and its words; the parent
#     holds no criterion node; its ?view=spec answers its four requirements and no prose node; and
#     its spec still reads approved, by a carried review at the version the conversion wrote;
#   - children: the first child's tree read answers no conversation node (a node of kind comment,
#     comment_block, message or ask of its own issue), each one found printed, and GET
#     /api/v1/issues/{key}/asks of it answers no ask of kind approval; the second child reads
#     `converted: true`, no view, and its ?view=spec answers its own requirement as the query's;
#   - plants: an ask the agent raises on the second child, and an approval request the agent opens
#     on its spec, are each found by the same two reads, so the absences above are ones those
#     searches could have found.
#
# Then the automatic case of R6, in a second pass over the emptied database: the same four issues,
# written through the earlier release, whose server stays live; this head booted beside it without
# the hold, and no command run. It waits until this head's log holds two lines naming the live
# process that lists no issue_room (its instance and its commit, read from server_processes), and
# reads `converted: false` in the tree read of each of the four issues; at the bound it fails,
# naming each issue that still reads `converted: false`. It stops the earlier release's server and
# polls every 2 s, failing after 300 s and naming each issue that still reads `converted: false`,
# until the parent and each child read `converted: true`; it reads `converted: false` for the done
# issue, whose documents are byte for byte as captured; and it finds in this head's log one line
# for each issue: converted for the parent and each child, and passed over, with its reason, for
# the done issue. A line is the step's for an issue where it names the issue's key as a word of its
# own and says `converted` (and neither `passed over` nor a deferral) or `passed over` and `done`.
#
# Inputs: DATABASE_URL (infra_1253_upgrade on this machine), DISPATCH_TOKEN (the agent bearer both
# servers accept: e2e-token), PROJECT (the project it creates: CORE), DEV_SIGNIN_LOGIN (the person:
# alice), BASELINE_SHA or BASELINE_BINARY (lib-baseline.sh), and `envoy-dispatch` on PATH, built
# stamped with this head's commit as the Verification preamble builds it.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. The last line is `PASS: conversion-scope.sh complete <N> sections`. Every
# check is `cmd || fail …`, never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1253_upgrade DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice BASELINE_SHA=<the commit of the release this branch lands after> \
         bash e2e/acceptance/conversion-scope.sh
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

require_env DATABASE_URL DISPATCH_TOKEN PROJECT DEV_SIGNIN_LOGIN
require_commands cp curl date diff grep jq psql sed sort
readonly driver=conversion-scope.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"
session="conversion-scope-$(date +%s)-$$"
readonly session
# The typed blocks of the block schema; a block node of any other supertag is prose.
readonly typed_supertags='["ask","callout","check","criterion","decision","plan_task","reading","requirement"]'

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

# conversion_scope_empty_database: drops and creates the database database_url names, so the
# earlier release's Store.Migrate writes its schema and the four issues are its only content. It
# takes only infra_1253_upgrade on this machine, the database of this issue's upgrade drivers, and
# refuses every other with status 2.
conversion_scope_empty_database() {
  local scheme="" authority="" name="" query=""
  # The URL's parts are taken before loopback_database_url runs, whose own match replaces
  # BASH_REMATCH.
  if [[ "$database_url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]]; then
    scheme="${BASH_REMATCH[1]}" authority="${BASH_REMATCH[3]}" name="${BASH_REMATCH[4]}" query="${BASH_REMATCH[5]}"
  fi
  if [[ -z "$name" ]] || ! loopback_database_url "$database_url"; then
    printf '%s: DATABASE_URL names no database on this machine; the run drops and creates infra_1253_upgrade alone\n' "$driver" >&2
    exit 2
  fi
  [[ "$name" == infra_1253_upgrade ]] || {
    printf '%s: DATABASE_URL names the database %s; the run drops and creates infra_1253_upgrade alone\n' "$driver" "$name" >&2
    exit 2
  }
  baseline_psql "${scheme}${authority}/postgres${query}" -q \
    -c "drop database if exists \"$name\" with (force)" -c "create database \"$name\"" \
    >"$work/empty-database" 2>&1 || fail setup "drop and create the database $name" "psql: $(<"$work/empty-database")"
  printf '%s: the database %s was dropped and created empty\n' "$driver" "$name"
}

# conversion_scope_key SECTION WHAT KEY: KEY is an issue key, which the SQL below names inline.
conversion_scope_key() {
  [[ "$3" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "$2" "'$3' is no issue key"
}

# conversion_scope_tree SECTION FILE ISSUE [VIEW]: the agent's tree read of ISSUE (lib-tree.sh's
# tree_read), every page of it followed through its cursor, as one answer in FILE: the first
# page's members and every page's nodes and tuples.
conversion_scope_tree() {
  local section="$1" file="$2" issue="$3" view="${4:-}" cursor="" pages=0
  : >"$work/pages"
  while true; do
    tree_read agent "$issue" "$view" "" "$cursor"
    tree_want 200 "$section" "read the tree of $issue${view:+ (?view=$view)}"
    cat "$work/body" >>"$work/pages"
    pages=$((pages + 1))
    cursor="$(jq -r '.cursor // ""' "$work/body")"
    [[ -n "$cursor" ]] || break
    ((pages < 500)) || fail "$section" "read the tree of $issue" "its cursor did not end after $pages pages"
  done
  jq -s '.[0] + {nodes: [.[].nodes[]?], tuples: [.[].tuples[]?]}' "$work/pages" >"$file" ||
    fail "$section" "read the tree of $issue" "the pages do not join: $(head -c 600 "$work/pages")"
}

# conversion_scope_digests FILE KEY…: one line per version of every document of each issue KEY,
# `<key> <slug> <artifact id> v<number> <sha256 of its stored markdown>`, read from the rows.
conversion_scope_digests() {
  local file="$1" key list=""
  shift
  for key in "$@"; do
    conversion_scope_key digests "read the digests" "$key"
    list+="${list:+, }'$key'"
  done
  query digests "read the digest of every version of every document of $*" "
    select coalesce(string_agg(a.issue_key || ' ' || a.slug || ' ' || a.id || ' v' || v.number || ' ' ||
             coalesce(encode(sha256(convert_to(v.markdown, 'UTF8')), 'hex'), 'no-markdown'),
             E'\n' order by a.issue_key, a.slug, v.number), '')
    from artifacts a join artifact_versions v on v.artifact_id = a.id
    where a.issue_key in ($list)"
  printf '%s\n' "$answer" >"$file"
}

# conversion_scope_same_documents SECTION KEY BEFORE AFTER: the documents of KEY in AFTER are
# byte for byte those of BEFORE, every version of each; each document that differs is printed
# with the issue key and its name, and the run fails.
conversion_scope_same_documents() {
  local section="$1" key="$2" changed
  grep "^$key " "$3" >"$work/same.before" || true
  grep "^$key " "$4" >"$work/same.after" || true
  [[ -s "$work/same.before" ]] || fail "$section" "the documents of $key were captured" "the capture holds no version of them"
  changed="$(diff "$work/same.before" "$work/same.after" | sed -n 's/^[<>] [^ ]* \([^ ]*\) .*/\1/p' | sort -u)" || true
  if [[ -n "$changed" ]]; then
    while IFS= read -r name; do
      printf 'changed document: issue %s document %s\n' "$key" "$name" >&2
    done <<<"$changed"
    fail "$section" "the documents of $key are byte for byte as captured" \
      "these documents of $key changed: $(tr '\n' ' ' <<<"$changed")"
  fi
  pass "$section" "the $(wc -l <"$work/same.before") versions of the documents of $key are byte for byte as captured"
}

# conversion_scope_converted ISSUE: the `converted` the agent's tree read of ISSUE answers, in
# tree_value ("error" where the read answers no 200).
conversion_scope_converted() {
  tree_read agent "$1"
  if [[ "$status" == 200 ]]; then
    tree_value="$(jq -r '.converted' "$work/body")"
  else
    tree_value="error: HTTP $status"
  fi
}

# The issues the seed wrote last, its parent's discussion and documents, and the done issue's
# ask and thread, which conversion_scope_seed sets.
done_key="" done_spec="" done_verification="" done_plan="" done_ask="" done_thread="" done_reply=""
parent_key="" parent_spec="" parent_comment="" parent_reply_agent="" parent_reply_person="" parent_ask=""
parent_approved_version="" child_one="" child_two="" child_two_spec=""

# conversion_scope_seed: writes the four issues through the server at `base`, the earlier
# release's, and waits until every document is settled and indexed.
conversion_scope_seed() {
  local section=seed
  # The done issue: three documents, an answered ask, a person's thread, an agent's decision with
  # its reading, and no decisions document, closed last.
  printf 'The done issue, which no conversion rewrites.\n\n:::requirement{#r-done}\nThe done issue keeps its rows in Postgres.\n:::\n\n::::decision{#d-done-store uncertainty="low"}\nThe done issue keeps its store in Postgres.\n\n:::reading{#g-done-store}\nThe agent reads the store as settled.\n:::\n::::\n' \
    >"$work/done-spec.md"
  corpus_issue "$section" "A done issue the conversion leaves alone" "$work/done-spec.md"
  done_key="$corpus_key" done_spec="$corpus_spec"
  conversion_scope_key "$section" "create the done issue" "$done_key"
  printf 'How the done issue is proved.\n\n:::check{#c-done serves="dispatch://%s/spec#b-r-done" surface="cli" run="bash e2e/acceptance/conversion-scope.sh" negative_control="Rewrite the done issue and the digests differ."}\nThe done issue keeps its rows.\n:::\n' \
    "$done_key" >"$work/done-verification.md"
  corpus_upload "$section" "$done_key" verification.md "$work/done-verification.md"
  done_verification="$corpus_doc"
  printf 'How the done issue was built.\n\n:::plan_task{#t-done serves="dispatch://%s/spec#b-r-done"}\nWrite the done issue.\n:::\n' \
    "$done_key" >"$work/done-plan.md"
  corpus_upload "$section" "$done_key" plan.md "$work/done-plan.md"
  done_plan="$corpus_doc"
  corpus_ask "$section" "$done_key" "Which store holds the done issue?"
  done_ask="$corpus_item"
  corpus_answer "$section" "$done_ask" "Postgres, for the done issue." Postgres
  corpus_comment person "$section" "$done_key" "Is the done issue's store settled?"
  done_thread="$corpus_item"
  corpus_comment person "$section" "$done_key" "It is: Postgres holds it." "$done_thread"
  done_reply="$corpus_item"
  pass "$section" "wrote the done issue $done_key: three documents, the ask $done_ask and the thread $done_thread"

  # The in-flight parent: its discussion first, so its requirements can cite it.
  printf 'The in-flight parent, which the conversion redraws.\n' >"$work/parent-spec.md"
  corpus_issue "$section" "An in-flight parent the conversion redraws" "$work/parent-spec.md"
  parent_key="$corpus_key" parent_spec="$corpus_spec"
  conversion_scope_key "$section" "create the parent" "$parent_key"
  corpus_comment person "$section" "$parent_key" "Which shapes must the parent hold?"$'\n\n'"Its second block asks who approves the spec."
  parent_comment="$corpus_item"
  corpus_comment agent "$section" "$parent_key" "The agent answers the second block: a person approves it." "$parent_comment" b2
  parent_reply_agent="$corpus_item"
  corpus_comment person "$section" "$parent_key" "Then the parent asks a person to approve its spec." "$parent_comment"
  parent_reply_person="$corpus_item"
  corpus_ask "$section" "$parent_key" "Which store holds the parent?"
  parent_ask="$corpus_item"
  corpus_answer "$section" "$parent_ask" "Postgres, for the parent." Postgres
  corpus_edit "$section" "write the four requirements of $parent_key" "$parent_spec" "$(jq -cn \
    --arg ask "dispatch://$parent_key/ask/$parent_ask" --arg comment "dispatch://$parent_key/comment/$parent_comment" \
    --arg other "dispatch://$done_key/ask/$done_ask" \
    '[{op: "insert", after: "end", markdown: (
      ":::requirement{#r-from-ask}\nThe parent keeps its rows in the store its ask chose.\n\nFrom: " + $ask + "\n:::\n\n" +
      ":::requirement{#r-from-comment}\nThe parent holds every shape the person asked for.\n\nFrom: " + $comment + "\n:::\n\n" +
      ":::requirement{#r-from-other}\nThe parent reads the store the done issue chose.\n\nFrom: " + $other + "\n:::\n\n" +
      ":::requirement{#r-from-both}\nThe parent asks a person to approve its spec.\n\nFrom: " + $other + " and " + $comment + "\n:::")}]')"
  {
    printf 'How the parent is proved.\n\n'
    for requirement in r-from-ask r-from-comment r-from-other r-from-both; do
      printf ':::check{#c-%s serves="dispatch://%s/spec#b-%s" surface="cli" run="bash e2e/acceptance/conversion-scope.sh" negative_control="Move %s and the check reads a gap."}\nThe parent meets %s.\n:::\n\n' \
        "${requirement#r-}" "$parent_key" "$requirement" "$requirement" "$requirement"
    done
  } >"$work/parent-verification.md"
  corpus_upload "$section" "$parent_key" verification.md "$work/parent-verification.md"
  printf 'How the parent is built.\n\n:::plan_task{#t-parent serves="dispatch://%s/spec#b-r-from-ask"}\nWrite the parent.\n:::\n' \
    "$parent_key" >"$work/parent-plan.md"
  corpus_upload "$section" "$parent_key" plan.md "$work/parent-plan.md"
  corpus_wait_settled "$section"
  corpus_review "$section" "$parent_spec" approved
  parent_approved_version="$tree_value"
  pass "$section" "wrote the parent $parent_key: its discussion, four requirements, their checks and a plan; $person_login approved its spec at version $parent_approved_version"

  # The first child: a spec of design prose alone, a check that serves its parent, an agent's
  # comment and no ask.
  printf 'The first child reads its parent'"'"'s requirements and holds none of its own.\n' >"$work/child-one-spec.md"
  corpus_issue "$section" "A child of the parent with no requirement of its own" "$work/child-one-spec.md" "$parent_key"
  child_one="$corpus_key"
  conversion_scope_key "$section" "create the first child" "$child_one"
  printf 'How the first child is proved.\n\n:::check{#c-child-one serves="dispatch://%s/spec#b-r-from-ask" surface="cli" run="bash e2e/acceptance/conversion-scope.sh" negative_control="Move the check and its parent reads a gap."}\nThe first child proves its parent'"'"'s store.\n:::\n' \
    "$parent_key" >"$work/child-one-verification.md"
  corpus_upload "$section" "$child_one" verification.md "$work/child-one-verification.md"
  corpus_comment agent "$section" "$child_one" "Status: the first child is written."
  pass "$section" "wrote the first child $child_one, with an agent's comment and no ask"

  # The second child: a requirement of its own.
  printf ':::requirement{#r-child-own}\nThe second child holds a requirement of its own.\n:::\n\nThe second child'"'"'s summary.\n' \
    >"$work/child-two-spec.md"
  corpus_issue "$section" "A child of the parent with a requirement of its own" "$work/child-two-spec.md" "$parent_key"
  child_two="$corpus_key" child_two_spec="$corpus_spec"
  conversion_scope_key "$section" "create the second child" "$child_two"
  pass "$section" "wrote the second child $child_two, whose spec holds r-child-own"

  tree_send PATCH "/api/v1/issues/$done_key" "$(jq -cn --argjson a "$(tree_actor)" '{status: "done", actor: $a}')"
  tree_want 200 "$section" "close $done_key"
  expect "$section" "$done_key is done" '"done"' .status
  corpus_wait_settled "$section"
}

# --- the first pass: the conversion by hand -------------------------------------------------------

conversion_scope_empty_database
# Read before lib.sh's read_bearer could unset it: both servers accept the run's DISPATCH_TOKEN.
baseline_start
# A copy of the earlier release's build, so the second pass boots it again without building it.
cp "$baseline_binary" "$work/baseline-dispatch"
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The issues the conversion's scope reads"

conversion_scope_seed
conversion_scope_digests "$work/digests.before" "$done_key" "$parent_key" "$child_one" "$child_two"
pass seed "captured the digest of every version of every document of the four issues: $(wc -l <"$work/digests.before") versions"
# The earlier release's decisions read of the done issue and the readings its spec holds.
tree_send GET "/api/v1/issues/$done_key/decisions"
tree_want 200 seed "read the decisions of $done_key through the earlier release"
jq -cS '[.decisions[]? | {block_id, kind, replaced, waiting_review, kept: (.kept != null)}] | sort_by(.block_id)' \
  "$work/body" >"$work/done-decisions.before"
[[ "$(<"$work/done-decisions.before")" != "[]" ]] ||
  fail seed "the earlier release answers the decision of $done_key" "its decisions read answers none: $(body)"
tree_send GET "/api/v1/artifacts/$done_spec/blocks"
tree_want 200 seed "read the blocks of the spec of $done_key"
jq -cS '[.[] | select(.type == "reading") | .id] | sort' "$work/body" >"$work/done-readings.before"
pass seed "the earlier release answers the decisions $(<"$work/done-decisions.before") and the readings $(<"$work/done-readings.before") of $done_key"
baseline_stop_server

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
query "$section" "read whether $done_key holds a decisions document" \
  "select count(*) from artifacts where issue_key = '$done_key' and slug = 'decisions-md'"
[[ "$answer" == 0 ]] || fail "$section" "$done_key holds no decisions document" "it holds $answer"
pass "$section" "$done_key holds no decisions document"
readonly room_count_sql="select (select count(*) from document_rooms r join artifacts a on a.id = r.artifact_id where a.issue_key = '$done_key')
  || ' document_rooms, ' || (select count(*) from issue_trees where issue_key = '$done_key') || ' issue_trees'"
query "$section" "read the room count of $done_key" "$room_count_sql"
readonly done_rooms_before="$answer"
pass "$section" "$done_key holds $done_rooms_before before the conversion"
conversion_scope_tree "$section" "$work/parent.before.json" "$parent_key"
jq -cS --arg k "$parent_key" '[.nodes[] | select((.supertag == "requirement" or .supertag == "check") and ((.issue // $k) == $k))
  | {id: .block_id, supertag, author, text}] | sort_by(.id)' "$work/parent.before.json" >"$work/parent-nodes.before"
requirements_before="$(jq '[.[] | select(.supertag == "requirement")] | length' "$work/parent-nodes.before")"
readonly requirements_before
[[ "$requirements_before" == 4 ]] ||
  fail "$section" "the parent $parent_key holds its four requirements before the conversion" \
    "its tree read answers $requirements_before: $(<"$work/parent-nodes.before")"
pass "$section" "the parent $parent_key holds $requirements_before requirements and their checks before the conversion"
tree_wait_announcement "$section"

section=convert
tree_convert "$section"
sed 's/^/  /' "$work/out"
conversion_scope_digests "$work/digests.after" "$done_key" "$parent_key" "$child_one" "$child_two"

# --- done: the done issue converts not at all ------------------------------------------------------

section=done
conversion_scope_same_documents "$section" "$done_key" "$work/digests.before" "$work/digests.after"
for key in "$parent_key" "$child_one" "$child_two"; do
  if diff <(grep "^$key " "$work/digests.before") <(grep "^$key " "$work/digests.after") >/dev/null; then
    printf 'unchanged: the documents of %s\n' "$key"
  else
    printf 'rewritten: the documents of %s\n' "$key"
  fi
done
query "$section" "read the room count of $done_key" "$room_count_sql"
[[ "$answer" == "$done_rooms_before" ]] ||
  fail "$section" "the room count of $done_key is unchanged" "it reads $answer, and read $done_rooms_before before the conversion"
pass "$section" "the room count of $done_key is unchanged: $answer"
conversion_scope_tree "$section" "$work/done.json" "$done_key"
cp "$work/done.json" "$work/body"
expect "$section" "$done_key reads converted: false" false '.converted'
expect "$section" "the tree read of $done_key answers its three document ids" \
  "$(jq -cnS --arg s "$done_spec" --arg v "$done_verification" --arg p "$done_plan" '{plan: $p, spec: $s, verification: $v}')" '.documents'
for node in "ask $done_ask" "comment $done_thread" "comment $done_reply"; do
  expect "$section" "the tree read of $done_key answers the ${node%% *} ${node#* }" true \
    --arg kind "${node%% *}" --arg id "${node#* }" '[.nodes[] | select(.kind == $kind and (.id | endswith($id)))] | length == 1'
done
conversion_scope_tree "$section" "$work/done-decisions.json" "$done_key" decisions
cp "$work/done-decisions.json" "$work/body"
expect "$section" "the ?view=decisions of $done_key answers its decision nodes and their review state" \
  "$(<"$work/done-decisions.before")" \
  '[.nodes[] | select(.decision != null) | .decision | {block_id, kind, replaced, waiting_review, kept: (.kept != null)}] | sort_by(.block_id)'
expect "$section" "the ?view=decisions of $done_key answers the readings of its decisions" \
  "$(<"$work/done-readings.before")" '[.nodes[] | select(.supertag == "reading") | .block_id] | sort'

# --- redraw: the in-flight parent ------------------------------------------------------------------

section=redraw
conversion_scope_tree "$section" "$work/parent.after.json" "$parent_key"
cp "$work/parent.after.json" "$work/body"
expect "$section" "$parent_key reads converted: true" true '.converted'
for node in "comment $parent_comment" "ask $parent_ask"; do
  expect "$section" "the discussion of $parent_key holds the ${node%% *} ${node#* }" true \
    --arg kind "${node%% *}" --arg id "${node#* }" '[.nodes[] | select(.kind == $kind and (.id | endswith($id)))] | length == 1'
done
for reply in "$parent_reply_agent" "$parent_reply_person"; do
  expect "$section" "the reply $reply hangs under the comment $parent_comment it answers" true \
    --arg id "$reply" --arg c "$parent_comment" \
    '[.nodes[] | select(.kind == "comment" and (.id | endswith($id)) and ((.parent.id // "") | contains($c)))] | length == 1'
done
# Where each requirement's From: line puts it: under the first conversation node of the parent it
# cites, else under the issue.
redrawn=0
misplaced=""
for placed in "r-from-ask ask $parent_ask" "r-from-comment comment $parent_comment" \
  "r-from-other issue $parent_key" "r-from-both comment $parent_comment"; do
  read -r requirement kind id <<<"$placed"
  found="$(jq -r --arg r "$requirement" --arg k "$parent_key" \
    '[.nodes[] | select(.supertag == "requirement" and .block_id == $r and ((.issue // $k) == $k))]
     | if length == 1 then .[0] | "\(.parent.kind // "none") \(.parent.id // "none")" else "\(length) nodes" end' \
    "$work/parent.after.json")"
  if [[ "$found" == "$kind "* && "${found#* }" == *"$id" ]]; then
    redrawn=$((redrawn + 1))
  else
    misplaced+="; $requirement hangs under $found, want $kind $id"
  fi
done
[[ "$redrawn" == "$requirements_before" ]] ||
  fail "$section" "every requirement of $parent_key hangs where its From: line puts it" \
    "$redrawn of $requirements_before requirements redrawn${misplaced}"
pass "$section" "all $redrawn requirements of $parent_key hang where their From: lines put them"
# Each check under the requirement it serves.
for requirement in r-from-ask r-from-comment r-from-other r-from-both; do
  expect "$section" "the check c-${requirement#r-} hangs under the requirement $requirement it serves" true \
    --arg c "c-${requirement#r-}" --arg r "$requirement" --arg k "$parent_key" \
    '(.nodes | map({key: .id, value: .}) | from_entries) as $by
     | [.nodes[] | select(.supertag == "check" and .block_id == $c and ((.issue // $k) == $k))
        | $by[.parent.id // ""] | select(. != null and .supertag == "requirement" and .block_id == $r)] | length == 1'
done
jq -cS --arg k "$parent_key" '[.nodes[] | select((.supertag == "requirement" or .supertag == "check") and ((.issue // $k) == $k))
  | {id: .block_id, supertag, author, text}] | sort_by(.id)' "$work/parent.after.json" >"$work/parent-nodes.after"
differs="$(jq -rn --slurpfile b "$work/parent-nodes.before" --slurpfile a "$work/parent-nodes.after" \
  '($a[0] | map({key: .id, value: .}) | from_entries) as $after
   | [$b[0][] | . as $was | $after[$was.id] as $now
      | if $now == null then "\($was.id) is gone"
        elif $now.author != $was.author then "\($was.id) reads the author \($now.author | tojson), was \($was.author | tojson)"
        elif $now.text != $was.text then "\($was.id) reads the words \($now.text | tojson), was \($was.text | tojson)"
        else empty end] | join("; ")')"
[[ -z "$differs" ]] || fail "$section" "each requirement and check of $parent_key keeps its author and its words" "$differs"
pass "$section" "each requirement and check of $parent_key keeps its author and its words"
expect "$section" "$parent_key holds no criterion node" '[]' --arg k "$parent_key" \
  '[.nodes[] | select(.supertag == "criterion" and ((.issue // $k) == $k)) | .id]'
conversion_scope_tree "$section" "$work/parent-spec.json" "$parent_key" spec
cp "$work/parent-spec.json" "$work/body"
expect "$section" "the ?view=spec of $parent_key answers its four requirements as the query's" \
  '["r-from-ask","r-from-both","r-from-comment","r-from-other"]' --arg k "$parent_key" \
  '[.nodes[] | select(.role == "query" and .supertag == "requirement" and ((.issue // $k) == $k)) | .block_id] | sort'
expect "$section" "the ?view=spec of $parent_key answers no prose node" '[]' --argjson typed "$typed_supertags" \
  '[.nodes[] | select(.kind == "block" and (.supertag as $s | $typed | index($s) | not)) | "\(.id) (\(.supertag))"]'
tree_send GET "/api/v1/artifacts/$parent_spec"
tree_want 200 "$section" "read the spec of $parent_key"
expect "$section" "the spec of $parent_key still reads approved, by $person_login's approval carried to the version the conversion wrote" \
  true --argjson was "$parent_approved_version" \
  '.approval | .state == "approved" and .version == .latest_version and .version > $was and .kind == "person"
   and ((.reason // "") | startswith("Carried forward from the approval of version"))'

# --- children: a child of an approved parent ------------------------------------------------------

section=children
conversion_scope_tree "$section" "$work/child-one.json" "$child_one"
cp "$work/child-one.json" "$work/body"
expect "$section" "$child_one reads converted: true" true '.converted'
readonly conversation_nodes='[.nodes[] | select((.kind == "comment" or .kind == "comment_block" or .kind == "message" or .kind == "ask")
  and ((.issue // $k) == $k)) | "\(.kind) \(.id) (\(.role), by \((.author // []) | map(.id // .login // .kind) | join(",")))"]'
found="$(jq -c --arg k "$child_one" "$conversation_nodes" "$work/child-one.json")"
[[ "$found" == "[]" ]] ||
  fail "$section" "the conversation view of $child_one answers nothing" "it answers these nodes of $child_one: $found"
pass "$section" "the conversation view of $child_one answers nothing: no comment, message or ask of its own"
tree_send GET "/api/v1/issues/$child_one/asks"
tree_want 200 "$section" "read the asks of $child_one"
expect "$section" "$child_one carries no approval request of its own" '[]' '[.[] | select(.kind == "approval") | .id]'
conversion_scope_tree "$section" "$work/child-two.json" "$child_two"
cp "$work/child-two.json" "$work/body"
expect "$section" "$child_two reads converted: true and is no view" '[true,null]' '[.converted, .view_of]'
expect "$section" "$child_two keeps its own requirement r-child-own" true --arg k "$child_two" \
  '[.nodes[] | select(.supertag == "requirement" and .block_id == "r-child-own" and ((.issue // $k) == $k))] | length == 1'
conversion_scope_tree "$section" "$work/child-two-spec.json" "$child_two" spec
cp "$work/child-two-spec.json" "$work/body"
expect "$section" "the ?view=spec of $child_two answers r-child-own as the query's" true --arg k "$child_two" \
  '[.nodes[] | select(.role == "query" and .supertag == "requirement" and .block_id == "r-child-own" and ((.issue // $k) == $k))] | length == 1'

# --- plants: the same two reads find a conversation node and an approval request on a child --------

section=plants
corpus_ask "$section" "$child_two" "Which store holds the second child?"
planted_ask="$corpus_item"
tree_send POST "/api/v1/artifacts/$child_two_spec/approval-requests" \
  "$(jq -cn --argjson a "$(tree_actor)" '{summary: "The second child asks for its own approval.", actor: $a}')"
tree_want 201 "$section" "open an approval request on the spec of $child_two"
conversion_scope_tree "$section" "$work/child-two-planted.json" "$child_two"
found="$(jq -c --arg k "$child_two" "$conversation_nodes" "$work/child-two-planted.json")"
jq -e --arg id "$planted_ask" --arg k "$child_two" \
  '[.nodes[] | select(.kind == "ask" and (.id | endswith($id)) and ((.issue // $k) == $k))] | length == 1' \
  "$work/child-two-planted.json" >/dev/null ||
  fail "$section" "the conversation read finds the ask $planted_ask planted on $child_two" "it answers: $found"
pass "$section" "the conversation read finds the ask $planted_ask planted on $child_two: $found"
tree_send GET "/api/v1/issues/$child_two/asks"
tree_want 200 "$section" "read the asks of $child_two"
expect "$section" "the asks read finds the approval request planted on $child_two" true \
  '[.[] | select(.kind == "approval")] | length == 1'

# --- automatic: the step converts once every live process serves the issue room --------------------

section=automatic
tree_stop_head
baseline_stop
conversion_scope_empty_database
BASELINE_BINARY="$work/baseline-dispatch"
# The bearer of the first pass, which baseline_start reads again and both servers accept.
DISPATCH_TOKEN="$token"
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN
base="$baseline_url"
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The issues the conversion's scope reads"
conversion_scope_seed
conversion_scope_digests "$work/digests.automatic" "$done_key" "$parent_key" "$child_one" "$child_two"
automatic_issues=("$done_key" "$parent_key" "$child_one" "$child_two")
readonly automatic_issues
pass "$section" "wrote the four issues again through the earlier release, whose server stays live: ${automatic_issues[*]}"
tree_boot_head head-automatic unheld
base="$tree_head_url"
query "$section" "read the live process that lists no issue_room" "
  select coalesce(string_agg(instance::text || ' ' || commit, E'\n'), '') from server_processes
  where not (coalesce(capabilities, '{}'::text[]) @> array['issue_room'])"
[[ -n "$answer" && "$answer" != *$'\n'* ]] ||
  fail "$section" "one live process lists no issue_room: the earlier release's" "server_processes holds: '${answer:-none}'"
read -r base_instance base_commit <<<"$answer"
pass "$section" "the earlier release's process $base_instance, commit $base_commit, lists no issue_room"

# conversion_scope_unconverted ISSUE…: the issues among ISSUE… whose tree read answers anything
# but `converted: true`, each with what it answers, in tree_value.
conversion_scope_unconverted() {
  local issue left=""
  for issue in "$@"; do
    conversion_scope_converted "$issue"
    [[ "$tree_value" == true ]] || left+=" $issue reads converted: $tree_value;"
  done
  tree_value="$left"
}

deadline=$((SECONDS + tree_wait_seconds))
while true; do
  named="$(grep -F -- "$base_instance" "$tree_head_log" | grep -cF -- "$base_commit")" || named=0
  printf 'the step at %s s: %s lines of %s name the live process %s\n' "$SECONDS" "$named" "$tree_head_log" "$base_instance"
  ((named < 2)) || break
  if ((SECONDS >= deadline)); then
    conversion_scope_unconverted "${automatic_issues[@]}"
    fail "$section" "this head's log holds two lines naming the live process that lists no issue_room within ${tree_wait_seconds} s" \
      "after ${tree_wait_seconds} s it holds $named;$tree_value"
  fi
  sleep "$tree_poll_seconds"
done
pass "$section" "this head's log holds $named lines naming the live process $base_instance that lists no issue_room"
for issue in "${automatic_issues[@]}"; do
  conversion_scope_converted "$issue"
  [[ "$tree_value" == false ]] ||
    fail "$section" "$issue reads converted: false while a process that lists no issue_room is live" "it reads converted: $tree_value"
done
pass "$section" "each of ${automatic_issues[*]} reads converted: false while the earlier release's process is live"
baseline_stop_server
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  conversion_scope_unconverted "$parent_key" "$child_one" "$child_two"
  printf 'conversion at %s s:%s\n' "$SECONDS" "${tree_value:- every in-flight issue reads converted: true}"
  [[ -n "$tree_value" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the step converts the parent and each child within ${tree_wait_seconds} s of the last process that lists no issue_room stopping" \
      "after ${tree_wait_seconds} s:$tree_value"
  sleep "$tree_poll_seconds"
done
pass "$section" "the step converted $parent_key, $child_one and $child_two with no command run"
conversion_scope_converted "$done_key"
[[ "$tree_value" == false ]] || fail "$section" "$done_key reads converted: false" "it reads converted: $tree_value"
pass "$section" "$done_key reads converted: false"
conversion_scope_digests "$work/digests.automatic.after" "$done_key"
conversion_scope_same_documents "$section" "$done_key" "$work/digests.automatic" "$work/digests.automatic.after"

# conversion_scope_step_lines KEY: the lines of this head's log that name KEY as a word of its own,
# in $work/step.lines.
conversion_scope_step_lines() {
  grep -E -- "(^|[^A-Za-z0-9_-])$1([^0-9]|\$)" "$tree_head_log" >"$work/step.lines" || true
}
for key in "$parent_key" "$child_one" "$child_two"; do
  conversion_scope_step_lines "$key"
  count="$(grep -E '(^|[^A-Za-z])converted([^A-Za-z]|$)' "$work/step.lines" | grep -cvE 'passed over|defer')" || count=0
  [[ "$count" == 1 ]] ||
    fail "$section" "this head's log holds one line converting $key" \
      "it holds $count: $(grep -E '(^|[^A-Za-z])converted([^A-Za-z]|$)' "$work/step.lines" | head -n 5)"
  pass "$section" "this head's log holds one line converting $key"
done
conversion_scope_step_lines "$done_key"
count="$(grep -F 'passed over' "$work/step.lines" | grep -c 'done')" || count=0
[[ "$count" == 1 ]] ||
  fail "$section" "this head's log holds one line passing $done_key over, with its reason" \
    "it holds $count: $(head -n 5 "$work/step.lines")"
pass "$section" "this head's log holds one line passing $done_key over, with its reason: $(grep -F 'passed over' "$work/step.lines" | grep 'done')"

printf 'PASS: %s complete 9 sections\n' "$driver"
