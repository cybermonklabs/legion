#!/usr/bin/env bash
# Sourced by the two harnesses of the private-name check, check-private-names.test.sh (the rules
# the script spells, its modes and a pull request's text) and check-private-names-terms.test.sh
# (the deployment's own term list, PRIVATE_NAME_TERMS): what both read and nothing either alone
# does. It creates the run's temporary directory, tees the harness's own output to a log that
# finish_self_log scans with the guard at the end, sources test-lib.sh, and holds the name pieces,
# the placeholder terms, the fixture repository and the stand-in GitHub API the cases share.
#
# Each case builds a miniature git repository under that directory and symlinks the real script
# into it, so the script under test is the file CI runs: it resolves its root from its own path,
# which the symlink puts at the fixture root. The names it hunts are assembled from pieces below,
# so neither this file nor a harness holds one and the check scans them like any other file.
#
# Not executable: a harness sources it after its own `set -euo pipefail` and `set +x`.
# shellcheck disable=SC2034 # the pieces, terms and paths below are the harnesses' to read

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-private-names.sh"
pr_text_script="$script_dir/check-pr-text.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Every case's stdout (echoed "case:" lines and `check`'s PASS/FAIL lines) is the harness's own
# output — the log of ci.yaml's test job — so it is also teed to self_log, which finish_self_log,
# the last case of each harness, scans with the guard itself: a future case that prints an
# assembled form or a placeholder term turns red there, in the suite, before it ever reaches a
# real job log. `exec 3>&1 4>&2` keeps the real terminal fds reachable so the redirection can be
# undone before reading the file: a process substitution's write end must close (and its process
# be waited on) before its output is guaranteed to be flushed to disk.
self_log="$work/suite-output.log"
exec 3>&1 4>&2
exec > >(tee "$self_log") 2>&1
tee_pid=$!

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

repo="agent""-c"
repo_snake="agent""_c"
repo_joined="agent""c"
repo_spaced="agent"" c"
repo_en_dash="agent""–c"
repo_nonbreaking_hyphen="agent""‑c"
repo_percent_hyphen="agent""%2Dc"
nbsp=$'\xc2\xa0'
en_dash=$'\xe2\x80\x93'
key="AGENT""C"
company="traj""ectory"
company_proper="T""rajectory"
labs="labs"
internal="internal"
private_host_suffix="private.example"
example_host_suffix="example"
vendored_nats="packages/claude-envoy/dist/envoy-channel.js"
tailnet_suffix="ts.net"
tailnet_label="realtailnet"
tailnet_example="example-tailnet"

# The term list every case runs the guard under, unless a case sets its own: placeholder terms
# standing in for the deployment's own list (PRIVATE_NAME_TERMS, an Actions secret), on a pull
# request's event. One holds regex characters, to show a term is a fixed string, and one a "/",
# standing in for an owner/repository form. Whatever list or event the job's own environment
# holds is replaced, never read: the test job holds no secret.
planted="zephyr-orchard"
planted_dotted="copper.kettle"
planted_owner="lanternfish/harbor-map"
planted_terms=$(printf '%s\n' "$planted" "$planted_dotted" "$planted_owner")
export GITHUB_EVENT_NAME=pull_request
export PRIVATE_NAME_TERMS=$planted_terms

# names_planted <text>: true/false for check; whether text holds a planted term, or either half of
# the owner/repository one, in any case.
names_planted() {
  local term
  for term in "$planted" "$planted_dotted" "$planted_owner" "${planted_owner%%/*}" "${planted_owner#*/}"; do
    if grep -q -i -F -- "$term" <<< "$1"; then
      echo true
      return
    fi
  done
  echo false
}

# fixture <name>: a clean repository with one tracked file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/docs"
  ln -s "$check_script" "$root/.github/scripts/check-private-names.sh"
  printf '# Notes\n\nNothing private here.\n' > "$root/docs/notes.md"
  git -C "$root" init -q
  git -C "$root" add docs/notes.md
  echo "$root"
}

# run_check <root> [path...]: sets `out` to the combined output and `status` to the exit code.
run_check() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && .github/scripts/check-private-names.sh "$@" 2>&1)
  status=$?
  set -e
}

# write_pr_text <title> <body> <commit message> [branch] [author email] [committer email]: writes
# the fixture check-pr-text.sh reads as CI runs it. A pull_request event payload carries the head
# the run checks (its base and head sha, and its one commit); the stand-in `gh` answers the pull
# request route with the title, body and branch name the pull request has now (`$work/pull`, the
# event's own unless a case rewrites it); and it answers the compare route for exactly that
# base...head with that commit (message, author and committer) in the route's own JSON shape, so
# the real `.commits[] | .commit.message, …`
# filter that feeds the required check runs for real, rather than a filter the stub fakes. It
# matches that URL whole and refuses a call without --paginate, so losing either paging token
# fails every case below instead of passing on a list GitHub cut short at 250 commits. It answers
# the pull request's three comment routes (issue comments, reviews and review comments), which
# report no total, one page of 100 at a time: page N of each is `$work/<route>.N` (`comments`,
# `reviews` or `review-comments`), and a page no case wrote is an empty list, as GitHub answers past
# the last. The stub also answers the pull request's own commits route, which reads whatever head
# the pull request has by then: the same commit, until the mid-run-push case appends a later one
# that names a private thing. A case rewrites `$work/compare` (one JSON object per page, as
# `gh api --paginate` prints them), `$work/pr-commits` or a comment route's pages, then calls
# run_pr_text, which runs the script and sets `out` and `status`; the stub logs each call it
# answers to `$work/calls`. For the issue-key rule it answers four routes more, each from a file a
# case writes and otherwise as GitHub answers a repository with no such record: the commits that
# touch the cutoff marker (`$work/cutoff-commits`, else none, so the rule is inactive); one
# release by its tag (`$work/release.<tag>`, else a 404 as gh prints it, and a file holding
# `HTTP 500` a server error); one commit's files (`$work/commit.<sha>`, else a refusal); and the
# pull request's files (`$work/pr-files`, one JSON list a page as `gh api --paginate` prints
# them, else a refusal; read without --paginate, also a refusal). The repository's history and
# tags, which the rule reads with git, are a bare repository under `$work/remote`, which
# run_pr_text names as GITHUB_SERVER_URL.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PR_TEXT_TEST_DIR/calls"
# page <route> <url>: the page of a comment route the url's `page=N` asks for.
page() {
  local file="$PR_TEXT_TEST_DIR/$1.${2##*&page=}"
  if [ -e "$file" ]; then cat "$file"; else echo '[]'; fi
}
# The compare route is the event's own base...head, matched whole: base-sha...event-head-sha as
# write_pr_text writes the event, or a real commit's id where a case sets one for git to read.
compare_route="api repos/example/repo/compare/$(jq -r '.pull_request.base.sha' "$PR_TEXT_TEST_DIR/event.json")"
compare_route+="...$(jq -r '.pull_request.head.sha' "$PR_TEXT_TEST_DIR/event.json")?per_page=100"
if [ "$1 $2" = "$compare_route" ]; then
  [[ " $* " == *" --paginate "* ]] || { echo "compare route read without --paginate: $*" >&2; exit 3; }
  cat "$PR_TEXT_TEST_DIR/compare"
  exit 0
fi
case "$1 $2" in
  "api repos/example/repo/issues/7/comments?per_page=100&page="[1-9]*) page comments "$2" ;;
  "api repos/example/repo/pulls/7/reviews?per_page=100&page="[1-9]*) page reviews "$2" ;;
  "api repos/example/repo/pulls/7/comments?per_page=100&page="[1-9]*) page review-comments "$2" ;;
  "api repos/example/repo/pulls/7/commits") cat "$PR_TEXT_TEST_DIR/pr-commits" ;;
  "api repos/example/repo/pulls/7") cat "$PR_TEXT_TEST_DIR/pull" ;;
  "api repos/example/repo/commits?path=.github/issue-key-cutoff&sha=main&per_page=100")
    if [ -e "$PR_TEXT_TEST_DIR/cutoff-commits" ]; then cat "$PR_TEXT_TEST_DIR/cutoff-commits"; else echo '[]'; fi ;;
  "api repos/example/repo/releases/tags/"*)
    file="$PR_TEXT_TEST_DIR/release.${2#repos/example/repo/releases/tags/}"
    if [ ! -e "$file" ]; then echo "gh: Not Found (HTTP 404)" >&2; exit 1; fi
    if grep -q -x 'HTTP 500' "$file"; then echo "gh: Server Error (HTTP 500)" >&2; exit 1; fi
    cat "$file" ;;
  "api repos/example/repo/pulls/7/files?per_page=100")
    [[ " $* " == *" --paginate "* ]] || { echo "files route read without --paginate: $*" >&2; exit 3; }
    if [ -e "$PR_TEXT_TEST_DIR/pr-files" ]; then cat "$PR_TEXT_TEST_DIR/pr-files"; else echo "no files answer" >&2; exit 3; fi ;;
  "api repos/example/repo/commits/"*"?per_page=100")
    sha=${2#repos/example/repo/commits/}
    sha=${sha%%\?*}
    if [ -e "$PR_TEXT_TEST_DIR/commit.$sha" ]; then cat "$PR_TEXT_TEST_DIR/commit.$sha"; else echo "no such commit: $sha" >&2; exit 3; fi ;;
  *) echo "unexpected gh call: $*" >&2; exit 3 ;;
esac
GH
chmod +x "$work/bin/gh"
# pr_commit <message> [author email] [committer email]: one commit as both routes list it.
pr_commit() {
  jq -nc --arg msg "$1" --arg author_email "${2:-dev@example.com}" \
    --arg committer_email "${3:-dev@example.com}" \
    '{commit: {message: $msg, author: {name: "Dev", email: $author_email},
               committer: {name: "Dev", email: $committer_email}}}'
}
write_pr_text() {
  local title=$1 body=$2 message=$3 branch=${4:-fix/keep-the-order} commit
  commit=$(pr_commit "$message" "${5:-}" "${6:-}")
  jq -nc --argjson commit "$commit" '{total_commits: 1, commits: [$commit]}' > "$work/compare"
  jq -nc --argjson commit "$commit" '[$commit]' > "$work/pr-commits"
  printf '%s' "$body" > "$work/pr-body"
  jq -n --arg title "$title" --arg branch "$branch" --rawfile body "$work/pr-body" \
    '{pull_request: {number: 7, title: $title, commits: 1,
                      head: {ref: $branch, sha: "event-head-sha"}, base: {sha: "base-sha"},
                      body: (if $body == "" then null else $body end)}}' \
    > "$work/event.json"
  jq '.pull_request' "$work/event.json" > "$work/pull"
  rm -f "$work"/comments.* "$work"/reviews.* "$work"/review-comments.* "$work/cutoff-commits" \
    "$work"/release.* "$work"/commit.* "$work/pr-files"
}
run_pr_text() {
  if [ $# -gt 0 ]; then write_pr_text "$@"; fi
  : > "$work/calls"
  set +e
  out=$(GITHUB_EVENT_PATH="$work/event.json" GITHUB_REPOSITORY=example/repo \
    GITHUB_SERVER_URL="file://$work/remote" PR_TEXT_TEST_DIR="$work" PATH="$work/bin:$PATH" \
    "$pr_text_script" 2>&1)
  status=$?
  set -e
}
# app: who posted an item a case writes, unless the case says otherwise: an App installed on the
# repository, which posts as a Bot, with the association every Legion App's comments carry here.
app='{"user": {"login": "dev-app[bot]", "type": "Bot"}, "author_association": "CONTRIBUTOR"}'
# items <id> <body> [<id> <body>...]: one page of a comment route, each item its id, body and
# author (app) in the route's own JSON shape.
items() {
  local list='[]'
  while [ $# -gt 0 ]; do
    list=$(jq -c --argjson id "$1" --arg body "$2" --argjson author "$app" \
      '. + [{id: $id, body: $body} + $author]' <<< "$list")
    shift 2
  done
  printf '%s\n' "$list"
}
# by <login> <type> <association> <page> [index]: rewrites a page's items (only the one at index,
# when given) as posted by that author.
by() {
  local filter='map(author)'
  if [ $# -gt 4 ]; then filter=".[$5] |= author"; fi
  jq -c --arg login "$1" --arg type "$2" --arg association "$3" \
    "def author: .user = {login: \$login, type: \$type} | .author_association = \$association; $filter" \
    "$4" > "$4.next"
  mv "$4.next" "$4"
}

# clean_pr: a pull request whose title, body, branch and commit name nothing.
clean_pr() { write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"; }

# finish_self_log: the last case of a harness. It ends the tee and runs the guard over everything
# the harness printed, under the placeholder list.
finish_self_log() {
  local suite_status=0
  echo "case: the suite's own output never names a private thing"
  exec 1>&3 2>&4
  wait "$tee_pid" 2> /dev/null || true
  "$check_script" "$self_log" > /dev/null 2>&1 || suite_status=$?
  check "the suite's own PASS/FAIL log passes the guard, its placeholder terms included" \
    "$(is "$suite_status" 0)"
}
