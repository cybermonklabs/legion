# ci-ok's one reading of whether `changes` selected a job, so no reading of it disagrees with
# another.
#
# `selection` is read by ci-ok.sh, the verdict ci.yaml's ci-ok job reaches over every job's result
# and ci-ok-report.sh posts at once on a merge-group run one job's failure has decided. $map is
# SELECTED_BY (job to the `changes` output its `if:` tests), $out is `changes`' outputs, $pr is
# PULL_REQUEST_ONLY and $event is `github.event_name`. A job is selected when its output is 'true'
# and, for a PULL_REQUEST_ONLY job, the run is a pull_request run; `why` says which. ci-ok.sh
# reads this file with `jq -L .github/scripts 'include "ci-ok"; …'`.
#
# `$out.filter_verdicts` is the paths filter's own answer for every filter, which the `changes`
# job publishes beside the outputs (ci.yaml's `changes` job says why). It is what tells a job
# the `full-ci` label held back from one its paths never selected: the nine slow suites' outputs
# are written `false` on a pull request without that label whether the filter said yes or no, so
# the output alone reads the same in both cases. Only a pull request can be gated that way - on
# every other event the label term of those expressions is already true, so the output is the
# filter's verdict and this reading never fires. The verdict itself does not move: such a job
# was not selected, and ci-ok accepts its skip exactly as before.
def selection($job; $map; $out; $pr; $event):
  $map[$job] as $output
  | ($out[$output] // "unset") as $value
  | ($pr | any(. == $job)) as $pr_only
  | ((($out.filter_verdicts // "{}") | try (fromjson | objects) catch empty) // {}) as $verdicts
  | if $value != "true" then
      if $event == "pull_request" and $verdicts[$output] == "true" then
        {selected: false, why: "selected by paths, gated by full-ci: runs in the merge queue"}
      else {selected: false, why: "needs.changes.outputs.\($output) is \($value)"}
      end
    elif $pr_only and $event != "pull_request" then
      {selected: false, why: "it runs only on pull_request, and this run is \($event)"}
    else {selected: true,
          why: "needs.changes.outputs.\($output) is true\(if $pr_only then " on a \($event)" else "" end)"}
    end;
