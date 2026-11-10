#!/usr/bin/env bash
# Drives the growth pass of the node and tuple store's census against a running server's own
# database (dispatch://INFRA-1149's c-growth). It runs `envoy-dispatch census --store --sizing`
# and reads every figure that check names out of the report: the write rate by kind, the row count
# and the total relation size of every table this branch's migrations touch, the tuples and nodes
# a write of each kind states, the rows a day and the bytes a row of each store table, and the
# one-year projection of `graph_tuples` and of `graph_nodes` against `pgmigrate.CensusTableLimit`,
# which this driver reads out of the Go source rather than trusting the report's own number.
#
# It proves three things about the command besides what it printed. It exits non-zero when the
# command does, which is what the command does when a store table passes the limit inside a year
# or when a table the migrations alter is already at or above it. It runs the command twice over
# one unchanged database and compares the two outputs, which must agree, since every figure in the
# report comes from a count. And it proves the command writes nothing, by reading the highest
# tuple sequence and both store tables' row counts before and after the two runs.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DATABASE_URL (the database that
# server serves, which `envoy-dispatch census` reads from its own environment: no bearer reaches
# the command and no route serves it), and `envoy-dispatch` on PATH. A missing DATABASE_URL and a
# missing command are each exit 2, as every unmet input of this driver is.
#
# It reads this pod's own test database and never a deployment's: it refuses a DATABASE_URL whose
# host is not loopback, and it proves the database is the one the server serves by writing a
# comment through the API and reading that row back through the DSN.
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/db DISPATCH_URL=http://127.0.0.1:8777 \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE graph-sizing.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the built
       `envoy-dispatch` must be on PATH, and DATABASE_URL must name the database that server
       serves, on this machine)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DATABASE_URL
require_commands curl envoy-dispatch jq psql
read_bearer

readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT and DATABASE_URL are the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$DATABASE_URL"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
# The limit the report is judged against, read from the Go source that declares it rather than
# from the report: a build whose limit moved must move this check with it.
readonly census_source="$script_dir/../../../envoy/internal/pgmigrate/census.go"
session="graph-sizing-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
cleanup() {
  local code=$?
  if [[ -n "$key" ]]; then
    send PATCH "/api/v1/issues/$key" \
      "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] ||
      printf 'graph-sizing.sh: could not close %s: HTTP %s\n' "$key" "$status" >&2
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# send METHOD PATH [JSON]: sends METHOD PATH as the agent, with the bearer in $work/auth. The
# answer's body goes to $work/body and its HTTP status to $status.
send() {
  local -a data=()
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS -X "$1" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# query SECTION WHAT SQL: one value of one row, read through DATABASE_URL. psql runs without
# PGHOSTADDR, which libpq would otherwise connect to in place of the DSN's host.
query() {
  local section="$1" what="$2" sql="$3"
  answer="$(env -u PGHOSTADDR psql "$database_url" -Atqc "$sql" 2>"$work/psql")" ||
    fail "$section" "$what" "psql: $(<"$work/psql")"
}

# want_line SECTION WHAT PATTERN: the report holds a line matching the extended regex PATTERN.
want_line() {
  grep -Eq -- "$3" "$work/report" || fail "$1" "$2" "no line matching /$3/ in the report"
  pass "$1" "$2"
}

# census SECTION WHAT FILE: runs `envoy-dispatch census --store --sizing` with DATABASE_URL and
# nothing else of this driver's, its output in FILE, and sets $census_code to its exit status.
census() {
  census_code=0
  env -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE DATABASE_URL="$database_url" \
    envoy-dispatch census --store --sizing >"$3" 2>&1 || census_code=$?
}

# C1: the inputs. The database is on this machine and is the one the server serves.
host="${database_url#*://}"
host="${host#*@}"
host="${host%%[:/]*}"
case "$host" in
  127.0.0.1 | ::1 | localhost) pass C1 "DATABASE_URL names this machine ($host)" ;;
  *) fail C1 "DATABASE_URL names this machine" "host $host is not loopback; this driver reads this pod's own test database and never a deployment's" ;;
esac
[[ -r "$census_source" ]] || fail C1 "read the census limit" "$census_source is unreadable"
limit_expression="$(sed -n 's/^const CensusTableLimit int64 = \(.*\)$/\1/p' "$census_source")"
[[ -n "$limit_expression" ]] ||
  fail C1 "read the census limit" "no CensusTableLimit in $census_source"
limit=$(( limit_expression ))
[[ "$limit" -gt 0 ]] || fail C1 "read the census limit" "CensusTableLimit reads $limit"
pass C1 "pgmigrate.CensusTableLimit is $limit bytes"

# C2: one write through the API, so the window holds a write of this run and the DSN is proved to
# be the server's own database.
send POST "/api/v1/issues" "$(jq -cn --arg p "$project" --arg s "$session" \
  '{project: $p, title: "Store sizing check", spec: "The growth pass of the store census.", actor: {kind: "session", id: $s}}')"
want_status 201 C2 "create the issue"
key="$(jq -er '.key' "$work/body")" || fail C2 "create the issue" "no key: $(body)"
pass C2 "created $key in $project"
send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg s "$session" \
  '{body: "A write of this run, so the sizing window holds one.", actor: {kind: "session", id: $s}}')"
want_status 201 C2 "write a comment"
comment="$(jq -er '.id' "$work/body")" || fail C2 "write a comment" "no id: $(body)"
query C2 "read that comment through DATABASE_URL" \
  "select count(*) from comments where id = '$comment'"
[[ "$answer" == 1 ]] ||
  fail C2 "read that comment through DATABASE_URL" \
    "the comment the server wrote is not in the database DATABASE_URL names (count $answer)"
pass C2 "DATABASE_URL is the database the server serves"

# C3: the store before the census, which must be the store after it.
query C3 "read the highest tuple sequence" "select coalesce(max(seq), 0) from graph_tuples"
tuples_before="$answer"
query C3 "count the tuples" "select count(*) from graph_tuples"
tuple_rows_before="$answer"
query C3 "count the nodes" "select count(*) from graph_nodes"
node_rows_before="$answer"
pass C3 "the store holds $tuple_rows_before tuple(s) through seq $tuples_before and $node_rows_before node(s)"

# C4: the command itself.
census C4 "run the sizing census" "$work/report"
[[ "$census_code" == 0 ]] ||
  fail C4 "run the sizing census" "envoy-dispatch census --store --sizing exited $census_code: $(cat "$work/report")"
pass C4 "envoy-dispatch census --store --sizing exited 0"

# C5: the write rate by kind, read over the last 28 days of events.
want_line C5 "the window the rate is read over" \
  "^census: sizing window_days=28 year_days=365 limit_bytes=$limit\$"
want_line C5 "the write rate by kind" '^census: sizing write-rate type=[^ ]+ per_day=[0-9]+\.[0-9]+$'

# C6: every table this branch's migrations touch, each inside the limit.
for table in graph_nodes graph_tuples graph_store_cutover graph_erasures block_refs comments document_blocks; do
  want_line C6 "the size of $table" \
    "^census: sizing table name=$table altered=(true|false) rows=[0-9,]+ bytes=[0-9]+ limit_bytes=$limit verdict=inside\$"
done
grep -E '^census: sizing table name=[^ ]+ (held=false|.*verdict=at-or-above)' "$work/report" >"$work/over" ||
  true
[[ ! -s "$work/over" ]] ||
  fail C6 "every table is held and inside the limit" "$(<"$work/over")"
pass C6 "no table of this branch is missing, at or above the limit"

# C7: what a write of each kind states, and where the two store tables reach in a year.
for kind in comment block block_ref delivery; do
  want_line C7 "the tuples a $kind write states" \
    "^census: sizing tuples-a-write kind=$kind tuples=[0-9]+ nodes=[0-9]+ rows_a_day=[0-9]+\.[0-9]+ tuples_a_day=[0-9]+\.[0-9]+ nodes_a_day=[0-9]+\.[0-9]+ driver=.+\$"
done
for table in graph_tuples graph_nodes; do
  want_line C7 "the rows a day of $table" \
    "^census: sizing rows-a-day table=$table rows_a_day=[0-9]+\.[0-9]+\$"
  want_line C7 "the bytes a row of $table" \
    "^census: sizing bytes-a-row table=$table bytes_a_row=[0-9]+ source=.+\$"
  want_line C7 "the year ahead of $table" \
    "^census: sizing year table=$table bytes_now=[0-9]+ bytes_at_a_year=[0-9]+ limit_bytes=$limit verdict=inside\$"
done
want_line C7 "the sizing's own verdict" '^census: sizing ok$'

# C8: two runs over one unchanged database print one report.
census C8 "run the sizing census again" "$work/report2"
[[ "$census_code" == 0 ]] ||
  fail C8 "run the sizing census again" "the second run exited $census_code: $(cat "$work/report2")"
diff -u "$work/report" "$work/report2" >"$work/diff" ||
  fail C8 "two runs over one unchanged database agree" "$(head -c 800 "$work/diff")"
pass C8 "two runs over one unchanged database print one report"

# C9: the command wrote nothing.
query C9 "read the highest tuple sequence again" "select coalesce(max(seq), 0) from graph_tuples"
[[ "$answer" == "$tuples_before" ]] ||
  fail C9 "the census appended no tuple" "the highest tuple sequence moved from $tuples_before to $answer"
query C9 "count the tuples again" "select count(*) from graph_tuples"
[[ "$answer" == "$tuple_rows_before" ]] ||
  fail C9 "the census removed no tuple" "graph_tuples went from $tuple_rows_before rows to $answer"
query C9 "count the nodes again" "select count(*) from graph_nodes"
[[ "$answer" == "$node_rows_before" ]] ||
  fail C9 "the census wrote no node" "graph_nodes went from $node_rows_before rows to $answer"
pass C9 "the census wrote nothing: the log's highest sequence and both tables' rows are where they were"

printf 'graph-sizing.sh: every check passed\n'
