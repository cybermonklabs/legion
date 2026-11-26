#!/usr/bin/env bash
# Check c-artifacts-in-tree (the issue tree's spec, requirement r-artifacts-in-tree, R16): every
# artifact of an issue is in the tree, over rows the earlier release wrote and rows this head
# writes, and the block index's marking rule 2 holds across a build of rule 1 running beside it.
#
# It runs over infra_1253_upgrade, as the Verification preamble states it for a driver that reads
# rows an earlier release wrote. Its sections, in the order they run:
#   setup    empties the database DATABASE_URL names (on this machine, its name beginning
#            infra_1253_upgrade), boots the release before through lib-baseline.sh's baseline_start,
#            signs DEV_SIGNIN_LOGIN in and creates PROJECT;
#   before   through the release before writes issue O: its spec (a requirement and a paragraph),
#            and uploads verification.md (the markdown document), diagram.png (an image),
#            data.csv (a file), plan.md and notes.md; it captures the ref key, the version count and
#            the bytes of each version of the three artifacts (an md5 of each, read by SQL), then
#            stops that server;
#   held     a build of rule 1 uses up no mark of rule 2: it boots this head held
#            (DISPATCH_TEST_HOOKS=1, DISPATCH_TEST_HOLD_ISSUE_ROOM_STEP=1) and with
#            DISPATCH_TEST_HOLD_BLOCK_INDEX=1, waits for the line that ends its boot backfill
#            (lib-tree.sh's tree_backfill_end), reads O's spec row of document_block_versions marked
#            (store_owed_at set, store_owed_rule 2), boots the release before beside it, has it write
#            a new version of that spec and waits (2 s, 300 s) until the row's store_version is that
#            version, and reads the mark still there; then, in the same window, has the release
#            before create issue N, whose spec holds a requirement, a check and a paragraph, waits
#            until that spec's row stands at its version, and reads it holding no mark;
#   restart  stops the release before, restarts this head without the block index's hold, waits
#            for the backfill, and reads: O's new spec version has its version node, each of its
#            blocks its rank and the supertag of its type, and the row states that version at rule
#            2 with no mark; the horizon of rule 2 in graph_backfill_horizon stands above the id of
#            N's issue.created event; and N's spec is stated at rule 2, its requirement, check and
#            paragraph carrying the supertags requirement, check and block, each with its rank;
#   walk-before every artifact the earlier release stored on O is a node under O, and the three
#            artifacts' bytes and version counts equal the capture, and this head serves the
#            image's bytes byte for byte as they were uploaded;
#   walk-head through this head writes issue H with the same three artifacts, and reaches each as a
#            node under H; the image's node holds its mime, size and sha256 as values, and its bytes
#            read back through GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number} byte
#            for byte;
#   document H's Verification document is a subtree of block nodes, each a node of the store and its
#            checks in the verification query's view; a new version written by an edit has its
#            version node, and its stored markdown (artifact_versions.markdown) is what the version
#            read renders;
#   convert  waits for this head's build announcement and converts PROJECT
#            (`envoy-dispatch convert-issues --project <PROJECT>`); O's spec, Verification document
#            and plan each read back as a query node stating the types it takes, with their blocks
#            under O; the role of each in GET /api/v1/issues/{key}/artifacts and .../{slug} is the
#            name of its query, and no column of O's artifacts rows holds a role; GET
#            /api/v1/issues/{key} answers verification_artifact_id and plan_artifact_id with the
#            query nodes' artifacts; the role census of R16 reads 0, and a role planted on one row
#            (in a transaction rolled back) is counted; artifacts.role still stands;
#   rule2    boots the release before beside this head and has it write a new version of O's
#            notes.md, whose mark this head's pass has cleared; reads the release before's
#            block_index_event_cursors row at marking_rule 1, and the row of notes.md at
#            store_version equal to that version with stated_version behind it, before this head's
#            tick; after that tick the version has its version node, each block its rank, text and
#            supertag, the row states it at rule 2 and the census owes nothing, with no second
#            backfill and no restart; this head's cursor row is at marking_rule 2, at or above the
#            horizon rule 2 recorded; a version this head then writes holds no mark after its tick;
#   retired  a later backfill marks no document rule 2 has stated: a fresh issue L, born converted,
#            whose plan.md is uploaded with one plan node, then again holding the pre-mortem of
#            e2e/fixtures/acme-1098-premortem.json under the first version's token, and converted by
#            POST /api/v1/issues/{key}/artifacts/plan-md/premortem-convert, which retires it in its
#            room; by SQL the retired plan's row and the room's tree artifact's row are marked as the
#            backfill of rule 2 leaves an unstated document, the owed count falls to 0, and after a
#            restart held (DISPATCH_TEST_HOLD_BLOCK_INDEX=1) whose boot runs the backfill again the
#            owed count still reads 0 and each row the driver marked holds stated_rule 2;
#   census   in that held boot, L's Verification document is uploaded twice above the bound, so its
#            first version is deferred and replaced before any pass states it; after a restart and
#            the wait for the backfill, the stored markdown of every version whose version_renders
#            row holds a time equals the render the version read answers (Dispatch-Render-Source
#            tree), and `envoy-dispatch census --store` prints its `census: versions` line with
#            tree_mismatch=0 and tree_mismatch_repaired=0 (lib-tree.sh's tree_census_versions, which
#            otherwise fails naming the corpus and printing that line), rendered_from_tree equal to
#            the rows that hold a time and deferred equal to the rows that hold none, at least one.
# The Go half of the check, TestAVersionWrittenAfterTheTickListedItsDocumentStaysOwed, is the
# check's own step.
#
# Inputs: DATABASE_URL (infra_1253_upgrade, emptied here), DISPATCH_TOKEN (the agent bearer every
# server accepts, e2e-token), PROJECT (CORE), DEV_SIGNIN_LOGIN (optional, `alice` by default),
# BASELINE_SHA or BASELINE_BINARY and LEGION_WORKSPACE as lib-baseline.sh reads them, and on PATH
# envoy-dispatch (this head, stamped), curl, jq, psql, cmp, sha256sum, base64, go and git. Each unmet
# input is exit 2. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and exits 1. The last line is
# `PASS: issue-artifacts.sh complete 11 sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && DATABASE_URL=$INFRA_1253_UPGRADE_DATABASE_URL DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE [DEV_SIGNIN_LOGIN=alice] BASELINE_SHA=<commit> bash e2e/acceptance/issue-artifacts.sh
       (DATABASE_URL names infra_1253_upgrade on this machine, which this driver empties; the
       stamped envoy-dispatch of this head is on PATH)
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
require_commands base64 cmp curl cut envoy-dispatch jq md5sum paste psql sed sha256sum tr wc
readonly driver=issue-artifacts.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
# The pairs every server gets that the run gave, which a boot here adds to for one server alone.
readonly given_extra_env="${BASELINE_EXTRA_ENV-}"
readonly hold_index=DISPATCH_TEST_HOLD_BLOCK_INDEX=1
# The blocks past which a version write leaves its block index to the reconciliation
# (docs' deferBlockIndexBlocks).
readonly bound_blocks=2000
fixture="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../fixtures" && pwd)/acme-1098-premortem.json"
readonly fixture
[[ -r "$fixture" ]] || {
  printf '%s: the pre-mortem capture %s cannot be read\n' "$driver" "$fixture" >&2
  exit 2
}

# A CSV, which every upload route stores as a file; the image is seed-corpus.sh's corpus_png, which
# its corpus_image uploads.
readonly csv_text=$'name,value\nalpha,1\nbeta,2\n'

# The servers the driver boots beside lib-tree.sh's head: the release before, booted again.
before_pid=""
before_url=""
boots=0

# --- the helpers of this driver ---------------------------------------------------------------------

# issue_artifacts_boot_head [EXTRA]: boots this head, held as every upgrade boot is, with the pair
# EXTRA beside the run's own for this boot alone; it sets base to its address. Each boot logs to a
# file of its own.
issue_artifacts_boot_head() {
  boots=$((boots + 1))
  BASELINE_EXTRA_ENV="${given_extra_env:+$given_extra_env$'\n'}${1:-}"
  tree_boot_head "head-$boots" held
  BASELINE_EXTRA_ENV="$given_extra_env"
  baseline_read_extra_env
  base="$tree_head_url"
}

# issue_artifacts_boot_before: boots the release before again, beside this head, over the same
# database, as baseline_start booted it; it leaves before_pid and before_url.
issue_artifacts_boot_before() {
  boots=$((boots + 1))
  baseline_boot "before-$boots" "$baseline_binary" "$baseline_database_url"
  before_pid="$baseline_boot_pid"
  before_url="$baseline_boot_url"
  printf 'release before %s beside this head\n' "$before_url"
}

issue_artifacts_stop_before() {
  [[ -z "$before_pid" ]] || baseline_stop_pid "$before_pid"
  before_pid=""
  before_url=""
}

# issue_artifacts_wait_log SECTION LOG: waits, every 2 s for up to 300 s, until LOG holds the line
# that ends the boot backfill (tree_backfill_end), and fails naming the log where it never does.
issue_artifacts_wait_log() {
  local deadline=$((SECONDS + tree_wait_seconds))
  until grep -qF -- "$tree_backfill_end" "$2"; do
    ((SECONDS < deadline)) ||
      fail "$1" "the boot backfill logs its end within ${tree_wait_seconds} s" "$2 holds no line '$tree_backfill_end'"
    sleep "$tree_poll_seconds"
  done
  pass "$1" "the boot backfill logged its end ($2)"
}

# issue_artifacts_row SECTION ARTIFACT: the document_block_versions row of ARTIFACT, each column
# `null` where it holds none: row_version, row_store_version, row_owed (store_owed_at),
# row_owed_rule, row_stated_version, row_stated_rule, and row_line naming them all.
row_version="" row_store_version="" row_owed="" row_owed_rule="" row_stated_version="" row_stated_rule="" row_line=""
issue_artifacts_row() {
  with_vars "$1" "read the document_block_versions row of $2" "
    select coalesce((select concat_ws('|', coalesce(version::text, 'null'), coalesce(store_version::text, 'null'),
             coalesce(store_owed_at::text, 'null'), coalesce(store_owed_rule::text, 'null'),
             coalesce(stated_version::text, 'null'), coalesce(stated_rule::text, 'null'))
           from document_block_versions where artifact_id = :'a'), 'none|none|none|none|none|none')" "a=$2"
  IFS='|' read -r row_version row_store_version row_owed row_owed_rule row_stated_version row_stated_rule <<<"$answer"
  row_line="version=$row_version store_version=$row_store_version store_owed_at=$row_owed store_owed_rule=$row_owed_rule stated_version=$row_stated_version stated_rule=$row_stated_rule"
}

# issue_artifacts_owed SECTION: the owed count of the Verification preamble, in owed_count.
owed_count=""
issue_artifacts_owed() {
  query "$1" "read the owed count" \
    "select count(*) from document_block_versions where version is null or store_owed_at is not null"
  owed_count="$answer"
}

# issue_artifacts_values SECTION KIND ID ATTRIBUTE: the values the node KIND/ID holds now under
# ATTRIBUTE, comma-separated, `(none)` for none, in node_values.
node_values=""
issue_artifacts_values() {
  with_vars "$1" "read the $4 of $2 $3" "
    select coalesce(string_agg(held.to_id, ',' order by held.to_id), '(none)') from (
      select distinct on (to_id) to_id, asserted from graph_tuples
      where from_kind = :'fk' and from_id = :'fi' and kind = :'k' and to_kind = 'value'
      order by to_id, seq desc) held
    where held.asserted" "fk=$2" "fi=$3" "k=$4"
  node_values="$answer"
}

# issue_artifacts_node SECTION KIND ID: the supertag and state of the node KIND/ID, `(none)` for no
# node, in node_row.
node_row=""
issue_artifacts_node() {
  with_vars "$1" "read the node $2 $3" "
    select coalesce((select supertag || '|' || state from graph_nodes where kind = :'k' and id = :'i'), '(none)')" \
    "k=$2" "i=$3"
  node_row="$answer"
}

# issue_artifacts_version_node SECTION REF_KEY NUMBER: version NUMBER of the document REF_KEY has
# its live version node (graphstore.VersionID), a document version or a query version.
issue_artifacts_version_node() {
  issue_artifacts_node "$1" version "$2@v$3"
  case "$node_row" in
    "document version|live" | "query version|live") ;;
    *) fail "$1" "version $3 of $2 has its version node" "the store holds $node_row for version $2@v$3" ;;
  esac
  pass "$1" "version $3 of $2 has its version node ($node_row)"
}

# issue_artifacts_resolve SECTION REF: the store node the reference REF names, as the reference
# read resolves it (GET /api/v1/references?to=); sets resolved_kind and resolved_id.
resolved_kind=""
resolved_id=""
issue_artifacts_resolve() {
  tree_send GET "/api/v1/references?to=$(q "$2")"
  tree_want 200 "$1" "read the node $2 names"
  tree_field "$1" "read the node $2 names" .node.kind
  resolved_kind="$tree_value"
  tree_field "$1" "read the node $2 names" .node.id
  resolved_id="$tree_value"
}

# The typed block names of the schema this head installed (GET /api/v1/schema/blocks), whose block
# nodes carry their type as supertag; every other block's node carries `block`.
typed_names=""

# issue_artifacts_blocks SECTION DOCUMENT ARTIFACT [TEXT]: each block of ARTIFACT's newest version,
# as its blocks read lists it, is a live node of the store, by the reference DOCUMENT#b-<id>, that
# holds its rank and the supertag of its type, and where TEXT is `text` its text too; it fails
# naming the document, the block and what differs.
issue_artifacts_blocks() {
  local section="$1" document="$2" artifact="$3" with_text="${4:-}" line id type want count=0
  local -a blocks
  tree_send GET "/api/v1/artifacts/$artifact/blocks"
  tree_want 200 "$section" "read the blocks of $document"
  mapfile -t blocks < <(jq -r '.[] | "\(.id)\t\(.type)"' "$work/body")
  ((${#blocks[@]} > 0)) || fail "$section" "read the blocks of $document" "the blocks read lists none"
  for line in "${blocks[@]}"; do
    IFS=$'\t' read -r id type <<<"$line"
    issue_artifacts_resolve "$section" "$document#b-$id"
    want=block
    [[ ",$typed_names," != *",$type,"* ]] || want="$type"
    issue_artifacts_node "$section" "$resolved_kind" "$resolved_id"
    [[ "$node_row" == "$want|live" ]] ||
      fail "$section" "block $id of $document carries the supertag $want" \
        "its node $resolved_kind $resolved_id reads $node_row (the block is a $type)"
    issue_artifacts_values "$section" "$resolved_kind" "$resolved_id" rank
    [[ "$node_values" != "(none)" ]] ||
      fail "$section" "block $id of $document holds its rank" "its node $resolved_kind $resolved_id holds no rank"
    if [[ "$with_text" == text ]]; then
      issue_artifacts_values "$section" "$resolved_kind" "$resolved_id" text
      [[ "$node_values" != "(none)" ]] ||
        fail "$section" "block $id of $document holds its text" "its node $resolved_kind $resolved_id holds no text"
    fi
    count=$((count + 1))
  done
  pass "$section" "each of the $count blocks of $document is a node holding its rank${with_text:+ and its text} and the supertag of its type"
}

# issue_artifacts_edit SECTION ARTIFACT FROM TO: seed-corpus.sh's corpus_edit of one replace: the
# agent replaces FROM by TO in ARTIFACT, which writes its next version.
issue_artifacts_edit() {
  corpus_edit "$1" "edit $2" "$2" "$(jq -cn --arg f "$3" --arg w "$4" '[{op: "replace", find: $f, with: $w}]')"
}

# issue_artifacts_issue SECTION TITLE FILE: seed-corpus.sh's corpus_issue, the agent creating an
# issue of PROJECT whose spec is FILE's markdown; sets created_key and created_spec.
created_key=""
created_spec=""
issue_artifacts_issue() {
  corpus_issue "$1" "$2" "$3"
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
  created_key="$corpus_key"
  created_spec="$corpus_spec"
}

# issue_artifacts_upload SECTION ISSUE NAME FILE: seed-corpus.sh's corpus_upload, the agent
# uploading FILE as the document NAME of ISSUE (a name ISSUE holds already writes its next version,
# under the token of the version it replaces); sets uploaded_id, uploaded_slug and uploaded_ref.
uploaded_id=""
uploaded_slug=""
uploaded_ref=""
issue_artifacts_upload() {
  corpus_upload "$1" "$2" "$3" "$4"
  uploaded_id="$corpus_doc"
  uploaded_slug="$corpus_slug"
  uploaded_ref="$2/$corpus_slug"
}

# issue_artifacts_image SECTION ISSUE: seed-corpus.sh's corpus_image, the agent uploading its PNG,
# corpus_png, as diagram.png of ISSUE, stored as an image; sets what issue_artifacts_upload sets.
issue_artifacts_image() {
  corpus_image "$1" "$2" diagram.png
  uploaded_id="$corpus_doc"
  tree_field "$1" "upload diagram.png to $2" .artifact.slug
  uploaded_slug="$tree_value"
  uploaded_ref="$2/$tree_value"
}

# issue_artifacts_file SECTION ISSUE: the agent uploads $work/data.csv as data.csv of ISSUE through
# the multipart form the dashboard sends, declared text/csv, which the route stores as a file (no
# writer of seed-corpus.sh uploads a file); sets what issue_artifacts_upload sets.
issue_artifacts_file() {
  tree_last_request="POST /api/v1/issues/$2/artifacts (multipart data.csv)"
  status="$(curl -sS -X POST -o "$work/body" -w '%{http_code}' -H "@$work/auth" \
    -F "name=data.csv" -F "actor=$(tree_actor)" -F "file=@$work/data.csv;type=text/csv" \
    "$base/api/v1/issues/$2/artifacts" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  tree_want 201 "$1" "upload data.csv to $2"
  expect "$1" "data.csv is stored as a file" '"file"' .artifact.kind
  tree_field "$1" "upload data.csv to $2" .artifact.id
  uploaded_id="$tree_value"
  tree_field "$1" "upload data.csv to $2" .artifact.slug
  uploaded_slug="$tree_value"
  uploaded_ref="$2/$tree_value"
}

# issue_artifacts_download SECTION ISSUE SLUG NUMBER FILE: version NUMBER of SLUG on ISSUE, as GET
# /api/v1/issues/{key}/artifacts/{slug}/versions/{number} answers it (with ?download=true, a
# document's markdown as itself), into FILE, and its Dispatch-Render-Source in render_source.
render_source=""
issue_artifacts_download() {
  tree_last_request="GET /api/v1/issues/$2/artifacts/$3/versions/$4?download=true"
  status="$(curl -sS -o "$5" -D "$work/headers" -w '%{http_code}' -H "@$work/auth" \
    "$base/api/v1/issues/$(q "$2")/artifacts/$(q "$3")/versions/$4?download=true" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  cp "$5" "$work/body"
  tree_want 200 "$1" "download version $4 of $3 on $2"
  render_source="$(sed -nE 's/^[Dd]ispatch-[Rr]ender-[Ss]ource: *([a-z]+).*$/\1/p' "$work/headers")"
}

# issue_artifacts_walk SECTION ISSUE: every artifact of ISSUE but its tree is a live node of the
# store whose newest part_of names ISSUE; it fails naming the ref key of each the walk does not
# reach.
issue_artifacts_walk() {
  with_vars "$1" "walk the artifacts of $2" "
    select coalesce(string_agg(a.ref_key, ' ' order by a.ref_key) filter (where not reached), ''),
           count(*) filter (where reached), count(*)
    from (
      select a.ref_key, exists (
          select 1 from graph_nodes node
          where node.kind = 'artifact' and node.id = a.ref_key and node.state <> 'gone'
            and coalesce((select tuple.asserted from graph_tuples tuple
                          where tuple.from_kind = 'artifact' and tuple.from_id = a.ref_key and tuple.kind = 'part_of'
                            and tuple.to_kind = 'issue' and tuple.to_id = a.issue_key
                          order by tuple.seq desc limit 1), false)) as reached
      from artifacts a where a.issue_key = :'k' and a.kind <> 'tree') a" "k=$2"
  local unreached reached total
  IFS='|' read -r unreached reached total <<<"$answer"
  [[ "$total" -gt 0 ]] || fail "$1" "walk the artifacts of $2" "the issue holds no artifact"
  [[ -z "$unreached" ]] ||
    fail "$1" "every artifact of $2 is a node under $2" \
      "the walk reaches $reached of $total; it does not reach $unreached"
  pass "$1" "the walk of $2 reaches all $total of its artifacts as nodes under it"
}

# issue_artifacts_capture SECTION FILE IDS: for each artifact of IDS (a space-separated list), one
# line `<ref_key> <kind> <versions> <md5 of each version's bytes>`, written to FILE.
issue_artifacts_capture() {
  with_vars "$1" "read the ref key, the versions and the bytes of $3" "
    select string_agg(line, chr(10) order by line) from (
      select a.ref_key || ' ' || a.kind || ' ' || count(v.id) || ' '
             || string_agg(md5(coalesce(v.content, convert_to(v.markdown, 'UTF8'))), ',' order by v.number) as line
      from artifacts a join artifact_versions v on v.artifact_id = a.id
      where a.id = any(string_to_array(:'ids', ' ')::uuid[])
      group by a.id, a.ref_key, a.kind) captured" "ids=$3"
  printf '%s\n' "$answer" >"$2"
}

work_dir
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  issue_artifacts_stop_before
  tree_stop_head
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s' "$corpus_png" | base64 -d >"$work/diagram.png" || {
  printf '%s: base64 could not decode the image\n' "$driver" >&2
  exit 2
}
printf '%s' "$csv_text" >"$work/data.csv"
image_sha256="$(sha256sum "$work/diagram.png" | cut -d' ' -f1)"
image_size="$(wc -c <"$work/diagram.png" | tr -d ' ')"
readonly image_sha256 image_size

# --- setup: the release before over an empty database ----------------------------------------------

section=setup
tree_reset_database "$database_url" 'infra_1253_upgrade*' "$section"
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-artifacts-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "Every artifact in the tree"

# --- before: the issue the earlier release writes ---------------------------------------------------

section=before
readonly old_sentence="The spec of the issue the release before writes."
cat >"$work/old-spec.md" <<SPEC
$old_sentence

:::requirement{#r-old}
Every artifact the release before stored is a node after the upgrade.
:::
SPEC
issue_artifacts_issue "$section" "The artifacts the release before stores" "$work/old-spec.md"
old="$created_key" old_spec="$created_spec"
readonly old old_spec
cat >"$work/verification.md" <<VERIFICATION
How the artifacts are proved.

:::check{#c-old serves="dispatch://$old/spec#b-r-old"}
The walk reaches every artifact.
:::
VERIFICATION
issue_artifacts_upload "$section" "$old" verification.md "$work/verification.md"
old_verification="$uploaded_id" old_verification_ref="$uploaded_ref"
issue_artifacts_image "$section" "$old"
old_image="$uploaded_id" old_image_slug="$uploaded_slug" old_image_ref="$uploaded_ref"
issue_artifacts_file "$section" "$old"
old_file="$uploaded_id" old_file_ref="$uploaded_ref"
printf ':::plan_task{#t-old serves="dispatch://%s/spec#b-r-old"}\nWalk the artifacts.\n:::\n' "$old" >"$work/plan.md"
issue_artifacts_upload "$section" "$old" plan.md "$work/plan.md"
old_plan="$uploaded_id"
readonly notes_sentence="The notes the release before writes again later."
printf '# Notes\n\n%s\n' "$notes_sentence" >"$work/notes.md"
issue_artifacts_upload "$section" "$old" notes.md "$work/notes.md"
old_notes="$uploaded_id" old_notes_ref="$uploaded_ref"
readonly old_verification old_verification_ref old_image old_image_slug old_image_ref old_file old_file_ref
readonly old_plan old_notes old_notes_ref
issue_artifacts_capture "$section" "$work/captured.txt" "$old_verification $old_image $old_file"
printf 'captured: %s\n' "$(paste -sd ';' "$work/captured.txt")"
[[ "$(wc -l <"$work/captured.txt")" == 3 ]] ||
  fail "$section" "capture the three artifacts the release before stored" "$(<"$work/captured.txt")"
pass "$section" "captured the ref key, versions and bytes of $old_verification_ref, $old_image_ref and $old_file_ref"
baseline_stop_server
pass "$section" "stopped the release before"

# --- held: a build of rule 1 beside this head, its block index held ----------------------------------

section=held
issue_artifacts_boot_head "$hold_index"
issue_artifacts_wait_log "$section" "$tree_head_log"
readonly head_commit="$tree_head_commit"
issue_artifacts_row "$section" "$old_spec"
[[ "$row_owed" != null && "$row_owed_rule" == 2 ]] ||
  fail "$section" "the boot backfill marks $old's spec at rule 2" "its row reads $row_line"
pass "$section" "the boot backfill marked $old's spec ($old_spec) at rule 2: $row_line"
issue_artifacts_boot_before
base="$before_url"
issue_artifacts_edit "$section" "$old_spec" "$old_sentence" "$old_sentence It holds a second version."
corpus_version "$section" "$old_spec"
spec_version="$tree_value"
readonly spec_version
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_row "$section" "$old_spec"
  [[ "$row_store_version" != "$spec_version" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the release before indexes version $spec_version of $old's spec in its request" \
      "after ${tree_wait_seconds} s its row reads $row_line"
  sleep "$tree_poll_seconds"
done
[[ "$row_owed" != null && "$row_owed_rule" == 2 ]] ||
  fail "$section" "the release before's index of version $spec_version uses up no mark of rule 2" \
    "document $old_spec: its row reads $row_line, store_owed_at cleared where the mark of rule 2 stood"
pass "$section" "the release before indexed version $spec_version of $old's spec, and the mark of rule 2 stands: $row_line"
cat >"$work/new-spec.md" <<'SPEC'
The spec of the issue the release before creates while this head is held.

:::requirement{#r-new}
The backfill of rule 2 marks this spec at the restart.
:::

:::check{#c-new}
The restart states the spec at rule 2.
:::
SPEC
issue_artifacts_issue "$section" "The issue the release before creates beside a held head" "$work/new-spec.md"
new="$created_key" new_spec="$created_spec"
readonly new new_spec
corpus_version "$section" "$new_spec"
new_version="$tree_value"
readonly new_version
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_row "$section" "$new_spec"
  [[ "$row_store_version" != "$new_version" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the release before indexes $new's spec in its request" "after ${tree_wait_seconds} s its row reads $row_line"
  sleep "$tree_poll_seconds"
done
[[ "$row_owed" == null ]] ||
  fail "$section" "$new's spec, created after this head's backfill ran, holds no mark" "its row reads $row_line"
pass "$section" "$new's spec ($new_spec) stands at version $new_version with no mark: $row_line"
with_vars "$section" "read the id of $new's issue.created event" "
  select coalesce((select min(id)::text from events where issue_key = :'k' and type = 'issue.created'), 'none')" "k=$new"
created_event="$answer"
readonly created_event
[[ "$created_event" =~ ^[0-9]+$ ]] || fail "$section" "read the id of $new's issue.created event" "the log holds $created_event"
issue_artifacts_stop_before
pass "$section" "stopped the release before"

# --- restart: this head unheld, and what rule 2 states ------------------------------------------------

section=restart
tree_stop_head
issue_artifacts_boot_head
tree_wait_backfill "$section"
tree_send GET /api/v1/schema/blocks
tree_want 200 "$section" "read the block schema this head installed"
tree_field "$section" "read the block schema this head installed" '[.types[].name] | join(",")'
typed_names="$tree_value"
readonly typed_names
issue_artifacts_version_node "$section" "$old/spec" "$spec_version"
issue_artifacts_blocks "$section" "dispatch://$old/spec" "$old_spec"
issue_artifacts_row "$section" "$old_spec"
[[ "$row_stated_version" == "$spec_version" ]] ||
  fail "$section" "the restart states version $spec_version of $old's spec" "document $old_spec: stated_version differs, its row reads $row_line"
[[ "$row_owed" == null ]] ||
  fail "$section" "$old's spec holds no mark after the restart" "document $old_spec: store_owed_at differs, its row reads $row_line"
pass "$section" "$old's spec stands stated at version $spec_version with no mark: $row_line"
query "$section" "read the horizon of rule 2" \
  "select coalesce((select event_id::text from graph_backfill_horizon where marking_rule = 2), 'none')"
horizon="$answer"
[[ "$horizon" =~ ^[0-9]+$ && "$horizon" -gt "$created_event" ]] ||
  fail "$section" "the horizon of rule 2 stands above $new's issue.created event, so no tick of this head reads it" \
    "graph_backfill_horizon records $horizon for rule 2, and $new's issue.created event is $created_event"
pass "$section" "the horizon of rule 2, $horizon, stands above $new's issue.created event, $created_event"
issue_artifacts_row "$section" "$new_spec"
[[ "$row_stated_version" == "$new_version" && "$row_stated_rule" == 2 && "$row_owed" == null ]] ||
  fail "$section" "the backfill of rule 2 marks $new's spec and the pass states it" \
    "issue $new, its spec document $new_spec: stated_version $row_stated_version, stated_rule $row_stated_rule, store_owed_at $row_owed"
issue_artifacts_blocks "$section" "dispatch://$new/spec" "$new_spec"
pass "$section" "$new's spec stands stated at version $new_version at rule 2: $row_line"

# --- walk-before: the earlier release's artifacts are nodes ------------------------------------------

section=walk-before
issue_artifacts_walk "$section" "$old"
issue_artifacts_capture "$section" "$work/walked.txt" "$old_verification $old_image $old_file"
cmp -s "$work/captured.txt" "$work/walked.txt" ||
  fail "$section" "the three artifacts hold the bytes and the versions the capture recorded" \
    "captured: $(paste -sd ';' "$work/captured.txt"); read now: $(paste -sd ';' "$work/walked.txt")"
issue_artifacts_download "$section" "$old" "$old_image_slug" 1 "$work/walked-image.bin"
cmp -s "$work/walked-image.bin" "$work/diagram.png" ||
  fail "$section" "this head serves the earlier release's diagram.png byte for byte" "the bytes differ"
pass "$section" "$old_verification_ref, $old_image_ref and $old_file_ref hold the bytes and the versions the capture recorded"

# --- walk-head: the same three artifacts, written through this head ----------------------------------

section=walk-head
printf 'The spec of the issue this head writes.\n\n:::requirement{#r-head}\nEvery artifact this head stores is a node.\n:::\n' >"$work/head-spec.md"
issue_artifacts_issue "$section" "The artifacts this head stores" "$work/head-spec.md"
head_issue="$created_key"
readonly head_issue
readonly head_sentence="The walk reaches every artifact this head stores."
cat >"$work/head-verification.md" <<VERIFICATION
How the artifacts this head stores are proved.

:::check{#c-head serves="dispatch://$head_issue/spec#b-r-head"}
$head_sentence
:::
VERIFICATION
issue_artifacts_upload "$section" "$head_issue" verification.md "$work/head-verification.md"
head_verification="$uploaded_id" head_verification_slug="$uploaded_slug" head_verification_ref="$uploaded_ref"
issue_artifacts_image "$section" "$head_issue"
head_image_slug="$uploaded_slug" head_image_ref="$uploaded_ref"
issue_artifacts_file "$section" "$head_issue"
readonly head_verification head_verification_slug head_verification_ref head_image_slug head_image_ref
issue_artifacts_walk "$section" "$head_issue"
for value in "mime image/png" "size $image_size" "sha256 $image_sha256"; do
  read -r attribute want <<<"$value"
  issue_artifacts_values "$section" artifact "$head_image_ref" "$attribute"
  [[ "$node_values" == "$want" ]] ||
    fail "$section" "the image's node holds its $attribute as a value" "the node $head_image_ref holds $attribute $node_values, want $want"
done
pass "$section" "the node $head_image_ref holds mime image/png, size $image_size and sha256 $image_sha256 as values"
issue_artifacts_download "$section" "$head_issue" "$head_image_slug" 1 "$work/head-image.bin"
cmp -s "$work/head-image.bin" "$work/diagram.png" ||
  fail "$section" "GET /api/v1/issues/$head_issue/artifacts/$head_image_slug/versions/1 answers the image byte for byte" "the bytes differ"
pass "$section" "GET /api/v1/issues/$head_issue/artifacts/$head_image_slug/versions/1 answers the image byte for byte"

# --- document: the markdown document as a subtree of block nodes, and its version node -------------

section=document
issue_artifacts_blocks "$section" "dispatch://$head_issue/artifact/$head_verification_slug" "$head_verification" text
tree_read agent "$head_issue" verification
tree_want 200 "$section" "read $head_issue's verification view"
expect "$section" "the verification query renders the check c-head" 1 '[.nodes[] | select(.block_id == "c-head")] | length'
issue_artifacts_edit "$section" "$head_verification" "$head_sentence" "$head_sentence Its second version says so."
corpus_version "$section" "$head_verification"
head_version="$tree_value"
readonly head_version
issue_artifacts_version_node "$section" "$head_verification_ref" "$head_version"
query "$section" "read the markdown stored for version $head_version" "
  select coalesce(md5(markdown), 'null') from artifact_versions where artifact_id = '$head_verification' and number = $head_version"
stored_md5="$answer"
issue_artifacts_download "$section" "$head_issue" "$head_verification_slug" "$head_version" "$work/head-version.md"
read_md5="$(md5sum <"$work/head-version.md" | cut -d' ' -f1)"
[[ "$stored_md5" == "$read_md5" ]] ||
  fail "$section" "version $head_version's stored markdown is what the version read renders" \
    "artifact_versions.markdown has md5 $stored_md5, the read (Dispatch-Render-Source ${render_source:-none}) $read_md5"
pass "$section" "version $head_version of $head_verification_ref stores the markdown the version read renders (source ${render_source:-none})"

# --- convert: the three role documents of the earlier release's issue become query nodes ----------

section=convert
tree_wait_announcement "$section"
tree_convert "$section"
for row in "spec $old/spec spec" "verification $old_verification_ref verification-md" "plan $old/plan-md plan-md"; do
  read -r name ref slug <<<"$row"
  issue_artifacts_node "$section" artifact "$ref"
  [[ "$node_row" == "query|live" ]] ||
    fail "$section" "$ref reads back as a query node" "its node reads $node_row"
  with_vars "$section" "read what the query node $ref states" "
    select coalesce((select (attributes->>'query') || '|' || (attributes->'takes')::text from graph_nodes
                     where kind = 'artifact' and id = :'r'), '(none)')" "r=$ref"
  [[ "$answer" == "$name|["* ]] ||
    fail "$section" "the query node $ref states its query and the types it takes" "it states $answer"
  pass "$section" "$ref is the query node $name, taking ${answer#*|}"
  tree_send GET "/api/v1/issues/$old/artifacts/$slug"
  tree_want 200 "$section" "read $slug of $old"
  expect "$section" "GET /api/v1/issues/$old/artifacts/$slug answers the role $name" "\"$name\"" .role
  tree_send GET "/api/v1/issues/$old/artifacts"
  tree_want 200 "$section" "list $old's artifacts"
  expect "$section" "GET /api/v1/issues/$old/artifacts answers $slug with the role $name" "[\"$name\"]" \
    --arg s "$slug" '[.[] | select(.slug == $s) | .role]'
done
issue_artifacts_blocks "$section" "dispatch://$old/artifact/verification-md" "$old_verification"
issue_artifacts_resolve "$section" "dispatch://$old/artifact/verification-md#b-c-old"
with_vars "$section" "read where the check c-old hangs" "
  with recursive up(kind, id, depth) as (
    select :'k'::text, :'i'::text, 0
    union all
    select t.to_kind, t.to_id, up.depth + 1 from up
    cross join lateral (select tuple.to_kind, tuple.to_id, tuple.asserted from graph_tuples tuple
      where tuple.from_kind = up.kind and tuple.from_id = up.id and tuple.kind = 'part_of'
      order by tuple.seq desc limit 1) t
    where t.asserted and up.depth < 64)
  select coalesce(bool_or(kind = 'issue' and id = :'o'), false) from up" "k=$resolved_kind" "i=$resolved_id" "o=$old"
[[ "$answer" == t ]] || fail "$section" "the blocks of $old's converted documents hang under $old" "c-old's part_of chain reaches no issue $old"
pass "$section" "the blocks of $old's converted documents hang under $old"
query "$section" "read the role columns of $old's artifacts" \
  "select count(*) from artifacts where issue_key = '$old' and role is not null"
[[ "$answer" == 0 ]] || fail "$section" "no column of $old's artifacts rows holds a role" "$answer rows hold one"
pass "$section" "no column of $old's artifacts rows holds a role"
tree_send GET "/api/v1/issues/$old"
tree_want 200 "$section" "read $old"
expect "$section" "GET /api/v1/issues/$old answers verification_artifact_id and plan_artifact_id with the query nodes' artifacts" \
  "$(jq -cn --arg v "$old_verification" --arg p "$old_plan" '[$v, $p]')" '[.verification_artifact_id, .plan_artifact_id]'
readonly role_census="select count(*) from artifacts a
  where a.role is not null and exists (
    select 1 from document_rooms r join artifacts d on d.id = r.artifact_id where d.issue_key = a.issue_key)"
query "$section" "run the role census of R16" "$role_census"
[[ "$answer" == 0 ]] || fail "$section" "the role census of R16 reads 0" "it reads $answer"
with_vars "$section" "plant a role on $old's Verification document's row, in a transaction rolled back" "
  begin;
  update artifacts set role = 'verification' where id = :'v';
  $role_census;
  rollback;" "v=$old_verification"
[[ "$answer" == 1 ]] || fail "$section" "the role census counts a role planted on one row of $old" "it reads $answer"
pass "$section" "the role census of R16 reads 0, and 1 with a role planted on $old's Verification document"
query "$section" "read artifacts.role in the catalog" \
  "select count(*) from information_schema.columns where table_name = 'artifacts' and column_name = 'role'"
[[ "$answer" == 1 ]] || fail "$section" "no migration of this branch drops artifacts.role" "the catalog holds $answer such column"
pass "$section" "artifacts.role still stands"

# --- rule2: the tick of this head marks a version a build of rule 1 wrote ----------------------------

section=rule2
issue_artifacts_row "$section" "$old_notes"
[[ "$row_owed" == null && "$row_stated_rule" == 2 ]] ||
  fail "$section" "this head's pass has stated $old_notes_ref and cleared its mark" "its row reads $row_line"
issue_artifacts_boot_before
base="$before_url"
issue_artifacts_edit "$section" "$old_notes" "$notes_sentence" "$notes_sentence The release before writes it again."
corpus_version "$section" "$old_notes"
notes_version="$tree_value"
readonly notes_version
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_row "$section" "$old_notes"
  [[ "$row_store_version" != "$notes_version" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the release before indexes version $notes_version of $old_notes_ref" "after ${tree_wait_seconds} s its row reads $row_line"
  sleep 0.2
done
[[ "$row_stated_version" =~ ^[0-9]+$ && "$row_stated_version" -lt "$notes_version" ]] ||
  fail "$section" "before this head's tick the row of $old_notes_ref stands at store_version $notes_version with stated_version behind it" \
    "its row reads $row_line: this head's tick reached it before the read"
pass "$section" "before this head's tick: $row_line"
with_vars "$section" "read the release before's cursor at marking rule 1" "
  select count(*) from block_index_event_cursors where marking_rule = 1 and build_commit <> :'h'" "h=$head_commit"
[[ "$answer" -gt 0 ]] ||
  fail "$section" "the release before keeps a block_index_event_cursors row at marking_rule 1" "no row of a build other than $head_commit at rule 1: the run proves nothing of the widened rule"
pass "$section" "the release before keeps $answer cursor rows at marking_rule 1"
base="$tree_head_url"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_row "$section" "$old_notes"
  [[ "$row_stated_version" != "$notes_version" ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "this head's next tick states version $notes_version of $old_notes_ref" \
      "version $notes_version, store_version $row_store_version, stated_version $row_stated_version after ${tree_wait_seconds} s: $row_line"
  sleep "$tree_poll_seconds"
done
[[ "$row_stated_rule" == 2 && "$row_owed" == null ]] ||
  fail "$section" "this head's tick states version $notes_version at rule 2 and clears its mark" "its row reads $row_line"
issue_artifacts_version_node "$section" "$old_notes_ref" "$notes_version"
issue_artifacts_blocks "$section" "dispatch://$old/artifact/notes-md" "$old_notes" text
issue_artifacts_owed "$section"
[[ "$owed_count" == 0 ]] || fail "$section" "the census owes nothing after the tick" "the owed count reads $owed_count"
pass "$section" "this head's tick stated version $notes_version at rule 2, with no second backfill and no restart: $row_line"
with_vars "$section" "read this head's cursor and the horizon of rule 2" "
  select coalesce((select event_id::text from block_index_event_cursors where build_commit = :'h' and marking_rule = 2), 'none')
         || '|' || coalesce((select event_id::text from graph_backfill_horizon where marking_rule = 2), 'none')
         || '|' || coalesce((select string_agg(event_id::text, ',') from block_index_event_cursors where marking_rule = 1), 'none')" \
  "h=$head_commit"
IFS='|' read -r head_cursor rule2_horizon rule1_cursors <<<"$answer"
[[ "$head_cursor" =~ ^[0-9]+$ && "$rule2_horizon" =~ ^[0-9]+$ && "$head_cursor" -ge "$rule2_horizon" ]] ||
  fail "$section" "this head's cursor stands at marking_rule 2 from the horizon rule 2 recorded" \
    "its cursor reads $head_cursor, the horizon $rule2_horizon, the cursors of rule 1 $rule1_cursors"
pass "$section" "this head's cursor at marking_rule 2 reads $head_cursor, from the horizon $rule2_horizon (the cursors of rule 1: $rule1_cursors)"
issue_artifacts_stop_before
issue_artifacts_edit "$section" "$old_notes" "The release before writes it again." "This head writes it last."
corpus_version "$section" "$old_notes"
last_notes="$tree_value"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_row "$section" "$old_notes"
  [[ "$row_stated_version" != "$last_notes" || "$row_owed" != null ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "a version this head writes to $old_notes_ref holds no mark after the next tick" "its row reads $row_line"
  sleep "$tree_poll_seconds"
done
pass "$section" "version $last_notes, which this head wrote, holds no mark after its tick: $row_line"

# --- retired: a later backfill marks no document rule 2 has stated -----------------------------------

section=retired
printf 'The spec of the issue whose plan the pre-mortem conversion retires.\n' >"$work/last-spec.md"
issue_artifacts_issue "$section" "The issue whose plan is retired in its room" "$work/last-spec.md"
last="$created_key"
readonly last
tree_read agent "$last"
tree_want 200 "$section" "read $last's tree"
expect "$section" "$last is born converted" true .converted
printf 'How the issue is built.\n\n:::plan_task{#t-last}\nWrite the pre-mortem.\n:::\n' >"$work/last-plan.md"
issue_artifacts_upload "$section" "$last" plan.md "$work/last-plan.md"
last_plan="$uploaded_id"
readonly last_plan
query "$section" "read the room $last's plan joined" \
  "select coalesce((select room::text from document_rooms where artifact_id = '$last_plan'), 'none')"
last_room="$answer"
readonly last_room
[[ "$last_room" != none ]] || fail "$section" "$last's plan joins its issue's room" "no document_rooms row names it"
jq -j '.document.markdown' "$fixture" >"$work/premortem.md"
issue_artifacts_upload "$section" "$last" plan.md "$work/premortem.md"
pass "$section" "the pre-mortem of $(basename "$fixture") is the newest version of $last's plan"
tree_send POST "/api/v1/issues/$last/artifacts/plan-md/premortem-convert" \
  "$(jq -c --argjson a "$(tree_actor)" \
    '{prevented_by: [.risks[] | {risk: .index, checks: [], reason: ("No check of this issue prevents risk \(.index).")}], actor: $a}' \
    "$fixture")"
tree_want 200 "$section" "convert $last's plan"
with_vars "$section" "read the retirement of $last's plan" "
  select (select retired_at is not null from artifacts where id = :'p') || '|'
         || coalesce((select room::text from document_rooms where artifact_id = :'p'), 'none') || '|'
         || (select count(*) from artifacts a where a.kind = 'doc'
             and not exists (select 1 from document_rooms r where r.artifact_id = a.id))" "p=$last_plan"
IFS='|' read -r retired kept_room roomless <<<"$answer"
[[ "$retired" == true && "$kept_room" == "$last_room" && "$roomless" -gt 0 ]] ||
  fail "$section" "the conversion retires the plan in its room, beside documents of no room" \
    "retired: $retired, its document_rooms row names $kept_room (it joined $last_room), and $roomless documents name no room: the case proves nothing"
pass "$section" "$last's plan is retired and its document_rooms row still names $last_room; $roomless documents name no room"
query "$section" "read $last's tree artifact" \
  "select coalesce((select artifact_id::text from issue_trees where issue_key = '$last'), 'none')"
last_tree="$answer"
readonly last_tree
[[ "$last_tree" == "$last_room" ]] || fail "$section" "the room of $last is its tree artifact" "issue_trees names $last_tree, the plan's room is $last_room"
with_vars "$section" "mark the retired plan and the room's tree artifact as the backfill of rule 2 leaves them" "
  update document_block_versions
     set store_owed_at = clock_timestamp(), store_owed_rule = 2, stated_version = null, stated_rule = null
   where artifact_id = :'p';
  insert into document_block_versions (artifact_id, store_owed_at, store_owed_rule)
  select :'t'::uuid, clock_timestamp(), 2
   where not exists (select 1 from artifact_versions where artifact_id = :'t')
     and not exists (select 1 from document_block_versions where artifact_id = :'t');
  update document_block_versions
     set store_owed_at = clock_timestamp(), store_owed_rule = 2, stated_version = null, stated_rule = null
   where artifact_id = :'t' and not exists (select 1 from artifact_versions where artifact_id = :'t');
  select string_agg(artifact_id::text, ' ' order by artifact_id) from document_block_versions
   where artifact_id in (:'p', :'t') and store_owed_at is not null" "p=$last_plan" "t=$last_tree"
marked="$answer"
readonly marked
[[ -n "$marked" ]] || fail "$section" "mark the retired plan and the tree artifact" "no row holds a mark"
printf 'marked: %s\n' "$marked"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_artifacts_owed "$section"
  [[ "$owed_count" != 0 ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the room clear of indexOwedRoom clears the marks" "after ${tree_wait_seconds} s the owed count reads $owed_count"
  sleep "$tree_poll_seconds"
done
pass "$section" "the owed count fell to 0 once the room clear ran"
tree_stop_head
issue_artifacts_boot_head "$hold_index"
issue_artifacts_wait_log "$section" "$tree_head_log"
issue_artifacts_owed "$section"
read -r -a marked_rows <<<"$marked"
held_marks=""
for artifact in "${marked_rows[@]}"; do
  issue_artifacts_row "$section" "$artifact"
  [[ "$row_owed" == null && "$row_stated_rule" == 2 ]] || held_marks+=" $artifact ($row_line)"
done
[[ "$owed_count" == 0 && -z "$held_marks" ]] ||
  fail "$section" "the second backfill marks no document rule 2 has stated" \
    "the owed count reads $owed_count;${held_marks:- every row the driver marked holds stated_rule 2}"
pass "$section" "after the second backfill the owed count still reads 0, and each row the driver marked holds stated_rule 2"

# --- census: the versions the tree renders, and a deferred row --------------------------------------

section=census
{
  printf 'How the issue is proved, past the bound.\n\n'
  for ((index = 1; index <= bound_blocks + 1; index++)); do
    printf 'Paragraph %d of the first deferred version.\n\n' "$index"
  done
} >"$work/deferred-1.md"
issue_artifacts_upload "$section" "$last" verification.md "$work/deferred-1.md"
sed 's/of the first deferred version/of the second deferred version/' "$work/deferred-1.md" >"$work/deferred-2.md"
issue_artifacts_upload "$section" "$last" verification.md "$work/deferred-2.md"
pass "$section" "wrote two versions of $last's Verification document past the bound while the block index is held"
tree_stop_head
issue_artifacts_boot_head
tree_wait_backfill "$section"
query "$section" "read the version_renders rows" \
  "select count(*) filter (where at is not null) || '|' || count(*) filter (where at is null) from version_renders"
IFS='|' read -r timed untimed <<<"$answer"
((untimed > 0)) ||
  fail "$section" "a version_renders row holds no time" "$timed rows hold a time and none holds none: the deferred version replaced before its pass holds a time"
query "$section" "list the versions whose row holds a time" "
  select string_agg(coalesce(a.issue_key, '') || ' ' || a.slug || ' ' || v.number || ' ' || coalesce(md5(av.markdown), 'null'), chr(10))
  from version_renders v join artifacts a on a.id = v.artifact_id
  join artifact_versions av on av.artifact_id = v.artifact_id and av.number = v.number
  where v.at is not null and a.issue_key is not null"
printf '%s\n' "$answer" >"$work/timed.txt"
compared=0
while read -r issue slug number stored; do
  [[ -n "$issue" ]] || continue
  issue_artifacts_download "$section" "$issue" "$slug" "$number" "$work/render.md"
  rendered="$(md5sum <"$work/render.md" | cut -d' ' -f1)"
  [[ "$rendered" == "$stored" && "$render_source" == tree ]] ||
    fail "$section" "version $number of $slug on $issue stores the render of the tree as of its row's time" \
      "artifact_versions.markdown has md5 $stored, the version read (Dispatch-Render-Source ${render_source:-none}) $rendered"
  compared=$((compared + 1))
done <"$work/timed.txt"
pass "$section" "each of the $compared versions whose version_renders row holds a time stores the render of the tree as of that time"
tree_census_versions "$section" "the census reads no version whose tree does not render as its stored markdown" \
  "the corpus of $project"
rendered_from_tree="$(tree_census_member rendered_from_tree)"
deferred="$(tree_census_member deferred)"
[[ "$rendered_from_tree" == "$timed" && "$deferred" == "$untimed" ]] ||
  fail "$section" "the census compares the versions whose row holds a time and counts the rows that hold none as deferred" \
    "it reads rendered_from_tree=${rendered_from_tree:-(none)} deferred=${deferred:-(none)}, and version_renders holds $timed rows with a time and $untimed with none"
pass "$section" "the census compares the $timed versions whose row holds a time, and counts $untimed deferred"

printf 'PASS: %s complete 11 sections\n' "$driver"
