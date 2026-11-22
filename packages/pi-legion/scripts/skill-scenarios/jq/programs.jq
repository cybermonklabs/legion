# The programs routes run over their answers, each a function of this module: a route's `program`
# names one (`include "programs"; <name>`, github.jq's program), and gh-standin.ts runs it with
# `-L` at this directory, with the call's flags as gh read them as $flags (by long name), its
# request's query as $query (decoded, a GET's fields among them), whether the call carried --json
# as $json, and the moment of the call as $now. A program reads every flag and parameter it is
# handed or fails, which the stand-in refuses as unmodeled. They share opt, the value of a flag of
# the call by its long name; reads, which fails on a query parameter the program does not read;
# and paged, the page of a REST list the query asks for, `per_page` long (30 unless it says) and
# the `page`-th (the first unless it says), so a page past the end is empty, as GitHub answers one
# (measured on cybermonklabs/legion, 2026-11-18: `pulls?state=all&per_page=100&page=99999`
# answers `[]`, and `actions/runs?…&page=2` past its runs answers
# `{"total_count":3,"workflow_runs":[]}`, the count of the whole list).
def opt($long): $flags[$long];
def reads($names): ($query | keys - $names) as $other
  | if $other == [] then . else error("a parameter the rig does not read: " + ($other | join(","))) end;
def paged: (($query.per_page // "30") | tonumber) as $per | (($query.page // "1") | tonumber) as $page
  | .[($page - 1) * $per:$page * $per];

# gh pr list as gh 2.98.0 prints it: its filters (--state, open unless it says; --head; --base;
# --limit) over the one pull request of the world, and to no terminal number, title, branch,
# state and creation, a tab between each, or the fields --json names.
def pr_list_program:
(opt("--state") // "open") as $state | opt("--head") as $head
| opt("--base") as $base | (opt("--limit") // "30" | tonumber) as $limit
| [.] | map(select(($state == "open" or $state == "all")
    and ($head == null or .headRefName == $head) and ($base == null or .baseRefName == $base)))
| .[:$limit]
| if $json then .
  else map([(.number|tostring), .title, .headRefName, .state, .createdAt] | join("\t"))
       | join("\n") end;

# gh pr view with no --json as gh 2.98.0 prints it to no terminal (measured): a line per field of
# the pull request, empty for those the world holds none of, its author by the login GraphQL
# gives a bot, and its body after `--`. Its reviewers line once the role has sent a review is
# not measured, so a view after a review is refused (the program fails, gh-standin.ts).
def pr_view_program:
if (.reviews | length) > 0 then error("no measured reviewers line")
else "title:\t\(.title)\nstate:\t\(.state)\nauthor:\t\(.author.login | ltrimstr("app/"))\n"
  + "labels:\t\nassignees:\t\nreviewers:\t\nprojects:\t\nmilestone:\t\n"
  + "number:\t\(.number)\nurl:\t\(.url)\nadditions:\t\(.additions)\n"
  + "deletions:\t\(.deletions)\nauto-merge:\tdisabled\n--\n\(.body)" end;

# The REST list of the pull requests, filtered by its query as GitHub filters it: `state` (open
# unless it says), `head` (owner:branch), `base`, and its page; any other parameter is not read.
def pulls_query_program:
reads(["state", "head", "base", "per_page", "page"])
| map(select((($query.state // "open") == "open" or $query.state == "all")
    and ($query.head == null or .head.label == $query.head)
    and ($query.base == null or .base.ref == $query.base)))
| paged;

# gh run list, each run in the shape its --json answers it, filtered by its flags as gh asks
# GitHub to filter them (--status, --workflow, --commit, --branch, --event, --limit, 20 unless it
# says), and printed as gh 2.98.0 prints the list to no terminal: status, conclusion, title,
# workflow, branch, event, id, elapsed and creation, a tab between each. A list with no run prints
# nothing and exits 0 there (gh's NoResultsError, list.go:149-150 and ghcmd cmd.go:213-219 at
# v2.98.0, read from its source: requests.log lines 55 and 61). --workflow names a workflow as gh
# resolves it (pkg/cmd/workflow/shared/shared.go, FindWorkflow, requests.log line 57): by its file
# name, by its id, or by its name in any case; each record carries its workflow's file
# (`workflowFile`, which no --json field names).
# A file or an id no run of the world holds, which GitHub answers with a 404 the rig has not
# measured, is not modeled; a name none holds is answered by the route ahead of this one. The time
# of a run still running is the time since it started, as gh prints it (measured on
# cybermonklabs/legion run 44586483868, 2026-11-19: ip-runlist and ip-runlist-json, requests.log
# lines 29 and 31). Each label and line is in github-answers/README.txt, with its request.
def run_list_program:
def seconds: sub("\\..*Z$"; "Z") | fromdate;
def elapsed: ((if .status == "completed" then .updatedAt | seconds else $now end)
              - (.startedAt | seconds)) as $s
  | (($s / 3600) | floor) as $h | ((($s % 3600) / 60) | floor) as $m
  | (if $h > 0 then "\($h)h" else "" end)
    + (if $h > 0 or $m > 0 then "\($m)m" else "" end) + "\($s % 60)s";
opt("--status") as $status | opt("--workflow") as $workflow
| opt("--commit") as $commit | opt("--branch") as $branch
| opt("--event") as $event | (opt("--limit") // "20" | tonumber) as $limit
| (map(.workflowFile) | unique) as $files | (map(.workflowDatabaseId | tostring) | unique) as $ids
| if $workflow != null and ($workflow | test("\\.ya?ml$"; "i")) and ($files | index($workflow)) == null
  then error("a workflow file no run of the world names")
  elif $workflow != null and ($workflow | test("^[0-9]+$")) and ($ids | index($workflow)) == null
  then error("a workflow id no run of the world names")
  else . end
| def picks: if $workflow == null then true
    elif ($workflow | test("\\.ya?ml$"; "i")) then .workflowFile == $workflow
    elif ($workflow | test("^[0-9]+$")) then (.workflowDatabaseId | tostring) == $workflow
    else (.workflowName | ascii_downcase) == ($workflow | ascii_downcase) end;
  [.[] | select(($status == null or .status == $status or .conclusion == $status)
    and picks
    and ($commit == null or .headSha == $commit)
    and ($branch == null or .headBranch == $branch)
    and ($event == null or .event == $event))] | .[:$limit]
| if $json then .
  else map([.status, .conclusion, .displayTitle, .workflowName, .headBranch, .event,
            (.databaseId|tostring), elapsed, .createdAt] | join("\t")) | join("\n") end;

# The REST list of the runs of the repository, filtered by its query as GitHub filters it
# (`head_sha`, `branch`, `event`, `status`), with no pull request in any run where it asks
# `exclude_pull_requests=true` (measured on cybermonklabs/legion, 2026-11-18: a pull_request run
# that lists 1 lists 0 then), and its page, the count of the whole list beside it. Any other
# parameter (`actor`, `created`, `check_suite_id`, …) is not read.
def runs_query_program:
reads(["head_sha", "branch", "event", "status", "exclude_pull_requests", "per_page", "page"])
| .workflow_runs |= map(select(($query.head_sha == null or .head_sha == $query.head_sha)
    and ($query.branch == null or .head_branch == $query.branch)
    and ($query.event == null or .event == $query.event)
    and ($query.status == null or .status == $query.status or .conclusion == $query.status))
  | if $query.exclude_pull_requests == "true" then .pull_requests = [] else . end)
| .total_count = (.workflow_runs | length)
| .workflow_runs |= paged;

# The check runs of a commit, as GitHub answers its check-runs endpoint (measured on
# cybermonklabs/legion 05e64382, whose CI run has two attempts, 2026-11-18, github-answers/
# README.txt): with no `filter`, or any value but `all`, the latest check run of each name in each
# check suite, by its completion (GitHub's default, `latest`: 64 check runs, one per name,
# `ci-ok` its attempt-2 success alone, the re-run's check runs staying in their run's suite; and
# per suite, not per name across the commit, measured by the reviewer on b789479b99, 2026-11-19:
# `check_name=pr-title` answers the five pr-title runs of five suites, as `filter=all` does), and
# with `filter=all` every check run the commit holds (122, `ci-ok` its success and its attempt-1
# failure); `check_name` and `status` keep the check runs of that name or status, and the page
# the query asks. The program runs over every check run the world holds at the commit, in their
# order, each naming its suite (`check_suite.id`); any other parameter (`app_id`, …) is not read.
def check_runs_program:
reads(["filter", "check_name", "status", "per_page", "page"])
| (.check_runs | to_entries) as $all
| (if $query.filter == "all" then $all
   else [$all | group_by([.value.name, .value.check_suite.id])[]
         | max_by([.value.completed_at, .value.id])] | sort_by(.key)
   end | map(.value))
| map(select(($query.check_name == null or .name == $query.check_name)
    and ($query.status == null or .status == $query.status)))
| {total_count:length, check_runs:paged};

# The jobs of a run, as GitHub answers them: with no `filter` or `filter=latest`, the jobs of its
# latest attempt, and with `filter=all` those of every attempt (measured on cybermonklabs/legion
# run 44406867953, two attempts, 2026-11-18: 58 jobs, all of attempt 2, and 116 with
# `filter=all`); the page the query asks, the count of the whole list beside it. The program runs
# over {jobs, all}, the latest jobs and every job of every attempt as the world holds them; another
# `filter` or parameter is not read.
def jobs_program:
reads(["filter", "per_page", "page"])
| (if ($query.filter // "latest") == "latest" then .jobs
   elif $query.filter == "all" then .all
   else error("a filter the rig does not read") end)
| {total_count:length, jobs:paged};

# The jobs of one attempt of a run, as GitHub answers them: every job of that attempt, and the
# page the query asks, the count of the whole list beside it. The endpoint takes `per_page` and
# `page` alone; a `filter` is no parameter of it, and GitHub answers whatever it says as if it were
# absent, as it answers an unknown `filter` on a commit's check runs (measured, check_runs_program;
# the architect's ruling of 2026-11-19). Any other parameter is not read.
def attempt_jobs_program:
reads(["filter", "per_page", "page"])
| .jobs | {total_count:length, jobs:paged};

# A run, or one attempt of it, as GitHub answers it: with `exclude_pull_requests=true` it lists no
# pull request, as GitHub's runs list does (measured on cybermonklabs/legion, 2026-11-18,
# runs_query_program), and the endpoints document the parameter (gh run view sends it). A page
# (`per_page`, `page`) is nothing to one object, which gh asks for when it pages the read
# (--paginate); any other parameter is not read.
def run_program:
reads(["exclude_pull_requests", "per_page", "page"])
| if $query.exclude_pull_requests == "true" then .pull_requests = [] else . end;

# The check suites of a commit, as GitHub answers its check-suites endpoint: those holding a check
# run named `check_name`, those of the app `app_id`, and the page the query asks, the count of the
# whole list beside it; GitHub documents the four (the review of 6b97edd6, P2). The program runs over
# {check_suites, names}, the suites as the world holds them and the names of the check runs of
# each suite by its id; any other parameter is not read.
def check_suites_program:
reads(["check_name", "app_id", "per_page", "page"])
| .names as $names
| [.check_suites[] | select(($query.check_name == null
                              or ($names[(.id|tostring)] // [] | index($query.check_name)) != null)
                             and ($query.app_id == null or (.app.id|tostring) == $query.app_id))]
| {total_count:length, check_suites:paged};

# The annotations of a check run, and the page the query asks; any other parameter is not read.
def annotations_program:
reads(["per_page", "page"]) | paged;

# The tree of a commit as GitHub's git/trees answers it: the entries of its top level, or, where
# the read names `recursive`, whatever its value (GitHub documents that any value recurses), every
# entry under it, its subtrees with their entries; any other parameter is not read.
def trees_query_program:
reads(["recursive"])
| if $query.recursive == null then .tree |= map(select(.path | contains("/") | not)) else . end;

# The text gh 2.98.0 prints for `gh run view <run>` (`--verbose` lists the steps of every job,
# without it the steps of a failed job alone) and for `gh run view --job <job>` to no terminal
# (measured, ip-runview, ip-runview-v and ip-runview-job, requests.log lines 17, 19 and 21): the
# run's line, what triggered it and how long ago, its jobs, each with its time and
# id, and where to look next. The program runs over {run, jobs} or {run, job}; how long ago is
# gh's own rounding of $now less the run's creation. The run's line names, after its workflow, the
# pull request of its branch as `<owner>/<repo>#<number>` (view.go:349-352 at v2.98.0, requests.log
# line 49, which reads it through PullRequestForRun; measured
# `* legion/INFRA-1303 CI cybermonklabs/legion#3514 · 44586483868` on 2026-11-19). A run, a job or
# a step not yet completed is marked `*` and a job not yet completed has no time (measured on that
# same run while it ran). The defs below, before run_view_program, are what the programs of
# `gh run view` and `gh run watch` share, which renders a run and a job alike
# (shared.RenderRunHeader and shared.RenderJobs, pkg/cmd/run/shared/presentation.go at v2.98.0).
def ago($t): ($now - ($t | fromdate)) as $s | (($s / 3600) | floor) as $h
  | def about($n; $unit): "about \($n) \($unit)\(if $n == 1 then "" else "s" end) ago";
  if $s < 60 then "less than a minute ago"
  elif $s < 3600 then about(($s / 60) | floor; "minute")
  elif $s < 86400 then about($h; "hour")
  elif $s < 2592000 then about(($h / 24) | floor; "day")
  elif $s < 31536000 then about(((($h / 24) | floor) / 30) | floor; "month")
  else about(($s / 31536000) | floor; "year") end;
def mark: if .status != "completed" then "*" elif .conclusion == "success" then "✓"
  elif .conclusion == "failure" then "X" else "-" end;
def took: ((.completed_at | fromdate) - (.started_at | fromdate)) as $s
  | (($s / 3600) | floor) as $h | ((($s % 3600) / 60) | floor) as $m
  | if $h > 0 then "\($h)h\($m)m\($s % 60)s" elif $m > 0 then "\($m)m\($s % 60)s"
    else "\($s % 60)s" end;
def pull: if (.pull_requests | length) > 0
  then " " + .repository.full_name + "#" + (.pull_requests[0].number | tostring) else "" end;
def heading: "\n" + mark + " " + .head_branch + " " + .name + pull + " · "
  + (.id | tostring) + "\nTriggered via " + .event + " " + ago(.created_at) + "\n\n";
def line($steps): mark + " " + .name + (if .status == "completed" then " in " + took else "" end)
  + " (ID " + (.id | tostring)
  + ")" + (if $steps then .steps | map("\n  " + mark + " " + .name) | join("")
           else "" end);
def run_view_program:
if has("job") then
  (.run | heading) + (.job | line(true)) + "\n\n"
  + (if .job.conclusion == "failure"
     then "To see the logs for the failed steps, try: gh run view --log-failed --job=\(.job.id)"
     else "To see the full job log, try: gh run view --log --job=\(.job.id)" end)
  + "\nView this run on GitHub: " + .run.html_url
else
  (opt("--verbose") == true) as $verbose
  | (.run | heading) + "JOBS\n" + (.jobs | map(line($verbose or .conclusion == "failure")) | join("\n"))
  + "\n\n"
  + (if .run.conclusion == "failure"
     then "To see what failed, try: gh run view \(.run.id) --log-failed"
     else "For more information about a job, try: gh run view --job=<job-id>" end)
  + "\nView this run on GitHub: " + .run.html_url
end;

# The text gh 2.98.0 prints for `gh run watch <run>` to no terminal, read from gh's own source at
# v2.98.0 (github.com/cli/cli, pkg/cmd/run/watch/watch.go, with pkg/cmd/run/shared/shared.go and
# presentation.go), over {run} or {run, jobs}, the run as GetRun reads it and the jobs of its
# latest attempt as GetJobs reads them:
#   - a run that has completed: `Run <workflow> (<id>) has already completed with '<conclusion>'`
#     (watch.go:139-145; GetRun names the run by its workflow, shared.go:592-596); the route exits
#     1 where the call names --exit-status and the run did not succeed, gh's SilentError, which
#     prints nothing more (watch.go:141-143, internal/ghcmd/cmd.go:199-200)
#   - a run that has not: the frame gh prints each time it reads the run (watch.go:162-187 and
#     renderRun, watch.go:215-283): `Refreshing run status every <interval> seconds. Press Ctrl+C
#     to quit.`, a blank line, the run's header as gh run view prints it but for the blank lines
#     around it (presentation.go:10-26), a blank line, `JOBS`, and each job of the attempt with
#     every step (RenderJobs with verbose, watch.go:268), or with --compact its failed steps and
#     the step in progress alone (RenderJobsCompact, watch.go:266); no job of an attempt in
#     progress has failed in any world, so no frame carries gh's ANNOTATIONS (watch.go:270-279). The
#     route marks that reply `watch`, and the stand-in prints the frame again at each interval
#     (gh-standin.ts), as gh reads the run again, for as long as the call runs: the run cannot
#     complete while it does, since the rig settles a re-run only once the turn that pressed it
#     has ended
def run_watch_program:
def failed: .conclusion == "failure" or .conclusion == "action_required"
  or .conclusion == "startup_failure" or .conclusion == "timed_out";
def step_line: "\n  " + mark + " " + .name;
def compact: line(false)
  + (if .status == "completed" and .conclusion == "success" then ""
     else ([.steps[] | select(failed) | step_line] | join(""))
       + ([.steps[] | select(.status == "in_progress")] | last // null
          | if . == null then "" else step_line end) end);
if .run.status == "completed"
then "Run \(.run.name) (\(.run.id)) has already completed with '\(.run.conclusion)'"
else (opt("--interval") // "3" | tonumber) as $every
  | (if opt("--compact") == true then .jobs | map(compact) else .jobs | map(line(true)) end) as $jobs
  | "Refreshing run status every \($every) seconds. Press Ctrl+C to quit.\n\n"
    + (.run | heading | ltrimstr("\n") | rtrimstr("\n\n")) + "\n\nJOBS\n" + ($jobs | join("\n")) + "\n"
end;

# The text gh 2.98.0 prints for `gh pr checks --watch` to no terminal, read from gh's own source
# at v2.98.0 (github.com/cli/cli, pkg/cmd/pr/checks/checks.go:204-246 and output.go:69-92), over
# {rows, pending}, the rows `gh pr checks` prints in a state of the head and whether a check of it
# is pending: while one is, the frame gh prints each time it reads the checks (the summary prints
# nothing to no terminal); once none is, the rows, then the rows again as its final table; and
# nothing where the base requires no check the call reads ({none}, whose error the route prints).
def checks_watch_program:
(opt("--interval") // "10" | tonumber) as $every
| if .none then ""
  elif .pending then "Refreshing checks status every \($every) seconds. Press Ctrl+C to quit.\n\n" + .rows + "\n"
  else .rows + "\n" + .rows end;
