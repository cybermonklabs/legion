# shellcheck shell=bash
# The seed of verification v16 and the vocabulary the two private-projects drivers share:
# project-access.sh (c-private-unseen) and project-access-blob.sh (c-blob-unchanged). Sourced,
# never run, after scripts/e2e/lib/two-builds.sh, whose servers, identity and loopback guard it
# uses.
#
# A driver talks to Dispatch servers it started, each on a database of its own (own_database) and
# under a name of its own (the blob driver's `base` and `head`, project-access.sh's `main` and
# `twin`), and registers each name's URL in build_url and its database's in NAME_database_url
# before it sends anything. It sets, before it calls anything here:
#   work          its own directory
#   call_seconds  how long one request waits (two-builds.sh sets 20)
#   run_epoch     the run's start, in seconds since the epoch
#   agent_token   the shared agent token its servers take (two_builds_identity)
#   database_url  the caller's DATABASE_URL, the server own_database makes databases on
# and defines fail, which reports and exits, and note, which prints a line of the run's progress.
# Everything a seed writes goes through the server's own routes, as a person at the dashboard or
# an agent would write it; a fact no route of this stack writes is named where it is written.
#
# The seed's three people, by the names verification v16 gives them: alice, a full member that no
# access list names; bob, a restricted member that PRIV's list names; carol, a full member that
# PRIV's list names, who makes both projects and so owns both.

: "${work:=}" "${call_seconds:=}" "${run_epoch:=}" "${agent_token:=}" "${database_url:=}"
declare -A build_url=()

readonly alice=alice@example.com bob=bob@outside.example carol=carol@example.com
# The session the shared token's writes name as their actor.
readonly seed_session=project-access-seed

# stop_all PID... stops each PID as lib/rig.sh's stop_pid does, but sends every one its TERM before
# it waits on any, so they stop together rather than each after the one before has gone. It is a
# cleanup's, so it never fails.
stop_all() {
  local pid
  for pid in "$@"; do
    [ -z "$pid" ] || ! signalable "$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$@"; do
    stop_pid "$pid"
  done
}

# own_database VAR NAME makes a database for the server NAME, on the server DATABASE_URL names,
# with the vector extension every build migrates on, and assigns VAR its connection string. The
# database's name carries this shell's pid and random bytes, so two runs on one server never meet.
# It runs in the caller's shell, not in a substitution, so made_databases holds every database it
# made, which drop_own_databases drops.
made_databases=()
own_database() {
  local name _url
  name="project_access_$$_$(openssl rand -hex 4)_$2"
  run_database_url _url DATABASE_URL "$database_url" "$name" "this run creates and drops a database there" ||
    fail "$_url"
  psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c "create database \"$name\"" >/dev/null ||
    fail "could not create database $name"
  made_databases+=("$name")
  psql "$_url" -X -q -v ON_ERROR_STOP=1 -c 'create extension if not exists vector' >/dev/null ||
    fail "could not install the vector extension in $name, which only the cluster's master role can"
  printf -v "$1" '%s' "$_url"
}

# drop_own_databases drops every database own_database made. It is a cleanup's, so it never fails.
drop_own_databases() {
  local name
  for name in "${made_databases[@]}"; do
    psql "$database_url" -X -q -c "drop database if exists \"$name\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop database $name; drop it by hand" >&2
  done
}

email_of() {
  case "$1" in
    alice*) printf '%s' "$alice" ;;
    bob*) printf '%s' "$bob" ;;
    carol*) printf '%s' "$carol" ;;
    *) fail "no person is named $1" ;;
  esac
}

# send BUILD CALLER METHOD PATH [BODY]: one request to BUILD as CALLER, the status in $status, the
# body in $work/body and the response headers in $work/headers. A person (alice, bob, carol) sends
# their session cookie and the build's origin, as a browser does; any other caller sends the header
# $work/auth/BUILD-CALLER holds, so no argument list carries a bearer.
send() {
  local build=$1 caller=$2 method=$3 path=$4 body=${5:-} url=${build_url[$1]}
  local -a credential data=()
  case "$caller" in
    alice | bob | carol) credential=(-b "$work/cookies/$build-$caller" -H "Origin: $url") ;;
    *) credential=(-H "@$work/auth/$build-$caller") ;;
  esac
  [ -z "$body" ] || data=(-H 'Content-Type: application/json' --data-binary "$body")
  status=$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' --max-time "$call_seconds" -X "$method" \
    "${credential[@]}" "${data[@]}" "$url$path" 2>"$work/curl") || status="curl failed: $(<"$work/curl")"
}

# expect_status CODE WHAT: the last answer is CODE.
expect_status() {
  [ "$status" = "$1" ] || fail "$2: HTTP $status, want $1: $(head -c 600 "$work/body")"
}

# event_log_reaches BUILD CALLER ID: BUILD's event log, as CALLER reads it, holds an event at ID or
# above.
event_log_reaches() {
  send "$1" "$2" GET "/api/v1/events/log?after=$(($3 - 1))&limit=1"
  [ "$status" = 200 ] && jq -e --argjson id "$3" 'any(.[]; .id >= $id)' "$work/body" >/dev/null
}

# Each server writes its meaning index behind every write, in its own time and in two steps: the
# block index's window arm writes the windows of each document, comment, message and ask written
# since its last tick (docs.RunBlockIndexReconciliation, which ticks every 30 s once no batch of it
# is full, BlockIndexReconcileInterval), and the embedding queue then embeds each window, and each
# issue, it holds no current vector of (embedqueue.Run, whose scans start at least 5 s apart,
# retryInterval, each of at most one embedder batch). Until both are done a block reads
# `"embedding":"none"` and then `"pending"` rather than `"embedded"`, and the coverage route and
# meaning search answer short, so two servers read in between answer apart for no reason of
# access: on CI one answered `"embedded"` where the other answered `"pending"`, and search
# coverage `"current":1` where the other answered 2. A driver that compares two servers after a
# write that indexes reads them only once both have settled (await_index_settled).
#
# index_settled BUILD holds once BUILD's meaning index has nothing waiting, read in this order:
#   1. Its window arm owes no item its windows: no row of document_block_versions,
#      comment_block_state, message_block_state or ask_block_state, the markers that arm reads
#      (refs.DocumentsOwedWindows, refs.ItemsOwedWindows), carries windows_owed_at, in BUILD's
#      database (BUILD_database_url). The coverage route cannot say this: a window kind's items
#      are its windows (model.SearchCoverageKinds, embedqueue.Coverage), so an item the arm has
#      not windowed yet is no item of the route's, and the route reads nothing lacking while a
#      tick's windows are still to come. No route reads the markers.
#   2. Its coverage route (GET /api/v1/search/coverage), as carol reads it, has nothing waiting:
#      it counts an item, and in the total and in each kind (issue, document_block,
#      comment_block, message_block, ask_part) lacking is 0, an item with no current vector, and
#      failed is 0, an item the queue gave up on, so one it does not try again for the same text.
#      The route counts only what its reader may see, and carol, who owns both projects and whom
#      PRIV's list names, sees every project these seeds write, so every caller a compare reads
#      as sees a part of what she does. A route that counts no item says nothing of what a
#      compare reads, so it does not hold. A server that runs no embedder names no identity
#      there (null), and no vector will ever be written: it is not waited on for vectors, and is
#      still waited on for its windows, which the window arm writes with no embedder, and which
#      a block's embedding reads ("none" before them, "pending" after).
# The markers are read before the route, so a window written between the two reads is one the
# route counts. When BUILD has not settled it returns 1 with why in $unsettled.
unsettled=''
readonly windows_owed_counts="select
    (select count(*) from document_block_versions where windows_owed_at is not null),
    (select count(*) from comment_block_state where windows_owed_at is not null),
    (select count(*) from message_block_state where windows_owed_at is not null),
    (select count(*) from ask_block_state where windows_owed_at is not null)"
# What of a coverage report is still waiting, as one line, or nothing when nothing is.
# shellcheck disable=SC2016 # a jq program: jq, not the shell, expands its \(…)
readonly coverage_waiting='def counts: "\(.items) items, \(.current) current, \(.lacking) lacking, \(.failed) failed";
  if .identity == null then empty
  elif (.total.items // 0) == 0 then "its coverage route counts no item, so it says nothing of what a compare reads"
  else [.kinds | to_entries[] | select(.value.lacking != 0 or .value.failed != 0) | "\(.key) \(.value | counts)"] as $kinds
    | if ($kinds | length) == 0 and .total.lacking == 0 and .total.failed == 0 then empty
      else "its coverage route counts \(.total | counts) in all" + (if $kinds == [] then "" else ": " + ($kinds | join("; ")) end)
      end
  end'
index_settled() {
  local build=$1 url_var=${1}_database_url owed documents comments messages asks
  [ -n "${!url_var:-}" ] || fail "index_settled: $url_var names no database of the $build server"
  if ! owed=$(psql "${!url_var}" -X -q -tA -F ' ' -v ON_ERROR_STOP=1 -c "$windows_owed_counts" 2>&1); then
    unsettled="its database's window markers could not be read: $(head -c 300 <<<"$owed")"
    return 1
  fi
  read -r documents comments messages asks <<<"$owed"
  if [ "$documents $comments $messages $asks" != '0 0 0 0' ]; then
    unsettled="its window arm owes ${documents:-?} documents, ${comments:-?} comments, ${messages:-?} messages and ${asks:-?} asks their windows"
    return 1
  fi
  send "$build" carol GET /api/v1/search/coverage
  if [ "$status" != 200 ]; then
    unsettled="GET /api/v1/search/coverage answered HTTP $status: $(head -c 300 "$work/body")"
    return 1
  fi
  unsettled=$(jq -r "$coverage_waiting" "$work/body" 2>/dev/null) ||
    unsettled="GET /api/v1/search/coverage answered no coverage report: $(head -c 300 "$work/body")"
  [ -z "$unsettled" ]
}

# How long await_index_settled waits for a server these drivers start. Those servers run the dev
# embedder (DISPATCH_DEV_EMBEDDER=1), which embeds in the process, calls no service and spends a
# budget of 100,000,000 tokens a minute nothing reaches (embed.DevClient's Limits), so no batch is
# ever throttled: after a seed's last write, its windows wait at most one tick of the window arm,
# 30 s, and their vectors at most one scan's gap, 5 s, and a batch of 96 of them, more than a
# seed here writes, embeds in milliseconds. 120 s is over three times those 35 s, room for a CI
# runner whose statements run slow, and the bound every other wait of these drivers on a
# server's own background work takes.
# shellcheck disable=SC2034 # read by the drivers that source this file
readonly index_settle_seconds=120

# await_index_settled SECONDS BUILD... waits, bounded, until every BUILD's meaning index has
# settled (index_settled), reading each one not settled yet every half second, and notes each one
# as it settles, with how long it took. A settled server is read no more: nothing writes while a
# driver waits, so its index stays as it is. Past SECONDS it fails the run, naming each server
# that has not settled and what it still waits on: the windows its window arm owes, or its
# coverage's counts in all and of each kind still lacking or failed.
await_index_settled() {
  local limit=$1 started=$SECONDS polls=0 build reasons=''
  shift
  local -a waiting=("$@") still
  local -A why=()
  note "waiting up to ${limit}s for the meaning index of $* to settle"
  while :; do
    polls=$((polls + 1))
    still=()
    for build in "${waiting[@]}"; do
      if index_settled "$build"; then
        note "$build's meaning index settled after $((SECONDS - started))s ($polls polls)"
      else
        still+=("$build")
        why[$build]=$unsettled
      fi
    done
    waiting=("${still[@]}")
    [ "${#waiting[@]}" -gt 0 ] || return 0
    ((SECONDS - started < limit)) || break
    sleep 0.5
  done
  for build in "${waiting[@]}"; do
    reasons+=$'\n'"  $build: ${why[$build]}"
  done
  fail "timed out after ${limit}s ($polls polls) waiting for the meaning index to settle:$reasons"
}

# remember BUILD LABEL VALUE records VALUE, an id BUILD's seed wrote new, under LABEL: the mask
# reads it as <LABEL>, and a path names it as {LABEL} (resolve). The mask replaces VALUE wherever it
# stands in an answer, so a value shorter than eight bytes, which could stand inside other text, is
# refused rather than masked.
remember() {
  [ -n "$3" ] && [ "$3" != null ] || fail "the seed of the $1 build read no $2"
  [ "${#3}" -ge 8 ] || fail "the seed of the $1 build read $2 as '$3', too short to mask"
  printf '%s\t%s\n' "$2" "$3" >>"$work/ids-$1.tsv"
}
recalled() { awk -F '\t' -v label="$2" '$1 == label { print $2; exit }' "$work/ids-$1.tsv"; }

# resolve BUILD PATH prints PATH with each {LABEL} replaced by the id BUILD's seed recorded under
# it, URL-encoded as one path or query component.
resolve() {
  local build=$1 path=$2 label value
  while [[ "$path" =~ \{([^{}]+)\} ]]; do
    label=${BASH_REMATCH[1]}
    value=$(recalled "$build" "$label")
    [ -n "$value" ] || fail "the seed of the $build build recorded no $label"
    path=${path//"{$label}"/$(jq -rn --arg v "$value" '$v|@uri')}
  done
  printf '%s' "$path"
}

# sign_in_as BUILD PERSON signs PERSON in at BUILD's dev sign-in route, which records them in
# `people` as a real sign-in does, and keeps their session cookie for send, as a cookie jar and as
# the one `Cookie:` header line socket-access.ts reads.
sign_in_as() {
  local build=$1 person=$2 answer jar=$work/cookies/$1-$2
  answer=$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$call_seconds" -c "$jar" \
    "${build_url[$build]}/auth/_dev/signin?login=$(email_of "$person")") || answer=000
  if [ "$answer" != 302 ] || ! grep -q dsession "$jar"; then
    fail "$build: the dev sign-in of $person answered $answer with no session cookie"
  fi
  awk -F '\t' '$6 == "dsession" { print "Cookie: " $6 "=" $7 }' "$jar" >"$work/auth/$build-$person"
}

# with_actor CALLER JSON: the shared token's writes name the session that makes them; a person's
# and a personal token's name none, since the credential is the writer.
with_actor() {
  if [ "$1" = shared ]; then
    jq -c --arg s "$seed_session" '. + {actor: {kind: "session", id: $s}}' <<<"$2"
  else
    printf '%s' "$2"
  fi
}

# seed_people BUILD signs the three people in and writes the shared token's header.
seed_people() {
  local build=$1 person
  mkdir -p "$work/cookies" "$work/auth" "$work/answers"
  : >"$work/ids-$build.tsv"
  printf 'Authorization: Bearer %s\n' "$agent_token" >"$work/auth/$build-shared"
  for person in alice bob carol; do
    sign_in_as "$build" "$person"
  done
}

# make_project BUILD KEY: carol makes the project KEY, and so owns it.
make_project() {
  send "$1" carol POST /api/v1/projects "$(jq -cn --arg k "$2" '{key: $k, name: ("Project " + $k)}')"
  expect_status 201 "$1: carol makes $2"
}

# seed_objects BUILD KEY AUTHOR writes the objects of project KEY: carol's issue KEY-1, whose spec
# is a document with blocks, a comment on it by AUTHOR, carol's ask, carol's message, a document
# of the issue and one of the project. Each one's id is remembered under "KEY <kind>", and each
# block of the spec, whose ids are new uuids, under "KEY block <n>" in document order.
seed_objects() {
  local build=$1 project=$2 author=$3 key block index=0
  send "$build" carol POST /api/v1/issues "$(jq -cn --arg p "$project" \
    '{project: $p, title: ("The issue of " + $p), spec: "# Spec\n\nThe first paragraph.\n"}')"
  expect_status 201 "$build: carol opens the issue of $project"
  key=$(jq -r .key "$work/body")
  [ "$key" = "$project-1" ] || fail "$build: the issue of $project is $key, not $project-1"
  send "$build" carol GET "/api/v1/issues/$key/summary"
  expect_status 200 "$build: carol reads $key"
  remember "$build" "$project spec" "$(jq -r '.documents[] | select(.primary) | .id' "$work/body")"
  send "$build" carol GET "$(resolve "$build" "/api/v1/artifacts/{$project spec}/blocks")"
  expect_status 200 "$build: carol reads the blocks of $key's spec"
  while IFS= read -r block; do
    index=$((index + 1))
    remember "$build" "$project block $index" "$block"
  done < <(jq -r '.[].id' "$work/body")
  [ "$index" -gt 0 ] || fail "$build: the spec of $key holds no block"
  send "$build" "$author" POST "/api/v1/issues/$key/comments" "$(jq -cn --arg a "$author" '{body: ("A remark from " + $a + ".")}')"
  expect_status 201 "$build: $author comments on $key"
  remember "$build" "$project comment" "$(jq -r .id "$work/body")"
  send "$build" carol POST "/api/v1/issues/$key/asks" '{"question":"Is this the right plan?"}'
  expect_status 201 "$build: carol asks on $key"
  remember "$build" "$project ask" "$(jq -r .id "$work/body")"
  send "$build" carol POST "/api/v1/issues/$key/messages" '{"body":"A message from carol."}'
  expect_status 201 "$build: carol writes a message on $key"
  remember "$build" "$project message" "$(jq -r .id "$work/body")"
  send "$build" carol POST "/api/v1/issues/$key/artifacts" '{"name":"notes.md","content":"# Notes\n\nCarol'"'"'s notes.\n"}'
  expect_status 201 "$build: carol uploads a document to $key"
  remember "$build" "$project notes" "$(jq -r .artifact.id "$work/body")"
  send "$build" carol POST "/api/v1/projects/$project/artifacts" '{"name":"Playbook.md","content":"# Playbook\n\nHow we work.\n"}'
  expect_status 201 "$build: carol uploads a document to $project"
  remember "$build" "$project playbook" "$(jq -r .artifact.id "$work/body")"
}

# mint_token BUILD PERSON CALLER [PROJECTS] mints PERSON a personal token at the mint route, as
# the caller named CALLER: listing the projects PROJECTS (a JSON array) names, or, left out, no
# list, which is the one token a build before lists can mint. Its header goes to
# $work/auth/BUILD-CALLER, and its plaintext, id and prefix are remembered under CALLER.
mint_token() {
  local build=$1 person=$2 caller=$3 projects=${4:-} body minted
  body=$(jq -cn --arg n "$caller" --argjson p "${projects:-null}" '{name: $n} + (if $p == null then {} else {projects: $p} end)')
  send "$build" "$person" POST /api/v1/me/agent-tokens "$body"
  expect_status 201 "$build: $person mints $caller"
  minted=$(jq -r .token "$work/body")
  printf 'Authorization: Bearer %s\n' "$minted" >"$work/auth/$build-$caller"
  remember "$build" "$caller" "$minted"
  remember "$build" "$caller id" "$(jq -r .id "$work/body")"
  remember "$build" "$caller prefix" "$(jq -r .prefix "$work/body")"
}

# The durations and the window start the mask reads as <duration> and <time>, by their JSON
# member: each is measured again at every read from the moment of the read, so no two reads of
# the same row agree on it.
#   took_ms       how long a search took (GET /api/v1/search, /search/results)
#   age_seconds   how old an open ask is (GET /api/v1/asks/open)
#   oldest_lacking_seconds
#                 how long the oldest item that lacks a current embedding has waited
#                 (GET /api/v1/search/coverage), in whole seconds, so the twin's read a second
#                 later answers one more
#   keyword_ms, meaning_ms, total_ms
#                 the timing summary {count, p50, p95, max, over_300ms} of the related-item checks
#                 the run made (GET /api/v1/related-checks)
#   since         the start of a window that ends at the read, an RFC 3339 instant
#                 (GET /api/v1/agents/{session_id}/answers); and, as a decimal event id, the
#                 cursor the live socket's events_start frame names, which is the newest event
#                 the caller can see (viewerEventHead), so no event of a project it does not see
#                 moves it
#   from          the default start of a delivery window, seven days before the read
#                 (GET /api/v1/delivery/timeline, /measures)
#   last_seen     a session's last heartbeat as the listener reports it, in epoch milliseconds
#                 (GET /api/v1/agents); each server reads a fake listener of its own
readonly durations='["took_ms","age_seconds","oldest_lacking_seconds"]'
readonly timings='["keyword_ms","meaning_ms","total_ms"]'
readonly window_starts='["since","from"]'
readonly event_cursors='["since"]'
readonly epoch_instants='["last_seen"]'

# mask BUILD FILE [EXTRA] prints FILE with every id BUILD's seed recorded replaced by <LABEL>, each
# `LABEL<tab>VALUE` line of the file EXTRA likewise, every RFC 3339 instant the run wrote (between
# run_epoch and now) by <time>, every duration by <duration>, the window starts and epoch instants
# named above by <time>, and the event cursors named above (`since`) by <event id>, and is
# otherwise the body byte for byte: jq reads it as one raw string and writes it back as it was.
# Nothing else is masked: keys, slugs, names, versions and the event numbers an event itself carries
# are each seed's own and are compared as they are.
mask() {
  local build=$1 file=$2 extra=${3:-/dev/null}
  jq -Rrj --slurp \
    --rawfile ids "$work/ids-$build.tsv" \
    --rawfile extra "$extra" \
    --argjson durations "$durations" \
    --argjson timings "$timings" \
    --argjson window_starts "$window_starts" \
    --argjson event_cursors "$event_cursors" \
    --argjson epoch_instants "$epoch_instants" \
    --argjson from "$run_epoch" \
    --argjson to "$(date +%s)" '
      def literal: gsub("(?<c>[.*+?^$|(){}\\[\\]\\\\/])"; "\\\(.c)");
      def instant:
        (sub("\\.[0-9]+"; "") | sub("Z$"; "+00:00")) as $t
        | ($t[0:19] + "Z" | fromdateiso8601)
          - (($t[19:20] + "1" | tonumber) * (($t[20:22] | tonumber) * 3600 + ($t[23:25] | tonumber) * 60));
      def written: (try instant catch null) as $at | $at != null and $at >= $from - 1 and $at <= $to + 1;
      reduce (($extra + $ids) | split("\n")[] | select(. != "") | split("\t")) as $id (.;
        gsub($id[1] | literal; "<" + $id[0] + ">"))
      | gsub("(?<t>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2}))";
          if (.t | written) then "<time>" else .t end)
      | reduce $durations[] as $member (.; gsub("\"\($member)\":[0-9]+"; "\"\($member)\":<duration>"))
      | reduce $timings[] as $member (.; gsub("\"\($member)\":\\{[^{}]*\\}"; "\"\($member)\":<duration>"))
      | reduce $window_starts[] as $member (.;
          gsub("\"\($member)\":\"[0-9]{4}-[0-9]{2}-[0-9]{2}T[^\"]*\""; "\"\($member)\":\"<time>\""))
      | reduce $event_cursors[] as $member (.; gsub("\"\($member)\":\"[0-9]+\""; "\"\($member)\":\"<event id>\""))
      | reduce $epoch_instants[] as $member (.; gsub("\"\($member)\":[0-9]+"; "\"\($member)\":<time>"))' "$file"
}

# The response headers are compared beside the status and the body, each `name: value` line with
# the name in lower case, the status line left out, sorted, and masked as the body is. The value of
# each header below is masked as <masked>, with why it differs between two answers of one server,
# and its line is kept, so a header one answer sends and the other does not still differs:
#   date            the answer's own time
#   content-length  the raw body's length; the bodies are compared masked, and an id or a duration
#                   the mask names can be a byte longer in one answer
#   etag            a digest of the raw body, which holds each server's own ids
readonly masked_headers='date|content-length|etag'

# keep_answer NAME BUILD [EXTRA] keeps the last answer, masked as BUILD's (mask), as
# $work/answers/NAME.status, NAME.body and NAME.headers (masked_headers), for same_answers, and
# the body as it was sent as NAME.raw. A socket's answer, whose status is no HTTP status, has no
# headers.
keep_answer() {
  printf '%s' "$status" >"$work/answers/$1.status"
  cp "$work/body" "$work/answers/$1.raw"
  mask "$2" "$work/body" "${3:-}" >"$work/answers/$1.body"
  if [[ "$status" =~ ^[0-9]{3}$ ]]; then
    tr -d '\r' <"$work/headers" | sed -E '1d; /^$/d; s/^([^:]+):[[:space:]]*/\L\1\E: /' |
      sed -E "s/^($masked_headers): .*/\1: <masked>/" | LC_ALL=C sort >"$work/answers/$1.header-lines"
    mask "$2" "$work/answers/$1.header-lines" "${3:-}" >"$work/answers/$1.headers"
  else
    : >"$work/answers/$1.headers"
  fi
}

# first_difference A B prints the 1-based offset of the first byte at which the files A and B
# differ, as cmp names it; past the end of the shorter file, that is one past its length. It
# returns 1, printing nothing, when they are the same.
first_difference() {
  local said
  said=$(cmp "$1" "$2" 2>&1) && return 1
  case "$said" in
    *'is empty'*) echo 1 ;;
    *EOF*) echo $(($(sed -nE 's/.*after (byte|char) ([0-9]+).*/\2/p' <<<"$said") + 1)) ;;
    *) sed -nE 's/.*differ: (byte|char) ([0-9]+),.*/\2/p' <<<"$said" ;;
  esac
}

# Every answer judged counts in compared, a pair as one and a read held to a rule of its own as
# one, and an answer that fails its judgement counts once in differed, so no_difference can never
# name more answers that differ than it judged.
compared=0
differed=0
# same_answers WHAT LEFT RIGHT [LEFT_LINE RIGHT_LINE]: the answers kept as LEFT and RIGHT
# (keep_answer) are the same status, the same bytes and the same headers. Bytes the two requests
# were sent alike are the same answer before any mask: a caller cannot tell them apart, and a mask
# could only part them (a refusal that names the caller's own list, which a fill also names). Other
# bytes are compared masked, each with its own fill. LEFT_LINE and RIGHT_LINE, each a header line
# or empty, are the lines the two servers are declared to answer apart: LEFT's headers must hold
# LEFT_LINE and RIGHT's RIGHT_LINE, and each line is dropped from both before they are compared. A
# pair that differs, however many of these it breaks, counts once, and is written to
# $work/differences under WHAT, with both answers around the first byte that differs or both
# headers that differ; no_difference reads that file.
same_answers() {
  local what=$1 left=$work/answers/$2 right=$work/answers/$3 at differs=0 side line
  local -a drop=()
  compared=$((compared + 1))
  printf '%s\n' "$(<"$right.status")" >>"$work/statuses"
  if [ "$(<"$left.status")" != "$(<"$right.status")" ]; then
    differs=1
    printf '%s: %s answered HTTP %s, %s HTTP %s\n   %s: %s\n   %s: %s\n' "$what" "$2" "$(<"$left.status")" \
      "$3" "$(<"$right.status")" "$2" "$(head -c 400 "$left.body")" "$3" "$(head -c 400 "$right.body")" >>"$work/differences"
  elif cmp -s "$left.raw" "$right.raw"; then
    :
  elif at=$(first_difference "$left.body" "$right.body"); then
    differs=1
    printf '%s (HTTP %s): the answers differ at byte %s\n   %s: %s\n   %s: %s\n' "$what" "$(<"$right.status")" "$at" \
      "$2" "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$left.body" | head -c 240)" \
      "$3" "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$right.body" | head -c 240)" >>"$work/differences"
  fi
  for side in "$2" "$3"; do
    line=${4:-}
    [ "$side" = "$2" ] || line=${5:-}
    [ -n "$line" ] || continue
    drop+=(-e "$line")
    grep -qxF "$line" "$work/answers/$side.headers" && continue
    differs=1
    printf '%s: %s answered no "%s": %s\n' "$what" "$side" "$line" \
      "$(grep -i "^${line%%:*}:" "$work/answers/$side.headers" || echo 'the header is absent')" >>"$work/differences"
  done
  for side in "$left" "$right"; do
    if [ ${#drop[@]} -gt 0 ]; then
      grep -vxF "${drop[@]}" "$side.headers" >"$side.headers.compared" || true
    else
      cp "$side.headers" "$side.headers.compared"
    fi
  done
  if ! cmp -s "$left.headers.compared" "$right.headers.compared"; then
    differs=1
    printf '%s: the headers differ:\n%s\n' "$what" "$(diff "$left.headers.compared" "$right.headers.compared" |
      sed -nE "s/^< /   $2: /p; s/^> /   $3: /p")" >>"$work/differences"
  fi
  [ "$differs" = 0 ] || differed=$((differed + 1))
}

# judge STATUS PROBLEM counts one answer held to a rule of its own rather than beside another (a
# status it must have, a name it must or must not hold): STATUS joins the statuses, and PROBLEM,
# when it is not empty, is how the answer broke the rule, written to $work/differences as one that
# differed.
judge() {
  compared=$((compared + 1))
  printf '%s\n' "$1" >>"$work/statuses"
  [ -z "$2" ] || {
    differed=$((differed + 1))
    printf '%s\n' "$2" >>"$work/differences"
  }
}

# no_difference fails the check when any pair so far differed, printing every one.
no_difference() {
  [ "$differed" -eq 0 ] || fail "$differed of $compared answers differ:
$(<"$work/differences")"
}

# statuses prints how many of the answers compared came back with each status, so a run whose
# every answer is the same refusal reads as that rather than as a pass over the seed.
statuses() { sort "$work/statuses" | uniq -c | awk '{ printf "%s%s %s", sep, $2, $1; sep = ", " }'; }
