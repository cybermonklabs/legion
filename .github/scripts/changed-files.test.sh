#!/usr/bin/env bash
# Tests for `changed-files.sh`: the files of a pull request or a merge group, read with a counted
# retry inside the step's budget and written as the repository ci.yaml's `changes` job hands its
# paths-filter steps.
#
# A pull request case runs the script with a stand-in `gh` first on PATH. It answers the files
# route with two pages of fixture rows, back to back as `gh api --paginate` prints them, and given
# `--jq <filter>` it parses each page with jq and fails on one it cannot parse, as gh does. It
# records every call, and told to, it answers its first k calls badly: a 502, the answer
# paths-filter met on pull request #4449, a hang (ignoring TERM, or not), a 200 whose second page
# is cut off mid-row, a 200 that lists no file, or GitHub's refusal for its rate limit (alone, or
# followed by a hang that ignores TERM). It answers gh-read.sh's GET /rate_limit, which the ladder
# reads after such a refusal, apart: by hanging, or by saying no budget is spent. A case counts the
# attempts the script made.
#
# A merge group case builds an origin repository holding the base and the head, and checks out the
# head the way the case names: a plain clone of the head alone, or what actions/checkout makes with
# `sparse-checkout:` - a blob:none partial clone - at depth 1 or at the job's `fetch-depth: 2`. A
# stand-in `git` first on PATH counts each `git fetch` the script runs and can refuse the first k
# before handing the call to the real git; one case points origin at a listener that accepts and
# never answers. Each case reads the written repository the way paths-filter reads it with
# `base: HEAD`: `git diff --no-renames --name-status -z HEAD`.
#
# Run from anywhere: .github/scripts/changed-files.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
listener=""
trap '[ -z "$listener" ] || kill "$listener" 2> /dev/null; rm -rf "$work"' EXIT
mkdir "$work/bin"
real_git=$(command -v git)
# A 60 s budget with no wait between attempts: the read is bounded at 50 s, and each attempt at
# (60 - 10) / 3 - 10 = 6 s. The hanging case waits it out once, and the fetches from a local
# origin, which take milliseconds, stay well inside it on a loaded runner.
export GH_RETRY_SECONDS=0 GITHUB_REPOSITORY=o/r STANDIN_DIR="$work"
BUDGET=60

# Two pages of the files route, 11 rows. The rows carry every status the API answers with, and two
# pairs of paths that trade a file for a directory (docs/old becomes a directory, lib/x/y becomes
# the file lib/x), which a repository holds only because a removal goes into the index before an
# addition.
cat > "$work/pages.json" <<'JSON'
[{"filename": "src/app.ts", "status": "modified"},
 {"filename": "src/new.ts", "status": "added"},
 {"filename": "src/gone.ts", "status": "removed"},
 {"filename": "pkg/after.go", "status": "renamed", "previous_filename": "pkg/before.go"}]
[{"filename": "copy/of.txt", "status": "copied", "previous_filename": "copy/source.txt"},
 {"filename": "bin/run.sh", "status": "changed"},
 {"filename": "docs/old", "status": "removed"},
 {"filename": "docs/old/index.md", "status": "added"},
 {"filename": "lib/x/y", "status": "removed"},
 {"filename": "lib/x", "status": "added"},
 {"filename": "name with spaces.md", "status": "added"}]
JSON
# The same 200, its second page cut off mid-row.
cat > "$work/truncated.json" <<'JSON'
[{"filename": "src/app.ts", "status": "modified"},
 {"filename": "src/new.ts", "status": "added"},
 {"filename": "src/gone.ts", "status": "removed"},
 {"filename": "pkg/after.go", "status": "renamed", "previous_filename": "pkg/before.go"}]
[{"filename": "copy/of.txt", "status": "cop
JSON
echo '[]' > "$work/empty.json"
# What paths-filter's own read makes of those rows (src/main.ts, getChangedFilesFromApi): every
# filename, and a renamed row's previous_filename beside it. Sorted, as `diff_paths` prints.
expected_paths=$(printf '%s\n' 'bin/run.sh' 'copy/of.txt' 'docs/old' 'docs/old/index.md' 'lib/x' 'lib/x/y' \
  'name with spaces.md' 'pkg/after.go' 'pkg/before.go' 'src/app.ts' 'src/gone.ts' 'src/new.ts')

cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
# gh-read.sh's lookup of when a spent budget resets, kept out of the calls a case counts. It prints
# what gh-read.sh's --jq makes of the answer: the reset of the budget at 0, or 0 when none is.
if [ "${2:-}" = rate_limit ]; then
  printf '%s\n' "$*" >> "$STANDIN_DIR/rate-limit-calls"
  case ${GH_RATE_LIMIT:-back} in
    hang) exec sleep 120 > /dev/null 2>&1 ;;
    # The core budget spent until that many seconds from now.
    spent-for:*) echo $(($(date +%s) + ${GH_RATE_LIMIT#spent-for:})) ;;
    # No budget at 0: the window turned over.
    *) echo 0 ;;
  esac
  exit 0
fi
printf '%s\n' "$*" >> "$STANDIN_DIR/gh-calls"
calls=$(wc -l < "$STANDIN_DIR/gh-calls")
body="$STANDIN_DIR/pages.json"
if [ "$calls" -le "${GH_REFUSE_FIRST:-0}" ]; then
  case ${GH_REFUSAL:-502} in
    502) echo "gh: Bad Gateway (HTTP 502)" >&2; exit 1 ;;
    # The answer paths-filter printed in run 44595619799; that log holds no HTTP status, and the
    # script retries a refusal whatever its status.
    load) echo "gh: Server Error: Sorry, this diff is temporarily unavailable due to heavy server load." >&2; exit 1 ;;
    # 120 s: past every case's bound, and short enough that a child a broken script leaves ends.
    # Its output goes nowhere, so a child left behind holds no pipe of the case's open.
    hang) exec sleep 120 > /dev/null 2>&1 ;;
    # A read that ignores TERM, so only the KILL `timeout -k` sends 10 s after its bound ends it.
    hang-ignoring-term) trap '' TERM; exec sleep 120 > /dev/null 2>&1 ;;
    truncated) body="$STANDIN_DIR/truncated.json" ;;
    empty) body="$STANDIN_DIR/empty.json" ;;
    # GitHub's refusal for the hourly API budget, as gh prints it, which gh-read.sh answers with a
    # read of GET /rate_limit.
    rate-limit) echo "gh: API rate limit exceeded for installation ID 1. (HTTP 403)" >&2; exit 1 ;;
    # The same refusal, and then a read that ignores TERM.
    rate-limit-hang) echo "gh: API rate limit exceeded for installation ID 1. (HTTP 403)" >&2; trap '' TERM; exec sleep 120 > /dev/null 2>&1 ;;
  esac
fi
# `--jq <filter>`: each page parsed and filtered, and a page that does not parse fails the call,
# as gh does; without it the body comes back as it was sent, whole or not.
filter=""
while [ "$#" -gt 0 ]; do
  [ "$1" = --jq ] && filter=$2
  shift
done
if [ -n "$filter" ]; then
  jq -c "$filter" "$body" || { echo "gh: the response is not valid JSON" >&2; exit 1; }
else
  cat "$body"
fi
GH
chmod +x "$work/bin/gh"

cat > "$work/bin/git" <<GIT
#!/usr/bin/env bash
if [ "\${1:-}" = fetch ]; then
  printf '%s\n' "\$*" >> "\$STANDIN_DIR/git-fetches"
  if [ "\$(wc -l < "\$STANDIN_DIR/git-fetches")" -le "\${GIT_REFUSE_FIRST:-0}" ]; then
    echo "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 502" >&2
    exit 128
  fi
fi
exec "$real_git" "\$@"
GIT
chmod +x "$work/bin/git"

# list_files <event> <directory> [VAR=value...]: runs the script with --budget $BUDGET under a
# `timeout` of CASE_SECONDS (the budget and 15 s by default) over $work/event.json, writing the
# repository to <directory>; sets out, err and status, and leaves the stand-ins' call logs in
# gh_calls, rate_limit_calls and fetches. The script runs without JOB_TIMEOUT_MINUTES and
# STEP_TIMEOUT_MINUTES, as in the changes job, which sets neither: the test job that runs this
# harness sets the first for gh-read.sh's wait, and with it gh-read.sh would stop the ladder at
# its first rate-limit refusal, before the bound over the read that the rate-limit cases test. A
# VAR=value sets them back for a case that wants them.
list_files() {
  local event_name=$1 dest=$2
  shift 2
  rm -f "$work/gh-calls" "$work/rate-limit-calls" "$work/git-fetches"
  case_marker="changed-files-case-$$-$dest"
  set +e
  out=$(env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES -u GITHUB_SHA CHANGED_FILES_CASE="$case_marker" PATH="$work/bin:$PATH" \
    GITHUB_EVENT_NAME="$event_name" GITHUB_EVENT_PATH="$work/event.json" "$@" \
    timeout "${CASE_SECONDS:-$((BUDGET + 15))}" "$script_dir/changed-files.sh" --budget "$BUDGET" "$dest" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
  gh_calls=$(cat "$work/gh-calls" 2> /dev/null || true)
  rate_limit_calls=$(cat "$work/rate-limit-calls" 2> /dev/null || true)
  fetches=$(cat "$work/git-fetches" 2> /dev/null || true)
}

# under_worker <directory> [VAR=value...]: list_files for a pull request, with the case's bound at
# the budget and 5 s, run as a child of a parent process named Runner.Worker: a copy of bash by
# that name, as GitHub's runner starts each job's steps. gh-read.sh measures JOB_TIMEOUT_MINUTES
# from that process's start and STEP_TIMEOUT_MINUTES from its child's, the case's `timeout`. On a
# runner a step starts well after its job; a stand-in that started its child at once would start
# the two within the same second, and a gh-read.sh that measured the step's limit from the job's
# start would read the same deadline. So the stand-in waits JOB_LEAD seconds before it starts the
# step, more than the 2 s either side the cases allow a deadline. Sets out, err and status;
# job_started, the clock just before the stand-in starts; and started, the clock just before the
# step starts, which the stand-in writes to step-started.
readonly JOB_LEAD=6
worker="$work/worker/Runner.Worker"
mkdir "$work/worker"
cp "$(command -v bash)" "$worker"
under_worker() {
  local dest=$1
  shift
  rm -f "$work/gh-calls" "$work/rate-limit-calls" "$work/git-fetches" "$work/step-started"
  case_marker="changed-files-case-$$-$dest"
  job_started=$(date +%s)
  set +e
  # shellcheck disable=SC2016 # the worker's own shell expands "$1", "$2", "$@" and "$?", not this one
  out=$(env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES -u GITHUB_SHA CHANGED_FILES_CASE="$case_marker" PATH="$work/bin:$PATH" \
    GITHUB_EVENT_NAME=pull_request GITHUB_EVENT_PATH="$work/event.json" "$@" \
    "$worker" -c 'sleep "$1" && date +%s > "$2" && shift 2 && "$@"; exit "$?"' worker "$JOB_LEAD" "$work/step-started" \
    timeout "$((BUDGET + 5))" "$script_dir/changed-files.sh" --budget "$BUDGET" "$dest" 2> "$work/err")
  status=$?
  set -e
  started=$(cat "$work/step-started" 2> /dev/null || echo 0)
  err=$(cat "$work/err")
  gh_calls=$(cat "$work/gh-calls" 2> /dev/null || true)
  rate_limit_calls=$(cat "$work/rate-limit-calls" 2> /dev/null || true)
}

# left_behind: how many processes the last case started still run. Every process the script
# starts inherits the case's marker in its environment, so this counts what any part of the read
# left behind: a stand-in, an attempt's `timeout`, gh-read.sh's lookup or its own `sleep`.
left_behind() {
  local found
  found=$(grep -lsazx -- "CHANGED_FILES_CASE=$case_marker" /proc/[0-9]*/environ 2> /dev/null || true)
  call_count "$found"
}

# diff_paths <directory>: the paths paths-filter reads there with `base: HEAD`, one a line, sorted.
diff_paths() {
  "$real_git" -C "$1" diff --no-renames --name-status -z HEAD | tr '\0' '\n' | awk 'NR % 2 == 0' | LC_ALL=C sort
}

# diff_status <directory> <path>: the status that diff gives the path.
diff_status() {
  "$real_git" -C "$1" diff --no-renames --name-status -z HEAD | tr '\0' '\n' |
    awk -v want="$2" 'NR % 2 == 1 { status = $0; next } $0 == want { print status }'
}

call_count() { if [ -z "$1" ]; then echo 0; else wc -l <<<"$1" | tr -d ' '; fi; }

# pr_event <changed_files>: the payload of pull request #7, which counts that many changed files.
pr_event() {
  jq -n --argjson n "$1" '{action: "synchronize", number: 7, pull_request: {number: 7, changed_files: $n}}' > "$work/event.json"
}
pr_event 11

echo "case: a pull request whose files read at the first attempt"
list_files pull_request "$work/pr"
check "it answers" "$(is "$status" 0)"
check "it read the files route once, every page of it, parsed by gh" \
  "$(is "$gh_calls" "api --paginate repos/o/r/pulls/7/files?per_page=100 --jq .[]")"
check "the repository's diff names exactly the paths paths-filter's own read names" \
  "$(is "$(diff_paths "$work/pr")" "$expected_paths")"
check "an added file is added" "$(is "$(diff_status "$work/pr" src/new.ts)" A)"
check "a removed file is deleted" "$(is "$(diff_status "$work/pr" src/gone.ts)" D)"
check "a modified file is modified" "$(is "$(diff_status "$work/pr" src/app.ts)" M)"
check "a renamed file is its new name added" "$(is "$(diff_status "$work/pr" pkg/after.go)" A)"
check "  and its old name deleted" "$(is "$(diff_status "$work/pr" pkg/before.go)" D)"
check "a file that became a directory is deleted" "$(is "$(diff_status "$work/pr" docs/old)" D)"
check "  and the file under it added" "$(is "$(diff_status "$work/pr" docs/old/index.md)" A)"
check "a directory that became a file is its file deleted" "$(is "$(diff_status "$work/pr" lib/x/y)" D)"
check "  and the new file added" "$(is "$(diff_status "$work/pr" lib/x)" A)"
check "it says how many path lines it read, and from where" \
  "$(is "$out" "12 path(s) changed: pull request #7, read from GET repos/o/r/pulls/7/files")"

echo "case: GitHub answers 502 to the first two reads and then the files"
list_files pull_request "$work/pr-502" GH_REFUSE_FIRST=2
check "it answers" "$(is "$status" 0)"
check "after two retries: three reads" "$(is "$(call_count "$gh_calls")" 3)"
check "with the same paths" "$(is "$(diff_paths "$work/pr-502")" "$expected_paths")"
check "each refusal is in the log" "$(is "$(grep -c 'HTTP 502' <<<"$err")" 2)"

echo "case: GitHub answers 'this diff is temporarily unavailable' once and then the files"
list_files pull_request "$work/pr-load" GH_REFUSE_FIRST=1 GH_REFUSAL=load
check "it answers" "$(is "$status" 0)"
check "after one retry: two reads" "$(is "$(call_count "$gh_calls")" 2)"
check "with the same paths" "$(is "$(diff_paths "$work/pr-load")" "$expected_paths")"

echo "case: a 200 whose second page is cut off mid-row, once, and then the files"
list_files pull_request "$work/pr-cut" GH_REFUSE_FIRST=1 GH_REFUSAL=truncated
check "it answers" "$(is "$status" 0)"
check "the cut-off answer is a failed attempt: two reads" "$(is "$(call_count "$gh_calls")" 2)"
check "with the same paths" "$(is "$(diff_paths "$work/pr-cut")" "$expected_paths")"

echo "case: every 200 is cut off"
list_files pull_request "$work/pr-cut-down" GH_REFUSE_FIRST=99 GH_REFUSAL=truncated
check "it fails" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$gh_calls")" 3)"
check "naming the read" "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files failed 3 attempts, ")"
check "it writes no repository" "$(is "$(test -e "$work/pr-cut-down" && echo yes || echo no)" no)"

echo "case: a 200 that lists no file, once, for a pull request of 11 files, and then the files"
list_files pull_request "$work/pr-short" GH_REFUSE_FIRST=1 GH_REFUSAL=empty
check "it answers" "$(is "$status" 0)"
check "the short list is a failed attempt: two reads" "$(is "$(call_count "$gh_calls")" 2)"
check "with the same paths" "$(is "$(diff_paths "$work/pr-short")" "$expected_paths")"
check "the log names the short count" \
  "$(contains "$err" "^GET repos/o/r/pulls/7/files listed 0 file(s), and the event says pull request #7 changes 11\$")"

echo "case: every list is short of the count the event names"
pr_event 13
list_files pull_request "$work/pr-short-down"
check "it fails" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$gh_calls")" 3)"
check "naming the read and the count" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: .*short of the 13 files the event counts")"
check "it writes no repository" "$(is "$(test -e "$work/pr-short-down" && echo yes || echo no)" no)"

echo "case: a pull request of more files than the API lists, in a checkout that is not its merge commit"
# Above 3,000 files the script reads the merge commit's diff instead of the API (the merge group
# section below holds the case where it can); here GITHUB_SHA names no commit of the checkout.
pr_event 3500
list_files pull_request "$work/pr-huge" GITHUB_SHA=0000000000000000000000000000000000000001
check "it fails" "$(is "$status" 1)"
check "naming the count, the cap, the merge commit it wants and what to do" \
  "$(contains "$err" "^::error::pull request #7 changes 3500 files, more than the 3000 GET repos/o/r/pulls/7/files lists, and the checkout's HEAD (.*) is not its merge commit GITHUB_SHA (0000000000000000000000000000000000000001) with two parents, whose diff against its first parent would name them all; split it into pull requests of at most 3000 files each, which the API lists whole\$")"
check "having read nothing" "$(is "$gh_calls" "")"

echo "case: a payload with no changed_files"
jq -n '{action: "synchronize", number: 7, pull_request: {number: 7}}' > "$work/event.json"
list_files pull_request "$work/pr-uncounted"
check "it fails" "$(is "$status" 1)"
check "naming what is missing" "$(contains "$err" "^::error::the pull_request payload carries no .pull_request.changed_files")"
check "having read nothing" "$(is "$gh_calls" "")"
pr_event 11

echo "case: a read that hangs is ended at its bound and counts as one refused attempt"
list_files pull_request "$work/pr-hang" GH_REFUSE_FIRST=1 GH_REFUSAL=hang
check "it answers" "$(is "$status" 0)"
check "two reads" "$(is "$(call_count "$gh_calls")" 2)"
check "with the same paths" "$(is "$(diff_paths "$work/pr-hang")" "$expected_paths")"

echo "case: GitHub never answers the files"
list_files pull_request "$work/pr-down" GH_REFUSE_FIRST=99
check "it fails" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$gh_calls")" 3)"
check "naming the pull request, the read, the attempts and their bounds" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files failed 3 attempts, 0 s and 0 s apart, each bounded at 6 s, under one bound of 50 s within the step's 60 s budget, ")"
check "it writes no repository" "$(is "$(test -e "$work/pr-down" && echo yes || echo no)" no)"

echo "case: GitHub never answers, and each read ignores TERM, so only KILL ends it"
# The worst case the attempts are divided for: three attempts, each running to its 6 s bound and
# killed 10 s later, 48 s, which ends before the bound of 50 s over the read. On a loaded machine
# the steps between the attempts can spend those 2 s, and then the bound over the read ends the
# third attempt's wait for its KILL and names itself instead: either way the read is named. The
# case's own bound is the 60 s budget and 5 s for the script's own steps around the read, so a
# script that spent more is killed here and fails the first check.
CASE_SECONDS=$((BUDGET + 5)) list_files pull_request "$work/pr-term" GH_REFUSE_FIRST=99 GH_REFUSAL=hang-ignoring-term
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$gh_calls")" 3)"
check "naming the read" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files ")"
left=$(left_behind)
check "nothing the read started outlives the script ($left left)" "$(is "$left" 0)"

echo "case: every read is refused for GitHub's rate limit, and GET /rate_limit hangs"
# gh-read.sh answers each refusal with a read of GET /rate_limit, bounded at 30 s, past the 6 s
# the attempts are divided for: three attempts take 90 s. The bound over the read ends it at 50 s,
# and the case's bound is the budget and 5 s, as above.
CASE_SECONDS=$((BUDGET + 5)) list_files pull_request "$work/pr-rate-hang" GH_REFUSE_FIRST=99 GH_REFUSAL=rate-limit GH_RATE_LIMIT=hang
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "the ladder looked the reset up" "$(is "$([ "$(call_count "$rate_limit_calls")" -ge 1 ] && echo yes || echo no)" yes)"
check "naming the read and the bound that ended it" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files did not end within the step's 60 s budget, whose bound of 50 s ends the read")"
# When the bound fired, a lookup of GET /rate_limit was running under its own `timeout`, in a
# process group of its own.
left=$(left_behind)
check "nothing the read started outlives the script ($left left)" "$(is "$left" 0)"

echo "case: every read is refused for GitHub's rate limit and then ignores TERM, and GET /rate_limit says the budget is back"
# gh-read.sh runs an attempt's command a second time when the reset has passed: each attempt is
# two reads killed 16 s after they start, 32 s, and three attempts take 96 s. The bound over the
# read ends it at 50 s.
CASE_SECONDS=$((BUDGET + 5)) list_files pull_request "$work/pr-rate-twice" GH_REFUSE_FIRST=99 GH_REFUSAL=rate-limit-hang
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "the ladder ran the first attempt's read twice" "$(is "$([ "$(call_count "$gh_calls")" -ge 2 ] && echo yes || echo no)" yes)"
check "naming the read and the bound that ended it" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files did not end within the step's 60 s budget, whose bound of 50 s ends the read")"
# When the bound fired, a read that ignores TERM was running under its attempt's `timeout`, in a
# process group of its own, with up to 16 s left before that `timeout` would KILL it.
left=$(left_behind)
check "nothing the read started outlives the script ($left left)" "$(is "$left" 0)"

echo "case: under a Runner.Worker with JOB_TIMEOUT_MINUTES=30 and STEP_TIMEOUT_MINUTES=2, a rate limit whose reset gh-read.sh waits for past the bound"
# Every read is refused for the hourly budget, which GET /rate_limit says resets 70 s from now.
# gh-read.sh measures the step's 2 minutes from the step's start, the earlier of the two limits,
# and a reset 71 s away leaves its 30 s margin before them, so it waits. The bound over the read
# ends that wait at 50 s, inside the 60 s budget, whatever the step's limit allows. The step starts
# JOB_LEAD s after the stand-in job, so a deadline measured from the job's start names a time 6 s
# or more before the one this case reads, outside the 2 s it allows.
under_worker "$work/pr-worker" JOB_TIMEOUT_MINUTES=30 STEP_TIMEOUT_MINUTES=2 GH_REFUSE_FIRST=99 GH_REFUSAL=rate-limit GH_RATE_LIMIT=spent-for:70
check "the stand-in job started $JOB_LEAD s or more before its step ($((started - job_started)) s)" \
  "$(is "$([ "$((started - job_started))" -ge "$JOB_LEAD" ] && echo yes || echo no)" yes)"
# step_end_at <seconds>: the times, 2 s either side, at which a limit of that many seconds from
# `started` ends, as a grep alternation; the process starts gh-read.sh reads are whole seconds.
step_end_at() {
  local pattern="" at skew
  for skew in -2 -1 0 1 2; do
    at=$(date -u -d "@$((started + $1 + skew))" +%Y-%m-%dT%H:%M:%SZ)
    if [ -z "$pattern" ]; then pattern=$at; else pattern="$pattern\\|$at"; fi
  done
  printf '\\(%s\\)' "$pattern"
}
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "gh-read.sh derived the step's limit, 2 minutes from the step's start, and waited within it" \
  "$(contains "$err" "^::warning::the API budget is spent until .*; waiting [0-9]* s for it, within this step's limit at $(step_end_at 120), then calling once more\$")"
check "the bound over the read ended that wait, by name" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files did not end within the step's 60 s budget, whose bound of 50 s ends the read")"
left=$(left_behind)
check "nothing the read started outlives the script, gh-read.sh's sleep among them ($left left)" "$(is "$left" 0)"

echo "control: the same refusals with STEP_TIMEOUT_MINUTES=1"
# The step's limit is now 60 s from its start, and a reset 71 s away is past it: gh-read.sh waits
# for nothing, names the limit it derived, and stops the ladder at its first attempt, since a later
# one within seconds would meet the same refusal. The line the case above reads is absent. Run
# under the same stand-in Runner.Worker, the nearest one among the script's parents also in CI,
# whose own runner is a Runner.Worker further up.
under_worker "$work/pr-worker-short" JOB_TIMEOUT_MINUTES=30 STEP_TIMEOUT_MINUTES=1 GH_REFUSE_FIRST=99 GH_REFUSAL=rate-limit \
  GH_RATE_LIMIT=spent-for:70
check "the stand-in job started $JOB_LEAD s or more before its step ($((started - job_started)) s)" \
  "$(is "$([ "$((started - job_started))" -ge "$JOB_LEAD" ] && echo yes || echo no)" yes)"
check "it fails" "$(is "$status" 1)"
check "gh-read.sh waited for nothing" "$(is "$(contains "$err" "waiting [0-9]* s for it")" false)"
check "  naming the step's limit it derived, 1 minute from the step's start" \
  "$(contains "$err" "^::error::the API budget is spent until .*, past this step's limit at $(step_end_at 60); re-run the job after that time\$")"
check "after one attempt" "$(is "$(call_count "$gh_calls")" 1)"
check "naming the rate limit" \
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GitHub refused GET repos/o/r/pulls/7/files for its rate limit, ")"

echo "case: the budget is divided over three attempts and their waits"
# 300 s with a 1 s backoff unit, so 1 s and 2 s between attempts: the read is bounded at 290 s,
# and each attempt at (290 - 3) / 3 - 10 = 85 s. Every read is refused at once, so the case reads
# the bounds from the error line.
BUDGET=300
list_files pull_request "$work/pr-300" GH_REFUSE_FIRST=99 GH_RETRY_SECONDS=1
BUDGET=60
check "it fails after three refused attempts" "$(is "$status" 1)"
check "each attempt bounded at 85 s, the read at 290 s, within the 300 s budget" \
  "$(contains "$err" "failed 3 attempts, 1 s and 2 s apart, each bounded at 85 s, under one bound of 290 s within the step's 300 s budget, ")"

echo "case: a pull request that changes no file"
pr_event 0
echo '[]' > "$work/pages.json"
list_files pull_request "$work/pr-empty"
check "it answers" "$(is "$status" 0)"
check "the repository's diff is empty, so every filter is false" "$(is "$(diff_paths "$work/pr-empty")" "")"

# 42 s: (42 - 10) / 3 - 10 = 0 s an attempt. 43 s would leave 1 s, which the script takes.
echo "case: a budget that leaves no second for an attempt, and no budget at all"
rm -f "$work/gh-calls"
set +e
err=$(env PATH="$work/bin:$PATH" GITHUB_EVENT_NAME=pull_request GITHUB_EVENT_PATH="$work/event.json" \
  "$script_dir/changed-files.sh" --budget 42 "$work/pr-small" 2>&1 > /dev/null)
status=$?
set -e
check "a 42 s budget is refused" "$(is "$status" 2)"
check "  and says why" "$(contains "$err" "^::error::changed-files.sh's budget of 42 s leaves no second for an attempt: 10 s between TERM and KILL for the bound over the read, and inside it 3 attempts, each with 10 s between TERM and KILL, and 0 s of waiting between them\$")"
set +e
err=$(env PATH="$work/bin:$PATH" GITHUB_EVENT_NAME=pull_request GITHUB_EVENT_PATH="$work/event.json" \
  "$script_dir/changed-files.sh" "$work/pr-nobudget" 2>&1 > /dev/null)
status=$?
set -e
check "a call with no budget is refused" "$(is "$status" 2)"
check "  and says the budget comes first" "$(contains "$err" '^::error::usage: changed-files.sh --budget <seconds> <directory>')"
check "neither read the API" "$(is "$(cat "$work/gh-calls" 2> /dev/null || true)" "")"

origin="$work/origin"
"$real_git" init -q --initial-branch=main "$origin"
"$real_git" -C "$origin" config uploadpack.allowAnySHA1InWant true
"$real_git" -C "$origin" config uploadpack.allowFilter true
commit() { "$real_git" -C "$origin" add -A && "$real_git" -C "$origin" -c user.name=t -c user.email=t@localhost -c commit.gpgsign=false commit -q --no-verify -m "$1"; }
mkdir -p "$origin/src" "$origin/docs" "$origin/lib/x" "$origin/.github"
printf 'app\n' > "$origin/src/app.ts"
printf 'gone\n' > "$origin/src/gone.ts"
printf 'same\n' > "$origin/src/same.ts"
printf 'old\n' > "$origin/docs/old"
printf 'y\n' > "$origin/lib/x/y"
printf 'before\n' > "$origin/src/before.go"
printf 'ci\n' > "$origin/.github/ci.yaml"
commit base
base_sha=$("$real_git" -C "$origin" rev-parse HEAD)
printf 'app, changed\n' > "$origin/src/app.ts"
rm "$origin/src/gone.ts" "$origin/docs/old" "$origin/lib/x/y"
rmdir "$origin/lib/x"
"$real_git" -C "$origin" mv src/before.go src/after.go
mkdir -p "$origin/docs/old"
printf 'index\n' > "$origin/docs/old/index.md"
printf 'x\n' > "$origin/lib/x"
printf 'new\n' > "$origin/src/new.ts"
commit head
head_sha=$("$real_git" -C "$origin" rev-parse HEAD)
group_paths=$(printf '%s\n' 'docs/old' 'docs/old/index.md' 'lib/x' 'lib/x/y' 'src/after.go' 'src/app.ts' \
  'src/before.go' 'src/gone.ts' 'src/new.ts')
jq -n --arg base "$base_sha" --arg head "$head_sha" '{merge_group: {base_sha: $base, head_sha: $head}}' > "$work/event.json"

# sparse_checkout <directory> <depth>: the head, checked out as actions/checkout does for a job with
# `sparse-checkout: .github` (src/git-source-provider.ts: a blob:none fetch of the one commit at
# that depth, then the sparse cone and the checkout).
sparse_checkout() {
  "$real_git" init -q "$1"
  "$real_git" -C "$1" remote add origin "file://$origin"
  "$real_git" -C "$1" -c protocol.version=2 fetch -q --no-tags --prune --no-recurse-submodules \
    --filter=blob:none --depth="$2" origin "+$head_sha:refs/remotes/origin/group"
  "$real_git" -C "$1" sparse-checkout set .github
  "$real_git" -C "$1" checkout -q --force -B group refs/remotes/origin/group
}

echo "case: a merge group whose base the checkout lacks"
"$real_git" clone -q --depth 1 "file://$origin" "$work/checkout"
cd "$work/checkout"
check "the clone lacks the base" "$(is "$("$real_git" cat-file -e "$base_sha^{commit}" 2> /dev/null && echo has || echo lacks)" lacks)"
list_files merge_group "$work/group"
check "it answers" "$(is "$status" 0)"
check "it fetched the base, and only the base" "$(is "$fetches" "fetch --depth=1 --no-tags origin $base_sha")"
check "it read no API" "$(is "$gh_calls" "")"
check "the repository's diff names exactly the paths of git diff base..head" \
  "$(is "$(diff_paths "$work/group")" "$group_paths")"
check "  which is the diff paths-filter runs for a merge group" \
  "$(is "$("$real_git" diff --no-renames --name-only "$base_sha..$head_sha" | LC_ALL=C sort)" "$group_paths")"
check "a file that became a directory is deleted" "$(is "$(diff_status "$work/group" docs/old)" D)"
check "a renamed file is its new name added" "$(is "$(diff_status "$work/group" src/after.go)" A)"
check "it says where it read them" \
  "$(is "$out" "9 path(s) changed: merge group $base_sha..$head_sha, read with git diff (its base fetched from origin)")"

echo "case: the job's checkout, sparse at fetch-depth 2, holds the base as the head's parent"
sparse_checkout "$work/sparse-2" 2
cd "$work/sparse-2"
list_files merge_group "$work/group-sparse-2"
check "it answers" "$(is "$status" 0)"
check "it fetched nothing" "$(is "$fetches" "")"
check "with the same paths" "$(is "$(diff_paths "$work/group-sparse-2")" "$group_paths")"
check "and says so" \
  "$(is "$out" "9 path(s) changed: merge group $base_sha..$head_sha, read with git diff (no read: the base is a parent of the head, in the checkout)")"

echo "case: a sparse blob:none checkout at depth 1, origin answering"
sparse_checkout "$work/sparse-1" 1
cd "$work/sparse-1"
list_files merge_group "$work/group-sparse-1"
check "it answers" "$(is "$status" 0)"
check "the one fetch is the bounded one: no lazy fetch ran before it" \
  "$(is "$fetches" "fetch --depth=1 --no-tags origin $base_sha")"
check "with the same paths" "$(is "$(diff_paths "$work/group-sparse-1")" "$group_paths")"

echo "case: a sparse blob:none checkout at depth 1, origin accepting and never answering"
# A lazy fetch of the base, which `git cat-file -e` on a partial clone makes by itself, would wait
# on this origin with no bound. The case's own bound is the budget and 5 s, so a script that made
# one is killed here and fails the first check.
python3 - "$work/port" <<'PY' &
import socket, sys
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen(64)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
held = []
while True:
    held.append(s.accept()[0])
PY
listener=$!
for _ in $(seq 100); do [ -s "$work/port" ] && break; sleep 0.1; done
sparse_checkout "$work/sparse-stall" 1
cd "$work/sparse-stall"
"$real_git" remote set-url origin "http://127.0.0.1:$(cat "$work/port")/origin.git"
CASE_SECONDS=$((BUDGET + 5)) list_files merge_group "$work/group-stall"
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "after three bounded fetches" "$(is "$(call_count "$fetches")" 3)"
check "naming the commit and the read" \
  "$(contains "$err" "^::error::the changes job could not fetch commit $base_sha of the merge group from origin: git fetch failed 3 attempts, ")"
kill "$listener" 2> /dev/null || true
listener=""

echo "case: the base is a parent of the head, but its tree is not in the checkout, and origin never answers"
# A tree:0 partial clone at depth 2 holds both commits and only the trees its checkout read, so
# `git diff base..head` must fetch the base's tree. Without GIT_NO_LAZY_FETCH git would fetch it
# by itself from this origin and wait with no bound; with it the diff fails at once, by name. Git
# reads GIT_NO_LAZY_FETCH from 2.44 on, the runner's is 2.56.1; an older git cannot run this case.
git_version=$("$real_git" version | awk '{print $3}')
if [ "$(printf '%s\n' 2.44 "$git_version" | sort -V | head -n 1)" != 2.44 ]; then
  echo "  SKIP: git $git_version predates GIT_NO_LAZY_FETCH (2.44), so this case did not run"
else
  python3 - "$work/port2" <<'PY' &
import socket, sys
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen(64)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
held = []
while True:
    held.append(s.accept()[0])
PY
  listener=$!
  for _ in $(seq 100); do [ -s "$work/port2" ] && break; sleep 0.1; done
  "$real_git" clone -q --filter=tree:0 --depth 2 --no-checkout "file://$origin" "$work/treeless"
  "$real_git" -C "$work/treeless" checkout -q --detach "$head_sha"
  "$real_git" -C "$work/treeless" remote set-url origin "http://127.0.0.1:$(cat "$work/port2")/origin.git"
  cd "$work/treeless"
  CASE_SECONDS=$((BUDGET + 5)) list_files merge_group "$work/group-treeless"
  check "it fails by itself, inside its budget" "$(is "$status" 1)"
  check "having fetched nothing itself" "$(is "$fetches" "")"
  check "naming the diff" \
    "$(contains "$err" "^::error::the changes job could not diff the merge group's base $base_sha and head $head_sha in the checkout")"
  kill "$listener" 2> /dev/null || true
  listener=""
  cd "$work"
fi

echo "case: a merge group whose base fetch is refused twice and then served"
"$real_git" clone -q --depth 1 "file://$origin" "$work/checkout-502"
cd "$work/checkout-502"
list_files merge_group "$work/group-502" GIT_REFUSE_FIRST=2
check "it answers" "$(is "$status" 0)"
check "after two retries: three fetches" "$(is "$(call_count "$fetches")" 3)"
check "with the same paths" "$(is "$(diff_paths "$work/group-502")" "$group_paths")"

echo "case: a merge group whose base fetch is never served"
"$real_git" clone -q --depth 1 "file://$origin" "$work/checkout-down"
cd "$work/checkout-down"
list_files merge_group "$work/group-down" GIT_REFUSE_FIRST=99
check "it fails" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$fetches")" 3)"
check "naming the commit, the read and the attempts" \
  "$(contains "$err" "^::error::the changes job could not fetch commit $base_sha of the merge group from origin: git fetch failed 3 attempts, ")"
check "it writes no repository" "$(is "$(test -e "$work/group-down" && echo yes || echo no)" no)"

echo "case: a checkout whose HEAD is not the merge group's head"
"$real_git" -C "$work/checkout-down" -c user.name=t -c user.email=t@localhost -c commit.gpgsign=false \
  commit -q --allow-empty --no-verify -m other
list_files merge_group "$work/group-other"
check "it fails" "$(is "$status" 1)"
check "naming both commits" "$(contains "$err" "^::error::the checkout's HEAD is [0-9a-f]*, not the merge group's head $head_sha")"
check "having fetched nothing" "$(is "$fetches" "")"
cd "$work"

# A pull request past the 3,000 files the API lists: the checkout is its merge commit, as
# actions/checkout makes refs/pull/<n>/merge, whose first parent is a base newer than the one the
# pull request branched from (it adds other/base-only.txt), and whose second is the pull request's
# head. The merge's diff against its first parent is the pull request's change, base..head, and not
# the newer base's own file.
commit_env=(env GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@localhost GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@localhost)
base_only=$(printf 'base only\n' | "$real_git" -C "$origin" hash-object -w --stdin)
tree_with_base_only() { # tree_with_base_only <commit>: that commit's tree with other/base-only.txt added
  GIT_INDEX_FILE="$work/merge-index" "$real_git" -C "$origin" read-tree "$1"
  GIT_INDEX_FILE="$work/merge-index" "$real_git" -C "$origin" update-index --add --cacheinfo "100644,$base_only,other/base-only.txt"
  GIT_INDEX_FILE="$work/merge-index" "$real_git" -C "$origin" write-tree
}
newer_base=$("${commit_env[@]}" "$real_git" -C "$origin" commit-tree "$(tree_with_base_only "$base_sha")" -p "$base_sha" -m "newer base")
merge_sha=$("${commit_env[@]}" "$real_git" -C "$origin" commit-tree "$(tree_with_base_only "$head_sha")" -p "$newer_base" -p "$head_sha" -m "merge")
"$real_git" -C "$origin" update-ref refs/pull/7/merge "$merge_sha"
pr_event 3500

echo "case: a pull request of 3500 files, in the changes job's checkout of its merge commit"
# As actions/checkout fetches it: blob:none, at the job's fetch-depth 2, the .github cone.
"$real_git" init -q "$work/merge-checkout"
"$real_git" -C "$work/merge-checkout" remote add origin "file://$origin"
"$real_git" -C "$work/merge-checkout" -c protocol.version=2 fetch -q --no-tags --prune --no-recurse-submodules \
  --filter=blob:none --depth=2 origin "+$merge_sha:refs/remotes/pull/7/merge"
"$real_git" -C "$work/merge-checkout" sparse-checkout set .github
"$real_git" -C "$work/merge-checkout" checkout -q --force refs/remotes/pull/7/merge
cd "$work/merge-checkout"
list_files pull_request "$work/pr-merge" GITHUB_SHA="$merge_sha"
check "it answers" "$(is "$status" 0)"
check "it read no API" "$(is "$gh_calls" "")"
check "and fetched nothing" "$(is "$fetches" "")"
check "the repository's diff names exactly the pull request's paths, base..head" \
  "$(is "$(diff_paths "$work/pr-merge")" "$group_paths")"
check "  and not the newer base's own file" "$(is "$(diff_status "$work/pr-merge" other/base-only.txt)" "")"
check "it says where it read them" \
  "$(is "$out" "9 path(s) changed: pull request #7, 3500 files past the 3000 the API lists, read with git diff of its merge commit $merge_sha against its first parent $newer_base")"

echo "control: the same pull request in a checkout of its head, a commit with one parent"
cd "$work/sparse-2"
list_files pull_request "$work/pr-merge-head" GITHUB_SHA="$head_sha"
check "it fails" "$(is "$status" 1)"
check "naming the checkout's HEAD, which has no second parent, and what to do" \
  "$(contains "$err" "^::error::pull request #7 changes 3500 files, more than the 3000 GET repos/o/r/pulls/7/files lists, and the checkout's HEAD ($head_sha) is not its merge commit GITHUB_SHA ($head_sha) with two parents, .*; split it into pull requests of at most 3000 files each")"
check "having read nothing" "$(is "$gh_calls" "")"
check "it writes no repository" "$(is "$(test -e "$work/pr-merge-head" && echo yes || echo no)" no)"
cd "$work"

echo "case: an event that carries no change is refused"
list_files workflow_dispatch "$work/dispatched"
check "it fails" "$(is "$status" 1)"
check "and names the event" \
  "$(contains "$err" "^::error::changed-files.sh lists the files of a pull_request or a merge_group event, not of workflow_dispatch\$")"

summary "the change's files the changes job hands its paths-filter steps"
