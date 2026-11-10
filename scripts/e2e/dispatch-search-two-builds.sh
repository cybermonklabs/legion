#!/usr/bin/env bash
# The live proof that a narrowed search asks an older build of Dispatch for something it cannot
# serve, rather than being answered unnarrowed. Two real `envoy-dispatch` binaries run against one
# PostgreSQL — the one built from the commit `--base-commit` names, which must be the release
# before the five narrowing members were added (legion-envoy-v29.47.1), and the one built from
# this checkout — the way a rolling release serves both at once. A search carrying
# ?number_project=, ?status=, ?author=, ?updated_since= or ?keywords_only= goes to
# GET /api/v1/search/results, which the older build has no route for and answers 404, where the
# same search on GET /api/v1/search is answered with the member dropped.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/dispatch-search-two-builds.sh --base-commit legion-envoy-v29.47.1
#
# DATABASE_URL is required and must name a loopback host: the run drops and recreates its `public`
# schema, so the base build migrates it from empty and this build migrates it forward, as a
# deployment's upgrade does. The database must already hold the `vector` extension, which meaning
# search needs. --base-commit is required and names the commit the base build is built from, which
# for this proof is the release before pull request #2809 added the five members
# (legion-envoy-v29.47.1); its tree is taken with `git archive` into the run's own work directory,
# so the checkout is never touched. The ports are SEARCH_TWO_BUILDS_BASE_PORT (19081),
# SEARCH_TWO_BUILDS_HEAD_PORT (19082) and SEARCH_TWO_BUILDS_ENVOY_PORT (19083), each refused when
# something already answers on it, so this proof and dispatch-two-builds.sh beside it can run at
# once. The work directory, under TMPDIR, holds the base tree, both binaries and every process's
# log: removed when the run passes, kept and printed when it fails.
#
# It is a sibling of dispatch-two-builds.sh rather than a case added to it: that proof needs a
# base build older than this release's reference grammar, and this one needs the release before
# the five members, so one --base-commit cannot serve both.
# The harness the two run on — the argument, the prerequisites, the build of both binaries, the
# reset, the fake Envoy and the start of each server — is scripts/e2e/lib/two-builds.sh, which
# both source; what is below is this proof's own seed and cases.
#
# Both builds run the dev embedder (DISPATCH_DEV_EMBEDDER=1), which hashes a text's lower-case
# words into the buckets of one vector in the server's own process and calls no model service, so
# the meaning leg of search runs here as it does in a deployment. The run waits on
# GET /api/v1/search/coverage until every seeded item carries a vector before it reads anything.
#
# The corpus is seeded through this build's API: two projects whose next issue number is the same
# 1042, so one bare number names an issue of each, and in the first project three issues sharing
# the rare word "astrolabe" that a filter can tell apart — a different status, a different author
# and a different write time — beside two issues sharing the word "quasar".
#
# What each member is read by, in the answer the route gives:
#
#   number_project   which issue leads the page. The member narrows no rows at all; it decides
#                    which project a bare issue number names, so with it the project's own 1042
#                    leads, and without it both projects' 1042 lead in project key order.
#   status           the rows left, which is the one issue in `in_progress` and its spec.
#   author           the rows left, which is the one issue one session wrote and its spec.
#   updated_since    the rows left, which is the one issue written after the mark and its spec.
#   keywords_only    the rows left. It filters no row in SQL, so this corpus is built to separate
#                    the two legs instead: one issue holds both words of the two-word query and
#                    the keyword leg (websearch_to_tsquery, an AND of the words) finds it, while
#                    the other holds one of them and little else, which only the meaning leg
#                    reaches. Measured against these binaries, the full search answers both and
#                    keywords_only=true answers the first alone. This is the signal because it is
#                    the one thing the answer exposes that the member changes: the member leaves
#                    the answer's own filters echo untouched, since it is no filter.
#
# The four cases, made for each of the five members in that order:
#
#   case 1   GET /api/v1/search carrying the member, sent to the base build, answers 200 with the
#            answer that build's own unnarrowed search gives. That is the hazard this proof
#            exists to record, not a failure of the base build: it never saw the member.
#   case 2   GET /api/v1/search/results carrying it, sent to the base build, answers 404
#            NOT_FOUND "no route for GET /api/v1/search/results".
#   case 3   the same request to this build answers 200, narrowed.
#   case 4   GET /api/v1/search carrying it, sent to this build, answers 200, narrowed the same
#            way: the old page of an earlier release is still served correctly.
#
# The negative control is cases 2 and 3 together: with the GET /api/v1/search/results row of
# packages/envoy/internal/dispatch/api/routes_table.go commented out, neither build serves the
# path, so case 3 can no longer pass. Run that way once, this build does not start at all — the
# route table's shape map (routes_shapes.go) still names the path, and a shape naming a route the
# table does not mount panics at boot — so the run fails at `head-build`. With that shape
# commented out beside it, this build boots and the run fails at `number_project` case 3, with
# this build answering the same 404 NOT_FOUND "no route for GET /api/v1/search/results" the base
# build answers. Either way nothing serves the path and the run does not pass.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh, which lib/two-builds.sh sources, reads of its caller: start_process and
# await_start log under $evidence/logs.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/two-builds.sh
. "$root/scripts/e2e/lib/two-builds.sh"

# This proof's own ports, and a longer wait on one call than the shared default: a search of
# this corpus runs the meaning leg too. two_builds_setup reads this script's --base-commit and
# DATABASE_URL before it drops every DISPATCH_*, ENVOY_*, NATS_* and PGHOSTADDR the shell
# inherited, and arms the teardown, whose last line carries the label given here.
base_port=${SEARCH_TWO_BUILDS_BASE_PORT:-19081}
head_port=${SEARCH_TWO_BUILDS_HEAD_PORT:-19082}
envoy_port=${SEARCH_TWO_BUILDS_ENVOY_PORT:-19083}
call_seconds=30
two_builds_setup 'dispatch search two builds e2e' "$@"

# fetch URL reads one GET as an agent bearer and sets answer_status and answer_body, whatever the
# status: a case of this proof judges the status itself, and 404 is the answer two of them want.
answer_status=''
answer_body=''
fetch() {
  local out
  out=$(mktemp "$work/answer.XXXXXX")
  answer_status=$(curl -sS -o "$out" -w '%{http_code}' --max-time 30 \
    -H "Authorization: Bearer $agent_token" "$1") || answer_status=000
  answer_body=$(cat "$out")
  rm -f "$out"
}

# signal ANSWER prints one search answer as the one line this proof compares: each result as its
# kind and the key of the issue it belongs to, in the order the answer gives them, joined with
# "; ". Both what a member leaves in the answer and which row leads it are in that line, which is
# what the five members are read by (see the header). An empty answer prints an empty line.
signal() {
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  printf '%s' "$1" | bun -e 'let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { const answer = JSON.parse(data); console.log((answer.results ?? []).map((result) => `${result.kind} ${result.owner?.key ?? "-"}`).join("; ")); });'
}

# new_issue PROJECT TITLE SPEC SESSION writes one issue through $target_url as that session and
# prints its key. The spec is given rather than left to default, so every word the search reads of
# the issue is this script's own.
new_issue() {
  local project=$1 title=$2 spec=$3 session=$4
  call POST /api/v1/issues \
    "{\"project\":\"$project\",\"title\":\"$title\",\"spec\":\"$spec\",\"actor\":{\"kind\":\"session\",\"id\":\"$session\"}}" |
    json_field key
}

# every_item_embedded succeeds when the coverage report says no searchable item lacks a vector,
# and keeps the reading it took for the timeout hook. The queue embeds in the background, so a
# search run before this holds answers by words alone and the keywords_only case has nothing to
# tell apart.
coverage_reading=''
every_item_embedded() {
  local body
  body=$(curl -sS --max-time 30 -H "Authorization: Bearer $agent_token" \
    "$head_url/api/v1/search/coverage") || return 1
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  coverage_reading=$(printf '%s' "$body" | bun -e 'let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { const report = JSON.parse(data); const kinds = Object.entries(report.kinds ?? {}).map(([kind, counts]) => `${kind} ${counts.lacking}/${counts.items}`).join(", "); console.log(`lacking=${report.total.lacking} items=${report.total.items} (${kinds})`); });') || return 1
  [ "${coverage_reading%% *}" = lacking=0 ]
}
report_coverage() { note "the last coverage reading: ${coverage_reading:-none taken}"; }

# prove_member MEMBER QUERY PLAIN NARROWED makes this proof's four cases for one member: QUERY is
# the whole query string carrying the member, PLAIN the same query without it, and NARROWED the
# answer's signal this build must give for the member to have been read. The control after case 4
# is this build's own unnarrowed answer, which must differ from NARROWED: without that, a member
# this build ignored would pass every case.
prove_member() {
  local member=$1 query=$2 plain=$3 narrowed=$4
  local base_plain base_member head_results head_search head_plain code error

  fetch "$base_url/api/v1/search?$plain"
  [ "$answer_status" = 200 ] ||
    fail "the base build answered the unnarrowed search $answer_status: $answer_body"
  base_plain=$(signal "$answer_body") ||
    fail "could not read the base build's unnarrowed answer: $answer_body"
  fetch "$base_url/api/v1/search?$query"
  [ "$answer_status" = 200 ] ||
    fail "the base build answered GET /api/v1/search carrying $member $answer_status: $answer_body"
  base_member=$(signal "$answer_body") ||
    fail "could not read the base build's answer carrying $member: $answer_body"
  [ "$base_member" = "$base_plain" ] ||
    fail "the base build read $member after all: carrying it, it answered [$base_member], where its unnarrowed answer is [$base_plain]"
  [ "$base_member" != "$narrowed" ] ||
    fail "the base build answered $member narrowed, so this corpus tells the two builds apart on nothing: [$base_member]"
  note "case 1 GET /api/v1/search?$query on the base build: 200, [$base_member], its own unnarrowed answer"

  fetch "$base_url/api/v1/search/results?$query"
  [ "$answer_status" = 404 ] ||
    fail "the base build answered GET /api/v1/search/results carrying $member $answer_status, not 404: $answer_body"
  code=$(printf '%s' "$answer_body" | json_field code) ||
    fail "the base build's 404 for $member carries no code: $answer_body"
  error=$(printf '%s' "$answer_body" | json_field error) ||
    fail "the base build's 404 for $member carries no error: $answer_body"
  [ "$code" = NOT_FOUND ] ||
    fail "the base build's 404 for $member is code $code, not NOT_FOUND: $answer_body"
  [ "$error" = "no route for GET /api/v1/search/results" ] ||
    fail "the base build's 404 for $member says \"$error\", not \"no route for GET /api/v1/search/results\""
  note "case 2 GET /api/v1/search/results?$query on the base build: 404 $code \"$error\""

  fetch "$head_url/api/v1/search/results?$query"
  [ "$answer_status" = 200 ] ||
    fail "this build answered GET /api/v1/search/results carrying $member $answer_status: $answer_body"
  head_results=$(signal "$answer_body") ||
    fail "could not read this build's /results answer for $member: $answer_body"
  [ "$head_results" = "$narrowed" ] ||
    fail "this build answered GET /api/v1/search/results carrying $member [$head_results], want [$narrowed]"
  note "case 3 GET /api/v1/search/results?$query on this build: 200, [$head_results]"

  fetch "$head_url/api/v1/search?$query"
  [ "$answer_status" = 200 ] ||
    fail "this build answered GET /api/v1/search carrying $member $answer_status: $answer_body"
  head_search=$(signal "$answer_body") ||
    fail "could not read this build's /search answer for $member: $answer_body"
  [ "$head_search" = "$narrowed" ] ||
    fail "this build answered GET /api/v1/search carrying $member [$head_search], want the [$narrowed] it answers on /results"
  note "case 4 GET /api/v1/search?$query on this build: 200, [$head_search], the same answer"

  fetch "$head_url/api/v1/search?$plain"
  [ "$answer_status" = 200 ] ||
    fail "this build answered the unnarrowed search $answer_status: $answer_body"
  head_plain=$(signal "$answer_body") ||
    fail "could not read this build's unnarrowed answer: $answer_body"
  [ "$head_plain" != "$narrowed" ] ||
    fail "this build's unnarrowed answer is already [$head_plain], so $member changed nothing and the cases above prove nothing"
  note "control GET /api/v1/search?$plain on this build: 200, [$head_plain], which $member changed"
}

# The base build has to carry the dev embedder, or its meaning leg runs on nothing and the
# keywords_only case has no second leg to tell apart. It is read from the commit's own tree, and
# it is this proof's own prerequisite, which the shared block runs after the checks the two
# proofs share.
base_build_has_dev_embedder() {
  git -C "$root" show "$base_sha:packages/envoy/cmd/dispatch/embedder.go" 2>/dev/null |
    grep -q DISPATCH_DEV_EMBEDDER ||
    fail "the base build at $base_commit has no dev embedder (DISPATCH_DEV_EMBEDDER in packages/envoy/cmd/dispatch/embedder.go), so meaning search cannot run on both builds"
}

begin prerequisites
two_builds_prerequisites \
  "--base-commit is required: it names the commit the base build is built from, which for this proof is the release before the five narrowing members (legion-envoy-v29.47.1)" \
  base_build_has_dev_embedder
pass

begin build
two_builds_build dispatch-search-two-builds
pass

begin reset-database
two_builds_reset_database
pass

begin fake-envoy
two_builds_fake_envoy
pass

two_builds_identity

begin base-build
# DISPATCH_DEV_EMBEDDER is what the sibling proof does not need: it embeds in this process,
# calls no model service, and needs the DISPATCH_DEV_SIGNIN=1 the shared start_server sets
# beside it.
start_server base "$work/bin/dispatch-base" "$base_port" DISPATCH_DEV_EMBEDDER=1
await_start base "$base_pid" 120 "the base build to answer /healthz" \
  curl -fsS --max-time 2 "$base_url/healthz"
note "the base build serves $base_url on the schema it migrated"
pass

begin head-build
start_server head "$work/bin/dispatch-head" "$head_port" DISPATCH_DEV_EMBEDDER=1
await_start head "$head_pid" 120 "this build to answer /healthz" \
  curl -fsS --max-time 2 "$head_url/healthz"
kill -0 "$base_pid" 2>/dev/null || fail "the base build exited when this build migrated the schema"
note "this build serves $head_url on the schema it migrated forward, beside the base build"
pass

begin seed
# Everything is written through this build, as a rolling release's new pods take the writes, and
# both builds then read one corpus.
target_url=$head_url
actor='"actor":{"kind":"session","id":"alpha"}'
call POST /api/v1/projects "{\"key\":\"SRCH\",\"name\":\"Search\",$actor}" >/dev/null ||
  fail "this build refused the project SRCH"
call POST /api/v1/projects "{\"key\":\"ALFA\",\"name\":\"Alfa\",$actor}" >/dev/null ||
  fail "this build refused the project ALFA"
# Both projects' next issue is 1042, so one bare number names an issue of each and
# number_project is what decides which of them the page meant. One statement moves both, as the
# Go tests of the search bar do.
psql "$database_url" -X -q -v ON_ERROR_STOP=1 \
  -c "update projects set next_number = 1042 where key in ('SRCH', 'ALFA')" >/dev/null ||
  fail "could not set both projects' next issue number"
# The number is in each twin's title as well, so the keyword leg of both builds finds them: the
# base build reads no bare number as an issue key at all, and without the word in the text its
# answer would be empty rather than the unnarrowed rows case 1 exists to record.
twin_key=$(new_issue SRCH "Quadrant mount bracket 1042" "The quadrant mount bracket." alpha) ||
  fail "this build refused the SRCH twin"
other_twin_key=$(new_issue ALFA "Quadrant mount spare 1042" "The quadrant mount spare." alpha) ||
  fail "this build refused the ALFA twin"
[ "$twin_key" = SRCH-1042 ] && [ "$other_twin_key" = ALFA-1042 ] ||
  fail "the twins are $twin_key and $other_twin_key, not SRCH-1042 and ALFA-1042"
# Three issues sharing one rare word, told apart by author, by status and by write time.
plate_key=$(new_issue SRCH "Astrolabe mounting plate" "The astrolabe mounting plate." alpha) ||
  fail "this build refused the astrolabe mounting plate"
ring_key=$(new_issue SRCH "Astrolabe bearing ring" "The astrolabe bearing ring." beta) ||
  fail "this build refused the astrolabe bearing ring"
call PATCH "/api/v1/issues/$ring_key" "{\"status\":\"in_progress\",\"actor\":{\"kind\":\"session\",\"id\":\"beta\"}}" >/dev/null ||
  fail "this build refused the status of $ring_key"
# Two issues sharing one word: the first holds both words of the two-word query, which the
# keyword leg's AND needs, and the second holds that word and nothing else, which only the
# meaning leg reaches. A short text is what puts it there: the dev embedder's vector is the
# text's own words, so one word against a two-word query scores about 0.707, well above
# searchMeaningFloor, where a long text scores far lower.
both_words_key=$(new_issue SRCH "Quasar drift survey" "The quasar drift survey." alpha) ||
  fail "this build refused the quasar drift survey"
one_word_key=$(new_issue SRCH "Quasar" "Quasar." alpha) ||
  fail "this build refused the one-word quasar issue"
# The mark every item written so far is older than, and every item written after it is newer
# than. The seconds either side of it are the margin the filter needs: `date` truncates to the
# second, so a mark taken in the same second as the last write names a moment before that write,
# and the filter reads at or after itself.
sleep 2
since_mark=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 2
vane_key=$(new_issue SRCH "Astrolabe sight vane" "The astrolabe sight vane." beta) ||
  fail "this build refused the astrolabe sight vane"
note "the twins $twin_key and $other_twin_key, the astrolabe issues $plate_key (alpha), $ring_key (beta, in_progress) and $vane_key (written after $since_mark)"
note "the quasar issues $both_words_key (both words) and $one_word_key (one word)"
pass

begin embedded
# The queue embeds what the writes above enqueued in the background. Until it is done a search
# answers by words alone, and the keywords_only case has no meaning leg to take a row away from.
timeout_hook=report_coverage
until_true 180 "every seeded item to carry a vector from the dev embedder" every_item_embedded
timeout_hook=''
note "coverage: $coverage_reading"
pass

begin number_project
# The member narrows no rows: it says which project a bare issue number names, so the signal is
# which of the two 1042s leads the page. Without it both lead, in project key order, so ALFA's
# does; with it SRCH's does. The base build answers the page's own project nothing at all.
prove_member number_project "q=1042&number_project=SRCH" "q=1042" \
  "issue $twin_key; issue $other_twin_key"
pass

begin status
prove_member status "q=astrolabe&status=in_progress" "q=astrolabe" \
  "issue $ring_key; document $ring_key"
pass

begin author
prove_member author "q=astrolabe&author=alpha" "q=astrolabe" \
  "issue $plate_key; document $plate_key"
pass

begin updated_since
prove_member updated_since "q=astrolabe&updated_since=$since_mark" "q=astrolabe" \
  "issue $vane_key; document $vane_key"
pass

begin keywords_only
# The keyword leg is an AND of the query's two words, so it reaches the issue holding both and
# not the one holding one of them; the meaning leg reaches both. keywords_only=true therefore
# takes the second issue and its spec out of the answer, which is what this reads.
prove_member keywords_only "q=quasar%20drift&keywords_only=true" "q=quasar%20drift" \
  "issue $both_words_key; document $both_words_key"
pass
