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
# job publishes beside the outputs
# (.github/ci-notes/ci.md#the-paths-filters-own-verdict-for-every-filter-before-the-full-ci-term
# says why). It is what tells a job an event held back from one its paths never selected: a slow
# suite's output is written `false` on a pull request without the label full-ci or the job's own
# lane:<output> label, and an output CI_MERGE_GROUP_SKIPS names is written `false` on a merge
# group, whether the filter said yes or no, so the output alone reads the same in both cases. Only
# those two events hold a job back that way - on a dispatched run no term rewrites the filter's
# verdict, so this reading never fires there. The verdict itself does not move: such a job was
# not selected, and ci-ok accepts its skip exactly as before. `gated` marks it, so ci-ok.sh can
# name it apart from the jobs that ran: its paths chose it and it did not run at this head.
def selection($job; $map; $out; $pr; $event):
  $map[$job] as $output
  | ($out[$output] // "unset") as $value
  | ($pr | any(. == $job)) as $pr_only
  | (($out.filter_verdicts | try (fromjson | objects) catch empty) // {}) as $verdicts
  | if $value != "true" then
      if $event == "pull_request" and $verdicts[$output] == "true" then
        {selected: false, gated: true,
         why: "selected by paths, held back on a pull request without full-ci or lane:\($output): runs in the merge queue"}
      elif $event == "merge_group" and $verdicts[$output] == "true" then
        {selected: false, gated: true,
         why: "selected by paths, held out of the merge group by CI_MERGE_GROUP_SKIPS: package-local unit tests, which run on the pull request"}
      else {selected: false, why: "needs.changes.outputs.\($output) is \($value)"}
      end
    elif $pr_only and $event != "pull_request" then
      {selected: false, why: "it runs only on pull_request, and this run is \($event)"}
    else {selected: true,
          why: "needs.changes.outputs.\($output) is true\(if $pr_only then " on a \($event)" else "" end)"}
    end;
