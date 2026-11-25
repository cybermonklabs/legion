---
title: A pull request's checks and their re-run
description: How GitHub counts a pull request's checks at a head, which reads give that count and which do not, when a run's log and its re-run can be read, and how a re-run's attempts read afterwards. Each fact names the measurement it rests on.
sidebar:
  order: 7
---

This page says what GitHub and `gh` answer when you read the checks of a pull request's head, and
when you re-run a run that failed there. It states what was measured in cybermonklabs/legion and
names each measurement. It says nothing about who acts on a red check or when: that is a rule of
the Legion skills (`skills/legion-worker/references/red-at-the-head.md`), not a fact about GitHub.
The commands use `gh` 2.98.0.

## The checks GitHub counts for a pull request

A pull request's checks are its head commit's status check rollup: the latest run of each check
name at the head, the lanes under an aggregate check and the checks the base does not require
included. Call that the counted set. Two reads give it.

At the pull request's current head:

```bash
gh pr checks <n>
```

Without `--required`. With it, `gh` prints only the checks the base requires: at `ed157dc9`,
read while `ci-ok` had not yet reported, it printed 1 row where the rollup held 46, and once CI
settled the same read gives 2 of 48. So no lane under the aggregate `ci-ok` and no check the base
does not require is in it. On a base that requires nothing, `--required` prints that no
required checks are reported and exits 1 (dispatch://INFRA-3035, on cybermonklabs/legion#3658).

At a named head, such as a commit you have just pushed:

```bash
gh api graphql --paginate -F oid=<head> -f query='query($oid:GitObjectID!,$endCursor:String){repository(owner:"<owner>",name:"<repo>"){object(oid:$oid){... on Commit{statusCheckRollup{state contexts(first:100, after:$endCursor){totalCount pageInfo{hasNextPage endCursor} nodes{__typename ... on CheckRun{name databaseId status conclusion checkSuite{app{databaseId} workflowRun{event databaseId}}} ... on StatusContext{context state}}}}}}}}'
```

GitHub gives at most 100 contexts a page. `--paginate` follows `hasNextPage` only when the query
takes the `$endCursor` variable and asks for `endCursor` in its page info, as this one does. A head
of this repository carries about 65 contexts. The read costs 1 point of the token's GraphQL limit,
which was 10,000 points an hour for the Legion implement App's installation token it was measured
with on 2026-11-19; `gh api rate_limit` shows a token's own limit.

### A read that is not the counted set

`repos/<owner>/<repo>/commits/<head>/check-runs` answers every check run at the commit, a run
started by hand with `workflow_dispatch` among them, and GitHub counts such a run for no pull
request. Measured on 2026-11-11: at `9b728582e2762eb62b9c582d7d843df772b951ff` it answered 113
check runs, later 114, where the pull request's rollup held 57; the runs left out belonged to two
by-hand runs at that commit (dispatch://INFRA-1303/comment/07122575-8262-4eda-8f60-6c7c74c82186
and dispatch://INFRA-3035's spec).

A by-hand run at the head of an open pull request names that pull request in its own
`pull_requests` field all the same. Measured on 2026-11-19: each of the 30 latest
`workflow_dispatch` runs of cybermonklabs/legion listed one pull request, run 44457284390 at
`38c89d64` among them, which listed #3729 while #3729 was open at that head. That field is no
evidence that GitHub counts the run for the pull request; the rollup is.

### An empty set, and one that has not caught up

A rollup of `null` means GitHub counts no context at that head, and `gh pr checks` prints that no
checks are reported. Measured at `ebb186ba575924b3a341769ad2168387d056724f`, where the commit
endpoint answered 55 runs of a by-hand `workflow_dispatch` and the rollup was `null`.

After a push GitHub fills the head's checks over seconds and minutes. A read made at once can find
a check pending, queued, in progress, or not there yet, and a later read of the same head finds it.
`gh pr checks --watch` and `gh run watch` read GitHub again at an interval, 10 and 3 seconds by
default (`gh pr checks --help`, `gh run watch --help`).

## A run's log, and its re-run, wait for the whole run

A check can read `completed` while other jobs of its workflow run still run. Until the whole run
ends, GitHub keeps its log and its re-run back:

- `gh run view <run-id> --log-failed` answers `run <run-id> is still in progress; logs will be
  available when it is complete` and exits 1 (measured on run 43686474806).
- `gh run rerun <run-id> --failed` is refused (measured on run 43686474806). On run 44186145691,
  while it was `in_progress`, it answered `This workflow is already running`
  (dispatch://INFRA-1478/artifact/retro-addendum-md, line 38).
- A refused re-run starts no attempt.

`gh run view <run-id> --json status --jq .status` reads the run's own status; the log and the
re-run are answered once it reads `completed`.

A failed step's log can hold no line that states the failure: a log of `677 bytes, none printed`
hid an apt failure's own diagnosis for a whole fleet until a round printed the body
(dispatch://INFRA-1545).

## What a re-run does, and how its attempts read afterwards

`gh run rerun <run-id> --failed` re-runs only the run's failed jobs, with the jobs that depend on
them; `--job <job-id>` re-runs one job with the jobs that depend on it (`gh run rerun --help`).
`--job` takes the job's `databaseId` from `gh run view <run-id> --json jobs`, not the number in the
job's address in the browser, which answers 404.

A re-run keeps the run's id and raises its `run_attempt` by one. Every job of the new attempt, the
ones carried over from the attempt before included, has a job id and a check run of its own.
Measured on run 44406867953, whose attempt 2 re-ran its 2 failed jobs: attempt 1 lists 58 jobs and
attempt 2 lists 58, all at `run_attempt` 2, and each of the 56 jobs not re-run is carried to
attempt 2 with an id and a check run of its own, its attempt-1 conclusion, its steps and its runner
(`packages/pi-legion/scripts/skill-scenarios/github-answers/README.txt`, the reads measured on
2026-11-18).

The rollup carries only the latest run of each check name, so a failure a re-run superseded is not
in the counted set at all. The head's own pull request runs, with each run's attempt, are:

```bash
gh api "repos/<owner>/<repo>/actions/runs?head_sha=<head>&event=pull_request&per_page=100" \
  --jq '.workflow_runs[] | [.id,.name,.run_attempt,.conclusion] | @tsv'
```

`event=pull_request` leaves out the by-hand `workflow_dispatch` runs, as the rollup does. A run
whose `run_attempt` is above 1 was re-run. Each earlier attempt's jobs, `<n>` from 1 to one below
the run's `run_attempt`, are:

```bash
gh api --paginate "repos/<owner>/<repo>/actions/runs/<run-id>/attempts/<n>/jobs?per_page=100" \
  --jq '.total_count, (.jobs[] | [.name,.conclusion] | @tsv)'
```

That endpoint takes `per_page` and `page` and no filter
(<https://docs.github.com/en/rest/actions/workflow-jobs#list-jobs-for-a-workflow-run-attempt>). A
run can hold more than 100 jobs, so a read that returned fewer jobs than `total_count` is
incomplete. The run's own jobs endpoint, `actions/runs/<run-id>/jobs`, lists the latest attempt's
jobs alone unless it is given `filter=all`: for run 44406867953 it listed 58, and 116 with
`filter=all` (`packages/pi-legion/scripts/skill-scenarios/github-answers/README.txt`).

Measured on run 43330369709 at `94fa9812fd0fcbecf444016d93ff41428dcce523`: attempt 1 ended
`cancelled`, its job `dispatch-browser / merge` `cancelled` and the aggregate `ci-ok` `failure`,
and attempt 2 passed. An aggregate that needs a cancelled lane reads `failure` over it. GitHub
cancels a job that runs past its `timeout-minutes`
(<https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idtimeout-minutes>),
so a lane that hangs reads `cancelled` at each attempt it hangs.

## Whether a branch carries a fix

A clone answers no to every ancestry question about a commit it has not fetched, so fetch first.

- For a file: `git log --oneline <base>..origin/main -- <path>` prints nothing both for a path that
  does not exist and for a path no commit in that range changed, and exits 0 in both cases, so
  `test -e <path>` tells the two apart.
- For a commit: `git merge-base --is-ancestor <sha> <head>` exits 0 when `<head>` carries `<sha>`,
  1 when it does not, and anything else when it cannot compare them, for example for a commit the
  clone does not hold.

Two repositories can carry the same pull request numbers, so a number alone does not say which
repository holds a fix.

## What Legion's daemon does with a red check

A red check moves no phase. When CI settles on a head, the daemon names every check GitHub
reported, with the conclusion GitHub gave it, to the issue's architect, the worker of the issue's
current phase and the implementer, and judges none of them (`tellChecks` in
`packages/daemon/internal/workflow/review.go`). A head that conflicts with its base is handled
apart, as [Concepts](/legion/legion/concepts/) says.
