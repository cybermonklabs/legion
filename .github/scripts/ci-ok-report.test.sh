#!/usr/bin/env bash
# Tests for `ci-ok-report.sh`, the `post-check-run.sh` it posts with, and `ci-ok-hold.sh`, which
# holds the ci-ok job's failing verdict of a group behind the front: the failing ci-ok of a
# merge-group run goes up from the job whose own result decides it, the moment that job ends,
# naming that job in the title and every other job at the result the jobs route shows; every
# other run posts nothing, because ci.yaml's ci-ok job carries the verdict there; a failure only
# a runner shutdown caused posts nothing either, since the platform killed the job and decided
# nothing of the group; a group behind the front of the queue, whose base is not the tip of the
# queue's branch, posts nothing and leaves its failing verdict to the ci-ok job, since the queue
# cannot take it out before the groups in front of it end; a jobs list GitHub refuses still
# posts the verdict, where a refused post fails the step; and the ci-ok job's hold lets a failing
# verdict go up the moment the group is the front and not before, waits through the tips of the
# groups in front of it, never lets a group built on a broken entry conclude while that entry
# stands, and lets the verdict go up at once on every refusal.
#
# Each case writes the run's jobs as the jobs route lists them and the `changes` and `job.status`
# the reporting job saw, and runs the real scripts with a stand-in `gh` first on PATH: it answers
# the jobs read and the read of the queue branch's tip (one tip, or a sequence of them, one per
# read), records each check run posted, and logs every call it gets, so a case can count the
# GitHub calls a run makes. The map is ci-ok.test.sh's four jobs and the always-run list ci.yaml's
# own two, in a ci.yaml of the case's own (CI_OK_WORKFLOW); one case reads the real ci.yaml, and
# one reads the hold step out of it.
#
# Run from anywhere: .github/scripts/ci-ok-report.test.sh
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
echo "\$*" >> "$work/calls"
case "\$*" in
  *"repos/o/r/git/ref/heads/main"*)
    # A tips file answers one line per read, its first, and keeps its last line for every read
    # after: the queue's branch moving under a step that reads it more than once.
    if [ -e "$work/tips" ]; then
      head -n 1 "$work/tips"
      if [ "\$(wc -l < "$work/tips")" -gt 1 ]; then
        tail -n +2 "$work/tips" > "$work/tips.next"
        mv "$work/tips.next" "$work/tips"
      fi
      exit 0
    fi
    if [ ! -e "$work/tip" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/tip" ;;
  *"repos/o/r/git/ref/heads/release"*)
    cat "$work/tip-release" ;;
  *"/actions/runs/41/jobs?per_page=100"*)
    if [ ! -e "$work/jobs" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/jobs" ;;
  *"/commits/abc123/check-runs"*)
    if [ ! -e "$work/check-runs" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/check-runs" ;;
  *"/check-runs/"*"/annotations"*)
    id=
    for arg in "\$@"; do
      case \$arg in
        *"/check-runs/"*"/annotations"*)
          id=\${arg#*/check-runs/}
          id=\${id%%/*} ;;
      esac
    done
    if [ -e "$work/annotations.\$id" ]; then
      jq -r 'select(.annotation_level == "failure") | .message' "$work/annotations.\$id"
    fi ;;
  *"--method POST repos/o/r/check-runs"*)
    # post-check-run.sh sends the body as a file, so that a retried post can send it again.
    n=\$(find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l)
    body=
    while [ \$# -gt 0 ]; do
      case \$1 in --input) body=\$2 ;; esac
      shift
    done
    cp "\$body" "$work/posted.\$n.json"
    if [ -e "$work/post-fails" ]; then echo "HTTP 403" >&2; exit 1; fi
    echo "https://github.com/o/r/runs/\$n" ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

cat > "$work/ci.yaml" <<'YAML'
env:
  CI_OK_SELECTED_BY: |
    {"typecheck": "typecheck", "envoy-client": "envoy_client", "worker-image": "worker_image", "docs": "docs"}
  CI_OK_PULL_REQUEST_ONLY: '["worker-image"]'
  CI_OK_ALWAYS_RUN: '["changes", "lint"]'
YAML

# What `changes` gave the reporting job on a merge group: typecheck and envoy-client selected,
# worker-image held back by its pull_request-only `if:`.
selects='{"result": "success", "outputs": {"typecheck": "true", "envoy_client": "true", "worker_image": "false", "docs": "false"}}'

# jobs <name>=<status>[/<conclusion>]...: the run's jobs as the jobs route lists them, one JSON
# object per line, as `gh api --jq '.jobs[] | …'` prints them, and the check run each of them
# has on the head, as the commit's check-runs route lists them. The run's first case writes
# none, which is both routes refusing the read. A job written here carries no annotation, so its
# failure is read as a failure until a case writes the runner's with `annotations`. The queue's
# branch is at base123, the base run_report hands the script, so the group is the front of the
# queue until a case calls `behind` or `tip_refused`.
jobs() {
  local pair n=0
  rm -f "$work"/posted.*.json "$work/post-fails" "$work/jobs" "$work/check-runs" "$work"/annotations.* \
    "$work/calls" "$work/tips"
  echo base123 > "$work/tip"
  for pair in "$@"; do
    jq -nc --arg name "${pair%%=*}" --arg status "${pair#*=}" \
      '{name: $name, status: ($status | split("/")[0]), conclusion: ($status | split("/")[1])}' \
      >> "$work/jobs"
    jq -nc --arg name "${pair%%=*}" --arg url "https://api.github.com/repos/o/r/check-runs/$n" \
      --arg details "https://github.com/o/r/actions/runs/41/job/$n" \
      '{name: $name, url: $url, details_url: $details}' >> "$work/check-runs"
    n=$((n + 1))
  done
}

# behind: the queue's branch is at another commit than the group's base, so a group stands in
# front of this one. tip_refused: GitHub refuses the read of the branch's tip.
behind() { echo other456 > "$work/tip"; }
tip_refused() { rm -f "$work/tip"; }

# calls <pattern>: how many of the run's GitHub calls carried the pattern.
calls() {
  if [ ! -e "$work/calls" ]; then echo 0; return; fi
  grep -c -- "$1" "$work/calls" || true
}

# annotations <job> <message>...: the failure annotations GitHub filed on that job's check run,
# which is where the runner's shutdown shows (runner-shutdown.sh).
annotations() {
  local job=$1 id
  shift
  id=$(jq -r --arg job "$job" 'select(.name == $job) | .url | split("/")[-1]' "$work/check-runs")
  printf '%s\n' "$@" | jq -R '{annotation_level: "failure", message: .}' > "$work/annotations.$id"
}

# run_report <changes> <event> <job status> [<workflow>]: sets out and status. The group's base is
# base123 on the queue's main, unless the case sets base_sha or base_ref.
run_report() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_SERVER_URL=https://github.com GH_RETRY_SECONDS=0 JOB=typecheck CHANGES=$1 \
    EVENT=$2 JOB_STATUS=$3 HEAD_SHA=abc123 BASE_SHA=${base_sha-base123} \
    BASE_REF=${base_ref-refs/heads/main} CI_OK_WORKFLOW=${4:-$work/ci.yaml} \
    "$script_dir/ci-ok-report.sh" 2>&1)
  status=$?
  set -e
}

# verdict <typecheck result>: sets verdict_status to the exit status of ci-ok.sh, the ci-ok job's
# verdict at the end of a run, over this case's results: `changes` as $selects, lint and
# envoy-client succeeded, and typecheck at the result given.
verdict() {
  local needs
  needs=$(jq -nc --argjson changes "$selects" --arg typecheck "$1" \
    '{changes: $changes, lint: {result: "success"}, typecheck: {result: $typecheck},
      "envoy-client": {result: "success"}, "worker-image": {result: "skipped"}, docs: {result: "skipped"}}')
  set +e
  PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 GH_RETRY_SECONDS=0 \
    HEAD_SHA=abc123 EVENT=merge_group NEEDS=$needs \
    SELECTED_BY="$(yq -r '.env.CI_OK_SELECTED_BY' "$work/ci.yaml")" \
    PULL_REQUEST_ONLY="$(yq -r '.env.CI_OK_PULL_REQUEST_ONLY' "$work/ci.yaml")" \
    ALWAYS_RUN="$(yq -r '.env.CI_OK_ALWAYS_RUN' "$work/ci.yaml")" \
    "$script_dir/ci-ok.sh" > "$work/verdict.out" 2>&1
  verdict_status=$?
  set -e
}

posted() { find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l | tr -d ' '; }
field() { jq -r "$1" "$work/posted.0.json"; }

echo "case: a merge_group run whose reporting job failed posts without waiting"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts one check run" "$(is "$(posted)" 1)"
check "named ci-ok" "$(is "$(field .name)" ci-ok)"
check "on the head" "$(is "$(field .head_sha)" abc123)"
check "completed" "$(is "$(field .status)" completed)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "linking the run" "$(is "$(field .details_url)" https://github.com/o/r/actions/runs/41)"
check "marked as a ci-ok a job posted, which ci-ok-reconcile.sh reads" \
  "$(is "$(field .external_id)" ci-ok-post:41)"
check "records the reporting job at its own job.status, not the status the route shows it at" \
  "$(contains "$(field .output.summary)" '^typecheck: failure (selected')"
check "records a running job at its live status" \
  "$(contains "$(field .output.summary)" '^lint: in_progress$')"
check "records a queued job at its live status" \
  "$(contains "$(field .output.summary)" '^envoy-client: queued (selected')"
check "records a job the route does not list as unlisted" \
  "$(contains "$(field .output.summary)" '^docs: unlisted (not selected')"
check "says the failing job decided it" \
  "$(contains "$(field .output.summary)" 'Decided by typecheck, whose failure decides this merge-group run')"
check "says the verdict is not the ci-ok job's" \
  "$(contains "$(field .output.summary)" 'rather than from the ci-ok job at the end of the run')"
check "titles it with the job whose result decided the run" \
  "$(contains "$(field .output.title)" '^typecheck decided this merge-group run: ')"
check "says nothing of a runner shutdown, since this job's check run carries no annotation" \
  "$(is "$(contains "$out" 'runner was shut down')" false)"
check "reads the tip of the queue's branch once, which is the group's base: the front" \
  "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 1)"

echo "case: the same failure in a group behind the front of the queue"
# The queue's branch is at another commit than this group's base, so a group stands in front of
# it. The queue takes a failed entry out only once every group in front of it has ended, and it
# counts a group whose ci-ok failed as no longer building, so an early post here would only make
# it build one more group on this one (on 2026-11-22, 41 groups in 28 minutes behind one broken
# entry). So nothing is posted, the group stays building until its run ends, and the run's
# own ci-ok job posts the failing verdict. Every input but the tip is the first case's, which
# posts: the tip alone makes the difference.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
behind
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says the group stands behind the front, naming its base and the branch's tip" \
  "$(contains "$out" '^typecheck ended failure (changes ended success) on a merge-group run whose group stands behind the front of the queue: its base base123 is not other456, the tip of refs/heads/main')"
check "leaves the verdict to the ci-ok job, which holds it until the group is the front" \
  "$(contains "$out" 'the ci-ok job reaches this run.s verdict at the end of the run and holds a failing one until the group is the front$')"
check "makes one GitHub call, the read of the tip" "$(is "$(calls .)" 1)"
check "reads no job list" "$(is "$(calls 'actions/runs/41/jobs')" 0)"
check "reads no check runs for a runner shutdown" "$(is "$(calls 'check-runs')" 0)"
# What the group still gets: the ci-ok job's verdict at the end of the run, ci-ok.sh over every
# job's result, fails it on the same failure, and passes the same run with that job green.
verdict failure
check "the ci-ok job's verdict at the end of the run still fails the group" \
  "$(is "$verdict_status" 1)"
check "on the failed job" \
  "$(contains "$(cat "$work/verdict.out")" '^::error::typecheck ended failure, but changes selected it')"
verdict success
check "and passes it when that job succeeded (control)" "$(is "$verdict_status" 0)"

echo "case: a cancellation in a group behind the front of the queue"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
behind
run_report "$selects" merge_group cancelled
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says the group stands behind the front" \
  "$(contains "$out" '^typecheck ended cancelled (changes ended success) on a merge-group run whose group stands behind the front')"

echo "case: a failed changes in a group behind the front of the queue"
jobs typecheck=in_progress lint=in_progress
behind
run_report '{"result": "failure", "outputs": {}}' merge_group success
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says the group stands behind the front" \
  "$(contains "$out" '^typecheck ended success (changes ended failure) on a merge-group run whose group stands behind the front')"

echo "case: a changes its time limit cancelled, in a group behind the front of the queue"
jobs typecheck=in_progress lint=in_progress
behind
run_report '{"result": "cancelled", "outputs": {}}' merge_group success
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says the group stands behind the front" \
  "$(contains "$out" '^typecheck ended success (changes ended cancelled) on a merge-group run whose group stands behind the front')"

echo "case: a failure whose group's place in the queue GitHub will not tell"
# The tip is read three times, then the group is treated as the front and the post goes up, as
# every other refusal here prefers the failing verdict: a front group with no early post would
# hold the queue until the end of its run.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
tip_refused
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts its failing ci-ok" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "tried the tip three times" "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 3)"
check "warns why it posted" \
  "$(contains "$out" '::warning::GitHub refused the tip of refs/heads/main three times')"

echo "case: a group of a queue on another branch reads that branch's tip, not main's"
# Main's tip is the group's base here, so a read of main would call the group the front. The
# queue's own branch is elsewhere, so a group stands in front of it and nothing goes up.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
echo other789 > "$work/tip-release"
base_ref=refs/heads/release run_report "$selects" merge_group failure
check "posts nothing" "$(is "$(posted)" 0)"
check "reads the release branch's tip" "$(is "$(calls 'repos/o/r/git/ref/heads/release')" 1)"
check "and not main's" "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 0)"
check "names that branch" "$(contains "$out" 'is not other789, the tip of refs/heads/release')"
# Control: with the release branch at the group's base, the group is that queue's front.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
echo base123 > "$work/tip-release"
base_ref=refs/heads/release run_report "$selects" merge_group failure
check "posts at once when that branch is at the group's base (control)" "$(is "$(posted)" 1)"

echo "case: a merge_group run without its group's base"
jobs typecheck=in_progress
base_sha='' run_report "$selects" merge_group failure
check "fails the step" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"
check "names the input" \
  "$(contains "$out" 'ci-ok-report.sh needs BASE_SHA in its environment on a merge_group run')"

echo "case: a merge_group run whose reporting job's runner was shut down under it"
# GitHub reports such a job `failure`, so without the annotations on its check run this would be
# the case above: the early post that takes the pull request out of the queue. The platform
# killed the job, which decides nothing about the group, so nothing is posted and the run's own
# ci-ok job reaches the verdict at the end of it (runner-shutdown.sh, and ci-ok.sh reads such a
# job as cancelled there).
jobs typecheck=in_progress lint=in_progress envoy-client=queued
annotations typecheck \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says the runner was shut down and what was read" \
  "$(contains "$out" "^typecheck ended failure on this merge-group run only because its runner was shut down: its check run carries the runner's .has received a shutdown signal. annotation and exit code 143")"
check "leaves this run's verdict to the ci-ok job" \
  "$(contains "$out" 'the ci-ok job posts this run.s verdict at the end of the run, where that job reads as cancelled')"

echo "case: a merge_group run whose changes failed only on a runner shutdown"
# `changes` is the job that reaches this step in practice: the steps after a kill are skipped,
# so the killed job's own report step rarely runs, while the job reporting after it was handed
# that result in `toJSON(needs.changes)`.
jobs typecheck=in_progress lint=in_progress changes=completed/failure
annotations changes \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_report '{"result": "failure", "outputs": {}}' merge_group success
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "names changes as the job the platform killed" \
  "$(contains "$out" '^changes ended failure on this merge-group run only because its runner was shut down')"

echo "case: a merge_group run whose reporting job its timeout-minutes cancelled"
# `cancelled` is what a job cut off by its `timeout-minutes` reports (measured on probe run
# 41869007039), and it decides the group as a failure does. The queue takes the pull request out
# on the verdict this job posts and dequeued.yaml force-cancels the run, and a force-cancel leaves
# every `if: always()` step skipped, so no later job of the run reports and the ci-ok job never
# runs: this reporting job is the run's last chance to post ci-ok, and a group that gets none sits
# in the queue until GitHub's 180-minute check timeout removes it.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group cancelled
check "exits 0" "$(is "$status" 0)"
check "posts one check run rather than waiting" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "says the cancellation decided it" \
  "$(contains "$(field .output.summary)" 'whose cancellation decides this merge-group run')"

echo "case: a merge_group run whose changes failed"
jobs typecheck=in_progress lint=in_progress
run_report '{"result": "failure", "outputs": {}}' merge_group success
check "posts although this job succeeded" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "titled with changes, the job whose result decided the run" \
  "$(contains "$(field .output.title)" '^changes decided this merge-group run: ')"
check "titled with changes' failure" "$(contains "$(field .output.title)" 'changes ended failure')"

echo "case: a merge_group run whose changes its own time limit cancelled"
# `changes` carries no report step and reports nothing of its own, and its time limit cuts it off
# as `cancelled`, which decides the group as a failure does.
jobs typecheck=in_progress lint=in_progress
run_report '{"result": "cancelled", "outputs": {}}' merge_group success
check "posts one check run rather than waiting" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "says the cancelled changes decided it" \
  "$(contains "$(field .output.summary)" 'reporting after changes was cancelled, which decides this merge-group run')"

echo "case: a merge_group run this job decides nothing of"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group success
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "leaves the verdict to the ci-ok job" \
  "$(contains "$out" 'typecheck ended success on a merge_group run, which this job decides nothing of; the ci-ok job posts this run.s verdict')"
check "reads no tip, since nothing is posted whatever the group's place" \
  "$(is "$(calls 'git/ref')" 0)"

echo "case: the same failure on a pull_request run"
# No queue holds a pull request, and its run is cancelled by the next push, so nothing is posted
# early there: the ci-ok job fails on this job's result at the end of the run.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" pull_request failure
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "reads no job list either" "$(is "$(contains "$out" 'HTTP 502')" false)"
check "makes no GitHub call at all" "$(is "$(calls .)" 0)"

echo "case: the same failure on a dispatched run"
jobs typecheck=in_progress
run_report "$selects" workflow_dispatch failure
check "posts nothing" "$(is "$(posted)" 0)"

echo "case: a decided merge_group run whose reads GitHub refuses posts its verdict all the same"
# The jobs route is read three times before the script gives up, and so are the check runs it
# would tell a runner shutdown by. On a decided merge-group run nothing this job waits for can
# help: the queue takes the pull request out on this verdict and
# dequeued.yaml force-cancels the run, and a force-cancel leaves every `if: always()` step
# skipped. A run that waited for a read it could not make would carry no ci-ok at all and hold the
# queue until GitHub's 180-minute check timeout dropped its pull request. So every other job is
# recorded `unread`, which ci-ok.sh fails on, and the verdict goes up: nothing worse can come of
# posting a failing verdict on a group that is about to be thrown away. A failure this script
# could not read is a failure for the same reason — a read that did not happen is no evidence
# that the platform killed anything.
jobs
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts its failing ci-ok rather than waiting on a read it cannot make" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "records every other job as unread" "$(contains "$(field .output.summary)" '^lint: unread$')"
check "and the reporting job at its own status" \
  "$(contains "$(field .output.summary)" '^typecheck: failure (selected')"
check "warns why it posted on an unread run" \
  "$(contains "$out" "::warning::GitHub refused this run's job list at every attempt gh-read.sh made")"
check "warns that it could not tell a runner shutdown from a failure either" \
  "$(contains "$out" '::warning::GitHub refused the check runs on abc123 at every attempt gh-read.sh made')"

echo "case: GitHub refuses the post"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
touch "$work/post-fails"
run_report "$selects" merge_group failure
check "fails the step" "$(is "$status" 1)"
check "tried three times" "$(is "$(posted)" 3)"
check "says so" "$(contains "$out" 'GitHub refused the ci-ok check run on abc123 at every attempt gh-read.sh made')"

echo "case: CHANGES is not toJSON(needs.changes)"
jobs typecheck=in_progress
run_report '{}' merge_group failure
check "fails" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"

echo "case: the real ci.yaml's map, with nothing selected"
jobs typecheck=in_progress
run_report '{"result": "success", "outputs": {}}' merge_group failure "$script_dir/../workflows/ci.yaml"
check "reads it and posts" "$(is "$(posted)" 1)"
check "failing on the job that decided the run" "$(is "$(field .conclusion)" failure)"

echo "case: post-check-run.sh called with no marker"
# The marker's prefix is what tells a posted check run from a job's own check run of the same
# name, whose own external_id is a UUID, and ci-ok-reconcile.sh PATCHes only the check runs
# carrying it, so a post that set none would leave a check run no later attempt could reconcile.
jobs
: > "$work/summary.md"
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GH_RETRY_SECONDS=0 \
  HEAD_SHA=abc123 CHECK_NAME=pr-title CONCLUSION=success TITLE='the title is conventional' \
  SUMMARY_FILE="$work/summary.md" DETAILS_URL=https://github.com/o/r/actions/runs/41 \
  "$script_dir/post-check-run.sh" 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"
check "names the input" \
  "$(contains "$out" 'post-check-run.sh needs EXTERNAL_ID in its environment')"

echo "case: post-check-run.sh posts the marker it was given"
# As ci.yaml's lint job calls it for pr-title, the other check run posted this way.
jobs
: > "$work/summary.md"
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GH_RETRY_SECONDS=0 \
  HEAD_SHA=abc123 CHECK_NAME=pr-title CONCLUSION=success TITLE='the title is conventional' \
  SUMMARY_FILE="$work/summary.md" DETAILS_URL=https://github.com/o/r/actions/runs/41 \
  EXTERNAL_ID=pr-title:41 "$script_dir/post-check-run.sh" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "posts one check run" "$(is "$(posted)" 1)"
check "carrying that external_id" "$(is "$(field .external_id)" pr-title:41)"

# run_hold [<bound>]: sets out and status to what ci-ok-hold.sh, the ci-ok job's hold step, did
# for the group whose base is base123 (or base_sha) on the queue's main, with a bound of <bound>
# seconds (3600 when not given) and a pause of `poll` seconds between reads (0 when unset). A
# hold still running after 60 s is killed and its status is timeout's 124, so a hold that never
# ends fails the checks of its case rather than hanging the harness.
run_hold() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GH_RETRY_SECONDS=0 \
    BASE_SHA=${base_sha-base123} BASE_REF=refs/heads/main CI_OK_HOLD_SECONDS=${1:-3600} \
    CI_OK_HOLD_POLL_SECONDS=${poll-0} timeout 60 "$script_dir/ci-ok-hold.sh" 2>&1)
  status=$?
  set -e
}

# tips <sha>...: main's tip at each read, one per read, the last for every read after it.
tips() { printf '%s\n' "$@" > "$work/tips"; }

# front_line: the line the hold prints when it lets the failing verdict go up at the front.
front_line='^This group is the front of the queue after [0-9]* s: refs/heads/main is at its base'

echo "case: a failing verdict of a group behind the front goes up once the group is the front"
# The group's test job failed while two groups stood in front of it, as #4820's did (run
# 45095618270): the job's report step posts nothing, and the ci-ok job's verdict fails at the end
# of the run. The hold reads main at the tips of the groups in front as they merge, and lets the
# job end, which is when the queue reads its failing ci-ok, at the read that finds main at the
# group's base and not one read sooner.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
behind
run_report "$selects" merge_group failure
check "the failing job's report step posts nothing behind the front" "$(is "$(posted)" 0)"
tips other456 other456 other789 base123
rm -f "$work/calls"
run_hold
check "exits 0, so the job ends on its failing verdict" "$(is "$status" 0)"
check "read main four times: the hold ended at the read that found the group's base" \
  "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 4)"
check "says the group stood behind the first group in front" \
  "$(contains "$out" '^This group stands behind the front of the queue after [0-9]* s: its base base123 is not other456, the tip of refs/heads/main. The failing ci-ok waits')"
check "and behind the next, once the first merged" \
  "$(contains "$out" 'its base base123 is not other789, the tip of refs/heads/main')"
check "says once per tip, not once per read" \
  "$(is "$(grep -c 'stands behind the front' <<<"$out")" 2)"
check "ends on the line that says the group is the front" \
  "$(contains "$(tail -n 1 <<<"$out")" "$front_line")"
check "posts no check run of its own: the job's own check run is the verdict" "$(is "$(posted)" 0)"
check "warns of nothing" "$(is "$(contains "$out" '::warning::')" false)"
# Control: the same hold for a group that is the front already lets the verdict go up at once.
jobs
tips base123
run_hold
check "lets the verdict of the front group go up at the first read (control)" \
  "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 1)"
check "saying it is the front" "$(contains "$out" "$front_line")"

echo "case: a group behind the front whose entries ahead fail never lets its verdict go up early"
# A group in front of this one fails: the queue takes it out once it is the front and rebuilds
# every group behind it, this one too, and the rebuilt group's cancel-started force-cancels this
# run. Main never reaches this group's base: it stays where it was and then moves to the tip of a
# rebuilt group. Until the force-cancel, nothing of this run says failure: a later job's failure
# posts nothing, and the hold waits. Its bound, 4 s here with a pause of 1 s, is all that ends
# it, for a run nothing cancelled, and it says so. The bound leaves room for a second read on a
# loaded machine, where one read can take a second.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
tips other456 other789
run_report "$selects" merge_group failure
check "a failing job posts nothing while a group in front builds" "$(is "$(posted)" 0)"
run_report "$selects" merge_group failure
check "nor once main has moved to a rebuilt group's tip" "$(is "$(posted)" 0)"
check "which is not this group's base" "$(contains "$out" 'its base base123 is not other789')"
rm -f "$work/calls"
poll=1 run_hold 4
check "the hold exits 0 at its bound" "$(is "$status" 0)"
check "never says the group is the front" "$(is "$(contains "$out" "$front_line")" false)"
check "read main more than once before the bound" \
  "$(is "$(($(calls 'repos/o/r/git/ref/heads/main') > 1))" 1)"
check "warns that the bound, not the front, ended it" \
  "$(contains "$out" '^::warning::This group still stands behind the front of the queue after [0-9]* s, and the next read would pass this step.s bound of 4 s')"

echo "case: one entry that breaks every group built on it builds no group more"
# The 2026-11-22 cascade: #4812's entry broke every group built on it, and each group's failing
# ci-ok made the queue count it as no longer building and build another on it, 41 groups in 28
# minutes. Here g1 carries the broken entry, behind a front group f whose base is f000; g2 is
# built on g1 (base g1head) and g3 on g2 (base g2head); every one of the three fails. The queue
# builds a group whenever one of them concludes failure while it stands behind the front, so the
# count of such conclusions is the count of groups built on the broken entry: none, while f
# builds and after g1 is the front. g1's verdict goes up at the read that finds main at its base,
# where the queue takes it out and rebuilds g2 and g3, whose rebuilt groups' cancel-started ends
# their runs. Their holds stand for those runs: a hold that ends before its bound, at any tip but
# its own base, is a verdict concluded behind the front.
early=0
for group in g1:base123 g2:g1head g3:g2head; do
  jobs typecheck=in_progress lint=in_progress envoy-client=queued
  echo f000 > "$work/tip"
  base_sha=${group#*:} run_report "$selects" merge_group failure
  early=$((early + $(posted)))
done
check "no group's failing job posts while f builds" "$(is "$early" 0)"
jobs
tips f000 f000 f000 base123
base_sha=base123 run_hold
check "g1's hold ends at the read that finds main at g1's base" \
  "$(is "$(calls 'repos/o/r/git/ref/heads/main') $(contains "$out" "$front_line")" '4 true')"
for group in g2:g1head g3:g2head; do
  jobs
  tips f000 base123
  poll=1 base_sha=${group#*:} run_hold 4
  if [ "$(contains "$out" 'the next read would pass this step.s bound of 4 s')" = false ]; then
    early=$((early + 1))
  fi
  check "${group%%:*}'s hold waits on past main at g1's base, to its bound" \
    "$(contains "$out" "its base ${group#*:} is not base123, the tip of refs/heads/main")"
done
check "no group concluded behind the front, so the queue built none on the broken entry" \
  "$(is "$early" 0)"

echo "case: every refusal of the hold lets the failing verdict go up at once"
# A hold that cannot read where its group stands would otherwise keep a failing group building
# until its bound; the ci-ok steps prefer the failing verdict on every refusal. The bound here
# is 4 s, so a hold that read on past a refusal would read main more than three times.
jobs
tip_refused
poll=1 run_hold 4
check "a tip GitHub refuses: exits 0" "$(is "$status" 0)"
check "after the three attempts gh-read.sh makes" "$(is "$(calls 'repos/o/r/git/ref/heads/main')" 3)"
check "and warns why the verdict went up" \
  "$(contains "$out" '^::warning::GitHub refused the tip of refs/heads/main at every attempt gh-read.sh made')"
jobs
base_sha='' run_hold
check "a missing input: fails the step, which ends the job on its verdict" "$(is "$status" 1)"
check "names the input" \
  "$(contains "$out" 'ci-ok-hold.sh needs BASE_SHA in its environment; the failing ci-ok goes up now')"
check "and reads nothing" "$(is "$(calls .)" 0)"
jobs
run_hold 30m
check "a bound that is no number of seconds: fails the step" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "ci-ok-hold.sh's CI_OK_HOLD_SECONDS is not a whole number of seconds: 30m")"

echo "case: the real ci.yaml's ci-ok job holds a failing verdict of a merge group's first attempt"
# The hold is the step between the verdict and the reconcile step, under failure(): a passing
# verdict never waits, and on a re-run attempt the refusal is the verdict of a tree the queue
# no longer waits on. It reads the group's base and branch from the merge_group event, its bound
# leaves its last read inside the step's own limit, and the job can read the branch's tip.
ci_ok_job=$(yq -o=json '.jobs["ci-ok"]' "$script_dir/../workflows/ci.yaml")
step_index() { jq --arg name "$1" '[.steps[].name] | index($name)' <<<"$ci_ok_job"; }
hold_at=$(step_index 'Hold a failing verdict until this group is the front of the queue')
hold=$(jq --argjson i "$hold_at" '.steps[$i]' <<<"$ci_ok_job")
check "comes after the verdict step" \
  "$(is "$((hold_at > $(step_index 'Judge every job of this run')))" 1)"
check "and before the reconcile step, so nothing on the head says failure while it waits" \
  "$(is "$((hold_at < $(step_index "Set every other ci-ok on this head to this job's conclusion")))" 1)"
check "runs only after a failure, on a merge group's first attempt" \
  "$(is "$(jq -r .if <<<"$hold")" "\${{ failure() && github.event_name == 'merge_group' && github.run_attempt == '1' }}")"
check "runs ci-ok-hold.sh" "$(is "$(jq -r .run <<<"$hold")" .github/scripts/ci-ok-hold.sh)"
check "with the group's base and the queue's branch" \
  "$(is "$(jq -r '.env.BASE_SHA + " " + .env.BASE_REF' <<<"$hold")" \
    "\${{ github.event.merge_group.base_sha }} \${{ github.event.merge_group.base_ref }}")"
bound=$(jq -r .env.CI_OK_HOLD_SECONDS <<<"$hold")
check "a bound inside the step's own limit, with a pause and a read to spare" \
  "$(is "$((bound + 60 < $(jq -r '."timeout-minutes"' <<<"$hold") * 60))" 1)"
check "and a job that may read the branch's tip" \
  "$(is "$(jq -r '.permissions.contents' <<<"$ci_ok_job")" read)"
jobs
tips base123
run_hold "$bound"
check "a hold with that bound lets the front group's verdict go up" \
  "$(is "$status $(contains "$out" "$front_line")" '0 true')"

summary "ci-ok-report.sh posts the failing ci-ok of a merge-group run one job's result decided"
