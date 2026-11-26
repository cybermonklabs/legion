#!/usr/bin/env bash
# Drives the one truth of a run of the command bar and of this child's reasoning nodes across an
# upgrade and a rollback (check c-census, spec requirements r-one-truth and r-interaction). Two
# products of one transformation agree after both
# lose the same row, so the driver starts before the migration and holds both products to an
# inventory of its own:
#
#   B1  The base build, booted through lib-baseline.sh, which builds it from BASELINE_SHA and
#       stamps it with that commit, over the run's empty database, which it migrates; the driver
#       keeps the versions schema_migrations holds then as a file. BASELINE_SHA is the release this
#       child lands after: c-census's run sets it, unless its caller did, to the merge base with
#       the branch of cybermonklabs/legion#4173, and once that pull request merges the implementer
#       sets it to the first parent of its merge commit on main. It has no default, as
#       lib-baseline.sh's has none: unset, the run is refused, exit 2. Before anything is built,
#       the driver refuses a base whose tree lacks the migration 20261109013400_graph_store, which
#       creates the store, naming the base: over a
#       build that holds no store no two-build sentence of c-census can pass. Its /healthz names
#       that commit as its own and as its fleet_commit, the build processes.Live reads every live
#       process as serving. Through its own API three people each ask the command bar, and the
#       driver reads each run's stream to its terminal frame; the server stops, so nothing appends
#       a frame after the inventory is read.
#       With psql it writes one run row a day ahead that holds no frame, and one run for each row
#       of the identity table below, each a day ahead too and each form straight into
#       going_requests.author, since the older server's sign-in stores a login in its canonical
#       form only; so every run holds a terminal frame or an ends_at a day ahead, and neither
#       build's sweep ends one by itself. It then reads the inventory - each run's id, author,
#       words and frames - out of that database and keeps it as a file.
#   H1  This head's server (DISPATCH_E2E_SERVER_BIN, stamped with its own commit) boots over the
#       same database and runs every migration it holds that the database does not record, so the
#       migrations it applied are the versions schema_migrations holds now and did not after B1;
#       its /healthz names its own commit as the fleet's. The driver runs `envoy-dispatch
#       backfill-graph-store`, which gives each run the base build recorded its interaction node,
#       waits ten minutes at most until no document_block_versions row has a null version, and
#       compares the store with the inventory row by row, then going_requests and going_events the
#       same way, naming every run a side lacks.
#   H2  Through this build's routes: a pre-mortem reserved for alice with one risk prevented by a
#       check, a trade-off lens with one finding block, triggered by the pre-mortem, a constraint
#       reserved for bob, the requirement's constrained_by tuple to it, one lens_kind through
#       `envoy-dispatch lens-kind-add` (that kind's only writer), an edit of the words of an
#       existing requirement block under the header of one of alice's runs, whose ends_at psql first
#       sets a day ahead on both sides in one transaction, so the run's window decides no result,
#       and one lens on the shared bearer e2e-token whose body names a session no other row names.
#       It keeps an inventory of those writes. Then alice routes the issue to a role and comments
#       on it, one message addressed to that role, whose node and page this head's events outbox
#       writes, publishing through the run's NATS. Last, so that no kind of the store is checked
#       over no row, a second issue's spec holds an ask block that alice answers and a session
#       follows, and an agent's decision with a shaped pointer at a requirement, a from_ask to that
#       ask and a reading under it, which give the eight kinds cybermonklabs/legion#4173 adds
#       (shaped, from_ask, decided_by, followed_by, reading_parent, ask_parent, ask_anchor and
#       ask_answer) a row each, and an ask the agent opens on a document of the project that belongs
#       to no issue, the one ask whose node states an owned_by, so that with the ties above each of
#       the six kinds the census holds of graph_edges' answer of those ties (shaped_edge,
#       from_ask_edge, decided_by_edge, followed_by_edge, ask_parent_edge and ask_anchor_edge) holds
#       a row too. Before any further backfill the census, run whole, exits 0 and reads the person,
#       the session, the role and the page kinds and each of those fourteen at zero differences
#       over more than zero rows: the shared bearer proves no author, so that lens is
#       the project's and settles nothing, yet it names the session its body names as its writer
#       through authored_by and its write stated that session's node (spec v46 R12), so the session
#       is on both sides; the driver reads that authored_by tuple.
#   B2  The base build again, as in a rollback, through run-server.sh, booted beside this head's
#       server, which still serves: two builds serve the one database at once, each on a port of
#       its own, and both sign cookies with the run's one dev sign-in key. While both serve, alice
#       proposes the requirement r3 through this head's state route, the base build's own edit
#       route writes a version of the spec that changes the words of r3's second paragraph, a block
#       of its own whose text r3's holds, so that build's index pass writes r3's node again, and
#       this head reads r3 still proposed (cybermonklabs/legion#3178's keep-state rule). dave, a login
#       no other row names, writes through this head a lens reserved for him, his only node, and
#       reads his own page through this head with that lens on it; bob reads dave's page through
#       the base build and is answered, status and body byte for byte, what that build answers him
#       for a login nobody holds (cybermonklabs/legion#3178's person-page rule). Then this head's
#       server stops and the base build serves alone: one new run, whose frames it writes with no
#       frame value; with psql the hand-written run's ends_at moves into the past, standing for time
#       passing, on both sides in one transaction (its row, and its node's attribute and value
#       tuple), and a third run is dated two hours back, and the driver waits ten minutes at most
#       until that build's sweep has ended the first and deleted the row of the second.
#   H3  This head again. Each boot wrote one announcement once processes.Live read every live
#       process as its build - the base build of B2, booted beside this head, once this head
#       stopped - so they name the base build, this head, the base build and this head, in order:
#       two builds. Before the backfill, a census beside a planted instance, which exits 1 on its
#       differences: it names the new run, a row with no node, and the terminal frame of the run row
#       written by hand, a row with no frame value. Then the backfill again: a node for the new run,
#       a frame value for each frame that build appended, and the run whose row it deleted at state
#       gone. alice moves her pre-mortem to proposed, the finding to settled and her risk to
#       overtaken. Only then the census, the store-to-projection comparison, which is reported
#       apart from the inventory's: a run the upgrade lost shows in the first and never in the
#       second. Run whole, it exits 0, every kind of the store checked and none read apart, and it
#       reads zero differences for the interaction and frame kinds, for both block projections of
#       cybermonklabs/legion#3178, for comment, person, session, role and page, for the eight kinds
#       of cybermonklabs/legion#4173 and the six of graph_edges' answer of them, and for the
#       visibility and state of every node of this child's
#       kinds, each kind having checked more than zero rows, the moved nodes counted as live ones
#       are, and every interaction at `author`. Then,
#       with psql, one interaction node no going_requests row projects, a copy of bob's run's node
#       under an id no row names, and one frame value no going_events row projects, of the run row
#       written by hand at the sequence after its one frame's: the census exits 1 and reads each as
#       one difference of its kind naming its run, two in all, and once both are deleted, exactly
#       as planted, it exits 0 and reads zero again.
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
#
# The identity table is written into this driver by a person, and no code the server runs produces
# it: each row is a form a record can hold of a person's identity, the node its run's authored_by
# must name, and the person it resolves to. carol is the person of the table who asks the command
# bar and writes no comment; karol is one character from her and must stay a person of his own.
#
# Inputs: DATABASE_URL (an empty database on this machine that the run created and drops; every
# server here migrates and serves it, and this driver reads and writes its rows), BASELINE_SHA
# (the base commit, read in LEGION_WORKSPACE's checkout or else this one and handed to
# lib-baseline.sh in full; it has no default; a base whose tree
# lacks the migration 20261109013400_graph_store is refused, exit 2, naming it), and optionally
# DISPATCH_E2E_SERVER_BIN (this head's envoy-dispatch, which serves and runs backfill-graph-store,
# census and lens-kind-add, default $TMPDIR/bin/envoy-dispatch) and LEGION_WORKSPACE
# (lib-baseline.sh's checkout). It starts the two fakes run-server.sh needs, the fake Envoy and the
# fake GitHub, a NATS server with JetStream, packages/envoy's natsd (internal/testnats/cmd/natsd),
# which it builds and which this head's boots alone are handed (run-server.sh's
# DISPATCH_E2E_NATS_URLS, boot_head), so this head's events outbox publishes, and every server
# (lib-processes.sh), and stops each when it exits, also when a check fails, so the run can drop
# its database. Each unmet input is exit 2. No BASELINE_BINARY is read: the base is always built
# here and stamped with its commit.
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
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/census?sslmode=disable \
         BASELINE_SHA=<commit> [DISPATCH_E2E_SERVER_BIN=$TMPDIR/bin/envoy-dispatch] \
         node-visibility-census.sh
       (DATABASE_URL names an empty database on this machine, which the run created and drops;
       BASELINE_SHA is the commit of the release this branch lands after and has no default, and
       a base whose tree lacks the migration 20261109013400_graph_store is refused)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib-processes.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-processes.sh"
require_env DATABASE_URL BASELINE_SHA
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
# The base, read before anything is built. BASELINE_SHA names it, the release this child lands
# after; c-census's run sets it, when its caller has not, to the merge base with the branch of
# cybermonklabs/legion#4173. It has no default, as lib-baseline.sh's has none: only the caller
# knows that release, and require_env above refuses a run that names none. It is read in the
# checkout lib-baseline.sh builds it from, LEGION_WORKSPACE or else this tree, and handed to
# lib-baseline.sh in full, so the commit built is the commit read here. A base whose tree lacks
# the migration that creates the store is refused, named: over a build that holds no store, no
# sentence of c-census that reads two builds can pass.
readonly base_repo="${LEGION_WORKSPACE:-$tree}"
readonly store_migration=packages/envoy/internal/dispatch/store/migrations/20261109013400_graph_store.up.sql
readonly base_named="$BASELINE_SHA"
BASELINE_SHA="$(git -C "$base_repo" rev-parse --verify --quiet "${base_named}^{commit}")" || {
  printf "BASELINE_SHA '%s' names no commit of %s\n" "$base_named" "$base_repo" >&2
  usage
  exit 2
}
git -C "$base_repo" cat-file -e "$BASELINE_SHA:$store_migration" 2>/dev/null || {
  printf '%s: the base %s (BASELINE_SHA %s) lacks the migration 20261109013400 (graph_store): its tree holds no %s, so it is a build with no store, over which no two-build sentence of c-census can pass\n' \
    "$driver" "$BASELINE_SHA" "$base_named" "$store_migration" >&2
  usage
  exit 2
}
printf '%s: the base %s (BASELINE_SHA %s) holds the migration 20261109013400 (graph_store): %s\n' \
  "$driver" "$BASELINE_SHA" "$base_named" "$store_migration"
# The run's NATS reaches this head's boots alone (boot_head); one a caller's shell exported reaches
# no server here, the base build's boots included.
unset DISPATCH_E2E_NATS_URLS
stamp="$(date +%s)-$$"
readonly stamp
# The session every bearer write names, the session the identity table says the server proved
# carol's, the session the lens on the shared bearer names, which no other row names, and the
# session that follows the ask alice answers (H2), which no other row names either.
readonly session="census-$stamp"
readonly carol_session="census-carol-$stamp"
readonly bearer_session="census-bearer-lens-$stamp"
readonly follower_session="census-follower-$stamp"
readonly project=CENSUS
# The two seeded people, the person of the identity table, karol, a login one character from
# hers, who must stay a person of his own, dave, a login no other row names, whose only node is a
# lens reserved for him (B2), and a login nobody holds, which no row names at all.
readonly alice=alice bob=bob carol=carol twin=karol third=dave
readonly nobody="nobody-$stamp"
readonly added_kind="census-$stamp"
# The role the one message addressed to a role is addressed to (H2): a name roleRoutePattern
# admits (lower case, digits and hyphens) that no other row names.
readonly role="census-role-$stamp"
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
# shellcheck source-path=SCRIPTDIR source=node-visibility-census-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/node-visibility-census-lib.sh"

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
start_nats

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
# Each run is written a day ahead, as the run row above is. Each build's sweep, once a minute while
# it serves, ends every run past its ends_at that holds no terminal frame: a run written by hand
# that is overdue would gain a frame the inventory lacks. A day ahead, none of these runs is overdue
# at any sweep of this run, whichever build sweeps, until this driver moves its ends_at.
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

boot_head H1 "this head's server"
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
[[ -n "$head_migrations" ]] ||
  fail H1 "this head ran its migrations over the base build's schema" \
    "the versions schema_migrations holds after this head's boot and did not hold after B1 are: '$head_migrations'"
pass H1 "this head ran $(wc -w <<<"$head_migrations") migration(s) the base build had not: $head_migrations"

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

# The one message addressed to a role (Verification v55): the census refuses a kind it checked no
# row of (cybermonklabs/legion#3178), and no row of this run addressed a role, so without it the census
# refuses the role and the page kinds. alice routes $key to the role $role through this head and
# writes a comment on it, as cybermonklabs/legion#3178's census test addresses its role
# (addressTheRole, packages/envoy/cmd/dispatch/store_census_test.go): the route is the role's row,
# which issues.route holds now and the issue.updated event names (graphcensus's addressedRoles),
# and the comment is a message that route addresses to the role, which the events outbox's
# publisher resolves as a destination. The publisher is the one writer of a role's node and its
# page beside the row that addresses the role (outbox.deliveredGraph), and this head publishes
# through the run's NATS (boot_head), so the driver waits, 60 s at most, until the store holds both
# nodes, and reads that the publisher wrote them: no backfill runs before H3.
request "cookie:$alice" PATCH "/api/v1/issues/$key" "$(jq -cn --arg r "role:$role" '{route: $r}')"
want_status 200 H2 "$alice routes $key to the role $role through this head"
request "cookie:$alice" POST "/api/v1/issues/$key/comments" \
  "$(jq -cn --arg b "A comment on $key, which is routed to the role $role." '{body: $b}')"
want_status 201 H2 "$alice writes a comment on $key, a message its route addresses to the role $role"
readonly role_sql="select (select count(*) from issues where key = '$key' and route = 'role:$role') || '|'
  || (select count(*) from graph_nodes where kind = 'role' and id = '$role') || '|'
  || (select count(*) from graph_nodes where kind = 'page' and id = 'role/$role')"
query H2 "read the role $role's route, node and page" "$role_sql"
waited=0
until [[ "$answer" == "1|1|1" ]]; do
  [[ "$waited" -lt 60 ]] ||
    fail_with_log H2 "this head's events outbox writes the node of the role $role and its page, role/$role, within 60 s" \
      "issues routed to it|role nodes|page nodes read $answer" "$work/this head's server.log"
  sleep 1
  waited=$((waited + 1))
  query H2 "read the role $role's route, node and page" "$role_sql"
done
want_value H2 "the node of the role $role and its page, role/$role, are made by the events outbox's publisher" \
  "system:dispatch-outbox system:dispatch-outbox" \
  "select string_agg((made_by->>'kind') || ':' || (made_by->>'id'), ' ' order by kind desc)
   from graph_nodes where (kind = 'role' and id = '$role') or (kind = 'page' and id = 'role/$role')"
pass H2 "$key is routed to the role $role, and this head's events outbox wrote the role's node and its page, role/$role (waited ${waited} s)"

# The eight kinds of cybermonklabs/legion#4173 (decision_kinds): no write above gives any of them a
# row, and the census refuses none of them for holding none, so each would be read over no row.
# Through this head: the agent bearer creates a second issue of the project and writes an ask block
# into its spec, whose row settlement opens and places in that spec (ask_parent, ask_anchor); alice
# answers it (ask_answer), and the session $follower_session follows it (followed_by). The bearer
# then writes into the same spec a decision with a shaped pointer at its requirement r1 (shaped)
# and a from_ask to that ask (from_ask), whose decided_by the server takes from the write that
# introduced the block (decided_by), and a reading under it (reading_parent). They stand in a second
# issue's spec, which no later write of this driver edits, so the base build's version of the first
# issue's spec (B2) moves none of them. The driver waits until no document owes a settlement or its
# block index, so the census reads them.
request agent POST /api/v1/issues "$(jq -cn --arg p "$project" --argjson a "$(actor)" '{project: $p,
  title: "Census check: an ask and a decision", force: true, actor: $a,
  spec: ":::requirement{#r1}\nThe decision of this spec shapes this requirement.\n:::\n\n:::requirement{#r2}\nThe ask of this spec stands below this requirement.\n:::\n"}')"
want_status 201 H2 "the agent bearer creates a second issue of $project through this head"
decision_key="$(jq -er '.key' "$work/body")" || fail H2 "create a second issue of $project" "no key: $(body)"
readonly decision_key
request agent POST "/api/v1/issues/$decision_key/artifacts/spec/edits" "$(jq -cn --argjson a "$(actor)" '{ops: [{op: "insert", after: "end",
  markdown: ":::ask{#a1 urgency=\"med\"}\nWhich transport does every service speak?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::"}], actor: $a}')"
[[ "$status" == 200 || "$status" == 201 ]] ||
  fail H2 "the agent bearer writes the ask block a1 into the spec of $decision_key" "HTTP $status: $(body)"
wait_ask H2 "$decision_key" a1
readonly decision_ask="$ask_id"
pass H2 "the agent bearer wrote the ask block a1 into the spec of $decision_key, whose row settlement opened: ask $decision_ask"
request "cookie:$alice" POST "/api/v1/asks/$decision_ask/answer" \
  '{"text":"Plain HTTP, which every service already speaks.","selected":["REST"],"expected_edited_at":null}'
want_status 200 H2 "$alice answers the ask $decision_ask in her own words"
request agent PUT "/api/v1/asks/$decision_ask/followers/$(q "$follower_session")" \
  "$(jq -cn --arg s "$follower_session" '{actor: {kind: "session", id: $s}}')"
[[ "$status" =~ ^20[04]$ ]] || fail H2 "the session $follower_session follows the ask $decision_ask" "HTTP $status: $(body)"
pass H2 "$alice answered the ask $decision_ask, and the session $follower_session follows it"
wait_settled H2 "the answer of the ask $decision_ask"
request agent POST "/api/v1/issues/$decision_key/artifacts/spec/edits" "$(jq -cn --arg s "dispatch://$decision_key/spec#b-r1" \
  --arg f "dispatch://$decision_key/ask/$decision_ask" --argjson a "$(actor)" '{ops: [{op: "insert", after: "end",
  markdown: ("::::decision{#d1 shaped=\"" + $s + "\" from_ask=\"" + $f + "\"}\nPlain HTTP for every service this quarter.\n\n:::reading{#g1}\nThe agent reads the answer so.\n:::\n::::")}], actor: $a}')"
[[ "$status" == 200 || "$status" == 201 ]] ||
  fail H2 "the agent bearer writes the decision d1 into the spec of $decision_key" "HTTP $status: $(body)"
wait_settled H2 "the decision d1 of $decision_key"
pass H2 "the agent bearer wrote the decision d1 into the spec of $decision_key, shaping r1, settling the ask $decision_ask and holding the reading g1"
# The ask a1 stands in an issue's spec, so its node states no owned_by, and graph_edges answers an
# ask's owned_by out of an ask a document owns alone: without one, ask_parent_edge would be read
# over no row. Through this head the agent bearer uploads a document of $project that belongs to
# no issue and opens an ask on it (createAskFor), whose write states, in the transaction of its
# asks row, the ask's node and its owned_by to that document (graphstore.AskFacts); the driver
# reads the row, that tuple and the edge graph_edges answers out of it.
request agent POST "/api/v1/projects/$project/artifacts" "$(jq -cn --argjson a "$(actor)" \
  '{name: "census-notes.md", content: "# Census notes\n\nAn ask on this document belongs to no issue.\n", actor: $a}')"
want_status 201 H2 "the agent bearer uploads a document of $project through this head"
owner_document="$(jq -er '.artifact | select(.issue_key == null) | .id' "$work/body")" ||
  fail H2 "the agent bearer uploads a document of $project that belongs to no issue" "$(body)"
owner_ref="$(jq -er '.artifact.ref_key' "$work/body")" || fail H2 "upload a document of $project" "no ref_key: $(body)"
readonly owner_document owner_ref
request agent POST "/api/v1/artifacts/$owner_document/asks" "$(jq -cn --argjson a "$(actor)" \
  '{question: "Which document owns this ask?", actor: $a}')"
want_status 201 H2 "the agent bearer opens an ask on the document $owner_ref through this head"
owned_ask="$(jq -er '.id' "$work/body")" || fail H2 "open an ask on the document $owner_ref" "no id: $(body)"
readonly owned_ask
want_value H2 "the ask $owned_ask belongs to no issue, its row names its node and the document $owner_ref, its node states owned_by $owner_ref, and graph_edges answers that edge" \
  "1|1|1" \
  "select (select count(*) from asks where id = '$owned_ask' and issue_key is null and artifact_id = '$owner_document'
             and node_kind = 'ask' and node_address = '$owned_ask') || '|'
          || (select count(*) from graph_tuples where from_kind = 'ask' and from_id = '$owned_ask' and kind = 'owned_by'
             and to_kind = 'artifact' and to_id = '$owner_ref' and asserted) || '|'
          || (select count(*) from graph_edges where from_kind = 'ask' and from_id = '$owned_ask' and kind = 'owned_by'
             and to_kind = 'artifact' and to_id = '$owner_ref')"
wait_settled H2 "the document $owner_ref and its ask $owned_ask"
pass H2 "the agent bearer opened the ask $owned_ask on $owner_ref, a document of $project that belongs to no issue, which owns it"

# Straight after those writes, before any further backfill: the census, run whole, exits 0.
take_census H2 h2 0
census_zero H2 h2 person
census_zero H2 h2 session
census_zero H2 h2 role
census_zero H2 h2 page
for kind in "${populated_kinds[@]}"; do
  census_zero H2 h2 "$kind"
done
census_differences H2 h2
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
# writes r3's node again: the write cybermonklabs/legion#3178's keep-state rule (its R8) keeps r3's
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
# (cybermonklabs/legion#3178's person-page rule, its R15).
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

# shellcheck source-path=SCRIPTDIR source=node-visibility-census-stages.sh
. "$(dirname "${BASH_SOURCE[0]}")/node-visibility-census-stages.sh"
