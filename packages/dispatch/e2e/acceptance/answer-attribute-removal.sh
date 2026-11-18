#!/usr/bin/env bash
# dispatch://INFRA-1150's c-attribute-removal-count: the three measurements this child takes
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
# DISPATCH_AGENT_TOKEN, which it takes from DISPATCH_TOKEN. It then stops that server and runs the
# three statements of the spec's section "What this child measures before it writes a migration",
# verbatim, over the database, each in a read-only transaction, and prints
#     documents_holding_an_ask=<n> at=<UTC>
#     standing_approvals_over_those_documents=<n> at=<UTC>
#     colliding_block_ids=<n> at=<UTC>
#     collision issue=<KEY> block=<id> shape=<block_left_spec|block_in_decisions_document>
# the last once for each row the third statement reads; each time is `date -u +%FT%TZ` taken as
# its statement answered. It fails when either of the first two counts differs from the count
# the corpus holds (seed-decisions.sh's corpus_* lines), printing both. The third count may be
# anything, zero included; the corpus's own is printed beside it.
#
# It then runs TestTheTablesThisChildsMigrationsTouch in packages/envoy, requires its
# `--- PASS:` line, and prints the tables it names (`touched: <table>` each, or `touched: none`)
# and the limit it prints, pgmigrate.CensusTableLimit as this branch holds it. For each table it
# reads pg_total_relation_size over the database (0 for a table the base schema does not hold
# yet, which no migration can be held up on) and prints
#     pg_total_relation_size table=<table> bytes=<n> at=<UTC>
# A reading at or above the limit fails the run.
#
# Then the pull request body, from the file PR_BODY_FILE names or, without it,
# `gh pr list --repo cybermonklabs/legion --head legion/INFRA-1150 --json body --jq '.[0].body'`.
# An empty body fails. It must hold exactly one line of each of these forms, a carriage return
# before the line's end aside, and leading and trailing spaces allowed:
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
# readings, and exits 0; the implementer runs it to take the readings.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1, and a missing or bad input
# exits 2. It prints no SKIP line.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_answer_attribute_removal?sslmode=disable \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [PR_BODY_FILE=<file holding the body>] \
         [MEASURE_ONLY=1] bash e2e/acceptance/answer-attribute-removal.sh
       (run from packages/dispatch; DATABASE_URL names a database on this machine whose name
       begins infra_1150_, which this run drops and creates again; without PR_BODY_FILE the
       body is read with gh; MEASURE_ONLY=1 prints the body's lines and reads no body)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=seed-decisions.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-decisions.sh"

require_env DATABASE_URL
require_commands date go grep jq psql sed tr
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
seed_decisions
# Nothing writes while the statements read.
baseline_stop_server

# measure SQL: SQL's rows, `|` between columns, read in a read-only transaction, into $work/rows.
measure() {
  PGOPTIONS='-c default_transaction_read_only=on' env -u PGHOSTADDR \
    psql "$database_url" -X -At -F '|' -v ON_ERROR_STOP=1 -c "$1" >"$work/rows" 2>"$work/psql"
}

# The three statements, as the spec writes them.
read -r -d '' documents_sql <<'SQL' || true
select count(distinct k.block_artifact_id) as documents_holding_an_ask_block
from asks k
where k.block_id is not null
  and k.block_artifact_id is not null
  and k.state = 'open'
   or k.block_id is not null
  and k.block_artifact_id is not null
  and k.state in ('answered', 'resolved')
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
    select distinct block_artifact_id
    from asks
    where block_id is not null
      and block_artifact_id is not null
      and state in ('open', 'answered', 'resolved'))
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
against_corpus documents_holding_an_ask "$documents"
against_corpus standing_approvals_over_those_documents "$approvals"

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
  exit 0
fi

# The pull request body.
if [[ -n "${PR_BODY_FILE:-}" ]]; then
  [[ -f "$PR_BODY_FILE" && -r "$PR_BODY_FILE" ]] ||
    fail body "read the pull request body" "PR_BODY_FILE names $PR_BODY_FILE, which cannot be read"
  tr -d '\r' <"$PR_BODY_FILE" >"$work/body.md"
  body_source="the file $PR_BODY_FILE"
else
  require_commands gh
  gh pr list --repo cybermonklabs/legion --head legion/INFRA-1150 --json body --jq '.[0].body' \
    >"$work/body.raw" 2>"$work/gh" || fail body "read the pull request body" "gh: $(<"$work/gh")"
  tr -d '\r' <"$work/body.raw" >"$work/body.md"
  body_source="the pull request of legion/INFRA-1150"
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
