#!/usr/bin/env bash
# Drives the one statement in Dispatch that removes bytes (dispatch://INFRA-1149's c-erasure).
# It writes a document, has a person approve a version of it, and writes a comment holding a
# secret that version's markdown holds too. It then runs `envoy-dispatch erase` against that
# comment's node and proves, in order: the command refuses until it has listed what it would
# remove, and that list holds the version and the review the erase would end; after the erase a
# read of the node's address answers what a deleted node answers, byte for byte, no tuple and no
# projection row of it remains, the `artifact_versions` row holding the erased text is gone and
# the `artifact_reviews` row whose digest covered those bytes is gone, reported by its issue and
# its version; the `graph_erasures` row holds what was removed, who ran it and when, and holds no
# erased text; every node left carries a supertag of the closed list the store itself holds, so
# the erase wrote no node of its own; `envoy-dispatch census --store` reads zero differences; and
# the command's path over HTTP answers 404 to the deployment's agent bearer and to a person's
# cookie alike, because no route serves it.
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
# shellcheck disable=SC2153 # PROJECT and DATABASE_URL are the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$DATABASE_URL"
readonly login="${DEV_SIGNIN_LOGIN:-alice}"
session="graph-erase-$(date +%s)-$$"
readonly session
# The bytes this run erases. Nothing but this run writes them, so a row that still holds them
# after the erase is a row the erase missed.
secret="passphrase-$(date +%s)-$$-hunter2"
readonly secret

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
want_rows E3 "the refused run wrote nothing" 1 \
  "select count(*) from comments where id = '$comment'"
want_rows E3 "the refused run recorded no erasure" 0 \
  "select count(*) from graph_erasures where node_id = '$comment'"

# The answer an address nobody holds gives, read before the erase, so the comparison after it is
# against this server's own words and not against a status this driver chose.
missing="00000000-0000-4000-8000-000000000000"
send GET "/api/v1/references?to=$(q "dispatch://$key/comment/$missing")"
readonly nobody_status="$status"
cp "$work/body" "$work/nobody"
pass E3 "an address nobody holds answers HTTP $nobody_status"

# E4: the erase itself.
erase E4 --node "$node" --confirm
[[ "$erase_code" == 0 ]] ||
  fail E4 "erase $node" "exited $erase_code: $(<"$work/erase")"
grep -Fq "removed by" "$work/erase" || fail E4 "erase $node" "$(<"$work/erase")"
pass E4 "envoy-dispatch erase --node $node --confirm removed it"

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
want_rows E5 "no tuple of it remains" 0 \
  "select count(*) from graph_tuples where (from_kind = 'comment' and from_id = '$comment') or (to_kind = 'comment' and to_id = '$comment')"
want_rows E5 "no projection row of it remains" 0 \
  "select count(*) from comments where id = '$comment'"
want_rows E5 "the version holding the erased text is gone" 0 \
  "select count(*) from artifact_versions where artifact_id = '$document' and number = $version"
want_rows E5 "the review over those bytes is gone ($key, $document_ref version $version)" 0 \
  "select count(*) from artifact_reviews where artifact_id = '$document' and version = $version"
want_rows E5 "no row anywhere still holds the erased text" 0 \
  "select (select count(*) from comments where body like '%$secret%') + (select count(*) from artifact_versions where markdown like '%$secret%')"

# E6: the record of the erase.
query E6 "read the graph_erasures row" \
  "select jsonb_build_object('node_kind', node_kind, 'node_id', node_id, 'removed', removed, 'ran_by', ran_by, 'ran_at', ran_at)::text from graph_erasures where node_id = '$comment'"
[[ -n "$answer" ]] || fail E6 "read the graph_erasures row" "the erase recorded nothing"
printf '%s' "$answer" >"$work/erasure.json"
jq -e --arg id "$comment" '.node_kind == "comment" and .node_id == $id' "$work/erasure.json" >/dev/null ||
  fail E6 "the record names the node" "$(<"$work/erasure.json")"
jq -e '(.removed.rows | length) > 0 and (.removed.versions | length) > 0 and (.removed.reviews | length) > 0' \
  "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds what was removed" "$(<"$work/erasure.json")"
jq -e '(.ran_by.database_user | length) > 0 and (.ran_at | length) > 0' "$work/erasure.json" >/dev/null ||
  fail E6 "the record holds who ran it and when" "$(<"$work/erasure.json")"
pass E6 "the graph_erasures row holds what was removed, who ran it and when"
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
grep -Fqx 'census: store ok' "$work/census" ||
  fail E8 "the store census after the erase" "$(tail -n 3 "$work/census")"
pass E8 "envoy-dispatch census --store reads zero differences"

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
