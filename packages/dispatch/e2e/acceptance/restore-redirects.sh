#!/usr/bin/env bash
# dispatch://INFRA-1150's c-restore-redirects: no citation of a restored ask dangles. Every citation
# it resolves was written by the release before this branch.
#
# It drops and creates the database DATABASE_URL names (seed_reset_database: a database on this
# machine whose name begins infra_1150_), boots the base release on it through lib-baseline.sh
# (BASELINE_BINARY, default $TMPDIR/base-dispatch, the stamped build of BASELINE_SHA), with the dev
# sign-in, the pinned agent bearer e2e-token and the hold setting (DISPATCH_TEST_HOOKS=1 and
# DISPATCH_TEST_HOLD_DECISIONS_STEP=1, through BASELINE_EXTRA_ENV), and has seed-decisions.sh write
# the corpus through that server's API: a comment citing the second shape's ask block in the
# decisions document as dispatch://<A>/artifact/decisions-md#b-q-doc-plain, a block of a third
# document (notes.md) citing the same address, and a comment citing the old spec address of an ask
# of the main shape, dispatch://<A>/spec#b-q-left-format. Through the same server it writes one
# more citation of the second shape's address, a comment on issue C, whose closure is the one the
# closure leg reads: an issue's closure leaves its own documents out.
#
# Before this head boots it captures, through the base server, what each of R8's three resolvers
# answers: GET /api/v1/references?to=<old address> (blockReferenceNode), GET
# /api/v1/block-links?from=<notes.md>&to=<decisions.md> (refs.BlockLinks, whose block it reads
# the text of through the first resolver), and GET /api/v1/issues/<C>/references (refs.Closure);
# the citations of each set of the dangling counter, which the seed and this run wrote; and the
# heading of every section of both decisions documents, from the base's Decisions read. It stops
# that server and boots this head, HEAD_BINARY (default $TMPDIR/bin/envoy-dispatch), with the hold
# setting, so the commands below and not the boot make the first move.
#
# Before one ask moves it reads the tuple kind (the census's store-only moved_to line), the
# collision census, the three resolvers and the counter (the dangling_citations line of
# `envoy-dispatch census --store --verbose`): moved=0, and returning equal to the captured
# citations of the main shape's old spec address, which answers 404 NOT_FOUND. It reads the
# restore's dry run listing the second shape's asks as about to move. It runs the restore
# (`envoy-dispatch restore-answered-asks --project SEED`) and reads the citation again three ways:
# the references read and the block-links leg answer the restored ask's block at its new address
# with the captured text, and the closure answers the spec as the member the citation reaches. A
# text is held to the captured one character for character with two differences and no other,
# each printed: the eight server-owned ask attributes are absent, and a block that took a new id
# carries it. It reads one moved_to tuple for the move, with its time, and no second row; one for
# the colliding ask, to its new id; none for an ask of the main shape, whose old spec address now
# answers the restored ask block. The counter then reads zero in both sets.
#
# It runs the conversion (`envoy-dispatch convert-decisions --project SEED`) and reads
# dispatch://<A>/artifact/decisions-md resolving to the issue, and a citation of every section of
# both decisions documents resolving through one moved_to tuple: a section that cites no ask to the
# decision node that keeps its heading block id, a section that cites an ask, kept as a reading or
# dropped by the plain-answer rule, and a section that held an ask block, to that ask's block.
#
# Last it reads every migration this branch adds over BASELINE_SHA (default
# `git merge-base origin/main HEAD`, as lib-baseline.sh's) and the statement with which the
# backfill marks asks rows (graphcensus/backfill.go): no `create table` names block_moves, and no
# `update` sets the body of a comment or a message, block_refs.to_id, asks.block_id or
# asks.block_artifact_id. The same matcher runs first over planted statements of each kind, which
# it must flag, and one it must not.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. Every check is `cmd || fail …`, never part of an `&&` list.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_restore_redirects?sslmode=disable \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [BASELINE_SHA=<the base's commit>] \
         [HEAD_BINARY=$TMPDIR/bin/envoy-dispatch] bash e2e/acceptance/restore-redirects.sh
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
require_commands cmp curl git jq psql sed sort tr
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
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
checkout="$(git -C "$script_dir" rev-parse --show-toplevel)" || {
  printf 'cannot find the checkout %s is part of\n' "$script_dir" >&2
  exit 2
}
readonly checkout
base_sha="${BASELINE_SHA:-$(git -C "$checkout" merge-base origin/main HEAD)}" || {
  printf 'BASELINE_SHA is unset and git merge-base origin/main HEAD found no commit\n' >&2
  exit 2
}
git -C "$checkout" rev-parse --verify --quiet "${base_sha}^{commit}" >/dev/null || {
  printf 'BASELINE_SHA %s names no commit\n' "$base_sha" >&2
  exit 2
}
readonly base_sha
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
# send AS METHOD PATH [JSON]: METHOD PATH to $server as the agent (the bearer, in $work/auth). The
# answer's body goes to $work/body and its status to $status.
send() {
  local -a data=()
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$server$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# field SECTION WHAT JQ: one value of the last answer's body, which must be there, in $value.
field() {
  value="$(jq -er "$3" "$work/body" 2>/dev/null)" || fail "$1" "$2" "no $3 in: $(body)"
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

# census SECTION NAME: `envoy-dispatch census --store --verbose` into $work/NAME.out, its lines of
# the moves printed, and the counter's two sets in $moved and $returning.
census() {
  dispatch_command "$2" census --store --verbose
  [[ "$command_code" -le 1 ]] || fail "$1" "run the store census" "exited $command_code: $(<"$work/$2.out")"
  grep -E '^census: (store-only|dangling_citation|collisions|decisions)' "$work/$2.out" | sed 's/^/  /' || true
  local line
  line="$(grep '^census: dangling_citations ' "$work/$2.out" || true)"
  [[ "$line" =~ ^census:\ dangling_citations\ moved=([0-9,]+)\ returning=([0-9,]+)$ ]] ||
    fail "$1" "the counter prints its two sets" "${line:-no dangling_citations line}"
  moved="${BASH_REMATCH[1]//,/}"
  returning="${BASH_REMATCH[2]//,/}"
}

# counted SET NAME: the citations the counter counted in SET, `kind:id target` a line, sorted.
counted() {
  sed -nE "s/^census: dangling_citation set=$1 from=([^ ]+) to=([^ ]+)\$/\\1 \\2/p" "$work/$2.out" | LC_ALL=C sort
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

# resolve SECTION ADDRESS: GET /api/v1/references?to=ADDRESS, which must answer 200.
resolve() {
  send agent GET "/api/v1/references?to=$(q "$2")"
  want_status 200 "$1" "resolve $2"
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

# moved_to_rows ADDRESS: every moved_to tuple from the block node ADDRESS, one `to_id|asserted|at`
# line each, oldest first, into $work/moved.
moved_to_rows() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c \
    "select to_id, asserted, at from graph_tuples where kind = 'moved_to' and from_kind = 'block' and from_id = '$1' order by seq" \
    >"$work/moved" 2>"$work/psql" || fail moved_to "read the moved_to tuples of $1" "psql: $(<"$work/psql")"
}

# one_move SECTION FROM TO: one moved_to tuple from the block node FROM, to TO, asserting, with its
# time, and no second row for the same move.
one_move() {
  moved_to_rows "$2"
  [[ "$(wc -l <"$work/moved")" == 1 ]] ||
    fail "$1" "one moved_to tuple from $2" "$(wc -l <"$work/moved") rows: $(tr '\n' ' ' <"$work/moved")"
  IFS='|' read -r to asserted at <"$work/moved"
  [[ "$to" == "$3" && "$asserted" == t && -n "$at" ]] ||
    fail "$1" "the moved_to tuple from $2 reaches $3, with the time of the move" "$(<"$work/moved")"
  pass "$1" "one moved_to tuple from $2 to $3, at $at, and no second row"
}

# moved_to_naming ADDRESS: how many moved_to tuples name the block node ADDRESS at either end, in
# $answer.
moved_to_naming() {
  query moved_to "read the moved_to tuples naming $1" \
    "select count(*) from graph_tuples where kind = 'moved_to' and ((from_kind = 'block' and from_id = '$1') or (to_kind = 'block' and to_id = '$1'))"
}

# ---------------------------------------------------------------------------------------------
# The base release writes the corpus and the citations.
# ---------------------------------------------------------------------------------------------
seed_reset_database "$database_url"
DISPATCH_TOKEN=e2e-token
BASELINE_EXTRA_ENV=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_DECISIONS_STEP=1'
baseline_start
unset DISPATCH_TOKEN DATABASE_URL BASELINE_EXTRA_ENV
# database_url stays writable: lib-baseline.sh's baseline_boot, which boots this head, holds a
# local of that name.
seed_decisions
printf 'Authorization: Bearer %s\n' "$baseline_token" >"$work/auth"
server="$baseline_url"

readonly issue_a="${seed_ids[issue_a]}" issue_b="${seed_ids[issue_b]}" issue_c="${seed_ids[issue_c]}"
readonly notes_a="${seed_ids[notes_a]}"
readonly moved_address="dispatch://$issue_a/artifact/decisions-md#b-q-doc-plain"
readonly moved_node="$issue_a/decisions-md#q-doc-plain"
readonly returning_address="dispatch://$issue_a/spec#b-q-left-format"
readonly returning_node="$issue_a/spec#q-left-format"

# One more citation of the moved address, from another issue, whose closure the third leg reads.
send agent POST "/api/v1/issues/$issue_c/comments" \
  "$(jq -cn --arg b "Issue C reads the answer of $moved_address." --argjson a "$(seed_actor)" '{body: $b, actor: $a}')"
want_status 201 capture "a comment on $issue_c cites $moved_address through the base release"
field capture "the comment's id" '.id'
readonly comment_c="$value"
seed_wait_settled "the comment on $issue_c settles"

send agent GET /healthz
want_status 200 capture "read the base release's /healthz"
base_commit="$(jq -r '.commit // ""' "$work/body")"
readonly base_commit

# The citations of each set, as the base release wrote them.
printf '%s\n' "comment:${seed_ids[comment_decisions_block]} $moved_node" "artifact:$notes_a $moved_node" \
  "comment:$comment_c $moved_node" | LC_ALL=C sort >"$work/capture/moved-set"
printf '%s\n' "comment:${seed_ids[comment_old_spec_address]} $returning_node" >"$work/capture/returning-set"
printf 'captured citations of the moved set:\n%s\ncaptured citations of the returning set:\n%s\n' \
  "$(<"$work/capture/moved-set")" "$(<"$work/capture/returning-set")"

# The three resolvers, as the base release answers the second shape's address.
resolve capture "$moved_address"
expect capture "the references read answers the block in the decisions document" "\"$moved_node\"" '.node.id'
expect capture "the references read lists its three citations" 3 '[.edges[] | select(.kind == "mentions")] | length'
jq -j '.node.text' "$work/body" >"$work/capture/text"
send agent GET "/api/v1/block-links?from=$(q "dispatch://$issue_a/artifact/notes-md")&to=$(q "dispatch://$issue_a/artifact/decisions-md")"
want_status 200 capture "read the block links from notes.md to decisions.md"
expect capture "the block-links leg answers the citing block of notes.md and the cited block" "[\"$moved_node\"]" \
  "[.links[] | select(.from.id | startswith(\"$issue_a/notes-md#\")) | .to.id]"
send agent GET "/api/v1/issues/$issue_c/references"
want_status 200 capture "read the closure of $issue_c"
# The member the citation itself reaches is the one at depth 1, reached through the comment.
readonly closure_filter='[.members[] | select(.depth == 1) | {ref_key: .artifact.ref_key, via: "\(.via.kind):\(.via.id)"}]'
expect capture "the closure of $issue_c answers the decisions document as the member the citation reaches" \
  "[{\"ref_key\":\"$issue_a/decisions-md\",\"via\":\"comment:$comment_c\"}]" "$closure_filter"
send agent GET "/api/v1/references?to=$(q "$returning_address")"
want_status 404 capture "the old spec address of the main shape answers 404 before the restore"

# Every section of both decisions documents, by its heading block, from the base's Decisions read.
: >"$work/capture/sections"
for issue in "$issue_a" "$issue_b"; do
  send agent GET "/api/v1/issues/$issue/decisions"
  want_status 200 capture "read the Decisions of $issue through the base release"
  jq -r --arg i "$issue" '.decisions[] | "\($i) \(.block_id) \(.heading)"' "$work/body" >>"$work/capture/sections"
done
printf 'captured sections:\n%s\n' "$(<"$work/capture/sections")"
[[ "$(wc -l <"$work/capture/sections")" == 8 ]] ||
  fail capture "the two decisions documents hold eight sections" "$(wc -l <"$work/capture/sections") read"
baseline_stop_server

# ---------------------------------------------------------------------------------------------
# This head, held, before one ask moves.
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

dispatch_command backfill backfill-graph-store
[[ "$command_code" == 0 ]] || fail before "envoy-dispatch backfill-graph-store" "exited $command_code: $(<"$work/backfill.out")"
pass before "envoy-dispatch backfill-graph-store stated the rows the release before wrote"

census before census-before
grep -q '^census: store-only kind=moved_to tuples=0 unresolved=0 ' "$work/census-before.out" ||
  fail before "the moved_to kind answers, with no tuple yet" "$(grep 'kind=moved_to' "$work/census-before.out" || echo 'no line')"
for block in q-doc-collide q-left-store; do
  grep -q "^census: collisions issue=$issue_a block=$block\$" "$work/census-before.out" ||
    fail before "the collision census lists $issue_a's $block" "no such line"
done
pass before "the tuple kind answers and the collision census lists both colliding ids"
[[ "$moved" == 0 ]] || fail before "the first set reads zero before the first restore" "moved=$moved"
counted returning census-before >"$work/returning"
cmp -s "$work/returning" "$work/capture/returning-set" ||
  fail before "the second set reads the captured citations of the old spec addresses" \
    "returning=$returning: $(tr '\n' ';' <"$work/returning") want $(tr '\n' ';' <"$work/capture/returning-set")"
pass before "the counter reads moved=0 and returning=$returning, the captured citation of $returning_address"
resolve before "$moved_address"
expect before "the references read answers the block in the decisions document" "\"$moved_node\"" '.node.id'
send agent GET "/api/v1/block-links?from=$(q "dispatch://$issue_a/artifact/notes-md")&to=$(q "dispatch://$issue_a/artifact/decisions-md")"
want_status 200 before "read the block links from notes.md to decisions.md"
expect before "the block-links leg answers the cited block" "[\"$moved_node\"]" \
  "[.links[] | select(.from.id | startswith(\"$issue_a/notes-md#\")) | .to.id]"
send agent GET "/api/v1/issues/$issue_c/references"
want_status 200 before "read the closure of $issue_c"
expect before "the closure answers the decisions document" \
  "[{\"ref_key\":\"$issue_a/decisions-md\",\"via\":\"comment:$comment_c\"}]" "$closure_filter"
send agent GET "/api/v1/references?to=$(q "$returning_address")"
want_status 404 before "$returning_address answers 404 before the restore"
expect before "$returning_address answers NOT_FOUND" '"NOT_FOUND"' '.code'
pass before "the three resolvers answer, and $returning_address answers 404 NOT_FOUND, before one ask moves"

dispatch_command dry restore-answered-asks --project "$project" --dry-run
grep '^restore-answered-asks: ' "$work/dry.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail before "the restore's dry run" "exited $command_code: $(<"$work/dry.out")"
grep -q "^restore-answered-asks: issue=$issue_a restored=4 moved=2 new_ids=2 .* dry_run=true\$" "$work/dry.out" ||
  fail before "the dry run lists the second shape's two asks of $issue_a as about to move" "$(grep "issue=$issue_a " "$work/dry.out" || echo 'no line')"
pass before "the dry run lists the second shape's two asks as about to move, and wrote nothing"

# ---------------------------------------------------------------------------------------------
# The restore, and the citation read again.
# ---------------------------------------------------------------------------------------------
dispatch_command restore restore-answered-asks --project "$project"
grep '^restore-answered-asks: ' "$work/restore.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail restore "envoy-dispatch restore-answered-asks --project $project" "exited $command_code: $(<"$work/restore.out")"
pass restore "envoy-dispatch restore-answered-asks --project $project"
wait_settled restore

query restore "read the new block of the colliding ask" "select block_id from asks where id = '${seed_ids[ask_doc_collide]}'"
readonly collide_id="$answer"
[[ "$collide_id" != q-doc-collide ]] || fail restore "the colliding ask takes a new block id" "it kept q-doc-collide"
readonly restored_node="$issue_a/spec#q-doc-plain"

resolve after "$moved_address"
expect after "the references read reaches the same ask at its new address" "\"$restored_node\"" '.node.id'
jq -j '.node.text' "$work/body" >"$work/text.got"
same_text after "the references read answers the captured text" "$work/capture/text" "$work/text.got" q-doc-plain q-doc-plain
send agent GET "/api/v1/block-links?from=$(q "dispatch://$issue_a/artifact/notes-md")&to=$(q "dispatch://$issue_a/spec")"
want_status 200 after "read the block links from notes.md to the spec"
expect after "the block-links leg answers the citing block of notes.md and the restored ask's block" "[\"$restored_node\"]" \
  "[.links[] | select(.from.id | startswith(\"$issue_a/notes-md#\")) | .to.id]"
field after "the reference of the block the block-links leg answers" '.links[0].to.ref'
resolve after "$value"
jq -j '.node.text' "$work/body" >"$work/text.got"
same_text after "the block the block-links leg answers holds the captured text" "$work/capture/text" "$work/text.got" q-doc-plain q-doc-plain
send agent GET "/api/v1/issues/$issue_c/references"
want_status 200 after "read the closure of $issue_c"
expect after "the closure answers the spec as the member the citation reaches" \
  "[{\"ref_key\":\"$issue_a/spec\",\"via\":\"comment:$comment_c\"}]" "$closure_filter"

one_move moved_to "$moved_node" "$restored_node"
one_move moved_to "$issue_a/decisions-md#q-doc-collide" "$issue_a/spec#$collide_id"
for block in q-left-format q-left-store; do
  moved_to_naming "$issue_a/spec#$block"
  [[ "$answer" == 0 ]] || fail moved_to "no moved_to tuple names $issue_a/spec#$block, an ask of the main shape" "$answer do"
done
pass moved_to "no moved_to tuple names an ask of the main shape"
resolve after "$returning_address"
expect after "$returning_address answers the restored ask block" "\"$returning_node\"" '.node.id'
expect after "$returning_address answers an ask block" '"ask"' '.node.text | capture("^:::(?<t>[a-z]+)").t'
pass after "$returning_address answers the restored ask block after the restore"

census after census-after
[[ "$moved" == 0 ]] ||
  fail after "the first set reads zero after the last restore" "moved=$moved: $(counted moved census-after | tr '\n' ';') old address $moved_address"
[[ "$returning" == 0 ]] ||
  fail after "the second set reads zero after the restore" "returning=$returning: $(counted returning census-after | tr '\n' ';')"
pass after "the counter reads moved=0 and returning=0 after the restore, over the $(wc -l <"$work/capture/moved-set") captured citations of the moved address and the $(wc -l <"$work/capture/returning-set") of the old spec address"

# ---------------------------------------------------------------------------------------------
# The conversion: the document and every section of it resolve.
# ---------------------------------------------------------------------------------------------
dispatch_command convert convert-decisions --project "$project"
grep '^convert-decisions: ' "$work/convert.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail convert "envoy-dispatch convert-decisions --project $project" "exited $command_code: $(<"$work/convert.out")"
pass convert "envoy-dispatch convert-decisions --project $project"
wait_settled convert

resolve convert "dispatch://$issue_a/artifact/decisions-md"
expect convert "dispatch://$issue_a/artifact/decisions-md resolves to the issue, whose Decisions read answers it" \
  "{\"id\":\"$issue_a\",\"kind\":\"issue\"}" '.node | {kind, id}'

query convert "read the block ids of the asks" "select string_agg(id::text || '=' || block_id, ' ') from asks where issue_key in ('$issue_a', '$issue_b')"
declare -A block_of=()
for pair in $answer; do
  block_of["${pair%%=*}"]="${pair#*=}"
done
# The node each section became, by its heading.
declare -A target_of=(
  ["Readings come from the seeded database"]="$issue_a/spec#${block_of[${seed_ids[ask_left_store]}]}"
  ["How a reading is written: dispatch://$issue_a/ask/${seed_ids[ask_left_format]}"]="$issue_a/spec#${block_of[${seed_ids[ask_left_format]}]}"
  ["Which reader reads the corpus first"]="$issue_a/spec#${block_of[${seed_ids[ask_doc_plain]}]}"
  ["Which line names a moved block"]="$issue_a/spec#${block_of[${seed_ids[ask_doc_collide]}]}"
  ["The release before this one writes the corpus"]="$issue_b/spec#${block_of[${seed_ids[ask_b_left]}]}"
)
while read -r issue heading_id heading; do
  # A section that cites no ask becomes the decision node that keeps its heading block id.
  target="${target_of[$heading]:-$issue/spec#$heading_id}"
  resolve convert "dispatch://$issue/artifact/decisions-md#b-$heading_id"
  expect convert "the section \"$heading\" resolves to the node it became" "\"$target\"" '.node.id'
  one_move convert "$issue/decisions-md#$heading_id" "$target"
done <"$work/capture/sections"
pass convert "a citation of every section of both decisions documents resolves through one moved_to tuple"
census convert census-convert
[[ "$moved" == 0 && "$returning" == 0 ]] ||
  fail convert "the counter reads zero in both sets after the conversion" "moved=$moved returning=$returning"
pass convert "the counter reads zero in both sets after the conversion"

# ---------------------------------------------------------------------------------------------
# No migration of this branch moves a citation, and neither does the backfill.
# ---------------------------------------------------------------------------------------------
# rewrites STATEMENT: what STATEMENT, one SQL statement, comments out and lower case, does that this
# child may not do: create a block_moves table, or set a comment's or a message's body,
# block_refs.to_id, asks.block_id or asks.block_artifact_id; nothing where it does neither.
rewrites() {
  local statement table set
  statement="$(tr '\n\t' '  ' <<<"$1" | tr -s ' ' | sed -E 's/^ //')"
  statement="${statement,,}"
  if [[ "$statement" =~ ^create\ table\ (if\ not\ exists\ )?(public\.)?\"?block_moves\"?([^a-z0-9_]|$) ]]; then
    printf 'creates the table block_moves'
    return 0
  fi
  [[ "$statement" =~ ^update\ (only\ )?(public\.)?\"?([a-z_]+)\"?(\ (as\ )?[a-z_]+)?\ set\ (.*)$ ]] || return 0
  table="${BASH_REMATCH[3]}"
  set="${BASH_REMATCH[6]}"
  set="${set%% where *}"
  set="${set%% from *}"
  set="${set%% returning *}"
  set=" ${set//,/ , }"
  case "$table" in
    comments | messages) [[ ! "$set" =~ [\ .\"]body\"?\ ?= ]] || printf 'sets %s.body' "$table" ;;
    block_refs) [[ ! "$set" =~ [\ .\"]to_id\"?\ ?= ]] || printf 'sets block_refs.to_id' ;;
    asks)
      if [[ "$set" =~ [\ .\"]block_id\"?\ ?= ]]; then
        printf 'sets asks.block_id'
      elif [[ "$set" =~ [\ .\"]block_artifact_id\"?\ ?= ]]; then
        printf 'sets asks.block_artifact_id'
      fi
      ;;
  esac
}

# statements FILE: FILE's SQL statements, its `--` comments taken out, one a line (NUL-separated).
statements() {
  sed -E 's/--.*$//' "$1" | tr '\n' ' ' | tr ';' '\0'
}

# The matcher's control: a planted statement of each kind it must flag, and one it must not.
planted=(
  "create table block_moves (id bigserial primary key, from_id text, to_id text)"
  "CREATE TABLE IF NOT EXISTS public.block_moves (id int)"
  "update comments set body = replace(body, 'decisions-md', 'spec') where issue_key = 'A'"
  "update messages m set edited_at = now(), body = '' where m.id = 1"
  "update block_refs set to_id = 'A/spec#x' where to_id = 'A/decisions-md#x'"
  "update asks set block_id = 'x' where id = '00000000-0000-0000-0000-000000000000'"
  "UPDATE asks SET node_kind = 'ask', block_artifact_id = null"
)
for statement in "${planted[@]}"; do
  found="$(rewrites "$statement")"
  [[ -n "$found" ]] || fail migrations "the matcher flags the planted statement: $statement" "it does not"
  printf 'control: the matcher flags the planted statement (%s): %s\n' "$found" "$statement"
done
readonly marking_like="update asks set node_kind = \$2, node_address = id::text where id = any(\$1::uuid[]) and node_kind is null"
[[ -z "$(rewrites "$marking_like")" ]] || fail migrations "the matcher passes a statement that sets only node_kind and node_address" "it flags it"
pass migrations "the matcher flags each of the ${#planted[@]} planted statements and passes one that marks asks rows"

readonly migrations_dir=packages/envoy/internal/dispatch/store/migrations
git -C "$checkout" ls-tree -r --name-only "$base_sha" -- "$migrations_dir" | sed 's#.*/##' | LC_ALL=C sort >"$work/base-migrations" ||
  fail migrations "list the migrations of $base_sha" "git ls-tree failed"
find "$checkout/$migrations_dir" -maxdepth 1 -name '*.sql' -printf '%f\n' | LC_ALL=C sort >"$work/head-migrations"
mapfile -t added < <(LC_ALL=C comm -13 "$work/base-migrations" "$work/head-migrations")
[[ "${#added[@]}" -gt 0 ]] || fail migrations "this branch adds migrations over $base_sha" "it adds none"
findings=0
read_statements=0
for migration in "${added[@]}"; do
  while IFS= read -r -d '' statement; do
    [[ "$statement" =~ [^[:space:]] ]] || continue
    read_statements=$((read_statements + 1))
    found="$(rewrites "$statement")"
    if [[ -n "$found" ]]; then
      printf 'finding: %s %s: %s\n' "$migration" "$found" "$statement"
      findings=$((findings + 1))
    fi
  done < <(statements "$checkout/$migrations_dir/$migration")
  printf 'read migration %s\n' "$migration"
done
[[ "$findings" == 0 ]] ||
  fail migrations "no migration of this branch creates block_moves or rewrites a citation's address" "$findings findings, each printed above"
pass migrations "the ${#added[@]} migrations this branch adds over $base_sha ($read_statements statements) create no block_moves table and set no comment or message body, block_refs.to_id, asks.block_id or asks.block_artifact_id"

# The backfill's statement that marks asks rows.
backfill_go="$checkout/packages/envoy/internal/dispatch/graphcensus/backfill.go"
mapfile -t marking < <(awk '
  /`/ { n = split($0, parts, "`"); for (i = 1; i <= n; i++) { if (i > 1) inside = !inside; if (inside) text = text " " parts[i]; if (i > 1 && !inside && text != "") { print text; text = "" } } next }
  inside { text = text " " $0 }
' "$backfill_go" | grep -i 'update asks ' || true)
[[ "${#marking[@]}" -gt 0 ]] || fail backfill "find the statement that marks asks rows in graphcensus/backfill.go" "no update of asks read"
for statement in "${marking[@]}"; do
  found="$(rewrites "$statement")"
  printf 'the backfill marks asks rows with: %s\n' "$(tr -s ' ' <<<"$statement")"
  [[ -z "$found" ]] || fail backfill "the backfill that marks asks rows sets no citation's address" "it $found"
  [[ "${statement,,}" =~ set\ node_kind ]] || fail backfill "the statement sets asks.node_kind" "$statement"
done
pass backfill "the backfill that marks asks rows with asks.node_kind and asks.node_address sets nothing else a citation reads"
