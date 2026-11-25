# shellcheck shell=bash
# The cleanup trap and the helper functions of node-visibility-census.sh (check c-census), which
# sources this file from beside it, after lib-processes.sh. Sourced, never run. Its functions read
# and set the driver's own variables.

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

# take_census SECTION NAME EXIT: runs envoy-dispatch census --store, whole, keeps its report in
# $work/census.NAME and prints each kind's line, the store-only and project lines, the total of
# differences and the verdict. The command exits 0 when nothing refuses, 1 when the report refuses
# and 2 when it could not take the census (cmd/dispatch's census), and the report refuses on a
# difference, a project finding, a far end that does not resolve or a kind that checked no row
# (graphcensus's StoreReport.Refused). EXIT is 0 for a census of the store as its writers left it,
# every kind of it checked, whose verdict is `census: store ok`, and 1 for a census beside a
# planted difference, whose verdict is `census: store REFUSED (<n> difference(s))`, the reason the
# report names first.
take_census() {
  local verdict want
  run_cli "$1" "run envoy-dispatch census --store" census --store
  cp "$work/cli.out" "$work/census.$2"
  grep -E '^census: store (kind=[a-z_]+ (nodes|tuples)=|rows_checked=|ok|REFUSED|absent)|^census: store kind=[a-z_]+ (difference|and |holds no row|checked no row)|^census: (store-only|project) ' \
    "$work/census.$2" >"$work/census.$2.lines" ||
    fail "$1" "envoy-dispatch census --store prints its report" "it printed no census line (exit $cli_code): $(<"$work/census.$2")"
  sed 's/^/  /' "$work/census.$2.lines"
  verdict="$(grep -E '^census: store (ok|REFUSED|absent)' "$work/census.$2" | tail -n 1)" || verdict=""
  if [[ "$3" == 0 ]]; then
    want='^census: store ok$'
  else
    want='^census: store REFUSED \([0-9,]+ difference\(s\)\)$'
  fi
  [[ "$cli_code" == "$3" ]] ||
    fail "$1" "envoy-dispatch census --store, run whole, exits $3" "it exited $cli_code: ${verdict:-it printed no verdict}"
  [[ "$verdict" =~ $want ]] ||
    fail "$1" "envoy-dispatch census --store, run whole, ends on the verdict /$want/" "its verdict reads: ${verdict:-none}"
  pass "$1" "envoy-dispatch census --store, run whole, exited $3: $verdict"
}

# start_nats: builds natsd, packages/envoy's one nats-server for a shell script
# (internal/testnats/cmd/natsd, the nats-server its go.mod pins, with JetStream on 127.0.0.1), as
# scripts/e2e/dispatch-claimant-delivery.sh builds it for its Dispatch, and starts it on a port the
# kernel picks (-p -1), its store under $work. It sets $nats_url, and its pid joins the fakes'
# (pids), so stop_processes stops it with them. run-server.sh starts a server's events outbox only
# for a server it hands a NATS (DISPATCH_E2E_NATS_URLS), and that publisher is the one writer of a
# role's node and its page beside the row that addresses the role (outbox.deliveredGraph).
start_nats() {
  local pid line
  mkdir -p "$work/bin" "$work/nats-store"
  go -C "$tree/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd >"$work/natsd-build.log" 2>&1 ||
    fail setup "build packages/envoy's ./internal/testnats/cmd/natsd" "go build exited non-zero: $(tail -n 20 "$work/natsd-build.log")"
  "$work/bin/natsd" -p -1 -store_dir "$work/nats-store" >"$work/nats.log" 2>&1 &
  pid="$!"
  pids+=("$pid")
  await_line setup "start the NATS server" "$pid" "$work/nats.log" 'Server is ready'
  line="$(grep -m 1 -E 'Listening for client connections on 127\.0\.0\.1:[0-9]+$' "$work/nats.log")" ||
    fail_with_log setup "start the NATS server" "it logged no client port" "$work/nats.log"
  nats_url="nats://127.0.0.1:${line##*:}"
  [[ "$nats_url" =~ ^nats://127\.0\.0\.1:[0-9]+$ ]] || fail setup "start the NATS server" "its line reads: $line"
  readonly nats_url
  printf '%s: natsd, a NATS server with JetStream, listens on %s (pid %s)\n' "$driver" "$nats_url" "$pid"
}

# boot_head SECTION NAME: boots this head's server (boot_server) with the run's NATS as its
# DISPATCH_E2E_NATS_URLS, which run-server.sh hands it as NATS_URLS, so its events outbox
# publishes and writes what a delivery states in the store, as a deployment's does. The base build
# boots with none, in B1 through lib-baseline.sh and in B2 through boot_server.
boot_head() {
  DISPATCH_E2E_NATS_URLS="$nats_url" boot_server "$1" "$2" "$server_bin"
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

# census_kind FILE KIND: KIND's counts in the census report FILE, into $c_store, $c_rows,
# $c_row_differences and $c_store_differences, and into $c_side what its store side counts: node,
# or tuple for a kind the census holds triple by triple (graphcensus's KindReport.Triples), whose
# line counts tuples where every other kind's counts nodes.
census_kind() {
  local line
  line="$(grep -m 1 -E "^census: store kind=$2 (nodes|tuples)=" "$1")" || return 1
  [[ "$line" =~ (node|tuple)s=([0-9,]+)\ rows=([0-9,]+)\ .*\ rows_without_a_(node|tuple)=([0-9,]+)\ (node|tuple)s_without_a_row=([0-9,]+) ]] || return 1
  c_side="${BASH_REMATCH[1]}"
  c_store="${BASH_REMATCH[2]//,/}"
  c_rows="${BASH_REMATCH[3]//,/}"
  c_row_differences="${BASH_REMATCH[5]//,/}"
  c_store_differences="${BASH_REMATCH[7]//,/}"
}

# census_zero SECTION NAME KIND: the census NAME read KIND at zero differences in both directions,
# having checked more than zero rows of it; a kind that checked no row fails, named.
census_zero() {
  census_kind "$work/census.$2" "$3" || fail "$1" "the census reads the kind $3" "no line of it in: $(<"$work/census.$2")"
  [[ "$c_row_differences" == 0 && "$c_store_differences" == 0 ]] ||
    fail "$1" "the census reads $3 at zero differences" \
      "$c_row_differences row(s) without a $c_side and $c_store_differences $c_side(s) without a row: $(grep -E "^census: store kind=$3 " "$work/census.$2" | tr '\n' ' ')"
  [[ "$c_rows" -gt 0 ]] ||
    fail "$1" "the census checked more than zero rows of the kind $3" "it checked $c_rows rows of $3, so it proves nothing of it"
  pass "$1" "the census reads $3 at zero differences: it checked $c_rows row(s) against $c_store $c_side(s)"
}

# The eight kinds of the store cybermonklabs/legion#4173 adds: a decision's shaped, from_ask and
# decided_by, an ask's followers, a reading's parent, and an ask's placement, anchor and answer.
# The census refuses none of them for holding no row and no tuple (graphcensus's emptyAgrees: a
# deployment holds none until a decision is written or an ask answered), so a census that read
# one of them over no row would pass while proving nothing of it. H2 writes a row of each, and
# every census of the store as its writers left it reads each through census_zero, which fails
# on a kind that checked no row, naming it.
readonly -a decision_kinds=(shaped from_ask decided_by followed_by reading_parent ask_parent ask_anchor ask_answer)
# The six kinds the census holds of graph_edges' answer of those ties (graphcensus's
# edgeProjection, graphEdgesOut): shaped, from_ask and decided_by out of a block, shaped,
# followed_by and anchored_to out of an ask, and an ask's owned_by (ask_parent_edge), which only an
# ask a document owns states: an ask on no issue, whose asks.artifact_id names that document
# (graphstore.AskPlaces' Owner). The census refuses none of them for holding no row either, so H2
# writes such an ask, and every census that reads the eight reads these six through census_zero
# too, each named where it checked no row.
readonly -a edge_kinds=(shaped_edge from_ask_edge decided_by_edge followed_by_edge ask_parent_edge ask_anchor_edge)
# The fourteen kinds H2 gives a row each, decision_kinds and edge_kinds, which every census of the
# store as its writers left it reads through census_zero.
readonly -a populated_kinds=("${decision_kinds[@]}" "${edge_kinds[@]}")

# wait_settled SECTION WHAT: waits, 120 s at most, until no document owes a settlement
# (doc_settlements_pending) or its block index (a document_block_versions row with a null version),
# so the census that follows reads every write before it, as decision-nodes.sh waits.
readonly owed_sql="select (select count(*) from doc_settlements_pending)
  + (select count(*) from document_block_versions where version is null)"
wait_settled() {
  local waited=0
  query "$1" "read what the documents owe" "$owed_sql"
  until [[ "$answer" == 0 ]]; do
    [[ "$waited" -lt 120 ]] ||
      fail "$1" "$2: every document settles within 120 s" "$answer settlement(s) and block index(es) still owed"
    sleep 1
    waited=$((waited + 1))
    query "$1" "read what the documents owe" "$owed_sql"
  done
}

# wait_ask SECTION ISSUE BLOCK: waits, 60 s at most, for the one ask of ISSUE whose block is BLOCK,
# whose row settlement opens once the block is written, and sets $ask_id to its id.
wait_ask() {
  local waited=0
  while true; do
    request agent GET "/api/v1/issues/$2/asks?state=all"
    want_status 200 "$1" "read the asks of $2"
    ask_id="$(jq -r --arg b "$3" '[.[] | select(.block_id == $b)] | if length == 1 then .[0].id else "" end' "$work/body")" ||
      fail "$1" "read the asks of $2" "unreadable: $(body)"
    [[ -z "$ask_id" ]] || return 0
    [[ "$waited" -lt 60 ]] || fail "$1" "the ask of block $3 of $2 opens within 60 s" "no single ask of $2 names block $3: $(body)"
    sleep 1
    waited=$((waited + 1))
  done
}

# census_differences SECTION NAME: the census NAME read zero differences over every kind, on its total line,
# which ends with the read sequence its snapshot saw (`read_seq=`).
census_differences() {
  local line
  line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.$2")" ||
    fail "$1" "the census reads zero differences" "no total in: $(<"$work/census.$2")"
  [[ "$line" =~ differences=0\ read_seq=[0-9]+$ ]] ||
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
# ends_at value tuple together, written as graphstore.InteractionFacts writes that attribute, the
# writer through which the backfill gives each run its node (RFC 3339 in UTC with the fraction of
# a second time.RFC3339Nano writes, stated by the run's made_by and caused by no event), so the
# newest tuple and the column agree with the row.
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
