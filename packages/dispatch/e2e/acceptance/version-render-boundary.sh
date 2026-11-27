#!/usr/bin/env bash
# The check c-old-versions-render: every version written before the cutover still answers the
# bytes it holds, and every standing approval still covers them (spec requirements
# r-render-at-version and r-approval-bytes).
#
# It empties the database DATABASE_URL names (infra_1253_upgrade on this machine, dropped and
# created again), boots the release before this branch on it through lib-baseline.sh (BASELINE_SHA
# or BASELINE_BINARY), and writes through that server's API an issue whose spec, verification.md,
# plan.md and notes.md hold the shapes the tree census reads (seed-corpus.sh's corpus_shapes), with
# several versions each, and one image upload, whose version holds no markdown. The person
# DEV_SIGNIN_LOGIN writes one more document, private.md, holding a callout, #author-only, and
# approves the newest version of the spec, the verification.md and the notes.md, and an older
# version of the plan.md, which its next version supersedes: so the corpus holds standing
# approvals. A second person, bob, signs in through the same server, so that this head's half reads
# as a reader the block is hidden from and writes nothing through the API. It stops that server,
# boots this head over the same database with the hold of R6's step, waits for the backfill, and
# from then on writes nothing through the API.
#
# It prints the two counts of R19 over that corpus, each with the database's time: the
# artifact_versions rows with a markdown body, and the standing approvals (the newest review of a
# document an approval of its newest version); a count of zero fails. It renders every version row
# with a markdown body through the one renderer (GET /api/v1/artifacts/{id}/versions/{number}, as
# the person, who may see every block of the version) and compares the answer's markdown with
# artifact_versions.markdown byte for byte, and the answer's Dispatch-Render-Source with `stored`:
# one mismatch fails, printing the artifact, the version number and the offset of the first byte
# that differs. It prints how many it rendered, and reads no version_renders row for any of them,
# so the set is the pre-cutover side. For each standing approval over a rendered version it
# recomputes the digest artifact_version_text_sha256 takes of the stored markdown (the sha256 of
# its UTF-8 bytes) and compares it with artifact_reviews.content_sha256; one difference fails. It
# reads the update trigger on that column, artifact_reviews_content_sha256_update, out of the
# catalog. It reads the version whose markdown is null, the image's, and finds it served as plain
# file bytes (the image's own, as image/png) with no Dispatch-Render-Source header, rather than
# rendered as an empty document (Verification v231). Last it
# makes #author-only a block written author-only: the earlier release writes every block at the
# visibility `project` and refuses a `visibility` attribute (seed-corpus.sh's head comment), so the
# driver sets that block's graph_nodes row to visibility author in the store, its one write after
# the upgrade. It reads the rendered versions that hold a block written author-only (a block node
# of visibility author whose fence the version's markdown holds): at least one must stand, and each
# renders for bob with that block absent and the rest of the version byte for byte as stored.
#
# Inputs: DATABASE_URL (infra_1253_upgrade on this machine), DISPATCH_TOKEN (e2e-token), PROJECT
# (CORE), DEV_SIGNIN_LOGIN (alice), BASELINE_SHA or BASELINE_BINARY, and `envoy-dispatch` on PATH,
# built stamped with this head's commit.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. The last line is `PASS: version-render-boundary.sh complete <N> sections`.
# Every check is `cmd || fail …`, never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1253_upgrade DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice BASELINE_SHA=<the commit of the release this branch lands after> \
         bash e2e/acceptance/version-render-boundary.sh
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
require_commands base64 cmp curl date grep jq psql tr
readonly driver=version-render-boundary.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"
# The reader the author-only block is hidden from.
readonly hidden_from=bob
session="version-render-boundary-$(date +%s)-$$"
readonly session
# The fence that opens the block the hidden section makes author-only, and its words.
readonly author_only_open=':::callout{#author-only kind="note" title=""}'
readonly author_only_words="A paragraph the author alone reads."

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

# version_render_boundary_read JAR PATH: GET PATH at `base` with the session cookie JAR holds, its
# body in $work/body, its headers in $work/headers, its status in `status`, and its
# Dispatch-Render-Source in `render_source` ("" where it names none). A read writes nothing.
render_source=""
version_render_boundary_read() {
  tree_last_request="GET $2"
  status="$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' -b "$1" "$base$2" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  render_source="$(grep -i '^Dispatch-Render-Source:' "$work/headers" | tail -n 1 | cut -d: -f2- | tr -d ' \r')" || render_source=""
}

# version_render_boundary_stored ID NUMBER FILE: the stored markdown of version NUMBER of ID, byte
# for byte, in FILE.
version_render_boundary_stored() {
  query render "read the stored markdown of $1 v$2" \
    "select encode(convert_to(markdown, 'UTF8'), 'base64') from artifact_versions where artifact_id = '$1' and number = $2"
  base64 -d <<<"$answer" >"$3" || fail render "read the stored markdown of $1 v$2" "base64 could not decode it"
}

# version_render_boundary_first_difference A B: the offset, from 0, of the first byte at which the
# files A and B differ, in tree_value: where one is a prefix of the other, the length of the shorter.
version_render_boundary_first_difference() {
  local said
  said="$(cmp "$1" "$2" 2>&1)" || true
  if [[ "$said" =~ after\ byte\ ([0-9]+) ]]; then
    tree_value="${BASH_REMATCH[1]}"
  elif [[ "$said" =~ EOF\ on\ .*\ which\ is\ empty ]]; then
    tree_value=0
  elif [[ "$said" =~ byte\ ([0-9]+) ]]; then
    tree_value=$((BASH_REMATCH[1] - 1))
  else
    tree_value="unknown ($said)"
  fi
}

# --- setup and the corpus, through the earlier release ----------------------------------------------

tree_reset_database "$database_url" infra_1253_upgrade setup
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
base="$baseline_url"
tree_init
tree_sign_in setup "$person_login"
tree_create_project setup "$project" "The versions written before the cutover"
# The reader the author-only block is hidden from, signed in now so this head's half writes
# nothing: every server this run boots signs cookies with the one key (lib-baseline.sh).
tree_last_request="GET /auth/_dev/signin?login=$hidden_from"
status="$(curl -sS -o "$work/body" -w '%{http_code}' -c "$work/jar.$hidden_from" \
  "$base/auth/_dev/signin?login=$(q "$hidden_from")" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
tree_want 302 setup "dev sign-in as $hidden_from"
grep -q $'\tdsession\t' "$work/jar.$hidden_from" || fail setup "dev sign-in as $hidden_from" "the answer set no session cookie"
pass setup "signed in as $hidden_from, the reader the author-only block is hidden from"

section=corpus
{
  printf 'The issue whose versions were written before the cutover.\n\n:::requirement{#r-bytes}\nEvery version answers the bytes it holds.\n:::\n\n'
  corpus_shapes spec boundary-spec
} >"$work/spec.md"
corpus_issue "$section" "The versions written before the cutover" "$work/spec.md"
# Not readonly: seed-corpus.sh's corpus_upload takes a local of that name.
issue="$corpus_key"
readonly spec="$corpus_spec"
corpus_revise "$section" "$spec" spec 2
corpus_revise "$section" "$spec" spec 3
{
  printf 'How the boundary is proved.\n\n:::check{#c-bytes serves="dispatch://%s/spec#b-r-bytes" surface="cli" run="bash e2e/acceptance/version-render-boundary.sh" negative_control="Render a version from the tuples and its bytes differ."}\nEvery version renders the bytes it holds.\n:::\n\n' "$issue"
  corpus_shapes verification.md boundary-verification
} >"$work/verification.md"
corpus_upload "$section" "$issue" verification.md "$work/verification.md"
readonly verification="$corpus_doc"
corpus_revise "$section" "$verification" verification.md 2
corpus_revise "$section" "$verification" verification.md 3
{
  printf 'How the boundary is built.\n\n:::plan_task{#t-bytes serves="dispatch://%s/spec#b-r-bytes"}\nWrite the versions.\n:::\n\n' "$issue"
  corpus_shapes plan.md boundary-plan
} >"$work/plan.md"
corpus_upload "$section" "$issue" plan.md "$work/plan.md"
readonly plan="$corpus_doc"
corpus_revise "$section" "$plan" plan.md 2
{
  printf '# Notes\n\nThe boundary meets dispatch://%s/spec#b-r-bytes.\n\n' "$issue"
  corpus_shapes notes.md boundary-notes
} >"$work/notes.md"
corpus_upload "$section" "$issue" notes.md "$work/notes.md"
readonly notes="$corpus_doc"
corpus_revise "$section" "$notes" notes.md 2
corpus_revise "$section" "$notes" notes.md 3
corpus_image "$section" "$issue" diagram.png
readonly image="$corpus_doc"
pass "$section" "wrote $issue: its spec, verification.md and notes.md in three versions each, its plan.md in two, and the image $image"
# The document holding the block the hidden section makes author-only, written by the person, who
# may see it.
printf '# Private\n\nA paragraph every reader reads.\n\n%s\n%s\n:::\n\nA closing paragraph every reader reads.\n' \
  "$author_only_open" "$author_only_words" >"$work/private.md"
tree_person_send POST "/api/v1/issues/$issue/artifacts" "$(jq -cn --rawfile c "$work/private.md" '{name: "private.md", content: $c}')"
tree_want 201 "$section" "$person_login writes private.md"
tree_field "$section" "write private.md" .artifact.id
readonly author_only_written="$tree_value"
pass "$section" "$person_login wrote private.md ($author_only_written), holding the block #author-only"
corpus_wait_settled "$section"
corpus_review "$section" "$spec" approved
corpus_review "$section" "$verification" approved
corpus_review "$section" "$notes" approved
corpus_review "$section" "$plan" approved
corpus_revise "$section" "$plan" plan.md 3
unset 'corpus_approved[$plan]'
corpus_wait_settled "$section"
corpus_approvals_stand "$section"
baseline_stop_server

# --- upgrade: this head over those rows, writing nothing ---------------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"

# --- counts: R19's two counts over the corpus --------------------------------------------------------

section=counts
query "$section" "count the versions with a markdown body" \
  "select clock_timestamp(), (select count(*) from artifact_versions where markdown is not null)"
IFS='|' read -r at versions <<<"$answer"
corpus_count pre_cutover_versions "$versions" "$at"
((versions > 0)) || fail "$section" "the corpus holds versions with a markdown body" "the count reads $versions"
readonly standing_sql="with newest as (
    select distinct on (artifact_id) artifact_id, state, version, content_sha256
    from artifact_reviews
    order by artifact_id, event_seq desc nulls last, created_at desc, id desc
  )
  select n.artifact_id, n.version, n.content_sha256 from newest n
  where n.state = 'approved'
    and n.version = (select max(number) from artifact_versions v where v.artifact_id = n.artifact_id)"
query "$section" "count the standing approvals" "select clock_timestamp(), (select count(*) from ($standing_sql) standing)"
IFS='|' read -r at approvals <<<"$answer"
corpus_count standing_approvals "$approvals" "$at"
((approvals > 0)) || fail "$section" "the corpus holds standing approvals" "the count reads $approvals"
pass "$section" "the corpus holds $versions versions with a markdown body and $approvals standing approvals"

# --- render: every version with a markdown body, byte for byte ---------------------------------------

section=render
query "$section" "list the versions with a markdown body" \
  "select coalesce(string_agg(artifact_id || ' ' || number, E'\n' order by artifact_id, number), '') from artifact_versions where markdown is not null"
readonly rendered_versions="$answer"
rendered=0
while read -r id number; do
  [[ -n "$number" ]] || continue
  version_render_boundary_read "$work/jar" "/api/v1/artifacts/$id/versions/$number"
  tree_want 200 "$section" "render version $number of $id"
  jq -j '.markdown' "$work/body" >"$work/rendered" || fail "$section" "render version $number of $id" "no markdown in the answer: $(body)"
  version_render_boundary_stored "$id" "$number" "$work/stored"
  if ! cmp -s "$work/stored" "$work/rendered"; then
    version_render_boundary_first_difference "$work/stored" "$work/rendered"
    fail "$section" "every version written before the cutover renders as artifact_versions.markdown, byte for byte" \
      "artifact $id version $number differs from byte $tree_value (stored $(wc -c <"$work/stored") bytes, rendered $(wc -c <"$work/rendered"))"
  fi
  [[ "$render_source" == stored ]] ||
    fail "$section" "the renderer says version $number of $id answers its stored bytes" "Dispatch-Render-Source reads '$render_source', want stored"
  rendered=$((rendered + 1))
done <<<"$rendered_versions"
((rendered == versions)) ||
  fail "$section" "the renderer rendered every version with a markdown body" "it rendered $rendered of $versions"
pass "$section" "rendered $rendered versions, each byte for byte as artifact_versions.markdown and answered as its stored bytes"
query "$section" "count the version_renders rows of the rendered versions" \
  "select count(*) from version_renders r join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.number
   where v.markdown is not null"
[[ "$answer" == 0 ]] ||
  fail "$section" "no rendered version holds a version_renders row: the set is the pre-cutover side" "$answer of them hold one"
pass "$section" "no rendered version holds a version_renders row: the set is the pre-cutover side"

# --- approvals: each standing approval still covers the stored bytes ---------------------------------

section=approvals
query "$section" "recompute the digest of the stored markdown of each standing approval" "
  select coalesce(string_agg(s.artifact_id || ' v' || s.version || ' ' || coalesce(s.content_sha256, 'null') || ' ' ||
           coalesce(encode(sha256(convert_to(v.markdown, 'UTF8')), 'hex'), 'no-markdown'), E'\n' order by s.artifact_id), '')
  from ($standing_sql) s join artifact_versions v on v.artifact_id = s.artifact_id and v.number = s.version"
covered=0
while read -r id version pinned recomputed; do
  [[ -n "$recomputed" ]] || continue
  printf 'approval of %s %s: content_sha256 %s, the stored markdown digests to %s\n' "$id" "$version" "$pinned" "$recomputed"
  [[ "$pinned" == "$recomputed" ]] ||
    fail "$section" "every standing approval covers the stored bytes of its version" \
      "the approval of $id $version pins $pinned, and its stored markdown digests to $recomputed"
  covered=$((covered + 1))
done <<<"$answer"
((covered == approvals)) ||
  fail "$section" "every standing approval is over a rendered version" "$covered of $approvals approvals read a version with a markdown body"
pass "$section" "each of the $covered standing approvals pins the digest of its version's stored markdown"
query "$section" "read the update trigger on artifact_reviews.content_sha256 out of the catalog" \
  "select count(*) from pg_trigger where tgrelid = 'artifact_reviews'::regclass
     and tgname = 'artifact_reviews_content_sha256_update' and not tgisinternal and tgenabled <> 'D'"
[[ "$answer" == 1 ]] ||
  fail "$section" "the update trigger artifact_reviews_content_sha256_update stands on artifact_reviews" "the catalog holds $answer such enabled triggers"
pass "$section" "the update trigger artifact_reviews_content_sha256_update stands on artifact_reviews"

# --- none: a version that holds no markdown ------------------------------------------------------------

section=none
query "$section" "read a version whose markdown is null" \
  "select coalesce((select artifact_id || ' ' || number from artifact_versions where markdown is null order by artifact_id, number limit 1), '')"
read -r null_id null_number <<<"$answer"
[[ -n "$null_number" ]] || fail "$section" "the corpus holds a version whose markdown is null" "artifact_versions holds none"
version_render_boundary_read "$work/jar" "/api/v1/artifacts/$null_id/versions/$null_number"
tree_want 200 "$section" "read version $null_number of $null_id, which holds no markdown"
content_type="$(sed -nE 's/^[Cc]ontent-[Tt]ype: *([^;[:space:]]+).*$/\1/p' "$work/headers" | tail -n 1)"
[[ -z "$render_source" && "$content_type" == image/png ]] ||
  fail "$section" "version $null_number of $null_id is served as plain file bytes with no Dispatch-Render-Source header" \
    "Dispatch-Render-Source reads '$render_source', want no header; Content-Type reads '$content_type', want image/png"
printf '%s' "$corpus_png" | base64 -d | cmp -s - "$work/body" ||
  fail "$section" "version $null_number of $null_id answers the image's own bytes" "it answers $(wc -c <"$work/body") bytes that are not the PNG uploaded"
! jq -e '.markdown == ""' "$work/body" >/dev/null 2>&1 ||
  fail "$section" "version $null_number of $null_id answers no empty document" "it answers an empty markdown: $(body)"
pass "$section" "version $null_number of $null_id ($image is the image) is served as its own bytes, image/png, with no Dispatch-Render-Source header, and as no empty document"

# --- hidden: a block written author-only, read by a reader it is hidden from ----------------------------

section=hidden
# No API write of the earlier release can make a block author-only, so the store's row of
# #author-only is set to visibility author here, after the render section read it as the person.
with_vars "$section" "set the block #author-only of private.md to visibility author" \
  "update graph_nodes set visibility = 'author'
   where kind = 'block' and id = (select ref_key from artifacts where id = :'doc'::uuid) || '#author-only'
   returning id" doc="$author_only_written"
[[ -n "$answer" && "$answer" != *$'\n'* ]] ||
  fail "$section" "the block #author-only of private.md stands at visibility author" \
    "the update changed '$answer', want the one node of that block"
pass "$section" "the block $answer of private.md stands at visibility author"
query "$section" "read the rendered versions that hold the block written author-only" "
  select coalesce(string_agg(v.artifact_id || ' ' || v.number, E'\n' order by v.number), '')
  from artifact_versions v join artifacts a on a.id = v.artifact_id
  join graph_nodes n on n.kind = 'block' and n.id = a.ref_key || '#author-only' and n.visibility = 'author'
  where v.artifact_id = '$author_only_written' and v.markdown like '%{#author-only %'"
held="$answer"
[[ -n "$held" ]] ||
  fail "$section" "at least one rendered version holds a block written author-only" \
    "private.md ($author_only_written) holds no block node of visibility author"
while read -r id number; do
  [[ -n "$number" ]] || continue
  version_render_boundary_stored "$id" "$number" "$work/stored"
  # The block is its text as the version's record of block texts holds it (document_block_texts,
  # the latest row at or before the version), which the renderer leaves out at its place in the
  # stored bytes (docs' withoutHiddenBlocks); every other byte, the blank lines around it among
  # them, stays as stored.
  query "$section" "read the text version $number of $id records of #author-only" "
    select coalesce((select encode(convert_to(t.text, 'UTF8'), 'base64') from document_block_texts t
                     where t.artifact_id = '$id' and t.block_id = 'author-only' and t.version <= $number and t.text is not null
                     order by t.version desc limit 1), '')"
  base64 -d <<<"$answer" >"$work/hidden-block" ||
    fail "$section" "read the text version $number of $id records of #author-only" "base64 could not decode it"
  [[ -s "$work/hidden-block" ]] ||
    fail "$section" "version $number of $id records the text of #author-only" "document_block_texts holds none at or before it"
  jq -R -s -j --rawfile block "$work/hidden-block" \
    '(index($block)) as $from | if $from == null then error("no such block") else .[:$from] + .[$from + ($block | length):] end' \
    "$work/stored" >"$work/expected" ||
    fail "$section" "cut the author-only block out of version $number of $id" "its stored markdown does not hold the text its record holds"
  version_render_boundary_read "$work/jar.$hidden_from" "/api/v1/artifacts/$id/versions/$number"
  tree_want 200 "$section" "render version $number of $id as $hidden_from"
  jq -j '.markdown' "$work/body" >"$work/rendered" || fail "$section" "render version $number of $id as $hidden_from" "no markdown: $(body)"
  ! grep -qF "$author_only_words" "$work/rendered" ||
    fail "$section" "version $number of $id renders for $hidden_from without the block written author-only" "the answer holds its words"
  if ! cmp -s "$work/expected" "$work/rendered"; then
    version_render_boundary_first_difference "$work/expected" "$work/rendered"
    fail "$section" "version $number of $id renders for $hidden_from as stored, with the author-only block absent" \
      "artifact $id version $number differs from byte $tree_value of the stored bytes less that block"
  fi
  pass "$section" "version $number of $id renders for $hidden_from as stored, with the block written author-only absent"
done <<<"$held"

printf 'PASS: %s complete 8 sections\n' "$driver"
