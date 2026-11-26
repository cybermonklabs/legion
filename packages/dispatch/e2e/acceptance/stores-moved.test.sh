#!/usr/bin/env bash
# Tests of stores-moved.sh's parsers: each runs over each fixture under testdata/stores-moved/, with
# a near miss beside each that must not match. It sources the driver, which runs no step when
# sourced, and needs no database and no server.
#
#   bash e2e/acceptance/stores-moved.test.sh        (from packages/dispatch; prints one line a check
#                                                    and a last line `stores-moved.test.sh: N checks
#                                                    passed`, or exits 1 naming the failures)
#
# The fixtures were captured from the real systems before the parsers were written, on the pod's
# test PostgreSQL 18.7, with the base build of 93625c08 (the line's commit before R32) and this
# head's build of 1de3a542, each `go build -ldflags "-X main.buildCommit=<commit>" ./cmd/dispatch`
# and booted as lib-baseline.sh boots a server, over databases of the capture's own:
#
#   locks-wait-on-graph-nodes.csv, locks-wait-on-priority-nodes.csv: the move's backend
#       (`select pid from pg_stat_activity where application_name = 'envoy-dispatch stores move'`)
#       during the roll's swap, with the driver's four transactions open:
#       psql --csv -c "select l.relation::regclass::text as relation, l.mode, l.granted from pg_locks l
#                      where l.pid = <pid> and l.locktype = 'relation' order by 1, 2"
#       once while the swap waited on graph_nodes behind the create shape, and once while it held
#       graph_nodes and waited on priority_nodes behind the patch shape.
#   advisory-held.csv, advisory-waiting.csv: the rows of pg_locks of the advisory lock stores-moved
#       (`locktype = 'advisory' and objsubid = 1 and ((classid::bigint << 32) | objid::bigint) =
#       hashtext('stores-moved')::bigint`), with one server holding it under
#       DISPATCH_TEST_STORES_MOVE_HOLD_FILE, and with a second server waiting on it.
#   head-roll.txt: this head's boot log over the roll, at $baseline_work/logs/<name>.log, holding an
#       attempt of the swap that gave up, the census before the drop and the move's end.
#   head-first.txt, head-second.txt: the two runners over the database of a stopped move, the first
#       behind a session holding access share on priority_nodes, as the nightly dump does, and the
#       second waiting on stores-moved and then moving nothing.
#   head-last.txt: a boot with DISPATCH_TEST_STORES_MOVE_SWAP_ATTEMPTS=1 behind that session.
#   head-stopped.txt: a boot with DISPATCH_TEST_STORES_MOVE_BATCH=2 and _STOP_AFTER=3.
#   head-deadlock.txt: the fourteenth control's build (this head with the swap's
#       `set local lock_timeout` taken out, in a scratch copy) behind the patch shape, which then
#       writes graph_nodes behind the swap.
#   base-handler-failure.txt: the base build's log, its issue list read with advances=any over a
#       moved database whose view priority_tuples was dropped: the release after's drop, read by a
#       build that still reads the old name, which is the seventh control's failure. The handler's
#       log line (api/handler_errors.go) is the same code in both builds.
#   census-*.out: this head's `envoy-dispatch census` over the roll's database: the planted plan
#       (census-planted.out), the rehearsal held with DISPATCH_TEST_STORES_REHEARSAL_HOLD_FILE
#       (census-held.out), stopped with DISPATCH_TEST_STORES_REHEARSAL_STOP=2s (census-stopped.out),
#       with the row of 20261109051232_graph_supertags deleted (census-pending-supertags.out), with
#       the row of 20261121060636_search_document_text deleted (census-pending-search-text.out),
#       with DISPATCH_TEST_STORES_REHEARSAL_EXTRA_MIGRATION naming a file that replaces
#       block_schema_changed (census-extra-migration.out), and with
#       DISPATCH_TEST_STORES_MOVE_EXPLAIN=1 (census-explain.out).
#   head-migrations.txt, read-priorities.json, read-history.json: captured after the rest, before
#       boot_seconds read the line that begins the boot's migrations, from this head's build with
#       the boot's two lines of its migrations (cmd/dispatch/main.go; the working copy above
#       50c8c8db, stamped with that commit), over an empty database of the capture's own with
#       DISPATCH_TEST_HOOKS=1 and DISPATCH_TEST_STORES_MOVE_HOLD_FILE. The move held after the
#       migrations ended while one priority, held-open-capture, was written by SQL as t1_priority
#       writes one, and then the hold file was made. They are that boot's log, and its answers to
#       GET /api/v1/priorities and GET /api/v1/priorities/held-open-capture/history.
#
# A scratch path of the capture's own in a fixture is written /tmp/tmp.capture or
# /tmp/dispatch-baseline.capture; nothing else in them was changed. A near miss is the captured text
# with one row or line changed, built below beside the check it serves.

set -euo pipefail
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR source=stores-moved.sh
. "$here/stores-moved.sh"
set +e +o pipefail
data="$here/testdata/stores-moved"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

checks=0
failures=()
# is NAME GOT WANT: GOT equals WANT.
is() {
  checks=$((checks + 1))
  if [[ "$2" == "$3" ]]; then
    printf 'ok: %s\n' "$1"
  else
    printf 'FAIL: %s: got %q, want %q\n' "$1" "$2" "$3"
    failures+=("$1")
  fi
}
# holds NAME GOT WORDS: GOT holds WORDS.
holds() {
  checks=$((checks + 1))
  if [[ -n "$3" && "$2" == *"$3"* ]]; then
    printf 'ok: %s\n' "$1"
  else
    printf 'FAIL: %s: got %q, which does not hold %q\n' "$1" "$2" "$3"
    failures+=("$1")
  fi
}

# --- pg_locks of the swap ---------------------------------------------------------------------
waits_graph="$(<"$data/locks-wait-on-graph-nodes.csv")"
waits_priority="$(<"$data/locks-wait-on-priority-nodes.csv")"
is "the swap waits on graph_nodes" "$(lock_waited "$waits_graph")" graph_nodes
is "waiting on graph_nodes, it holds graph_nodes in no mode" "$(lock_modes "$waits_graph" graph_nodes)" none
is "a wait on graph_nodes is no wait the rule judges" "$(swap_verdict "$waits_graph"; echo "exit $?")" "exit 0"
is "the swap waits on priority_nodes" "$(lock_waited "$waits_priority")" priority_nodes
is "waiting on priority_nodes, it holds graph_nodes in ShareRowExclusiveLock" "$(lock_modes "$waits_priority" graph_nodes)" ShareRowExclusiveLock
is "the captured wait on priority_nodes keeps the rule" "$(swap_verdict "$waits_priority"; echo "exit $?")" "exit 0"
# Near misses: the captured wait on priority_nodes with one row changed, as the thirteenth,
# fifteenth and sixteenth controls change it.
is "near miss: graph_nodes in access exclusive mode" \
  "$(swap_verdict "${waits_priority/ShareRowExclusiveLock/AccessExclusiveLock}"; echo "exit $?")" \
  $'lock modes: graph_nodes in AccessExclusiveLock\nexit 1'
is "near miss: no lock on graph_nodes at a wait on priority_nodes" \
  "$(swap_verdict "$(grep -v '^graph_nodes,' <<<"$waits_priority")"; echo "exit $?")" $'lock modes: graph_nodes in none\nexit 1'
is "near miss: an old table held in exclusive mode" \
  "$(swap_verdict "$(printf '%s\npriority_tuples,ExclusiveLock,f\n' "${waits_priority/priority_nodes,AccessExclusiveLock,f/priority_nodes,ExclusiveLock,t}")"; echo "exit $?")" \
  $'lock modes: priority_nodes in ExclusiveLock\nexit 1'
is "near miss: a wait on a table that is none of the five" \
  "$(swap_verdict "${waits_priority/priority_nodes,AccessExclusiveLock,f/priority_nodes_x,AccessExclusiveLock,f}"; echo "exit $?")" "exit 0"

# --- the advisory lock stores-moved -----------------------------------------------------------
is "one server holds stores-moved" "$(advisory_counts "$(<"$data/advisory-held.csv")")" "1 0"
is "a second server waits on stores-moved" "$(advisory_counts "$(<"$data/advisory-waiting.csv")")" "1 1"
is "near miss: no row of stores-moved" "$(advisory_counts "$(awk 'NR == 1' "$data/advisory-waiting.csv")")" "0 0"

# --- the head's boot log ----------------------------------------------------------------------
is "the roll's boot logs one attempt that gave up" "$(gave_up_lines "$data/head-roll.txt" | wc -l)" 1
holds "that line names the lock it waited for" "$(gave_up_lines "$data/head-roll.txt")" 'lock="priority_nodes in access exclusive mode"'
is "the first runner logs five attempts that gave up" "$(gave_up_lines "$data/head-first.txt" | wc -l)" 5
is "near miss: the second runner logs none" "$(gave_up_lines "$data/head-second.txt" | wc -l)" 0
holds "the last attempt's line" "$(last_attempt_line "$data/head-last.txt")" 'attempt=1 attempts=1'
is "near miss: an attempt that is not the last" "$(last_attempt_line "$data/head-first.txt")" ""
holds "the boot that met a deadlock logs it" "$(deadlock_line "$data/head-deadlock.txt")" 'ERROR: deadlock detected (SQLSTATE 40P01)'
is "near miss: the roll's boot meets none" "$(deadlock_line "$data/head-roll.txt" "$scratch/no-such-file")" ""
holds "the second runner moves nothing" "$(log_line "$data/head-second.txt" "$moved_nothing_words")" 'priority_nodes="a view"'
is "near miss: the first runner moves" "$(log_line "$data/head-first.txt" "$moved_nothing_words")" ""
holds "the second runner waits on stores-moved" \
  "$(log_line "$data/head-second.txt" 'another process holds the advisory lock stores-moved; the boot waits for it')" level=INFO
holds "the stopped boot says it stopped" "$(log_line "$data/head-stopped.txt" "$stopped_words")" level=ERROR
is "near miss: the roll's boot did not stop" "$(log_line "$data/head-roll.txt" "$stopped_words")" ""
holds "the boot logs where its migrations begin" "$(log_line "$data/head-migrations.txt" "$migrations_began_words")" level=INFO
holds "the boot logs where its migrations end, and how long they took" \
  "$(log_line "$data/head-migrations.txt" "msg=\"dispatch: the boot's migrations ended\"")" 'took=5.775224265s'
is "near miss: the roll's boot, a build before the two lines, logs no end of its migrations" \
  "$(log_line "$data/head-roll.txt" "msg=\"dispatch: the boot's migrations ended\"")" ""
# 22:19:17.749, the line that begins the migrations, to 22:19:23.806, the move's end; the boot's
# first line, at 22:19:17.733, is 16ms earlier, and would read 6.073s.
is "the boot took 6.057s from the line that begins its migrations to the move's end" "$(boot_seconds "$data/head-migrations.txt")" 6.057
{
  printf '%s\n' 'time=2026-11-23T21:19:17.733Z level=INFO msg="dispatch: NATS publisher disabled"'
  cat "$data/head-migrations.txt"
} >"$scratch/earlier-first.txt"
is "near miss: a first line an hour before the migrations changes nothing" "$(boot_seconds "$scratch/earlier-first.txt")" 6.057
grep -vF "$migrations_began_words" "$data/head-migrations.txt" >"$scratch/no-begin.txt"
is "near miss: without the line that begins its migrations there is no time, none from the first line" \
  "$(boot_seconds "$scratch/no-begin.txt"; echo "exit $?")" "exit 1"
is "near miss: the roll's boot, a build before the line that begins its migrations, has no time" \
  "$(boot_seconds "$data/head-roll.txt"; echo "exit $?")" "exit 1"
grep -vF "$move_ended_words" "$data/head-migrations.txt" >"$scratch/no-move-end.txt"
is "near miss: without the line that ends the move there is no time" "$(boot_seconds "$scratch/no-move-end.txt"; echo "exit $?")" "exit 1"

# --- the handler's failure --------------------------------------------------------------------
holds "the failure names priority_tuples" "$(handler_failure_line "$data/base-handler-failure.txt" priority_tuples)" \
  'relation \"priority_tuples\" does not exist (SQLSTATE 42P01)'
is "near miss: no failure names priority_nodes" "$(handler_failure_line "$data/base-handler-failure.txt" priority_nodes)" ""
handler_line="$(handler_failure_line "$data/base-handler-failure.txt" priority_tuples)"
printf '%s\n' "${handler_line//priority_tuples/priority_tuples_current}" >"$scratch/longer-name.txt"
is "near miss: a failure naming a longer relation" "$(handler_failure_line "$scratch/longer-name.txt" priority_tuples)" ""
printf '%s\n' "${handler_line/API handler failed/the outbox failed}" >"$scratch/another-line.txt"
is "near miss: another line naming priority_tuples" "$(handler_failure_line "$scratch/another-line.txt" priority_tuples)" ""

# --- the census's rehearsal -------------------------------------------------------------------
is "the held rehearsal's copy" "$(copy_ended "$data/census-held.out")" "60 21ms"
is "near miss: a rehearsal refused before its copy" "$(copy_ended "$data/census-pending-supertags.out")" ""
is "the held rehearsal's priority node rows" "$(rehearsal_count "$data/census-held.out" priority 'node rows')" "3 3 0"
is "the held rehearsal's priority tuples" "$(rehearsal_count "$data/census-held.out" priority tuples)" "23 23 0"
is "the held rehearsal's workflow node rows" "$(rehearsal_count "$data/census-held.out" workflow 'node rows')" "3 3 0"
is "the held rehearsal's workflow tuples" "$(rehearsal_count "$data/census-held.out" workflow tuples)" "37 37 0"
is "the planted rehearsal's workflow node rows" "$(rehearsal_count "$data/census-planted.out" workflow 'node rows')" "4 4 0"
is "near miss: no line of a store the census has not" "$(rehearsal_count "$data/census-held.out" decision tuples)" ""
holds "the planted plan is refused by its address" "$(rehearsal_refusal "$data/census-planted.out")" \
  'store=workflow node kind=plan id=d4a89772-872b-410a-8186-db89796fa04e'
holds "the stopped rehearsal refuses naming the stop" "$(rehearsal_refusal "$data/census-stopped.out")" 'it stopped after 2s'
is "near miss: the held rehearsal refuses nothing" "$(rehearsal_refusal "$data/census-held.out")" ""
is "the refused pending migration of the base" "$(refused_migrations "$data/census-pending-supertags.out")" \
  20261109051232_graph_supertags.up.sql
is "the refused extra migration" "$(refused_migrations "$data/census-extra-migration.out")" /tmp/tmp.capture/block_schema_changed.up.sql
is "near miss: a skipped migration is not refused" "$(refused_migrations "$data/census-pending-search-text.out")" ""
holds "the skipped migration of another tree" \
  "$(skipped_migration "$data/census-pending-search-text.out" 20261121060636_search_document_text.up.sql)" 'it touches none of'
is "near miss: the refused migration is not skipped" \
  "$(skipped_migration "$data/census-pending-supertags.out" 20261109051232_graph_supertags.up.sql)" ""
is "near miss: this issue's own skipped migration is no other tree's" \
  "$(skipped_migration "$data/census-held.out" 20261126000400_graph_nodes_artifact_id_index.up.sql)" ""
holds "a census over the old tables prints a rehearsal" "$(rehearsal_lines "$data/census-held.out")" 'census: rehearsal: no difference'
grep -v 'rehearsal' "$data/census-held.out" >"$scratch/no-rehearsal.out"
is "near miss: the census without its rehearsal's lines prints none" "$(rehearsal_lines "$scratch/no-rehearsal.out")" ""
is "the census's plans read graph_tuples by its indexes" "$(seq_scan_statement "$data/census-explain.out")" ""
awk '/plan of the census of the priority tuples:/ { on = 1 }
  on && /Index Scan using graph_tuples_as_of_out on graph_tuples/ { sub(/Index Scan using graph_tuples_as_of_out on graph_tuples/, "Seq Scan on graph_tuples"); on = 0 }
  { print }' "$data/census-explain.out" >"$scratch/seq-scan.out"
is "near miss: a plan that reads graph_tuples by Seq Scan" "$(seq_scan_statement "$scratch/seq-scan.out")" "priority tuples"

# --- the reads before and after the move -----------------------------------------------------
# The kept priorities read is read_fixtures': the list less each priority's work.
history="$data/read-history.json"
jq -S '.priorities | map(del(.work))' "$data/read-priorities.json" >"$scratch/priorities.json"
priorities="$scratch/priorities.json"
is "a read equals itself" "$(compare_read "$history" "$history" "" '[]')" ""
jq '.[0].proven = true' "$history" >"$scratch/history-added.json"
holds "near miss: a field the read after adds that added_fields does not name differs" \
  "$(compare_read "$history" "$scratch/history-added.json" "" '[]')" 'differs {"before":'
is "a field the read after adds that added_fields names is ignored" \
  "$(compare_read "$history" "$scratch/history-added.json" "rank proven" '[]')" ""
jq '.[0].proven = false' "$history" >"$scratch/history-held.json"
holds "near miss: a field added_fields names that the read before holds too still differs" \
  "$(compare_read "$scratch/history-held.json" "$scratch/history-added.json" "proven" '[]')" 'differs {"before":'
jq 'del(.[0])' "$history" >"$scratch/history-less.json"
holds "near miss: with no write after the inventory, an act whose issue is empty is no row left out" \
  "$(compare_read "$history" "$scratch/history-less.json" "" '[]')" 'differs {"before":'
jq '.[0].title = "Retitled"' "$priorities" >"$scratch/priorities-retitled.json"
is "a row whose id a write after the inventory touched is left out and named" \
  "$(compare_read "$priorities" "$scratch/priorities-retitled.json" "" '["held-open-capture"]')" "left held-open-capture"
holds "near miss: the same row, which no write touched, differs" \
  "$(compare_read "$priorities" "$scratch/priorities-retitled.json" "" '["another-priority"]')" '"title":"Retitled"'
jq 'map(. + {advances_revision: 3})' "$priorities" >"$scratch/priorities-revision-3.json"
jq 'map(. + {advances_revision: 9})' "$priorities" >"$scratch/priorities-revision-9.json"
is "advances_revision is no difference" \
  "$(compare_read "$scratch/priorities-revision-3.json" "$scratch/priorities-revision-9.json" "" '[]')" ""

if [[ "${#failures[@]}" -gt 0 ]]; then
  printf 'stores-moved.test.sh: %d of %d checks failed: %s\n' "${#failures[@]}" "$checks" "${failures[*]}"
  exit 1
fi
printf 'stores-moved.test.sh: %d checks passed\n' "$checks"
