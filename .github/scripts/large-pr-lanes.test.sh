#!/usr/bin/env bash
# Tests for `large-pr-lanes.sh`, the three halves of the approval-time run of a large pull request
# (.github/ci-notes/ci.md#a-large-pull-request-runs-its-slow-suites-once-at-the-approved-head):
#
#   label  puts large-pr on a pull request of 100 changed files or 10,000 changed lines or more,
#          from the pull_request event's payload, takes it off one that is no longer large, writes
#          nothing when the label already says the right thing, passes a fork's pull request over,
#          reads the size from the pull request when the payload leaves it out, and fails the step
#          when GitHub refuses the label a large pull request needs.
#   lead   runs the suites unless an older run of the workflow at the same head has a ci-ok
#          still open or passed, which it follows; a run of another workflow, a newer run and a
#          failed one are no reason to follow, and a refused read runs the suites.
#   hold   the job named ci-ok of large-pr-lanes.yaml concludes exactly as `approval / ci-ok`
#          does, waits while that job is absent or still running, reads an earlier attempt's
#          verdict only when nothing else re-ran, and fails a run that ends with no verdict; when
#          `approval` is skipped it concludes as the ci-ok of the run it follows, or fails when no
#          run is left to follow; and it fails when the caller never starts.
#
# Each case runs the real script with a stand-in `gh` first on PATH, which answers each read of
# the jobs and runs routes from a JSON answer through the script's own `--jq`, as gh does, and
# records each write it is sent.
#
# Run from anywhere: .github/scripts/large-pr-lanes.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >> "$work/calls"
filter=.
args=("\$@")
for ((i = 0; i < \${#args[@]}; i++)); do [ "\${args[i]}" = --jq ] && filter=\${args[i + 1]}; done
case "\$*" in
  *"--method POST repos/o/r/issues/7/labels -f labels[]=large-pr"*)
    if [ -e "$work/write-fails" ]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
    echo added >> "$work/writes" ;;
  *"--method DELETE repos/o/r/issues/7/labels/large-pr"*)
    if [ -e "$work/write-fails" ]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
    echo removed >> "$work/writes" ;;
  "api repos/o/r/pulls/7 "*)
    cat "$work/pull-size" ;;
  *"repos/o/r/actions/runs?event=pull_request_review&head_sha=abc&per_page=100"*)
    if [ -e "$work/runs-fail" ]; then echo "HTTP 502" >&2; exit 1; fi
    jq -r "\$filter" "$work/head-runs.json" ;;
  *"repos/o/r/actions/runs/"*"/jobs?filter=latest&per_page=100"*)
    # One answer per read of each run, reads-<run>/1, /2, ...; past the last, the last again.
    all="\$*"
    run=\${all#*actions/runs/}
    run=\${run%%/jobs*}
    n=\$(( \$(cat "$work/read-count-\$run" 2>/dev/null || echo 0) + 1 ))
    echo "\$n" > "$work/read-count-\$run"
    if [ -e "$work/reads-\$run/fail.\$n" ]; then echo "HTTP 502" >&2; exit 1; fi
    last=\$(find "$work/reads-\$run" -name '[0-9]*' | wc -l)
    [ "\$n" -le "\$last" ] || n=\$last
    jq -r "\$filter" "$work/reads-\$run/\$n" ;;
  # `.github/scripts/gh-read.sh#gh_wait_reset` asks the rate_limit route whether a command that
  # failed with 1 spent the budget: both budgets have calls left, so a refusal here is the
  # command's own.
  "api rate_limit "*)
    jq -r "\$filter" <<<'{"resources": {"core": {"remaining": 5000, "reset": 0}, "graphql": {"remaining": 5000, "reset": 0}}}' ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

reset() {
  rm -rf "$work/calls" "$work/writes" "$work/write-fails" "$work/pull-size" "$work"/reads-* \
    "$work"/read-count-* "$work/runs-fail" "$work/output"
  printf '{"workflow_runs": []}\n' > "$work/head-runs.json"
}
writes() { [ -e "$work/writes" ] && tr '\n' ' ' < "$work/writes" || echo ""; }
call_count() { [ -e "$work/calls" ] && wc -l < "$work/calls" | tr -d ' ' || echo 0; }

# event <changed files> <additions> <deletions> [<label>...]: a pull_request payload of #7 from
# o/r; a size given as `-` is left out of the payload, as GitHub can.
event() {
  local changed=$1 additions=$2 deletions=$3
  shift 3
  jq -n --arg c "$changed" --arg a "$additions" --arg d "$deletions" --args \
    '{pull_request: ({number: 7, head: {repo: {full_name: "o/r"}},
                      labels: [$ARGS.positional[] | {name: .}]}
                     + (if $c == "-" then {} else {changed_files: ($c | tonumber)} end)
                     + (if $a == "-" then {} else {additions: ($a | tonumber)} end)
                     + (if $d == "-" then {} else {deletions: ($d | tonumber)} end))}' \
    "$@" > "$work/event.json"
}

run_label() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_EVENT_PATH="$work/event.json" \
    GH_RETRY_SECONDS=0 "$script_dir/large-pr-lanes.sh" label 2>&1)
  status=$?
  set -e
}

echo "case: 100 changed files is large"
reset; event 100 50 50
run_label
check "exits 0" "$(is "$status" 0)"
check "labels it" "$(is "$(writes)" "added ")"
check "says why" "$(contains "$out" '^#7 is large, 100 changed files and 100 changed lines (large is 100 files or 10000 lines): labelled large-pr')"

echo "case: 99 changed files and 9,999 lines is not"
reset; event 99 9000 999
run_label
check "exits 0" "$(is "$status" 0)"
check "writes nothing" "$(is "$(call_count)" 0)"
check "says so" "$(contains "$out" '^#7 is not large, 99 changed files and 9999 changed lines')"

echo "case: 10,000 changed lines in one file is large"
reset; event 1 6000 4000
run_label
check "labels it" "$(is "$(writes)" "added ")"

echo "case: a large pull request that carries the label already"
reset; event 300 1 1 full-ci LARGE-PR
run_label
check "exits 0" "$(is "$status" 0)"
check "writes nothing, reading the label without regard to case as GitHub does" "$(is "$(call_count)" 0)"
check "says so" "$(contains "$out" 'carries large-pr already$')"

echo "case: a pull request that shrank below both lines"
reset; event 3 10 10 large-pr
run_label
check "exits 0" "$(is "$status" 0)"
check "takes the label off" "$(is "$(writes)" "removed ")"

echo "case: the payload leaves the size out"
reset; event - - -
printf '20000\t0\t4\n' > "$work/pull-size"
run_label
check "reads it from the pull request" "$(contains "$(cat "$work/calls")" '^api repos/o/r/pulls/7 ')"
check "and labels by it" "$(is "$(writes)" "added ")"

echo "case: a fork's pull request"
reset; event 500 1 1
jq '.pull_request.head.repo.full_name = "someone/r"' "$work/event.json" > "$work/fork.json"
mv "$work/fork.json" "$work/event.json"
run_label
check "exits 0" "$(is "$status" 0)"
check "calls nothing, since its token cannot write a label" "$(is "$(call_count)" 0)"
check "says so" "$(contains "$out" '^#7 comes from someone/r, a fork')"

echo "case: GitHub refuses the label a large pull request needs"
reset; event 150 1 1
touch "$work/write-fails"
run_label
check "fails the step" "$(is "$status" 1)"
check "tried the write three times" "$(is "$(grep -c -e '--method POST repos/o/r/issues/7/labels' "$work/calls")" 3)"
check "says what that costs" "$(contains "$out" '::error::GitHub refused the label large-pr on #7 at every attempt gh-read.sh made, so an approval of it would run no slow suite before the merge queue')"

echo "case: GitHub refuses to take the label off"
reset; event 2 1 1 large-pr
touch "$work/write-fails"
run_label
check "only warns, since a label left on blocks nothing" "$(is "$status" 0)"
check "names the cost" "$(contains "$out" '::warning::GitHub refused to take large-pr off #7')"

echo "case: a payload with no pull request"
reset; printf '{"review": {}}\n' > "$work/event.json"
run_label
check "fails the step" "$(is "$status" 1)"
check "says what it reads" "$(contains "$out" 'large-pr-lanes.sh label reads a pull_request event')"

# reads_of <run> <answer>...: the answers of successive reads of that run's jobs, each a list of
# rows name|status|conclusion|attempt separated by `;`, written as the JSON GitHub answers.
# reads <answer>...: the same for this run, 41.
reads_of() {
  local run=$1 n=0 answer row name status conclusion attempt
  shift
  mkdir -p "$work/reads-$run"
  for answer in "$@"; do
    n=$((n + 1))
    IFS=';' read -ra rows <<<"$answer"
    for row in "${rows[@]}"; do
      [ -n "$row" ] || continue
      IFS='|' read -r name status conclusion attempt <<<"$row"
      jq -n --arg name "$name" --arg status "$status" --arg conclusion "$conclusion" \
        --arg attempt "${attempt:-1}" --arg url "https://github.com/o/r/actions/runs/$run/job/$n" \
        '{name: $name, status: $status, conclusion: (if $conclusion == "-" then null else $conclusion end),
          run_attempt: ($attempt | tonumber), html_url: $url}'
    done | jq -s '{jobs: .}' > "$work/reads-$run/$n"
  done
}
reads() { reads_of 41 "$@"; }
# head_runs <run>[=<path>]...: the runs at the head, as the runs route lists them; a run's path
# is large-pr-lanes.yaml's unless one is given.
head_runs() {
  local spec
  for spec in "$@"; do
    if [ "$spec" = "${spec%%=*}" ]; then spec="$spec=.github/workflows/large-pr-lanes.yaml"; fi
    jq -n --arg id "${spec%%=*}" --arg path "${spec#*=}" '{id: ($id | tonumber), path: $path}'
  done | jq -s '{workflow_runs: .}' > "$work/head-runs.json"
}

run_hold() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 HEAD_SHA=abc \
    GITHUB_RUN_ATTEMPT=${1:-1} CALLER=approval LARGE_PR_HOLD_POLL_SECONDS=0 GH_RETRY_SECONDS=0 \
    LARGE_PR_HOLD_START_SECONDS=${START:-3600} timeout 30 "$script_dir/large-pr-lanes.sh" hold 2>&1)
  status=$?
  set -e
}
run_lead() {
  set +e
  : > "$work/output"
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 HEAD_SHA=abc \
    GITHUB_OUTPUT="$work/output" GH_RETRY_SECONDS=0 "$script_dir/large-pr-lanes.sh" lead 2>&1)
  status=$?
  set -e
}
reads_made() { cat "$work/read-count-${1:-41}"; }
output() { tr '\n' ' ' < "$work/output"; }

echo "case: lead with no earlier approval-time run at the head"
reset
run_lead
check "exits 0" "$(is "$status" 0)"
check "runs the suites" "$(is "$(output)" "run=true ")"

echo "case: lead with an earlier run whose ci-ok is open"
reset
head_runs 40
reads_of 40 "ci-ok|in_progress|-|1;approval / changes|in_progress|-|1"
run_lead
check "follows it" "$(is "$(output)" "run=false ")"
check "says so" "$(contains "$out" '^run 40, an approval-time run at abc, is open or passed: this approval follows it')"

echo "case: lead with an earlier run that passed, after one that failed"
reset
head_runs 38 39
reads_of 38 "ci-ok|completed|failure|1"
reads_of 39 "ci-ok|completed|success|1"
run_lead
check "follows the one that passed" "$(is "$(output)" "run=false ")"
check "and says which" "$(contains "$out" '^run 39, an approval-time run at abc, is open or passed')"

echo "case: lead with an earlier run whose ci-ok is still queued"
reset
head_runs 40
reads_of 40 "ci-ok|queued|-|1"
run_lead
check "follows it: a queued ci-ok is open" "$(is "$(output)" "run=false ")"

echo "case: lead with only a failed earlier run, a newer run and another workflow's run"
reset
head_runs 38 42 37=.github/workflows/ci.yaml
reads_of 38 "ci-ok|completed|failure|1"
reads_of 42 "ci-ok|in_progress|-|1"
reads_of 37 "ci-ok|in_progress|-|1"
run_lead
check "runs the suites" "$(is "$(output)" "run=true ")"
check "and read no run but 38" "$(is "$(grep -c -e 'actions/runs/37/' -e 'actions/runs/42/' "$work/calls")" 0)"

echo "case: lead whose read GitHub refuses"
reset
touch "$work/runs-fail"
run_lead
check "runs the suites rather than follow nothing" "$(is "$(output)" "run=true ")"
check "and warns" "$(contains "$out" '::warning::GitHub refused the approval-time runs at abc')"

self='ci-ok|in_progress|-|1'
lead_done='lead|completed|success|1'
echo "case: the verdict passes after the run's jobs end"
reset
reads "$self;$lead_done;approval / changes|in_progress|-|1" \
  "$self;$lead_done;approval / changes|completed|success|1;approval / pi-legion / pi-legion|in_progress|-|1" \
  "$self;$lead_done;approval / changes|completed|success|1;approval / pi-legion / pi-legion|completed|success|1;approval / ci-ok|queued|-|1" \
  "$self;$lead_done;approval / changes|completed|success|1;approval / pi-legion / pi-legion|completed|success|1;approval / ci-ok|completed|success|1"
run_hold
check "exits 0" "$(is "$status" 0)"
check "waited through the absent and the queued verdict" "$(is "$(reads_made)" 4)"
check "names the verdict" "$(contains "$out" '^approval / ci-ok concluded success: https://github.com/o/r/actions/runs/41/job/4$')"

echo "case: the verdict fails"
reset
reads "$self;approval / pi-legion / pi-legion|completed|failure|1;approval / ci-ok|completed|failure|1"
run_hold
check "fails, so the head's ci-ok is red" "$(is "$status" 1)"
check "says why" "$(contains "$out" '::error::approval / ci-ok concluded failure, so this ci-ok fails')"

echo "case: the verdict is cancelled"
reset
reads "$self;approval / ci-ok|completed|cancelled|1"
run_hold
check "fails: only success passes" "$(is "$status" 1)"

echo "case: a job named ci-ok elsewhere in the run, or a verdict named for another caller"
reset
reads "$self;other / ci-ok|completed|success|1;approval / lint / lint|in_progress|-|1" \
  "$self;other / ci-ok|completed|success|1;approval / lint / lint|completed|success|1;approval / ci-ok|completed|failure|1"
run_hold
check "reads only approval / ci-ok" "$(is "$status" 1)"
check "after waiting for it" "$(is "$(reads_made)" 2)"

echo "case: attempt 2 that re-ran failed jobs, while attempt 1's verdict is still the newest listed"
reset
reads "ci-ok|in_progress|-|2;approval / pi-legion / pi-legion|in_progress|-|2;approval / ci-ok|completed|failure|1" \
  "ci-ok|in_progress|-|2;approval / pi-legion / pi-legion|completed|success|2;approval / ci-ok|completed|success|2"
run_hold 2
check "waits for attempt 2's verdict" "$(is "$(reads_made)" 2)"
check "and concludes as it did" "$(is "$status" 0)"

echo "case: attempt 2 that re-ran this job alone"
reset
reads "ci-ok|in_progress|-|2;approval / pi-legion / pi-legion|completed|success|1;approval / ci-ok|completed|success|1"
run_hold 2
check "reads attempt 1's verdict, which nothing re-ran" "$(is "$status" 0)"
check "at once" "$(is "$(reads_made)" 1)"

echo "case: the run ends with no verdict"
reset
reads "$self;approval / changes|completed|failure|1;approval / lint / lint|completed|skipped|1"
run_hold
check "fails rather than hold the head pending to its timeout" "$(is "$status" 1)"
check "after three reads in a row" "$(is "$(reads_made)" 3)"
check "says why" "$(contains "$out" 'no job of approval is open and approval / ci-ok has not concluded on attempt 1, 3 reads in a row')"

echo "case: a moment with every job ended and the verdict not yet created"
reset
reads "$self;approval / changes|completed|success|1" "$self;approval / changes|completed|success|1" \
  "$self;approval / changes|completed|success|1;approval / ci-ok|completed|success|1"
run_hold
check "is waited through" "$(is "$status" 0)"

echo "case: lead still open, then the caller starts"
# START=0, so the start clock fails the moment it runs: a hold that started it while lead was
# queued or running would fail at once rather than reach the third read. The case so needs no
# second of wall time to pass between the reads.
reset
reads "$self;lead|queued|-|1" "$self;lead|in_progress|-|1" \
  "$self;$lead_done;approval / changes|completed|success|1;approval / ci-ok|completed|success|1"
START=0 run_hold
check "is waited for: the start clock runs only once lead has ended" "$(is "$status" 0)"
check "through every read" "$(is "$(reads_made)" 3)"

echo "case: the caller never starts"
# A caller cancelled before it listed a job, or one waiting behind another approval-time run of
# its merge commit in ci.yaml's concurrency group, lists neither its jobs nor its skip.
reset
reads "$self;$lead_done"
START=0 run_hold
check "fails at once rather than hold the head pending to its timeout" "$(is "$status" 1)"
check "says why" "$(contains "$out" '::error::no job of approval is listed 0 s after lead ended, neither its jobs nor its skip, so this ci-ok fails: the caller was cancelled, or it waits behind another approval-time run')"
check "that a waiting caller runs once the other ends, and what to re-run when" \
  "$(contains "$out" "keeps it waiting rather than cancelling it and starts it, running every suite, once that run ends. Once this whole run has ended, re-run its failed jobs (gh run rerun 41 --failed)")"

follower="$self;$lead_done;approval|completed|skipped|1"
echo "case: the caller is skipped, and the run it follows passes"
reset
head_runs 39 40
reads_of 39 "ci-ok|completed|failure|1"
reads_of 40 "ci-ok|in_progress|-|1" "ci-ok|in_progress|-|1" "ci-ok|completed|success|1"
reads "$follower"
run_hold
check "exits 0" "$(is "$status" 0)"
check "follows the one open, not the one that failed" "$(contains "$out" '^following run 40, the approval-time run at abc$')"
check "and concludes as its ci-ok" "$(contains "$out" '^the ci-ok of run 40 concluded success: https://github.com/o/r/actions/runs/40/job/3$')"
check "reading the run it chose, not choosing again" "$(is "$(reads_made 40)" 3)"

echo "case: the caller is skipped, and the run it follows is still queued"
reset
head_runs 40
reads_of 40 "ci-ok|queued|-|1" "ci-ok|completed|success|1"
reads "$follower"
run_hold
check "follows it, as lead does" "$(contains "$out" '^following run 40, the approval-time run at abc$')"
check "and concludes as its ci-ok" "$(is "$status" 0)"

echo "case: the caller is skipped, and the run it follows fails"
reset
head_runs 40
reads_of 40 "ci-ok|in_progress|-|1" "ci-ok|completed|failure|1"
reads "$follower"
run_hold
check "fails" "$(is "$status" 1)"
check "naming the run" "$(contains "$out" '::error::the ci-ok of run 40 concluded failure, so this ci-ok fails')"
check "and the order to re-run in: the run it followed, then this one" \
  "$(contains "$out" "re-run run 40's failed jobs first (gh run rerun 40 --failed), and then this run's (gh run rerun 41 --failed)")"

echo "case: the caller is skipped, and no run at the head is open or passed any more"
reset
# 38 ran the suites and failed, 39 followed it and failed with it, 40 was a review that ran no
# ci-ok (its job skipped under another name).
head_runs 38 39 40
reads_of 38 "ci-ok|completed|failure|1"
reads_of 39 "ci-ok|completed|failure|1"
reads_of 40 ""
reads "$follower"
run_hold
check "fails" "$(is "$status" 1)"
check "says what to re-run, one run to each command, oldest first, then this one" \
  "$(contains "$out" 'Re-run the failed jobs of each, oldest first, the first before any other, since each later one follows the earliest open or passed: gh run rerun 38 --failed; gh run rerun 39 --failed; then this run.s, gh run rerun 41 --failed')"
check "and names no run that ran no ci-ok" "$(is "$(contains "$out" 'rerun 40')" false)"
check "and no command of two runs, which gh refuses" "$(is "$(contains "$out" 'gh run rerun [0-9][0-9]* [0-9]')" false)"

echo "case: GitHub refuses a read"
reset
reads "$self;approval / ci-ok|completed|success|1"
touch "$work/reads-41/fail.1" "$work/reads-41/fail.2" "$work/reads-41/fail.3"
run_hold
check "warns and reads again" "$(contains "$out" "::warning::GitHub refused run 41's jobs at every attempt gh-read.sh made")"
check "then concludes" "$(is "$status" 0)"

echo "case: an input the step did not set"
reset
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 GITHUB_RUN_ATTEMPT=1 HEAD_SHA=abc \
  "$script_dir/large-pr-lanes.sh" hold 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names the input" "$(contains "$out" 'large-pr-lanes.sh hold needs CALLER in its environment')"

echo "case: no mode"
set +e
out=$("$script_dir/large-pr-lanes.sh" 2>&1)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"

summary "large-pr-lanes.sh labels a large pull request, runs or follows its approval-time run, and holds ci-ok to that run's verdict"
