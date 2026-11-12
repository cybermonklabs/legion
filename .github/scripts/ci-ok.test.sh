#!/usr/bin/env bash
# Tests for `ci-ok.sh`: ci-ok passes only when every job the always-run list names succeeded,
# every job `changes` selected succeeded, and every job it did not select succeeded or was
# skipped.
#
# ci-ok.sh reads only its environment, so each case hands it the NEEDS ci.yaml's ci-ok job builds
# with `toJSON(needs)`, which ci-ok-report.sh builds for its early post too,
# and the SELECTED_BY, PULL_REQUEST_ONLY and ALWAYS_RUN of ci.yaml's env, shrunk to four selected
# jobs: typecheck,
# envoy-client and docs, each selected by its own output, and worker-image, which also runs only
# on a pull request. The always-run list is ci.yaml's own `changes` and lint, and a third job,
# smoke-gate, for the cases that prove the list is read rather than hard-coded. Some cases pass
# another event than pull_request, which is what
# PULL_REQUEST_ONLY turns on; the script takes the event as a string and knows no workflow's
# triggers. docs also stands for one of the eight slow suites, whose output a pull request
# without the label `full-ci` holds false whatever the paths filter answered: three cases hand
# the run the filter's own verdicts beside the outputs (with_verdicts below) and hold the report
# to telling that skip from a filter miss.
#
# A job that ends `failure` is asked about: ci-ok.sh reads the check runs on this run's head to
# tell a job the platform killed from one the change broke (runner-shutdown.sh), so each case
# runs with a stand-in `gh` first on PATH, which answers that read and each check run's
# annotations from files the case writes.
#
# Run from anywhere: .github/scripts/ci-ok.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The stand-in GitHub: the check runs on the head this run's jobs are on, and the annotations of
# one of them, each answered as `gh api --jq` would print it. A missing check-runs file is the
# route refusing the read.
mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
case "\$*" in
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
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

# check_runs <job>[=<run id>]...: the check runs on the head, as the commit's check-runs route
# lists them, each job's own numbered in the order given and carrying the details_url of run 41
# unless the case gives it another run's. It clears the annotations with them, so a job of a run
# written this way carries none and its failure is read as a failure.
check_runs() {
  local spec name run n=0
  rm -f "$work/check-runs" "$work"/annotations.*
  for spec in "$@"; do
    name=${spec%%=*}
    run=${spec#*=}
    [ "$run" != "$spec" ] || run=41
    jq -nc --arg name "$name" --arg url "https://api.github.com/repos/o/r/check-runs/$n" \
      --arg details "https://github.com/o/r/actions/runs/$run/job/$n" \
      '{name: $name, url: $url, details_url: $details}' >> "$work/check-runs"
    n=$((n + 1))
  done
}

# annotations <job> <message>...: the failure annotations GitHub filed on that job's check run,
# which is where the runner's shutdown shows.
annotations() {
  local job=$1 id
  shift
  id=$(jq -r --arg job "$job" 'select(.name == $job) | .url | split("/")[-1]' "$work/check-runs")
  printf '%s\n' "$@" | jq -R '{annotation_level: "failure", message: .}' > "$work/annotations.$id"
}

# Every case but the last four runs over a head whose check runs carry no annotation at all, so
# nothing of it is read as a runner shutdown and a failure is a failure.
check_runs changes lint typecheck envoy-client worker-image docs smoke-gate contracts

selected_by='{"typecheck": "typecheck", "envoy-client": "envoy_client", "worker-image": "worker_image", "docs": "docs"}'
pull_request_only='["worker-image"]'
always_run='["changes", "lint"]'
# The same list with a third always-run job, which ci-ok.sh must judge as it judges lint.
three_always='["changes", "lint", "smoke-gate"]'

# needs <changes result> <changes outputs> <lint result> [<job>=<result>]...: the NEEDS
# ci-ok-report.sh hands ci-ok.sh, every other job with no outputs.
needs() {
  local changes=$1 outputs=$2 lint=$3 pair doc
  shift 3
  doc=$(jq -n --arg c "$changes" --argjson o "$outputs" --arg l "$lint" \
    '{changes: {result: $c, outputs: $o}, lint: {result: $l, outputs: {}}}')
  for pair in "$@"; do
    doc=$(jq --arg j "${pair%%=*}" --arg r "${pair#*=}" '.[$j] = {result: $r, outputs: {}}' <<<"$doc")
  done
  echo "$doc"
}

# The outputs of a `changes` that selected typecheck and worker-image and not envoy-client or docs.
selects='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}'
# The same with docs selected too.
selects_docs='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "true"}'
# The same outputs read on another event, docs unselected: ci-ok.sh takes EVENT as given.
push_selects='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}'

# `changes` publishes the paths filter's own verdict for every filter beside its outputs
# (ci.yaml's changes job, filter_verdicts), as the JSON text toJSON() gives. The eight slow
# suites' outputs carry a `full-ci` term in front of that verdict, so on a pull request without
# the label the output is false whatever the filter said; docs stands for one of the eight below.
with_verdicts() { # with_verdicts <outputs> <the filter's own verdicts>
  jq -nc --argjson outputs "$1" --argjson verdicts "$2" '$outputs + {filter_verdicts: ($verdicts | tojson)}'
}
# The filter said yes and the output is false: only the missing label can have done that.
gated_selects=$(with_verdicts "$selects" \
  '{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "true"}')
# The filter said no, so the label decided nothing and the output would be false with it too.
unmatched_selects=$(with_verdicts "$selects" \
  '{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}')

# run_ci_ok <needs> [<event>] [<always run>]: sets out and status. The event defaults to
# pull_request and the always-run list to ci.yaml's own two jobs. The GitHub half of the
# environment is the ci-ok job's own, pointed at the stand-in above: the head its check runs are
# on, the run they must belong to, and gh-read.sh's retry delay turned off.
run_ci_ok() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GH_RETRY_SECONDS=0 HEAD_SHA=abc123 NEEDS=$1 EVENT=${2:-pull_request} SELECTED_BY=$selected_by \
    PULL_REQUEST_ONLY=$pull_request_only ALWAYS_RUN=${3:-$always_run} "$script_dir/ci-ok.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a job changes selected ended skipped"
run_ci_ok "$(needs success "$selects" success typecheck=skipped envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names the job, its result and why" \
  "$(contains "$out" "^::error::typecheck ended skipped, but changes selected it (needs.changes.outputs.typecheck is true)")"
check "says what the skip means" \
  "$(contains "$out" "a selected job must succeed, and a skip here means the job's if: did not admit the run although changes chose it, a disagreement between the two the author must fix\$")"
check "prints its selection" "$(contains "$out" '^typecheck: skipped (selected: needs.changes.outputs.typecheck is true)$')"

echo "case: every selected job succeeded and the unselected ones were skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "passes" "$(is "$status" 0)"
check "prints every job's result" "$(contains "$out" '^changes: success$')"
check "and lint's" "$(contains "$out" '^lint: success$')"
check "and the selected job's selection" "$(contains "$out" '^typecheck: success (selected: needs.changes.outputs.typecheck is true)$')"
check "and the unselected job's" \
  "$(contains "$out" '^envoy-client: skipped (not selected: needs.changes.outputs.envoy_client is false)$')"
check "says why it passed" "$(contains "$out" 'changes and lint succeeded, every job changes selected succeeded')"
check "reports no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a job changes did not select, skipped, beside a selected one that succeeded"
run_ci_ok "$(needs success '{"typecheck": "true", "envoy_client": "false", "worker_image": "false", "docs": "false"}' success \
  typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "passes" "$(is "$status" 0)"
check "prints worker-image unselected" \
  "$(contains "$out" '^worker-image: skipped (not selected: needs.changes.outputs.worker_image is false)$')"

echo "case: a selected job was cancelled"
# A selected job reads `cancelled` when its own `timeout-minutes` cut it off, when the run was
# cancelled, or when a force-cancel ended it: none of the three is its filter's doing, so the
# error sends the author to the job's log and its limit rather than to its `if:`.
run_ci_ok "$(needs success "$selects" success typecheck=cancelled envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error::typecheck ended cancelled, but changes selected it')"
check "says it was cut off, and where to look" \
  "$(contains "$out" "a cancellation here is the job cut off rather than filtered out — its own timeout-minutes, a cancelled run or a force-cancel — so the job's log and its timeout-minutes say which, not its filter\$")"
check "does not call it a disagreement between the filter and the if:" \
  "$(is "$(contains "$out" 'did not admit the run although changes chose it')" false)"

echo "case: an unselected job failed"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=failure worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error::envoy-client ended failure; a job changes did not select must succeed or be skipped')"

echo "case: worker-image's output is true on a push, where its if: admits only pull_request"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" push
check "passes" "$(is "$status" 0)"
check "prints why it is not selected" \
  "$(contains "$out" '^worker-image: skipped (not selected: it runs only on pull_request, and this run is push)$')"

echo "case: worker-image's output is true on a pull request, and it was skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::worker-image ended skipped, but changes selected it (needs.changes.outputs.worker_image is true on a pull_request)")"

echo "case: changes' docs output is false and docs was skipped, on an event that is not a pull request"
run_ci_ok "$(needs success "$push_selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" push
check "passes" "$(is "$status" 0)"
check "prints why it is not selected" \
  "$(contains "$out" '^docs: skipped (not selected: needs.changes.outputs.docs is false)$')"
check "reports no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: docs' output is true on a pull request, and it was skipped"
run_ci_ok "$(needs success "$selects_docs" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::docs ended skipped, but changes selected it (needs.changes.outputs.docs is true)")"

echo "case: docs' output is true on a merge group, and it was skipped"
run_ci_ok "$(needs success "$selects_docs" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" merge_group
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::docs ended skipped, but changes selected it (needs.changes.outputs.docs is true)")"

echo "case: docs' paths matched and the pull request has no full-ci label, so changes held it back"
run_ci_ok "$(needs success "$gated_selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "passes, as it does for any job changes did not select" "$(is "$status" 0)"
check "says the label held it back, not the filter" \
  "$(contains "$out" '^docs: skipped (not selected: selected by paths, gated by full-ci: runs in the merge queue)$')"
check "reports no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: docs' paths did not match on a pull request, where the label would have changed nothing"
run_ci_ok "$(needs success "$unmatched_selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "passes" "$(is "$status" 0)"
check "keeps today's wording" \
  "$(contains "$out" '^docs: skipped (not selected: needs.changes.outputs.docs is false)$')"
check "says nothing about full-ci" "$(is "$(contains "$out" 'gated by full-ci')" false)"

echo "case: a merge-group run never says full-ci, since the label term of those outputs is already true there"
run_ci_ok "$(needs success "$gated_selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" merge_group
check "passes" "$(is "$status" 0)"
check "keeps today's wording" \
  "$(contains "$out" '^docs: skipped (not selected: needs.changes.outputs.docs is false)$')"
check "says nothing about full-ci" "$(is "$(contains "$out" 'gated by full-ci')" false)"

echo "case: changes failed"
run_ci_ok "$(needs failure '{}' success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it and why" "$(contains "$out" '^::error::changes ended failure; it selects the jobs from this head')"
check "prints the jobs it could not select" "$(contains "$out" '^envoy-client: skipped (not selected: needs.changes.outputs.envoy_client is unset)$')"

echo "case: lint was skipped"
run_ci_ok "$(needs success "$selects" skipped typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it and why" "$(contains "$out" '^::error::lint ended skipped; lint runs on every change')"

echo "case: a job SELECTED_BY does not name"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  contracts=skipped)"
check "fails" "$(is "$status" 1)"
check "names it, its result and why" "$(contains "$out" '^::error::contracts ended skipped, and ci-ok does not know what selects it')"
check "names the three classes it could join" \
  "$(contains "$out" "map it in CI_OK_SELECTED_BY in ci.yaml's env when a changes output gates it, name it in CI_OK_ALWAYS_RUN when it runs on every change, or name it in CI_OK_EXEMPT when it is outside ci-ok's accounting\$")"

echo "case: a job SELECTED_BY maps is absent from NEEDS"
run_ci_ok "$(needs success "$selects" success typecheck=success worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it as absent" "$(contains "$out" '^::error::envoy-client is absent from NEEDS')"
check "prints it" "$(contains "$out" '^envoy-client: absent ')"

echo "case: a job changes selected failed"
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails, so the ci-ok job's own check run carries the failure" "$(is "$status" 1)"
check "names the job, its result and why" \
  "$(contains "$out" '^::error::typecheck ended failure, but changes selected it (needs.changes.outputs.typecheck is true)')"
check "prints every other job's result" "$(contains "$out" '^worker-image: success (selected')"
check "says the job failed" \
  "$(contains "$out" 'a selected job must succeed, and this one failed: its log says what broke$')"

echo "case: a job the always-run list names succeeded"
# The class is ci.yaml's CI_OK_ALWAYS_RUN, not two names in the script: a third job of the list
# is judged as lint is, and the pass line names it with the other two.
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=success)" pull_request "$three_always"
check "passes" "$(is "$status" 0)"
check "prints its result with no selection" "$(contains "$out" '^smoke-gate: success$')"
check "names it in the pass line" \
  "$(contains "$out" '^changes, lint and smoke-gate succeeded, every job changes selected succeeded')"

echo "case: a job the always-run list names was skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=skipped)" pull_request "$three_always"
check "fails" "$(is "$status" 1)"
check "names it and why, as it does lint" \
  "$(contains "$out" '^::error::smoke-gate ended skipped; smoke-gate runs on every change, so ci-ok passes only when it succeeded$')"

echo "case: a job the always-run list names is absent from NEEDS"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)" \
  pull_request "$three_always"
check "fails" "$(is "$status" 1)"
check "prints it absent" "$(contains "$out" '^smoke-gate: absent$')"
check "names it and why" \
  "$(contains "$out" '^::error::smoke-gate ended absent; smoke-gate runs on every change')"

echo "case: a job the always-run list does not name, which ci-ok.sh judged by name before"
# smoke-gate is in no class while the list is ci.yaml's two: the verdict fails it with the three
# remedies rather than holding it to success.
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=success)"
check "fails" "$(is "$status" 1)"
check "names it and the three classes it could join" \
  "$(contains "$out" '^::error::smoke-gate ended success, and ci-ok does not know what selects it; map it in CI_OK_SELECTED_BY')"

echo "case: a selected job whose runner was shut down under it"
# GitHub reports such a job `failure`, and the runner's two ##[error] lines are the annotations
# on its check run (runner-shutdown.sh holds the reading). ci-ok reads it as cancelled: the
# verdict still fails, since a selected job that did not succeed fails whichever it is, and the
# error names the shutdown rather than the change or the job's timeout-minutes.
check_runs changes lint typecheck envoy-client worker-image docs
annotations typecheck \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "reads it as cancelled" \
  "$(contains "$out" '^typecheck: cancelled (selected: needs.changes.outputs.typecheck is true)$')"
check "names the job and that changes selected it" \
  "$(contains "$out" '^::error::typecheck ended cancelled, but changes selected it (needs.changes.outputs.typecheck is true)')"
check "says a runner shutdown cut it off, and what was read" \
  "$(contains "$out" 'a runner shutdown cut this one off: its check run carries the runner.s .has received a shutdown signal. annotation and exit code 143')"
check "blames the platform rather than the change" \
  "$(is "$(contains "$out" 'this one failed: its log says what broke')" false)"
check "does not send the author to the job's timeout-minutes" \
  "$(is "$(contains "$out" 'its own timeout-minutes, a cancelled run or a force-cancel')" false)"
check "tells the author to re-run the job, since a pull request's run can be re-run" \
  "$(contains "$out" 'rather than this change breaking anything, and re-running the job is the whole of the remedy$')"

echo "case: a selected job whose runner was shut down, on a merge-group run"
# A re-run is no remedy there: ci.yaml's "Refuse a re-run attempt of a merge-group run" step
# fails ci-ok on every attempt above the first of a merge-group run, so the author adds the pull
# request to the queue again and a new group tests it.
check_runs changes lint typecheck envoy-client worker-image docs
annotations typecheck \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=skipped docs=skipped)" merge_group
check "fails" "$(is "$status" 1)"
check "reads it as cancelled" \
  "$(contains "$out" '^typecheck: cancelled (selected: needs.changes.outputs.typecheck is true)$')"
check "sends the author to the queue rather than to a re-run" \
  "$(contains "$out" "adding the pull request to the merge queue again is the whole of the remedy: ci.yaml's .Refuse a re-run attempt of a merge-group run. step fails ci-ok on every attempt above the first, so re-running this run cannot pass\$")"
check "does not tell the author to re-run the job" \
  "$(is "$(contains "$out" 're-running the job is the whole of the remedy')" false)"

echo "case: a selected job that failed of itself"
# The one thing this reading must not do is take a real failure for a platform kill: the job's
# own error is what its check run carries, so the result and the error are today's.
check_runs changes lint typecheck envoy-client worker-image docs
annotations typecheck 'Process completed with exit code 1.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "keeps it a failure" \
  "$(contains "$out" '^typecheck: failure (selected: needs.changes.outputs.typecheck is true)$')"
check "says the job failed" \
  "$(contains "$out" 'a selected job must succeed, and this one failed: its log says what broke$')"
check "says nothing of a runner shutdown" "$(is "$(contains "$out" 'runner shutdown')" false)"

echo "case: the shutdown annotation with another exit code"
# Both facts are the reading: the message alone is not a platform kill, since a job can log that
# line and still fail of itself.
check_runs changes lint typecheck envoy-client worker-image docs
annotations typecheck \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 1.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "keeps it a failure" \
  "$(contains "$out" '^typecheck: failure (selected: needs.changes.outputs.typecheck is true)$')"

echo "case: a failure of the change followed by the shutdown"
# A step failed and GitHub filed `Process completed with exit code 1.`; the job then ran its
# `if: always()` ci-ok-report step, as every job of ci.yaml does, and the runner was taken away
# during that step, so the same check run carries the runner's two lines as well. The exit code
# that is not 143 is the change's own fault, and the job stays a failure.
check_runs changes lint typecheck envoy-client worker-image docs
annotations typecheck \
  'Process completed with exit code 1.' \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "keeps it a failure" \
  "$(contains "$out" '^typecheck: failure (selected: needs.changes.outputs.typecheck is true)$')"
check "says the job failed" \
  "$(contains "$out" 'a selected job must succeed, and this one failed: its log says what broke$')"
check "says nothing of a runner shutdown" "$(is "$(contains "$out" 'runner shutdown')" false)"

echo "case: another run's check run of the same name carries the shutdown"
# A commit carries every workflow's check runs and every attempt's, so the run id in the
# details_url is what makes the mapping this run's: a shutdown on another run's typecheck is
# nothing to do with this one.
check_runs changes lint envoy-client worker-image docs typecheck=99
annotations typecheck \
  'The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.' \
  'Process completed with exit code 143.'
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "keeps it a failure" \
  "$(contains "$out" '^typecheck: failure (selected: needs.changes.outputs.typecheck is true)$')"

echo "case: GitHub refuses the check runs of this head"
# The read is tried three times and the verdict is reached without it: a read that did not
# happen is no evidence that the platform killed anything, so the failure stands as a failure
# and the warning says why the two could not be told apart.
rm -f "$work/check-runs"
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "keeps it a failure" \
  "$(contains "$out" '^typecheck: failure (selected: needs.changes.outputs.typecheck is true)$')"
check "warns that it could not tell a shutdown from a failure" \
  "$(contains "$out" '::warning::GitHub refused the check runs on abc123 three times')"

summary "ci-ok.sh fails unless every job changes selected succeeded"
