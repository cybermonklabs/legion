#!/usr/bin/env bash
# Drives a type of the store read in the write's own transaction (check c-schema-bump, spec
# requirement r-lens-catalogue). A type is a `supertag` row of the
# store, so a write of a node checks the node's type against the store inside its own transaction
# and no process holds a list of types read at boot. This driver starts one Dispatch server over an
# empty database of the run's own, which the server migrates at boot, and then, with that one
# process serving throughout:
#
#   - deletes the `lens` supertag row and the attribute rows of that supertag with psql, in one
#     transaction, and writes a lens under a spec through the server: the write is refused 409
#     SUPERTAG_NOT_HELD, the spec holds no lens, and the store holds nothing of the write - no
#     lens, no finding and no tuple - so neither the node nor the rest of the write was stored,
#     where a list of types read at boot would have taken it;
#   - writes the deleted rows back with psql, as they were, writes the same lens again through the
#     same server and reads it among the spec's lenses;
#   - reads the server's row of server_processes before and after, and fails if its instance or
#     its started_at changed, so no restart stood between the two writes;
#   - adds one lens kind with its constraint through `envoy-dispatch lens-kind-add` and reads it in
#     the catalogue, with the installed block schema version unmoved from boot, the rows of
#     schema_migrations the same before and after the add and the server's row of server_processes
#     the same, so the new kind is a row: no migration ran and no restart.
#
# It starts the server with e2e/run-server.sh (lib-processes.sh's boot_server), one server process
# on a port the kernel chooses, which the server's own line names, and the two fakes
# run-server.sh needs - e2e/fake-envoy.ts and e2e/fake-github.ts - each on a port the OS chooses,
# read from the line each prints; no secrets broker, which nothing here reads. It stops every
# process it started when it exits, also when a check fails, so the run can drop its database
# after it.
#
# Inputs: DATABASE_URL (an empty database on this machine that the run created and may drop; the
# server migrates it and this driver reads and writes its rows), and optionally
# DISPATCH_E2E_SERVER_BIN (the envoy-dispatch binary to serve and to run lens-kind-add with,
# default $TMPDIR/bin/envoy-dispatch, this head's build) and DEV_SIGNIN_LOGIN (the person the dev
# sign-in route signs in to own the project, default alice). The server accepts the bearer
# `e2e-token` (run-server.sh). Each unmet input is exit 2.
#
# The first PASS line is `PASS: lens-schema.sh the catalogue holds its ten kinds`, read before any
# lens is written; the lines before it say what was started. Each passing check prints
# `PASS: <section> <what>`; the first failing one prints `FAIL: <section> <what>: <observed>`,
# naming the write and the answer it took, and the script exits 1. The last line is
# `PASS: lens-schema.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/lens_schema?sslmode=disable \
         [DISPATCH_E2E_SERVER_BIN=$TMPDIR/bin/envoy-dispatch] [DEV_SIGNIN_LOGIN=alice] lens-schema.sh
       (DATABASE_URL names an empty database on this machine, which the server migrates)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib-processes.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-processes.sh"
require_env DATABASE_URL
require_commands bun curl jq openssl psql python3

readonly driver=lens-schema.sh
# shellcheck disable=SC2153,SC2031 # the caller's DATABASE_URL, checked by name above, not a server's
readonly database_url="$DATABASE_URL"
require_local_database
require_server_bin
readonly login="${DEV_SIGNIN_LOGIN:-alice}"
readonly token=e2e-token
stamp="$(date +%s)-$$"
readonly stamp
readonly session="lens-schema-$stamp"
readonly project=SCHEMA
# The kind lens-kind-add adds: a slug of letters and digits in words joined by single hyphens.
readonly added_kind="schema-bump-$stamp"

# The rows deleted and written back: the `lens` supertag row, and each attribute row an
# attribute_of tuple ties to that supertag, which is how a reader of the schema reads them.
readonly lens_type_rows="(n.kind = 'supertag' and n.id = 'lens')
  or (n.kind = 'attribute' and exists (
    select 1 from graph_tuples t
    where t.from_kind = 'attribute' and t.from_id = n.id and t.kind = 'attribute_of'
      and t.to_kind = 'supertag' and t.to_id = 'lens' and t.asserted))"
# What a lens write leaves in the store: lens nodes, findings (block nodes whose id names a lens),
# and the tuples out of either.
readonly lens_census="select (select count(*) from graph_nodes where kind = 'lens') || '|'
  || (select count(*) from graph_nodes where kind = 'block' and id like 'lens/%') || '|'
  || (select count(*) from graph_tuples where from_kind = 'lens' or (from_kind = 'block' and from_id like 'lens/%'))"

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

cleanup() {
  local code=$?
  stop_processes
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# server_row SECTION WHAT: sets $server_row to the instance and started_at of the one row of
# server_processes, which is this server's.
server_row() {
  query "$1" "$2" "select count(*) || '|' || coalesce(min(instance::text), '') || '|' || coalesce(min(started_at::text), '')
    from server_processes"
  [[ "$answer" == 1\|* ]] || fail "$1" "$2" "server_processes holds '$answer', want one row, this server's"
  server_row="${answer#1|}"
}

# --- boot ---------------------------------------------------------------------------------------

query boot "read the run's database before the server starts" \
  "select count(*) from information_schema.tables where table_schema = 'public'"
readonly tables_before="$answer"
[[ "$tables_before" == 0 ]] ||
  fail boot "the run's database is empty before the server starts" "it holds $tables_before tables"

start_fakes
boot_server setup "the server" "$server_bin"

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue
boot_version="$(jq -r '.version' "$work/catalogue.json")"
readonly boot_version

# --- boot: one process, over a database it migrated ---------------------------------------------

pass boot "the run's database held no table before the server started"
want_value boot "the server ran its migrations at boot: the store holds the lens supertag" 1 \
  "select count(*) from graph_nodes where kind = 'supertag' and id = 'lens' and state <> 'gone'"
server_row boot "read the server's row of server_processes"
readonly row_before="$server_row"
pass boot "server_processes holds one row, this server's: $row_before"
query boot "read the block schema version the server installed" \
  "select schema_version from server_processes"
readonly boot_schema_version="$answer"
[[ "$boot_schema_version" == "$boot_version" ]] ||
  fail boot "the server's row names the block schema version it installed" \
    "the row reads $boot_schema_version and GET /api/v1/schema/blocks reads $boot_version"
pass boot "the server installed block schema version $boot_version, which its row names"
cp "$work/catalogue.json" "$work/body"
expect boot "the server reads lens among the store's types" true \
  '[.store_types[].name] | index("lens") != null'
# A type is a row: each of the five types this tree's nodes take is a supertag row of the store,
# and none of them is a block type, so adding one declares no block schema version.
want_value boot "the store holds lens, risk, constraint, interaction and lens_kind as supertag rows" 5 \
  "select count(*) from graph_nodes where kind = 'supertag' and state <> 'gone'
     and id in ('lens', 'risk', 'constraint', 'interaction', 'lens_kind')"
expect boot "the server lists the five among the store's types, and none among the block types it installed" \
  '{"block_types":[],"store_types":["constraint","interaction","lens","lens_kind","risk"]}' \
  '["constraint", "interaction", "lens", "lens_kind", "risk"] as $five
   | {store_types: [.store_types[].name | select(. as $n | $five | index($n))] | sort,
      block_types: [.types[].name | select(. as $n | $five | index($n))]}'

# --- setup: the person, the project and the spec ------------------------------------------------

sign_in setup "$login" ", the project's default assignee"
send POST /api/v1/projects "$(jq -cn --arg k "$project" --arg l "$login" --argjson a "$(actor)" \
  '{key: $k, name: "Lens schema check", default_assignee: $l, actor: $a}')"
want_status 201 setup "create the project $project"
send POST /api/v1/issues "$(jq -cn --arg p "$project" --argjson a "$(actor)" \
  '{project: $p, title: "Lens schema check", force: true, actor: $a, spec: "The spec a lens is written under."}')"
want_status 201 setup "create the issue"
key="$(jq -er '.key' "$work/body")" || fail setup "create the issue" "no key: $(body)"
readonly key spec="dispatch://$key/spec"
pass setup "created $key in $project, whose spec is $spec"
lens_write="$(jq -cn --arg on "$spec" --argjson a "$(actor)" \
  '{on: $on, kind: "trade-off", verdict: "One store.", findings: ["A finding written with the lens."], actor: $a}')"
readonly lens_write

# --- S1: a write of a type the store no longer holds --------------------------------------------

query S1 "read the lens supertag row and its attribute rows" \
  "select coalesce(json_agg(row_to_json(n) order by n.kind, n.id), '[]') from graph_nodes n where $lens_type_rows"
printf '%s' "$answer" >"$work/lens-type-rows.json"
jq -e '([.[] | select(.kind == "supertag" and .id == "lens")] | length) == 1
  and ([.[] | select(.kind == "attribute")] | length) >= 1' "$work/lens-type-rows.json" >/dev/null ||
  fail S1 "the store holds the lens supertag row and its attribute rows" "$(<"$work/lens-type-rows.json")"
pass S1 "the store holds the lens supertag row and its attribute rows: $(jq -c '[.[] | "\(.kind)/\(.id)"]' "$work/lens-type-rows.json")"
query S1 "count what the store holds of lenses" "$lens_census"
readonly census_before="$answer"

psql_run S1 "delete the lens supertag row and its attribute rows in one transaction" \
  -c "begin;
      create temporary table deleted_lens_type on commit drop as select n.kind, n.id from graph_nodes n where $lens_type_rows;
      delete from graph_nodes n using deleted_lens_type d where n.kind = d.kind and n.id = d.id;
      commit;"
with_vars S1 "read the deleted rows" \
  "select count(*) from graph_nodes n where (n.kind, n.id) in
     (select kind, id from json_populate_recordset(null::graph_nodes, :'rows'::json))" \
  "rows=$(<"$work/lens-type-rows.json")"
[[ "$answer" == 0 ]] ||
  fail S1 "the lens supertag row and its attribute rows are gone from the store" "$answer of them are still there"
pass S1 "the lens supertag row and its attribute rows are gone from the store"
send GET /api/v1/schema/blocks
want_status 200 S1 "read the store's types after the delete"
expect S1 "the store's types, which listed lens, list it no more" false \
  '[.store_types[].name] | index("lens") != null'

send POST "/api/v1/issues/$key/lenses" "$lens_write"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
send GET "/api/v1/lenses?on=$(q "$spec")"
want_status 200 S1 "read the spec's lenses after the write"
stored="$(jq -c '[.lenses[].address]' "$work/body")"
[[ "$stored" == "[]" ]] ||
  fail S1 "the write of a lens, whose type the store no longer holds, is refused" \
    "the write POST /api/v1/issues/$key/lenses $lens_write was taken: HTTP $refused_status $(<"$work/refusal.json"); the spec holds the lens $stored"
pass S1 "the spec holds no lens after the write"
query S1 "count what the store holds of lenses after the write" "$lens_census"
[[ "$answer" == "$census_before" ]] ||
  fail S1 "the store holds nothing of the refused write: no lens, no finding, no tuple" \
    "lenses|findings|tuples read $census_before before the write and $answer after it; the write answered HTTP $refused_status $(<"$work/refusal.json")"
pass S1 "the store holds nothing of the refused write: no lens, no finding, no tuple ($answer)"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 409 S1 "the write is refused with HTTP 409"
pass S1 "the write is refused with HTTP $status"
code="$(jq -r '.code // empty' "$work/body")"
[[ -n "$code" ]] || fail S1 "the refusal carries a code" "the write $lens_write answered HTTP $status: $(body)"
pass S1 "the refusal carries a code, $code"
expect S1 "the refusal is SUPERTAG_NOT_HELD" '"SUPERTAG_NOT_HELD"' '.code'

# --- S2: the rows written back, and the same write taken by the same process --------------------

psql_run S2 "write the deleted rows back" -v rows="$(<"$work/lens-type-rows.json")" <<'SQL'
insert into graph_nodes select * from json_populate_recordset(null::graph_nodes, :'rows'::json);
SQL
pass S2 "wrote the lens supertag row and its attribute rows back with psql"
query S2 "read the rows written back" \
  "select coalesce(json_agg(row_to_json(n) order by n.kind, n.id), '[]') from graph_nodes n where $lens_type_rows"
[[ "$(jq -cS '.' <<<"$answer")" == "$(jq -cS '.' "$work/lens-type-rows.json")" ]] ||
  fail S2 "the rows are back as they were" "before: $(<"$work/lens-type-rows.json"); now: $answer"
pass S2 "the rows are back as they were, column for column"
send GET /api/v1/schema/blocks
want_status 200 S2 "read the store's types after the rows came back"
expect S2 "the store's types list lens again" true '[.store_types[].name] | index("lens") != null'

send POST "/api/v1/issues/$key/lenses" "$lens_write"
[[ "$status" == 201 ]] ||
  fail S2 "the same lens write is taken by the same server" "the write $lens_write answered HTTP $status: $(body)"
lens="$(jq -er '.address' "$work/body")" || fail S2 "the same lens write is taken" "no address: $(body)"
readonly lens
pass S2 "the same lens write is taken by the same server ($lens)"
send GET "/api/v1/lenses?on=$(q "$spec")"
want_status 200 S2 "read the spec's lenses"
expect S2 "the spec's lenses hold the lens, of its kind, with its finding" \
  "$(jq -cnS --arg l "$lens" '[{address: $l, findings: ["A finding written with the lens."], kind: "trade-off", verdict: "One store."}]')" \
  '[.lenses[] | {address, kind: .kind.id, verdict, findings: [.findings[].text]}]'

server_row S2 "read the server's row of server_processes again"
[[ "$server_row" == "$row_before" ]] ||
  fail S2 "the server's instance and started_at are the ones it had" "before: $row_before; after: $server_row"
kill -0 "$server_pid" 2>/dev/null || fail S2 "the server this driver started still serves" "pid $server_pid is gone"
pass S2 "one process took both writes: its instance and started_at are the ones it had ($server_row)"

# --- S3: a kind added at runtime: no migration, no restart, the block schema version unmoved -----

readonly kind_source="Edward Tufte, The Visual Display of Quantitative Information, Graphics Press, 1983"
readonly kind_reference='Cheshire, CT: Graphics Press, 1983; ch. 4, “Data-Ink and Graphical Redesign”, p. 93'
# The rows of schema_migrations, each version with the time it was applied, in version order: a
# migration, run by whatever runs it, adds a row, and the server ran its own at boot.
readonly migrations_sql="select coalesce(json_agg(json_build_object('version', version, 'applied_at', applied_at)
  order by version), '[]') from schema_migrations"
query S3 "read the rows of schema_migrations before the add" "$migrations_sql"
printf '%s' "$answer" >"$work/migrations-before.json"
jq -e 'length > 0' "$work/migrations-before.json" >/dev/null ||
  fail S3 "schema_migrations holds the migrations the server ran at boot" "it holds $(<"$work/migrations-before.json")"
DATABASE_URL="$database_url" "$server_bin" lens-kind-add --id "$added_kind" --words "the data-ink check" \
  --source "$kind_source" --reference "$kind_reference" >"$work/kind.out" 2>&1 ||
  fail S3 "envoy-dispatch lens-kind-add --id $added_kind adds the kind" "it exited $?: $(<"$work/kind.out")"
grep -q "^lens-kind-add: added dispatch://lens_kind/$added_kind, held to its source dispatch://constraint/" "$work/kind.out" ||
  fail S3 "envoy-dispatch lens-kind-add --id $added_kind adds the kind" "it printed: $(<"$work/kind.out")"
pass S3 "envoy-dispatch lens-kind-add added $added_kind: $(<"$work/kind.out")"
query S3 "read the rows of schema_migrations after the add" "$migrations_sql"
printf '%s' "$answer" >"$work/migrations-after.json"
changed="$(jq -cn --slurpfile b "$work/migrations-before.json" --slurpfile a "$work/migrations-after.json" \
  '{added: ($a[0] - $b[0]), removed: ($b[0] - $a[0])}')" ||
  fail S3 "compare the rows of schema_migrations" "before: $(<"$work/migrations-before.json"); after: $answer"
[[ "$changed" == '{"added":[],"removed":[]}' ]] ||
  fail S3 "the rows of schema_migrations are the same before and after the add: no migration ran" \
    "lens-kind-add --id $added_kind changed them: $changed"
held="$(jq -r '"\(length) rows, to version \(.[-1].version)"' "$work/migrations-after.json")"
pass S3 "the rows of schema_migrations are the same before and after the add, $held: no migration ran"

send GET /api/v1/schema/blocks
want_status 200 S3 "read the catalogue after the add"
expect S3 "the catalogue holds the kind added, with its words and its one source" \
  "$(jq -cnS --arg id "$added_kind" --arg s "$kind_source" --arg r "$kind_reference" \
    '[{id: $id, ref: "dispatch://lens_kind/\($id)", words: "the data-ink check", sources: [{standard: $s, reference: $r}]}]')" \
  --arg id "$added_kind" '[.lens_kinds[] | select(.id == $id) | {id, ref, words, sources: [.sources[] | {standard, reference}]}]'
expect S3 "the installed block schema version is the one the server installed at boot, $boot_version" \
  "$boot_version" '.version'
want_value S3 "the server's row still names block schema version $boot_schema_version" "$boot_schema_version" \
  "select schema_version from server_processes"
server_row S3 "read the server's row of server_processes after the add"
[[ "$server_row" == "$row_before" ]] ||
  fail S3 "the server's instance and started_at are the ones it had" "before: $row_before; after: $server_row"
pass S3 "the same process answered the kind added: its instance and started_at are the ones it had"

pass "$driver" complete
