# shellcheck shell=bash
# The seed of verification v16 (dispatch://INFRA-3862) and the vocabulary the two private-projects
# drivers share: project-access.sh (c-private-unseen) and project-access-blob.sh
# (c-blob-unchanged). Sourced, never run, after scripts/e2e/lib/two-builds.sh, whose servers,
# identity and loopback guard it uses.
#
# A driver talks to Dispatch servers it started, each on a database of its own (own_database) and
# under a name of its own (the blob driver's `base` and `head`, project-access.sh's `main` and
# `twin`), and registers each name's URL in build_url before it sends anything. It sets, before it
# calls anything here:
#   work          its own directory
#   call_seconds  how long one request waits (two-builds.sh sets 20)
#   run_epoch     the run's start, in seconds since the epoch
#   agent_token   the shared agent token its servers take (two_builds_identity)
#   database_url  the caller's DATABASE_URL, the server own_database makes databases on
# and defines fail, which reports and exits. Everything a seed writes goes through the server's
# own routes, as a person at the dashboard or an agent would write it; a fact no route of this
# stack writes is named where it is written.
#
# The seed's three people, by the names verification v16 gives them: alice, a full member that no
# access list names; bob, a restricted member that PRIV's list names; carol, a full member that
# PRIV's list names, who makes both projects and so owns both.

: "${work:=}" "${call_seconds:=}" "${run_epoch:=}" "${agent_token:=}" "${database_url:=}"
declare -A build_url=()

readonly alice=alice@example.com bob=bob@outside.example carol=carol@example.com
# The session the shared token's writes name as their actor.
readonly seed_session=project-access-seed

# free_port prints a port the kernel chose and nothing holds. Another process can take it before
# the server binds it; two_builds_prerequisites refuses a port that answers, and a server that
# loses the race fails to bind, so a run that loses it fails rather than talking to a stranger.
free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
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

# send BUILD CALLER METHOD PATH [BODY]: one request to BUILD as CALLER, the status in $status and
# the body in $work/body. A person (alice, bob, carol) sends their session cookie and the build's
# origin, as a browser does; any other caller sends the header $work/auth/BUILD-CALLER holds, so no
# argument list carries a bearer.
send() {
  local build=$1 caller=$2 method=$3 path=$4 body=${5:-} url=${build_url[$1]}
  local -a credential data=()
  case "$caller" in
    alice | bob | carol) credential=(-b "$work/cookies/$build-$caller" -H "Origin: $url") ;;
    *) credential=(-H "@$work/auth/$build-$caller") ;;
  esac
  [ -z "$body" ] || data=(-H 'Content-Type: application/json' --data-binary "$body")
  status=$(curl -sS -o "$work/body" -w '%{http_code}' --max-time "$call_seconds" -X "$method" \
    "${credential[@]}" "${data[@]}" "$url$path" 2>"$work/curl") || status="curl failed: $(<"$work/curl")"
}

# expect_status CODE WHAT: the last answer is CODE.
expect_status() {
  [ "$status" = "$1" ] || fail "$2: HTTP $status, want $1: $(head -c 600 "$work/body")"
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
#   keyword_ms, meaning_ms, total_ms
#                 the timing summary {count, p50, p95, max, over_300ms} of the related-item checks
#                 the run made (GET /api/v1/related-checks)
#   since         the start of a window that ends at the read, an RFC 3339 instant
#                 (GET /api/v1/agents/{session_id}/answers); and, as a decimal event id, the
#                 cursor the live socket's events_start frame names, which is the event log's
#                 head, so it counts every event of the server, a hidden one's included
readonly durations='["took_ms","age_seconds"]'
readonly timings='["keyword_ms","meaning_ms","total_ms"]'
readonly window_starts='["since"]'
readonly event_cursors='["since"]'

# mask BUILD FILE [EXTRA] prints FILE with every id BUILD's seed recorded replaced by <LABEL>, each
# `LABEL<tab>VALUE` line of the file EXTRA likewise, every RFC 3339 instant the run wrote (between
# run_epoch and now) by <time>, and every duration by <duration>, and is otherwise the body byte
# for byte: jq reads it as one raw string and writes it back as it was. Nothing else is masked:
# keys, slugs, names, versions and event numbers are each seed's own and are compared as they are.
mask() {
  local build=$1 file=$2 extra=${3:-/dev/null}
  jq -Rrj --slurp \
    --rawfile ids "$work/ids-$build.tsv" \
    --rawfile extra "$extra" \
    --argjson durations "$durations" \
    --argjson timings "$timings" \
    --argjson window_starts "$window_starts" \
    --argjson event_cursors "$event_cursors" \
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
      | reduce $event_cursors[] as $member (.; gsub("\"\($member)\":\"[0-9]+\""; "\"\($member)\":\"<event id>\""))' "$file"
}

# keep_answer NAME BUILD [EXTRA] keeps the last answer, masked as BUILD's (mask), as
# $work/answers/NAME.status and NAME.body, for same_answers.
keep_answer() {
  printf '%s' "$status" >"$work/answers/$1.status"
  mask "$2" "$work/body" "${3:-}" >"$work/answers/$1.body"
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

compared=0
differed=0
# same_answers WHAT LEFT RIGHT: the answers kept as LEFT and RIGHT (keep_answer) are the same
# status and the same bytes. A pair that differs is written to $work/differences under WHAT, with
# both answers around the first byte that differs; no_difference reads that file.
same_answers() {
  local what=$1 left=$work/answers/$2 right=$work/answers/$3 at
  compared=$((compared + 1))
  printf '%s\n' "$(<"$right.status")" >>"$work/statuses"
  if [ "$(<"$left.status")" != "$(<"$right.status")" ]; then
    differed=$((differed + 1))
    printf '%s: %s answered HTTP %s, %s HTTP %s\n   %s: %s\n   %s: %s\n' "$what" "$2" "$(<"$left.status")" \
      "$3" "$(<"$right.status")" "$2" "$(head -c 400 "$left.body")" "$3" "$(head -c 400 "$right.body")" >>"$work/differences"
  elif at=$(first_difference "$left.body" "$right.body"); then
    differed=$((differed + 1))
    printf '%s (HTTP %s): the answers differ at byte %s\n   %s: %s\n   %s: %s\n' "$what" "$(<"$right.status")" "$at" \
      "$2" "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$left.body" | head -c 240)" \
      "$3" "$(tail -c +"$((at > 120 ? at - 120 : 1))" "$right.body" | head -c 240)" >>"$work/differences"
  fi
}

# no_difference fails the check when any pair so far differed, printing every one.
no_difference() {
  [ "$differed" -eq 0 ] || fail "$differed of $compared answers differ:
$(<"$work/differences")"
}

# statuses prints how many of the answers compared came back with each status, so a run whose
# every answer is the same refusal reads as that rather than as a pass over the seed.
statuses() { sort "$work/statuses" | uniq -c | awk '{ printf "%s%s %s", sep, $2, $1; sep = ", " }'; }
