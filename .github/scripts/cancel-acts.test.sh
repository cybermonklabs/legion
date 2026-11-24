#!/usr/bin/env bash
# Tests for the two cancel acts and what each of them does with a read GitHub refuses:
# cancel-started-act.sh, which ci.yaml's cancel-started job runs over the merge-group runs the
# queue has replaced, and cancel-dequeued-act.sh, which dequeued.yaml's cancel-dequeued job runs
# over the run of the group the queue took out. Both reclaim runner time and decide nothing, so a
# refusal that passes of itself — a 5xx, a 429, or a rate limit gh-read.sh could not wait out —
# warns and leaves the job green, while a 4xx that is not 429 fails it as every refusal did
# before: merge-group run 45467602844 went red whole for one HTTP 500 of a run list, in job
# 138706588845.
#
# Each case runs the real scripts with a stand-in `gh` first on PATH. It answers the three routes
# the two acts call — `timeline`, the GraphQL read of a pull request's timeline; `runs`, a
# merge-group run list; and `cancel`, a force-cancel — and logs every call, so a case counts the
# attempts a ladder made and the cancels an act acted on. A case says per route how many of its
# calls are refused and with which of gh's own words, so "a 500 on the first call and an answer on
# the second" is one line. GH_RETRY_SECONDS and DEQUEUED_READ_SECONDS are 0, so the attempts are
# made without the waits between them, and every case runs under `timeout`, so an act that hung
# would fail its case instead of hanging the job.
#
# Run from anywhere: .github/scripts/cancel-acts.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# cancel-started-act.sh runs .github/scripts/merge-group-cancels.sh by that path, so each case
# runs the acts from the root of the tree the harness is in.
repo_root=$(cd "$script_dir/../.." && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
# The stand-in GitHub: it counts each route's calls in <route>.count, refuses the first
# <route>.refuse of them (or every one, where that file holds \`always\`) with the words in
# <route>.words, and answers the rest with <route>.body.
set -euo pipefail
echo "\$*" >> "$work/calls"
case "\$*" in
  *"api graphql"*) route=timeline ;;
  *"--event merge_group"*) route=runs ;;
  *force-cancel*) route=cancel ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
count=\$((\$(cat "$work/\$route.count" 2> /dev/null || echo 0) + 1))
echo "\$count" > "$work/\$route.count"
refuse=\$(cat "$work/\$route.refuse" 2> /dev/null || echo 0)
if [ "\$refuse" = always ] || [ "\$count" -le "\$refuse" ]; then
  cat "$work/\$route.words" >&2
  exit 1
fi
[ ! -e "$work/\$route.body" ] || cat "$work/\$route.body"
GH
chmod +x "$work/bin/gh"

# fresh: a case's stand-in GitHub, with no refusal, no answer and no call behind it.
fresh() { rm -f "$work"/*.count "$work"/*.refuse "$work"/*.words "$work"/*.body "$work/calls"; }
# refuses <route> <how many|always> <gh's words>: how that route is refused.
refuses() { printf '%s\n' "$2" > "$work/$1.refuse"; printf '%s\n' "$3" > "$work/$1.words"; }
# answers <route> <body>: what that route says when it is not refused.
answers() { printf '%s\n' "$2" > "$work/$1.body"; }
# calls <text>: how many gh calls the case made whose words hold that text.
calls() { grep -c -- "$1" "$work/calls" || true; }
# reads <route>: how many calls that route took.
reads() { cat "$work/$1.count" 2> /dev/null || echo 0; }

# What gh prints for each refusal, in its own words: `gh run list` names the route it called and
# the status (the 500 line is run 45467602844's, job 138706588845, with this repository's name in
# place of that one's), and `gh api` names the status in parentheses.
runs_500="failed to get runs: HTTP 500 (https://api.github.com/repos/o/r/actions/workflows/397779729/runs?event=merge_group&exclude_pull_requests=true&per_page=100)"
runs_404="failed to get runs: HTTP 404 (https://api.github.com/repos/o/r/actions/workflows/397779729/runs?event=merge_group&exclude_pull_requests=true&per_page=100)"
api_500="gh: Internal Server Error (HTTP 500)"
api_403="gh: Resource not accessible by integration (HTTP 403)"

# The merge-group run list ci.yaml's cancel-started job reads: the asking run, and the group of
# the same pull request the queue replaced with it, whose branches carry the real shapes
# (`gh-readonly-queue/main/pr-<n>-<parent sha>`).
main_tip=04cec161128c5a1cdada47ba40626f02b1fdbe38
main_older=7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c
queue_branch=gh-readonly-queue/main/pr-5331-$main_tip
asking_run=45467602844
replaced_run=45467300100
merge_group_runs=$(jq -nc --arg newer "$queue_branch" --arg older "gh-readonly-queue/main/pr-5331-$main_older" \
  --argjson asking "$asking_run" --argjson replaced "$replaced_run" \
  '[{databaseId: $replaced, headBranch: $older, status: "in_progress", createdAt: "2026-11-24T18:50:00Z"},
    {databaseId: $asking, headBranch: $newer, status: "in_progress", createdAt: "2026-11-24T19:05:00Z"}]')

# The removal the cancel-dequeued act's timeline read selects, as `gh api graphql --jq` prints
# it, and the runs of the commit that removal names.
removed_head=bcc5d7e66dce573d06f577d8fc0e8101f328259d
removed_run=45467400200
removal=$(jq -nc --arg oid "$removed_head" \
  '{createdAt: "2026-11-24T19:00:00Z", reason: "dequeued", beforeCommit: {oid: $oid}}')
commit_runs=$(jq -nc --arg sha "$removed_head" --argjson id "$removed_run" \
  '[{databaseId: $id, headSha: $sha, status: "in_progress", conclusion: null}]')

# run_started, run_dequeued: one act, its stdout and stderr together in `out` (the job log holds
# both) and its exit status in `status`.
run_started() {
  set +e
  out=$(cd "$repo_root" && PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r \
    GH_RETRY_SECONDS=0 HEAD_BRANCH="$queue_branch" RUN_ID="$asking_run" \
    timeout 60 "$script_dir/cancel-started-act.sh" 2>&1)
  status=$?
  set -e
}

run_dequeued() {
  set +e
  out=$(cd "$repo_root" && PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r \
    GH_RETRY_SECONDS=0 DEQUEUED_READ_SECONDS=0 PR_NUMBER=5331 \
    timeout 60 "$script_dir/cancel-dequeued-act.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the cancel-started act reads the run list and force-cancels the group the queue replaced"
fresh
answers runs "$merge_group_runs"
run_started
check "the act ends green" "$(is "$status" 0)"
check "it force-cancels the replaced group and nothing else" \
  "$(is "$(calls "runs/$replaced_run/force-cancel")" 1)"
check "and only that one run" "$(is "$(calls force-cancel)" 1)"

echo "case: a 500 the next attempt of the ladder gets past leaves the act doing its work"
fresh
answers runs "$merge_group_runs"
refuses runs 1 "$runs_500"
run_started
check "the act ends green" "$(is "$status" 0)"
check "it read the list twice" "$(is "$(reads runs)" 2)"
check "and still force-cancels the replaced group" \
  "$(is "$(calls "runs/$replaced_run/force-cancel")" 1)"

echo "case: a 500 at every attempt warns, names the route and the status, and leaves the job green"
fresh
answers runs "$merge_group_runs"
refuses runs always "$runs_500"
run_started
check "the act ends green" "$(is "$status" 0)"
check "gh-read.sh made its three attempts" "$(is "$(reads runs)" 3)"
check "the warning names the route" \
  "$(contains "$out" '^::warning::GitHub refused GET repos/o/r/actions/workflows/ci.yaml/runs,')"
check "and the status gh named" "$(contains "$out" '^::warning::.*HTTP 500')"
check "the log holds gh's own words" "$(contains "$out" '^failed to get runs: HTTP 500')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

echo "case: a 404 at every attempt is no 5xx and fails the job as every refusal did"
fresh
answers runs "$merge_group_runs"
refuses runs always "$runs_404"
run_started
check "the act fails" "$(is "$status" 1)"
check "the error names the route and the status" \
  "$(contains "$out" '^::error::GitHub refused GET repos/o/r/actions/workflows/ci.yaml/runs,.*HTTP 404')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

echo "case: a force-cancel GitHub refuses warns and leaves the job green, as it did"
fresh
answers runs "$merge_group_runs"
refuses cancel always "$api_500"
run_started
check "the act ends green" "$(is "$status" 0)"
check "the warning names the run it could not cancel" \
  "$(contains "$out" "^::warning::force-cancel of run $replaced_run failed")"

echo "case: the cancel-dequeued act reads the removal and force-cancels the removed group's run"
fresh
answers timeline "$removal"
answers runs "$commit_runs"
run_dequeued
check "the act ends green" "$(is "$status" 0)"
check "it force-cancels the removed group's run" \
  "$(is "$(calls "runs/$removed_run/force-cancel")" 1)"
check "and names the removal it read" \
  "$(contains "$out" "left the queue at 2026-11-24T19:00:00Z for reason 'dequeued'")"

echo "case: a 500 on one timeline read is read again, and the act goes on"
fresh
answers timeline "$removal"
answers runs "$commit_runs"
refuses timeline 1 "$api_500"
run_dequeued
check "the act ends green" "$(is "$status" 0)"
check "it read the timeline twice" "$(is "$(reads timeline)" 2)"
check "and still force-cancels the removed group's run" \
  "$(is "$(calls "runs/$removed_run/force-cancel")" 1)"

echo "case: a 500 at all seven timeline reads warns and leaves the job green"
fresh
answers timeline "$removal"
answers runs "$commit_runs"
refuses timeline always "$api_500"
run_dequeued
check "the act ends green" "$(is "$status" 0)"
check "it made all seven reads" "$(is "$(reads timeline)" 7)"
check "the warning names them and the status" \
  "$(contains "$out" '^::warning::GitHub refused all 7 reads of pull request #5331.s timeline, each of them for a 5xx')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

echo "case: a 403 that is no rate limit at all seven timeline reads still fails the job"
fresh
answers timeline "$removal"
refuses timeline always "$api_403"
run_dequeued
check "the act fails" "$(is "$status" 1)"
check "the error names the reads and the status" \
  "$(contains "$out" '^::error::GitHub refused all 7 reads of pull request #5331.s timeline,.*HTTP 403')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

echo "case: a 500 at every read of the removed group's runs warns and leaves the job green"
fresh
answers timeline "$removal"
answers runs "$commit_runs"
refuses runs always "$runs_500"
run_dequeued
check "the act ends green" "$(is "$status" 0)"
check "gh-read.sh made its three attempts" "$(is "$(reads runs)" 3)"
check "the warning names the route and the status" \
  "$(contains "$out" '^::warning::GitHub refused GET repos/o/r/actions/workflows/ci.yaml/runs,.*HTTP 500')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

echo "case: a 404 of that run list is no 5xx and fails the job as every refusal did"
fresh
answers timeline "$removal"
answers runs "$commit_runs"
refuses runs always "$runs_404"
run_dequeued
check "the act fails" "$(is "$status" 1)"
check "the error names the route and the status" \
  "$(contains "$out" '^::error::GitHub refused GET repos/o/r/actions/workflows/ci.yaml/runs,.*HTTP 404')"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

summary "the two cancel acts' reads of GitHub"
