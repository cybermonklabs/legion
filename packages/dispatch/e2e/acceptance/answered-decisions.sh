#!/usr/bin/env bash
# dispatch://INFRA-1150's c-answers-restored: the restore returns every answered ask the release
# before let leave its spec, in both shapes, over rows that release wrote, and an agent can no
# longer take an answered ask's block out of a document.
#
# It drops and creates the database DATABASE_URL names (seed_reset_database: a database on this
# machine whose name begins infra_1150_), boots the base release on it through lib-baseline.sh
# (BASELINE_BINARY, default $TMPDIR/base-dispatch, the stamped build of BASELINE_SHA), with the dev
# sign-in, the pinned agent bearer e2e-token and the hold setting (DISPATCH_TEST_HOOKS=1 and
# DISPATCH_TEST_HOLD_DECISIONS_STEP=1, through BASELINE_EXTRA_ENV, which reaches every server it
# boots), and has seed-decisions.sh write the corpus through that server's own API. Through the
# same server it closes issue D, so one issue the restore owes is closed.
#
# Before this head boots it captures, through the base server: each restored ask's id, question,
# options, answer and answerer (GET /api/v1/asks/{id}); the spec blocks the base's Decisions read
# links each ask's section to (`links[].to`); every version of the spec and the decisions document
# of each issue the restore owes (GET /api/v1/artifacts/{id}/versions/{n}); the text the citation
# of the second shape's old addresses answers (GET /api/v1/references?to=), and that of the main
# shape's colliding old spec address; and the block_refs rows the decisions document holds for the
# second shape's ask blocks. It then stops that server and boots this head, HEAD_BINARY (default
# $TMPDIR/bin/envoy-dispatch, built stamped as the Verification preamble builds it), over the same
# database with the hold setting, so the commands below and not the boot make the first move.
#
# It runs `envoy-dispatch backfill-graph-store` (the step's first phase, which the census and the
# restore read an answer from) and the store census (`envoy-dispatch census --store`), which must
# list each spec whose answered asks left it with exactly those asks; then the restore,
# `envoy-dispatch restore-answered-asks --project SEED`, and the census again, which must list
# none. For each restored ask it reads, at this head: the same ask id, its answer and answerer
# character for character as captured; its block in the spec's `## Decisions` part, in answer-time
# order, the pointer line gone; that block's question and options character for character as the
# captured row, and no answer and no server-owned attribute in its markdown (the stored version's);
# for the main shape its old block id and no moved_to tuple naming it, and for the second shape one
# moved_to tuple from its old address to its new one; one current shaped tuple from the ask's node
# to each spec node captured from its section's links, and no other; and no copied_from on its
# block. It reads every captured version of both documents answering the captured bytes. For the
# second shape it reads the two documents move together: the decisions document's block index and
# block_refs hold nothing of the moved asks, and the spec's hold each and each captured edge. For
# both collisions it reads the restored ask's new id, held by no other document of the issue, in
# the asks row asks_block_id_unique keys; for the second shape the old address still resolving
# through its one moved_to tuple to the captured text, and for the main shape no moved_to tuple and
# the old spec address answering the live block that holds that id, as captured. A text is held to
# the captured one character for character with two differences and no other, each printed: the
# eight server-owned ask attributes are absent, and a block that took a new id carries it. It runs
# the restore again and reads the same result for the closed issue, and for every other.
#
# Last, on the spec, which still holds the answered ask q-kept, it makes three writes as the agent
# - a delete of that block, a whole-document replacement without it, and a retype of it - and reads
# each refused 409 ANSWERED_ASK_KEPT, quoting the ask's question and naming its node and its block,
# the document's version unchanged and the answer still in place; then three such writes as the
# person, each dropping or retyping an answered ask's block, which are taken.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. Every check is `cmd || fail …`, never part of an `&&` list.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_answered_decisions?sslmode=disable \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [HEAD_BINARY=$TMPDIR/bin/envoy-dispatch] \
         bash e2e/acceptance/answered-decisions.sh
       (run from packages/dispatch; DATABASE_URL names a database on this machine whose name
       begins infra_1150_, which this run drops and creates again)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=seed-decisions.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-decisions.sh"

require_env DATABASE_URL TMPDIR
require_commands cmp curl jq psql sed sort
# shellcheck disable=SC2031 # the run's own DATABASE_URL: only a server's subshell sets another
database_url="$DATABASE_URL"
if [[ -z "${BASELINE_BINARY:-}" ]]; then
  # shellcheck disable=SC2031 # this shell's own TMPDIR: only a server's subshell sets another
  BASELINE_BINARY="$TMPDIR/base-dispatch"
fi
# shellcheck disable=SC2031 # this shell's own TMPDIR
head_binary="${HEAD_BINARY:-$TMPDIR/bin/envoy-dispatch}"
[[ -f "$head_binary" && -x "$head_binary" ]] || {
  printf 'HEAD_BINARY %s is not an executable file: build it as the Verification preamble does\n' "$head_binary" >&2
  exit 2
}
readonly head_binary
readonly project=SEED
# The eight server-owned ask attributes R10 takes out of markdown.
readonly -a server_attributes=(state answered_by answered_at selected answer invalid copied_from copied_from_document)

work_dir
head_pid=""
cleanup() {
  local code=$?
  baseline_stop_pid "$head_pid"
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$work/capture" "$work/home"

# ---------------------------------------------------------------------------------------------
# The helpers.
# ---------------------------------------------------------------------------------------------
# send AS METHOD PATH [JSON]: METHOD PATH to $server as AS, `agent` (the bearer, in $work/auth) or
# `person` (the dev sign-in's cookie in $work/jar and the server's origin, which a cookie's write
# must name). The answer's body goes to $work/body and its status to $status.
send() {
  local -a auth data=()
  case "$1" in
    agent) auth=(-H "@$work/auth") ;;
    person) auth=(-b "$work/jar" -H "Origin: $server") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$server$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# field SECTION WHAT JQ: one value of the last answer's body, which must be there, in $value.
field() {
  value="$(jq -er "$3" "$work/body" 2>/dev/null)" || fail "$1" "$2" "no $3 in: $(body)"
}

# sign_in SECTION: signs the person in at $server, the cookie in $work/jar.
sign_in() {
  status="$(curl -sS -o "$work/body" -w '%{http_code}' -c "$work/jar" \
    "$server/auth/_dev/signin?login=$seed_person" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  [[ "$status" =~ ^[23][0-9][0-9]$ ]] || fail "$1" "sign $seed_person in at $server" "HTTP $status: $(body)"
  grep -q $'\t' "$work/jar" || fail "$1" "sign $seed_person in at $server" "the dev sign-in set no cookie"
}

# dispatch_command NAME ARGS…: this head's `envoy-dispatch ARGS` over the database, in an
# environment holding only PATH, a home and a TMPDIR of this run's and DATABASE_URL, its output in
# $work/NAME.out and its exit in $command_code.
dispatch_command() {
  local name="$1"
  shift
  command_code=0
  # shellcheck disable=SC2031 # this shell's own PATH: only a server's subshell sets another
  env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work" DATABASE_URL="$database_url" \
    "$head_binary" "$@" >"$work/$name.out" 2>&1 || command_code=$?
}

# census SECTION NAME: `envoy-dispatch census --store --verbose`, which prints its lines whatever it
# refuses, into $work/NAME.out; printed with the run's own output.
census() {
  dispatch_command "$2" census --store --verbose
  [[ "$command_code" -le 1 ]] || fail "$1" "run the store census" "exited $command_code: $(<"$work/$2.out")"
  grep -E '^census: (decisions|spec_with_moved_answers|dangling_citations|collisions|store-only|store rows_checked)' "$work/$2.out" |
    sed 's/^/  /' || true
}

# wait_settled SECTION: waits, within two minutes, until no document owes a settlement or its block
# index, so a read that follows reads every write before it.
wait_settled() {
  local waited=0
  while true; do
    query "$1" "read what the documents owe" \
      "select (select count(*) from doc_settlements_pending) + (select count(*) from document_block_versions where version is null)"
    [[ "$answer" != 0 ]] || return 0
    [[ "$waited" -lt 1200 ]] || fail "$1" "every document settles" "$answer settlements and block indexes still owed after 120 s"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# latest_markdown SECTION DOCUMENT: the newest stored version of DOCUMENT, its number in $version
# and its markdown in $work/latest.md, read through the versions route.
latest_markdown() {
  send agent GET "/api/v1/artifacts/$2"
  want_status 200 "$1" "read document $2"
  field "$1" "the newest version of $2" '[.versions[].number] | max'
  version="$value"
  send agent GET "/api/v1/artifacts/$2/versions/$version"
  want_status 200 "$1" "read version $version of $2"
  jq -j '.markdown' "$work/body" >"$work/latest.md"
}

# ask_block ID FILE: the markdown of the ask block ID in FILE, its fence and attributes included,
# on stdout; nothing where FILE holds no such block.
ask_block() {
  awk -v id="$1" '
    !open && match($0, /^:+ask\{#/) {
      rest = substr($0, RLENGTH + 1)
      if (index(rest, id " ") == 1 || index(rest, id "}") == 1) {
        open = 1; fence = substr($0, 1, index($0, "ask{") - 1); print; next
      }
    }
    open { print; if ($0 == fence) exit }
  ' "$2"
}

# block_body FILE: the lines between the first line and the closing fence of the block in FILE,
# nested blocks left out.
block_body() {
  awk 'NR == 1 { next } { lines[NR] = $0; last = NR } END {
    depth = 0
    for (n = 2; n < last; n++) {
      line = lines[n]
      if (line ~ /^:::+[a-z]+\{/) { depth++; continue }
      if (depth > 0) { if (line ~ /^:::+$/) depth--; continue }
      print line
    }
  }' "$1"
}

# expected_body ASK_FILE: the question and options of the captured ask row ASK_FILE as a block
# body: the question, and where it has options, a blank line and one `- label: description` line
# each.
expected_body() {
  jq -r '.question, (if (.options // []) | length > 0 then "", (.options[] | "- " + .label + (if (.description // "") == "" then "" else ": " + .description end)) else empty end)' "$1"
}

# same_text SECTION WHAT CAPTURED GOT OLD_ID NEW_ID: GOT, the text a citation resolves to now,
# equals CAPTURED, the text it resolved to before the upgrade, character for character but for two
# differences, each printed: the eight server-owned ask attributes are absent from GOT, and where
# NEW_ID is not OLD_ID the block carries NEW_ID in place of OLD_ID. A server-owned attribute still
# in GOT fails, as does any other difference.
same_text() {
  local section="$1" what="$2" captured="$3" got="$4" old="$5" new="$6" name opening
  printf '%s: the captured text:\n%s\n%s: the text now:\n%s\n' "$what" "$(<"$captured")" "$what" "$(<"$got")"
  opening="$(head -n 1 "$got")"
  for name in "${server_attributes[@]}"; do
    [[ "$opening" != *" $name=\""* ]] ||
      fail "$section" "$what" "the server-owned attribute $name is still in the text: $opening"
  done
  cp "$captured" "$work/expected.txt"
  opening="$(head -n 1 "$captured")"
  for name in "${server_attributes[@]}"; do
    if [[ "$opening" =~ \ $name=\"[^\"]*\" ]]; then
      printf '%s: allowed difference: the server-owned attribute %s is absent\n' "$what" "${BASH_REMATCH[0]# }"
      sed -i -E "1s/ $name=\"[^\"]*\"//" "$work/expected.txt"
    fi
  done
  if [[ "$old" != "$new" ]]; then
    printf '%s: allowed difference: the block took the id %s in place of %s\n' "$what" "$new" "$old"
    sed -i "1s/{#$old /{#$new /; 1s/{#$old}/{#$new}/" "$work/expected.txt"
  fi
  cmp -s "$work/expected.txt" "$got" ||
    fail "$section" "$what" "the text differs beyond those two: want $(<"$work/expected.txt"), got $(<"$got")"
  pass "$section" "$what"
}

# moved_to_rows ADDRESS: every moved_to tuple from the block node ADDRESS, one `to_id|asserted`
# line each, oldest first, into $work/moved.
moved_to_rows() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c \
    "select to_id, asserted from graph_tuples where kind = 'moved_to' and from_kind = 'block' and from_id = '$1' order by seq" \
    >"$work/moved" 2>"$work/psql" || fail moved_to "read the moved_to tuples of $1" "psql: $(<"$work/psql")"
}

# moved_to_naming ADDRESS: how many moved_to tuples name the block node ADDRESS at either end, in
# $answer.
moved_to_naming() {
  query moved_to "read the moved_to tuples naming $1" \
    "select count(*) from graph_tuples where kind = 'moved_to' and ((from_kind = 'block' and from_id = '$1') or (to_kind = 'block' and to_id = '$1'))"
}

# shaped_of ASK: the spec nodes the current shaped tuples of ASK's node reach, one a line, sorted.
shaped_of() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -c "
    select s.to_id from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
      where t.from_kind = 'ask' and t.from_id = '$1' and t.kind = 'shaped' and t.to_kind <> 'value'
      order by t.to_kind, t.to_id, t.seq desc
    ) s where s.asserted order by s.to_id" 2>"$work/psql" ||
    fail shaped "read the shaped tuples of ask $1" "psql: $(<"$work/psql")"
}

# ---------------------------------------------------------------------------------------------
# The base release writes the corpus.
# ---------------------------------------------------------------------------------------------
seed_reset_database "$database_url"
# The pinned agent bearer the e2e harness gives its own server, and the hold setting, which reach
# every server lib-baseline.sh boots: the base reads neither hook, and this head holds its step.
DISPATCH_TOKEN=e2e-token
BASELINE_EXTRA_ENV=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_DECISIONS_STEP=1'
baseline_start
unset DISPATCH_TOKEN DATABASE_URL BASELINE_EXTRA_ENV
# database_url stays writable: lib-baseline.sh's baseline_boot, which boots this head, holds a
# local of that name.
seed_decisions
printf 'Authorization: Bearer %s\n' "$baseline_token" >"$work/auth"
server="$baseline_url"

readonly issue_a="${seed_ids[issue_a]}" issue_b="${seed_ids[issue_b]}" issue_d="${seed_ids[issue_d]}"
readonly spec_a="${seed_ids[spec_a]}" spec_b="${seed_ids[spec_b]}" spec_d="${seed_ids[spec_d]}"
readonly decisions_a="${seed_ids[decisions_a]}" decisions_b="${seed_ids[decisions_b]}"
# The asks the restore owes: their shape, issue, spec and old block id.
readonly -a restored=(ask_left_store ask_left_format ask_doc_plain ask_doc_collide ask_b_left ask_d_approved)
declare -A shape_of=([ask_left_store]=main [ask_left_format]=main [ask_doc_plain]=second
  [ask_doc_collide]=second [ask_b_left]=main [ask_d_approved]=main)
declare -A issue_of=([ask_left_store]="$issue_a" [ask_left_format]="$issue_a" [ask_doc_plain]="$issue_a"
  [ask_doc_collide]="$issue_a" [ask_b_left]="$issue_b" [ask_d_approved]="$issue_d")
declare -A spec_of=([ask_left_store]="$spec_a" [ask_left_format]="$spec_a" [ask_doc_plain]="$spec_a"
  [ask_doc_collide]="$spec_a" [ask_b_left]="$spec_b" [ask_d_approved]="$spec_d")
declare -A old_id=([ask_left_store]=q-left-store [ask_left_format]=q-left-format [ask_doc_plain]=q-doc-plain
  [ask_doc_collide]=q-doc-collide [ask_b_left]=q-b-left [ask_d_approved]=q-d-approved)
# The headings of the sections that hold the second shape's ask blocks.
declare -A section_heading=([ask_doc_plain]="Which reader reads the corpus first"
  [ask_doc_collide]="Which line names a moved block")
# The two collisions: a live block of the spec holds the old block id.
declare -A collides=([ask_left_store]=1 [ask_doc_collide]=1)

# The closed issue: D, whose answered ask left its spec.
send agent PATCH "/api/v1/issues/$issue_d" "$(jq -cn --argjson a "$(seed_actor)" '{status: "done", actor: $a}')"
want_status 200 capture "close issue $issue_d through the base release"
expect capture "issue $issue_d is closed" '"done"' '.status'

send agent GET /healthz
want_status 200 capture "read the base release's /healthz"
base_commit="$(jq -r '.commit // ""' "$work/body")"
readonly base_commit
printf 'base release commit: %s\n' "${base_commit:-<none>}"

# Each restored ask, and q-kept, as the base release's ask read answers them.
for key in "${restored[@]}" ask_kept; do
  send agent GET "/api/v1/asks/${seed_ids[$key]}"
  want_status 200 capture "read $key through the base release"
  jq '.ask' "$work/body" >"$work/capture/ask.$key.json"
  jq -e '.state == "answered" and (.answer.text | type == "string") and (.answer.user | type == "string")' \
    "$work/capture/ask.$key.json" >/dev/null || fail capture "$key is answered" "$(body)"
done
pass capture "captured the id, question, options, answer and answerer of ${#restored[@]} restored asks"

# The spec blocks the base's Decisions read links each ask's section to.
for issue in "$issue_a" "$issue_b"; do
  send agent GET "/api/v1/issues/$issue/decisions"
  want_status 200 capture "read the Decisions of $issue through the base release"
  cp "$work/body" "$work/capture/decisions.$issue.json"
done
for key in "${restored[@]}"; do
  file="$work/capture/decisions.${issue_of[$key]}.json"
  [[ -f "$file" ]] || {
    : >"$work/capture/links.$key"
    continue
  }
  if [[ -n "${section_heading[$key]:-}" ]]; then
    jq -r --arg h "${section_heading[$key]}" '.decisions[] | select(.heading == $h) | .links[].to.id' "$file"
  else
    jq -r --arg a "${seed_ids[$key]}" '.decisions[] | select(.asks | index($a)) | .links[].to.id' "$file"
  fi | LC_ALL=C sort -u >"$work/capture/links.$key"
  printf 'captured links of %s: %s\n' "$key" "$(tr '\n' ' ' <"$work/capture/links.$key")"
done
[[ -s "$work/capture/links.ask_left_store" ]] ||
  fail capture "the section restating ask_left_store links spec blocks" "the base read links none"

# Every version of the spec and the decisions document of each issue the restore owes.
versions=0
for document in "$spec_a" "$decisions_a" "$spec_b" "$decisions_b" "$spec_d"; do
  send agent GET "/api/v1/artifacts/$document"
  want_status 200 capture "read document $document"
  for number in $(jq -r '.versions[].number' "$work/body"); do
    send agent GET "/api/v1/artifacts/$document/versions/$number"
    want_status 200 capture "read version $number of $document"
    jq -j '.markdown' "$work/body" >"$work/capture/version.$document.$number.md"
    versions=$((versions + 1))
  done
done
pass capture "captured $versions versions of the five documents"

# What the old addresses of the colliding blocks answer: the second shape's block in the
# decisions document, and the callout of the spec that holds the main shape's old id.
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/artifact/decisions-md#b-q-doc-collide")"
want_status 200 capture "resolve decisions-md#b-q-doc-collide through the base release"
jq -j '.node.text' "$work/body" >"$work/capture/text.q-doc-collide"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/spec#b-q-left-store")"
want_status 200 capture "resolve spec#b-q-left-store through the base release"
expect capture "spec#b-q-left-store is the callout that took that id" '"callout"' '.node.text | capture("^:::(?<t>[a-z]+)").t'
jq -j '.node.text' "$work/body" >"$work/capture/text.q-left-store"

# The block_refs rows the decisions document holds for the second shape's ask blocks.
for key in ask_doc_plain ask_doc_collide; do
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c "
    select kind, to_kind, to_id from block_refs
    where artifact_id = '$decisions_a' and block_id = '${old_id[$key]}' order by 1, 2, 3" \
    >"$work/capture/edges.$key" 2>"$work/psql" || fail capture "read the edges of $key" "psql: $(<"$work/psql")"
  printf 'captured edges of %s in the decisions document: %s\n' "$key" "$(wc -l <"$work/capture/edges.$key")"
done
baseline_stop_server

# ---------------------------------------------------------------------------------------------
# This head, held, over the same database.
# ---------------------------------------------------------------------------------------------
baseline_boot head "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
send agent GET /healthz
want_status 200 head "read this head's /healthz"
head_commit="$(jq -r '.commit // ""' "$work/body")"
[[ -n "$head_commit" && "$head_commit" != "$base_commit" ]] ||
  fail head "this head is stamped with a commit of its own" "it serves '$head_commit', the base '$base_commit'"
pass head "this head serves $head_commit over the base release's database, the decisions step held"
sign_in head

# The step's first phase, which states each answer on its ask's node.
dispatch_command backfill backfill-graph-store
[[ "$command_code" == 0 ]] || fail census "envoy-dispatch backfill-graph-store" "exited $command_code: $(<"$work/backfill.out")"
pass census "envoy-dispatch backfill-graph-store stated the rows the release before wrote"

# The census of specs whose answered asks were moved out lists each with exactly its asks.
census census census-before
declare -A owed=()
for key in "${restored[@]}"; do
  owed["${issue_of[$key]}"]+="${seed_ids[$key]} "
done
grep -q "^census: decisions documents=[0-9]* unconverted_sections=[0-9]* specs_with_moved_answers=${#owed[@]}\$" "$work/census-before.out" ||
  fail census "the census lists ${#owed[@]} specs whose answered asks left them" "$(grep '^census: decisions' "$work/census-before.out")"
for issue in "${!owed[@]}"; do
  want="$(tr ' ' '\n' <<<"${owed[$issue]}" | sed '/^$/d' | sort | tr '\n' ',')"
  line="$(grep "^census: spec_with_moved_answers issue=$issue asks=" "$work/census-before.out" || true)"
  got="$(tr ',' '\n' <<<"${line##*asks=}" | sed '/^$/d' | sort | tr '\n' ',')"
  [[ -n "$line" && "$got" == "$want" ]] ||
    fail census "the census lists $issue with the asks that left its spec" "got '${line:-no line}', want asks ${want%,}"
  pass census "the census lists $issue with asks ${want%,}"
done

# The restore, through its entry point.
dispatch_command restore restore-answered-asks --project "$project"
grep '^restore-answered-asks: ' "$work/restore.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail restore "envoy-dispatch restore-answered-asks --project $project" "exited $command_code: $(<"$work/restore.out")"
for issue in "${!owed[@]}"; do
  count="$(tr ' ' '\n' <<<"${owed[$issue]}" | sed '/^$/d' | wc -l)"
  grep -q "^restore-answered-asks: issue=$issue restored=$count " "$work/restore.out" ||
    fail restore "the restore returns $count asks to the spec of $issue" "$(grep "issue=$issue " "$work/restore.out" || echo 'no line')"
done
pass restore "envoy-dispatch restore-answered-asks --project $project restored every ask the census listed"
wait_settled restore

census census census-after
grep -q '^census: decisions documents=[0-9]* unconverted_sections=[0-9]* specs_with_moved_answers=0$' "$work/census-after.out" ||
  fail census "the census prints zero once the restore has run" "$(grep '^census: decisions' "$work/census-after.out")"
! grep -q '^census: spec_with_moved_answers ' "$work/census-after.out" ||
  fail census "the census lists no spec once the restore has run" "$(grep '^census: spec_with_moved_answers ' "$work/census-after.out")"
pass census "the census prints specs_with_moved_answers=0 once the restore has run"

# ---------------------------------------------------------------------------------------------
# Each restored ask.
# ---------------------------------------------------------------------------------------------
declare -A new_id=()
for key in "${restored[@]}"; do
  captured="$work/capture/ask.$key.json"
  send agent GET "/api/v1/asks/${seed_ids[$key]}"
  want_status 200 answer "read $key at this head"
  expect answer "$key keeps its id" "\"${seed_ids[$key]}\"" '.ask.id'
  expect answer "$key keeps its answer character for character" "$(jq -cS '.answer.text' "$captured")" '.ask.answer.text'
  expect answer "$key keeps its answerer" "$(jq -cS '.answer.user' "$captured")" '.ask.answer.user'
  expect answer "$key names the spec as its block's document" "\"${spec_of[$key]}\"" '.ask.block_artifact.id'
  field answer "the block id of $key" '.ask.block_id'
  new_id["$key"]="$value"
  if [[ -n "${collides[$key]:-}" ]]; then
    [[ "${new_id[$key]}" != "${old_id[$key]}" ]] ||
      fail collision "$key takes a new block id, since the spec holds ${old_id[$key]}" "it kept ${old_id[$key]}"
  else
    [[ "${new_id[$key]}" == "${old_id[$key]}" ]] ||
      fail restore "$key takes its old block id ${old_id[$key]}" "it took ${new_id[$key]}"
  fi
done

for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown main "$spec"
  cp "$work/latest.md" "$work/spec.$spec.md"
  ! grep -Fq "The decisions this spec rests on are in dispatch://" "$work/latest.md" ||
    fail main "the restored asks of $spec stand in place of the pointer line" "the pointer line is still there"
  [[ "$(grep -c '^## Decisions$' "$work/latest.md")" == 1 ]] ||
    fail main "$spec holds one Decisions part" "$(grep -c '^## Decisions$' "$work/latest.md") such headings"
  # The asks of this spec, in answer-time order, and the order the Decisions part holds them in.
  want_order=""
  while read -r _ key; do
    want_order+="${new_id[$key]} "
  done < <(for key in "${restored[@]}"; do
    if [[ "${spec_of[$key]}" == "$spec" ]]; then
      printf '%s %s\n' "$(jq -r '.answer.at' "$work/capture/ask.$key.json")" "$key"
    fi
  done | sort)
  got_order="$(sed -n '/^## Decisions$/,/^## /p' "$work/latest.md" | sed -nE 's/^:+ask\{#([^ }]+).*/\1/p' | tr '\n' ' ')"
  [[ "$got_order" == "$want_order" ]] ||
    fail main "the Decisions part of $spec holds the restored asks in answer-time order" "got $got_order, want $want_order"
  pass main "the Decisions part of $spec holds ${want_order% } in answer-time order, in place of the pointer line"
done

for key in "${restored[@]}"; do
  spec="${spec_of[$key]}"
  ask_block "${new_id[$key]}" "$work/spec.$spec.md" >"$work/block.md"
  [[ -s "$work/block.md" ]] || fail main "the spec holds the block of $key" "no ask block ${new_id[$key]} in $spec"
  opening="$(head -n 1 "$work/block.md")"
  for name in "${server_attributes[@]}"; do
    [[ "$opening" != *" $name=\""* ]] ||
      fail main "the block of $key holds no server-owned attribute" "it holds $name: $opening"
  done
  answer_text="$(jq -r '.answer.text' "$work/capture/ask.$key.json")"
  ! grep -Fq -- "$answer_text" "$work/block.md" || fail main "the block of $key holds no answer" "$(<"$work/block.md")"
  block_body "$work/block.md" >"$work/body.got"
  expected_body "$work/capture/ask.$key.json" >"$work/body.want"
  cmp -s "$work/body.got" "$work/body.want" ||
    fail main "the block of $key holds the question and options of its row character for character" \
      "want $(<"$work/body.want"), got $(<"$work/body.got")"
  pass main "the block of $key holds its row's question and options and no answer: $opening"

  spec_ref="${issue_of[$key]}/spec"
  if [[ "${shape_of[$key]}" == main ]]; then
    moved_to_naming "$spec_ref#${new_id[$key]}"
    [[ "$answer" == 0 ]] || fail moved_to "no moved_to tuple names the block of $key" "$answer do"
    moved_to_naming "$spec_ref#${old_id[$key]}"
    [[ "$answer" == 0 ]] || fail moved_to "no moved_to tuple names the old address of $key" "$answer do"
    pass moved_to "no moved_to tuple names $key, which returned under the spec's own address"
  else
    moved_to_rows "${issue_of[$key]}/decisions-md#${old_id[$key]}"
    [[ "$(<"$work/moved")" == "$spec_ref#${new_id[$key]}|t" ]] ||
      fail moved_to "one moved_to tuple from the old address of $key to its new one" "$(tr '\n' ' ' <"$work/moved")"
    pass moved_to "one moved_to tuple from ${issue_of[$key]}/decisions-md#${old_id[$key]} to $spec_ref#${new_id[$key]}"
  fi

  shaped_of "${seed_ids[$key]}" | LC_ALL=C sort >"$work/shaped"
  cmp -s "$work/shaped" "$work/capture/links.$key" ||
    fail shaped "one shaped tuple from $key to each spec node its section linked" \
      "got $(tr '\n' ' ' <"$work/shaped"), want $(tr '\n' ' ' <"$work/capture/links.$key")"
  pass shaped "$key shapes exactly the spec nodes its section linked: $(tr '\n' ' ' <"$work/shaped")"

  query copy "read the copied_from tuples of the block of $key" \
    "select count(*) from graph_tuples where kind = 'copied_from' and from_kind = 'block' and from_id = '$spec_ref#${new_id[$key]}'"
  [[ "$answer" == 0 ]] || fail copy "no mark of a copy on the restored block of $key" "$answer copied_from tuples"
  pass copy "the restored block of $key carries no mark of a copy"
done

# Every version captured before the upgrade answers the same bytes.
for captured in "$work"/capture/version.*.md; do
  name="${captured##*/version.}"
  document="${name%%.*}"
  number="${name#*.}"
  number="${number%.md}"
  send agent GET "/api/v1/artifacts/$document/versions/$number"
  want_status 200 history "read version $number of $document"
  jq -j '.markdown' "$work/body" >"$work/version.md"
  cmp -s "$captured" "$work/version.md" ||
    fail history "version $number of $document answers the bytes captured before the upgrade" "it differs"
done
pass history "every one of the $versions captured versions answers the bytes captured before the upgrade"

# The two documents of the second shape move together.
for key in ask_doc_plain ask_doc_collide; do
  query together "read the decisions document's index of $key" \
    "select count(*) from document_blocks where artifact_id = '$decisions_a' and block_id = '${old_id[$key]}'"
  [[ "$answer" == 0 ]] || fail together "the decisions document no longer holds the block of $key" "its index holds $answer"
  query together "read the spec's index of $key" \
    "select count(*) from document_blocks where artifact_id = '$spec_a' and block_id = '${new_id[$key]}' and type = 'ask'"
  [[ "$answer" == 1 ]] || fail together "the spec holds the block of $key" "its index holds $answer"
  query together "read the decisions document's edges of $key" \
    "select count(*) from block_refs where artifact_id = '$decisions_a' and block_id = '${old_id[$key]}'"
  [[ "$answer" == 0 ]] || fail together "no edge of $key is left under the decisions document" "$answer are"
  while IFS='|' read -r kind to_kind to_id; do
    query together "read the spec's edge $kind of $key" \
      "select count(*) from block_refs where artifact_id = '$spec_a' and block_id = '${new_id[$key]}' and kind = '$kind' and to_kind = '$to_kind' and to_id = '$to_id'"
    [[ "$answer" == 1 ]] || fail together "the spec holds the edge $kind -> $to_id of $key" "it holds $answer"
  done <"$work/capture/edges.$key"
  pass together "$key moved with its $(wc -l <"$work/capture/edges.$key") captured edges: the spec's index and block_refs hold it, the decisions document's hold nothing of it"
done

# ---------------------------------------------------------------------------------------------
# The two collisions.
# ---------------------------------------------------------------------------------------------
query collision "read that asks_block_id_unique stands" \
  "select count(*) from pg_indexes where indexname = 'asks_block_id_unique'"
[[ "$answer" == 1 ]] || fail collision "asks_block_id_unique stands" "pg_indexes holds $answer such index"
for key in ask_left_store ask_doc_collide; do
  query collision "read the asks row of $key" "select block_id from asks where id = '${seed_ids[$key]}'"
  [[ "$answer" == "${new_id[$key]}" ]] ||
    fail collision "asks_block_id_unique took the row of $key with its new id" "the row holds $answer"
  query collision "read which documents of $issue_a hold ${new_id[$key]}" \
    "select string_agg(a.ref_key, ' ') from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = '$issue_a' and b.block_id = '${new_id[$key]}'"
  [[ "$answer" == "$issue_a/spec" ]] ||
    fail collision "no other document of $issue_a holds the new id of $key" "held by: $answer"
  pass collision "$key took ${new_id[$key]}, which no other document of $issue_a holds, and its asks row reads it"
done
# The second shape: the old address still resolves through its one moved_to tuple.
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/artifact/decisions-md#b-q-doc-collide")"
want_status 200 collision "resolve decisions-md#b-q-doc-collide"
expect collision "decisions-md#b-q-doc-collide resolves to the restored block" "\"$issue_a/spec#${new_id[ask_doc_collide]}\"" '.node.id'
jq -j '.node.text' "$work/body" >"$work/text.got"
same_text collision "decisions-md#b-q-doc-collide answers the captured text" "$work/capture/text.q-doc-collide" \
  "$work/text.got" q-doc-collide "${new_id[ask_doc_collide]}"
# The main shape: no moved_to, and the old address answers the live block that holds that id.
moved_to_naming "$issue_a/spec#q-left-store"
[[ "$answer" == 0 ]] || fail collision "no moved_to tuple names $issue_a/spec#q-left-store" "$answer do"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/spec#b-q-left-store")"
want_status 200 collision "resolve spec#b-q-left-store"
expect collision "spec#b-q-left-store answers the live block that holds that id" "\"$issue_a/spec#q-left-store\"" '.node.id'
jq -j '.node.text' "$work/body" >"$work/text.got"
cmp -s "$work/capture/text.q-left-store" "$work/text.got" ||
  fail collision "spec#b-q-left-store answers the live callout as captured" "got $(<"$work/text.got")"
pass collision "spec#b-q-left-store answers the live callout that holds that id, character for character: $(<"$work/text.got")"

# ---------------------------------------------------------------------------------------------
# The closed issue, and the restore run again.
# ---------------------------------------------------------------------------------------------
send agent GET "/api/v1/issues/$issue_d"
want_status 200 closed "read issue $issue_d"
expect closed "issue $issue_d is still closed" '"done"' '.status'
pass closed "the restore returned ask_d_approved to the spec of the closed issue $issue_d, as for the open ones"
declare -A version_after=()
for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown closed "$spec"
  version_after["$spec"]="$version"
done
dispatch_command restore-again restore-answered-asks --project "$project"
[[ "$command_code" == 0 ]] || fail closed "run the restore again" "exited $command_code: $(<"$work/restore-again.out")"
! grep -E '^restore-answered-asks: issue=.* restored=[1-9]' "$work/restore-again.out" ||
  fail closed "the restore run again restores nothing more" "$(grep '^restore-answered-asks: ' "$work/restore-again.out")"
wait_settled closed
for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown closed "$spec"
  [[ "$version" == "${version_after[$spec]}" ]] ||
    fail closed "the restore run again writes no version of $spec" "it is at $version, was ${version_after[$spec]}"
  cmp -s "$work/latest.md" "$work/spec.$spec.md" || fail closed "the restore run again leaves $spec as it was" "it differs"
done
send agent GET "/api/v1/asks/${seed_ids[ask_d_approved]}"
want_status 200 closed "read ask_d_approved again"
expect closed "ask_d_approved of the closed issue reads the same answer" \
  "$(jq -cS '.answer.text' "$work/capture/ask.ask_d_approved.json")" '.ask.answer.text'
expect closed "ask_d_approved of the closed issue reads the same block" "\"q-d-approved\"" '.ask.block_id'
pass closed "the restore run again reads the same result, for the closed issue $issue_d and every other"

# ---------------------------------------------------------------------------------------------
# An agent cannot drop an answered ask's block; the person can.
# ---------------------------------------------------------------------------------------------
readonly kept_ask="${seed_ids[ask_kept]}"
kept_question="$(jq -r '.question' "$work/capture/ask.ask_kept.json")"
readonly kept_question
send agent GET "/api/v1/artifacts/$spec_a"
want_status 200 refusal "read the spec of $issue_a"
field refusal "the spec's name" '.name'
readonly spec_name="$value"

# refused WHAT: the last answer is the refusal of a write dropping or retyping q-kept, quoting the
# ask's question and naming its node and its block; the spec's version is unchanged and its
# answer still in place.
refused() {
  want_status 409 refusal "$1"
  expect refusal "$1 is ANSWERED_ASK_KEPT" '"ANSWERED_ASK_KEPT"' '.code'
  field refusal "$1 says why" '.error'
  message="$value"
  [[ "$message" == *"dispatch://$issue_a/ask/$kept_ask"* && "$message" == *"\"$kept_question\""* &&
    "$message" == *"dispatch://$issue_a/spec#b-q-kept"* ]] ||
    fail refusal "$1 quotes the ask and names its node and block" "$message"
  printf 'refusal: %s\n' "$message"
  latest_markdown refusal "$spec_a"
  [[ "$version" == "$kept_version" ]] || fail refusal "$1 writes no version" "the spec is at $version, was $kept_version"
  ask_block q-kept "$work/latest.md" >"$work/kept.md"
  [[ -s "$work/kept.md" ]] || fail refusal "$1 leaves q-kept in the spec" "the newest version holds no q-kept"
  send agent GET "/api/v1/asks/$kept_ask"
  want_status 200 refusal "read q-kept's ask"
  expect refusal "$1 leaves the answer in place" "$(jq -cS '.answer.text' "$work/capture/ask.ask_kept.json")" '.ask.answer.text'
  pass refusal "$1 is refused, quoting the ask and naming its node and block; the version and the answer stand"
}

latest_markdown refusal "$spec_a"
kept_version="$version"
cp "$work/latest.md" "$work/spec-before-refusals.md"
send agent POST "/api/v1/artifacts/$spec_a/edits" \
  "$(jq -cn --argjson a "$(seed_actor)" '{ops: [{op: "delete", block: "q-kept"}], actor: $a}')"
refused "an agent's delete of q-kept"
# without_block ID FILE: FILE without the ask block ID and the blank line after it.
without_block() {
  awk -v id="$1" '
    skip == 2 && $0 == "" { skip = 0; next }
    skip == 2 { skip = 0 }
    !skip && match($0, /^:+ask\{#/) {
      rest = substr($0, RLENGTH + 1)
      if (index(rest, id " ") == 1 || index(rest, id "}") == 1) { skip = 1; fence = substr($0, 1, index($0, "ask{") - 1); next }
    }
    skip == 1 { if ($0 == fence) skip = 2; next }
    { print }
  ' "$2"
}
without_block q-kept "$work/spec-before-refusals.md" >"$work/replacement.md"
send agent POST "/api/v1/issues/$issue_a/artifacts" \
  "$(jq -cn --arg n "$spec_name" --rawfile c "$work/replacement.md" --argjson a "$(seed_actor)" '{name: $n, content: $c, actor: $a}')"
refused "an agent's whole-document replacement without q-kept"
send agent POST "/api/v1/artifacts/$spec_a/edits" \
  "$(jq -cn --argjson a "$(seed_actor)" '{ops: [{op: "retype", block: "q-kept", type: "callout", attributes: {kind: "note", title: ""}}], actor: $a}')"
refused "an agent's retype of q-kept"
[[ "$message" == *retype* ]] || fail refusal "the retype's refusal says it retypes the block" "$message"

# The same three writes as the person, each dropping or retyping the block of an answered ask.
# taken WHAT ID STILL: the person's write WHAT was taken, and the newest version holds the ask
# block ID no longer (STILL empty) or holds a block under ID that is no ask (STILL a type).
taken() {
  [[ "$status" =~ ^20[01]$ ]] || fail person "$1" "HTTP $status: $(body)"
  wait_settled person
  latest_markdown person "$spec_a"
  [[ "$version" -gt "$kept_version" ]] || fail person "$1 writes a version" "the spec is still at $version"
  kept_version="$version"
  [[ -z "$(ask_block "$2" "$work/latest.md")" ]] || fail person "$1 is taken" "the ask block $2 is still there"
  if [[ -n "$3" ]]; then
    grep -Eq "^:+$3\\{#$2[ }]" "$work/latest.md" || fail person "$1 leaves a $3 under $2" "none in version $version"
  fi
  pass person "$1 is taken: version $version"
}
send person POST "/api/v1/artifacts/$spec_a/edits" '{"ops":[{"op":"delete","block":"q-kept"}]}'
taken "the person's delete of q-kept" q-kept ""
latest_markdown person "$spec_a"
without_block q-left-format "$work/latest.md" >"$work/replacement.md"
send person POST "/api/v1/issues/$issue_a/artifacts" \
  "$(jq -cn --arg n "$spec_name" --rawfile c "$work/replacement.md" '{name: $n, content: $c}')"
taken "the person's whole-document replacement without the answered q-left-format" q-left-format ""
send person POST "/api/v1/artifacts/$spec_a/edits" \
  '{"ops":[{"op":"retype","block":"q-doc-plain","type":"callout","attributes":{"kind":"note","title":""}}]}'
taken "the person's retype of the answered q-doc-plain" q-doc-plain callout
