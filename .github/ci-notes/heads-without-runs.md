# Notes for the check that names pull request heads no workflow ran and queued entries gone dirty

The derivations and the history of `.github/workflows/heads-without-runs.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that its comment, its job or
its key rests on. "The script" is `.github/scripts/heads-without-runs.sh`, whose header says what it
prints and when it fails.

## One check, three cases

The check is one instrument with three cases: a head with no run because its pull request conflicts
with its base branch (listed); a head with no run for no reason anyone can see (the one case that
fails the job); and an entry of main's merge queue whose head has come to conflict with main (a
warning).

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
`::error::` line. The job fails on the other heads only. A queued entry that conflicts with main
gets a `::warning::` line and fails nothing; the next section says why.

A conflict is the author's to fix. GitHub already shows it on the pull request page, and the merge
queue cannot take the pull request until it is fixed, so nothing reaches main untested this way. On
2026-11-27 there were 26 such heads, five of them more than 24 days old. A check that failed on them
would be red on every run for weeks, and a reader would stop opening it. The head with no run and
no conflict, the one nothing else reports, would then go unseen in a run that was red anyway.

A head whose mergeability the script could not tell fails the check: it is not proved to be a
conflict.

## What the queue case reads, and why it only warns

A reading of the pull requests' timelines and main's activity for 2026-11-26 found that the queue
does not remove an entry when its head comes to conflict with main, only when the entry next reaches
the group the queue builds. It found 12 such entries that day: a median of 18 minutes queued while
they conflicted, a longest of 111 minutes (#4065), and 376 pull-request minutes in all. They cost no
runner time, so a person reads them as a warning, and the job does not fail on them.

The check only reads. It removes no entry from the queue and makes no write call of any kind: a
wrong removal would make the queue build again every group behind the entry.

What it reads:

1. Every entry of main's merge queue, through GraphQL's `repository.mergeQueue(branch: "main")`:
   its position, its state, `enqueuedAt`, and its pull request's number and `headRefOid`. The head
   read is the head the queue holds. `enqueuedAt` stands for the time of the pull request's latest
   `AddedToMergeQueueEvent`: read together at 2026-11-27T02:44:56Z for the three entries then
   queued, two were equal and one was a second earlier.
2. One fetch, with no credential, of main and every queued head, `--filter=blob:none`, into a bare
   repository in a temporary directory of its own. `git merge-tree --write-tree --name-only` of
   main's tip and each head: exit 1 is a conflict and lists the files, exit 0 is clean. git answers
   1 for a revision it cannot read too, so a merge counts only when git also wrote a tree; anything
   else is UNKNOWN.
3. For each entry that conflicts, the pull request whose landing caused it. Main as it was when the
   entry was queued is the `after` of the last move of main before `enqueuedAt`, from the REST
   route `repos/<repo>/activity?ref=refs/heads/main`. Main's commit dates are not read: the queue
   writes the commit when it builds the group. Then each commit of
   `git rev-list --first-parent --reverse <main then>..<main now>` is merged with the head, in
   order, and the first that conflicts is the one. Its subject ends with `(#<n>)`, the pull request
   the queue squashed into it. The cause is UNKNOWN when the activity cannot be read or holds no
   move before `enqueuedAt`, when the head conflicted with main already when it was queued, or when
   the commit's subject names no pull request.

One move of main can carry several commits, one for each pull request of a group. A read of #4065
on 2026-11-27, with no fixture, showed it. Its head while it was queued was 1734f623. Main moved at
2026-11-26T06:12:10Z from bafbb698 to ea7a5472, four commits in one push. git merge-tree of the
head and bafbb698 exited 0, and of the head and ea7a5472 exited 1, naming `docs/kubernetes.md` and
`packages/daemon/internal/store/store_test.go`. The commit-by-commit scan from main at its enqueue
named be11d8db, `(#5310)`, the pull request the 2026-11-26 reading named too. ea7a5472, the move's
`after`, is #5350's commit, so naming the pull request of a move's `after` would have named #5350.

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

`pull-requests: read` for the list of open pull requests, the REST read of each named one, and the
queue's entries; `checks: read` for the check suites and check runs on each head; `contents: read`
for the head commits the list reads, for the checkout, and for main's activity (GitHub's REST page
for "List repository activities" names the "Contents" repository permission, read). The fetch uses
no credential. The repository is public, so GitHub may serve some of these to a token without the
scope. The scopes are declared so that the read does not rest on that. Nothing is written: no
comment, no check run, no issue, and no change to the queue.

## The job's budget

The run of 2026-11-27T02:44:22Z took 156 s from a development machine, for 476 open pull requests
and three queued entries, none of which conflicted. The fetch of main and a head took 3 s in the
#4065 read above, and each merge of the scan well under a second. A GitHub-hosted runner reads the
same API. Each call goes through gh-read.sh's gh_read, which tries a refused call twice more, 10 s
and then 20 s later, and waits out a spent rate limit within the job's JOB_TIMEOUT_MINUTES. A run in
which every page needs its two retries adds 22 x 30 s = 660 s. 156 + 660 = 816 s, and the checkout,
a list that grows and the scans of entries that conflict take it to 15 minutes. The budget is a hang
guard, and decides no verdict.
