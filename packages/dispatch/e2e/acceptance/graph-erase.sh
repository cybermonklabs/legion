#!/usr/bin/env bash
# Drives the one statement in Dispatch that removes bytes (dispatch://INFRA-1149's c-erasure).
# It writes a document, has a person approve a version of it, and writes a comment holding a
# secret that version's markdown holds too. It then runs `envoy-dispatch erase` against that
# comment's node and proves, in order: the command refuses until it has listed what it would
# remove, and that list holds the version and the review the erase would end and the document
# whose live state it would replace, with what that costs; after the erase a read of the node's
# address answers what a deleted node answers, byte for byte, no tuple and no projection row of it
# remains, the `artifact_versions` row holding the erased text is gone and the `artifact_reviews`
# row whose digest covered those bytes is gone, reported by its issue and its version; the
# document still loads, the marker where the secret was; no column of any table of the database
# holds the secret, by a search that names no store and finds a control row planted beside it,
# and the `events` rows that held it stand with their payload redacted, the marker naming the
# erase in each and the row's id, time and actor what they were before; the erase is recorded in
# one `graph_erasures` row, which holds what was removed - this run's own version and review by
# name - who ran it and when, and holds no erased text; every node left carries
# a supertag of the closed list the store itself holds, so the erase wrote no node of its own;
# `envoy-dispatch census --store` reads zero differences over rows it read; and the command's path
# over HTTP answers 404 to the deployment's agent bearer and to a person's cookie alike, because
# no route serves it.
#
# Every read that wants zero after the erase - the node's tuples, the version, the four stores -
# runs before it as well and finds the rows that held the secret, and the node's address answers
# 200 before it, so each answer after the erase is held against something that was there.
#
# The embedding store is read the same way, and its order is forced. The block-index pass cuts
# the windows of the comment and of the document into `embeddings` on a tick of its own, so the
# driver waits until both texts' windows hold the secret, then holds that pass's lease across the
# erase: every run erases a node whose windows are written, and no tick writes one mid-erase.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DATABASE_URL (the database that
# server serves, which `envoy-dispatch erase` and `envoy-dispatch census` read from their own
# environment: the command takes a database credential and no route), `envoy-dispatch` on PATH,
# and optionally DEV_SIGNIN_LOGIN (default alice), a person the server's dev sign-in route signs
# in, who approves the version: an approval needs the person and approve scopes, which an agent
# bearer does not hold. A missing DATABASE_URL and a missing command are each exit 2, as every
# unmet input of this driver is.
#
# It reads this pod's own test database and never a deployment's: it refuses a DATABASE_URL whose
# host is not loopback, and the erase it runs removes only the comment this run wrote.
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
         DISPATCH_TOKEN=e2e-token PROJECT=CORE [DEV_SIGNIN_LOGIN=alice] graph-erase.sh
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
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# One statement, so the directive covers both: a directive comment reaches the command after it
# alone, and before this it left the second of them reported.
# shellcheck disable=SC2153 # PROJECT and DATABASE_URL are the caller's inputs, checked by name above
readonly project="$PROJECT" database_url="$DATABASE_URL"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
# The block-index pass's lease, which writes the embedding windows this driver holds still across
# the erase, read from the Go sources that declare it: the lease's name and the first key every
# loop lease is taken with (duty.Exclusive).
readonly loop_source="$script_dir/../../../envoy/internal/dispatch/docs/block_index.go"
readonly lease_source="$script_dir/../../../envoy/internal/dispatch/duty/exclusive.go"
# How long the driver waits for that pass to write the windows - it runs once a tick, 30 s apart
# on this build - and how long the session holding its lease sleeps, which is a hang guard alone:
# release_block_index interrupts it as soon as the erase is done.
readonly window_wait=120
readonly lease_seconds=900
readonly lease_polls=1500
readonly login="${DEV_SIGNIN_LOGIN:-alice}"
session="graph-erase-$(date +%s)-$$"
readonly session
# The bytes this run erases. Nothing but this run writes them, so a row that still holds them
# after the erase is a row the erase missed.
#
# They are one word of letters and digits, and not the `passphrase-<time>-<pid>-hunter2` this
# driver used before, because Postgres splits a hyphenated token into several lexemes: a
# tsvector holding that shape holds `passphras`, `-<time>`, `-<pid>` and `hunter2` and never the
# whole, so a search for the secret could not find it in a search vector, and the run-unique
# parts are numbers this run's session name holds as well. One word of letters and digits is one
# lexeme, spelled as the text spells it, so the search below finds these bytes in a tsvector, in
# a lexeme of the term statistics and in every text column alike.
secret="passphrase$(date +%s)x$$hunter2"
readonly secret

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
      printf 'graph-erase.sh: could not close %s: HTTP %s\n' "$key" "$status" >&2
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# request AS METHOD PATH [JSON]: sends METHOD PATH as AS: `agent`, with the bearer in $work/auth,
# or `person`, with the session cookie the dev sign-in left in $work/jar and the dashboard's
# origin, which the server requires of every write a cookie authenticates. The answer's body goes
# to $work/body and its HTTP status to $status.
request() {
  local -a auth data=()
  if [[ "$1" == person ]]; then
    auth=(-b "$work/jar" -H "Origin: $origin")
  else
    auth=(-H "@$work/auth")
  fi
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# send METHOD PATH [JSON]: request as the agent.
send() { request agent "$@"; }
# person_send METHOD PATH [JSON]: request as the person DEV_SIGNIN_LOGIN signed in.
person_send() { request person "$@"; }

# want_rows SECTION WHAT WANT SQL: the count SQL reads is WANT.
want_rows() {
  query "$1" "$2" "$4"
  [[ "$answer" == "$3" ]] || fail "$1" "$2" "read $answer, want $3 (from: $4)"
  pass "$1" "$2"
}

# want_held SECTION WHAT SQL: the count SQL reads is above zero, and the check prints it. Each of
# these is a read E5 later wants zero from, run before the erase, so that zero is a row the erase
# removed and not a search that matched nothing.
want_held() {
  query "$1" "$2" "$3"
  [[ "$answer" =~ ^[1-9][0-9]*$ ]] || fail "$1" "$2" "read $answer, want at least 1 (from: $3)"
  pass "$1" "$2: $answer row(s)"
}

# held_soon SECTION WHAT SQL: polls SQL, a count, until it reads above zero, for up to
# $window_wait seconds, and prints the count it read. It is for a row a pass of the server writes
# on its own tick, which no request of this driver can ask for.
held_soon() {
  local waited=0
  while true; do
    query "$1" "$2" "$3"
    [[ ! "$answer" =~ ^[1-9][0-9]*$ ]] || break
    [[ "$waited" -lt $((window_wait * 2)) ]] ||
      fail "$1" "$2" "read $answer after ${window_wait}s, want at least 1 (from: $3)"
    sleep 0.5
    waited=$((waited + 1))
  done
  pass "$1" "$2: $answer row(s)"
}

# declared SECTION WHAT FILE SED: the one value SED prints out of a Go source file, refused when
# the file or the declaration is gone, so a build that moved the lease moves this driver with it.
declared() {
  [[ -r "$3" ]] || fail "$1" "$2" "$3 is unreadable"
  answer="$(sed -n "$4" "$3")"
  [[ -n "$answer" ]] || fail "$1" "$2" "no such declaration in $3"
}

# hold_block_index SECTION: takes the block-index pass's own lease, waiting out a tick that holds
# it, so no tick writes an embedding window while this driver erases: duty.Exclusive takes this
# lease before every pass and skips the pass where another session holds it. The lease is a
# session lock, so a psql of this driver's holds it until release_block_index interrupts that
# session's sleep. It is graph-sizing.sh's hold_block_index.
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
  pass "$1" "this driver holds the $lease_name lease, so no tick writes an embedding window during the erase"
}

# release_block_index gives that lease back: SIGINT makes psql cancel its sleep and read the end of
# its own input, which closes the session and frees the lock there and then.
release_block_index() {
  [[ -n "$lease_pid" ]] || return 0
  kill -INT "$lease_pid" 2>/dev/null || true
  wait "$lease_pid" 2>/dev/null || true
  lease_pid=""
}

# erase SECTION ARGS...: runs `envoy-dispatch erase ARGS`, its output in $work/erase and its exit
# status in $erase_code. The command reads DATABASE_URL from its own environment and no bearer
# reaches it, so this driver's is kept out of the environment it runs in.
erase() {
  local section="$1"
  shift
  erase_code=0
  env -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE DATABASE_URL="$database_url" \
    envoy-dispatch erase "$@" >"$work/erase" 2>&1 || erase_code=$?
  [[ "$erase_code" -lt 2 ]] ||
    fail "$section" "run envoy-dispatch erase $*" "exited $erase_code: $(<"$work/erase")"
}

# held_anywhere SECTION NEEDLE: searches every column of every table of the database for NEEDLE,
# and sets $answer to `columns <n>`, the number of columns searched, then one line
# `<schema>.<table>.<column> <rows>` for each column holding it. A bytea is searched for NEEDLE's
# UTF-8 bytes (convert_to), since its text form is hex; every other column for NEEDLE in its text
# form, which is how a jsonb, a tsvector and an array spell what they hold. A column of a type
# that holds no text - a number, a time, a uuid - is not searched.
#
# The same session first writes NEEDLE into a table of its own, `erase_control`, temporary and
# so dropped with the session, and searches it with the rest: a search that reached no row would
# answer none for every column, and the caller reads the control's line to tell that apart.
held_anywhere() {
  answer="$(env -u PGHOSTADDR psql "$database_url" -Atq -v ON_ERROR_STOP=1 -v needle="$2" \
    -f - 2>"$work/psql" <<'SQL'
select set_config('erase_driver.needle', :'needle', false) \g /dev/null
create temp table erase_control (body text);
insert into erase_control values ('planted beside the search: ' || :'needle');
create temp table erase_held (place text, held bigint);
create temp table erase_searched (columns integer);
do $$
declare
  needle text := current_setting('erase_driver.needle');
  col record;
  test text;
  held bigint;
  searched integer := 0;
begin
  for col in
    select c.table_schema, c.table_name, c.column_name, c.data_type, c.udt_name
      from information_schema.columns c
      join information_schema.tables t
        on t.table_schema = c.table_schema and t.table_name = c.table_name
     where t.table_type in ('BASE TABLE', 'LOCAL TEMPORARY')
       and c.table_schema not in ('pg_catalog', 'information_schema')
       and (c.table_schema not like 'pg_temp%' or c.table_schema = pg_my_temp_schema()::regnamespace::text)
       and c.data_type not in ('smallint', 'integer', 'bigint', 'numeric', 'real', 'double precision',
         'boolean', 'uuid', 'date', 'interval', 'time with time zone', 'time without time zone',
         'timestamp with time zone', 'timestamp without time zone', 'inet', 'cidr', 'macaddr',
         'macaddr8', 'oid', 'xid', 'xid8', 'pg_lsn', 'money', 'bit', 'bit varying')
     order by 1, 2, 3
  loop
    test := case
      when col.data_type = 'bytea' then
        format('position(convert_to($1, %L) in %I) > 0', 'UTF8', col.column_name)
      when col.udt_name = '_bytea' then
        format('exists (select 1 from unnest(%I) b where position(convert_to($1, %L) in b) > 0)', col.column_name, 'UTF8')
      else format('position($1 in %I::text) > 0', col.column_name)
    end;
    execute format('select count(*) from %I.%I where %s', col.table_schema, col.table_name, test)
      into held using needle;
    searched := searched + 1;
    if held > 0 then
      insert into erase_held values (col.table_schema || '.' || col.table_name || '.' || col.column_name, held);
    end if;
  end loop;
  insert into erase_searched values (searched);
end $$;
select 'columns ' || columns from erase_searched;
select place || ' ' || held from erase_held order by place;
SQL
  )" || fail "$1" "search every column of every table" "psql: $(<"$work/psql")"
}

# E1: the inputs, and the database this run works on.
host="${database_url#*://}"
host="${host#*@}"
host="${host%%[:/]*}"
case "$host" in
  127.0.0.1 | ::1 | localhost) pass E1 "DATABASE_URL names this machine ($host)" ;;
  *) fail E1 "DATABASE_URL names this machine" "host $host is not loopback; this driver erases rows and reads this pod's own test database, never a deployment's" ;;
esac

# E2: the issue, the document whose version holds the secret, and the person's approval of it.
send POST "/api/v1/issues" "$(jq -cn --arg p "$project" --arg s "$session" \
  '{project: $p, title: "Store erasure check", spec: "The one statement that removes bytes.", actor: {kind: "session", id: $s}}')"
want_status 201 E2 "create the issue"
key="$(jq -er '.key' "$work/body")" || fail E2 "create the issue" "no key: $(body)"
pass E2 "created $key in $project"

# The comment's whole body is one paragraph of the document, so the erase finds it in the
# version's markdown (graphcensus.erasedVersions matches the stored body inside it).
readonly leak="The passphrase is $secret and nobody else may read it."
send POST "/api/v1/issues/$key/artifacts" "$(jq -cn --arg s "$session" --arg leak "$leak" \
  '{name: "leaked-notes.md", content: ("# Leaked notes\n\n" + $leak + "\n"), actor: {kind: "session", id: $s}}')"
want_status 201 E2 "write the document"
document="$(jq -er '.artifact.id' "$work/body")" || fail E2 "write the document" "no id: $(body)"
document_ref="$(jq -er '.artifact.ref_key' "$work/body")" ||
  fail E2 "write the document" "no ref_key: $(body)"
version="$(jq -er '.version.number' "$work/body")" ||
  fail E2 "write the document" "no version: $(body)"
pass E2 "wrote $document_ref version $version holding the secret"

curl -sS -o /dev/null -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$login")" 2>"$work/curl" ||
  fail E2 "sign $login in" "curl: $(<"$work/curl")"
[[ -s "$work/jar" ]] ||
  fail E2 "sign $login in" "the dev sign-in route left no cookie; this driver needs a person to approve a version"
person_send POST "/api/v1/artifacts/$document/reviews" '{"state":"approved"}'
want_status 201 E2 "approve $document_ref version $version"
review="$(jq -er '.review.id // .id' "$work/body")" || fail E2 "approve the version" "no review id: $(body)"
pass E2 "$login approved $document_ref version $version (review $review)"
want_rows E2 "the approval covers the version's bytes" 1 \
  "select count(*) from artifact_reviews where artifact_id = '$document' and version = $version and content_sha256 is not null"

send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg s "$session" --arg leak "$leak" \
  '{body: $leak, actor: {kind: "session", id: $s}}')"
want_status 201 E2 "write the comment holding the secret"
comment="$(jq -er '.id' "$work/body")" || fail E2 "write the comment" "no id: $(body)"
readonly node="comment:$comment"
pass E2 "wrote $node"
want_rows E2 "the comment is a node of the store" 1 \
  "select count(*) from graph_nodes where kind = 'comment' and id = '$comment'"

# A second comment of the same session, which the erase does not touch: the store holds a
# session node for the author of a comment (R19's row side), so a session whose only row the
# erase removes would be left as a node the census rightly reports as projected by nothing. The
# erase removes the bytes it was pointed at and no author's node, and this comment is what keeps
# that true for the census below.
send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg s "$session" \
  '{body: "A second note of this run, holding no secret.", actor: {kind: "session", id: $s}}')"
want_status 201 E2 "write a comment the erase does not touch"
pass E2 "the session that wrote both keeps a row the erase leaves"

# The reads E5 wants zero from, each stated once here and run on both sides of the erase: before
# it each must find the rows that held the secret (E3), so after it each zero is the erase's work.
readonly node_tuples_sql="select count(*) from graph_tuples where (from_kind = 'comment' and from_id = '$comment') or (to_kind = 'comment' and to_id = '$comment')"
readonly version_sql="select count(*) from artifact_versions where artifact_id = '$document' and number = $version"
readonly -a store_reads=(
  "comment row|select count(*) from comments where body like '%$secret%'"
  "document version|select count(*) from artifact_versions where markdown like '%$secret%'"
  "block text|select count(*) from document_block_texts where text like '%$secret%'"
  "block excerpt|select count(*) from document_blocks where excerpt like '%$secret%'"
)

# E3: the command refuses until it has listed what it would remove.
erase E3 --node "$node"
[[ "$erase_code" == 1 ]] ||
  fail E3 "refuse without --confirm" "exited $erase_code, want 1: $(<"$work/erase")"
grep -Fq "nothing was removed" "$work/erase" ||
  fail E3 "refuse without --confirm" "no refusal in: $(<"$work/erase")"
pass E3 "envoy-dispatch erase --node $node removed nothing and said why"
grep -Eq "^  versions holding the erased text: [1-9][0-9]*\$" "$work/erase" ||
  fail E3 "list the versions it would end" "$(<"$work/erase")"
grep -Fq "    $document_ref version $version of $key" "$work/erase" ||
  fail E3 "list the versions it would end" "no $document_ref version $version in: $(<"$work/erase")"
pass E3 "the list holds $document_ref version $version"
grep -Eq "^  reviews over those bytes: [1-9][0-9]*\$" "$work/erase" ||
  fail E3 "list the reviews it would end" "$(<"$work/erase")"
grep -Fq "    $document_ref version $version approved (review $review) of $key" "$work/erase" ||
  fail E3 "list the reviews it would end" "no review $review in: $(<"$work/erase")"
pass E3 "the list holds the approval over those bytes (review $review)"
# The document's live state, which the erase replaces: its live text with the marker where the
# secret was, seeded as its one stored update in place of its history. That costs what a rebuild
# costs, so the list says it, per document, before anything goes: the operator's last look.
grep -Eq "^  live documents whose history is replaced: [1-9][0-9]*\$" "$work/erase" ||
  fail E3 "list the documents whose live state it would replace" "$(<"$work/erase")"
live_line="$(grep -F "    $document_ref of $key: " "$work/erase" || true)"
printf '%s\n' "$live_line" | grep -Eq "^    [^ ]+ of [^ ]+: its live text is redacted, the marker standing where the erased text was; its [1-9][0-9]* stored update\(s\) are replaced by one; [0-9]+ anchor mark\(s\) and [0-9]+ margin record\(s\) go with them; an editor open on it reloads and loses what it has not stored\$" ||
  fail E3 "list what replacing $document_ref's live state costs" "no such line for $document_ref in: $(<"$work/erase")"
pass E3 "the list holds $document_ref's live state: its text redacted, its history replaced, and what that costs"
want_rows E3 "the refused run wrote nothing" 1 \
  "select count(*) from comments where id = '$comment'"
want_rows E3 "the refused run recorded no erasure" 0 \
  "select count(*) from graph_erasures where node_id = '$comment'"

# The event log as it stands before the erase: the rows whose payload holds the secret, and the
# id, the time and the actor of each. R5 redacts a payload in place rather than deleting its
# row, because the log is append-only, so these are the rows E5 reads again afterwards and these
# are the facts that must be the same then.
query E3 "count the event rows holding the secret" \
  "select count(*) from events where payload::text like '%$secret%'"
events_held="$answer"
readonly events_held
[[ "$events_held" -gt 0 ]] ||
  fail E3 "the event log holds the comment's own write" \
    "no events row holds the secret, so the redaction E5 reads would prove nothing"
query E3 "read the id, time and actor of each event row holding the secret" \
  "select coalesce(jsonb_agg(jsonb_build_object('id', id, 'created_at', created_at, 'actor', actor) order by id), '[]'::jsonb)::text
     from events where payload::text like '%$secret%'"
events_before="$answer"
readonly events_before
pass E3 "the event log holds $events_held row(s) with the secret, read with their id, time and actor"

# The rest of what E5 reads gone, read here while it stands: the node's tuples, the version that
# holds the erased text, and each of the four stores that must hold no row with it afterwards.
want_held E3 "the comment's node holds tuples before the erase" "$node_tuples_sql"
want_held E3 "the version holding the secret is stored before the erase" "$version_sql"
for read in "${store_reads[@]}"; do
  want_held E3 "a ${read%%|*} holds the secret before the erase" "${read#*|}"
done

# The embedding windows both texts are cut into, which the block-index pass writes on its own
# tick (refs.WriteItemWindows for the comment, refs.WriteDocumentWindows for the document) and no
# request writes: the driver waits for both to hold the secret, so every run erases a node whose
# windows stand. A run that erased before the pass had cut them left nothing in `embeddings` for
# the search of E5 to find, which is how an erase that never touched that store passed some runs
# and failed others.
held_soon E3 "the comment's embedding windows hold the secret before the erase" \
  "select count(*) from embeddings where kind = 'comment_block'
     and split_part(id, '#', 1) = 'comment/$comment' and position('$secret' in text_snapshot) > 0"
held_soon E3 "the document's embedding windows hold the secret before the erase" \
  "select count(*) from embeddings where kind = 'document_block'
     and split_part(id, '#', 1) = '$document' and position('$secret' in text_snapshot) > 0"

# The answer an address nobody holds gives, read before the erase, so the comparison after it is
# against this server's own words and not against a status this driver chose.
missing="00000000-0000-4000-8000-000000000000"
send GET "/api/v1/references?to=$(q "dispatch://$key/comment/$missing")"
readonly nobody_status="$status"
cp "$work/body" "$work/nobody"
pass E3 "an address nobody holds answers HTTP $nobody_status"
# The node's own address, read before the erase: it resolves, so what it answers after the erase
# is the erase's doing, and the two answers E5 compares differ before it.
send GET "/api/v1/references?to=$(q "dispatch://$key/comment/$comment")"
want_status 200 E3 "the comment's address resolves before the erase"
[[ "$nobody_status" != 200 ]] ||
  fail E3 "an address nobody holds answers otherwise than one that resolves" \
    "both answered HTTP 200, so E5's comparison could not tell the erased node from a live one"
pass E3 "the comment's address answers HTTP 200 before the erase, where an address nobody holds answers HTTP $nobody_status"

# E4: the erase itself, under the block-index pass's lease, so no tick writes a window between the
# reads above and the erase and none while it runs. The lease goes back once the erase is done.
declared E4 "read the block index's lease" "$loop_source" \
  's/^const BlockIndexLease = "\(.*\)"$/\1/p'
lease_name="$answer"
declared E4 "read the first key of a loop lease" "$lease_source" \
  's/^const leaseNamespace int32 = \(0x[0-9A-Fa-f]*\)$/\1/p'
lease_namespace=$((answer))
hold_block_index E4
erase E4 --node "$node" --confirm
release_block_index
[[ "$erase_code" == 0 ]] ||
  fail E4 "erase $node" "exited $erase_code: $(<"$work/erase")"
grep -Fq "removed by" "$work/erase" || fail E4 "erase $node" "$(<"$work/erase")"
pass E4 "envoy-dispatch erase --node $node --confirm removed it, the $lease_name lease held across it and given back"

# E5: the bytes are gone, and so is everything that stood on them.
send GET "/api/v1/references?to=$(q "dispatch://$key/comment/$comment")"
[[ "$status" == "$nobody_status" ]] ||
  fail E5 "the node's address answers what a deleted node answers" \
    "HTTP $status, want $nobody_status: $(body)"
# The two answers differ in the address each was asked about and in nothing else, so each is
# read with its own id written as the same placeholder: what is compared is every other byte.
sed "s/$comment/<address>/g" "$work/body" >"$work/erased.answer"
sed "s/$missing/<address>/g" "$work/nobody" >"$work/nobody.answer"
cmp -s "$work/erased.answer" "$work/nobody.answer" ||
  fail E5 "the node's address answers what a deleted node answers" \
    "got $(head -c 300 "$work/erased.answer"), want $(head -c 300 "$work/nobody.answer")"
pass E5 "the node's address answers what an address nobody holds answers, byte for byte"
want_rows E5 "no node of it remains" 0 \
  "select count(*) from graph_nodes where kind = 'comment' and id = '$comment'"
want_rows E5 "no tuple of it remains" 0 "$node_tuples_sql"
want_rows E5 "no projection row of it remains" 0 \
  "select count(*) from comments where id = '$comment'"
want_rows E5 "the version holding the erased text is gone" 0 "$version_sql"
want_rows E5 "the review over those bytes is gone ($key, $document_ref version $version)" 0 \
  "select count(*) from artifact_reviews where artifact_id = '$document' and version = $version"
# Every store that can hold those bytes, which is what this check rests on: a search of two
# tables under a line that says "anywhere" reported none while three others held the secret
# (measured 2026-11-12 at fedf8392a354). The first four must hold no row with the text, and E3
# read each holding it; the event log is append-only, so its payload keeps the row with the text
# redacted.
for read in "${store_reads[@]}"; do
  want_rows E5 "no ${read%%|*} still holds the erased text" 0 "${read#*|}"
done
want_rows E5 "no event payload still holds the erased text" 0 \
  "select count(*) from events where payload::text like '%$secret%'"
# The document that held the secret still loads, from the one update the erase seeded its history
# with: its text keeps everything the erase did not take, and reads the marker where the secret
# was. It is read before the search below, so a read that wrote the secret back - a room still
# holding the old history, storing what it read - is a row that search finds.
send GET "/api/v1/artifacts/$document/text"
want_status 200 E5 "read $document_ref's live text after the erase"
jq -er '.markdown' "$work/body" >"$work/live.md" ||
  fail E5 "read $document_ref's live text after the erase" "no markdown: $(body)"
if grep -Fq "$secret" "$work/live.md"; then
  fail E5 "$document_ref's live text no longer holds the erased text" "$(<"$work/live.md")"
fi
grep -Fq "# Leaked notes" "$work/live.md" ||
  fail E5 "$document_ref keeps the text the erase did not take" "$(<"$work/live.md")"
grep -Fq "erased by envoy-dispatch erase $node, recorded in graph_erasures" "$work/live.md" ||
  fail E5 "$document_ref reads the marker where the erased text was" "$(<"$work/live.md")"
pass E5 "$document_ref still loads: its heading as it was, and the marker where the erased text was"
# Every column of every table, which the five lines above cannot say: they name the stores a run
# of 2026-11-12 measured, and a search of the whole database after a passing run of this driver
# found the secret in four more (doc_updates, text_trees, document_search and search_term_queue,
# measured 2026-11-13 at d92227ce05e3). This search names no store, so a store added later is
# searched without anyone adding a line here. The event log is searched with the rest: its rows
# stand with the marker where the secret was, which is not the secret.
held_anywhere E5 "$secret"
held="$(printf '%s\n' "$answer" | grep -v "^columns " | grep -v "^pg_temp_[0-9]*\.erase_control\.body " || true)"
[[ -z "$held" ]] ||
  fail E5 "no column of any table still holds the erased text" "$(printf '%s' "$held" | tr '\n' ';')"
printf '%s\n' "$answer" | grep -Eq "^pg_temp_[0-9]+\.erase_control\.body 1\$" ||
  fail E5 "the search finds the control row planted beside it" "the search answered: $(printf '%s' "$answer" | tr '\n' ';')"
pass E5 "no column of any table holds the erased text ($(printf '%s\n' "$answer" | sed -n 's/^columns //p') columns searched), and the same search found the control row planted in it"
# What the event log holds instead, which the four lines above cannot say: the marker naming the
# erase (graphcensus.eraseMarker, one constant), through which a reader of a redacted payload
# reaches the `graph_erasures` row recording the run. The rows are the rows E3 read, each with
# the id, the time and the actor it had then, because R5 moves the payload and nothing else.
marker="[erased by envoy-dispatch erase $node, recorded in graph_erasures]"
readonly marker
want_rows E5 "the event log holds every row that held the erased text, redacted and marked" \
  "$events_held" "select count(*) from events where position('$marker' in payload::text) > 0"
query E5 "read the id, time and actor of each redacted event row" \
  "select coalesce(jsonb_agg(jsonb_build_object('id', id, 'created_at', created_at, 'actor', actor) order by id), '[]'::jsonb)::text
     from events where position('$marker' in payload::text) > 0"
[[ "$answer" == "$events_before" ]] ||
  fail E5 "the redacted rows are the rows that held the secret, their id, time and actor untouched" \
    "now $answer, before the erase $events_before"
pass E5 "the redacted event rows are those rows, each with its id, its time and its actor unchanged"
grep -Eq "^  redacted: events [1-9][0-9]*\$" "$work/erase" ||
  fail E5 "the run says how many rows it redacted" "no redacted events line in: $(<"$work/erase")"
pass E5 "envoy-dispatch erase reported the rows it redacted"
# The erase waits out the bound of a read's detached store of its display trees after its commit
# and deletes once more the trees it deleted (graphcensus's sweepTrees), so a read that began
# before the commit cannot leave the tree of an erased text behind; the report says how many it
# found then.
grep -Eq "^  text_trees swept after the commit: [0-9]+\$" "$work/erase" ||
  fail E5 "the run says how many display trees it swept after the commit" "no sweep line in: $(<"$work/erase")"
pass E5 "envoy-dispatch erase reported its sweep of text_trees after the commit"

# E6: the record of the erase: one row, read whole.
want_rows E6 "the erase recorded one graph_erasures row" 1 \
  "select count(*) from graph_erasures where node_kind = 'comment' and node_id = '$comment'"
query E6 "read the graph_erasures row" \
  "select jsonb_build_object('node_kind', node_kind, 'node_id', node_id, 'removed', removed, 'ran_by', ran_by, 'ran_at', ran_at)::text from graph_erasures where node_id = '$comment'"
[[ -n "$answer" ]] || fail E6 "read the graph_erasures row" "the erase recorded nothing"
printf '%s' "$answer" >"$work/erasure.json"
jq -e --arg id "$comment" '.node_kind == "comment" and .node_id == $id' "$work/erasure.json" >/dev/null ||
  fail E6 "the record names the node" "$(<"$work/erasure.json")"
jq -e '(.removed.rows | length) > 0' "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds the rows it removed" "$(<"$work/erasure.json")"
# The version and the review this run wrote, each by its own name: a record that named some other
# version or review, or none, is not the record of this erase.
jq -e --arg doc "$document_ref" --arg key "$key" --argjson v "$version" \
  'any(.removed.versions[]; .document == $doc and .issue == $key and .version == $v)' \
  "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds the version the erase ended ($document_ref version $version of $key)" \
    "$(jq -c '.removed.versions' "$work/erasure.json")"
jq -e --arg doc "$document_ref" --arg key "$key" --argjson v "$version" --arg r "$review" \
  'any(.removed.reviews[]; .id == $r and .document == $doc and .issue == $key and .version == $v)' \
  "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds the review the erase ended (review $review of $document_ref version $version)" \
    "$(jq -c '.removed.reviews' "$work/erasure.json")"
jq -e '(.ran_by.database_user | length) > 0 and (.ran_at | length) > 0' "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds who ran it and when" "$(<"$work/erasure.json")"
pass E6 "the graph_erasures row holds what was removed - $document_ref version $version and review $review among it - who ran it and when"
jq -e --arg s "$secret" 'tostring | contains($s) | not' "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds no erased text" "the graph_erasures row holds the erased secret"
pass E6 "the graph_erasures row holds no erased text"

# E7: the erase wrote no node of its own, so every node left carries a supertag of the closed
# list the store itself holds.
want_rows E7 "every node carries a supertag the store declares" 0 \
  "select count(*) from graph_nodes n where not exists (select 1 from graph_nodes s where s.kind = 'supertag' and s.id = n.supertag)"
query E7 "count the supertag nodes" "select count(*) from graph_nodes where kind = 'supertag'"
[[ "$answer" == 29 ]] ||
  fail E7 "the closed list holds 29 supertags" "the store holds $answer supertag node(s)"
pass E7 "the closed list of 29 supertags is what every node's supertag comes from"

# E8: the census after the erase reads zero differences.
census_code=0
env -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE DATABASE_URL="$database_url" \
  envoy-dispatch census --store >"$work/census" 2>&1 || census_code=$?
[[ "$census_code" == 0 ]] ||
  fail E8 "the store census after the erase" "envoy-dispatch census --store exited $census_code: $(cat "$work/census")"
grep -Eq '^census: store rows_checked=[0-9,]+ differences=0$' "$work/census" ||
  fail E8 "the store census after the erase" "$(grep -E '^census: store (rows_checked|REFUSED)' "$work/census" || true)"
# Zero differences over zero rows compares nothing with nothing, so the census must have read rows.
grep -Eq '^census: store rows_checked=[1-9][0-9,]* ' "$work/census" ||
  fail E8 "the store census read rows" "$(grep -E '^census: store rows_checked' "$work/census" || true)"
grep -Fqx 'census: store ok' "$work/census" ||
  fail E8 "the store census after the erase" "$(tail -n 3 "$work/census")"
pass E8 "envoy-dispatch census --store reads zero differences over $(sed -n 's/^census: store rows_checked=\([0-9,]*\) .*$/\1/p' "$work/census") rows"

# E9: no route serves the erase. Both a credential that reaches every write route and a person's
# own cookie read 404, which is what a path this server does not mount answers.
for path in "/api/v1/erase" "/api/v1/nodes/comment/$comment" "/api/v1/graph/erase"; do
  for method in POST DELETE; do
    send "$method" "$path" '{"node":"comment:x","confirm":true}'
    [[ "$status" == 404 ]] ||
      fail E9 "the agent bearer reaches no erase route" "$method $path answered HTTP $status: $(body)"
    person_send "$method" "$path" '{"node":"comment:x","confirm":true}'
    [[ "$status" == 404 ]] ||
      fail E9 "a person's cookie reaches no erase route" "$method $path answered HTTP $status: $(body)"
  done
done
pass E9 "the erase is reached by the command and the database credential alone: every HTTP path answers 404"

printf 'graph-erase.sh: every check passed\n'
