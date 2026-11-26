#!/usr/bin/env bash
# The last step of check c-migration-size (spec requirement r-migration-size, R19): the pull
# request body read beside the log of TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit.
#
#   cd packages/dispatch && bash e2e/acceptance/migration-census-body.sh TEST_LOG PR_BODY
#
# TEST_LOG is the `go test -v` log of that test ($TMPDIR/migration-size.log), which prints, each at
# the start of its own line, one `TOUCHED: <table>` for each table this child's migration set
# touches, one `SHAPE: <table>` for each table whose shape a migration changes, and one
# `SIZE: <table> <bytes the census read> <the limit>` for each of graph_nodes and graph_tuples the
# set touches (store's requireTheIssueTreeMigrationsKeepToTheLimit). PR_BODY is the pull request
# body, as `gh pr view legion/<branch> --json body --jq .body` writes it, read once, at the head
# under test. Nothing here reads a pull request or the network itself.
#
# What it reads in the body, its R19 section:
#   - the table list, a Markdown table whose header names `Table`, `Migration`, `Recorded size
#     reading` and `Source`, one row a table, a table's name and its migration each written in
#     backticks or bare. A reading is "no recorded reading" or a size with its unit (B, bytes, kB,
#     KB, MB, GB, TB, KiB, MiB, GiB, TiB) and what it is a reading of;
#   - the two counts of the render boundary over the seeded corpus of c-old-versions-render, the
#     rows of the Markdown table whose first cell names `pre_cutover_versions` and
#     `standing_approvals`, each with its value and the time it was read;
#   - the sizing's one-year line of each table the log prints a SIZE: line of,
#     `census: sizing year table=<table> bytes_now=… bytes_at_a_year=… limit_bytes=… verdict=…`
#     (graphcensus/sizing.go), graph_tuples' among them, read again with the new term of
#     storeKinds;
#   - the inventory's new table `issue_query_marks` (R7) and new column
#     `version_renders.digest_owed` (R15).
#
# What it asserts, each a `PASS: <section> <what>` line or the first `FAIL: <section> <what>:
# <observed>`, exit 1:
#   log     the log holds the test's own --- PASS line, TOUCHED: and SHAPE: of comments and
#           document_block_versions, and TOUCHED: events. Each SIZE: line names the limit
#           pgmigrate.CensusTableLimit holds in packages/envoy/internal/pgmigrate/census.go;
#   tables  every TOUCHED: table is a row of the table list, printed with its migration, its
#           recorded reading and that reading's source, a reading with no source failing; a table
#           missing from the list fails, naming the table; comments is listed, and events is
#           listed with a recorded reading;
#   limit   no SHAPE: table's recorded reading stands at or above pgmigrate.CensusTableLimit, unless
#           this branch raises that constant beside it: its value at this checkout above its value
#           at BASELINE_SHA, the commit the pull request body names, which a run must then set;
#   sizes   each SIZE: line printed beside the one-year line the body records of that table, with
#           how far the census reading stands below the limit; a table with no such line fails;
#   counts  the two counts of the render boundary, each printed with the time it was read; a count
#           the body does not record, or records as zero, fails;
#   inventory the body names issue_query_marks and version_renders.digest_owed.
# Its last line is `PASS: migration-census-body.sh complete 6 sections`.
#
# Inputs: the two files; BASELINE_SHA only where a recorded reading stands at or above the limit.
# Each unmet input is exit 2.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && bash e2e/acceptance/migration-census-body.sh TEST_LOG PR_BODY
       (TEST_LOG: the go test -v log of TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit;
       PR_BODY: the pull request body; BASELINE_SHA: the commit the body names, read only when a
       recorded reading of a SHAPE: table stands at or above pgmigrate.CensusTableLimit)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[[ $# -eq 2 ]] || {
  usage
  exit 2
}
require_commands awk git grep sed
readonly driver=migration-census-body.sh
readonly test_log="$1" body_file="$2"
for input in "$test_log" "$body_file"; do
  [[ -r "$input" && -s "$input" ]] || {
    printf '%s: %s cannot be read or is empty\n' "$driver" "$input" >&2
    usage
    exit 2
  }
done
readonly test_name=TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit
readonly census_go=packages/envoy/internal/pgmigrate/census.go
checkout="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)"
readonly checkout

# The shapes a value of the limit and a recorded reading are read in: an expression of whole
# numbers, and a size with its unit.
readonly whole_expression='^[0-9 <>*+()]+$'
readonly sized_reading='([0-9][0-9,]*(\.[0-9]+)?)[[:space:]]*(bytes|B|kB|KB|MB|GB|TB|KiB|MiB|GiB|TiB)([^A-Za-z]|$)'

# migration_census_body_limit SOURCE: the value of `const CensusTableLimit` in the Go source
# SOURCE, a sum, product or shift of whole numbers, evaluated; it sets census_limit.
census_limit=""
migration_census_body_limit() {
  local line expression
  line="$(grep -E '^const CensusTableLimit( int64)? = ' <<<"$1")" ||
    fail limit "read pgmigrate.CensusTableLimit" "no line \`const CensusTableLimit int64 = …\` in $census_go"
  expression="${line#*= }"
  expression="${expression%%//*}"
  expression="${expression//_/}"
  [[ "$expression" =~ $whole_expression ]] ||
    fail limit "read pgmigrate.CensusTableLimit" "its value \`$expression\` is no expression of whole numbers"
  census_limit=$((expression))
}

# migration_census_body_bytes READING: the bytes READING records, its first number and unit, in
# reading_bytes; empty for a reading that records none.
reading_bytes=""
migration_census_body_bytes() {
  local number unit
  reading_bytes=""
  [[ "$1" =~ $sized_reading ]] || return 0
  number="${BASH_REMATCH[1]//,/}"
  unit="${BASH_REMATCH[3]}"
  reading_bytes="$(awk -v n="$number" -v u="$unit" 'BEGIN {
    m["bytes"] = 1; m["B"] = 1; m["kB"] = 1e3; m["KB"] = 1e3; m["MB"] = 1e6; m["GB"] = 1e9; m["TB"] = 1e12
    m["KiB"] = 1024; m["MiB"] = 1048576; m["GiB"] = 1073741824; m["TiB"] = 1099511627776
    printf "%.0f", n * m[u] }')"
}

# --- log: the test's own lines -------------------------------------------------------------------

section=log
grep -qE "^--- PASS: $test_name " "$test_log" ||
  fail "$section" "the log is a passing run of $test_name" "$test_log holds no line '--- PASS: $test_name'"
pass "$section" "the log holds the --- PASS line of $test_name"
mapfile -t touched < <(sed -nE 's/^TOUCHED: ([A-Za-z0-9_]+)$/\1/p' "$test_log")
mapfile -t shaped < <(sed -nE 's/^SHAPE: ([A-Za-z0-9_]+)$/\1/p' "$test_log")
mapfile -t sized < <(sed -nE 's/^SIZE: ([A-Za-z0-9_]+ [0-9]+ [0-9]+)$/\1/p' "$test_log")
((${#touched[@]} > 0)) ||
  fail "$section" "the log names the tables the migration set touches" "$test_log holds no TOUCHED: line"
printf 'TOUCHED: %s\n' "${touched[@]}"
((${#shaped[@]} == 0)) || printf 'SHAPE: %s\n' "${shaped[@]}"
pass "$section" "the log names ${#touched[@]} touched tables and ${#shaped[@]} whose shape a migration changes"
for wanted in "TOUCHED: comments" "SHAPE: comments" "TOUCHED: events" \
  "TOUCHED: document_block_versions" "SHAPE: document_block_versions"; do
  grep -qxF "$wanted" "$test_log" || fail "$section" "the log holds '$wanted'" "$test_log holds no such line"
done
pass "$section" "the log names comments and document_block_versions as touched and shaped, and events as touched"
migration_census_body_limit "$(<"$checkout/$census_go")"
readonly head_limit="$census_limit"
printf 'pgmigrate.CensusTableLimit at this checkout: %d bytes (%s)\n' "$head_limit" "$census_go"
for line in "${sized[@]}"; do
  read -r table bytes limit <<<"$line"
  [[ "$limit" == "$head_limit" ]] ||
    fail "$section" "the SIZE: line of $table names pgmigrate.CensusTableLimit" \
      "it names $limit, and $census_go holds $head_limit"
done
pass "$section" "${#sized[@]} SIZE: lines, each naming the limit $head_limit"

# --- tables: every touched table is listed with its reading ---------------------------------------

section=tables
declare -A listed_migration=() listed_reading=() listed_source=()
# The table list: the rows of the Markdown table whose header names the four columns, by name.
columns=""
while IFS= read -r line; do
  if [[ "$line" != \|* ]]; then
    columns=""
    continue
  fi
  IFS='|' read -r -a cells <<<"${line#|}"
  for index in "${!cells[@]}"; do
    cell="${cells[$index]}"
    cell="${cell#"${cell%%[![:space:]]*}"}"
    cell="${cell%"${cell##*[![:space:]]}"}"
    cells[index]="$cell"
  done
  if [[ -z "$columns" ]]; then
    table_at="" migration_at="" reading_at="" source_at=""
    for index in "${!cells[@]}"; do
      case "${cells[$index]}" in
        Table) table_at="$index" ;;
        Migration) migration_at="$index" ;;
        "Recorded size reading") reading_at="$index" ;;
        Source) source_at="$index" ;;
      esac
    done
    if [[ -n "$table_at" && -n "$migration_at" && -n "$reading_at" && -n "$source_at" ]]; then
      columns="$table_at $migration_at $reading_at $source_at"
    else
      columns=none
    fi
    continue
  fi
  [[ "$columns" != none ]] || continue
  [[ ! "${cells[0]}" =~ ^:?-+:?$ ]] || continue
  read -r table_at migration_at reading_at source_at <<<"$columns"
  table="${cells[$table_at]//\`/}"
  [[ "$table" =~ ^[A-Za-z0-9_.]+$ ]] || continue
  listed_migration["$table"]="${cells[$migration_at]//\`/}"
  listed_reading["$table"]="${cells[$reading_at]:-}"
  listed_source["$table"]="${cells[$source_at]:-}"
done <"$body_file"
((${#listed_migration[@]} > 0)) ||
  fail "$section" "the body lists the tables this child's migrations touch" \
    "it holds no Markdown table whose header names Table, Migration, Recorded size reading and Source"
printf 'the body lists %d tables\n' "${#listed_migration[@]}"
missing=""
for table in "${touched[@]}"; do
  if [[ -z "${listed_migration[$table]+held}" ]]; then
    missing+=" $table"
    continue
  fi
  reading="${listed_reading[$table]}"
  printf 'TABLE: %s migration=%s reading=%s source=%s\n' "$table" "${listed_migration[$table]}" \
    "${reading:-(none)}" "${listed_source[$table]:-(none)}"
  if [[ -n "$reading" && "$reading" != "no recorded reading" && -z "${listed_source[$table]}" ]]; then
    fail "$section" "the recorded reading of $table names its source" "the body records '$reading' and no source"
  fi
done
[[ -z "$missing" ]] ||
  fail "$section" "the body lists every table the migration set touches" "it lists none of:$missing"
pass "$section" "the body lists all ${#touched[@]} touched tables, each with its reading and that reading's source where one stands"
[[ -n "${listed_migration[comments]+held}" ]] ||
  fail "$section" "the body's table list holds comments" "it does not"
[[ -n "${listed_migration[events]+held}" ]] ||
  fail "$section" "the body's table list holds events" "it does not"
migration_census_body_bytes "${listed_reading[events]}"
[[ -n "$reading_bytes" ]] ||
  fail "$section" "the body records the size reading of events" \
    "its row reads '${listed_reading[events]:-}', which records no size"
pass "$section" "the body lists comments, and events with its reading of $reading_bytes bytes"

# --- limit: no shape change of a table at or above the limit, unless the branch raises it -----------

section=limit
base_limit=""
for table in "${shaped[@]}"; do
  [[ -n "${listed_migration[$table]+held}" ]] ||
    fail "$section" "the body records a reading of $table, whose shape a migration changes" "the body does not list $table"
  migration_census_body_bytes "${listed_reading[$table]}"
  if [[ -z "$reading_bytes" ]]; then
    printf 'SHAPE: %s: the body records no reading (%s)\n' "$table" "${listed_reading[$table]:-(none)}"
    continue
  fi
  printf 'SHAPE: %s: recorded reading %s bytes against the limit %s\n' "$table" "$reading_bytes" "$head_limit"
  ((reading_bytes >= head_limit)) || continue
  if [[ -z "$base_limit" ]]; then
    [[ -n "${BASELINE_SHA:-}" ]] || {
      printf '%s: %s records %s bytes for %s, at or above the limit; set BASELINE_SHA to the commit the pull request body names, to read whether this branch raises pgmigrate.CensusTableLimit\n' \
        "$driver" "$body_file" "$reading_bytes" "$table" >&2
      exit 2
    }
    migration_census_body_limit "$(git -C "$checkout" show "$BASELINE_SHA:$census_go" 2>/dev/null)"
    base_limit="$census_limit"
    printf 'pgmigrate.CensusTableLimit at BASELINE_SHA %s: %d bytes\n' "$BASELINE_SHA" "$base_limit"
  fi
  ((head_limit > base_limit)) ||
    fail "$section" "no table whose shape a migration changes stands at or above pgmigrate.CensusTableLimit" \
      "the body records $reading_bytes bytes for $table ('${listed_reading[$table]}'), at or above $head_limit, and $census_go holds $head_limit here and $base_limit at BASELINE_SHA: this branch raises no limit beside it"
  printf 'SHAPE: %s stands at or above the limit of BASELINE_SHA, %d, which this branch raises to %d\n' \
    "$table" "$base_limit" "$head_limit"
done
pass "$section" "no SHAPE: table's recorded reading stands at or above the limit this branch holds it to"

# --- sizes: each census reading beside the body's one-year projection ------------------------------

section=sizes
grep -qE '^census: sizing year table=graph_tuples .*verdict=' "$body_file" ||
  fail "$section" "the body holds the sizing's one-year line of graph_tuples" \
    "it holds no line 'census: sizing year table=graph_tuples … verdict='"
for line in "${sized[@]}"; do
  read -r table bytes limit <<<"$line"
  year="$(grep -m 1 -E "^census: sizing year table=$table " "$body_file")" ||
    fail "$section" "the body records the sizing projection of $table" \
      "it holds no line 'census: sizing year table=$table … verdict=' beside the log's SIZE: $table $bytes $limit"
  printf 'SIZE: %s %s %s, %s bytes below the limit (%s%% of it)\n  body: %s\n' "$table" "$bytes" "$limit" \
    "$((limit - bytes))" "$(awk -v b="$bytes" -v l="$limit" 'BEGIN { printf "%.4f", 100 * b / l }')" "$year"
done
pass "$section" "each of the ${#sized[@]} SIZE: tables stands beside the one-year projection the body records"

# --- counts: the render boundary's two counts ------------------------------------------------------

section=counts
for count in pre_cutover_versions standing_approvals; do
  row="$(grep -m 1 -E "^\|[[:space:]]*\`?$count\`?" "$body_file")" ||
    fail "$section" "the body records the count $count" "no row of its tables names $count"
  IFS='|' read -r -a cells <<<"${row#|}"
  value="${cells[1]:-}"
  value="${value//[[:space:],]/}"
  at="${cells[2]:-}"
  at="${at#"${at%%[![:space:]]*}"}"
  at="${at%"${at##*[![:space:]]}"}"
  [[ "$value" =~ ^[0-9]+$ ]] ||
    fail "$section" "the body records the count $count" "its row reads '${cells[1]:-}', no whole number: $row"
  ((value > 0)) ||
    fail "$section" "the count $count is above zero" "the body records it as 0 (read at ${at:-no time}): $row"
  printf 'count %s=%s at %s\n' "$count" "$value" "${at:-(no time)}"
done
pass "$section" "the body records both counts of the render boundary, each above zero"

# --- inventory: the new table and column -----------------------------------------------------------

section=inventory
for name in issue_query_marks version_renders.digest_owed; do
  grep -qF "$name" "$body_file" || fail "$section" "the body's inventory names $name" "the body names no $name"
done
pass "$section" "the body's inventory names issue_query_marks and version_renders.digest_owed"

printf 'PASS: %s complete 6 sections\n' "$driver"
