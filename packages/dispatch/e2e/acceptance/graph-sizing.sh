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
# It then writes a corpus of its own and measures the loop that serves it. Each document of that
# corpus holds one block more than `docs.deferBlockIndexBlocks`, the bound above which a write
# leaves the block index to the block-index reconciliation, so the whole corpus is owed an index
# when the window closes. The corpus is written while this driver holds the reconciliation's own
# lease, so no tick runs inside the measured window, and the two rates it prints - what that
# window wrote and what the loop then drained, each as documents and blocks a minute, each over
# the window named on its own line - are measured over one corpus.
#
# Two gates hold the loop, and the run prints the seconds each side spends on one document beside
# all three figures (dispatch://INFRA-1149/artifact/verification-md#b-c-growth at version 19).
# The first is the backlog: the loop must drain documents no slower than documents a write
# defers arrive, counted over the census's own window as the documents past the bound this
# database took, since those are the only work this loop is given, and a loop below that rate
# grows an owed set without bound. The second is the work itself: the loop must index a document
# no slower than a request indexes a document of the same size itself, within a tenth, which is
# the one comparison that answers whether the loop is a worse place for that work than the
# request was. The corpus straddles the bound by one block either way so that the two are one
# piece of work measured twice: the documents the loop drains hold the bound and one block, the
# documents the request path indexes hold the bound less one.
#
# A third figure is printed and judged by nothing: the loop's own design capacity, one batch of
# `blockIndexDocumentBatch` documents every `BlockIndexReconcileInterval`, which is 40 documents
# a minute against the 42.6 this pod drained. A margin of 7% decides no build, so it is a reading
# a reader would otherwise have to measure again, and no gate.
#
# The drain rate is not held to what the window wrote, and the run says so on a line of its own.
# A write past the bound defers the block index precisely so it does not pay for it: on this pod
# a document of 2,101 blocks whose index the write defers takes 0.309, 0.333 and 0.345 s, one of
# 1,999 blocks which that same write indexes itself takes 1.640, 1.467 and 1.557 s, and the loop
# settling a deferred document takes 1.41 s, so the deferring write spends 0.40 s a document
# where the loop spends 1.41. The loop therefore spends on each document what that write did not,
# and a window of deferring writes back to back always produces documents faster than any loop
# settles them, whatever the corpus. A gate on that comparison could not pass for the reason it
# exists, which is why the two gates above are the arrival rate and the request path.
#
# While the corpus is owed it reads the report's two `census: sizing backlog` lines, the owed count
# and the age of the oldest owed document, and fails where either is absent, since a backlog
# nobody can read is one nobody will notice.
#
# The corpus is a seeding of this database's write rate, so the run removes it on the way out,
# passed or failed (remove_corpus): its blocks land in the last 28 days of `document_blocks`,
# and left in place the projection that reads those days puts `graph_tuples` past the limit
# within a year, which would refuse the census of the next run and fail this driver on a corpus
# of its own rather than on the growth it watches. The removal takes the documents this run
# wrote under its own issue, their blocks, the nodes and tuples those blocks made and every row
# that references those documents, and nothing else; it leaves the issue, closed, and the one
# comment C2 wrote. What it cannot remove it prints, with what that leaves behind. Two runs of
# this driver over one database therefore both pass, which is how it is proved.
#
# The database every figure above comes from is this pod's own test database and never the
# deployment's. That database is seeded, so its write rate is this corpus's rather than
# production's, and a projection from a thin corpus would pass whatever production does. So the
# driver also reads production's own growth: the daily series of
# `cloudsql.googleapis.com/database/disk/bytes_used` for the Cloud SQL instance Dispatch runs on,
# from Cloud Monitoring through this pod's own Google identity (the metadata server's token, which
# is never printed, written to a file in the repository, or logged). It prints the window, the
# count of points it read and the slope in bytes a day, and it fails when the series holds no
# point, when the filter matches nothing, and when a page carries a `nextPageToken` it did not
# follow: an absent reading is a failure and never a pass, because a check an empty answer
# satisfies proves nothing. It states the slope it used and what it left out, since an instance
# filling for the first time is not organic growth.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DATABASE_URL (the database that
# server serves, which `envoy-dispatch census` reads from its own environment: no bearer reaches
# the command and no route serves it), and `envoy-dispatch` on PATH. A missing DATABASE_URL and a
# missing command are each exit 2, as every unmet input of this driver is. Two optional inputs
# belong to the production reading: PRODUCTION_WINDOW_DAYS, the days of that window, 21 unset and
# refused below 14; and PRODUCTION_DATABASE_ID, the Cloud SQL instance the series is read for,
# which is the negative control that points this check at an instance no project carries.
#
# It reads this pod's own test database and never a deployment's: it refuses a DATABASE_URL whose
# host is not loopback, and it proves the database is the one the server serves by writing a
# comment through the API and reading that row back through the DSN. Reading production's
# monitoring is a read and the one thing here that leaves this pod; it writes nothing anywhere but
# the database DATABASE_URL names.
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
       serves, on this machine. PRODUCTION_WINDOW_DAYS, at least 14, is the window the production
       reading covers, 21 unset; PRODUCTION_DATABASE_ID is the Cloud SQL instance it reads.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DATABASE_URL
require_commands awk curl envoy-dispatch jq psql
read_bearer

# The production reading's two optional inputs. A window shorter than a fortnight is no reading of
# production's growth, so it is refused by name here as every unmet input of this driver is.
readonly production_days="${PRODUCTION_WINDOW_DAYS:-21}"
if [[ ! "$production_days" =~ ^[0-9]+$ ]] || [[ "$production_days" -lt 14 ]]; then
  printf 'PRODUCTION_WINDOW_DAYS is %s; it must be a whole number of days, at least 14\n' \
    "$production_days" >&2
  usage
  exit 2
fi

readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
# shellcheck disable=SC2153 # DATABASE_URL is the caller's input, checked by name above
readonly database_url="$DATABASE_URL"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
# The limit the report is judged against, read from the Go source that declares it rather than
# from the report: a build whose limit moved must move this check with it.
readonly census_source="$script_dir/../../../envoy/internal/pgmigrate/census.go"
# The reconciliation this driver measures, each figure read from the Go source that declares it,
# as the limit above is: the bound above which a document write leaves its block index to the
# reconciliation, the batch one tick of that reconciliation reads, the lease it runs under, and
# the first key every loop lease is taken with.
readonly defer_source="$script_dir/../../../envoy/internal/dispatch/docs/reference_excerpts.go"
readonly loop_source="$script_dir/../../../envoy/internal/dispatch/docs/block_index.go"
readonly lease_source="$script_dir/../../../envoy/internal/dispatch/duty/exclusive.go"
# The Cloud SQL instance production's Dispatch runs on and the metric its disk use is published
# under. Both are named in dispatch://INFRA-1149 and neither is a secret, so both stand here in a
# public repository.
readonly production_project=theorem-agent-infra
readonly production_metric=cloudsql.googleapis.com/database/disk/bytes_used
readonly production_database_id="${PRODUCTION_DATABASE_ID:-theorem-agent-infra:dispatch-60a4923}"
# The most pages of that series this driver reads. A daily series of a few weeks is one page; the
# bound is here so a series that does not end is a failure naming the token it did not follow,
# rather than a loop.
readonly production_pages=20
# Where this pod's own Google credential comes from: the metadata server, and no broker.
readonly metadata_token_url="http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
# The corpus the reconciliation is measured over: documents enough for one tick to carry the whole
# batch, so the drain measured is one pass's and not a queue of passes a wait apart.
readonly drain_documents=8
# How long the session holding the block-index lease sleeps, the polls this driver waits for that
# lease (a fifth of a second apart) and the seconds it waits for the loop to drain the corpus. The
# sleep is a hang guard alone: release_block_index interrupts it as soon as the window closes.
readonly lease_seconds=1800
readonly lease_polls=1500
readonly drain_wait=600
# The tenth the second gate allows: the loop may spend up to a tenth more on one document than a
# request that indexes a document of the same size itself, and no more. The two are one piece of
# work measured twice on one machine, so the margin covers this pod moving a byte-identical write
# and nothing else.
readonly inline_tolerance=1.1
# The slope of production's daily series, read by awk out of the "<epoch> <timestamp> <bytes>"
# lines the pages above are reduced to. It prints the least-squares slope over the whole window
# and the one this check uses, which is the slope over the longest run of days whose day-over-day
# growth is at most that window median: an instance filling for the first time is a run of days
# far above that median, and so is a rise nobody has explained, and neither is organic growth.
# What it leaves out it prints as the stretches of days it dropped, each with what they added.
# shellcheck disable=SC2016 # $1, $2 and $3 are awk's fields, not this shell's arguments
readonly production_slope_program='
BEGIN { n = 0 }
{ at[n] = $1; iso[n] = $2; bytes[n] = $3 + 0; n++ }
function slope(from, to,   i, count, sx, sy, sxx, sxy, x) {
  count = to - from + 1
  for (i = from; i <= to; i++) {
    x = (at[i] - at[from]) / 86400
    sx += x; sy += bytes[i]; sxx += x * x; sxy += x * bytes[i]
  }
  if (count * sxx - sx * sx == 0) return 0
  return (count * sxy - sx * sy) / (count * sxx - sx * sx)
}
function gib(b) { return sprintf("%.2f GiB", b / 1073741824) }
END {
  if (n < 2) { print "a slope needs two points and this series holds " n; exit 1 }
  for (i = 0; i < n - 1; i++) { change[i] = bytes[i + 1] - bytes[i]; sorted[i] = change[i] }
  for (i = 1; i < n - 1; i++) {
    hold = sorted[i]
    for (j = i - 1; j >= 0 && sorted[j] > hold; j--) sorted[j + 1] = sorted[j]
    sorted[j + 1] = hold
  }
  half = int((n - 1) / 2)
  median = ((n - 1) % 2 == 1) ? sorted[half] : (sorted[half - 1] + sorted[half]) / 2
  best_from = 0; best_to = -1; run = -1
  for (i = 0; i < n - 1; i++) {
    if (change[i] <= median) {
      if (run < 0) run = i
      if (i - run >= best_to - best_from) { best_from = run; best_to = i }
    } else run = -1
  }
  printf "window_slope=%.0f\n", slope(0, n - 1)
  printf "window_slope_text=%s\n", gib(slope(0, n - 1))
  printf "used_slope=%.0f\n", slope(best_from, best_to + 1)
  printf "used_slope_text=%s\n", gib(slope(best_from, best_to + 1))
  printf "used_from=%s\n", substr(iso[best_from], 1, 10)
  printf "used_to=%s\n", substr(iso[best_to + 1], 1, 10)
  printf "used_points=%d\n", best_to + 2 - best_from
  printf "median_change=%.0f\n", median
  left = ""; count = 0; open = 0
  for (i = 0; i < n - 1; i++) {
    if (i >= best_from && i <= best_to) continue
    count++
    if (open == 0) { open = 1; from = i; total = 0 }
    total += change[i]
    if (i == n - 2 || (i + 1 >= best_from && i + 1 <= best_to)) {
      left = left (left == "" ? "" : ", ") substr(iso[from], 1, 10) ".." substr(iso[i + 1], 1, 10) \
        " +" gib(total) " over " (i + 1 - from) " day(s)"
      open = 0
    }
  }
  printf "excluded_days=%d\n", count
  printf "excluded=%s\n", (count == 0 ? "nothing" : left)
}
'
session="graph-sizing-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
lease_pid=""
cleanup() {
  local code=$?
  release_block_index
  if [[ -n "$key" ]]; then
    send PATCH "/api/v1/issues/$key" \
      "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] ||
      printf 'graph-sizing.sh: could not close %s: HTTP %s\n' "$key" "$status" >&2
    remove_corpus
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

# remove_corpus deletes the documents this run wrote under its own issue, the blocks they hold
# and the store rows those blocks made. It runs in cleanup, so a run that failed half way leaves
# no more than one that passed. The corpus is what makes this driver repeatable or not: its
# blocks land in the census's own window, and left in place they are read by the next run as the
# growth this check exists to catch, which would be a false refusal in the one check whose
# verdict has to be believed.
#
# It deletes nothing it did not write, in three steps. The tuples of the corpus's nodes go with
# the rows that hold its documents, in one transaction: every table that references artifacts
# without `on delete cascade` is read out of the catalogue rather than listed here, so a table
# added later is deleted from rather than silently blocking the delete. A vacuum follows,
# because `document_blocks` references `graph_nodes` by address with no index on that column,
# and the foreign key behind the last step reads that table once for each node: over the dead
# rows the first step leaves, that read is a quarter of an hour, and after the vacuum it is
# seconds. The nodes go last, since a node cannot go while the block row naming it is there.
#
# It refuses nothing and ends nothing: a failure here is printed with what it leaves, because a
# driver that half-cleans is worse than one that says what it left behind.
remove_corpus() {
  local counted removed
  counted="$(env -u PGHOSTADDR psql "$database_url" -Atq -v ON_ERROR_STOP=1 2>"$work/psql" <<SQL
select (select count(*) from artifacts where issue_key = '$key'),
       (select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id
          where a.issue_key = '$key')
SQL
  )" || {
    printf 'graph-sizing.sh: could not read the corpus it wrote under %s: %s\n' \
      "$key" "$(head -c 300 "$work/psql")" >&2
    return 0
  }
  [[ "$counted" != '0|0' ]] || return 0
  removed="$(env -u PGHOSTADDR psql "$database_url" -Atq -v ON_ERROR_STOP=1 2>"$work/psql" <<SQL
begin;
create temporary table corpus_documents as select id from artifacts where issue_key = '$key';
create temporary table corpus_nodes as
  select node_kind as kind, node_id as id from document_blocks
   where artifact_id in (select id from corpus_documents)
  union
  select kind, id from graph_nodes where id in (select id::text from corpus_documents);
-- A temporary table the planner has no statistics for is read as a handful of rows, and both
-- deletes below then run as nested loops over the whole of graph_tuples and graph_nodes: 14
-- minutes and 2 against the 2 seconds an index and one analyze buy.
create index on corpus_nodes (kind, id);
analyze corpus_documents;
analyze corpus_nodes;
select (select count(*) from corpus_documents), (select count(*) from corpus_nodes),
       (select count(*) from graph_tuples t join corpus_nodes n
          on (t.from_kind = n.kind and t.from_id = n.id) or (t.to_kind = n.kind and t.to_id = n.id));
delete from graph_tuples t using corpus_nodes n
 where (t.from_kind = n.kind and t.from_id = n.id) or (t.to_kind = n.kind and t.to_id = n.id);
do \$\$
declare reference record;
begin
  for reference in
    select c.conrelid::regclass as owner, a.attname as column_name
      from pg_constraint c
      join pg_attribute a on a.attrelid = c.conrelid and a.attnum = c.conkey[1]
     where c.contype = 'f' and c.confrelid = 'artifacts'::regclass
       and c.confdeltype = 'a' and array_length(c.conkey, 1) = 1
  loop
    execute format('delete from %s where %I in (select id from corpus_documents)',
                   reference.owner, reference.column_name);
  end loop;
end \$\$;
delete from artifacts where id in (select id from corpus_documents);
commit;
-- The dead rows the deletes above left, so the foreign key read behind the last step passes
-- over them rather than through them. It is outside a transaction, as vacuum must be.
vacuum document_blocks, block_refs, comments;
-- The nodes last: document_blocks names its block node, so a node goes only once the row that
-- names it is gone with its document.
begin;
delete from graph_nodes g using corpus_nodes n where g.kind = n.kind and g.id = n.id;
commit;
SQL
  )" || {
    printf 'graph-sizing.sh: could not remove the corpus it wrote under %s: %s\n' \
      "$key" "$(head -c 300 "$work/psql")" >&2
    printf 'graph-sizing.sh: it leaves %s document(s) and %s block(s) of its own in this database, which a later run of this driver reads as growth\n' \
      "${counted%%|*}" "${counted##*|}" >&2
    return 0
  }
  printf 'graph-sizing.sh: removed the corpus it wrote under %s: %s document(s), %s block(s), %s node(s) and %s tuple(s), with every row that references those documents\n' \
    "$key" "${counted%%|*}" "${counted##*|}" \
    "$(echo "$removed" | cut -d'|' -f2)" "$(echo "$removed" | cut -d'|' -f3)"
  printf 'graph-sizing.sh: it leaves %s itself, closed, and the one comment C2 wrote with that comment own node and tuples, which no run of this driver grows; the spec this run wrote with the issue went with the corpus, as a document of that issue\n' "$key"
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

# want_line_in SECTION WHAT FILE PATTERN: FILE holds a line matching the extended regex PATTERN,
# and the check prints the line it read, since the figures on these lines are the reading.
want_line_in() {
  line="$(grep -E -m 1 -- "$4" "$3")" || fail "$1" "$2" "no line matching /$4/ in $(basename "$3")"
  pass "$1" "$2: $line"
}

# declared SECTION WHAT FILE SED: the one value SED prints out of a Go source file, refused when
# the file or the declaration is gone. Every figure this driver holds the loop to is read from the
# source that declares it, so a build that moved one moves this check with it.
declared() {
  [[ -r "$3" ]] || fail "$1" "$2" "$3 is unreadable"
  answer="$(sed -n "$4" "$3")"
  [[ -n "$answer" ]] || fail "$1" "$2" "no such declaration in $3"
}

# owed SECTION WHAT: how many documents the block-index reconciliation still owes an index. It is
# the read cmd/dispatch/graph_cost_test.go's costBlockIndexDrained takes, over the column migration
# 0122's trigger leaves null for a write that deferred its blocks.
owed() {
  query "$1" "$2" 'select count(*) from document_block_versions where version is null'
}

# hold_block_index SECTION: takes the block-index pass's own lease, so that no tick of the server's
# runs inside the window this driver measures: duty.Exclusive takes this lease before every pass
# and skips the pass where another session holds it. It is costHoldBlockIndex (graph_cost_test.go)
# in shell. The lease is a session lock, so a psql of this driver's holds it until
# release_block_index interrupts that session's sleep, which gives the lease back at once.
hold_block_index() {
  local polls=0
  : >"$work/lease"
  env -u PGHOSTADDR psql "$database_url" -Atq -o /dev/null >/dev/null 2>"$work/lease" <<SQL &
select pg_advisory_lock($lease_namespace, hashtext('$lease_name'));
\\warn the $lease_name lease is held
select pg_sleep($lease_seconds);
SQL
  lease_pid=$!
  until grep -q "the $lease_name lease is held" "$work/lease"; do
    polls=$((polls + 1))
    [[ "$polls" -le "$lease_polls" ]] ||
      fail "$1" "hold the $lease_name lease" \
        "it was not given up inside $((lease_polls / 5))s: $(head -c 300 "$work/lease")"
    kill -0 "$lease_pid" 2>/dev/null ||
      fail "$1" "hold the $lease_name lease" "psql stopped: $(head -c 300 "$work/lease")"
    sleep 0.2
  done
  pass "$1" "this driver holds the $lease_name lease, so no tick runs inside the measured window"
}

# release_block_index gives that lease back: SIGINT makes psql cancel its sleep and read the end of
# its own input, which closes the session and frees the lock there and then. A kill of the sleeping
# psql would instead leave the backend holding the lease until its sleep returned.
release_block_index() {
  [[ -n "$lease_pid" ]] || return 0
  kill -INT "$lease_pid" 2>/dev/null || true
  wait "$lease_pid" 2>/dev/null || true
  lease_pid=""
}

# a_minute COUNT SECONDS: COUNT a minute, at two decimals, as every rate this driver prints.
a_minute() {
  awk -v count="$1" -v seconds="$2" \
    'BEGIN { printf("%.2f", seconds > 0 ? count * 60 / seconds : 0) }'
}

# since STARTED [FINISHED]: the seconds between two `date +%s.%N` readings, at two decimals.
since() {
  awk -v from="$1" -v to="${2:-$(date +%s.%N)}" 'BEGIN { printf "%.2f", to - from }'
}

# google_identity SECTION: this pod's own Google credential, from the metadata server, which no
# broker and no account key is part of. The credential reaches a file of this run alone, which
# curl is given as it is given the agent bearer's; it never reaches the output, a file of this
# repository, or a log, and the check below states its length and its first characters alone.
google_identity() {
  local status
  status="$(curl -sS -m 20 -H 'Metadata-Flavor: Google' -o "$work/metadata" -w '%{http_code}' \
    "$metadata_token_url" 2>"$work/curl")" ||
    fail "$1" "read this pod own Google identity" "curl: $(<"$work/curl")"
  [[ "$status" == 200 ]] ||
    fail "$1" "read this pod own Google identity" "the metadata server answered HTTP $status"
  jq -er '.access_token | select(length > 0)' "$work/metadata" >"$work/google-token" ||
    fail "$1" "read this pod own Google identity" "the metadata server answer carries no access_token"
  { printf 'Authorization: Bearer '; cat "$work/google-token"; } >"$work/google-auth"
  pass "$1" "the metadata server gave this pod its own access token, $(($(wc -c <"$work/google-token") - 1)) characters beginning $(head -c 4 "$work/google-token")"
}

# read_production_series SECTION: every page of the daily series, as `<epoch> <timestamp> <bytes>`
# lines in $work/points, with the series the filter matched in $production_series and the pages
# read in $production_read_pages. It follows every nextPageToken it is handed and fails on one it
# did not follow: a reading short of the series is a reading of something else.
read_production_series() {
  local page="" status pages=0
  : >"$work/points"
  production_series=0
  while :; do
    local -a paging=()
    [[ -z "$page" ]] || paging=(--data-urlencode "pageToken=$page")
    status="$(curl -sS -m 60 -G -H "@$work/google-auth" \
      --data-urlencode "filter=metric.type=\"$production_metric\" AND resource.labels.database_id=\"$production_database_id\"" \
      --data-urlencode "interval.startTime=$production_start" \
      --data-urlencode "interval.endTime=$production_end" \
      --data-urlencode 'aggregation.alignmentPeriod=86400s' \
      --data-urlencode 'aggregation.perSeriesAligner=ALIGN_MEAN' \
      "${paging[@]}" -o "$work/series" -w '%{http_code}' \
      "https://monitoring.googleapis.com/v3/projects/$production_project/timeSeries" 2>"$work/curl")" ||
      fail "$1" "read production disk use" "curl: $(<"$work/curl")"
    [[ "$status" == 200 ]] ||
      fail "$1" "read production disk use" \
        "Cloud Monitoring answered HTTP $status: $(head -c 300 "$work/series")"
    production_series=$((production_series + $(jq -r '(.timeSeries // []) | length' "$work/series")))
    jq -r '.timeSeries[]?.points[]? | "\(.interval.endTime | fromdateiso8601) \(.interval.endTime) \(.value.doubleValue // .value.int64Value)"' \
      "$work/series" >>"$work/points" ||
      fail "$1" "read production disk use" \
        "the answer carries no point this driver can read: $(head -c 300 "$work/series")"
    pages=$((pages + 1))
    page="$(jq -r '.nextPageToken // ""' "$work/series")"
    [[ -n "$page" ]] || break
    [[ "$pages" -lt "$production_pages" ]] ||
      fail "$1" "follow every page of production series" \
        "page $pages carried a nextPageToken, and this driver reads at most $production_pages pages, so this reading is short of the series"
  done
  production_read_pages="$pages"
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

# C10: the corpus the reconciliation must drain, and the rate the window that wrote it ran at.
# Every figure the corpus is built to is read from the Go source that declares it.
declared C10 "read the block index's deferral bound" "$defer_source" \
  's/^const deferBlockIndexBlocks = \([0-9]*\)$/\1/p'
defer_blocks="$answer"
declared C10 "read one tick's document batch" "$loop_source" \
  's/^\tblockIndexDocumentBatch *= *\([0-9]*\)$/\1/p'
document_batch="$answer"
declared C10 "read the reconciliation's interval" "$loop_source" \
  's/^\tBlockIndexReconcileInterval = \([0-9]*\) \* time\.Second$/\1/p'
reconcile_seconds="$answer"
declared C10 "read the block index's lease" "$loop_source" \
  's/^const BlockIndexLease = "\(.*\)"$/\1/p'
lease_name="$answer"
declared C10 "read the first key of a loop lease" "$lease_source" \
  's/^const leaseNamespace int32 = \(0x[0-9A-Fa-f]*\)$/\1/p'
lease_namespace=$(( answer ))
[[ "$drain_documents" -le "$document_batch" ]] ||
  fail C10 "one tick carries the whole corpus" \
    "this driver writes $drain_documents documents and one tick reads $document_batch"
pass C10 "a write defers above $defer_blocks block(s); one tick reads $document_batch document(s) under the $lease_name lease"

hold_block_index C10
owed C10 "count the documents owed an index before the corpus"
owed_before="$answer"

# The documents, written by write_document: a heading, a paragraph and one list, so a document's
# blocks are those three and one for each item. The corpus straddles the deferral bound by one
# block either way - one block above it the write defers the index, one block below it the write
# pays for it in its own request - so the two windows C12 compares are one piece of work measured
# twice and not two sizes of work.
write_document() { # BLOCKS FILE
  {
    printf '# The corpus the block-index reconciliation drains\n\n'
    printf 'Each item below is a block of this document.\n\n'
    seq 1 "$(($1 - 3))" | sed 's/^/- item /'
  } >"$2"
}
write_document "$((defer_blocks + 1))" "$work/document.md"
write_document "$((defer_blocks - 1))" "$work/inline.md"
write_started="$(date +%s.%N)"
for index in $(seq 1 "$drain_documents"); do
  jq -cn --arg name "sizing-drain-$index.md" --rawfile content "$work/document.md" --arg s "$session" \
    '{name: $name, content: $content, actor: {kind: "session", id: $s}}' >"$work/document.json"
  send POST "/api/v1/issues/$key/artifacts" "@$work/document.json"
  want_status 201 C10 "write document $index of the corpus"
done
write_seconds="$(since "$write_started")"
owed C10 "count the documents owed an index after the corpus"
[[ "$answer" == "$((owed_before + drain_documents))" ]] ||
  fail C10 "every document of the corpus is owed an index" \
    "$answer document(s) are owed, want the $owed_before of before and this run's $drain_documents: a write that indexed its own blocks is no corpus for the reconciliation"
pass C10 "the window wrote $drain_documents document(s) in ${write_seconds}s, every one of them owed an index"

# C11: the backlog the report states while that corpus is owed. The two lines are the whole of
# what somebody holding the database can read about this loop's debt, so an absent one is a
# failure: a backlog nobody can read is one nobody will notice.
census C11 "run the sizing census over the owed corpus" "$work/backlog"
[[ "$census_code" == 0 ]] ||
  fail C11 "run the sizing census over the owed corpus" \
    "envoy-dispatch census --store --sizing exited $census_code: $(cat "$work/backlog")"
for report in report backlog; do
  want_line_in C11 "the owed count in the $report" "$work/$report" \
    '^census: sizing backlog owed_documents=[0-9]+ dated_by=.+$'
  want_line_in C11 "the oldest owed document in the $report" "$work/$report" \
    '^census: sizing backlog oldest_owed_at=[^ ]+ oldest_owed_age_seconds=[0-9]+\.[0-9]+ against=[^ ]+$'
done
backlog_owed="$(sed -n 's/^census: sizing backlog owed_documents=\([0-9]*\) .*$/\1/p' "$work/backlog")"
[[ "$backlog_owed" == "$((owed_before + drain_documents))" ]] ||
  fail C11 "the report counts the corpus as owed" \
    "the report says $backlog_owed document(s) are owed where the database holds $((owed_before + drain_documents))"
backlog_oldest="$(sed -n 's/^census: sizing backlog oldest_owed_at=\([^ ]*\) .*$/\1/p' "$work/backlog")"
[[ "$backlog_oldest" != none ]] ||
  fail C11 "the report dates the oldest owed document" \
    "the report reads oldest_owed_at=none while $backlog_owed document(s) are owed"
pass C11 "the report owes $backlog_owed document(s), the oldest since $backlog_oldest"

# C12: the drain. The lease goes back, the loop takes it, and the window measured is the loop's
# own: from the first document it settled to the last, as cmd/dispatch's writeCostTick measures a
# tick. The wait before it is the reconciliation's interval and no part of its rate.
release_block_index
released="$(date +%s.%N)"
drain_started=""
drain_finished=""
while :; do
  owed C12 "count the documents still owed an index"
  left="$answer"
  now="$(date +%s.%N)"
  if [[ "$left" == "$owed_before" ]]; then
    drain_finished="$now"
    break
  fi
  [[ -n "$drain_started" || "$left" -ge "$((owed_before + drain_documents))" ]] || drain_started="$now"
  awk -v from="$released" -v to="$now" -v limit="$drain_wait" 'BEGIN { exit !(to - from <= limit) }' ||
    fail C12 "the loop drains the corpus" \
      "$((left - owed_before)) document(s) of the corpus were still owed an index ${drain_wait}s after the lease was given back"
  sleep 0.2
done
[[ -n "$drain_started" ]] ||
  fail C12 "the loop drains the corpus" \
    "the loop settled the whole corpus between two polls a fifth of a second apart, so this reading would be of the poll and not of the drain"
drain_seconds="$(since "$drain_started" "$drain_finished")"
query C12 "count the blocks the loop wrote" \
  "select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id
     where a.issue_key = '$key' and a.name like 'sizing-drain-%'"
drain_blocks="$answer"
[[ "$drain_blocks" -gt "$((defer_blocks * drain_documents))" ]] ||
  fail C12 "the corpus is past the deferral bound" \
    "the loop indexed $drain_blocks block(s) over $drain_documents document(s), which is not more than $defer_blocks each"
pass C12 "the window wrote $drain_documents document(s) and $drain_blocks block(s) over the ${write_seconds}s it held the lease: $(a_minute "$drain_documents" "$write_seconds") documents a minute, $(a_minute "$drain_blocks" "$write_seconds") blocks a minute"
pass C12 "the loop drained the same $drain_documents document(s) and $drain_blocks block(s) over the ${drain_seconds}s from its first settled document to its last: $(a_minute "$drain_documents" "$drain_seconds") documents a minute, $(a_minute "$drain_blocks" "$drain_seconds") blocks a minute (it began $(since "$released" "$drain_started")s after the lease went back, which is the reconciliation's interval and no part of this rate)"
# The two rates above are not held to each other, and this line says why rather than leaving a
# reader to work it out: a write past the bound defers the block index precisely so it does not
# pay for it, so the loop spends on each document what that write did not, and a window of such
# writes back to back always produces documents faster than any loop settles them. The seconds
# each side spends on one document are printed, because they are the whole of the reason.
pass C12 "a deferring write spends $(awk -v s="$write_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s on one of these documents where the loop spends $(awk -v s="$drain_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s, which is the deferral working: the write leaves the index to the loop, so a burst of such writes always outruns it and is no rate a deployment sustains"
# The loop's own design capacity, a reading and no gate: one tick reads a batch of
# blockIndexDocumentBatch documents and the next comes BlockIndexReconcileInterval later, so the
# loop is built to settle that many documents in that long. A margin this narrow decides no
# build, which is why it is printed and not judged.
pass C12 "the loop is built to settle $document_batch document(s) every ${reconcile_seconds}s, which is $(a_minute "$document_batch" "$reconcile_seconds") documents a minute against the $(a_minute "$drain_documents" "$drain_seconds") it drained: a reading of how near its own ceiling the loop ran, and no gate"
# The first gate. The work this loop owes is documents a write deferred, and no other write, so
# the rate work arrives at is the rate documents above the bound were written at over the
# census's own window. A loop below that rate grows an owed set without bound.
window_days="$(sed -n 's/^census: sizing window_days=\([0-9]*\) .*$/\1/p' "$work/report")"
[[ -n "$window_days" ]] ||
  fail C12 "read the window the write rate is measured over" "no window_days line in the report"
query C12 "count the documents a write deferred over that window" \
  "select count(*) from artifacts a
     where a.created_at > now() - make_interval(days => $window_days)
       and (select count(*) from document_blocks b where b.artifact_id = a.id) > $defer_blocks"
deferred_arrivals="$answer"
# A rate this low is nothing at two decimals, so this one figure carries five.
arrival_a_minute="$(awk -v count="$deferred_arrivals" -v days="$window_days" \
  'BEGIN { printf("%.5f", count / (days * 1440)) }')"
awk -v drained="$(a_minute "$drain_documents" "$drain_seconds")" -v arriving="$arrival_a_minute" \
  'BEGIN { exit !(drained >= arriving) }' ||
  fail C12 "the loop drains no slower than deferred work arrives" \
    "the loop drains $(a_minute "$drain_documents" "$drain_seconds") documents a minute where this database took $deferred_arrivals document(s) past the $defer_blocks-block bound over $window_days days, which is $arrival_a_minute a minute"
pass C12 "the loop drains no slower than deferred work arrives: $(a_minute "$drain_documents" "$drain_seconds") documents a minute against the $deferred_arrivals document(s) past the $defer_blocks-block bound this database took over $window_days days, $arrival_a_minute a minute"

# C13: the second gate. A request that indexes a document of the same size itself is the loop's
# work done where it used to be done, so it is the one measurement that answers whether the loop
# is a worse place for that work than the request was. These documents are one block under the
# bound, so their writes index them and own them: nothing of them is owed when they are written.
inline_started="$(date +%s.%N)"
for index in $(seq 1 "$drain_documents"); do
  jq -cn --arg name "sizing-inline-$index.md" --rawfile content "$work/inline.md" --arg s "$session" \
    '{name: $name, content: $content, actor: {kind: "session", id: $s}}' >"$work/document.json"
  send POST "/api/v1/issues/$key/artifacts" "@$work/document.json"
  want_status 201 C13 "write document $index of the in-request corpus"
done
inline_seconds="$(since "$inline_started")"
owed C13 "count the documents owed an index after the in-request corpus"
[[ "$answer" == "$owed_before" ]] ||
  fail C13 "every in-request document indexed its own blocks" \
    "$((answer - owed_before)) of the $drain_documents document(s) one block under the bound are owed an index, so their writes deferred and this is no measurement of the request path"
query C13 "count the blocks those writes indexed" \
  "select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id
     where a.issue_key = '$key' and a.name like 'sizing-inline-%'"
inline_blocks="$answer"
[[ "$inline_blocks" -gt 0 ]] ||
  fail C13 "the request path indexed those documents" "their writes left no block in document_blocks"
pass C13 "the request path wrote and indexed $drain_documents document(s) of $inline_blocks block(s) over ${inline_seconds}s: $(a_minute "$drain_documents" "$inline_seconds") documents a minute, $(a_minute "$inline_blocks" "$inline_seconds") blocks a minute"
awk -v loop="$drain_seconds" -v request="$inline_seconds" -v margin="$inline_tolerance" \
  'BEGIN { exit !(loop <= request * margin) }' ||
  fail C13 "the loop indexes a document no slower than the request path does" \
    "the loop spent $(awk -v s="$drain_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s on each of $drain_documents document(s) of $((defer_blocks + 1)) blocks where a request indexing a document of $((defer_blocks - 1)) blocks spent $(awk -v s="$inline_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s, which is past the tenth this check allows"
pass C13 "the loop indexes a document no slower than the request path does, within a tenth: $(awk -v s="$drain_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s a document against the request path's $(awk -v s="$inline_seconds" -v n="$drain_documents" 'BEGIN { printf("%.2f", s / n) }')s"

# C14: production's own growth. This pod's database is seeded, so every rate above it is this
# corpus's and not production's, and a projection from a thin corpus would pass whatever
# production does. This reads the deployment's own disk use from Cloud Monitoring, through the
# pod's Google identity, and an absent reading fails rather than passes.
google_identity C14
# The window, computed in epoch seconds and never in words: `date -d 'today 00:00 -21 days'`
# reads the `-21` as a zone offset and answers a day in the future, which Cloud Monitoring
# refuses as a start after its end.
midnight=$(( $(date -u +%s) / 86400 * 86400 ))
production_end="$(date -u -d "@$midnight" +%Y-%m-%dT%H:%M:%SZ)"
production_start="$(date -u -d "@$((midnight - production_days * 86400))" +%Y-%m-%dT%H:%M:%SZ)"
read_production_series C14
[[ "$production_series" -gt 0 ]] ||
  fail C14 "production's series answers this filter" \
    "the filter matched no series: metric.type=$production_metric resource.labels.database_id=$production_database_id over $production_start..$production_end"
production_points="$(wc -l <"$work/points")"
[[ "$production_points" -gt 0 ]] ||
  fail C14 "production's series holds points" \
    "the $production_series series this filter matched hold no point over $production_start..$production_end"
[[ "$production_points" -ge 14 ]] ||
  fail C14 "production's series holds a fortnight of days" \
    "the series holds $production_points aligned daily point(s) over $production_days days, which is short of the fortnight this reading is taken over"
sort -n -u -o "$work/points" "$work/points"
awk "$production_slope_program" "$work/points" >"$work/slope" ||
  fail C14 "read a slope out of production's series" "$(head -c 300 "$work/slope")"
reading() { sed -n "s/^$1=//p" "$work/slope"; }
pass C14 "production's disk use, read as this pod from Cloud Monitoring: metric=$production_metric database_id=$production_database_id window=$production_start..$production_end days=$production_days points=$production_points pages=$production_read_pages aligned=86400s aligner=ALIGN_MEAN"
pass C14 "the whole window's slope is $(reading window_slope) bytes a day ($(reading window_slope_text)), least squares over every point it read"
pass C14 "the slope this check uses is $(reading used_slope) bytes a day ($(reading used_slope_text)), least squares over the $(reading used_points) point(s) of $(reading used_from)..$(reading used_to): the longest run of days whose growth is at most the window's median day-over-day change of $(reading median_change) bytes"
pass C14 "it leaves out $(reading excluded_days) day(s), which an instance filling for the first time and an unexplained rise are and organic growth is not: $(reading excluded)"

printf 'graph-sizing.sh: every check passed\n'
