#!/usr/bin/env bash
# The check c-attribute-removal-count: the three measurements this child takes
# before it writes a migration, read over a seeded corpus the release before this branch wrote,
# and the pull request body that must hold them.
#
# It drops and creates the database DATABASE_URL names (seed_reset_database, which takes only a
# database on this machine whose name begins infra_1150_), boots the base release on it through
# lib-baseline.sh, and has seed-decisions.sh write the corpus through that server's own API, so
# every row the queries read was written by the base release. The server is BASELINE_BINARY,
# default $TMPDIR/base-dispatch, the stamped build of BASELINE_SHA the Verification preamble
# makes; the harness boots it with the dev sign-in on (DISPATCH_DEV_SIGNIN=1, which lib-baseline.sh
# sets for every server it boots) and with the pinned agent bearer e2e-token as
# DISPATCH_AGENT_TOKEN, which it takes from DISPATCH_TOKEN. Beside the seed's corpus, where each
# document holds one review, it has the person review two documents again through that server:
# spec B approved a second time at its newest version, and spec C approved and then sent back for
# changes, so its newest review is no approval. Neither moves the count the corpus holds. It then
# stops that server and runs the three statements of the spec's section "What this child measures
# before it writes a migration", verbatim, over the database, each in a read-only transaction, and
# prints
#     documents_holding_an_ask=<n> at=<UTC>
#     standing_approvals_over_those_documents=<n> at=<UTC>
#     colliding_block_ids=<n> at=<UTC>
#     collision issue=<KEY> block=<id> shape=<block_left_spec|block_in_decisions_document>
# the last once for each row the third statement reads; each time is `date -u +%FT%TZ` taken as
# its statement answered. The first two statements read the documents holding an open or answered
# ask block, by its asks row, or a copy of one, by the copied_from its document's newest version
# holds. It fails when either of the first two counts differs from the count the corpus holds
# (seed-decisions.sh's corpus_* lines), printing both. The third count may be anything, zero
# included; the corpus's own is printed beside it. The corpus holds copies.md, a document whose
# only ask block is a copy of the answered ask q-kept: it reads the first statement again with its
# select list naming each document it counts, and fails unless copies.md is among them, no asks row
# names it, and its newest version holds one ask block, which holds copied_from naming q-kept's ask
# and state="answered". The corpus holds a collision of each shape: it fails unless each of the
# corpus's collisions is among the rows the third statement read, under its shape, so the rows
# printed hold both shapes. It also reads the approval reviews over those documents at each one's
# newest version, and the documents holding one, and fails unless each reads apart from the
# standing approvals: a statement that counted reviews rather than documents, or any approval
# rather than the newest review, reads another count here.
#
# BASELINE_SHA, the commit of the release this branch lands after, is required and has no
# default: only the caller knows which release that is. Once the base server has booted, and
# before the seed writes a row, the commit it is stamped with (the `commit` of its /healthz, the
# one it registers in server_processes) must be the commit BASELINE_SHA names in this checkout, or
# the run fails naming both, so that a binary built from another commit never stands in for that
# release.
#
# It then runs TestTheTablesThisChildsMigrationsTouch in packages/envoy, requires its
# `--- PASS:` line, and prints the tables it names (`touched: <table>` each, or `touched: none`)
# and the limit it prints, pgmigrate.CensusTableLimit as this branch holds it. For each table it
# reads pg_total_relation_size over the database (0 for a table the base schema does not hold
# yet, which no migration can be held up on) and prints
#     pg_total_relation_size table=<table> bytes=<n> at=<UTC>
# A reading at or above the limit fails the run.
#
# Then the pull request body. Without PR_BODY_FILE it is the body of pull request #4173, this
# branch's: read with `gh pr view 4173 --repo cybermonklabs/legion --json body --jq .body` where gh
# is installed and signed in, and otherwise from GitHub's public REST API, which answers it to an
# unsigned request, the repository being public (`curl -fsS
# https://api.github.com/repos/cybermonklabs/legion/pulls/4173`, its `.body` read with `jq -r`).
# With PR_BODY_FILE it is the file that names, which the check's negative control uses: such a
# body is checked as below, and the run then prints a `SKIP: body …` line saying it read no body of
# pull request #4173, so a run with PR_BODY_FILE set, exported or not, never passes
# c-attribute-removal-count, whose run text refuses a SKIP line. An empty body fails. It must hold
# exactly one line of each of these forms, a carriage return before the line's end aside, and
# leading and trailing spaces allowed:
#     - measurement documents_holding_an_ask=<n> at=<YYYY-MM-DDTHH:MM:SSZ>
#     - measurement standing_approvals_over_those_documents=<n> at=<YYYY-MM-DDTHH:MM:SSZ>
#     - measurement colliding_block_ids=<n> at=<YYYY-MM-DDTHH:MM:SSZ>
#     - pg_total_relation_size table=<table> bytes=<n> at=<YYYY-MM-DDTHH:MM:SSZ>
# the first three each with the count this run read and the time that reading was taken, and the
# last once for each table the test names, its bytes below the limit. A body missing one of them,
# holding one twice, or recording another count, fails naming that measurement or table.
#
# MEASURE_ONLY=1 reads no body. It does everything above up to the body, then prints the block
# to paste into the pull request body, the lines of the forms just above with this run's
# readings, and last a `SKIP: body …` line, and exits 0; the implementer runs it to take the
# readings. The check's run text refuses a run that prints a SKIP line, so a run with
# MEASURE_ONLY set, exported or not, never passes c-attribute-removal-count.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1, and a missing or bad input
# exits 2. It prints a SKIP line under MEASURE_ONLY=1, and after a body read from PR_BODY_FILE,
# alone.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_answer_attribute_removal?sslmode=disable \
         BASELINE_SHA=<the commit of the release this branch lands after> \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [PR_BODY_FILE=<file holding the body>] \
         [MEASURE_ONLY=1] bash e2e/acceptance/answer-attribute-removal.sh
       (run from packages/dispatch; DATABASE_URL names a database on this machine whose name
       begins infra_1150_, which this run drops and creates again; without PR_BODY_FILE the
       body is read with gh, or with curl from GitHub's REST API where gh is absent or not
       signed in; with it the body is read from that file and checked, and a SKIP line follows,
       so the check does not pass on it; MEASURE_ONLY=1 prints the body's lines and a SKIP line
       and reads no body, so the check does not pass on it either.
       BASELINE_SHA is required and has no default; BASELINE_BINARY must be the server built
       from it and stamped with it)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=seed-decisions.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-decisions.sh"

require_env DATABASE_URL BASELINE_SHA
require_commands curl date git go grep jq paste psql sed tr
# Not readonly until baseline_start has booted: lib-baseline.sh's baseline_boot holds a local of
# this name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL: only a server's subshell in
# lib-baseline.sh sets another, for that server
database_url="$DATABASE_URL"
readonly measure_only="${MEASURE_ONLY:-}"
case "$measure_only" in
  "" | 1) ;;
  *)
    printf 'MEASURE_ONLY is 1 or unset, not %s\n' "$measure_only" >&2
    usage
    exit 2
    ;;
esac
if [[ -z "${BASELINE_BINARY:-}" ]]; then
  require_env TMPDIR
  # shellcheck disable=SC2031 # this shell's own TMPDIR: only a server's subshell sets another
  BASELINE_BINARY="$TMPDIR/base-dispatch"
fi
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
# The commit BASELINE_SHA names, in full: the one the base server booted below must be stamped with.
base_sha="$(git -C "$script_dir" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" || {
  printf 'BASELINE_SHA %s names no commit of this checkout\n' "$BASELINE_SHA" >&2
  exit 2
}
readonly base_sha
readonly envoy_dir="$script_dir/../../../envoy"
readonly test_name=TestTheTablesThisChildsMigrationsTouch
# A time as `date -u +%FT%TZ` writes it.
readonly time_pattern='[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z'

work_dir
cleanup() {
  local code=$?
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The corpus, written by the base release over a database of its own.
seed_reset_database "$database_url"
# The pinned agent bearer the e2e harness gives its own server, which baseline_start boots the
# base with; it reaches no process after this.
DISPATCH_TOKEN=e2e-token
baseline_start
unset DISPATCH_TOKEN DATABASE_URL
readonly database_url
# The base server is the release BASELINE_SHA names, as the commit it is stamped with says, before
# it writes a row.
base_status="$(curl -sS -o "$work/body" -w '%{http_code}' "$baseline_url/healthz" 2>"$work/curl")" ||
  base_status="curl failed: $(<"$work/curl")"
[[ "$base_status" == 200 ]] || fail base "read the base release's /healthz" "HTTP $base_status: $(body)"
base_commit="$(jq -r '.commit // ""' "$work/body")"
readonly base_commit
[[ "$base_commit" == "$base_sha" ]] ||
  fail base "the base release is stamped with the commit BASELINE_SHA names" \
    "it serves '$base_commit', and BASELINE_SHA $BASELINE_SHA is $base_sha"
pass base "the base release serves $base_commit, the commit BASELINE_SHA names"
seed_decisions
# A second review on two documents, so the standing approvals read apart from the approval reviews
# and from the documents holding one; the corpus count seed_decisions printed stands.
seed_review review_spec_b_again "${seed_ids[spec_b]}" approved
seed_review review_spec_c_approved "${seed_ids[spec_c]}" approved
seed_review review_spec_c_again "${seed_ids[spec_c]}" changes_requested "Name the fourth issue before asking again."
# Nothing writes while the statements read.
baseline_stop_server

# measure SQL: SQL's rows, `|` between columns, read in a read-only transaction, into $work/rows.
measure() {
  PGOPTIONS='-c default_transaction_read_only=on' env -u PGHOSTADDR \
    psql "$database_url" -X -At -F '|' -v ON_ERROR_STOP=1 -c "$1" >"$work/rows" 2>"$work/psql"
}

# The three statements, as the spec writes them.
read -r -d '' documents_sql <<'SQL' || true
select count(distinct d.artifact_id) as documents_holding_an_ask_block
from (
  select k.block_artifact_id as artifact_id
  from asks k
  where k.block_id is not null
    and k.block_artifact_id is not null
    and k.state in ('open', 'answered')
  union
  select v.artifact_id
  from artifact_versions v
  cross join lateral regexp_matches(v.markdown, 'copied_from="([^"]+)"', 'g') as m
  join asks k on k.id::text = m[1] and k.state in ('open', 'answered')
  where v.number = (select max(w.number) from artifact_versions w where w.artifact_id = v.artifact_id)
) d
SQL
read -r -d '' approvals_sql <<'SQL' || true
with newest as (
  select distinct on (artifact_id) artifact_id, state, version
  from artifact_reviews
  order by artifact_id, event_seq desc nulls last, created_at desc, id desc
)
select count(*) as standing_approvals_over_those_documents
from newest n
where n.state = 'approved'
  and n.version = (select max(number) from artifact_versions v where v.artifact_id = n.artifact_id)
  and n.artifact_id in (
    select block_artifact_id
    from asks
    where block_id is not null
      and block_artifact_id is not null
      and state in ('open', 'answered')
    union
    select c.artifact_id
    from artifact_versions c
    cross join lateral regexp_matches(c.markdown, 'copied_from="([^"]+)"', 'g') as m
    join asks k on k.id::text = m[1] and k.state in ('open', 'answered')
    where c.number = (select max(w.number) from artifact_versions w where w.artifact_id = c.artifact_id))
SQL
read -r -d '' collisions_sql <<'SQL' || true
select s.issue_key, k.id as ask_id, k.block_id, case when h.id = s.id then 'block_left_spec' else 'block_in_decisions_document' end as shape
from asks k
join artifacts h on h.id = k.block_artifact_id and (h.is_primary or h.role = 'decisions')
join artifacts s on s.issue_key = h.issue_key and s.is_primary
join document_blocks b on b.artifact_id = s.id and b.block_id = k.block_id and (h.id <> s.id or b.type <> 'ask')
where k.block_id is not null
  and k.state in ('answered', 'resolved')
SQL

# count_of NAME SQL: runs SQL, one count, into $count and the time it answered into $count_at.
count_of() {
  measure "$2" || fail measurement "$1" "psql: $(<"$work/psql")"
  count_at="$(date -u +%FT%TZ)"
  count="$(<"$work/rows")"
  [[ "$count" =~ ^[0-9]+$ ]] || fail measurement "$1" "the statement answered '$count', not one count"
}

# against_corpus NAME COUNT: COUNT is the count the corpus holds, which the seed printed.
against_corpus() {
  local corpus="${seed_ids[corpus_$1]:-}"
  [[ -n "$corpus" ]] || fail measurement "$1" "the seed printed no corpus_$1"
  [[ "$2" == "$corpus" ]] || fail measurement "$1" "read $2, the corpus holds $corpus"
  pass measurement "$1=$2, the count the corpus holds"
}

count_of documents_holding_an_ask "$documents_sql"
readonly documents="$count" documents_at="$count_at"
printf 'documents_holding_an_ask=%s at=%s\n' "$documents" "$documents_at"
count_of standing_approvals_over_those_documents "$approvals_sql"
readonly approvals="$count" approvals_at="$count_at"
printf 'standing_approvals_over_those_documents=%s at=%s\n' "$approvals" "$approvals_at"
measure "$collisions_sql" || fail measurement colliding_block_ids "psql: $(<"$work/psql")"
collisions_at="$(date -u +%FT%TZ)"
readonly collisions_at
mapfile -t collision_rows <"$work/rows"
readonly collisions="${#collision_rows[@]}"
printf 'colliding_block_ids=%s at=%s\n' "$collisions" "$collisions_at"
for row in "${collision_rows[@]}"; do
  IFS='|' read -r issue _ block shape <<<"$row"
  printf 'collision issue=%s block=%s shape=%s\n' "$issue" "$block" "$shape"
done
printf 'corpus colliding_block_ids=%s\n' "${seed_ids[corpus_colliding_block_ids]:-}"

# The corpus holds a document whose only ask block is a copy of an answered ask - copies.md, a copy
# of spec A's q-kept - and the first count includes it: no asks row names it, so the statement's
# copy arm finds it, by the copied_from its newest version holds. The statement is read again with
# its select list naming each document it counts, the rest of it as the spec writes it.
readonly copies_document="${seed_ids[copies_a]:-}"
[[ -n "$copies_document" ]] || fail measurement documents_holding_an_ask "the seed printed no copies_a"
documents_listing_sql="${documents_sql/select count(distinct d.artifact_id) as documents_holding_an_ask_block/select distinct d.artifact_id}"
[[ "$documents_listing_sql" != "$documents_sql" ]] ||
  fail measurement documents_holding_an_ask "the first statement opens with no select list this run can read each document of"
measure "$documents_listing_sql" || fail measurement documents_holding_an_ask "psql: $(<"$work/psql")"
printf 'documents_holding_an_ask counts: %s\n' "$(paste -sd ' ' "$work/rows")"
grep -qx -- "$copies_document" "$work/rows" ||
  fail measurement documents_holding_an_ask \
    "the first count leaves out copies.md ($copies_document), whose only ask block is a copy of the answered ask ${seed_ids[ask_kept]}: it counts $(paste -sd ' ' "$work/rows")"
measure "select count(*) from asks where block_artifact_id = '$copies_document'" ||
  fail corpus "read the asks rows of copies.md" "psql: $(<"$work/psql")"
[[ "$(<"$work/rows")" == 0 ]] ||
  fail corpus "no asks row names copies.md, whose only ask block is a copy" "$(<"$work/rows") rows do"
measure "select v.markdown from artifact_versions v where v.artifact_id = '$copies_document' order by v.number desc limit 1" ||
  fail corpus "read the newest version of copies.md" "psql: $(<"$work/psql")"
mapfile -t copy_openings < <(grep -E '^:+ask\{' "$work/rows" || true)
[[ "${#copy_openings[@]}" == 1 && "${copy_openings[0]}" == *" copied_from=\"${seed_ids[ask_kept]}\""* &&
  "${copy_openings[0]}" == *' state="answered"'* ]] ||
  fail corpus "the newest version of copies.md holds one ask block, a copy of the answered ask ${seed_ids[ask_kept]}" \
    "its ask blocks open: $(printf '%s / ' "${copy_openings[@]}")"
pass measurement "documents_holding_an_ask includes copies.md ($copies_document), whose only ask block is a copy of the answered ask ${seed_ids[ask_kept]}, which no asks row names: ${copy_openings[0]}"
against_corpus documents_holding_an_ask "$documents"
against_corpus standing_approvals_over_those_documents "$approvals"

# Every colliding block id of the restore is printed with its shape, for both shapes: the corpus
# holds a collision of each shape (seed-decisions.sh's corpus_collision_<n> lines), and each is among
# the rows the third statement read, under its shape.
declare -A collision_read=()
for row in "${collision_rows[@]}"; do
  IFS='|' read -r issue _ block shape <<<"$row"
  collision_read["$issue $block $shape"]=1
done
declare -A corpus_shape=()
for ((index = 1; index <= ${seed_ids[corpus_colliding_block_ids]:-0}; index++)); do
  collision="${seed_ids[corpus_collision_$index]:-}"
  [[ -n "$collision" ]] || fail measurement colliding_block_ids "the seed printed no corpus_collision_$index"
  [[ -n "${collision_read[$collision]:-}" ]] ||
    fail measurement colliding_block_ids "the corpus's collision $collision (issue, block, shape) is not among the rows printed"
  corpus_shape["${collision##* }"]="$collision"
done
for shape in block_left_spec block_in_decisions_document; do
  [[ -n "${corpus_shape[$shape]:-}" ]] ||
    fail measurement colliding_block_ids "the corpus holds no collision of the shape $shape, so this run cannot read that shape printed"
done
pass measurement "colliding_block_ids prints the corpus's collision of each shape: ${corpus_shape[block_left_spec]} and ${corpus_shape[block_in_decisions_document]}"

# The corpus tells the standing approvals apart from the approval reviews at a document's newest
# version and from the documents holding one: spec B holds two approvals, and spec C an approval
# its newest review, a request for changes, overrides. The documents are the second statement's,
# the rows of open and answered asks and the copies of one.
read -r -d '' approval_reviews_sql <<'SQL' || true
select count(*), count(distinct r.artifact_id)
from artifact_reviews r
where r.state = 'approved'
  and r.version = (select max(number) from artifact_versions v where v.artifact_id = r.artifact_id)
  and r.artifact_id in (
    select block_artifact_id
    from asks
    where block_id is not null
      and block_artifact_id is not null
      and state in ('open', 'answered')
    union
    select c.artifact_id
    from artifact_versions c
    cross join lateral regexp_matches(c.markdown, 'copied_from="([^"]+)"', 'g') as m
    join asks k on k.id::text = m[1] and k.state in ('open', 'answered')
    where c.number = (select max(w.number) from artifact_versions w where w.artifact_id = c.artifact_id))
SQL
measure "$approval_reviews_sql" || fail corpus "read the approval reviews over the documents holding an ask" "psql: $(<"$work/psql")"
IFS='|' read -r approval_reviews approved_documents <"$work/rows"
printf 'corpus approval_reviews_at_the_newest_version=%s documents_holding_one=%s standing_approvals=%s\n' \
  "$approval_reviews" "$approved_documents" "$approvals"
[[ "$approval_reviews" =~ ^[0-9]+$ && "$approved_documents" =~ ^[0-9]+$ &&
  "$approval_reviews" != "$approvals" && "$approved_documents" != "$approvals" ]] ||
  fail corpus "the standing approvals read apart from the approval reviews and from the documents holding one" \
    "approval reviews $approval_reviews, documents holding one $approved_documents, standing approvals $approvals"
pass corpus "the standing approvals ($approvals) read apart from the approval reviews ($approval_reviews) and the documents holding one ($approved_documents)"

# The tables this child's migrations touch, as the Go test names them, and the limit each is held
# to. `go -C` runs it from packages/envoy, as `(cd ../envoy && go test …)` from packages/dispatch.
go -C "$envoy_dir" test ./internal/dispatch/store -run "^$test_name\$" -count=1 -v \
  >"$work/touched.log" 2>&1 ||
  fail touched "run $test_name" "go test exited non-zero: $(tail -n 20 "$work/touched.log")"
grep -q "^--- PASS: $test_name " "$work/touched.log" ||
  fail touched "$test_name passes" "no '--- PASS: $test_name' line in: $(tail -n 20 "$work/touched.log")"
pass touched "$test_name passes"
mapfile -t touched < <(sed -nE 's/^[[:space:]]+[^[:space:]]+\.go:[0-9]+: touched: (.*)$/\1/p' "$work/touched.log")
mapfile -t limits < <(sed -nE 's/^[[:space:]]+[^[:space:]]+\.go:[0-9]+: census_table_limit: (.*)$/\1/p' "$work/touched.log")
[[ "${#touched[@]}" -gt 0 ]] ||
  fail touched "$test_name names the tables" "it printed no 'touched:' line: $(<"$work/touched.log")"
[[ "${#limits[@]}" -eq 1 && "${limits[0]}" =~ ^[0-9]+$ ]] ||
  fail touched "$test_name names the census table limit" "want one 'census_table_limit: <bytes>' line: $(<"$work/touched.log")"
readonly limit="${limits[0]}"
if [[ "${touched[*]}" == none ]]; then
  touched=()
fi
tables=()
for table in "${touched[@]}"; do
  [[ "$table" =~ ^[a-z_][a-z0-9_]*$ ]] ||
    fail touched "$test_name names the tables" "'$table' is no table name (or 'none' beside a table)"
  tables+=("$table")
  printf 'touched: %s\n' "$table"
done
readonly tables
if [[ "${#tables[@]}" -eq 0 ]]; then
  printf 'touched: none\n'
fi
printf 'census_table_limit: %s\n' "$limit"

sizes=()
for table in "${tables[@]}"; do
  measure "select coalesce(pg_total_relation_size(to_regclass('$table')), 0)" ||
    fail size "$table" "psql: $(<"$work/psql")"
  size_at="$(date -u +%FT%TZ)"
  size="$(<"$work/rows")"
  [[ "$size" =~ ^[0-9]+$ ]] || fail size "$table" "pg_total_relation_size answered '$size'"
  printf 'pg_total_relation_size table=%s bytes=%s at=%s\n' "$table" "$size" "$size_at"
  [[ "$size" -lt "$limit" ]] ||
    fail size "$table" "$size bytes is at or above pgmigrate.CensusTableLimit, $limit bytes"
  pass size "$table is $size bytes, below pgmigrate.CensusTableLimit ($limit)"
  sizes+=("$table $size $size_at")
done

if [[ "$measure_only" == 1 ]]; then
  printf '\nMEASURE_ONLY=1: no pull request body was read. The lines for the body, as this run read them:\n\n'
  printf -- '- measurement documents_holding_an_ask=%s at=%s\n' "$documents" "$documents_at"
  printf -- '- measurement standing_approvals_over_those_documents=%s at=%s\n' "$approvals" "$approvals_at"
  printf -- '- measurement colliding_block_ids=%s at=%s\n' "$collisions" "$collisions_at"
  for reading in "${sizes[@]}"; do
    read -r table size size_at <<<"$reading"
    printf -- '- pg_total_relation_size table=%s bytes=%s at=%s\n' "$table" "$size" "$size_at"
  done
  printf '\n'
  skip body "the pull request body: MEASURE_ONLY=1 read none, so this run does not pass c-attribute-removal-count"
  exit 0
fi

# The pull request body: the file PR_BODY_FILE names, or pull request #4173's, through gh where it
# is installed and signed in, and otherwise through GitHub's public REST API, which answers an
# unsigned request for a public repository's pull request.
if [[ -n "${PR_BODY_FILE:-}" ]]; then
  [[ -f "$PR_BODY_FILE" && -r "$PR_BODY_FILE" ]] ||
    fail body "read the pull request body" "PR_BODY_FILE names $PR_BODY_FILE, which cannot be read"
  tr -d '\r' <"$PR_BODY_FILE" >"$work/body.md"
  body_source="the file $PR_BODY_FILE"
else
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    gh pr view 4173 --repo cybermonklabs/legion --json body --jq .body \
      >"$work/body.raw" 2>"$work/gh" || fail body "read the pull request body" "gh: $(<"$work/gh")"
  else
    curl -fsS https://api.github.com/repos/cybermonklabs/legion/pulls/4173 \
      >"$work/pull.json" 2>"$work/curl" || fail body "read the pull request body" "curl: $(<"$work/curl")"
    jq -r .body "$work/pull.json" >"$work/body.raw" 2>"$work/jq" ||
      fail body "read the pull request body" "jq read no body from GitHub's answer: $(<"$work/jq")"
  fi
  tr -d '\r' <"$work/body.raw" >"$work/body.md"
  body_source="pull request #4173"
fi
body_text="$(tr -d '[:space:]' <"$work/body.md")"
[[ -n "$body_text" && "$body_text" != null ]] ||
  fail body "read the pull request body" "$body_source holds no body: a reading nobody recorded is not a reading that passed"
pass body "read the body from $body_source"

# body_line SECTION WHAT LEAD FORM SHAPE: the one line of the body that begins LEAD, which must
# match the extended regular expression SHAPE; FORM is the line's form as a failure names it. The
# groups of SHAPE are left in BASH_REMATCH.
body_line() {
  local -a lines
  mapfile -t lines < <(grep -E -- "^[[:space:]]*$3" "$work/body.md" || true)
  [[ "${#lines[@]}" -gt 0 ]] || fail "$1" "$2" "the body holds no line \"$4\""
  [[ "${#lines[@]}" -eq 1 ]] ||
    fail "$1" "$2" "the body holds ${#lines[@]} lines for it, want one: $(printf '%s / ' "${lines[@]}")"
  [[ "${lines[0]}" =~ ^[[:space:]]*$5[[:space:]]*$ ]] ||
    fail "$1" "$2" "the body's line is not \"$4\": ${lines[0]}"
}

# body_measurement NAME COUNT: the body records measurement NAME as COUNT, with its time.
body_measurement() {
  local recorded at
  body_line body "$1" "- measurement $1=" "- measurement $1=<n> at=<YYYY-MM-DDTHH:MM:SSZ>" \
    "- measurement $1=([0-9]+) at=($time_pattern)"
  recorded="${BASH_REMATCH[1]}"
  at="${BASH_REMATCH[2]}"
  date -u -d "$at" +%s >/dev/null 2>&1 || fail body "$1" "the body's time $at is no time"
  [[ "$recorded" == "$2" ]] || fail body "$1" "the body records $recorded, and this run read $2"
  pass body "$1=$2 is recorded, taken at $at"
}

body_measurement documents_holding_an_ask "$documents"
body_measurement standing_approvals_over_those_documents "$approvals"
body_measurement colliding_block_ids "$collisions"
for table in "${tables[@]}"; do
  body_line body "pg_total_relation_size $table" "- pg_total_relation_size table=$table " \
    "- pg_total_relation_size table=$table bytes=<n> at=<YYYY-MM-DDTHH:MM:SSZ>" \
    "- pg_total_relation_size table=$table bytes=([0-9]+) at=($time_pattern)"
  recorded="${BASH_REMATCH[1]}"
  at="${BASH_REMATCH[2]}"
  date -u -d "$at" +%s >/dev/null 2>&1 || fail body "pg_total_relation_size $table" "the body's time $at is no time"
  [[ "$recorded" -lt "$limit" ]] ||
    fail body "pg_total_relation_size $table" "the body records $recorded bytes, at or above pgmigrate.CensusTableLimit ($limit)"
  pass body "pg_total_relation_size $table=$recorded is recorded, taken at $at, below $limit"
done
if [[ "${#tables[@]}" -eq 0 ]]; then
  pass body "no migration of this child touches a table yet, so the body owes no pg_total_relation_size line"
fi
pass body "the body holds every reading this run took"
# A body read from PR_BODY_FILE is no reading of pull request #4173's body: the lines above are
# checked, and the check's run refuses this SKIP line.
if [[ -n "${PR_BODY_FILE:-}" ]]; then
  skip body "the body came from PR_BODY_FILE, not from pull request #4173, so this run does not pass c-attribute-removal-count"
fi
