#!/usr/bin/env bash
# The driver of the check c-room-cutover of the issue tree's Verification document: an issue's
# three rooms become one room in one transaction, the rooms converted are rooms an earlier release
# wrote, and the fleet decides when the conversion may run.
#
# First pass. It empties the database DATABASE_URL names (a database on this machine whose name
# ends `_upgrade`, which it drops and creates again), boots the release BASELINE_SHA names on it
# through lib-baseline.sh, signs DEV_SIGNIN_LOGIN in (default alice), creates PROJECT, and writes
# through that release's API three issues in flight, each with a spec, a verification-md and a
# plan-md: A, whose three documents it also edits through that server's document sockets, so
# doc_updates, doc_snapshots and doc_checkpoints hold rows that writer made; R, whose spec socket it
# holds across the conversion; and B, whose conversion a trigger makes fail. It records each of A's
# rooms' head state (its newest doc_updates version), its row counts in the three tables and each
# document's markdown, stops that server, boots this head's (the stamped envoy-dispatch on PATH)
# with the hold settings lib-tree.sh gives every upgrade boot, and waits for the backfill as the
# Verification preamble states it.
#
# The conversion. Before it runs `envoy-dispatch convert-issues --project PROJECT` it creates a
# trigger on document_rooms that raises for B's spec, and it holds a socket on R's spec. It then
# reads A converted: A's room holds a fragment for each document, each document's text equals its
# capture character for character, the first part_of the conversion wrote for each document's
# nodes rises verification-md, plan-md, spec (R21), the tree's walk answers the spec's blocks
# first, and the room's first update, version 1, is the conversion's. Every doc_updates,
# doc_snapshots and doc_checkpoints row of A's three old rooms is still stored, and the three
# document_rooms rows name A's tree. The run names the trigger for B; once the trigger is dropped
# no document_rooms row and no doc_updates row of a tree of B stands. R's held socket is closed,
# a write through it reaches nothing, and its reconnection with the state vector it held is
# admitted to R's room and reads the converted tree, and no block of its old state lands there.
# Two sockets on A, through the spec and through the Verification document, each write a block, and
# each reads the other's: one room.
#
# The fleet gate. With the baseline build booted beside this head, the conversion refuses naming
# that build's commit and its instance; with that server killed and its row still live, it refuses
# the same; with the row removed and this head stopped, a server_build_announcements row naming
# another commit is refused the same with no server up; a row naming the conversion's own commit
# lets it run, and it prints B converted. This head boots again, and a second run changes no row of
# document_rooms, doc_updates or graph_tuples. Then a second build of this head, stamped with
# another commit, boots beside it: an issue created then holds a document_rooms row from its first
# write, and once that build stops a run passes over it. Two more issues the baseline build writes
# beside this head are what a --dry-run then prints, each with its time, beside a total; the dry
# run changes no row of the three tables, and the real run converts exactly the issues it named.
#
# Second pass, over an emptied database: the baseline build and this head live together, as a roll
# runs them. An issue this head creates then holds no document_rooms row and its spec socket is
# admitted to the spec's own room; once the baseline stops, the conversion converts it.
#
# Third pass, over an emptied database: the deferral. The driver takes the block index's lease and
# the reference forms drain's (lib.sh's hold_index_leases: pg_advisory_lock(1280266064,
# hashtext('block-index')) and the same of 'refs-forms', as duty.Exclusive takes each) on psql
# sessions of its own and keeps them, since the drain indexes a document through the block index's
# own index and would index what the block index left owed. Through the earlier release it writes
# X1, whose Verification document of more than 2,000 blocks that write leaves to the
# reconciliation (document_block_versions.version reads null), and X2, of a few blocks a document.
# It boots this head without the hold and with no wait for the backfill. While the leases are
# held, the dry run and the real run each print X1 and X2 deferred, each naming a document of its
# issue that owes its block index, the server's log holds the step's deferred line for each, and
# both read converted: false. It gives both back and runs no command: both read converted: true
# within 300 s, and the store census reads no difference.
#
# The sockets speak the Hocuspocus framing the browser's provider speaks, through the Bun program
# e2e/acceptance/issue-room-peer.ts, which lib-tree.sh's tree_peer_start runs.
#
# Inputs: DATABASE_URL, PROJECT, BASELINE_SHA (the commit the baseline server is stamped with),
# DISPATCH_TOKEN (the agent bearer every server here accepts), DEV_SIGNIN_LOGIN (optional, default
# alice: a document socket admits a person alone), envoy-dispatch on PATH, go (the second build of
# this head), and bun with this package's node_modules installed. Each passing check prints
# `PASS: <section> <what>`; the first failing one prints `FAIL: <section> <what>: <observed>` and
# the script exits 1; a missing input exits 2. It prints no SKIP line, and its last line is
# `PASS: issue-room-cutover.sh complete <N> sections`.
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
         bash e2e/acceptance/issue-room-cutover.sh
       (run from packages/dispatch, with the stamped envoy-dispatch on PATH, go, and bun install
       run; DATABASE_URL names a database on this machine whose name ends _upgrade, which this run
       drops and creates again three times)
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

require_env DATABASE_URL PROJECT BASELINE_SHA
require_commands bun cmp curl git go grep head jq psql tail
readonly driver=issue-room-cutover.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
checkout="${LEGION_WORKSPACE:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
readonly checkout
tree_peer_require
base_commit="$(git -C "$checkout" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" || {
  printf 'BASELINE_SHA %s names no commit of %s\n' "$BASELINE_SHA" "$checkout" >&2
  exit 2
}
readonly base_commit
# The blocks of X1's Verification document: more than the 2,000 a write indexes in its own request
# (deferBlockIndexBlocks), so that write leaves it to the reconciliation.
readonly deferred_blocks=2100

work_dir
mkdir "$work/capture" "$work/bin"
stamp="$(date +%s)-$$"
readonly stamp
sections=0
peer_pid=""
peer_in=""
peer_out=""
beside_pid=""
other_pid=""
cleanup() {
  local code=$?
  tree_peer_stop
  drop_index_leases
  baseline_stop_pid "$beside_pid"
  baseline_stop_pid "$other_pid"
  tree_stop_head
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- helpers -------------------------------------------------------------------------------------

# issue_room_cutover_boot_beside NAME BINARY MODE: boots BINARY over the database beside whatever
# is live, through lib-baseline.sh's baseline_boot, with tree_hold_env's settings where MODE is
# `held` and none where it is `plain`, the caller's BASELINE_EXTRA_ENV pairs kept. Its pid is in
# beside_boot_pid and its address in beside_boot_url; the caller stops it.
issue_room_cutover_boot_beside() {
  local name="$1" binary="$2" mode="$3" given="${BASELINE_EXTRA_ENV-}"
  case "$mode" in
    held) BASELINE_EXTRA_ENV="${given:+$given$'\n'}$tree_hold_env" ;;
    plain) ;;
    *) tree_refuse "issue_room_cutover_boot_beside boots held or plain, not '$mode'" ;;
  esac
  baseline_read_extra_env
  baseline_boot "$name" "$binary" "$baseline_database_url"
  BASELINE_EXTRA_ENV="$given"
  baseline_read_extra_env
  beside_boot_pid="$baseline_boot_pid"
  beside_boot_url="$baseline_boot_url"
  printf 'beside server %s %s %s (pid %s)\n' "$name" "$mode" "$beside_boot_url" "$beside_boot_pid"
}

# issue_room_cutover_write_issue SECTION TITLE LABEL [BLOCKS]: an issue of the run through the
# server at $base, with a spec holding a requirement, a Verification document of a check serving
# it and a plan of a task serving it, each holding the shapes, and, with BLOCKS, that many filler
# blocks more in its Verification document; sets written, written_spec, written_verification and
# written_plan.
issue_room_cutover_write_issue() {
  local section="$1" title="$2" label="$3" filler="${4:-0}"
  {
    printf 'The spec of the %s issue.\n\n:::requirement{#r-%s}\nThe %s issue converts whole.\n:::\n\n' "$label" "$label" "$label"
    corpus_shapes spec "$label-spec"
  } >"$work/$label-spec.md"
  corpus_issue "$section" "$title" "$work/$label-spec.md"
  written="$corpus_key"
  written_spec="$corpus_spec"
  tree_key "$section" "the key of $title" "$written"
  tree_uuid "$section" "the spec of $written" "$written_spec"
  {
    printf ':::check{#c-%s serves="dispatch://%s/spec#b-r-%s" surface="cli" run="bash e2e/acceptance/issue-room-cutover.sh" negative_control="Convert one document alone and two rooms stand."}\nThe %s issue is one room.\n:::\n\n' \
      "$label" "$written" "$label" "$label"
    corpus_shapes verification.md "$label-verification"
    ((filler == 0)) || corpus_filler "$label" "$filler"
  } >"$work/$label-verification.md"
  corpus_upload "$section" "$written" verification.md "$work/$label-verification.md"
  written_verification="$corpus_doc"
  {
    printf ':::plan_task{#t-%s serves="dispatch://%s/spec#b-r-%s"}\nConvert the %s issue.\n:::\n\n' "$label" "$written" "$label" "$label"
    corpus_shapes plan.md "$label-plan"
  } >"$work/$label-plan.md"
  corpus_upload "$section" "$written" plan.md "$work/$label-plan.md"
  written_plan="$corpus_doc"
  tree_uuid "$section" "the verification-md of $written" "$written_verification"
  tree_uuid "$section" "the plan-md of $written" "$written_plan"
}

# issue_room_cutover_outcome FILE KEY: what a conversion's output in FILE printed for the issue
# KEY, in outcome: `converted` (a dry run's would-convert line too), `passed over`, `deferred` or
# `none`, read off the line that names the key as a word, which is kept in outcome_line.
issue_room_cutover_outcome() {
  local pattern="(^|[^A-Za-z0-9-])${2}([^0-9]|\$)" line
  outcome=none
  outcome_line=""
  while IFS= read -r line; do
    [[ "$line" =~ $pattern ]] || continue
    outcome_line="$line"
    case "$line" in
      *deferred*) outcome=deferred ;;
      *"passed over"*) outcome="passed over" ;;
      *convert*) outcome=converted ;;
    esac
  done <"$1"
}

# issue_room_cutover_refused SECTION WHAT NAME WORDS…: the conversion run kept as NAME refused,
# exiting non-zero, and its output names each of WORDS.
issue_room_cutover_refused() {
  local section="$1" what="$2" name="$3" word
  shift 3
  [[ "$tree_envoy_status" != 0 ]] ||
    fail "$section" "$what" "it exited 0, printing: $(tail -n 10 "$work/$name.out")"
  for word in "$@"; do
    grep -qF -- "$word" "$work/$name.out" ||
      fail "$section" "$what" "it exited $tree_envoy_status without naming $word: $(tail -n 10 "$work/$name.out")"
  done
}

# issue_room_cutover_run SECTION NAME [--dry-run]: `envoy-dispatch convert-issues --project
# PROJECT`, with --dry-run where given, whatever its exit status, its output printed and kept in
# $work/NAME.out and its status in tree_envoy_status.
issue_room_cutover_run() {
  local section="$1" name="$2"
  shift 2
  tree_envoy_dispatch_status convert-issues --project "$project" "$@"
  cp "$work/out" "$work/$name.out"
  printf 'convert-issues --project %s%s exited %s:\n%s\n' "$project" "${1:+ $1}" "$tree_envoy_status" "$(<"$work/$name.out")"
}

# issue_room_cutover_snapshot: one line that changes where any row of document_rooms, doc_updates
# or graph_tuples does, in snapshot.
issue_room_cutover_snapshot() {
  query snapshot "read document_rooms, doc_updates and graph_tuples" "
    select (select count(*) || ':' || md5(coalesce(string_agg(artifact_id::text || '>' || room::text, ',' order by artifact_id), ''))
              from document_rooms)
      || '|' || (select count(*) || ':' || md5(coalesce(string_agg(artifact_id::text || '@' || version, ',' order by artifact_id, version), ''))
              from doc_updates)
      || '|' || (select count(*) || ':' || coalesce(max(seq), 0) from graph_tuples)"
  snapshot="$answer"
}

# issue_room_cutover_census_reads_zero SECTION WHAT: `envoy-dispatch census --store` reads zero
# differences, and no difference line names an issue of KEYS (the rest of the arguments).
issue_room_cutover_census_reads_zero() {
  local section="$1" what="$2" summary key
  shift 2
  tree_store_census "$section"
  summary="$(grep -E '^census: store rows_checked=' "$work/out" || true)"
  printf '%s\n' "$summary"
  [[ "$summary" =~ differences=0([^0-9,]|$) ]] ||
    fail "$section" "$what" "${summary:-no summary line}; $(grep -E 'difference at' "$work/out" | head -n 20)"
  for key in "$@"; do
    ! grep -E "difference at .*${key}([^0-9]|\$)" "$work/out" >/dev/null ||
      fail "$section" "$what" "$(grep -E "difference at .*${key}([^0-9]|\$)" "$work/out" | head -n 10)"
  done
  pass "$section" "$what"
}

# issue_room_cutover_rows DOCUMENT: DOCUMENT's rows of the three stored tables, `<doc_updates>
# <doc_snapshots> <doc_checkpoints>`, in rows, and its newest doc_updates version in head_version.
issue_room_cutover_rows() {
  query rows "count the stored rows of $1" "
    select (select count(*) from doc_updates where artifact_id = '$1') || ' ' ||
           (select count(*) from doc_snapshots where artifact_id = '$1') || ' ' ||
           (select count(*) from doc_checkpoints where artifact_id = '$1') || '|' ||
           coalesce((select max(version)::text from doc_updates where artifact_id = '$1'), 'none')"
  rows="${answer%|*}"
  head_version="${answer#*|}"
}

# issue_room_cutover_text SECTION DOCUMENT FILE: DOCUMENT's markdown, as GET
# /api/v1/artifacts/{id}/text answers it, into FILE.
issue_room_cutover_text() {
  tree_send GET "/api/v1/artifacts/$2/text"
  tree_want 200 "$1" "read the text of $2"
  jq -j '.markdown' "$work/body" >"$3" || fail "$1" "read the text of $2" "no markdown in: $(body)"
}

# issue_room_cutover_peer_start SECTION: starts the socket peer against $base as the person, on
# the block schema version that server serves, as a coprocess whose stdin and stdout the driver
# holds; a peer already running is stopped first, since each one serves one server.
issue_room_cutover_peer_start() {
  local schema
  tree_peer_stop
  tree_send GET /api/v1/schema/blocks
  tree_want 200 "$1" "read the block schema $base serves"
  schema="$(jq -r '.version' "$work/body")"
  tree_peer_start "$schema"
}

# issue_room_cutover_append SECTION NAME FRAGMENT ID WORDS: NAME appends a paragraph ID of WORDS
# to FRAGMENT, and the room applies it.
issue_room_cutover_append() {
  tree_peer_append "$1" "$2" "$3" "$(jq -cn --arg id "$4" --arg w "$5" '{type: "paragraph", id: $id, text: $w}')"
}

# issue_room_cutover_owed ISSUE: a document of ISSUE that owes its block index, in owed, empty
# where none does.
issue_room_cutover_owed() {
  query owed "read a document of $1 that owes its block index" "
    select coalesce((select a.id::text || ' (' || a.slug || ')' from document_block_versions d join artifacts a on a.id = d.artifact_id
      where a.issue_key = '$1' and (d.version is null or d.store_owed_at is not null) order by a.slug limit 1), '')"
  owed="$answer"
}

# issue_room_cutover_names_document LINE ISSUE: whether LINE names a document of ISSUE, by its id,
# its address or its slug.
issue_room_cutover_names_document() {
  local line="$1" id ref slug
  while IFS='|' read -r id ref slug; do
    [[ "$line" != *"$id"* && "$line" != *"$ref"* && "$line" != *"$slug"* ]] || return 0
  done < <(env -u PGHOSTADDR psql "$database_url" -X -q -At -c "
    select id::text || '|' || ref_key || '|' || slug from artifacts where issue_key = '$2' and kind = 'doc'" 2>/dev/null)
  return 1
}

# --- setup: the earlier release over an emptied database --------------------------------------

section=setup
tree_reset_database "$database_url" '?*_upgrade' "$section"
baseline_start
[[ -z "$baseline_commit" || "$baseline_commit" == "$base_commit" ]] ||
  fail "$section" "the baseline is the build of BASELINE_SHA" "it was built from $baseline_commit, and BASELINE_SHA names $base_commit"
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-room-cutover-$stamp"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The cutover of an issue's three rooms"
sections=$((sections + 1))

# --- base: three issues the earlier release writes, A's documents edited through its sockets ----

section=base
issue_room_cutover_write_issue "$section" "The issue whose three rooms become one" whole
whole="$written" whole_spec="$written_spec" whole_verification="$written_verification" whole_plan="$written_plan"
readonly whole whole_spec whole_verification whole_plan
issue_room_cutover_write_issue "$section" "The issue whose socket is held across the conversion" held
held_issue="$written" held_spec="$written_spec" held_verification="$written_verification" held_plan="$written_plan"
readonly held_issue held_spec held_verification held_plan
issue_room_cutover_write_issue "$section" "The issue a trigger keeps from converting" refused
second="$written" second_spec="$written_spec"
readonly second second_spec
issue_room_cutover_peer_start "$section"
# Each socket a name of its own: a closed name reconnects keeping its document, which would carry
# one document's state into the next one's room.
edits=0
for document in "$whole_spec" "$whole_verification" "$whole_plan"; do
  edits=$((edits + 1))
  tree_peer_open "$section" "edit-$edits" "$document"
  issue_room_cutover_append "$section" "edit-$edits" prosemirror "p-old-${document:0:8}" \
    "A paragraph the earlier release's socket wrote into this document."
  tree_peer "$section" "close the socket on $document" close "edit-$edits"
done
tree_peer_stop
corpus_wait_settled "$section"
pass "$section" "the earlier release's sockets edited each of $whole's three documents"
for document in "$whole_spec" "$whole_verification" "$whole_plan"; do
  issue_room_cutover_rows "$document"
  printf '%s\n' "$rows" >"$work/capture/$document.rows"
  issue_room_cutover_text "$section" "$document" "$work/capture/$document.md"
  printf 'room %s: head version %s, doc_updates doc_snapshots doc_checkpoints %s\n' "$document" "$head_version" "$rows"
  [[ "$rows" != "0 "* ]] || fail "$section" "the earlier release's room of $document holds doc_updates rows" "it holds $rows"
done
pass "$section" "recorded the head state, the stored rows and the markdown of $whole's three rooms"
baseline_stop_server
sections=$((sections + 1))

# --- upgrade: this head over those rows ---------------------------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
sections=$((sections + 1))

# --- convert: three rooms become one -------------------------------------------------------------

section=convert
psql_run "$section" "create a trigger on document_rooms that raises for $second's spec" <<SQL
create function issue_room_cutover_refuse() returns trigger language plpgsql as \$\$
begin
  if new.artifact_id = '$second_spec' then
    raise exception 'issue_room_cutover_refuse refuses % a room', new.artifact_id;
  end if;
  return new;
end
\$\$;
create trigger issue_room_cutover_refuse before insert on document_rooms
  for each row execute function issue_room_cutover_refuse();
SQL
pass "$section" "the trigger issue_room_cutover_refuse raises for $second's spec $second_spec"
issue_room_cutover_peer_start "$section"
tree_peer_open "$section" held "$held_spec"
[[ "$fragments" == prosemirror ]] ||
  fail "$section" "the socket on $held_issue's spec, before the conversion, is admitted to the spec's own room" "its fragments: $fragments"
tree_peer "$section" "read what the held socket draws" blocks held prosemirror
printf '%s\n' "${reply#blocks held }" >"$work/held-before.json"
pass "$section" "a socket on $held_issue's spec is held across the conversion"
query "$section" "read the database's clock before the conversion" "select clock_timestamp()"
converted_after="$answer"
query "$section" "read the store's last seq before the conversion" "select coalesce(max(seq), 0) from graph_tuples"
seq_before="$answer"
readonly converted_after seq_before
tree_wait_announcement "$section"
issue_room_cutover_run "$section" first
issue_room_cutover_outcome "$work/first.out" "$whole"
[[ "$outcome" == converted ]] ||
  fail "$section" "convert-issues prints $whole converted" "it prints $outcome for it: ${outcome_line:-no line}"
tree_converted "$section" "$whole"
[[ "$converted" == true ]] || fail "$section" "$whole reads converted: true" "it reads converted: $converted"
room_tree="$tree_value"
tree_uuid "$section" "$whole's tree" "$room_tree"
query "$section" "read $whole's tree's address" "select ref_key from artifacts where id = '$room_tree'"
room_ref="$answer"
readonly room_tree room_ref
want_value "$section" "the three document_rooms rows of $whole name its tree $room_tree" 3 "
    select count(*) from document_rooms where artifact_id in ('$whole_spec', '$whole_verification', '$whole_plan') and room = '$room_tree'"
tree_peer_open "$section" probe-whole "$room_tree"
for document in "$whole_spec" "$whole_verification" "$whole_plan"; do
  [[ ",$fragments," == *",$document,"* ]] ||
    fail "$section" "$whole's room holds each document's content in its own fragment" "its fragments: $fragments, not $document"
done
tree_peer "$section" "close the probe of $whole's room" close probe-whole
for document in "$whole_spec" "$whole_verification" "$whole_plan"; do
  issue_room_cutover_text "$section" "$document" "$work/$document.after.md"
  cmp -s "$work/capture/$document.md" "$work/$document.after.md" ||
    fail "$section" "the text of $document equals the markdown captured before the upgrade" \
      "$(diff "$work/capture/$document.md" "$work/$document.after.md" | head -n 20)"
done
pass "$section" "$whole's room holds a fragment of each document, and each text equals its capture character for character"
tree_first_part_of "$section" "$room_tree" "$room_ref" "$seq_before" "$whole_verification" verification-md
verification_seq="$first_seq"
tree_first_part_of "$section" "$room_tree" "$room_ref" "$seq_before" "$whole_plan" plan-md
plan_seq="$first_seq"
tree_first_part_of "$section" "$room_tree" "$room_ref" "$seq_before" "$whole_spec" spec
spec_seq="$first_seq"
read_seqs="verification-md=$verification_seq plan-md=$plan_seq spec=$spec_seq"
[[ "$verification_seq" =~ ^[0-9]+$ && "$plan_seq" =~ ^[0-9]+$ && "$spec_seq" =~ ^[0-9]+$ ]] ||
  fail "$section" "the conversion wrote a part_of for the nodes of each of $whole's documents" "the seqs read, in that order: $read_seqs"
((verification_seq < plan_seq && plan_seq < spec_seq)) ||
  fail "$section" "the conversion writes $whole's documents in the order verification-md, plan-md, spec" "the seqs read, in that order: $read_seqs"
pass "$section" "the conversion wrote $whole's documents in the order verification-md, plan-md, spec: $read_seqs"
first_document=""
cursor=""
while [[ -z "$first_document" ]]; do
  tree_read agent "$whole" "" "" "$cursor"
  tree_want 200 "$section" "read $whole's tree"
  first_document="$(jq -r '[.nodes[] | select(.document != null) | .document.artifact_id][0] // ""' "$work/body")"
  cursor="$(jq -r '.cursor // ""' "$work/body")"
  [[ -n "$first_document" || -n "$cursor" ]] || break
done
[[ "$first_document" == "$whole_spec" ]] ||
  fail "$section" "the tree's walk shows the spec's blocks first" "the first block of a document it walks is of ${first_document:-no document}"
pass "$section" "$whole's tree walk shows the spec's blocks first"
want_value "$section" "the room's version 1 is its first update, written by the conversion" "1|true" "
    select coalesce(min(version)::text, 'none') || '|' ||
           coalesce((select (created_at >= '$converted_after'::timestamptz)::text from doc_updates where artifact_id = '$room_tree' and version = 1), 'none')
    from doc_updates where artifact_id = '$room_tree'"
for document in "$whole_spec" "$whole_verification" "$whole_plan"; do
  issue_room_cutover_rows "$document"
  [[ "$rows" == "$(<"$work/capture/$document.rows")" ]] ||
    fail "$section" "every doc_updates, doc_snapshots and doc_checkpoints row of the old room of $document is still stored" \
      "it held $(<"$work/capture/$document.rows") before the upgrade and holds $rows"
done
pass "$section" "every doc_updates, doc_snapshots and doc_checkpoints row of $whole's three old rooms is still stored"
grep -qF issue_room_cutover_refuse "$work/first.out" ||
  fail "$section" "the conversion's error names the trigger that refused $second" "its output: $(tail -n 20 "$work/first.out")"
pass "$section" "the conversion's error for $second names the trigger issue_room_cutover_refuse"
psql_run "$section" "drop the trigger" <<'SQL'
drop trigger issue_room_cutover_refuse on document_rooms;
drop function issue_room_cutover_refuse();
SQL
want_value "$section" "$second holds no document_rooms row, no tree and no doc_updates row of a tree" "0|0|0" "
    select (select count(*) from document_rooms d join artifacts a on a.id = d.artifact_id where a.issue_key = '$second') || '|' ||
           (select count(*) from issue_trees where issue_key = '$second') || '|' ||
           (select count(*) from doc_updates u join artifacts a on a.id = u.artifact_id where a.issue_key = '$second' and a.kind = 'tree')"
tree_converted "$section" "$second"
[[ "$converted" == false ]] || fail "$section" "$second, whose conversion failed, reads converted: false" "it reads converted: $converted"
tree_peer "$section" "wait for the held socket to close" wait-close held 60
[[ "$reply" == "closed held "* ]] ||
  fail "$section" "the conversion closes the socket held on $held_issue's spec" "the peer answered: $reply"
pass "$section" "the conversion closed the socket held on $held_issue's spec (${reply#closed held })"
tree_peer "$section" "write through the closed socket" append held prosemirror \
  "$(jq -cn '{type: "paragraph", id: "p-after-close", text: "Written through the held socket after its close."}')"
[[ "$reply" == "unsent held" ]] || fail "$section" "no write of the held socket reaches a document after the close" "the peer answered: $reply"
issue_room_cutover_outcome "$work/first.out" "$held_issue"
[[ "$outcome" == converted ]] ||
  fail "$section" "convert-issues prints $held_issue converted" "it prints $outcome for it: ${outcome_line:-no line}"
tree_converted "$section" "$held_issue"
held_tree="$tree_value"
tree_uuid "$section" "$held_issue's tree" "$held_tree"
corpus_wait_settled "$section"
for document in "$held_spec" "$held_verification" "$held_plan" "$held_tree"; do
  issue_room_cutover_text "$section" "$document" "$work/$document.converted.md"
done
tree_peer_open "$section" held "$held_spec"
for document in "$held_spec" "$held_verification" "$held_plan"; do
  [[ ",$fragments," == *",$document,"* ]] ||
    fail "$section" "the held socket's reconnection is admitted to $held_issue's room" "its fragments: $fragments, not $document"
done
tree_peer "$section" "read the converted spec the reconnection draws" blocks held "$held_spec"
[[ "$(jq 'length' <<<"${reply#blocks held }")" -gt 0 ]] ||
  fail "$section" "the reconnection reads the converted tree" "its spec fragment holds no block"
pass "$section" "the held socket reconnected with the state vector it held, was admitted to $held_issue's room and reads the converted tree"
tree_peer_open "$section" probe-held "$held_tree"
tree_peer "$section" "read the tree's own fragment of $held_issue's room" blocks probe-held prosemirror
landed="$(jq -r --slurpfile before "$work/held-before.json" \
  '[.[] | .id] as $now | [$before[0][] | .id | select(. as $id | $now | index($id))] | join(", ")' <<<"${reply#blocks probe-held }")"
[[ -z "$landed" ]] ||
  fail "$section" "no write of the held socket reaches a document after the close" "blocks of its old room landed in $held_issue's tree fragment: $landed"
tree_peer "$section" "close the probe" close probe-held
tree_peer "$section" "close the held socket" close held
corpus_wait_settled "$section"
for document in "$held_spec" "$held_verification" "$held_plan" "$held_tree"; do
  issue_room_cutover_text "$section" "$document" "$work/$document.reconnected.md"
  cmp -s "$work/$document.converted.md" "$work/$document.reconnected.md" ||
    fail "$section" "no write of the held socket reaches a document after the close" \
      "the text of $document changed across the reconnection: $(diff "$work/$document.converted.md" "$work/$document.reconnected.md" | head -n 20)"
done
pass "$section" "no write of the held socket reached a document of $held_issue after the close"
sections=$((sections + 1))

# --- rooms: the spec and the Verification document of A are one room -----------------------------

section=rooms
tree_peer_open "$section" spec "$whole_spec"
tree_peer_open "$section" verification "$whole_verification"
readonly into_spec="p-spec-${stamp//-/}" into_verification="p-verification-${stamp//-/}"
issue_room_cutover_append "$section" spec "$whole_spec" "$into_spec" "A block written through the spec's socket."
issue_room_cutover_append "$section" verification "$whole_verification" "$into_verification" \
  "A block written through the Verification document's socket."
deadline=$((SECONDS + 60))
until tree_peer_holds "$section" spec "$whole_verification" "$into_verification" &&
  tree_peer_holds "$section" verification "$whole_spec" "$into_spec"; do
  ((SECONDS < deadline)) ||
    fail "$section" "the writes through $whole's spec and Verification document land in one room" \
      "after 60 s each socket still lacks the other's block: the two writes land in two rooms of one issue"
  sleep 1
done
pass "$section" "the blocks written through $whole's spec and its Verification document land in one room, each socket reading both"
tree_peer "$section" "close the spec's socket" close spec
tree_peer "$section" "close the Verification document's socket" close verification
tree_peer_stop
corpus_wait_settled "$section"
sections=$((sections + 1))

# --- gate: the conversion runs only while the fleet serves its own build ------------------------

section=gate
issue_room_cutover_boot_beside base-beside "$baseline_binary" plain
beside_pid="$beside_boot_pid"
deadline=$((SECONDS + 60))
while true; do
  query "$section" "read the baseline build's process" \
    "select coalesce((select instance::text from server_processes where commit = '$base_commit' order by started_at desc limit 1), '')"
  [[ -z "$answer" ]] || break
  ((SECONDS < deadline)) || fail "$section" "the baseline build registers its process" "after 60 s server_processes lists no process of $base_commit"
  sleep 1
done
base_instance="$answer"
readonly base_instance
issue_room_cutover_run "$section" beside
issue_room_cutover_refused "$section" "the conversion refuses while the baseline build is live, naming its build and its instance" \
  beside "$base_commit" "$base_instance"
pass "$section" "the conversion refuses while $base_commit is live, naming that build and its instance $base_instance"
kill -KILL "$beside_pid" 2>/dev/null || true
wait "$beside_pid" 2>/dev/null || true
beside_pid=""
want_value "$section" "the stopped baseline's process row still stands" 1 \
  "select count(*) from server_processes where instance = '$base_instance'"
issue_room_cutover_run "$section" stopped
issue_room_cutover_refused "$section" "the conversion refuses while the stopped baseline's row stands, naming its build and its instance" \
  stopped "$base_commit" "$base_instance"
pass "$section" "the conversion refuses while the stopped baseline's row stands, naming $base_commit and $base_instance"
psql_run "$section" "remove the stopped baseline's row" -c "delete from server_processes where instance = '$base_instance'"
tree_stop_head
want_value "$section" "no server process is live" 0 "select count(*) from server_processes"
psql_run "$section" "insert an announcement naming another commit" -c "insert into server_build_announcements (commit) values ('$base_commit')"
issue_room_cutover_run "$section" announced
issue_room_cutover_refused "$section" "with no server up the conversion refuses an announcement naming another commit, naming that build" \
  announced "$base_commit"
pass "$section" "with no server up, the conversion refuses the announcement naming $base_commit"
head_commit="$tree_head_commit"
readonly head_commit
psql_run "$section" "insert an announcement naming the conversion's own commit" -c "insert into server_build_announcements (commit) values ('$head_commit')"
issue_room_cutover_run "$section" own
[[ "$tree_envoy_status" == 0 ]] ||
  fail "$section" "the conversion runs once the newest announcement names its own commit" "it exited $tree_envoy_status: $(tail -n 10 "$work/own.out")"
issue_room_cutover_outcome "$work/own.out" "$second"
[[ "$outcome" == converted ]] ||
  fail "$section" "the conversion prints each issue it converted, $second among them" "it prints $outcome for $second: ${outcome_line:-no line}"
pass "$section" "the newest announcement naming $head_commit lets the conversion run, and it printed $second converted"
tree_boot_head head-again
base="$tree_head_url"
tree_wait_announcement "$section"
issue_room_cutover_snapshot
before_second="$snapshot"
issue_room_cutover_run "$section" again
[[ "$tree_envoy_status" == 0 ]] || fail "$section" "a second run over converted issues runs" "it exited $tree_envoy_status: $(tail -n 10 "$work/again.out")"
for issue in "$whole" "$held_issue" "$second"; do
  issue_room_cutover_outcome "$work/again.out" "$issue"
  [[ "$outcome" != converted ]] || fail "$section" "a second run converts $issue no second time" "it prints: $outcome_line"
done
issue_room_cutover_snapshot
[[ "$snapshot" == "$before_second" ]] ||
  fail "$section" "a second run over converted issues changes nothing" "document_rooms, doc_updates and graph_tuples read $before_second before it and $snapshot after"
pass "$section" "a second run over converted issues changes no row of document_rooms, doc_updates or graph_tuples"
sections=$((sections + 1))

# --- birth: an issue born converted while two announcing builds are live ------------------------

section=birth
other_commit="$(git -C "$checkout" rev-parse --verify --quiet HEAD^)" ||
  fail "$section" "read another commit to stamp the second build with" "git rev-parse HEAD^ failed"
[[ "$other_commit" != "$head_commit" ]] || fail "$section" "the second build carries another commit" "HEAD^ is $other_commit, this head's commit"
readonly other_commit
go -C "$checkout/packages/envoy" build -ldflags "-X main.buildCommit=$other_commit" -o "$work/bin/envoy-dispatch-other" ./cmd/dispatch \
  >"$work/build-other" 2>&1 || fail "$section" "build this head again stamped with $other_commit" "$(tail -n 20 "$work/build-other")"
issue_room_cutover_boot_beside other "$work/bin/envoy-dispatch-other" held
other_pid="$beside_boot_pid"
deadline=$((SECONDS + 60))
until query "$section" "read the live builds that announce the issue room" \
  "select count(distinct commit) || '|' || count(*) filter (where 'issue_room' = any(capabilities)) || '|' || count(*) from server_processes" &&
  [[ "$answer" == "2|2|2" ]]; do
  ((SECONDS < deadline)) || fail "$section" "two builds of two commits are live, each announcing the issue room" "server_processes reads $answer (commits|announcing|processes)"
  sleep 1
done
pass "$section" "two builds, $head_commit and $other_commit, are live, each announcing the issue room"
printf 'The spec of an issue born beside two announcing builds.\n\n:::requirement{#r-born}\nThe issue is born converted.\n:::\n' >"$work/born-spec.md"
corpus_issue "$section" "An issue born while two announcing builds are live" "$work/born-spec.md"
born="$corpus_key" born_spec="$corpus_spec"
readonly born born_spec
tree_key "$section" "the born issue's key" "$born"
tree_uuid "$section" "the born issue's spec" "$born_spec"
want_value "$section" "$born's spec holds a document_rooms row to $born's room from its first write" 1 "
    select count(*) from document_rooms d join issue_trees t on t.artifact_id = d.room
    where d.artifact_id = '$born_spec' and t.issue_key = '$born'"
baseline_stop_pid "$other_pid"
other_pid=""
tree_wait_announcement "$section"
issue_room_cutover_run "$section" born
[[ "$tree_envoy_status" == 0 ]] || fail "$section" "the conversion runs once the second build stopped" "it exited $tree_envoy_status: $(tail -n 10 "$work/born.out")"
issue_room_cutover_outcome "$work/born.out" "$born"
[[ "$outcome" != converted ]] || fail "$section" "a later run passes over $born without converting it again" "it prints: $outcome_line"
pass "$section" "a later run passes over $born, born converted, without converting it again (${outcome_line:-it names it on no line})"
sections=$((sections + 1))

# --- dryrun: a dry run names what the real run converts, and changes nothing --------------------

section=dryrun
issue_room_cutover_boot_beside base-dry "$baseline_binary" plain
beside_pid="$beside_boot_pid"
base="$beside_boot_url"
issue_room_cutover_write_issue "$section" "The first issue a dry run names" dry-one
dry_one="$written"
issue_room_cutover_write_issue "$section" "The second issue a dry run names" dry-two
dry_two="$written"
readonly dry_one dry_two
corpus_wait_settled "$section"
baseline_stop_pid "$beside_pid"
beside_pid=""
base="$tree_head_url"
tree_wait_backfill "$section"
tree_wait_announcement "$section"
issue_room_cutover_snapshot
before_dry="$snapshot"
issue_room_cutover_run "$section" dry --dry-run
[[ "$tree_envoy_status" == 0 ]] || fail "$section" "the dry run runs" "it exited $tree_envoy_status: $(tail -n 10 "$work/dry.out")"
issue_room_cutover_snapshot
[[ "$snapshot" == "$before_dry" ]] ||
  fail "$section" "a dry run changes no row of document_rooms, doc_updates or graph_tuples" "they read $before_dry before it and $snapshot after"
pass "$section" "the dry run changed no row of document_rooms, doc_updates or graph_tuples"
grep -qiE 'total' "$work/dry.out" || fail "$section" "the dry run prints a total" "its output: $(tail -n 10 "$work/dry.out")"
query "$section" "read the issues of $project" "select string_agg(key, ' ' order by key) from issues where project_key = '$project'"
read -r -a project_issues <<<"$answer"
named=()
for issue in "${project_issues[@]}"; do
  issue_room_cutover_outcome "$work/dry.out" "$issue"
  [[ "$outcome_line" =~ [0-9](\.[0-9]+)?[[:space:]]*(ns|µs|us|ms|s)([^A-Za-z]|$) ]] ||
    fail "$section" "the dry run prints each issue with its time" "its line for $issue: ${outcome_line:-none}"
  [[ "$outcome" != converted ]] || named+=("$issue")
done
[[ " ${named[*]} " == *" $dry_one "* && " ${named[*]} " == *" $dry_two "* ]] ||
  fail "$section" "the dry run names $dry_one and $dry_two, which the earlier release wrote beside this head" "it names ${named[*]:-none} to convert"
pass "$section" "the dry run prints each of the ${#project_issues[@]} issues with its time, and a total, and names ${named[*]} to convert"
issue_room_cutover_run "$section" real
[[ "$tree_envoy_status" == 0 ]] || fail "$section" "the real run runs" "it exited $tree_envoy_status: $(tail -n 10 "$work/real.out")"
converted_now=()
for issue in "${project_issues[@]}"; do
  issue_room_cutover_outcome "$work/real.out" "$issue"
  [[ "$outcome" != converted ]] || converted_now+=("$issue")
done
[[ "${converted_now[*]}" == "${named[*]}" ]] ||
  fail "$section" "the real run converts exactly the issues the dry run named" "the dry run named ${named[*]} and the real run converted ${converted_now[*]:-none}"
pass "$section" "the real run converted exactly ${named[*]}, the issues the dry run named"
sections=$((sections + 1))

# --- roll: the baseline build live beside this head, over an emptied database --------------------

section=roll
tree_stop_head
tree_reset_database "$database_url" '?*_upgrade' "$section"
issue_room_cutover_boot_beside roll-base "$baseline_binary" plain
beside_pid="$beside_boot_pid"
base="$beside_boot_url"
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The roll of the issue room"
tree_boot_head roll-head
base="$tree_head_url"
query "$section" "read the baseline build's process" \
  "select coalesce((select instance::text || ' (' || commit || ')' from server_processes where commit = '$base_commit' limit 1), '(none)')"
roll_process="$answer"
printf 'The spec of an issue this head creates beside the baseline build.\n\n:::requirement{#r-roll}\nThe issue waits for the roll.\n:::\n' >"$work/roll-spec.md"
corpus_issue "$section" "An issue this head creates during a roll" "$work/roll-spec.md"
rolled="$corpus_key" rolled_spec="$corpus_spec"
readonly rolled rolled_spec
tree_key "$section" "the rolled issue's key" "$rolled"
tree_uuid "$section" "the rolled issue's spec" "$rolled_spec"
query "$section" "read the document_rooms row of $rolled's spec" "select count(*) from document_rooms where artifact_id = '$rolled_spec'"
[[ "$answer" == 0 ]] ||
  fail "$section" "an issue created while a process announcing no issue room is live is not born converted" \
    "$rolled holds a document_rooms row although the process $roll_process is live"
pass "$section" "$rolled, created beside the baseline process $roll_process, holds no document_rooms row"
issue_room_cutover_peer_start "$section"
tree_peer_open "$section" rolled "$rolled_spec"
[[ "$fragments" == prosemirror ]] ||
  fail "$section" "the spec socket of $rolled is admitted to the spec's own room, as today" "its fragments: $fragments"
tree_peer "$section" "close the spec socket of $rolled" close rolled
tree_peer_stop
pass "$section" "the spec socket of $rolled is admitted to the spec's own room"
baseline_stop_pid "$beside_pid"
beside_pid=""
tree_wait_backfill "$section"
tree_wait_announcement "$section"
issue_room_cutover_run "$section" roll
[[ "$tree_envoy_status" == 0 ]] || fail "$section" "the conversion runs once the baseline stopped" "it exited $tree_envoy_status: $(tail -n 10 "$work/roll.out")"
tree_converted "$section" "$rolled"
[[ "$converted" == true ]] || fail "$section" "the conversion converts $rolled once the baseline stopped" "it reads converted: $converted"
pass "$section" "once the baseline stopped, the conversion converted $rolled"
sections=$((sections + 1))

# --- deferral: an issue whose document owes its block index waits for it -------------------------

section=deferral
tree_stop_head
tree_reset_database "$database_url" '?*_upgrade' "$section"
issue_room_cutover_boot_beside defer-base "$baseline_binary" plain
beside_pid="$beside_boot_pid"
base="$beside_boot_url"
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The deferral of an issue's conversion"
hold_index_leases "$section"
issue_room_cutover_write_issue "$section" "An issue whose Verification document is left to the reconciliation" deferred-large "$deferred_blocks"
large="$written" large_verification="$written_verification"
issue_room_cutover_write_issue "$section" "An issue of a few blocks a document" deferred-small
small="$written"
readonly large large_verification small
want_value "$section" "the earlier release leaves $large's Verification document of more than 2,000 blocks to the reconciliation (its document_block_versions.version reads null)" true \
  "select coalesce((select (version is null)::text from document_block_versions where artifact_id = '$large_verification'), 'no row')"
baseline_stop_pid "$beside_pid"
beside_pid=""
tree_boot_head deferral unheld
base="$tree_head_url"
tree_wait_announcement "$section"
for run in dry real; do
  if [[ "$run" == dry ]]; then
    issue_room_cutover_run "$section" deferral-dry --dry-run
  else
    issue_room_cutover_run "$section" deferral-real
  fi
  for issue in "$large" "$small"; do
    issue_room_cutover_outcome "$work/deferral-$run.out" "$issue"
    [[ "$outcome" == deferred ]] ||
      fail "$section" "the $run run prints $issue deferred while the leases are held" "it prints $outcome: ${outcome_line:-no line}"
    issue_room_cutover_names_document "$outcome_line" "$issue" ||
      fail "$section" "the $run run's deferral of $issue names a document of $issue that owes its block index" "its line: $outcome_line"
  done
done
pass "$section" "the dry run and the real run each print $large and $small deferred, each naming a document that owes its block index"
deadline=$((SECONDS + tree_wait_seconds))
for issue in "$large" "$small"; do
  until grep -E "deferred" "$tree_head_log" | grep -qE "(^|[^A-Za-z0-9-])${issue}([^0-9]|\$)"; do
    ((SECONDS < deadline)) ||
      fail "$section" "the server's log holds the step's deferred line for $issue" "after ${tree_wait_seconds} s it holds none: $(grep -E 'deferred' "$tree_head_log" | tail -n 5)"
    sleep "$tree_poll_seconds"
  done
done
pass "$section" "the server's log holds the step's deferred line for $large and for $small"
for issue in "$large" "$small"; do
  tree_converted "$section" "$issue"
  if [[ "$converted" != false ]]; then
    issue_room_cutover_owed "$issue"
    fail "$section" "$issue reads converted: false while the leases are held" "it reads converted: $converted while ${owed:-no document} of it owes its block index"
  fi
done
pass "$section" "$large and $small read converted: false while the leases are held"
give_index_lease "$section"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  stayed=""
  for issue in "$large" "$small"; do
    tree_converted "$section" "$issue"
    [[ "$converted" == true ]] || stayed+=" $issue"
  done
  [[ -n "$stayed" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the step converts each deferred issue on a later tick, with no command run" \
      "after ${tree_wait_seconds} s these still read converted: false:$stayed"
  sleep "$tree_poll_seconds"
done
pass "$section" "with the leases given back and no command run, the step converted $large and $small"
issue_room_cutover_census_reads_zero "$section" "the conversion's census of $large and $small reads zero" "$large" "$small"
sections=$((sections + 1))

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
