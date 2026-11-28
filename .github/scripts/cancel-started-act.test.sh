#!/usr/bin/env bash
# Tests for cancel-started-act.sh, which ci.yaml's cancel-started job runs over the merge-group
# runs the queue has replaced, and for what it does with a run list GitHub refuses: it reads the
# list through gh-read.sh's gh_read, so a refusal the next attempt gets past leaves it doing its
# work, and a list refused at every attempt still fails the step. Merge-group run 45467602844
# went red whole for one HTTP 500 of that list, in job 138706588845.
#
# Each case runs the real scripts with a stand-in `gh` first on PATH. It answers the two routes
# the act calls - `runs`, the merge-group run list, and `cancel`, a force-cancel - and logs every
# call, so a case counts the attempts the ladder made and the cancels the act made. A case says
# per route how many of its calls are refused and with which of gh's own words. GH_RETRY_SECONDS
# is 0, so the attempts are made without the waits between them, and every case runs under
# `timeout`, so an act that hung would fail its case instead of hanging the job.
#
# Run from anywhere: .github/scripts/cancel-started-act.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# cancel-started-act.sh runs .github/scripts/merge-group-cancels.sh by that path, so each case
# runs the act from the root of the tree the harness is in.
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

# What `gh run list` prints for the refusal of run 45467602844, job 138706588845, with this
# repository's name in place of that one's.
runs_500="failed to get runs: HTTP 500 (https://api.github.com/repos/o/r/actions/workflows/397779729/runs?event=merge_group&exclude_pull_requests=true&per_page=100)"

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

# run_started: the act, its stdout and stderr together in `out` (the job log holds both) and its
# exit status in `status`.
run_started() {
  set +e
  out=$(cd "$repo_root" && PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r \
    GH_RETRY_SECONDS=0 HEAD_BRANCH="$queue_branch" RUN_ID="$asking_run" \
    timeout 60 "$script_dir/cancel-started-act.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the act reads the run list and force-cancels the group the queue replaced"
fresh
answers runs "$merge_group_runs"
run_started
check "the act ends green" "$(is "$status" 0)"
check "it read the list once" "$(is "$(reads runs)" 1)"
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
check "the log holds gh's own words for the refused attempt" "$(contains "$out" '^failed to get runs: HTTP 500')"

echo "case: a 500 at every attempt still fails the step"
fresh
answers runs "$merge_group_runs"
refuses runs always "$runs_500"
run_started
check "the act fails" "$(is "$(test "$status" -ne 0 && echo failed || echo "$status")" failed)"
check "gh-read.sh made its three attempts" "$(is "$(reads runs)" 3)"
check "it cancels nothing" "$(is "$(calls force-cancel)" 0)"

summary "cancel-started-act.sh's read of the run list"
