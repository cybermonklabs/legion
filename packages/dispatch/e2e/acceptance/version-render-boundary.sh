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
# DEV_SIGNIN_LOGIN writes one more document, private.md, holding a block written author-only (a
# callout carrying visibility="author"), and approves the newest version of the spec, the
# verification.md and the notes.md, and an older version of the plan.md, which its next version
# supersedes: so the corpus holds standing approvals. A second person, bob, signs in through the
# same server, so that this head's half reads as a reader the block is hidden from and writes
# nothing. It stops that server, boots this head over the same database with the hold of R6's step,
# waits for the backfill, and from then on writes nothing.
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
# catalog. It reads the version whose markdown is null, the image's, and finds the renderer saying
# it holds no bytes (Dispatch-Render-Source: none) rather than answering an empty document. Last it
# reads the rendered versions that hold a block written author-only (a block node of visibility
# author whose fence the version's markdown holds): at least one must stand, and each renders for
# bob with that block absent and the rest of the version byte for byte as stored.
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
# The fence that opens the block written author-only, and its words.
readonly author_only_open=':::callout{#author-only kind="note" title="" visibility="author"}'
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

# version_render_boundary_empty_database: drops and creates the database database_url names, so
# the earlier release's Store.Migrate writes its schema and the corpus is its only content. It takes
# only infra_1253_upgrade on this machine, the database of this issue's upgrade drivers, and
# refuses every other with status 2.
version_render_boundary_empty_database() {
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

version_render_boundary_empty_database
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
readonly issue="$corpus_key" spec="$corpus_spec"
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
# The document holding a block written author-only, written by the person, who may see it.
printf '# Private\n\nA paragraph every reader reads.\n\n%s\n%s\n:::\n\nA closing paragraph every reader reads.\n' \
  "$author_only_open" "$author_only_words" >"$work/private.md"
tree_person_send POST "/api/v1/issues/$issue/artifacts" "$(jq -cn --rawfile c "$work/private.md" '{name: "private.md", content: $c}')"
author_only_written=""
if [[ "$status" == 201 ]]; then
  tree_field "$section" "write private.md" .artifact.id
  author_only_written="$tree_value"
  pass "$section" "$person_login wrote private.md ($author_only_written), holding a block written author-only"
else
  readonly author_only_refusal="$tree_last_request answered HTTP $status: $(body)"
  printf '%s: the earlier release refused the document holding a block written author-only: %s\n' "$driver" "$author_only_refusal"
fi
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
printf 'count pre_cutover_versions=%s at %s\n' "$versions" "$at"
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
printf 'count standing_approvals=%s at %s\n' "$approvals" "$at"
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
[[ "$render_source" == none ]] ||
  fail "$section" "the renderer says version $null_number of $null_id holds no bytes" \
    "Dispatch-Render-Source reads '$render_source', want none; the answer is $(grep -i '^Content-Type:' "$work/headers" | tr -d '\r')"
! jq -e '.markdown == ""' "$work/body" >/dev/null 2>&1 ||
  fail "$section" "version $null_number of $null_id answers no empty document" "it answers an empty markdown: $(body)"
pass "$section" "the renderer says version $null_number of $null_id ($image is the image) holds no bytes, and answers no empty document"

# --- hidden: a block written author-only, read by a reader it is hidden from ----------------------------

section=hidden
held=""
if [[ -n "$author_only_written" ]]; then
  query "$section" "read the rendered versions that hold the block written author-only" "
    select coalesce(string_agg(v.artifact_id || ' ' || v.number, E'\n' order by v.number), '')
    from artifact_versions v join artifacts a on a.id = v.artifact_id
    join graph_nodes n on n.kind = 'block' and n.id = a.ref_key || '#author-only' and n.visibility = 'author'
    where v.artifact_id = '$author_only_written' and v.markdown like '%{#author-only %'"
  held="$answer"
fi
[[ -n "$held" ]] ||
  fail "$section" "at least one rendered version holds a block written author-only" \
    "${author_only_refusal:-private.md ($author_only_written) holds no block node of visibility author}"
while read -r id number; do
  [[ -n "$number" ]] || continue
  version_render_boundary_stored "$id" "$number" "$work/stored"
  # The block runs from its opening fence, which the stored markdown writes in its own attribute
  # order, through its closing fence's line end; the blank line after it goes with it.
  jq -R -s -j --arg open ':::callout{#author-only' \
    '(index($open)) as $from | (.[$from:] | index("\n:::\n")) as $close
     | .[:$from] + (.[$from + $close + 5:] | ltrimstr("\n"))' "$work/stored" >"$work/expected" ||
    fail "$section" "cut the author-only block out of version $number of $id" "its stored markdown holds no such block"
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
