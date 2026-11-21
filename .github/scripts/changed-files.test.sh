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
# is cut off mid-row, or a 200 that lists no file. A case counts the attempts the script made.
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
# A 45 s budget with no wait between attempts: 45 / 3 - 10 = 5 s an attempt. The hanging case
# waits it out once, and the fetches from a local origin, which take milliseconds, stay well inside
# it on a loaded runner.
export GH_RETRY_SECONDS=0 GITHUB_REPOSITORY=o/r STANDIN_DIR="$work"
BUDGET=45

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
    hang) exec sleep 120 ;;
    # A read that ignores TERM, so only the KILL `timeout -k` sends 10 s after its bound ends it.
    hang-ignoring-term) trap '' TERM; exec sleep 120 ;;
    truncated) body="$STANDIN_DIR/truncated.json" ;;
    empty) body="$STANDIN_DIR/empty.json" ;;
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
# `timeout` of CASE_SECONDS (60 by default) over $work/event.json, writing the repository to
# <directory>; sets out, err and status, and leaves the stand-ins' call logs in gh_calls and
# fetches.
list_files() {
  local event_name=$1 dest=$2
  shift 2
  rm -f "$work/gh-calls" "$work/git-fetches"
  set +e
  out=$(env PATH="$work/bin:$PATH" GITHUB_EVENT_NAME="$event_name" GITHUB_EVENT_PATH="$work/event.json" "$@" \
    timeout "${CASE_SECONDS:-60}" "$script_dir/changed-files.sh" --budget "$BUDGET" "$dest" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
  gh_calls=$(cat "$work/gh-calls" 2> /dev/null || true)
  fetches=$(cat "$work/git-fetches" 2> /dev/null || true)
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

echo "case: a pull request of more files than the API lists"
pr_event 3500
list_files pull_request "$work/pr-huge"
check "it fails" "$(is "$status" 1)"
check "naming the count and the cap" \
  "$(contains "$err" "^::error::pull request #7 changes 3500 files, and GET repos/o/r/pulls/7/files lists at most 3000")"
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
  "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: GET repos/o/r/pulls/7/files failed 3 attempts, 0 s and 0 s apart, each bounded at 5 s within the step's 45 s budget, ")"
check "it writes no repository" "$(is "$(test -e "$work/pr-down" && echo yes || echo no)" no)"

echo "case: GitHub never answers, and each read ignores TERM, so only KILL ends it"
# The worst case the budget is divided for: three attempts, each running to its 5 s bound and
# killed 10 s later, 45 s, the whole budget. The case's own bound is that budget and 5 s for the
# script's own steps around the read, so a script that spent more is killed here and fails the
# first check.
CASE_SECONDS=$((BUDGET + 5)) list_files pull_request "$work/pr-term" GH_REFUSE_FIRST=99 GH_REFUSAL=hang-ignoring-term
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "after three attempts" "$(is "$(call_count "$gh_calls")" 3)"
check "naming the read" "$(contains "$err" "^::error::the changes job could not read the files of pull request #7: ")"

echo "case: the budget is divided over three attempts and their waits"
# 300 s with a 1 s backoff unit, so 1 s and 2 s between attempts: (300 - 3) / 3 - 10 = 89 s an
# attempt. Every read is refused at once, so the case reads the bound from the error line.
BUDGET=300
list_files pull_request "$work/pr-300" GH_REFUSE_FIRST=99 GH_RETRY_SECONDS=1
BUDGET=45
check "it fails after three refused attempts" "$(is "$status" 1)"
check "each attempt bounded at 89 s within the 300 s budget" \
  "$(contains "$err" "failed 3 attempts, 1 s and 2 s apart, each bounded at 89 s within the step's 300 s budget, ")"

echo "case: a pull request that changes no file"
pr_event 0
echo '[]' > "$work/pages.json"
list_files pull_request "$work/pr-empty"
check "it answers" "$(is "$status" 0)"
check "the repository's diff is empty, so every filter is false" "$(is "$(diff_paths "$work/pr-empty")" "")"

# 32 s: 32 / 3 - 10 = 0 s an attempt. 33 s would leave 1 s, which the script takes.
echo "case: a budget that leaves no second for an attempt, and no budget at all"
rm -f "$work/gh-calls"
set +e
err=$(env PATH="$work/bin:$PATH" GITHUB_EVENT_NAME=pull_request GITHUB_EVENT_PATH="$work/event.json" \
  "$script_dir/changed-files.sh" --budget 32 "$work/pr-small" 2>&1 > /dev/null)
status=$?
set -e
check "a 32 s budget is refused" "$(is "$status" 2)"
check "  and says why" "$(contains "$err" "^::error::changed-files.sh's budget of 32 s leaves no second for an attempt: 3 attempts, each with 10 s between TERM and KILL, and 0 s of waiting between them\$")"
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

echo "case: an event that carries no change is refused"
list_files workflow_dispatch "$work/dispatched"
check "it fails" "$(is "$status" 1)"
check "and names the event" \
  "$(contains "$err" "^::error::changed-files.sh lists the files of a pull_request or a merge_group event, not of workflow_dispatch\$")"

summary "the change's files the changes job hands its paths-filter steps"
