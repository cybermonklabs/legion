#!/usr/bin/env bash
# Drives a check that CI proves end to end against a running server, as an agent sees it: a
# `check` block carrying `ci_check`, which names the GitHub check run that proves it. One section
# for each of the checks k1 to k5 of dispatch://INFRA-3204's Verification document, each running
# that check's `run` and asserting its `negative_control`:
#
# - k1: a check with `ci_check` is stored and read back by `dispatch doc-read`; the documents API
#   refuses each of the four writes that break a rule, 400 INVALID_CI_CHECK naming the block and
#   the rule, and stores nothing of them; a `ci_check` on a fresh block id is stored, and the old
#   id used again by a new block is refused again; a check carrying neither `ci_check` nor `run`
#   is stored as it is today; a document socket that declares the old schema version (3) is
#   admitted read-only, and one that declares the new one (4) read-write. The live editor's half
#   of k1, a person pasting the four blocks, is the browser row of e2e/verification.e2e.ts.
# - k2: `dispatch results` derives the result of the check from the named run at the issue's open
#   pull request head: the run's name, conclusion, output and URL, and the pull-request body line,
#   which names the run and no recorder; a failed run prints fail; check_results holds no row for
#   either block; the merged head still answers; three such checks at one head cost one
#   check-runs read; an open head is read before an older merged one; an issue with no such check
#   reads nothing; a summary of 2,500 runes is cut to 2,000 and the answer is not refused.
# - k3: the eleven outcomes of the run, among them the fake GitHub's 500 and a read held past the
#   server's real 10 s deadline, and two runs of one name decided by the higher id in both orders.
# - k4: a tester's and an implementer's result for a check CI proves are refused 409
#   CHECK_PROVED_BY_CI naming the block and the run, and the same call against an ordinary check
#   is recorded and shown.
# - k5: the whole line the reviewer's gate reads, then a failed run, a head moved to a commit the
#   run does not cover, and a head GitHub serves that Dispatch's head record does not hold yet.
#
# Inputs, each required: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an
# agent bearer, read here and never printed), PROJECT (an existing project key), FAKE_GITHUB_PORT
# (the harness's fake GitHub, e2e/fake-github.ts, which the server reads as
# DISPATCH_GITHUB_API_BASE), DATABASE_URL (the server's own database) and DEV_SIGNIN_LOGIN (a
# login the server's dev sign-in route signs in: a document socket admits a person alone).
#
# The harness runs with NATS disabled, so no `pull_request` event ever stores a pull request
# head, and the results read derives a check CI proves only at a head Dispatch holds. So each
# head is staged with psql as such an event would have stored it: one pull_request_heads row
# attributed to the issue, read just now, so the delivery reconcile has no reason to read it
# again. The fake GitHub is given one repository holding the same pull requests and each head's
# check runs, replacing every repository it served. That repository is PROJECT's default where
# PROJECT maps one, since every issue then resolves to it and a recorded result's commit must be
# one it holds, and otherwise one of this run's own, which nothing maps.
#
# It writes nine issues in PROJECT and closes every one of them, also when a check fails; on exit
# it deletes the pull_request_heads rows it staged and empties the fake GitHub. Nothing it writes
# is read by a second run, so it may be run again at once. Each passing check prints
# `PASS: <section> <what>`; the first failing one prints `FAIL: <section> <what>: <observed>` and
# the script exits 1. Every check is `cmd || fail …`, or, for a refusal, `cmd && fail …`, as
# verification.sh writes them.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         FAKE_GITHUB_PORT=9022 DATABASE_URL=postgres://… DEV_SIGNIN_LOGIN=alice ci-check.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the server must
       read the fake GitHub on FAKE_GITHUB_PORT, whose repositories the run replaces, and
       DATABASE_URL must be the server's own database, where the run stages pull request heads)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT FAKE_GITHUB_PORT DATABASE_URL DEV_SIGNIN_LOGIN
[[ "$FAKE_GITHUB_PORT" =~ ^[0-9]+$ ]] || {
  printf 'FAKE_GITHUB_PORT is %s, not a port number\n' "$FAKE_GITHUB_PORT" >&2
  usage
  exit 2
}
readonly fake_base="http://127.0.0.1:$FAKE_GITHUB_PORT"
readonly database_url="$DATABASE_URL"
readonly dev_login="$DEV_SIGNIN_LOGIN"
require_commands bun curl jq psql sha1sum cut
read_bearer

readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
readonly cli_entry="$script_dir/../../../envoy-client/bin/dispatch.ts"
readonly socket_entry="$script_dir/socket-scope.ts"
bun_bin="$(command -v bun)"
readonly bun_bin
readonly saved_path="$PATH"
readonly saved_tmpdir="${TMPDIR:-}"
run_tag="$(date +%s)-$$"
readonly run_tag
readonly session="ci-check-$run_tag"

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The issues the run writes, each cleared from the list once it is closed.
written=()
closed=()
seeded_github=0
driven_repo=""

# close_written ISSUE: closes ISSUE, an issue this script wrote, from cleanup.
close_written() {
  cli issue-update --issue "$1" --status "done" \
    --reason "ci-check.sh stopped before its last check; closing the issue it wrote." \
    >"$work/cleanup" 2>&1 ||
    printf 'ci-check.sh: could not close %s: %s\n' "$1" "$(<"$work/cleanup")" >&2
}
cleanup() {
  local code=$? issue index
  for index in "${!written[@]}"; do
    issue="${written[$index]}"
    [[ "${closed[$index]}" == 1 ]] || close_written "$issue"
  done
  if [[ -n "$driven_repo" && "${#written[@]}" -gt 0 ]]; then
    psql_quiet -v repo="$driven_repo" -v issues="${written[*]}" >"$work/cleanup" 2>&1 <<'SQL' ||
delete from pull_request_heads where repo = :'repo' and issue_key = any(string_to_array(:'issues', ' '));
SQL
      printf 'ci-check.sh: could not delete the pull request heads it staged: %s\n' "$(<"$work/cleanup")" >&2
  fi
  if [[ "$seeded_github" == 1 ]]; then
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary '{}' \
      "$fake_base/__fixture/repos" 2>"$work/curl" ||
      printf 'ci-check.sh: could not empty the fake GitHub: %s\n' "$(<"$work/curl")" >&2
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# clean_env: in a subshell, leaves only what this driver names in the environment, so nothing a
# calling shell exported (its Dispatch URL, token file, Envoy config or DATABASE_URL) reaches a
# child.
clean_env() {
  local name
  local -a exported
  mapfile -t exported < <(compgen -e)
  for name in "${exported[@]}"; do
    unset "$name" 2>/dev/null || true
  done
  export PATH="$saved_path" HOME="$work/home"
  if [[ -n "$saved_tmpdir" ]]; then
    export TMPDIR="$saved_tmpdir"
  fi
}

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names.
cli() {
  (
    set +x
    clean_env
    export DISPATCH_STATE_DIR="$work/state" DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session"
    export DISPATCH_URL="$base" DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}

q() { jq -rn --arg v "$1" '$v|@uri'; }

# send METHOD PATH [JSON]: sends METHOD PATH with the bearer in $work/auth. The answer's body goes
# to $work/body and its HTTP status to $status.
send() {
  local -a data=()
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS -X "$1" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}
get() { send GET "$1"; }
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# expect SECTION WHAT WANT JQ-ARGS...: jq -cS JQ-ARGS over the last answer's body prints WANT.
expect() {
  local section="$1" what="$2" want="$3" got
  shift 3
  got="$(jq -cS "$@" "$work/body" 2>&1)" || fail "$section" "$what" "jq: $got; body: $(body)"
  [[ "$got" == "$want" ]] || fail "$section" "$what" "got $got, want $want"
  pass "$section" "$what"
}

# psql_quiet ARGS...: runs the SQL on stdin against DATABASE_URL with the psql variables ARGS
# names (-v name=value), printing its rows unaligned. libpq would connect to PGHOSTADDR in place
# of the URL's host, so it is left out, as e2e/psql.ts leaves it.
psql_quiet() {
  env -u PGHOSTADDR psql "$database_url" -X -q -A -t -v ON_ERROR_STOP=1 "$@"
}

# sql SECTION WHAT ARGS...: psql_quiet, its rows in $work/sql; a failure fails.
sql() {
  local section="$1" what="$2"
  shift 2
  psql_quiet "$@" >"$work/sql" 2>&1 || fail "$section" "$what" "psql: $(<"$work/sql")"
}

# sha_of LABEL: a commit of this run's own, the same for one LABEL within a run and new each run.
sha_of() { printf '%s %s' "$run_tag" "$1" | sha1sum | cut -c1-40; }

# --- the fake GitHub ----------------------------------------------------------------------------

# The one repository the fake serves, as e2e/fake-github.ts takes it: installed, its pull requests
# by number, its check runs and their faults by commit, and the bare commits a recorded result
# names. fake rewrites it with a jq filter and gives it to the fake whole, which also starts the
# fake's count of check-runs reads again from none (GET /__fixture/requests).
printf '%s\n' '{"contents": "read", "pull_requests": "read", "installation_id": 256, "pulls": {}, "check_runs": {}, "check_run_faults": {}, "commits": []}' \
  >"$work/fixture.json"

# fake SECTION WHAT FILTER JQ-ARGS...: applies FILTER to the repository and seeds the fake with it.
fake() {
  local section="$1" what="$2" filter="$3" code
  shift 3
  jq -c "$@" "$filter" "$work/fixture.json" >"$work/fixture.next" 2>&1 ||
    fail "$section" "$what" "jq: $(<"$work/fixture.next")"
  mv "$work/fixture.next" "$work/fixture.json"
  jq -c --arg r "$driven_repo" '{($r): .}' "$work/fixture.json" >"$work/fixture.put"
  code="$(curl -sS -o "$work/fake-body" -w '%{http_code}' -X PUT \
    -H 'Content-Type: application/json' --data-binary "@$work/fixture.put" \
    "$fake_base/__fixture/repos" 2>"$work/curl")" || fail "$section" "$what" "$(<"$work/curl")"
  [[ "$code" == 2?? ]] || fail "$section" "$what" "the fake GitHub answered HTTP $code: $(<"$work/fake-body")"
  seeded_github=1
}

# check_run ID NAME CONCLUSION [STATUS]: one check run of the run 40 as the fake takes it, its
# output naming its job and its conclusion. A STATUS other than completed leaves it unconcluded.
check_run() {
  jq -cn --argjson id "$1" --arg name "$2" --arg conclusion "$3" --arg status "${4:-completed}" \
    --arg repo "$driven_repo" \
    '{id: $id, name: $name, check_suite_id: 41, status: $status,
      conclusion: (if $status == "completed" then $conclusion else null end),
      completed_at: (if $status == "completed" then "2026-11-12T10:00:00Z" else null end),
      html_url: "https://github.com/\($repo)/actions/runs/40/job/\($id)",
      output: {title: "\($name) \($conclusion)", summary: "The \($name) job concluded \($conclusion)."}}'
}

# runs_at SECTION WHAT SHA RUNS: the check runs of SHA are the JSON array RUNS, read at once.
runs_at() {
  fake "$1" "$2" '.check_runs[$sha] = $runs | del(.check_run_faults[$sha])' \
    --arg sha "$3" --argjson runs "$4"
}

# fault_at SECTION WHAT SHA FAULT: the check-runs read of SHA does FAULT in place of answering.
fault_at() {
  fake "$1" "$2" '.check_run_faults[$sha] = $fault' --arg sha "$3" --argjson fault "$4"
}

# fake_pull SECTION ISSUE NUMBER SHA STATE: the fake's pull request NUMBER of ISSUE, headed at SHA.
fake_pull() {
  fake "$1" "pull request $3 of $2 at $4" \
    '.pulls[$n] = {head_sha: $sha, state: (if $state == "open" then "open" else "closed" end),
                   merged: ($state == "merged"), title: "\($issue) the ci-check acceptance run"}' \
    --arg n "$3" --arg sha "$4" --arg state "$5" --arg issue "$2"
}

# stage_head SECTION ISSUE NUMBER SHA STATE: pull request NUMBER of ISSUE is STATE (open or
# merged) at SHA, in the fake and in Dispatch's own head record, as a `pull_request` event would
# have stored it. attribution_rule is delivery.AttributionRuleVersion, so the reconcile has no
# merged row to judge again either; were it to, it reads the same pull request from the fake.
stage_head() {
  fake_pull "$@"
  sql "$1" "stage pull request $3 of $2" -v repo="$driven_repo" -v number="$3" -v sha="$4" \
    -v state="$5" -v issue="$2" -v title="$2 the ci-check acceptance run" <<'SQL'
insert into pull_request_heads
  (repo, number, head_sha, state, issue_key, attribution_source, title, attribution_rule, checked_at)
values (:'repo', :'number', :'sha', :'state', :'issue', 'title_body', :'title', 2, now())
on conflict (repo, number) do update set
  head_sha = excluded.head_sha, state = excluded.state, issue_key = excluded.issue_key,
  title = excluded.title, attribution_rule = excluded.attribution_rule, checked_at = excluded.checked_at;
SQL
  pass "$1" "staged pull request $3 of $2, $5 at $4"
}

# check_run_reads SECTION WHAT: the fake's count of check-runs reads since its last seed, in
# $work/body.
check_run_reads() {
  curl -sS -o "$work/body" "$fake_base/__fixture/requests" 2>"$work/curl" ||
    fail "$1" "$2" "$(<"$work/curl")"
}

# --- Dispatch ----------------------------------------------------------------------------------

printf 'The ci-check acceptance run of a check CI proves.\n' >"$work/spec.md"

# issue SECTION TITLE: creates an issue; sets $key to its key and records it for cleanup.
issue() {
  run "$1" "create \"$2\"" issue --project "$project" --title "$2" --spec-file "$work/spec.md" --force
  [[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
    fail "$1" "create \"$2\"" "no issue key in: $(<"$work/out")"
  key="${BASH_REMATCH[1]}"
  written+=("$key")
  closed+=(0)
  pass "$1" "created $key in $project"
}

# close_issue SECTION ISSUE: closes ISSUE and marks it closed for cleanup.
close_issue() {
  local index
  run "$1" "close $2" issue-update --issue "$2" --status "done" \
    --reason "The ci-check acceptance run closes the issue it wrote."
  for index in "${!written[@]}"; do
    [[ "${written[$index]}" != "$2" ]] || closed[index]=1
  done
  pass "$1" "closed $2"
}

# upload SECTION ISSUE FILE: uploads FILE as ISSUE's verification.md through the CLI.
upload() {
  run "$1" "upload verification.md to $2" artifact --issue "$2" --name verification.md \
    --path "$3" --force
  pass "$1" "uploaded verification.md to $2"
}

# document_body FILE: the JSON an artifact POST takes for FILE as verification.md.
document_body() {
  jq -cn --arg a "$session" --rawfile c "$1" \
    '{actor: {kind: "session", id: $a}, name: "verification.md", content: $c}'
}

# artifact_id SECTION ISSUE: sets $artifact to the id of ISSUE's verification.md.
artifact_id() {
  get "/api/v1/issues/$2/artifacts"
  want_status 200 "$1" "read $2's documents"
  artifact="$(jq -er 'map(select(.name == "verification.md")) | .[0].id' "$work/body")" ||
    fail "$1" "find $2's verification.md" "$(body)"
}

# version_of SECTION ISSUE: sets $version to the current version of ISSUE's verification.md.
version_of() {
  get "/api/v1/issues/$2/artifacts/verification-md/text"
  want_status 200 "$1" "read $2's verification.md version"
  version="$(jq -er '.version' "$work/body")" || fail "$1" "read $2's verification.md version" "$(body)"
}

# check_block ID CI_CHECK WORDS: a check CI proves, as markdown writes it.
check_block() {
  printf ':::check{#%s surface="cli" ci_check="%s" negative_control="Break the %s job and its run fails."}\n%s\n:::\n\n' \
    "$1" "$2" "$2" "$3"
}

# ci_issue SECTION TITLE [BLOCK...]: creates an issue whose verification.md holds the BLOCKs, or,
# with none, the one check c-ci that the run `contracts` proves; sets $key and $artifact.
readonly ci_words="The contracts job passes at the head."
ci_issue() {
  local section="$1" title="$2"
  shift 2
  issue "$section" "$title"
  {
    printf 'How %s is proved.\n\n' "$key"
    if [[ $# -eq 0 ]]; then
      check_block c-ci contracts "$ci_words"
    else
      printf '%s\n' "$@"
    fi
  } >"$work/verification.md"
  upload "$section" "$key" "$work/verification.md"
  artifact_id "$section" "$key"
}

# results SECTION ISSUE: `dispatch results --issue ISSUE`, its output in $work/out.
results() { run "$1" "read the results of $2" results --issue "$2"; }

# line SECTION WHAT LINE: the last output holds LINE, whole.
line() {
  grep -qxF -- "$3" "$work/out" || fail "$1" "$2" "no line \"$3\" in: $(<"$work/out")"
  pass "$1" "$2"
}

# no_line SECTION WHAT TEXT: no line of the last output holds TEXT.
no_line() {
  grep -qF -- "$3" "$work/out" && fail "$1" "$2" "a line holds \"$3\": $(<"$work/out")"
  pass "$1" "$2"
}

# derived SECTION WHAT ID VERDICT SHA CASE NUMBER WORDS [RUN]: the last output holds the derived
# result of check ID, VERDICT at SHA by ci from RUN (contracts) concluded CASE, at the head of pull
# request NUMBER, and its pull-request body line, which names the run and no recorder.
derived() {
  local run_name="${9:-contracts}"
  line "$1" "$2" \
    "- $3 (before_merge): $4 at $5 by ci (CI run $run_name, $6), that commit is the head of pull request #$7. $8"
  line "$1" "$3's pull-request body line names CI run $run_name" \
    "  Pull request line: - Check $3 (before_merge): $4 at $5, from CI run $run_name. $8"
}

# unconcluded SECTION WHAT ID CASE DETAIL WORDS: the last output says check ID, proved by the run
# contracts, has no result for CASE, in DETAIL, and prints neither a pass nor a fail for it.
unconcluded() {
  line "$1" "$2" "- $3 (before_merge): no result: $5 $6"
  line "$1" "$3's pull-request body line says its run has no result ($4)" \
    "  Pull request line: - Check $3 (before_merge): CI run contracts has no result ($4)"
  no_line "$1" "$3 prints neither a pass nor a fail" "- $3 (before_merge): pass at"
  no_line "$1" "$3 prints no fail either" "- $3 (before_merge): fail at"
}

# no_rows SECTION WHAT ARTIFACT BLOCK: check_results holds no row for block BLOCK of ARTIFACT.
no_rows() {
  sql "$1" "$2" -v artifact="$3" -v block="$4" <<'SQL'
select count(*) from check_results where check_artifact_id = :'artifact' and check_block_id = :'block';
SQL
  [[ "$(<"$work/sql")" == 0 ]] || fail "$1" "$2" "check_results holds $(<"$work/sql") rows for $4"
  pass "$1" "$2"
}

# --- setup ---------------------------------------------------------------------------------------

# PROJECT's default repository, under the name it is called today, where it maps one: every issue
# of the project resolves to it, so a recorded result's commit must be one it holds, and the fake
# serves it. Where PROJECT maps none, the run drives a repository of its own, which nothing maps.
get "/api/v1/settings/repo-projects"
want_status 200 setup "the repository mappings answer"
default_repo="$(jq -r --arg p "$project" \
  'first(.[] | select(.project == $p and .default) | (.renamed_to // .repo)) // ""' "$work/body")" ||
  fail setup "read the repository mappings" "$(body)"
driven_repo="${default_repo:-acme/ci-check-$run_tag}"
readonly driven_repo
fake setup "seed the fake GitHub with $driven_repo" '.'
pass setup "seeded the fake GitHub with $driven_repo, replacing every repository it served"

# --- k1: the attribute, the four refusals, the fresh id and the two sockets ----------------------

issue k1 "ci_check is stored, and the four rules refuse"
readonly k1="$key"
held_commit="$(sha_of k1-held)"
plain_commit="$(sha_of k4-plain)"
readonly held_commit plain_commit
fake k1 "the commits the recorded results name" '.commits += [$held, $plain]' \
  --arg held "$held_commit" --arg plain "$plain_commit"

readonly k1_control="Delete the check and the requirement reads uncovered."
readonly k1_contracts=":::check{#c-contracts surface=\"cli\" ci_check=\"contracts\" negative_control=\"Break a contract and the contracts job fails.\"}
$ci_words
:::"
readonly k1_plain=":::check{#c-plain surface=\"cli\" negative_control=\"$k1_control\"}
A check that names neither a CI run nor a command.
:::"
readonly k1_held=":::check{#c-held surface=\"cli\" negative_control=\"$k1_control\"}
A check a tester records a result for.
:::"
readonly k1_fresh=":::check{#c-fresh surface=\"cli\" ci_check=\"contracts\" negative_control=\"$k1_control\"}
A check CI proves, under an id no block held before.
:::"

# k1_document FILE HELD [EXTRA]: k1's Verification document, c-held written as HELD, EXTRA after.
k1_document() {
  printf 'How %s is proved.\n\n%s\n\n%s\n\n%s\n' "$k1" "$k1_contracts" "$k1_plain" "$2" >"$1"
  [[ $# -lt 3 ]] || printf '\n%s\n' "$3" >>"$1"
}

k1_document "$work/k1.md" "$k1_held"
upload k1 "$k1" "$work/k1.md"
artifact_id k1 "$k1"
readonly k1_artifact="$artifact"
run k1 "read verification.md back" doc-read --issue "$k1" --artifact verification.md
line k1 "dispatch doc-read reads c-contracts back with its ci_check" \
  ':::check{#c-contracts surface="cli" when="before_merge" negative_control="Break a contract and the contracts job fails." ci_check="contracts"}'
line k1 "a check carrying neither ci_check nor run is stored as it is today" \
  ":::check{#c-plain surface=\"cli\" when=\"before_merge\" negative_control=\"$k1_control\"}"

run k1 "record a tester's pass for c-held" result --issue "$k1" --check c-held --role tester \
  --verdict pass --commit "$held_commit" --surface cli --run "bash e2e/acceptance/ci-check.sh" \
  --observed "the ci-check acceptance run recorded it"
pass k1 "recorded a tester's pass for c-held at $held_commit"
version_of k1 "$k1"
readonly k1_version="$version"

# refused_write WHAT FILE BLOCK RULE: posting FILE as k1's verification.md is refused 400
# INVALID_CI_CHECK, naming BLOCK and RULE.
refused_write() {
  local message
  send POST "/api/v1/issues/$k1/artifacts" "$(document_body "$2")"
  want_status 400 k1 "$1"
  expect k1 "$1 is refused INVALID_CI_CHECK" '"INVALID_CI_CHECK"' '.code'
  message="$(jq -er '.error' "$work/body")" || fail k1 "$1 names $3" "no error in: $(body)"
  [[ "$message" == *"check $3 (ci_check \"contracts\"): $4"* ]] ||
    fail k1 "$1 names $3 and its rule" "the refusal reads $message"
  pass k1 "$1 names $3 and the rule: $4"
}

k1_document "$work/k1-run.md" "$k1_held" ":::check{#c-run surface=\"cli\" ci_check=\"contracts\" run=\"bun test\" negative_control=\"$k1_control\"}
A check CI proves that names a command.
:::"
refused_write "a check carrying ci_check and run" "$work/k1-run.md" c-run \
  "a check proved by CI carries no run"
k1_document "$work/k1-when.md" "$k1_held" ":::check{#c-when surface=\"cli\" ci_check=\"contracts\" when=\"after_merge\" negative_control=\"$k1_control\"}
A check CI proves after the merge.
:::"
refused_write "a check carrying ci_check with when of after_merge" "$work/k1-when.md" c-when \
  "ci_check takes when of before_merge"
k1_document "$work/k1-control.md" "$k1_held" ":::check{#c-control surface=\"cli\" ci_check=\"contracts\"}
A check CI proves with no negative control.
:::"
refused_write "a check carrying ci_check and no negative_control" "$work/k1-control.md" c-control \
  "a check proved by CI names its negative control"
k1_document "$work/k1-held.md" ":::check{#c-held surface=\"cli\" ci_check=\"contracts\" negative_control=\"$k1_control\"}
A check a tester records a result for.
:::"
refused_write "ci_check added to c-held, which holds a recorded result" "$work/k1-held.md" c-held \
  "this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one"
version_of k1 "$k1"
[[ "$version" == "$k1_version" ]] ||
  fail k1 "the four refused writes store nothing" "verification.md is at version $version, was $k1_version"
pass k1 "the four refused writes store nothing: verification.md is still at version $k1_version"

k1_document "$work/k1-fresh.md" "$k1_held" "$k1_fresh"
send POST "/api/v1/issues/$k1/artifacts" "$(document_body "$work/k1-fresh.md")"
[[ "$status" == 2?? ]] || fail k1 "ci_check on the fresh id c-fresh is accepted" "HTTP $status: $(body)"
pass k1 "ci_check on the fresh id c-fresh is accepted, HTTP $status"
run k1 "read verification.md back" doc-read --issue "$k1" --artifact verification.md
line k1 "dispatch doc-read reads c-fresh back with its ci_check" \
  ":::check{#c-fresh surface=\"cli\" when=\"before_merge\" negative_control=\"$k1_control\" ci_check=\"contracts\"}"
k1_document "$work/k1-reused.md" ":::check{#c-held surface=\"cli\" ci_check=\"contracts\" negative_control=\"$k1_control\"}
A new check written under the id of one that holds a result.
:::" "$k1_fresh"
refused_write "a new block under the old id c-held, carrying ci_check" "$work/k1-reused.md" c-held \
  "this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one"

# The sockets: the scope Hocuspocus's authentication answers, as the browser's provider reads it.
get "/api/v1/schema/blocks"
want_status 200 k1 "the block schema answers"
expect k1 "the block schema is version 4" 4 '.version'
curl -sS -o /dev/null -D "$work/signin" "$base/auth/_dev/signin?login=$(q "$dev_login")" \
  2>"$work/curl" || fail k1 "sign $dev_login in" "$(<"$work/curl")"
sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' "$work/signin" | paste -sd ';' >"$work/cookie"
[[ -s "$work/cookie" && "$(<"$work/cookie")" == *=* ]] ||
  fail k1 "sign $dev_login in" "the dev sign-in set no cookie"
pass k1 "signed $dev_login in at the dev sign-in route"

# socket_scope VERSION: the scope a document socket on k1's verification.md declaring VERSION is
# admitted with, in $scope.
socket_scope() {
  scope="$(
    clean_env
    exec "$bun_bin" "$socket_entry" "$base" "$k1_artifact" "$1" "$work/cookie" 2>"$work/socket"
  )" || fail k1 "open a document socket at schema version $1" "$(<"$work/socket")"
}
socket_scope 3
[[ "$scope" == readonly ]] || fail k1 "the socket on schema version 3 is read-only" "its scope is $scope"
pass k1 "the socket on schema version 3 is admitted with scope readonly"
socket_scope 4
[[ "$scope" == read-write ]] || fail k1 "the socket on schema version 4 writes" "its scope is $scope"
pass k1 "the socket on schema version 4 is admitted with scope read-write"

# --- k2: the derived result, the merge, one read for three checks, the newest head --------------

ci_issue k2 "a check CI proves, at an open pull request"
readonly k2_open="$key" k2_open_artifact="$artifact"
open_sha="$(sha_of k2-open)"
runs_at k2 "a contracts run that concluded success" "$open_sha" \
  "[$(check_run 9001 contracts success)]"
stage_head k2 "$k2_open" 101 "$open_sha" open
results k2 "$k2_open"
derived k2 "c-ci prints the pass of CI run contracts at the open head" c-ci pass "$open_sha" \
  success 101 "$ci_words"
line k2 "the result's reading opens with the run's conclusion" "  Reading: conclusion: success"
line k2 "the reading carries the run's output title" "contracts success"
line k2 "the reading carries the run's output summary" "The contracts job concluded success."
line k2 "the reading carries the run's URL" "https://github.com/$driven_repo/actions/runs/40/job/9001"
get "/api/v1/issues/$k2_open/results"
want_status 200 k2 "GET /results answers"
expect k2 "the one result is the derived one: its id, role, producer, run and commit" \
  "$(jq -cn --arg sha "$open_sha" '[["ci-run:9001","ci","github_app","github-actions","contracts",$sha]]')" \
  '[.checks[] | select(.block_id == "c-ci") | .results[] | [.id, .role, .actor.kind, .actor.id, .run, .commit_sha]]'
no_rows k2 "check_results holds no row for $k2_open's c-ci" "$k2_open_artifact" c-ci

ci_issue k2 "a check CI proves, whose run failed"
readonly k2_fail="$key" k2_fail_artifact="$artifact"
fail_sha="$(sha_of k2-fail)"
runs_at k2 "a contracts run that concluded failure" "$fail_sha" \
  "[$(check_run 9002 contracts failure)]"
stage_head k2 "$k2_fail" 102 "$fail_sha" open
results k2 "$k2_fail"
derived k2 "c-ci prints fail at the head for a run that concluded failure" c-ci fail "$fail_sha" \
  failure 102 "$ci_words"
no_rows k2 "check_results holds no row for $k2_fail's c-ci" "$k2_fail_artifact" c-ci

stage_head k2 "$k2_open" 101 "$open_sha" merged
results k2 "$k2_open"
derived k2 "the merged pull request's head still answers with the pass" c-ci pass "$open_sha" \
  success 101 "$ci_words"

ci_issue k2 "three checks CI proves, at one head" \
  "$(check_block c-contracts contracts "The contracts job passes at the head.")" \
  "$(check_block c-lint lint "The lint job passes at the head.")" \
  "$(check_block c-e2e e2e "The e2e job passes at the head.")"
readonly k2_three="$key"
three_sha="$(sha_of k2-three)"
stage_head k2 "$k2_three" 103 "$three_sha" open
runs_at k2 "three runs at one head" "$three_sha" \
  "[$(check_run 9011 contracts success), $(check_run 9012 lint success), $(check_run 9013 e2e success)]"
results k2 "$k2_three"
derived k2 "c-contracts prints its pass" c-contracts pass "$three_sha" success 103 \
  "The contracts job passes at the head." contracts
derived k2 "c-lint prints its pass" c-lint pass "$three_sha" success 103 \
  "The lint job passes at the head." lint
derived k2 "c-e2e prints its pass" c-e2e pass "$three_sha" success 103 \
  "The e2e job passes at the head." e2e
check_run_reads k2 "count the check-runs reads of one answer"
expect k2 "the three-check answer read the head's check runs once, not three times" \
  "$(jq -cn --arg r "$driven_repo" --arg sha "$three_sha" '{($r): {($sha): 1}}')" '.check_runs'

ci_issue k2 "a check CI proves, with an open and an older merged pull request"
readonly k2_both="$key"
merged_sha="$(sha_of k2-merged)"
newer_sha="$(sha_of k2-newer)"
stage_head k2 "$k2_both" 150 "$merged_sha" merged
stage_head k2 "$k2_both" 204 "$newer_sha" open
runs_at k2 "a failed run at the merged head" "$merged_sha" "[$(check_run 9150 contracts failure)]"
runs_at k2 "a passing run at the open head" "$newer_sha" "[$(check_run 9204 contracts success)]"
results k2 "$k2_both"
derived k2 "the open pull request's head answers, not the older merged one's" c-ci pass \
  "$newer_sha" success 204 "$ci_words"
[[ "$(grep -c -- ' by ci (CI run ' "$work/out")" == 1 ]] ||
  fail k2 "the issue prints one derived result" "$(<"$work/out")"
pass k2 "the issue prints one derived result"
no_line k2 "no derived result names the merged head" "at $merged_sha by ci"

issue k2 "no check CI proves"
readonly k2_none="$key"
printf 'How %s is proved.\n\n%s\n' "$k2_none" "$k1_plain" >"$work/verification.md"
upload k2 "$k2_none" "$work/verification.md"
none_sha="$(sha_of k2-none)"
runs_at k2 "a contracts run at the head" "$none_sha" "[$(check_run 9005 contracts success)]"
stage_head k2 "$k2_none" 105 "$none_sha" open
results k2 "$k2_none"
check_run_reads k2 "count the check-runs reads of the answer"
expect k2 "an issue holding no ci_check check reads no check runs at all" '{}' '.check_runs'

ci_issue k2 "a check CI proves, whose run wrote a long summary"
readonly k2_long="$key"
long_sha="$(sha_of k2-long)"
runs_at k2 "a passing run whose summary is 2,500 runes" "$long_sha" \
  "$(check_run 9006 contracts success | jq -c '[.output = {title: null, summary: ([range(2500)] | map("é") | add)}]')"
stage_head k2 "$k2_long" 106 "$long_sha" open
results k2 "$k2_long"
derived k2 "a run with a long summary still answers its pass" c-ci pass "$long_sha" success 106 \
  "$ci_words"
get "/api/v1/issues/$k2_long/results"
want_status 200 k2 "GET /results answers with the long summary"
expect k2 "the reading holds the run's summary cut to 2,000 runes, between its conclusion and its URL" \
  "$(jq -cn --arg url "https://github.com/$driven_repo/actions/runs/40/job/9006" '["conclusion: success",2000,$url]')" \
  '[.checks[] | select(.block_id == "c-ci") | .results[0].observed | split("\n") | .[0], (.[1] | length), .[2]]'

# --- k3: every outcome of the named run ----------------------------------------------------------

ci_issue k3 "a check CI proves, through every outcome of its run"
readonly k3="$key"
k3_sha="$(sha_of k3)"
stage_head k3 "$k3" 301 "$k3_sha" open
readonly k3_at="$k3_sha, the head of $driven_repo#301"

runs_at k3 "a run concluded success" "$k3_sha" "[$(check_run 9101 contracts success)]"
results k3 "$k3"
derived k3 "success prints a pass, so the answers that follow come from a reader that works" \
  c-ci pass "$k3_sha" success 301 "$ci_words"

for conclusion in neutral skipped cancelled; do
  runs_at k3 "a run concluded $conclusion" "$k3_sha" "[$(check_run 9102 contracts "$conclusion")]"
  results k3 "$k3"
  unconcluded k3 "$conclusion prints no result, naming its case" c-ci "$conclusion" \
    "CI run contracts concluded $conclusion at $k3_at, which proves nothing." "$ci_words"
done

runs_at k3 "a run still in progress" "$k3_sha" "[$(check_run 9105 contracts "" in_progress)]"
results k3 "$k3"
unconcluded k3 "a run not completed prints no result, naming its case" c-ci not_completed \
  "CI run contracts is in_progress at $k3_at, and has not concluded." "$ci_words"

runs_at k3 "no run of that name at the head" "$k3_sha" "[$(check_run 9106 lint success)]"
results k3 "$k3"
unconcluded k3 "a run absent from the head prints no result, naming its case" c-ci absent \
  "No CI run named contracts is on $k3_at." "$ci_words"

runs_at k3 "a run only on page two of 101 runs" "$k3_sha" "$(
  jq -cn --arg repo "$driven_repo" --argjson last "$(check_run 9107 contracts success)" \
    '[range(1; 101) | {id: ., name: "job-\(.)", check_suite_id: 41, status: "completed",
       conclusion: "success", completed_at: "2026-11-12T10:00:00Z"}] + [$last]'
)"
results k3 "$k3"
derived k3 "a run only on page two prints its pass, since the read pages" c-ci pass "$k3_sha" \
  success 301 "$ci_words"
check_run_reads k3 "count the check-runs reads of the paged answer"
expect k3 "the answer read both pages of the head's check runs" \
  "$(jq -cn --arg r "$driven_repo" --arg sha "$k3_sha" '{($r): {($sha): 2}}')" '.check_runs'

runs_at k3 "a run concluded timed_out" "$k3_sha" "[$(check_run 9108 contracts timed_out)]"
results k3 "$k3"
derived k3 "timed_out prints a fail" c-ci fail "$k3_sha" timed_out 301 "$ci_words"

runs_at k3 "a run concluded superseded, which R3 does not name" "$k3_sha" \
  "[$(check_run 9109 contracts superseded)]"
results k3 "$k3"
derived k3 "a conclusion R3 does not name prints a fail, naming it as GitHub wrote it" c-ci fail \
  "$k3_sha" superseded 301 "$ci_words"

# unread SECTION WHAT WORDS: the last output says GitHub could not be read for c-ci at k3's head,
# with WORDS from what GitHub answered, and prints neither a pass nor a fail.
unread() {
  local said
  said="$(grep -F -- "- c-ci (before_merge): no result: GitHub could not be read for the check runs of $driven_repo at $k3_sha: " "$work/out")" ||
    fail "$1" "$2" "no line saying GitHub could not be read: $(<"$work/out")"
  [[ "$said" == *"$3"* ]] || fail "$1" "$2" "the line does not carry \"$3\": $said"
  pass "$1" "$2: $said"
  line "$1" "c-ci's pull-request body line says its run has no result (github_unreadable)" \
    "  Pull request line: - Check c-ci (before_merge): CI run contracts has no result (github_unreadable)"
  no_line "$1" "c-ci prints neither a pass nor a fail" " by ci (CI run "
}

runs_at k3 "a passing run" "$k3_sha" "[$(check_run 9110 contracts success)]"
fault_at k3 "the check-runs read answers 500" "$k3_sha" '{"status": 500}'
results k3 "$k3"
unread k3 "the 500 prints that GitHub could not be read, with what it answered" "500"

fault_at k3 "the check-runs read is held 12 s" "$k3_sha" '{"hold_ms": 12000}'
started="$SECONDS"
results k3 "$k3"
waited=$((SECONDS - started))
[[ "$waited" -ge 10 ]] || fail k3 "the held read waits the 10 s deadline" "the answer came in $waited s"
pass k3 "the held read answered after $waited s, the server's own 10 s deadline"
unread k3 "the read held past the deadline prints that GitHub could not be read" "no answer within 10s"

runs_at k3 "two runs of contracts: the lower id failed, the higher passed" "$k3_sha" \
  "[$(check_run 9120 contracts failure), $(check_run 9121 contracts success)]"
results k3 "$k3"
derived k3 "the run of the higher id decides: a pass, where the first listed failed" c-ci pass \
  "$k3_sha" success 301 "$ci_words"
runs_at k3 "the same two runs, the conclusions exchanged" "$k3_sha" \
  "[$(check_run 9120 contracts success), $(check_run 9121 contracts failure)]"
results k3 "$k3"
derived k3 "the run of the higher id decides: a fail, where the first listed passed" c-ci fail \
  "$k3_sha" failure 301 "$ci_words"

# --- k4: no role records a result for a check CI proves ------------------------------------------

readonly proved_refusal='check c-fresh is proved by CI run "contracts": dispatch results reads its result from that run, and no role records one'
for role in tester implementer; do
  cli result --issue "$k1" --check c-fresh --role "$role" --verdict pass --commit "$plain_commit" \
    >"$work/out" 2>&1 && fail k4 "the $role's result for c-fresh is refused" "it was recorded: $(<"$work/out")"
  line k4 "the $role's result for c-fresh is refused CHECK_PROVED_BY_CI, naming the block and the run" \
    "CHECK_PROVED_BY_CI: $proved_refusal"
done
no_rows k4 "nothing is stored for c-fresh" "$k1_artifact" c-fresh
run k4 "record a tester's pass for c-plain" result --issue "$k1" --check c-plain --role tester \
  --verdict pass --commit "$plain_commit" --surface cli --run "bash e2e/acceptance/ci-check.sh" \
  --observed "the ci-check acceptance run recorded it"
pass k4 "the same call against c-plain, which carries no ci_check, is recorded"
results k4 "$k1"
grep -A3 -F -- "- c-plain (before_merge). " "$work/out" |
  grep -qE "^  - pass by tester at ${plain_commit}\. " ||
  fail k4 "dispatch results shows the tester's pass under c-plain" "$(<"$work/out")"
pass k4 "dispatch results shows the tester's pass under c-plain at $plain_commit"

# --- k5: the line the reviewer's gate reads ------------------------------------------------------

ci_issue k5 "the one before_merge check carries ci_check"
readonly k5="$key"
first_sha="$(sha_of k5-first)"
moved_sha="$(sha_of k5-moved)"
served_sha="$(sha_of k5-served)"
runs_at k5 "a run concluded success at the head" "$first_sha" "[$(check_run 9401 contracts success)]"
stage_head k5 "$k5" 401 "$first_sha" open
results k5 "$k5"
derived k5 "the whole line: pass, at the head's commit, by ci, at the head of pull request 401" \
  c-ci pass "$first_sha" success 401 "$ci_words"

runs_at k5 "the run concluded failure" "$first_sha" "[$(check_run 9401 contracts failure)]"
results k5 "$k5"
derived k5 "the failed run prints fail at the head" c-ci fail "$first_sha" failure 401 "$ci_words"

runs_at k5 "the run concluded success again" "$first_sha" "[$(check_run 9401 contracts success)]"
stage_head k5 "$k5" 401 "$moved_sha" open
results k5 "$k5"
unconcluded k5 "the head moved to a commit the run does not cover prints no result" c-ci absent \
  "No CI run named contracts is on $moved_sha, the head of $driven_repo#401." "$ci_words"

runs_at k5 "the run concluded success at the moved head" "$moved_sha" \
  "[$(check_run 9402 contracts success)]"
runs_at k5 "a run concluded success at a newer head" "$served_sha" \
  "[$(check_run 9403 contracts success)]"
fake_pull k5 "$k5" 401 "$served_sha" open
curl -sS -o "$work/body" "$fake_base/repos/$driven_repo/pulls/401" 2>"$work/curl" ||
  fail k5 "read pull request 401 from the fake GitHub" "$(<"$work/curl")"
expect k5 "GitHub serves pull request 401 at the newer head" "\"$served_sha\"" '.head.sha'
results k5 "$k5"
derived k5 "the derived result stands at the commit Dispatch holds, not the head GitHub serves" \
  c-ci pass "$moved_sha" success 401 "$ci_words"
no_line k5 "no result names the head GitHub serves, so no passing result is current there" \
  "$served_sha"

# --- close: the issues the run wrote -------------------------------------------------------------

for written_issue in "${written[@]}"; do
  close_issue close "$written_issue"
done
