#!/usr/bin/env bash
# Drives the one truth of a run of the command bar and of this child's reasoning nodes across an
# upgrade and a rollback (dispatch://INFRA-1950's c-census, dispatch://INFRA-1950/spec#b-r-one-truth,
# dispatch://INFRA-1950/spec#b-r-interaction). Two products of one transformation agree after both
# lose the same row, so the driver starts before the migration and holds both products to an
# inventory of its own:
#
#   B1  The base build, booted through lib-baseline.sh, which builds it from BASELINE_SHA (unset,
#       `git merge-base origin/main HEAD`) and stamps it with that commit, over the run's empty
#       database, which it migrates; the driver keeps the versions schema_migrations holds then as
#       a file. Its /healthz names that commit as its own and as its fleet_commit, the build
#       processes.Live reads every live process as serving. Through its own API three people each
#       ask the command bar, and the driver reads each run's stream to its terminal frame; the
#       server stops, so nothing appends a frame after the inventory is read.
#       With psql it writes one run row a day ahead that holds no frame, and one run for each row
#       of the identity table below, each a day ahead too and each form straight into
#       going_requests.author, since the older server's sign-in stores a login in its canonical
#       form only; so every run holds a terminal frame or an ends_at a day ahead, and no boot of
#       either build ends one by itself. It then reads the inventory - each run's id, author,
#       words and frames - out of that database and keeps it as a file.
#   H1  This head's server (DISPATCH_E2E_SERVER_BIN, stamped with its own commit) boots over the
#       same database and runs every migration it holds that the database does not record, so the
#       migrations it applied are the versions schema_migrations holds now and did not after B1,
#       20261116205055_interaction_nodes among them; its /healthz names its own commit as the
#       fleet's. The driver runs `envoy-dispatch backfill-graph-store`, waits ten minutes at most
#       until no document_block_versions row has a null version, and compares the store with the
#       inventory row by row, then going_requests and going_events the same way, naming every run
#       a side lacks.
#   H2  Through this build's routes: a pre-mortem reserved for alice with one risk prevented by a
#       check, a trade-off lens with one finding block, triggered by the pre-mortem, a constraint
#       reserved for bob, the requirement's constrained_by tuple to it, one lens_kind through
#       `envoy-dispatch lens-kind-add` (that kind's only writer), an edit of the words of an
#       existing requirement block under the header of one of alice's runs, whose ends_at psql first
#       sets a day ahead on both sides in one transaction, so the run's window decides no result,
#       and one lens on the shared bearer e2e-token whose body names a session no other row names.
#       It keeps an inventory of those writes. Before any further backfill the census reads the
#       person and the session kinds at zero differences: the shared bearer proves no author, so
#       that lens is the project's and settles nothing, yet it names the session its body names as
#       its writer through authored_by and its write stated that session's node (spec v46 R12),
#       so the session is on both sides; the driver reads that authored_by tuple.
#   B2  The base build again, as in a rollback, through run-server.sh, booted beside this head's
#       server, which still serves: two builds serve the one database at once, each on a port of
#       its own, and both sign cookies with the run's one dev sign-in key. While both serve, alice
#       proposes the requirement r3 through this head's state route, the base build's own edit
#       route writes a version of the spec that changes the words of r3's second paragraph, a block
#       of its own whose text r3's holds, so that build's index pass writes r3's node again, and
#       this head reads r3 still proposed (dispatch://INFRA-1149's keep-state rule). dave, a login
#       no other row names, writes through this head a lens reserved for him, his only node, and
#       reads his own page through this head with that lens on it; bob reads dave's page through
#       the base build and is answered, status and body byte for byte, what that build answers him
#       for a login nobody holds (dispatch://INFRA-1149's person-page rule). Then this head's
#       server stops and the base build serves alone: one new run, whose frames it writes with no
#       frame value; with psql the hand-written run's ends_at moves into the past, standing for time
#       passing, on both sides in one transaction (its row, and its node's attribute and value
#       tuple), and a third run is dated two hours back, and the driver waits ten minutes at most
#       until that build's sweep has ended the first and deleted the row of the second.
#   H3  This head again. Each boot wrote one announcement once processes.Live read every live
#       process as its build - the base build of B2, booted beside this head, once this head
#       stopped - so they name the base build, this head, the base build and this head, in order:
#       two builds. Before the backfill, a census beside a planted instance: it names the new run,
#       a row with no node, and the terminal frame of the run row written by hand, a row with no
#       frame value. Then the backfill again: a node for the new run, a frame value for each frame
#       that build appended, and the run whose row it deleted at state gone. alice moves her
#       pre-mortem to proposed, the finding to settled and her risk to overtaken. Only then the
#       census, the store-to-projection comparison, which is reported apart from the inventory's: a
#       run the migration lost shows in the first and never in the second. It reads zero
#       differences for the interaction and frame kinds, for both block projections of
#       dispatch://INFRA-1149, for person and session, and for the visibility and state of every
#       node of this child's kinds, each kind having checked more than zero rows, the moved nodes
#       counted as live ones are, and every interaction at `author`. Then, with psql, one
#       interaction node no going_requests row projects, a copy of bob's run's node under an id no
#       row names, and one frame value no going_events row projects, of the run row written by hand
#       at the sequence after its one frame's: the census reads each as one difference of its kind
#       naming its run, two in all, and once both are deleted, exactly as planted, zero again.
#   S   The store-only kinds (lens, finding, risk, constraint, lens_kind) and their constrained_by,
#       has_kind, triggered_by and prevented_by tuples against the inventory of the routes' writes
#       and the migration's seed, node for node and tuple for tuple.
#   I   The identity table against each run's made_by, its authored_by and going_requests.author.
#   G   A grep of packages/envoy/cmd/dispatch and packages/envoy/scripts for a subcommand or a file
#       whose name holds reconcile or rebuild beside interaction, lens, risk or constraint, beside
#       the same grep over a copy under $TMPDIR holding a planted rebuild-lens.sh.
#   P   For alice and for bob, the set of nodes of this child's kinds the predicate admits, read
#       through GET /api/v1/references at each node's address, against the set computed from the
#       store's columns alone.
#   M   Each .census.sql of the migrations this head applied over the base, the select the
#       repository runs before a deploy, reading 0.
#   E   With no server running, `envoy-dispatch erase --node interaction:<uuid>` of the run the edit
#       was made under, as dispatch://INFRA-1149's erase runs: a listing, then --confirm with its
#       digest. The block's changed_by_run tuple blocks nothing and goes with the node; no row and no
#       node of the run is left, the backfill writes none back, and the census reads zero again.
#
# The identity table is written into this driver by a person, and no code the server runs produces
# it: each row is a form a record can hold of a person's identity, the node its run's authored_by
# must name, and the person it resolves to. carol is the person of the table who asks the command
# bar and writes no comment; karol is one character from her and must stay a person of his own.
#
# Inputs: DATABASE_URL (an empty database on this machine that the run created and drops; every
# server here migrates and serves it, and this driver reads and writes its rows), and optionally
# BASELINE_SHA (lib-baseline.sh's: the base commit; while a branch this child builds on is unmerged,
# a commit holding every such branch), DISPATCH_E2E_SERVER_BIN (this head's envoy-dispatch, which
# serves and runs backfill-graph-store, census, lens-kind-add and erase, default
# $TMPDIR/bin/envoy-dispatch) and LEGION_WORKSPACE (lib-baseline.sh's checkout). It starts the two
# fakes run-server.sh needs, the fake Envoy and the fake GitHub, and every server (lib-processes.sh),
# and stops each when it exits, also when a check fails, so the run can drop its database. Each
# unmet input is exit 2. No BASELINE_BINARY is read: the base is always built here and stamped with
# its commit.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The first PASS line after this head's
# migrations, read before any lens is written, is
# `PASS: node-visibility-census.sh the catalogue holds its ten kinds`. The last line is
# `PASS: node-visibility-census.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/infra_1950_census?sslmode=disable \
         [BASELINE_SHA=<commit>] [DISPATCH_E2E_SERVER_BIN=$TMPDIR/bin/envoy-dispatch] \
         node-visibility-census.sh
       (DATABASE_URL names an empty database on this machine, which the run created and drops;
       BASELINE_SHA unset is git merge-base origin/main HEAD)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib-processes.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-processes.sh"
require_env DATABASE_URL
require_commands bun curl git go jq openssl psql python3 tar

readonly driver=node-visibility-census.sh
# shellcheck disable=SC2153,SC2031 # the caller's DATABASE_URL, checked by name above, not a server's
# Not readonly: lib-baseline.sh's baseline_boot takes a local of this name.
database_url="$DATABASE_URL"
require_local_database
require_server_bin
[[ -z "${BASELINE_BINARY:-}" ]] || {
  printf 'BASELINE_BINARY is set: this driver builds the base itself, stamped with its commit\n' >&2
  usage
  exit 2
}
# The bearer every server here accepts: run-server.sh boots each with e2e-token, and
# lib-baseline.sh boots the base with the run's DISPATCH_TOKEN, which it reads from this shell.
readonly token=e2e-token
# shellcheck disable=SC2034 # read by lib-baseline.sh's baseline_start
DISPATCH_TOKEN="$token"
# The tree this driver is part of, whose sources G greps and whose migrations M reads.
tree="$(cd -- "$e2e_dir/../../.." && pwd)"
readonly tree
stamp="$(date +%s)-$$"
readonly stamp
# The session every bearer write names, the session the identity table says the server proved
# carol's, and the session the lens on the shared bearer names, which no other row names.
readonly session="census-$stamp"
readonly carol_session="census-carol-$stamp"
readonly bearer_session="census-bearer-lens-$stamp"
readonly project=CENSUS
# The two seeded people, the person of the identity table, karol, a login one character from
# hers, who must stay a person of his own, dave, a login no other row names, whose only node is a
# lens reserved for him (B2), and a login nobody holds, which no row names at all.
readonly alice=alice bob=bob carol=carol twin=karol third=dave
readonly nobody="nobody-$stamp"
readonly added_kind="census-$stamp"
# How long each of the two waits below may take: the owed block versions, and the older build's
# sweep.
readonly wait_limit=600
# The words of the requirement edited under a run, before the edit and after it.
readonly edited_from="The words of this requirement are edited under a run."
readonly edited_to="The words of this requirement were edited under a run."
# The words of the second paragraph of the requirement r3, which the base build's version changes
# while both builds serve (B2), before that version and after it.
readonly paragraph_from="The words of this paragraph are changed by the older build."
readonly paragraph_to="The words of this paragraph were changed by the older build."

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The pid of this head's server while the base build serves beside it (B2): boot_server keeps the
# pid of the server it started last alone, which stop_processes stops, so this one is stopped here.
beside_pid=""
cleanup() {
  local code=$?
  baseline_stop_pid "$beside_pid"
  stop_processes
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# stop_server SECTION WHAT: stops the server this driver started last, which deletes its row of
# server_processes as its drain ends, and reads that no process of the run is registered then, so
# the next server is the only one serving the database.
stop_server() {
  baseline_stop_pid "$server_pid"
  server_pid=""
  want_value "$1" "$2: server_processes holds no row once it stopped" 0 "select count(*) from server_processes"
}

# registered_commit SECTION WHAT: the commit the one row of server_processes names, in $answer.
registered_commit() {
  query "$1" "$2" "select count(*) || '|' || coalesce(min(commit), '') from server_processes"
  [[ "$answer" == 1\|* ]] || fail "$1" "$2" "server_processes holds '$answer', want one row"
  answer="${answer#1|}"
}

# ask SECTION LOGIN WORDS: LOGIN's cookie asks the command bar WORDS of the project, and the answer
# is the stored run, whose id goes to $asked.
ask() {
  request "cookie:$2" POST /api/v1/going/requests "$(jq -cn --arg w "$3" --arg p "$project" '{words: $w, project: $p}')"
  want_status 201 "$1" "$2 asks the command bar"
  asked="$(jq -er '.id' "$work/body")" || fail "$1" "$2 asks the command bar" "no id: $(body)"
  pass "$1" "$2 asked the command bar \"$3\" (run $asked)"
}

# read_stream SECTION LOGIN RUN: LOGIN's cookie reads RUN's stream until it ends, and it holds a
# terminal frame.
read_stream() {
  status="$(curl -sS -N --max-time 60 -o "$work/stream" -w '%{http_code}' -b "$work/jar.$2" \
    "$base/api/v1/going/requests/$3/stream" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  want_status 200 "$1" "$2 reads the stream of run $3"
  grep -Eq '^event: (end|error)$' "$work/stream" ||
    fail "$1" "$2 reads the stream of run $3 to its terminal frame" "the stream holds no terminal frame: $(head -c 600 "$work/stream")"
  pass "$1" "$2 read run $3's stream to its terminal frame ($(grep -E '^event: ' "$work/stream" | tr '\n' ' '))"
}

# run_cli SECTION WHAT ARGS…: runs this head's envoy-dispatch ARGS against the run's database, its
# output in $work/cli.out and its exit status in $cli_code; a status of 2 or above, which says the
# command could not run at all, fails.
run_cli() {
  local section="$1" what="$2"
  shift 2
  cli_code=0
  env -u PGHOSTADDR DATABASE_URL="$database_url" "$server_bin" "$@" >"$work/cli.out" 2>&1 || cli_code=$?
  [[ "$cli_code" -lt 2 ]] || fail "$section" "$what" "envoy-dispatch $* exited $cli_code: $(<"$work/cli.out")"
}

# backfill SECTION: runs envoy-dispatch backfill-graph-store, which exits 0, and prints its lines.
backfill() {
  run_cli "$1" "run envoy-dispatch backfill-graph-store" backfill-graph-store
  [[ "$cli_code" == 0 ]] || fail "$1" "envoy-dispatch backfill-graph-store exits 0" "exited $cli_code: $(<"$work/cli.out")"
  sed 's/^/  /' "$work/cli.out"
  pass "$1" "envoy-dispatch backfill-graph-store ran over the run's database"
}

# take_census SECTION NAME: runs envoy-dispatch census --store and keeps its report in
# $work/census.NAME. It exits 0 or 1, by whether the report refuses; what this driver reads of it is
# each kind's line and the total of differences.
take_census() {
  run_cli "$1" "run envoy-dispatch census --store" census --store
  cp "$work/cli.out" "$work/census.$2"
  grep -E '^census: store (kind=[a-z_]+ nodes=|rows_checked=|ok|REFUSED|absent)|^census: store kind=[a-z_]+ (difference|and )' \
    "$work/census.$2" >"$work/census.$2.lines" ||
    fail "$1" "envoy-dispatch census --store prints its report" "it printed no census line (exit $cli_code): $(<"$work/census.$2")"
  sed 's/^/  /' "$work/census.$2.lines"
  pass "$1" "envoy-dispatch census --store read the store (exit $cli_code)"
}

# fleet_reads SECTION COMMIT: the server serving now answers /healthz with COMMIT as the commit it
# serves and as its fleet_commit, the commit of the newest announcement its startup.Announcer wrote
# once processes.Live read every live server process as serving that one build. The announcer reads
# Live as the process registers and every 5 s after (startup.Interval), so the read is tried for up
# to 30 s.
fleet_reads() {
  local tries=0
  while true; do
    status="$(curl -sS --max-time 10 -o "$work/body" -w '%{http_code}' "$base/healthz" 2>"$work/curl")" ||
      status="curl failed: $(<"$work/curl")"
    if [[ "$status" == 200 ]] && jq -e --arg c "$2" '.commit == $c and .fleet_commit == $c' "$work/body" >/dev/null 2>&1; then
      break
    fi
    tries=$((tries + 1))
    [[ "$tries" -lt 60 ]] ||
      fail "$1" "processes.Live reads every live server process as the one build $2: /healthz names it as the commit served and as fleet_commit" \
        "HTTP $status: $(body)"
    sleep 0.5
  done
  pass "$1" "processes.Live reads every live server process as the one build $2: /healthz answers $(jq -c '{commit, fleet_commit}' "$work/body")"
}

# move_state SECTION ADDRESS STORE-ID FROM TO: alice moves the node ADDRESS, whose store address is
# STORE-ID, from the state FROM to TO, through POST /api/v1/issues/{key}/nodes/{id}/state, or
# through POST .../settle for TO settled, which the state route takes from no caller.
move_state() {
  if [[ "$5" == settled ]]; then
    request "cookie:$alice" POST "/api/v1/issues/$key/nodes/$(q "$3")/settle"
  else
    request "cookie:$alice" POST "/api/v1/issues/$key/nodes/$(q "$3")/state" "$(jq -cn --arg s "$5" '{state: $s}')"
  fi
  want_status 200 "$1" "$alice moves $2 from $4 to $5"
  expect "$1" "$alice moves $2 from $4 to $5" \
    "$(jq -cnS --arg a "$2" --arg f "$4" --arg t "$5" '{address: $a, previous: $f, state: $t}')" \
    '{address: .node.address, previous, state: .node.state}'
}

# at URL COMMAND ARGS…: runs COMMAND, a function of this driver or of lib.sh, with $base set to URL,
# the server it talks to while two builds serve (B2). Every function that sends a request reads
# $base when it runs, so it reads the local below, and $base is the same again once COMMAND ends.
at() {
  local base="$1"
  shift
  "$@"
}

# census_kind FILE KIND: KIND's counts in the census report FILE, into $c_nodes, $c_rows,
# $c_row_differences and $c_node_differences.
census_kind() {
  local line
  line="$(grep -m 1 -E "^census: store kind=$2 nodes=" "$1")" || return 1
  [[ "$line" =~ nodes=([0-9,]+)\ rows=([0-9,]+)\ .*\ rows_without_a_node=([0-9,]+)\ nodes_without_a_row=([0-9,]+) ]] || return 1
  c_nodes="${BASH_REMATCH[1]//,/}"
  c_rows="${BASH_REMATCH[2]//,/}"
  c_row_differences="${BASH_REMATCH[3]//,/}"
  c_node_differences="${BASH_REMATCH[4]//,/}"
}

# census_zero SECTION NAME KIND: the census NAME read KIND at zero differences in both directions,
# having checked more than zero rows of it; a kind that checked no row fails, named.
census_zero() {
  census_kind "$work/census.$2" "$3" || fail "$1" "the census reads the kind $3" "no line of it in: $(<"$work/census.$2")"
  [[ "$c_row_differences" == 0 && "$c_node_differences" == 0 ]] ||
    fail "$1" "the census reads $3 at zero differences" \
      "$c_row_differences row(s) without a node and $c_node_differences node(s) without a row: $(grep -E "^census: store kind=$3 " "$work/census.$2" | tr '\n' ' ')"
  [[ "$c_rows" -gt 0 ]] ||
    fail "$1" "the census checked more than zero rows of the kind $3" "it checked $c_rows rows of $3, so it proves nothing of it"
  pass "$1" "the census reads $3 at zero differences: it checked $c_rows row(s) against $c_nodes node(s)"
}

# census_differences SECTION NAME: the census NAME read zero differences over every kind.
census_differences() {
  local line
  line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.$2")" ||
    fail "$1" "the census reads zero differences" "no total in: $(<"$work/census.$2")"
  [[ "$line" =~ differences=0$ ]] ||
    fail "$1" "the census reads zero differences over every kind" "$line: $(grep -E '^census: store kind=[a-z_]+ difference' "$work/census.$2" | tr '\n' ' ')"
  pass "$1" "the census reads zero differences over every kind ($line)"
}

# runs_json SECTION WHAT FILE: every run going_requests holds, with its going_events rows as its
# frames, one JSON array in FILE: each run's id, author, project, words and frames.
readonly runs_sql="select coalesce(json_agg(json_build_object(
    'id', r.id::text, 'author', r.author, 'project', r.project, 'words', r.words,
    'frames', coalesce((select json_agg(json_build_object('sequence', e.sequence, 'name', e.name, 'data', e.data)
                                  order by e.sequence)
                        from going_events e where e.request_id = r.id), '[]'::json))
  order by r.created_at, r.id), '[]'::json)
  from going_requests r"
runs_json() {
  query "$1" "$2" "$runs_sql"
  printf '%s\n' "$answer" >"$3"
}

# nodes_json SECTION WHAT FILE: every interaction node the store holds, one JSON array in FILE: each
# node's id, its made_by as author, its words, state, visibility and frame values in sequence.
readonly nodes_sql="select coalesce(json_agg(json_build_object(
    'id', n.id, 'author', n.made_by, 'words', n.attributes->>'words', 'state', n.state,
    'visibility', n.visibility,
    'frames', coalesce((select json_agg(f.value order by (f.value->>'sequence')::integer)
                        from (select distinct t.to_id::jsonb as value from graph_tuples t
                              where t.from_kind = 'interaction' and t.from_id = n.id and t.kind = 'frame'
                                and t.to_kind = 'value' and t.asserted) f), '[]'::json))
  order by n.id), '[]'::json)
  from graph_nodes n where n.kind = 'interaction'"
nodes_json() {
  query "$1" "$2" "$nodes_sql"
  printf '%s\n' "$answer" >"$3"
}

# ends_at_moved: the statements that set the ends_at of the run psql's :'run' names to now() plus
# psql's :'by', an interval, on both sides, for a caller to run inside its own transaction. In
# production no recorded fact moves: a run's ends_at is written once, by the write that records the
# run. So a move moves the going_requests row, the interaction node's ends_at attribute and a new
# ends_at value tuple together, written as graphstore.InteractionFacts writes that attribute (RFC
# 3339 in UTC with the fraction of a second time.RFC3339Nano writes, as migration 20261116205055
# spells it, stated by the run's made_by and caused by no event), so the newest tuple and the column
# agree with the row.
readonly ends_at_moved="
  update going_requests set ends_at = now() + :'by'::interval where id = :'run'::uuid;
  with moved as (
    select r.id::text as id,
           to_char(r.ends_at at time zone 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS')
             || coalesce('.' || nullif(rtrim(to_char(r.ends_at at time zone 'UTC', 'US'), '0'), ''), '')
             || 'Z' as ends_at
    from going_requests r where r.id = :'run'::uuid
  ),
  node as (
    update graph_nodes n set attributes = jsonb_set(n.attributes, '{ends_at}', to_jsonb(moved.ends_at))
    from moved where n.kind = 'interaction' and n.id = moved.id
    returning n.id, n.made_by
  )
  insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
  select 'interaction', node.id, 'ends_at', 'value', moved.ends_at, now(), node.made_by, 0, null, true
  from node join moved on moved.id = node.id;"

# want_ends_at SECTION WHAT RUN WHEN: RUN's going_requests row holds an ends_at of which WHEN, an SQL
# condition over r.ends_at, holds, and its interaction node's ends_at attribute and its newest
# ends_at value tuple both equal it.
want_ends_at() {
  want_value "$1" "$2" "true|true|true" \
    "select ($4)::text || '|' || (r.ends_at = (n.attributes->>'ends_at')::timestamptz)::text || '|'
            || (r.ends_at = (select t.to_id::timestamptz from graph_tuples t
                             where t.from_kind = 'interaction' and t.from_id = n.id and t.kind = 'ends_at'
                               and t.to_kind = 'value' and t.asserted
                             order by t.seq desc limit 1))::text
     from going_requests r join graph_nodes n on n.kind = 'interaction' and n.id = r.id::text
     where r.id = '$3'"
}

# against_inventory INVENTORY STORE TABLE: one line for each run INVENTORY holds that the store's
# interaction nodes (STORE) or the going tables (TABLE) lack or hold otherwise: its author, its words
# or its frames. A side holds a run's frames when the inventory's are the first frames it holds: a
# sweep may end a run after the inventory was read, and the frame it appends is no loss.
readonly against_inventory_jq='
  def keyed: map({key: .id, value: .}) | from_entries;
  def framed($have; $want): ($have | length) >= ($want | length) and $have[0:($want | length)] == $want;
  def held($side; $run; $what):
    [ (if $side.author != $run.author then "\($what) records the author \($side.author | tojson), the inventory \($run.author | tojson)" else empty end),
      (if $side.words != $run.words then "\($what) holds the words \($side.words | tojson), the inventory \($run.words | tojson)" else empty end),
      (if framed($side.frames; $run.frames) | not
       then "\($what) holds the frames \($side.frames | tojson), the inventory \($run.frames | tojson)" else empty end) ];
  ($store[0] | keyed) as $s | ($table[0] | keyed) as $t
  | .[] as $run
  | (if $s[$run.id] == null and $t[$run.id] == null
     then ["it is missing from both sides: the store holds no interaction node of it and going_requests holds no row of it"]
     else (if $s[$run.id] == null then ["the store holds no interaction node of it"] else held($s[$run.id]; $run; "its node") end)
          + (if $t[$run.id] == null then ["going_requests holds no row of it"] else held($t[$run.id]; $run; "its row") end)
     end) as $found
  | select($found | length > 0)
  | "run \($run.id) (\($run.words | tojson)): \($found | join("; "))"'

# --- inputs -------------------------------------------------------------------------------------

query setup "read the run's database before any server starts" \
  "select count(*) from information_schema.tables where table_schema = 'public'"
[[ "$answer" == 0 ]] || fail setup "the run's database is empty before any server starts" "it holds $answer tables"
pass setup "the run's database ($database_host) held no table before any server started"

# The identity table. A person wrote these rows, and no code the server runs produces them: each
# names a form going_requests.author can hold, the node the run's authored_by must name, and the
# person that form resolves to. The third row's form is the actor the base build's record stored
# for carol's own run, read out of that record once she has asked (B1); every other form is written
# here. $1 of each row is what it is, $2 the form, $3 the authored_by end, $4 the person.
identities=(
  'a login in mixed case|{"kind": "user", "id": "CaRoL"}|person:carol|carol'
  'the same login with spaces around it|{"kind": "user", "id": "  CaRoL  "}|person:carol|carol'
  'the raw actor id the record of her own run stored|RECORDED|person:carol|carol'
  "a session the server proved hers|{\"kind\": \"session\", \"id\": \"$carol_session\", \"owner\": \"carol\", \"act\": {\"session\": \"$carol_session\"}}|session:$carol_session|carol"
  'a login one character from hers, which must stay a person of its own|{"kind": "user", "id": "karol"}|person:karol|karol'
)
readonly identities
pass setup "the identity table holds ${#identities[@]} forms: four of $carol and one of $twin, one character from her"

start_fakes

# --- B1: the base build, its three runs, the hand-written row, the identity table, the inventory -

baseline_start
base="$baseline_url"
registered_commit B1 "read the base build's row of server_processes"
[[ -n "$baseline_commit" && "$answer" == "$baseline_commit" ]] ||
  fail B1 "the base build registers the commit it was built from" "server_processes names '$answer', want $baseline_commit"
pass B1 "the base build serves $base, stamped with commit $baseline_commit, which its row of server_processes names"
fleet_reads B1 "$baseline_commit"
# The versions the base build applied, one a line, kept as a file: H1 reads the migrations this
# head applied as the versions schema_migrations holds after its boot that this list lacks.
query B1 "read the versions of schema_migrations the base build applied" \
  "select version from schema_migrations order by version"
printf '%s\n' "$answer" >"$work/base.migrations"
readonly versions_re=$'^[0-9]+(\n[0-9]+)*$'
[[ "$answer" =~ $versions_re ]] ||
  fail B1 "the base build ran its own migrations over the empty database" "schema_migrations reads '$answer'"
pass B1 "the base build migrated the empty database: schema_migrations holds $(wc -l <"$work/base.migrations") versions, kept in $work/base.migrations"

sign_in B1 "$alice" " at $base"
sign_in B1 "$bob" " at $base"
sign_in B1 "$carol" " at $base"
request agent POST /api/v1/projects "$(jq -cn --arg k "$project" --arg l "$alice" --argjson a "$(actor)" \
  '{key: $k, name: "Census check", default_assignee: $l, actor: $a}')"
want_status 201 B1 "create the project $project"
# r3 holds two paragraphs, each a block of its own, so a version that changes the words of the
# second changes r3's text too (B2).
cat >"$work/spec.md" <<SPEC
The spec the census reads.

:::requirement{#r1}
Every run of the command bar is one node of the store.
:::

:::requirement{#r2}
$edited_from
:::

:::requirement{#r3}
This requirement is proposed while two builds serve.

$paragraph_from
:::
SPEC
request agent POST /api/v1/issues "$(jq -cn --arg p "$project" --rawfile s "$work/spec.md" --argjson a "$(actor)" \
  '{project: $p, title: "Census check", spec: $s, force: true, actor: $a}')"
want_status 201 B1 "create the issue"
key="$(jq -er '.key' "$work/body")" || fail B1 "create the issue" "no key: $(body)"
readonly key spec="dispatch://$key/spec"
readonly requirement="$spec#b-r1" requirement3="$spec#b-r3"
pass B1 "the base build created $key in $project, whose spec holds the requirements r1, r2 and r3"
cat >"$work/verification.md" <<VERIFICATION
How $key is proved.

:::check{#c1 serves="$requirement"}
The census reads zero differences.
:::
VERIFICATION
request agent POST "/api/v1/issues/$key/artifacts" "$(jq -cn --rawfile c "$work/verification.md" --argjson a "$(actor)" \
  '{name: "verification.md", content: $c, actor: $a}')"
want_status 201 B1 "write the verification document, whose check c1 serves r1"
readonly check1="dispatch://$key/artifact/verification-md#b-c1"
request "cookie:$bob" POST "/api/v1/issues/$key/comments" "$(jq -cn --arg b "A note $bob wrote on $key." '{body: $b}')"
want_status 201 B1 "$bob writes a comment"
pass B1 "the base build wrote $check1 and $bob's comment"

ask B1 "$alice" "What does the spec of $key say?"
readonly run_alice="$asked"
read_stream B1 "$alice" "$run_alice"
ask B1 "$bob" "Who wrote the spec of $key?"
readonly run_bob="$asked"
read_stream B1 "$bob" "$run_bob"
ask B1 "$carol" "Which requirement is edited?"
readonly run_carol="$asked"
read_stream B1 "$carol" "$run_carol"

# The server goes before the inventory is read, so nothing appends a frame after it.
baseline_stop_server
want_value B1 "the base build stopped: server_processes holds no row" 0 "select count(*) from server_processes"

with_vars B1 "write one run row a day ahead, with no frame" \
  "insert into going_requests (author, project, words, ends_at)
   select author, project, 'A run row written by hand, a day ahead, with no frame.', now() + interval '1 day'
   from going_requests where id = :'run'::uuid
   returning id::text" "run=$run_alice"
readonly run_ahead="$answer"
[[ "$run_ahead" =~ ^[0-9a-f-]{36}$ ]] || fail B1 "write one run row a day ahead" "psql answered '$run_ahead'"
want_value B1 "the run row $run_ahead is a day ahead and holds no frame" "true|0" \
  "select (r.ends_at > now() + interval '23 hours')::text || '|' || (select count(*) from going_events e where e.request_id = r.id)
   from going_requests r where r.id = '$run_ahead'"

query B1 "read the actor the record of $carol's own run stored" "select author::text from going_requests where id = '$run_carol'"
readonly carol_recorded="$answer"
jq -e --arg c "$carol" '.kind == "user" and .id == $c' <<<"$carol_recorded" >/dev/null ||
  fail B1 "the older server's sign-in stored $carol's login in its canonical form" "the record holds $carol_recorded"
pass B1 "the record of $carol's own run stored the actor $carol_recorded"
# Each run is written a day ahead, as the run row above is. This head ends, in one pass at each of
# its boots before the backfill, every run past its ends_at that holds no terminal frame, and so may
# a build's periodic sweep: a run written by hand that is overdue would gain a frame the inventory
# lacks. A day ahead, none of these runs is overdue at any boot of this run, whichever build boots.
identity_runs=()
for row in "${identities[@]}"; do
  IFS='|' read -r what form wanted person <<<"$row"
  [[ "$form" != RECORDED ]] || form="$carol_recorded"
  jq -e . <<<"$form" >/dev/null || fail B1 "the identity table's form for $what is JSON" "$form"
  with_vars B1 "write the identity table's $what into going_requests.author" \
    "insert into going_requests (author, project, words, ends_at)
     values (:'form'::jsonb, :'project', :'words', now() + interval '1 day')
     returning id::text" "form=$form" "project=$project" "words=Identity table: $what."
  identity_runs+=("$answer|$form|$wanted|$person|$what")
  pass B1 "wrote run $answer with author $form ($what), a day ahead, which must resolve to $wanted, $person"
done
readonly identity_runs
want_value B1 "every run the inventory reads holds its terminal frame or an ends_at a day ahead, so neither build ends one of them unless this driver moves its ends_at" 0 \
  "select count(*) from going_requests r
   where r.ends_at < now() + interval '23 hours'
     and not exists (select 1 from going_events e where e.request_id = r.id and e.name in ('end', 'error'))"

runs_json B1 "read the inventory out of the base build's database" "$work/inventory.json"
inventory_size="$(jq 'length' "$work/inventory.json")" || fail B1 "count the runs of the inventory" "unreadable: $(head -c 600 "$work/inventory.json")"
readonly inventory_size
[[ "$inventory_size" == $((3 + 1 + ${#identities[@]})) ]] ||
  fail B1 "the inventory holds the three runs, the row a day ahead and one run for each row of the identity table" \
    "it holds $inventory_size runs: $(jq -c '[.[] | .id]' "$work/inventory.json")"
jq -e '[.[] | select(.id == $a or .id == $b or .id == $c) | .frames | map(select(.name == "end" or .name == "error")) | length] == [1, 1, 1]' \
  --arg a "$run_alice" --arg b "$run_bob" --arg c "$run_carol" "$work/inventory.json" >/dev/null ||
  fail B1 "the inventory holds each API run's terminal frame" "$(jq -c '[.[] | {id, frames}]' "$work/inventory.json")"
jq -r '.[] | "  inventory: run \(.id) by \(.author | tojson): \(.words | tojson), \(.frames | length) frame(s)"' "$work/inventory.json"
pass B1 "kept the inventory of $inventory_size runs, each its id, author, words and frames, in $work/inventory.json"

# --- H1: this head over the same database, its migrations and backfill, against the inventory ----

boot_server H1 "this head's server" "$server_bin"
seeded_catalogue

registered_commit H1 "read this head's row of server_processes"
readonly head_commit="$answer"
[[ -n "$head_commit" && "$head_commit" != "$baseline_commit" ]] ||
  fail H1 "this head is stamped with a commit of its own" "its row names '$head_commit', the base build's $baseline_commit"
pass H1 "this head registers commit $head_commit and the base build registered $baseline_commit: two stamped builds"
fleet_reads H1 "$head_commit"
# The migrations this head applied are the versions schema_migrations holds now that it did not
# hold after B1, and not the versions above the base's newest. A version is its migration's
# timestamp, and the base may be a commit holding every unmerged branch this child builds on, whose
# newest migration can sort after every one of this head's; the store applies each version
# schema_migrations does not record, wherever it sorts (store.go, migrate).
query H1 "read the versions of schema_migrations after this head's boot" \
  "select version from schema_migrations order by version"
printf '%s\n' "$answer" >"$work/head.migrations"
head_migrations="$(comm -13 <(sort "$work/base.migrations") <(sort "$work/head.migrations") | tr '\n' ' ')" ||
  fail H1 "compare the versions of schema_migrations after this head's boot with the base build's" "comm refused them"
readonly head_migrations="${head_migrations% }"
[[ -n "$head_migrations" && " $head_migrations " == *" 20261116205055 "* ]] ||
  fail H1 "this head ran its migrations over the base build's schema, 20261116205055_interaction_nodes among them" \
    "the versions schema_migrations holds after this head's boot and did not hold after B1 are: '$head_migrations'"
pass H1 "this head ran $(wc -w <<<"$head_migrations") migration(s) the base build had not, 20261116205055_interaction_nodes among them: $head_migrations"

backfill H1
query H1 "read the owed block versions" "select count(*) from document_block_versions where version is null"
printf '%s: %s document_block_versions row(s) owed a version after the backfill\n' "$driver" "$answer"
waited=0
until [[ "$answer" == 0 ]]; do
  [[ "$waited" -lt "$wait_limit" ]] ||
    fail H1 "no document_block_versions row holds a null version within $wait_limit s" "$answer row(s) still do"
  sleep 5
  waited=$((waited + 5))
  query H1 "read the owed block versions" "select count(*) from document_block_versions where version is null"
done
pass H1 "no document_block_versions row holds a null version (waited ${waited} s)"

nodes_json H1 "read the store's interaction nodes" "$work/store.h1.json"
runs_json H1 "read going_requests and going_events" "$work/table.h1.json"
jq -r --slurpfile store "$work/store.h1.json" --slurpfile table "$work/table.h1.json" "$against_inventory_jq" \
  "$work/inventory.json" >"$work/lost.h1" || fail H1 "hold the store and the going tables to the inventory" "jq refused it"
[[ ! -s "$work/lost.h1" ]] ||
  fail H1 "the store and going_requests and going_events hold every run of the inventory" "$(tr '\n' ' ' <"$work/lost.h1")"
pass H1 "the store holds an interaction node for each of the $inventory_size runs of the inventory, with its author, its words and its frames"
pass H1 "going_requests and going_events hold each of the $inventory_size runs of the inventory, with its author, its words and its frames"
extra="$(jq -c --slurpfile inventory "$work/inventory.json" '[.[].id] - [$inventory[0][].id]' "$work/store.h1.json")"
[[ "$extra" == "[]" ]] || fail H1 "the store holds no interaction node the inventory lacks" "it holds $extra"
pass H1 "the store holds no interaction node the inventory lacks"

# --- H2: this build's writes ----------------------------------------------------------------------

sign_in H2 "$alice" " at $base"
sign_in H2 "$bob" " at $base"
seed="$(jq -c '{lens_kind: [.lens_kinds[].id], constraint: [.lens_kinds[].sources[].ref | ltrimstr("dispatch://constraint/")],
  constrained_by: [.lens_kinds[] | .id as $k | .sources[] | "lens_kind:\($k) constrained_by constraint:\(.ref | ltrimstr("dispatch://constraint/"))"]}' \
  "$work/catalogue.json")" || fail H2 "read the migration's seed out of the catalogue" "unreadable: $(<"$work/catalogue.json")"
jq -e '(.lens_kind | length) > 0 and (.lens_kind | length) == (.constraint | length)' <<<"$seed" >/dev/null ||
  fail H2 "each seeded lens_kind carries one source" "$seed"
jq -c '. + {lens: [], finding: [], risk: [], has_kind: [], triggered_by: [], prevented_by: []}' <<<"$seed" >"$work/writes.json"
pass H2 "the migration's seed is $(jq '.lens_kind | length' <<<"$seed") lens kinds, each held to one constraint"
# written KIND ENTRY: one entry of the inventory of this build's writes.
written() {
  jq -c --arg k "$1" --arg v "$2" '.[$k] += [$v]' "$work/writes.json" >"$work/writes.next"
  mv "$work/writes.next" "$work/writes.json"
}
query H2 "read the node of the check c1" "select id from graph_nodes where kind = 'block' and id like '$key/%#c1'"
readonly check1_node="$answer"
[[ -n "$check1_node" ]] || fail H2 "read the node of the check c1" "the store holds none"

request "cookie:$alice" POST "/api/v1/issues/$key/lenses" \
  "$(jq -cn --arg on "$spec" '{on: $on, kind: "pre-mortem", visibility: "author"}')"
want_status 201 H2 "$alice invokes a pre-mortem on the spec reserved for her"
premortem="$(jq -er '.address' "$work/body")" || fail H2 "invoke the pre-mortem" "no address: $(body)"
readonly premortem premortem_id="${premortem#dispatch://lens/}"
written lens "$premortem_id"
written has_kind "lens:$premortem_id has_kind lens_kind:pre-mortem"
pass H2 "$alice invoked $premortem, reserved for her"

request "cookie:$alice" POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$premortem" --arg c "$check1" \
  '{lens: $l, heading: "A run lost by the migration", early_warning: "A run the inventory holds and the store lacks",
    prevention: "The census against an inventory of its own", prevented_by: $c, visibility: "author"}')"
want_status 201 H2 "$alice writes a risk under the pre-mortem, prevented by the check c1"
risk="$(jq -er '.address' "$work/body")" || fail H2 "write the risk" "no address: $(body)"
readonly risk risk_id="${risk#dispatch://risk/}"
written risk "$risk_id"
written prevented_by "risk:$risk_id prevented_by block:$check1_node"
pass H2 "$alice wrote $risk, prevented by $check1"

request "cookie:$alice" POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" --arg t "$premortem" \
  '{on: $on, kind: "trade-off", verdict: "One store, two projections.", findings: ["The going tables project the store."], triggered_by: $t}')"
want_status 201 H2 "$alice invokes a trade-off analysis with one finding, triggered by the pre-mortem"
tradeoff="$(jq -er '.address' "$work/body")" || fail H2 "invoke the trade-off analysis" "no address: $(body)"
finding="$(jq -er '.findings[0].address' "$work/body")" || fail H2 "invoke the trade-off analysis" "no finding: $(body)"
readonly tradeoff tradeoff_id="${tradeoff#dispatch://lens/}" finding
readonly finding_node="lens/${finding#dispatch://lens/}"
[[ "$finding_node" == "lens/$tradeoff_id#b-"* ]] || fail H2 "the finding stands under the trade-off analysis" "$finding"
written lens "$tradeoff_id"
written finding "${finding_node/\#b-/#}"
written has_kind "lens:$tradeoff_id has_kind lens_kind:trade-off"
written triggered_by "lens:$tradeoff_id triggered_by lens:$premortem_id"
pass H2 "$alice invoked $tradeoff, with the finding $finding"

request "cookie:$bob" POST "/api/v1/issues/$key/constraints" "$(jq -cn \
  '{standard: "RFC 3339, Date and Time on the Internet: Timestamps", reference: "RFC 3339 §5.6, https://www.rfc-editor.org/rfc/rfc3339", visibility: "author"}')"
want_status 201 H2 "$bob writes a constraint reserved for him"
constraint="$(jq -er '.address' "$work/body")" || fail H2 "write the constraint" "no address: $(body)"
readonly constraint constraint_id="${constraint#dispatch://constraint/}"
written constraint "$constraint_id"
pass H2 "$bob wrote $constraint, reserved for him"

request "cookie:$bob" POST "/api/v1/issues/$key/nodes/$(q "$key/spec#r1")/pointers" \
  "$(jq -cn --arg t "$constraint" '{kind: "constrained_by", action: "append", to: $t}')"
want_status 200 H2 "$bob points the requirement r1 at his constraint"
written constrained_by "block:$key/spec#r1 constrained_by constraint:$constraint_id"
pass H2 "the requirement block $requirement holds a constrained_by tuple to $constraint"

run_cli H2 "add a lens kind" lens-kind-add --id "$added_kind" --words "the census check" \
  --source "Gary Klein, Performing a Project Premortem, Harvard Business Review, 2007" \
  --reference "Harvard Business Review 85(9), September 2007, pp. 18-19"
[[ "$cli_code" == 0 ]] || fail H2 "envoy-dispatch lens-kind-add adds $added_kind" "exited $cli_code: $(<"$work/cli.out")"
added_source="$(sed -n "s#^lens-kind-add: added dispatch://lens_kind/$added_kind, held to its source dispatch://constraint/\([^ ,]*\).*#\1#p" "$work/cli.out")"
[[ -n "$added_source" ]] || fail H2 "envoy-dispatch lens-kind-add adds $added_kind" "it printed: $(<"$work/cli.out")"
readonly added_source
written lens_kind "$added_kind"
written constraint "$added_source"
written constrained_by "lens_kind:$added_kind constrained_by constraint:$added_source"
pass H2 "envoy-dispatch lens-kind-add added dispatch://lens_kind/$added_kind, held to dispatch://constraint/$added_source"

ask H2 "$alice" "Edit the requirement r2 of $key for me."
readonly run_edit="$asked"
# The run's window is this driver's, so a slow pod cannot fail the edit below: straight after the
# ask, well inside the window the server gave the run, its ends_at goes a day ahead on both sides.
with_vars H2 "set run $run_edit's ends_at a day ahead on both sides" "
  begin;
  $ends_at_moved
  commit;" "run=$run_edit" "by=1 day"
want_ends_at H2 "run $run_edit's ends_at is a day ahead on both sides: its row, its node's ends_at attribute and its newest ends_at value tuple" \
  "$run_edit" "r.ends_at > now() + interval '23 hours'"
query H2 "read the requirement block r2 before the edit" \
  "select (made_at < (select created_at from going_requests where id = '$run_edit'))::text || '|' || state
   from graph_nodes where kind = 'block' and id = '$key/spec#r2'"
[[ "$answer" == "true|live" ]] ||
  fail H2 "the requirement block r2 exists before the run that edits it" "the store reads '$answer' of $key/spec#r2"
request "cookie:$alice" POST "/api/v1/issues/$key/artifacts/spec/edits" \
  "$(jq -cn --arg f "$edited_from" --arg t "$edited_to" '{ops: [{op: "replace", find: $f, with: $t}]}')" "$run_edit"
[[ "$status" == 200 || "$status" == 201 ]] ||
  fail H2 "$alice edits the words of r2 under the header of run $run_edit" "HTTP $status: $(body)"
pass H2 "$alice edited the words of the existing requirement block r2 under the header of run $run_edit (HTTP $status)"
want_value H2 "the edit names r2, which it changed and did not create, as changed by the run, and not r1, which it left as it was" "1|0" \
  "select (select count(*) from graph_tuples where from_kind = 'block' and from_id = '$key/spec#r2'
             and kind = 'changed_by_run' and to_kind = 'interaction' and to_id = '$run_edit' and asserted)
          || '|' || (select count(*) from graph_tuples where from_kind = 'block' and from_id = '$key/spec#r1'
             and kind = 'changed_by_run' and to_kind = 'interaction' and to_id = '$run_edit')"
# Every node the edit names as changed by the run: r2 and the blocks inside it whose words it changed,
# each a block of the spec that stood before the run was asked.
query H2 "read every node the edit names as changed by the run" \
  "select coalesce(string_agg(t.from_kind || ':' || t.from_id || '|' || (n.made_at < r.created_at)::text || '|' || n.state,
                              ' ' order by t.from_id), '')
   from graph_tuples t
   join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id
   join going_requests r on r.id::text = t.to_id
   where t.kind = 'changed_by_run' and t.to_kind = 'interaction' and t.to_id = '$run_edit' and t.asserted"
changed_blocks=()
for changed in $answer; do
  [[ "$changed" == "block:$key/spec#"*"|true|live" ]] ||
    fail H2 "each node the edit names as changed by the run is a block of the spec that stood before the run" "it names $answer"
  changed_blocks+=("${changed%%|*}")
done
readonly changed_blocks
pass H2 "the edit names ${#changed_blocks[@]} block(s) of the spec as changed by the run, each standing before it: ${changed_blocks[*]}"

request agent POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" --arg s "$bearer_session" \
  '{on: $on, kind: "boring-solution", actor: {kind: "session", id: $s}}')"
want_status 201 H2 "the shared bearer invokes a lens whose body names the session $bearer_session"
bearer_lens="$(jq -er '.address' "$work/body")" || fail H2 "invoke the lens on the shared bearer" "no address: $(body)"
readonly bearer_lens bearer_lens_id="${bearer_lens#dispatch://lens/}"
expect H2 "the lens on the shared bearer is project-visible" '"project"' '.visibility'
expect H2 "the lens on the shared bearer answers the session its body names as its author" \
  "$(jq -cnS --arg s "$bearer_session" '{kind: "session", id: $s}')" '.author'
written lens "$bearer_lens_id"
written has_kind "lens:$bearer_lens_id has_kind lens_kind:boring-solution"
# Attribution is no proof (spec v46 R12): the shared bearer proves no author, so the lens is the
# project's and its writer settles nothing, while its authored_by names the session its body names,
# whose node the write stated beside that tuple. The tuple is read as it stands now, its newest
# entry for each far end, and only one that is asserted counts.
want_value H2 "the lens on the shared bearer is made by the session its body names, holds its authored_by to that session, and its write stated the session's node" \
  "$bearer_session|session:$bearer_session|1" \
  "select (l.made_by->>'id') || '|'
          || coalesce((select string_agg(held.to_kind || ':' || held.to_id, ' ' order by held.to_kind, held.to_id) from (
               select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
               where t.from_kind = 'lens' and t.from_id = l.id and t.kind = 'authored_by'
               order by t.to_kind, t.to_id, t.seq desc) held where held.asserted), '') || '|'
          || (select count(*) from graph_nodes s where s.kind = 'session' and s.id = '$bearer_session')
   from graph_nodes l where l.kind = 'lens' and l.id = '$bearer_lens_id'"
jq -r 'to_entries[] | "  inventory of writes: \(.key) \(.value | length)"' "$work/writes.json"
pass H2 "kept the inventory of this build's writes in $work/writes.json"

# Straight after those writes, before any further backfill.
take_census H2 h2
census_zero H2 h2 person
census_zero H2 h2 session
# The census's session node side is every session node (graphcensus's session kind), so were
# $bearer_session not on its row side, its node, which the lens's check above read, would be a node
# without a row, on which census_zero fails: the session is on both sides.
pass H2 "the census reads no session node without a row while $bearer_session, the writer the lens on the shared bearer names through authored_by, is a session node, so its row side holds that session too: it is on both sides"

# --- B2: the base build beside this head, then alone, as in a rollback ----------------------------

# A rollback rolls the older pods in while this head's still serve: the base build boots beside this
# head's server, over the one database and on a port of its own, and both sign cookies with the
# run's one dev sign-in key (boot_server, baseline_start), so a person signed in on one build is
# signed in on the other. Each request below names the build it goes to (at).
readonly head_url="$base"
beside_pid="$server_pid"
boot_server B2 "the base build after a rollback" "$baseline_binary"
readonly older_url="$base"
want_value B2 "both builds serve the one database at once: server_processes holds a row of the base build and a row of this head" \
  "2|1|1" \
  "select count(*) || '|' || count(*) filter (where commit = '$baseline_commit')
          || '|' || count(*) filter (where commit = '$head_commit')
   from server_processes"
pass B2 "the base build serves $older_url beside this head at $head_url, stamped with $baseline_commit"

# While both builds serve, alice proposes the requirement r3 through this head, and the base build
# writes a version of the spec that changes the words of r3's second paragraph, a block of its own.
# The index pass writes again only the node of a block whose text a version changed
# (refs.IndexBlocks), and r3's text holds its paragraphs, so that version's pass, the base build's,
# writes r3's node again: the write dispatch://INFRA-1149's keep-state rule (its R8) keeps r3's
# state through. A version that changed only a block outside r3 would leave r3's node unwritten,
# and r3 proposed whatever the base build's rule, so the driver first reads that the version moved
# r3's text.
readonly r3_changed_sql="select coalesce(max(b.changed_version), 0) from document_blocks b
  join artifacts a on a.id = b.artifact_id where a.ref_key = '$key/spec' and b.block_id = 'r3'"
at "$head_url" sign_in B2 "$alice" " at this head, $head_url"
at "$head_url" move_state B2 "$requirement3" "$key/spec#r3" live proposed
query B2 "read the version r3's text last changed in" "$r3_changed_sql"
readonly r3_proposed_at="$answer"
[[ "$r3_proposed_at" -gt 0 ]] || fail B2 "read the version r3's text last changed in" "document_blocks holds no row of r3"
at "$older_url" sign_in B2 "$alice" " at the base build, $older_url"
at "$older_url" request "cookie:$alice" POST "/api/v1/issues/$key/artifacts/spec/edits" \
  "$(jq -cn --arg f "$paragraph_from" --arg t "$paragraph_to" '{ops: [{op: "replace", find: $f, with: $t}]}')"
[[ "$status" == 200 || "$status" == 201 ]] ||
  fail B2 "$alice edits the words of r3's second paragraph through the base build" "HTTP $status: $(body)"
pass B2 "$alice edited the words of r3's second paragraph through the base build's own edit route (HTTP $status)"
query B2 "read the version r3's text last changed in" "$r3_changed_sql"
waited=0
until [[ "$answer" -gt "$r3_proposed_at" ]]; do
  [[ "$waited" -lt 60 ]] ||
    fail B2 "the base build's version changes r3's text within 60 s" "r3's text last changed in version $answer, as when it was proposed"
  sleep 1
  waited=$((waited + 1))
  query B2 "read the version r3's text last changed in" "$r3_changed_sql"
done
readonly r3_changed="$answer"
want_value B2 "the base build's version $r3_changed changed the words of r3's second paragraph, a block of its own, and with them r3's text, so its index pass wrote r3's node again" 1 \
  "select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id
   where a.ref_key = '$key/spec' and b.block_id <> 'r3' and b.changed_version = $r3_changed
     and position('$paragraph_to' in b.excerpt) > 0"
at "$head_url" request "cookie:$alice" GET "/api/v1/issues/$key/proposed"
want_status 200 B2 "this head reads the proposed nodes of $key"
listed="$(jq -r --arg a "$requirement3" '[.nodes[] | select(.node.ref == $a) | .node.state] | join(" ")' "$work/body")" ||
  fail B2 "this head reads the proposed nodes of $key" "unreadable: $(body)"
query B2 "read the state the store holds r3 at" "select state from graph_nodes where kind = 'block' and id = '$key/spec#r3'"
[[ "$listed" == proposed && "$answer" == proposed ]] ||
  fail B2 "this head reads the requirement $requirement3 still proposed after the base build's version $r3_changed" \
    "this head reads it ${listed:-not proposed: GET /api/v1/issues/$key/proposed lists no $requirement3}, and the store holds the block $key/spec#r3 at '$answer'"
pass B2 "this head reads the requirement $requirement3 still proposed after the base build's version $r3_changed wrote its node again"

# While both builds serve, dave, a login no other row names, writes through this head a lens
# reserved for him, the one node he holds, and reads his own page through this head with that lens
# on it. bob then reads dave's page through the base build, which registers no lens kind, so no
# node dave holds is one bob may see there, and it answers him what it answers for a login nobody
# holds: the status and the body, byte for byte once the login is substituted
# (dispatch://INFRA-1149's person-page rule, its R15).
at "$head_url" sign_in B2 "$third" " at this head, $head_url"
at "$head_url" request "cookie:$third" POST "/api/v1/issues/$key/lenses" \
  "$(jq -cn --arg on "$spec" '{on: $on, kind: "boring-solution", visibility: "author"}')"
want_status 201 B2 "$third invokes a lens on the spec reserved for him, through this head"
third_lens="$(jq -er '.address' "$work/body")" || fail B2 "$third invokes a lens through this head" "no address: $(body)"
readonly third_lens third_lens_id="${third_lens#dispatch://lens/}"
expect B2 "the lens $third invoked is reserved for its author" '"author"' '.visibility'
written lens "$third_lens_id"
written has_kind "lens:$third_lens_id has_kind lens_kind:boring-solution"
want_value B2 "the one node the store ties $third to is that lens, $third_lens, reserved for its author" \
  "lens:$third_lens_id:author" \
  "select coalesce(string_agg(n.kind || ':' || n.id || ':' || n.visibility, ' ' order by n.kind, n.id), '')
   from (select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted from graph_tuples t
         where t.to_kind = 'person' and t.to_id = '$third'
         order by t.from_kind, t.from_id, t.seq desc) held
   join graph_nodes n on n.kind = held.from_kind and n.id = held.from_id
   where held.asserted"
at "$head_url" request "cookie:$third" GET "/api/v1/people/$third/nodes"
want_status 200 B2 "$third reads his own page through this head"
jq -e --arg id "$third_lens_id" '[.rows[] | select(.kind == "lens" and .id == $id)] | length == 1' "$work/body" >/dev/null ||
  fail B2 "$third reads his own page through this head with the lens $third_lens on it" "$(body)"
pass B2 "$third reads his own page through this head with the lens $third_lens on it"
at "$older_url" sign_in B2 "$bob" " at the base build, $older_url"
at "$older_url" request "cookie:$bob" GET "/api/v1/people/$third/nodes"
readonly third_page_status="$status"
cp "$work/body" "$work/page.$third"
at "$older_url" request "cookie:$bob" GET "/api/v1/people/$nobody/nodes"
readonly nobody_page_status="$status"
cp "$work/body" "$work/page.$nobody"
pages="$third's page: HTTP $third_page_status $(head -c 600 "$work/page.$third"); $nobody's page: HTTP $nobody_page_status $(head -c 600 "$work/page.$nobody")"
readonly pages
[[ "$third_page_status" == "$nobody_page_status" ]] ||
  fail B2 "$bob reads $third's page through the base build and is answered the status a login nobody holds answers" "$pages"
cmp -s <(sed "s/$third/$nobody/g" "$work/page.$third") "$work/page.$nobody" ||
  fail B2 "$bob reads $third's page through the base build and is answered, byte for byte once the login is substituted, the body a login nobody holds answers" "$pages"
pass B2 "$bob reads $third's page through the base build and is answered what $nobody, a login nobody holds, is answered, byte for byte: HTTP $nobody_page_status $(head -c 200 "$work/page.$nobody")"

# The rollback goes on: this head's server stops, and the base build serves the database alone.
baseline_stop_pid "$beside_pid"
beside_pid=""
registered_commit B2 "read server_processes once this head's server stopped"
[[ "$answer" == "$baseline_commit" ]] ||
  fail B2 "the base build alone serves the database once this head's server stopped" "its one row names '$answer', want $baseline_commit"
pass B2 "this head's server stopped, and the base build alone serves the database at $base, stamped with $baseline_commit"
fleet_reads B2 "$baseline_commit"
sign_in B2 "$alice" " at $base"
ask B2 "$alice" "What did the census read?"
readonly run_new="$asked"
read_stream B2 "$alice" "$run_new"
want_value B2 "the base build wrote run $run_new with no interaction node and its frames with no frame value" "0|0|true" \
  "select (select count(*) from graph_nodes where kind = 'interaction' and id = '$run_new') || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$run_new') || '|'
          || ((select count(*) from going_events where request_id = '$run_new') > 0)::text"
want_value B2 "the run row $run_ahead, written by hand, has the node this head gave it" 1 \
  "select count(*) from graph_nodes where kind = 'interaction' and id = '$run_ahead'"
# The edit below stands for time passing, so that the older build's sweep finds the run overdue,
# and moves both sides in one transaction (ends_at_moved). The third run, carol's own (bob's stays,
# for P to read his run admitted to him alone), is dated two hours back on its row alone: that
# build's sweep deletes the row, and the backfill states its node gone.
readonly run_dated="$run_carol"
with_vars B2 "move run $run_ahead's ends_at into the past on both sides, and date run $run_dated two hours back" "
  begin;
  $ends_at_moved
  update going_requests set created_at = now() - interval '2 hours', ends_at = now() - interval '2 hours'
  where id = :'dated'::uuid;
  commit;" "run=$run_ahead" "by=-1 minute" "dated=$run_dated"
want_ends_at B2 "run $run_ahead's ends_at is in the past on both sides: its row, its node's ends_at attribute and its newest ends_at value tuple" \
  "$run_ahead" "r.ends_at < now()"
pass B2 "moved run $run_ahead's ends_at into the past on both sides in one transaction, and dated run $run_dated two hours back"
readonly sweep_sql="select (select count(*) from going_events where request_id = '$run_ahead' and name in ('end', 'error'))
  || '|' || (select count(*) from going_requests where id = '$run_dated')"
query B2 "read what the base build's sweep has done" "$sweep_sql"
waited=0
until [[ "$answer" == "1|0" ]]; do
  [[ "$waited" -lt "$wait_limit" ]] ||
    fail_with_log B2 "the base build's sweep ends run $run_ahead and deletes run $run_dated within $wait_limit s" \
      "terminal frames of $run_ahead|rows of $run_dated read $answer" "$work/the base build after a rollback.log"
  sleep 5
  waited=$((waited + 5))
  query B2 "read what the base build's sweep has done" "$sweep_sql"
done
pass B2 "the base build's sweep appended run $run_ahead's terminal frame and deleted run $run_dated's row (waited ${waited} s)"
stop_server B2 "the base build"

# --- H3: this head and the backfill again, and the census ---------------------------------------

boot_server H3 "this head's server again" "$server_bin"
registered_commit H3 "read this head's row of server_processes"
[[ "$answer" == "$head_commit" ]] || fail H3 "this head serves the database again" "its row names '$answer', want $head_commit"
pass H3 "this head serves the database again at $base, stamped with $head_commit"
fleet_reads H3 "$head_commit"
# A process announces the build the fleet serves only once processes.Live reads every live process
# as serving its own commit (startup.Announcer, servedByEvery). While both builds served (B2) the
# fleet agreed on none, so neither wrote a row then; the base build wrote its row once this head's
# server stopped, which fleet_reads B2 read. So the window of two builds adds no announcement.
want_value H3 "processes.Live read the base build and this head as two builds: the announcements, in their order, name the base build, this head, the base build after the rollback and this head again" \
  "$baseline_commit $head_commit $baseline_commit $head_commit" \
  "select coalesce(string_agg(commit, ' ' order by seq), '') from server_build_announcements"

# The census beside a planted instance of what it must find (the Verification preamble): before
# the backfill, run $run_new holds the row the base build wrote and no interaction node, and the
# terminal frame the base build's sweep appended to run $run_ahead holds its going_events row and
# no frame value. This census must name both, so the zero readings after the backfill read the
# store, not a census that finds nothing.
query H3 "read the sequence of the terminal frame the base build's sweep appended to run $run_ahead" \
  "select coalesce(string_agg(sequence::text, ' '), '') from going_events where request_id = '$run_ahead' and name in ('end', 'error')"
readonly ahead_frame="$answer"
[[ "$ahead_frame" =~ ^[0-9]+$ ]] || fail H3 "run $run_ahead holds one terminal frame" "the sequences of its terminal frames read '$ahead_frame'"
want_value H3 "before the backfill, run $run_new holds its row and no interaction node, and run $run_ahead holds the row of its terminal frame $ahead_frame and no frame value" \
  "1|0|1|0" \
  "select (select count(*) from going_requests where id = '$run_new') || '|'
          || (select count(*) from graph_nodes where kind = 'interaction' and id = '$run_new') || '|'
          || (select count(*) from going_events where request_id = '$run_ahead' and sequence = $ahead_frame) || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$run_ahead' and kind = 'frame')"
take_census H3 planted
grep -Fqx "census: store kind=interaction difference at interaction $run_new: the row names a node the store does not hold" \
  "$work/census.planted" ||
  fail H3 "the census names run $run_new, a row with no node, a difference of the kind interaction" \
    "it reads: $(grep -E '^census: store kind=interaction ' "$work/census.planted" | tr '\n' ' ')"
pass H3 "the census names run $run_new, a row with no node, a difference of the kind interaction: the row names a node the store does not hold"
grep -Eq "^census: store kind=frame difference at frame $run_ahead $ahead_frame: the row and its node disagree: the row digests [0-9a-f]{32} and the node \\(none\\)\$" \
  "$work/census.planted" ||
  fail H3 "the census names the terminal frame $ahead_frame of run $run_ahead, a row with no frame value, a difference of the kind frame" \
    "it reads: $(grep -E '^census: store kind=frame ' "$work/census.planted" | tr '\n' ' ')"
pass H3 "the census names the terminal frame $ahead_frame of run $run_ahead, a row with no frame value, a difference of the kind frame: its node holds none"
line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.planted")" ||
  fail H3 "the census reads its total of differences" "no total in: $(<"$work/census.planted")"
[[ ! "$line" =~ differences=0$ ]] || fail H3 "the census reads differences while the two are planted" "$line"
pass H3 "the census reads differences while the two are planted, beside its zero readings after the backfill: $line"

backfill H3
nodes_json H3 "read the store's interaction nodes" "$work/store.h3.json"
runs_json H3 "read going_requests and going_events" "$work/table.h3.json"
jq -e --arg id "$run_new" --slurpfile table "$work/table.h3.json" \
  '(.[] | select(.id == $id)) as $node | ($table[0][] | select(.id == $id)) as $row
   | $node.state == "live" and $node.visibility == "author" and $node.author == $row.author and $node.words == $row.words
     and $node.frames == $row.frames and ($row.frames | length) > 0' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a node for run $run_new, with a frame value for each frame the base build wrote" \
    "node: $(jq -c --arg id "$run_new" '.[] | select(.id == $id)' "$work/store.h3.json"); row: $(jq -c --arg id "$run_new" '.[] | select(.id == $id)' "$work/table.h3.json")"
pass H3 "the store holds a node for run $run_new, which the base build recorded, with a frame value for each of its $(jq --arg id "$run_new" '.[] | select(.id == $id) | .frames | length' "$work/table.h3.json") frame(s)"
jq -e --arg id "$run_ahead" --slurpfile table "$work/table.h3.json" \
  '(.[] | select(.id == $id)) as $node | ($table[0][] | select(.id == $id)) as $row
   | $node.frames == $row.frames and ($row.frames | map(select(.name == "error")) | length) == 1' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a frame value for the terminal frame the base build's sweep appended to run $run_ahead" \
    "node: $(jq -c --arg id "$run_ahead" '.[] | select(.id == $id)' "$work/store.h3.json"); row: $(jq -c --arg id "$run_ahead" '.[] | select(.id == $id)' "$work/table.h3.json")"
pass H3 "the store holds a frame value for the terminal frame the base build's sweep appended to run $run_ahead"
jq -e --slurpfile table "$work/table.h3.json" \
  '([.[] | {key: .id, value: .frames}] | from_entries) as $nodes
   | all($table[0][]; .frames == $nodes[.id])' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a frame value for every frame going_events holds" \
    "$(jq -c --slurpfile nodes "$work/store.h3.json" '[.[] | .id as $id | select(.frames != ($nodes[0][] | select(.id == $id) | .frames))]' "$work/table.h3.json")"
pass H3 "the store holds a frame value for every frame going_events holds, $(jq '[.[].frames[]] | length' "$work/table.h3.json") frame(s) of $(jq 'length' "$work/table.h3.json") run(s)"
want_value H3 "the interaction of run $run_dated, whose row the base build deleted, is at state gone, and no row of it is left" "gone|0|0" \
  "select n.state || '|' || (select count(*) from going_requests where id = '$run_dated')
          || '|' || (select count(*) from going_events where request_id = '$run_dated')
   from graph_nodes n where n.kind = 'interaction' and n.id = '$run_dated'"
# The inventory again: each of its runs but the one the base build deleted is in both products.
jq -c --arg gone "$run_dated" '[.[] | select(.id != $gone)]' "$work/inventory.json" >"$work/inventory.h3.json" ||
  fail H3 "read the inventory but run $run_dated" "unreadable: $(head -c 600 "$work/inventory.json")"
jq -r --slurpfile store "$work/store.h3.json" --slurpfile table "$work/table.h3.json" "$against_inventory_jq" \
  "$work/inventory.h3.json" >"$work/lost.h3" || fail H3 "hold the store and the going tables to the inventory" "jq refused it"
[[ ! -s "$work/lost.h3" ]] ||
  fail H3 "the store and the going tables still hold every run of the inventory the base build did not delete" "$(tr '\n' ' ' <"$work/lost.h3")"
pass H3 "the store and the going tables still hold the $(jq 'length' "$work/inventory.h3.json") runs of the inventory the base build did not delete"

# A node at proposed, settled or overtaken is compared with its row like a live one (R12): alice
# moves her pre-mortem to proposed, the trade-off analysis's finding to proposed and then settled,
# and her risk to overtaken, and the census below counts each among the nodes of its kind and
# reads zero differences.
readonly finding_store="${finding_node/\#b-/#}"
sign_in H3 "$alice" " at $base"
move_state H3 "$premortem" "lens/$premortem_id" live proposed
move_state H3 "$finding" "$finding_store" live proposed
move_state H3 "$finding" "$finding_store" proposed settled
move_state H3 "$risk" "risk/$risk_id" live overtaken

take_census C h3
census_zero C h3 interaction
census_zero C h3 frame
census_zero C h3 block
census_zero C h3 block_ref
census_zero C h3 person
census_zero C h3 session
for kind in interaction lens finding risk constraint lens_kind; do
  census_zero C h3 "${kind}_visibility_state"
done
census_differences C h3
# The census counted each moved node as it counts a live one: the nodes of the kind it checked are
# every node of the kind not gone, more than those at live.
for moved in "lens|lens|$premortem_id|proposed" "finding|block|$finding_store|settled" "risk|risk|$risk_id|overtaken"; do
  IFS='|' read -r kind node_kind id state <<<"$moved"
  narrow="kind = '$node_kind'"
  [[ "$kind" != finding ]] || narrow="$narrow and id like 'lens/%'"
  query C "count the $kind nodes not gone and those at live" \
    "select count(*) filter (where state <> 'gone') || '|' || count(*) filter (where state = 'live') || '|'
            || coalesce(max(state) filter (where id = '$id'), 'none')
     from graph_nodes where $narrow"
  IFS='|' read -r present live held <<<"$answer"
  census_kind "$work/census.h3" "${kind}_visibility_state" ||
    fail C "the census reads the kind ${kind}_visibility_state" "no line of it in: $(<"$work/census.h3")"
  [[ "$held" == "$state" && "$c_nodes" == "$present" && "$present" -gt "$live" ]] ||
    fail C "the census counts the $kind $id at $state among the nodes of its kind, as it counts a live one" \
      "the store holds it at $held, $present $kind node(s) are not gone and $live are live, and the census checked $c_nodes"
  pass C "the census counts the $kind $id at $state among the $c_nodes $kind node(s) it checked, every one not gone, $live of them live, at zero differences"
done
want_value C "every interaction node is at visibility author" "0|true" \
  "select count(*) filter (where visibility <> 'author') || '|' || (count(*) > 0)::text
   from graph_nodes where kind = 'interaction'"
want_value C "the interaction of run $run_dated, whose row is gone, is gone and no difference" "gone" \
  "select state from graph_nodes where kind = 'interaction' and id = '$run_dated'"
# The person of the identity table who asks the command bar and writes no comment.
want_value C "$carol writes no comment" 0 \
  "select count(*) from comments where user_threads_login(coalesce(author->>'owner', author->>'id', '')) = '$carol'"
want_value C "the store holds one person node for every spelling of $carol, one for $twin, and the session proved hers" "1|1|1" \
  "select (select count(*) from graph_nodes where kind = 'person' and lower(btrim(id)) = '$carol') || '|'
          || (select count(*) from graph_nodes where kind = 'person' and id = '$twin') || '|'
          || (select count(*) from graph_nodes where kind = 'session' and id = '$carol_session')"
# The census's person and session node sides are every node of each kind (graphcensus's person and
# session kinds), so had a row side not held $carol, $twin or $carol_session, whose nodes the check
# above read, that node would be a node without a row, on which census_zero above fails.
pass C "the census reads no person or session node without a row while $carol, $twin and her session $carol_session are nodes, so its row sides hold all three too: each is on both sides"
want_value C "the store still holds the node of the session $bearer_session, the writer the lens on the shared bearer names" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$bearer_session'"
# As at H2: the census's session node side is every session node, so a session node off its row
# side would be a node without a row, on which census_zero above fails.
pass C "the census reads no session node without a row while $bearer_session is a session node, so its row side still holds that session: it is on both sides"

# The census beside two differences planted after its zero reading (Verification v53): with psql,
# one interaction node no going_requests row projects - run $run_bob's node copied under an id no
# row names, every column of its row and every value tuple the store holds of a run but its frames
# (its visibility, its state, its words and its ends_at), so the census of a run's visibility and
# state reads the copy as it reads any run - and one frame value no going_events row projects, of
# run $run_ahead, whose one going_events row is its terminal frame $ahead_frame, at the sequence
# after that frame's, stated by the writer of that frame's value. The census names each as one
# difference of its kind naming its run, two in all: the copy, a node no row projects, and the row
# of run $run_ahead's frame $ahead_frame, whose node's frames no longer digest as its rows do. Both
# are deleted, exactly as planted, and the census reads zero again.
want_value C "run $run_ahead holds one going_events row, its terminal frame $ahead_frame, and its node one frame value" "1|1" \
  "select (select count(*) from going_events where request_id = '$run_ahead') || '|'
          || (select count(distinct t.to_id) from graph_tuples t
              where t.from_kind = 'interaction' and t.from_id = '$run_ahead' and t.kind = 'frame' and t.to_kind = 'value' and t.asserted)"
with_vars C "plant an interaction node no going_requests row projects, a copy of run $run_bob's node" "
  with planted as (select gen_random_uuid()::text as id),
  node as (
    insert into graph_nodes
    select copied.*
    from graph_nodes n cross join planted
    cross join lateral jsonb_populate_record(null::graph_nodes, to_jsonb(n) || jsonb_build_object('id', planted.id)) copied
    where n.kind = 'interaction' and n.id = :'source'
    returning id
  ),
  tuples as (
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
    select t.from_kind, node.id, t.kind, t.to_kind, t.to_id, t.at, t.who, t.source_event, t.legacy_source_seq, t.asserted
    from graph_tuples t cross join node
    where t.from_kind = 'interaction' and t.from_id = :'source' and t.to_kind = 'value' and t.kind <> 'frame'
    returning seq
  )
  select node.id || '|' || (select string_agg(seq::text, ',' order by seq) from tuples) from node" "source=$run_bob"
IFS='|' read -r planted_run planted_seqs <<<"$answer"
[[ "$planted_run" =~ ^[0-9a-f-]{36}$ && "$planted_seqs" =~ ^[0-9]+(,[0-9]+)*$ ]] ||
  fail C "plant an interaction node no going_requests row projects, a copy of run $run_bob's node" "psql answered '$answer'"
readonly planted_run planted_seqs
want_value C "no going_requests row names the planted run $planted_run, and its node holds a visibility and a state tuple" "0|1|1" \
  "select (select count(*) from going_requests where id::text = '$planted_run') || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$planted_run' and kind = 'visibility') || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$planted_run' and kind = 'state')"
with_vars C "plant a frame value of run $run_ahead that no going_events row projects" "
  insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
  select 'interaction', :'run', 'frame', 'value',
         jsonb_build_object('sequence', max(e.sequence) + 1, 'name', 'end',
                            'data', to_jsonb('A frame value no going_events row projects.'::text))::text,
         now(),
         (select t.who from graph_tuples t
          where t.from_kind = 'interaction' and t.from_id = :'run' and t.kind = 'frame' order by t.seq limit 1),
         0, null, true
  from going_events e where e.request_id = :'run'::uuid
  returning seq || '|' || (to_id::jsonb ->> 'sequence')" "run=$run_ahead"
IFS='|' read -r planted_frame_seq planted_frame <<<"$answer"
[[ "$planted_frame_seq" =~ ^[0-9]+$ && "$planted_frame" == $((ahead_frame + 1)) ]] ||
  fail C "plant a frame value of run $run_ahead at sequence $((ahead_frame + 1)), which no going_events row holds" "psql answered '$answer'"
readonly planted_frame_seq planted_frame
pass C "planted the interaction node $planted_run, a copy of run $run_bob's node under an id no row names, and a frame value of run $run_ahead at sequence $planted_frame, which no going_events row holds"

take_census C plants
census_kind "$work/census.plants" interaction ||
  fail C "the census reads the kind interaction" "no line of it in: $(<"$work/census.plants")"
found="$(grep -E '^census: store kind=interaction difference at ' "$work/census.plants" || true)"
want_re="^census: store kind=interaction difference at interaction $planted_run: an? [a-z]+ node no row projects\$"
[[ "$c_row_differences" == 0 && "$c_node_differences" == 1 && "$found" =~ $want_re ]] ||
  fail C "the census reads the planted node as one difference of the kind interaction, naming the run $planted_run" \
    "$c_row_differences row(s) without a node and $c_node_differences node(s) without a row: ${found:-no difference named}"
pass C "the census reads the planted node as one difference of the kind interaction, naming the run: $found"
census_kind "$work/census.plants" frame ||
  fail C "the census reads the kind frame" "no line of it in: $(<"$work/census.plants")"
found="$(grep -E '^census: store kind=frame difference at ' "$work/census.plants" || true)"
want_re="^census: store kind=frame difference at frame $run_ahead $ahead_frame: the row and its node disagree: the row digests [0-9a-f]{32} and the node [0-9a-f]{32}\$"
[[ "$c_row_differences" == 1 && "$c_node_differences" == 0 && "$found" =~ $want_re ]] ||
  fail C "the census reads the planted frame value as one difference of the kind frame, naming the run $run_ahead" \
    "$c_row_differences row(s) without a node and $c_node_differences node(s) without a row: ${found:-no difference named}"
pass C "the census reads the planted frame value as one difference of the kind frame, naming the run: $found"
census_zero C plants interaction_visibility_state
line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.plants")" ||
  fail C "the census reads its total of differences" "no total in: $(<"$work/census.plants")"
[[ "$line" =~ differences=2$ ]] ||
  fail C "the census reads the two planted differences and no other" "$line: $(grep -E '^census: store kind=[a-z_]+ difference' "$work/census.plants" | tr '\n' ' ')"
pass C "the census reads the two planted differences and no other: $line"

IFS=, read -r -a planted_tuples <<<"$planted_seqs,$planted_frame_seq"
with_vars C "delete the planted node, its tuples and the planted frame value" "
  with tuples as (
    delete from graph_tuples where seq = any(string_to_array(:'seqs', ',')::bigint[]) returning 1
  ),
  node as (
    delete from graph_nodes where kind = 'interaction' and id = :'planted' returning 1
  )
  select (select count(*) from tuples) || '|' || (select count(*) from node)" \
  "seqs=$planted_seqs,$planted_frame_seq" "planted=$planted_run"
[[ "$answer" == "${#planted_tuples[@]}|1" ]] ||
  fail C "delete exactly the ${#planted_tuples[@]} planted tuples and the planted node" "psql deleted '$answer' (tuples|nodes)"
want_value C "no node and no tuple names the planted run $planted_run, and run $run_ahead's node holds its one frame value again" "0|0|1" \
  "select (select count(*) from graph_nodes where kind = 'interaction' and id = '$planted_run') || '|'
          || (select count(*) from graph_tuples where (from_kind = 'interaction' and from_id = '$planted_run')
                                                   or (to_kind = 'interaction' and to_id = '$planted_run')) || '|'
          || (select count(distinct t.to_id) from graph_tuples t
              where t.from_kind = 'interaction' and t.from_id = '$run_ahead' and t.kind = 'frame' and t.to_kind = 'value' and t.asserted)"
take_census C unplanted
census_zero C unplanted interaction
census_zero C unplanted frame
census_zero C unplanted interaction_visibility_state
census_differences C unplanted

# --- S: the store-only kinds against the inventory of the writes and the migration's seed ---------

query S "read the store-only kinds and their tuples" "with current as (
    select distinct on (t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id)
           t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted
    from graph_tuples t
    where t.kind in ('constrained_by', 'has_kind', 'triggered_by', 'prevented_by')
    order by t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.seq desc
  ),
  stated as (
    select c.kind, c.from_kind || ':' || c.from_id || ' ' || c.kind || ' ' || c.to_kind || ':' || c.to_id as tuple
    from current c where c.asserted
  ),
  nodes as (
    select case when n.kind = 'block' then 'finding' else n.kind end as kind, n.id
    from graph_nodes n
    where n.state <> 'gone'
      and (n.kind in ('lens', 'risk', 'constraint', 'lens_kind') or (n.kind = 'block' and n.id like 'lens/%'))
  )
  select json_object_agg(listed.kind, listed.items) from (
    select kinds.kind, coalesce((select json_agg(nodes.id order by nodes.id) from nodes where nodes.kind = kinds.kind), '[]') as items
    from unnest(array['lens', 'finding', 'risk', 'constraint', 'lens_kind']) kinds (kind)
    union all
    select kinds.kind, coalesce((select json_agg(stated.tuple order by stated.tuple) from stated where stated.kind = kinds.kind), '[]')
    from unnest(array['constrained_by', 'has_kind', 'triggered_by', 'prevented_by']) kinds (kind)
  ) listed"
printf '%s\n' "$answer" >"$work/store-only.json"
for kind in lens finding risk constraint lens_kind constrained_by has_kind triggered_by prevented_by; do
  want="$(jq -c --arg k "$kind" '.[$k] | sort' "$work/writes.json")"
  got="$(jq -c --arg k "$kind" '.[$k] | sort' "$work/store-only.json")"
  [[ "$got" == "$want" ]] ||
    fail S "the store's $kind is the inventory's" "the store holds $got, the inventory of the writes and the seed $want"
  pass S "the store holds the $(jq 'length' <<<"$want") $kind the inventory of the routes' writes and the migration's seed holds, and no other"
done

# --- I: the identity table against each made_by, authored_by and author --------------------------

for entry in "${identity_runs[@]}"; do
  IFS='|' read -r id form wanted person what <<<"$entry"
  with_vars I "read run $id" \
    "select json_build_object('made_by', n.made_by, 'author', r.author,
       'authored_by', (select coalesce(json_agg(held.to_kind || ':' || held.to_id order by held.to_id), '[]') from (
          select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
          where t.from_kind = 'interaction' and t.from_id = n.id and t.kind = 'authored_by'
          order by t.to_kind, t.to_id, t.seq desc) held where held.asserted),
       'person', (select count(*) from graph_nodes p where p.kind = 'person' and p.id = :'person'),
       'alias', (select count(*) from graph_tuples a where a.from_kind = 'session' and a.from_id = split_part(:'wanted', ':', 2)
                   and a.kind = 'alias_of' and a.to_kind = 'person' and a.to_id = :'person' and a.asserted))
     from graph_nodes n left join going_requests r on r.id::text = n.id
     where n.kind = 'interaction' and n.id = :'id'" "id=$id" "person=$person" "wanted=$wanted"
  [[ -n "$answer" ]] || fail I "the store holds run $id ($what)" "no interaction node of it"
  printf '%s\n' "$answer" >"$work/identity.json"
  jq -e --argjson form "$form" '.made_by == $form' "$work/identity.json" >/dev/null ||
    fail I "run $id's made_by is the form the table wrote ($what)" "made_by $(jq -c '.made_by' "$work/identity.json"), the table $form"
  jq -e --argjson form "$form" '.author == $form' "$work/identity.json" >/dev/null ||
    fail I "run $id's going_requests.author, which projects its made_by, is the form the table wrote ($what)" \
      "author $(jq -c '.author' "$work/identity.json"), the table $form"
  jq -e --arg w "$wanted" '.authored_by == [$w] and .person == 1' "$work/identity.json" >/dev/null ||
    fail I "run $id ($what) is authored by $wanted, and $person is a person node" "$(<"$work/identity.json")"
  [[ "$wanted" != session:* ]] || jq -e '.alias == 1' "$work/identity.json" >/dev/null ||
    fail I "the session $wanted is tied to $person by alias_of" "$(<"$work/identity.json")"
  pass I "$what: run $id's made_by and author are $form, and its authored_by names $wanted, $person"
done

# --- G: no reconcile or rebuild of these kinds, beside a planted one -----------------------------

readonly name_pattern='(reconcile|rebuild)[-_.a-z0-9]*(interaction|lens|risk|constraint)|(interaction|lens|risk|constraint)[-_.a-z0-9]*(reconcile|rebuild)'
# named_in TREE: every name TREE's packages/envoy/cmd/dispatch and packages/envoy/scripts hold - the
# path of every file and directory under them, and every subcommand the table in cmd/dispatch
# declares - into $named, and each of them whose name holds the pattern into $hits.
named_in() {
  local path name
  named=()
  hits=()
  while IFS= read -r path; do
    named+=("file $path")
  done < <(cd -- "$1/packages/envoy" && find cmd/dispatch scripts -mindepth 1 -print)
  while IFS= read -r name; do
    named+=("subcommand $name")
  done < <(sed -n 's/^[[:space:]]*{"\([a-z0-9-]*\)", func.*/\1/p' "$1"/packages/envoy/cmd/dispatch/*.go)
  for name in "${named[@]}"; do
    path="${name#* }"
    path="${path##*/}"
    [[ ! "${path,,}" =~ $name_pattern ]] || hits+=("$name")
  done
}
mkdir -p "$work/planted/packages/envoy/cmd"
cp -R "$tree/packages/envoy/cmd/dispatch" "$work/planted/packages/envoy/cmd/" ||
  fail G "copy packages/envoy/cmd/dispatch and packages/envoy/scripts under $work/planted" "cp failed"
cp -R "$tree/packages/envoy/scripts" "$work/planted/packages/envoy/" ||
  fail G "copy packages/envoy/cmd/dispatch and packages/envoy/scripts under $work/planted" "cp failed"
printf '#!/usr/bin/env bash\n# Rebuilds the lens nodes from a projection: planted for the grep below.\n' \
  >"$work/planted/packages/envoy/scripts/rebuild-lens.sh"
printf 'G: the pattern, over each name'"'"'s last path element in lower case: %s\n' "$name_pattern"
named_in "$work/planted"
printf 'G: the copy %s with a planted scripts/rebuild-lens.sh: %d name(s) read, %d hit(s): %s\n' \
  "$work/planted" "${#named[@]}" "${#hits[@]}" "${hits[*]:-none}"
[[ "${#hits[@]}" == 1 && "${hits[0]}" == "file scripts/rebuild-lens.sh" ]] ||
  fail G "the grep finds the planted rebuild-lens.sh in the copy, and nothing else" "it found ${#hits[@]}: ${hits[*]:-none}"
pass G "the grep finds the planted scripts/rebuild-lens.sh in the copy under $work/planted"
named_in "$tree"
printf 'G: this tree, %s: %d name(s) read, %d hit(s): %s\n' "$tree" "${#named[@]}" "${#hits[@]}" "${hits[*]:-none}"
[[ " ${named[*]} " == *" subcommand lens-kind-add "* && " ${named[*]} " == *" subcommand backfill-graph-store "* ]] ||
  fail G "the grep reads the subcommand table of cmd/dispatch" "it read no lens-kind-add and no backfill-graph-store"
[[ "${#hits[@]}" == 0 ]] ||
  fail G "no subcommand and no file of packages/envoy/cmd/dispatch or packages/envoy/scripts holds reconcile or rebuild beside interaction, lens, risk or constraint" \
    "${hits[*]}"
pass G "no subcommand and no file of packages/envoy/cmd/dispatch or packages/envoy/scripts holds reconcile or rebuild beside interaction, lens, risk or constraint (0 of ${#named[@]} names)"

# --- P: the predicate's sets, against the store's own -------------------------------------------

sign_in P "$alice" " at $base"
sign_in P "$bob" " at $base"
# Every node of this child's kinds the store holds present, its address, and whether the store's
# columns alone admit it to :'person': a node at project, or one reserved for its author whose
# made_by names that person - the person a credential proves for a lens, a finding, a risk and a
# constraint, and the whole credential, a cookie's user, for a run. A lens kind is project's.
readonly candidates_sql="select coalesce(json_agg(json_build_object(
    'node', n.kind || ':' || n.id,
    'address', case when n.kind = 'block' then 'dispatch://' || split_part(n.id, '#', 1) || '#b-' || split_part(n.id, '#', 2)
                    else 'dispatch://' || n.kind || '/' || n.id end,
    'admits', n.visibility = 'project' or (n.visibility = 'author' and case
        when n.kind = 'lens_kind' then false
        when n.kind = 'interaction' then n.made_by->>'kind' = 'user' and n.made_by->>'id' = :'person'
             and n.made_by->'owner' is null and n.made_by->'service' is null
        else user_threads_login(coalesce(case when n.made_by->>'kind' = 'user' then n.made_by->>'id' end,
                                         n.made_by->>'owner', '')) = :'person'
      end)) order by n.kind, n.id), '[]')
  from graph_nodes n
  where n.state <> 'gone'
    and (n.kind in ('lens', 'risk', 'constraint', 'lens_kind', 'interaction') or (n.kind = 'block' and n.id like 'lens/%'))"
for person in "$alice" "$bob"; do
  with_vars P "read the store's nodes of this child's kinds" "$candidates_sql" "person=$person"
  printf '%s\n' "$answer" >"$work/candidates.$person.json"
  : >"$work/predicate.$person"
  while IFS=$'\t' read -r node address; do
    request "cookie:$person" GET "/api/v1/references?to=$(q "$address")"
    case "$status" in
      200) printf '%s\n' "$node" >>"$work/predicate.$person" ;;
      404) ;;
      *) fail P "$person reads $address" "HTTP $status: $(body)" ;;
    esac
  done < <(jq -r '.[] | [.node, .address] | @tsv' "$work/candidates.$person.json")
  jq -r '.[] | select(.admits) | .node' "$work/candidates.$person.json" >"$work/store.$person"
  [[ "$(<"$work/predicate.$person")" == "$(<"$work/store.$person")" ]] ||
    fail P "the nodes the predicate admits to $person are the nodes the store alone admits" \
      "the predicate only: $(comm -23 <(sort "$work/predicate.$person") <(sort "$work/store.$person") | tr '\n' ' '); the store only: $(comm -13 <(sort "$work/predicate.$person") <(sort "$work/store.$person") | tr '\n' ' ')"
  pass P "the predicate admits $(wc -l <"$work/predicate.$person") of $(jq 'length' "$work/candidates.$person.json") nodes to $person, the set the store's columns alone admit"
done
own_alice="$(comm -23 <(sort "$work/predicate.$alice") <(sort "$work/predicate.$bob") | tr '\n' ' ')"
own_bob="$(comm -13 <(sort "$work/predicate.$alice") <(sort "$work/predicate.$bob") | tr '\n' ' ')"
[[ " $own_alice " == *" lens:$premortem_id "* && " $own_alice " == *" risk:$risk_id "* && " $own_alice " == *" interaction:$run_alice "* &&
  " $own_bob " == *" constraint:$constraint_id "* && " $own_bob " == *" interaction:$run_bob "* ]] ||
  fail P "each person's set holds what that person alone wrote reserved for its author" "$alice alone: $own_alice; $bob alone: $own_bob"
pass P "$alice alone reads $own_alice; $bob alone reads $own_bob"

# --- M: the .census.sql of each migration this head applied ---------------------------------------

censused=0
for version in $head_migrations; do
  files=("$tree/packages/envoy/internal/dispatch/store/migrations/${version}_"*.census.sql)
  [[ -f "${files[0]}" ]] || fail M "migration $version has a .census.sql" "none in $tree/packages/envoy/internal/dispatch/store/migrations"
  psql_run M "run ${files[0]##*/}" -f "${files[0]}"
  [[ "$(<"$work/psql.out")" == 0 ]] || fail M "${files[0]##*/} reads 0" "it read: $(<"$work/psql.out")"
  pass M "${files[0]##*/}, the select run before a deploy, reads 0"
  censused=$((censused + 1))
done
[[ "$censused" -gt 0 ]] || fail M "run this head's migrations' .census.sql" "this head applied no migration the base build had not"

# --- E: the erase of the run the edit was made under ------------------------------------------------

# No server runs from here on, so no tick writes a row between the listing and its confirmation.
stop_server E "this head's server"
want_value E "before the erase, run $run_edit holds its row, its frames, its node and the edited blocks' changed_by_run" "1|true|1|${#changed_blocks[@]}" \
  "select (select count(*) from going_requests where id = '$run_edit') || '|'
          || ((select count(*) from going_events where request_id = '$run_edit') > 0)::text || '|'
          || (select count(*) from graph_nodes where kind = 'interaction' and id = '$run_edit') || '|'
          || (select count(*) from graph_tuples where kind = 'changed_by_run' and to_kind = 'interaction' and to_id = '$run_edit')"
run_cli E "list the erase of interaction:$run_edit" erase --node "interaction:$run_edit"
cp "$work/cli.out" "$work/erase.listing"
sed 's/^/  /' "$work/erase.listing"
[[ "$cli_code" == 1 ]] || fail E "the erase without --confirm lists and removes nothing" "exited $cli_code"
digest="$(sed -n 's/^  listing digest: \([0-9a-f]\{64\}\)$/\1/p' "$work/erase.listing")"
[[ -n "$digest" ]] || fail E "read the listing's digest" "no listing digest in: $(<"$work/erase.listing")"
! grep -q '^  blocked: ' "$work/erase.listing" ||
  fail E "the edited blocks' changed_by_run tuples are no blocker of the erase" "$(grep '^  blocked: ' "$work/erase.listing")"
for changed in "${changed_blocks[@]}"; do
  grep -Fq ": $changed changed_by_run interaction:$run_edit" "$work/erase.listing" ||
    fail E "the listing deletes the changed_by_run tuple from $changed with the node" "$(<"$work/erase.listing")"
done
grep -Eq "^    delete graph_tuples [0-9]+: block:$key/spec#r2 changed_by_run interaction:$run_edit\$" "$work/erase.listing" ||
  fail E "the listing deletes the changed_by_run tuple from the edited block r2 with the node" "$(<"$work/erase.listing")"
grep -Fqx "    delete going_requests (id)=($run_edit)" "$work/erase.listing" ||
  fail E "the listing deletes run $run_edit's going_requests row" "$(<"$work/erase.listing")"
pass E "the listing (digest $digest) names no blocker, and deletes the changed_by_run tuple of each of ${changed_blocks[*]} and the run's row with the node"
run_cli E "erase interaction:$run_edit" erase --node "interaction:$run_edit" --confirm "$digest"
sed 's/^/  /' "$work/cli.out"
[[ "$cli_code" == 0 ]] || fail E "envoy-dispatch erase --node interaction:$run_edit --confirm $digest erases the run" "exited $cli_code: $(<"$work/cli.out")"
pass E "envoy-dispatch erase --node interaction:$run_edit --confirm $digest erased the run"
readonly erased_sql="select (select count(*) from going_requests where id = '$run_edit') || '|'
          || (select count(*) from going_events where request_id = '$run_edit') || '|'
          || (select count(*) from graph_nodes where kind = 'interaction' and id = '$run_edit') || '|'
          || (select count(*) from graph_tuples where (from_kind = 'interaction' and from_id = '$run_edit')
                                                 or (to_kind = 'interaction' and to_id = '$run_edit'))"
want_value E "no row of run $run_edit in going_requests or going_events, no node and no tuple is left" "0|0|0|0" "$erased_sql"
changed_ids="$(printf "'%s'," "${changed_blocks[@]#block:}")"
want_value E "each edited block, ${changed_blocks[*]}, stands live, its changed_by_run gone with the run" "${#changed_blocks[@]}" \
  "select count(*) from graph_nodes where kind = 'block' and state = 'live' and id in (${changed_ids%,})"
backfill E
want_value E "the backfill writes no node of the erased run back" "0|0|0|0" "$erased_sql"
take_census E erased
census_zero E erased interaction
census_zero E erased frame
census_zero E erased block
census_zero E erased block_ref
census_zero E erased person
census_zero E erased session
for kind in interaction lens finding risk constraint lens_kind; do
  census_zero E erased "${kind}_visibility_state"
done
census_differences E erased

pass "$driver" complete
