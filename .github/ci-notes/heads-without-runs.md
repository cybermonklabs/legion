# Notes for the check that names pull request heads no workflow ran

The derivations and the history of `.github/workflows/heads-without-runs.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that its comment, its job or
its key rests on. "The script" is `.github/scripts/heads-without-runs.sh`, whose header says what it
prints and when it fails.

## Why the check exists

On 2026-11-26 pull request #4991 got a new head, de9728723ab26b62917a5cc18c8d4c237d7ba72e, and no
workflow ran on it. Read at 2026-11-27T00:33:13Z, more than three hours after its commit, the
Actions API's `actions/runs?head_sha=` answered `total_count` 0, and the commit carried 0 check
runs. Its two check suites were two GitHub Apps' queued suites, with no check run in either.

The ci-ok gate cannot see that shape. A lane job that a pull request's labels hold back still runs
as a skipped job, so it leaves a skipped check run on the head, and the gate reads it. A head that
no workflow ran has no check run at all: no ci-ok, no failed job, nothing on the pull request that
says why.

The cause is a merge conflict. #4991 conflicts with main: its next head, a338eaed, read
`mergeable` false at 2026-11-27T00:51:22Z and carried no check run either. GitHub's "Events that
trigger workflows" page says: "Workflows will not run on pull_request activity if the pull request
has a merge conflict. The merge conflict must be resolved first." A pull_request run checks out the
merge of the head into the base, and a conflict leaves no merge to check out.

It was not the only one. A read at 2026-11-27T00:31:26Z found 482 open pull requests, 92 of them
drafts, and 23 heads that were 20 minutes old or more with no check run. The script's first run, at
2026-11-27T00:51:22Z, named 26 such heads, and every one conflicted with its base. Five of them were
more than 24 days old.

So the check names each such head, and says which of two shapes it is: a conflict, which the
author fixes and GitHub shows on the pull request; or no run and no conflict, which nothing else
reports.

## What the check reads

Every open pull request, through GitHub's GraphQL API, 25 a page: its number, whether it is a draft,
when it was opened, its head, and of the head commit its committer date and, for each check suite on
it, whether the suite holds a check run. The read asks for one check run a suite,
`checkRuns(first: 1)`, so a suite with a run is told from one without, and nothing is counted. A
check run of any status or conclusion counts, `skipped` among them: the read asks for no conclusion.

25 a page, because larger pages failed on 2026-11-27. A page of 100 got an HTTP 502. A page of 50 got
`RESOURCE_LIMITS_EXCEEDED` on the eighth page, where the newer pull requests' heads hold more check
suites, and GitHub then answered `checkRuns` null for every suite after the limit. The same eighth
page read at 25 answered whole, and so did the whole list. The script refuses any answer that
carries an `errors` field or a `checkRuns` that is not a list, so a page cut short this way is
UNKNOWN, never read as heads with no run.

For each head it names, that pull request through the REST API, once: `mergeable` false is a
conflict, `mergeable` true is no conflict. A conflict is printed as "conflicts with its base
branch", and the branch is never named: this repository's log and step summary are public, and a
base branch's name is not this check's to print there. GitHub's REST page "REST API endpoints for
pull requests" says of a null: "If the value is null, then GitHub has started a background job to
compute the mergeability. After giving the job time to complete, resubmit the request." So the
script reads each null once more after 10 s. A null at the second read is named "mergeability not
yet computed". The first run met six nulls, and all six had a value 10 s later. GraphQL's own
`mergeable` is not used: it read UNKNOWN for three of the 23 heads at 2026-11-27T00:31:26Z, and the
REST read is the one whose null GitHub's page documents.

The age of a head is measured from the later of its commit's committer date and the opening of its
pull request. GitHub keeps no push time for a commit: GraphQL's `Commit.pushedDate` read null for
all 350 heads read on 2026-11-27. A rebase or an amend writes a new committer date, so the usual push
reads right. A commit made long ago and pushed just now reads as old, and can be named in the first
20 minutes after its push, before its run has posted a check run. The next run clears it once one
has.

The cost. The first run made 20 GraphQL calls, each one point, and 33 REST calls: one for each of
the 27 heads the list named (one had taken a new head by the time it was read) and six second
reads. It took 137 s from a development machine. At two runs an hour that is about 40 GraphQL
points and 70 REST calls an hour of the token's budget.

## Why a conflict alone does not fail the check

A head that conflicts with its base is named in the log and in the step summary, and gets no
`::error::` line. The job fails on the other heads only.

A conflict is the author's to fix. GitHub already shows it on the pull request page, and the merge
queue cannot take the pull request until it is fixed, so nothing reaches main untested this way. On
2026-11-27 there were 26 such heads, five of them more than 24 days old. A check that failed on them
would be red on every run for weeks, and a reader would stop opening it. The head with no run and
no conflict, the one nothing else reports, would then go unseen in a run that was red anyway.

A head whose mergeability the script could not tell fails the check: it is not proved to be a
conflict.

## Why a workflow of its own, and how it is proved

GitHub starts a `schedule` from the default branch alone, and offers `workflow_dispatch` only once
the default branch holds the file. No workflow of this repository already stands on main with a
schedule that runs every half hour, and a job added to dispatch-browser-nightly.yaml would start that
file's eight-runner browser lane on every cron. So the check is a small file of its own.

Until the file is on main, the harness proves the script:
`.github/scripts/heads-without-runs.test.sh`, which ci.yaml's test job runs, drives it with a
stand-in `gh` over fixtures in the shapes GitHub answered. Once the file is on main,
`gh workflow run heads-without-runs.yaml` runs it at once, and the schedule runs it every half hour
after that.

## The scopes the job holds

`pull-requests: read` for the list of open pull requests and the REST read of each named one;
`checks: read` for the check suites and check runs on each head; `contents: read` for the head
commits the list reads and for the checkout. The repository is public, so GitHub may serve some of
these to a token without the scope. The scopes are declared so that the read does not rest on that.
Nothing is written: no comment, no check run, no issue.

## The job's budget

The first run took 137 s, from a development machine on 2026-11-27, for 481 open pull requests. A
GitHub-hosted runner reads the same API. Each call goes through gh-read.sh's gh_read, which tries a
refused call twice more, 10 s and then 20 s later, and waits out a spent rate limit within the job's
JOB_TIMEOUT_MINUTES. A run in which every page needs its two retries adds 20 x 30 s = 600 s. 137 +
600 = 737 s, and the checkout and a list that grows take it to 15 minutes. The budget is a hang
guard, and decides no verdict.
