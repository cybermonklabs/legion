# Notes for the Dispatch browser nightly workflow

The derivations of the `quarantine-check` job of `.github/workflows/dispatch-browser-nightly.yaml`,
which points into this file. Each `# why:` line of that job names the section below that holds
the derivation its job or its step rests on, and the sections stand in the order their comment
blocks stood in the workflow file. A budget's own arithmetic stays beside the budget, in the
workflow file. The workflow's other comments still stand in it: its bodies are not moved yet
(.github/ci-notes/README.md), and this job is the first of it to hold its derivations here.
In the text moved here, "this workflow" refers to the workflow,
`.github/workflows/dispatch-browser-nightly.yaml`.

## The quarantine check, what it reads and when it runs

The quarantine's checks that depend on the date (R4 of dispatch://INFRA-983):
packages/dispatch/e2e/tools/quarantine-check.ts reads the `dispatch-browser flaky row` notices
of the merge jobs of the last 14 days of scheduled runs of this workflow and merge-group runs
of ci.yaml, and fails on a row flaky in two of them that packages/dispatch/e2e/quarantine.ts
does not hold, and on an entry of that file held longer than 14 days, each named in its
summary with the runs and the lines to add or to remove. A row of the embedder job or of the
two-process job cannot be quarantined: those jobs set no `DISPATCH_E2E_QUARANTINE`, so they run
a quarantined row as they run the rest, and the quarantine leg, one server process with no
embedder, runs none of their rows. For such a row the summary names the lines of a fix entry
instead, and says to fix the row in its own lines or to write that entry. A flaky row that no
row of the checkout's listings is named as, while they hold other rows of its file, was
renamed, deleted, or moved to another file or project since its runs: its flakes cannot be
matched to a row the suite runs, and no entry can name one, so the summary names it under
"Flakes left out" with a `::warning` annotation, and the check fails nothing. A flaky row whose
file the checkout holds, while no listing holds any row of that file, fails the check as a
failed read. Until 14 days after cybermonklabs/legion#4523 lands on main, a notice of a run
from before it, which names a row inside a `describe` by its own title alone, is named with the
row it means, and its flakes are not counted. A pull request's runs are not read,
since a pull request's head can carry its own defect; the tool's head says the rest. It needs
no other job of this run: what it reads is runs that have already ended, this one's not among
them, so a flaky row of tonight's full lane is counted by tomorrow's check.

The 02:17 schedule and a workflow_dispatch alone, not the 13:41 one: a second reading of the
same 14 days half a day later says nothing the first did not. Each reading spends about 34 REST
calls and 13 GraphQL calls of the repository's budgets (the measurement in
.github/ci-notes/dispatch-browser-nightly.md#the-quarantine-checks-measured-steps-and-its-budget).

## Why the quarantine check holds those three scopes, and what it reads of the checkout

`actions: read` is the run listing of the two workflows, `checks: read` the merge jobs' check
runs and their annotations, read through GraphQL, and `contents: read` the checkout. The tool
writes nothing to GitHub. For a row flaky in two runs it reads the row's own lines from listings
of this checkout as the jobs run the row, in one server process and in two-process mode
(DISPATCH_E2E_PROCESSES=2, and with DISPATCH_E2E_SIGNIN=oidc for `signin.multiprocess.e2e.ts`),
and their last change from the file's blame at this commit (one GraphQL request per file, which
the tool sends itself with the job's token); and it reads the files the quarantine cannot take
from listings of the embedder job's command and of two-process mode. So it needs the install
and the contracts build the listings load; it reads no git history, so the checkout stays
shallow.
It counts a flake only from a run whose head, the commit the run tested, holds that change, and holds the row's fix too where `packages/dispatch/e2e/quarantine.ts#fixes` names one.
GitHub's compare of each such commit with each head says whether the head holds it.
A run's time does not say it: a nightly started by hand at a branch head blames the branch's own commits, and a merge group of main created after them holds none of them until the branch lands.
A compare that cannot tell leaves its run counted, and the summary names the run with the reason.
It reads each fix's commit against this one through GitHub's compare too.

## The quarantine check's measured steps and its budget

The budget, by the rule .github/ci-notes/dispatch-browser.md states for every job. Since
`0d9a0258` the tool reads the check runs through GraphQL, and the check step of each of its
nightly jobs took 24 to 28 s on 2026-11-22: jobs 137090407550 (04:12:26Z to 04:12:50Z),
137103508525 (05:11:34Z to 05:11:59Z) and 137106841419 (05:26:38Z to 05:27:02Z) 24 or 25 s, and
the four after them, 137148928893, 137198886464, 137203993375 and 137215767381 (the last at
`54a60643`, 12:58:36Z to 12:59:04Z), 27 or 28 s, as the runs it reads grew from 1,198 to 1,347. On
2026-11-22 at 03:15Z, from this issue's pod, the tool's reads of the same 14 days took 22 s: 1,195
runs in 34 REST calls and 13 GraphQL calls costing 132 points.
To those the tool adds a REST call, GitHub's compare, for each pair of a candidate row's commit (its last change, and its fix where `fixes` names one) and the head of a flaky run that named the row, and none on a day with no candidate.
On a day with a candidate it also lists the checkout four times at once (`readCheckout`): the candidates' files in one server process, two-process mode under each of its two environments, and the embedder job's command. In this issue's pod on 2026-11-24 the four took 3.9 to 4.6 s each, run at once beside another test file's listings.
Before `0d9a0258`, the REST reader's 37
steps that succeeded on 2026-11-21 and 22 took 68 to 104 s, the longest job 137021330193 of run
44961580846, 23:28:02Z to 23:29:46Z on 2026-11-21. Seven steps of one day are no fortnight's
maximum, so the budget keeps the REST reader's, 104 x 1.15 = 120 s, until a fortnight of GraphQL
steps gives one. The steps without a budget, at their maximum over the 12 jobs of this workflow's
run 44545596758 that ran them: Set up job 2 s, checkout 12 s, setup-bun 4 s, `bun install` 11 s,
the contracts build 1 s, and the post and completion steps 3 s: 33 s, and 300 + 33 = 333 s rounds
up to 6 minutes.

The step's 5 minutes are its work and a wait for GitHub's rate limit. The check failed on that
limit at 02:14Z on 2026-11-22 (job 137061949842 of run 44974726732), refused `API rate limit
exceeded for installation`. The tool waits out a refusal that names the time GitHub may be
asked again: `retry-after`, or `x-ratelimit-reset` where `x-ratelimit-remaining` is 0, for REST
and for GraphQL alike, or, for a 403 or a 429 whose body names a secondary rate limit and that
carries no `retry-after`, 60 s, GitHub's documented minimum, as gh-read.sh waits it. It waits by
the one rule both of the browser lane's readers of GitHub
follow, gh-read.sh's gh_wait_reset and this tool
(.github/ci-notes/dispatch-browser.md#the-merge-jobs-measured-steps-and-its-budget states it):
only for a time that falls at least 30 s before the earlier of the job's limit and the step's,
which the job's env passes as JOB_TIMEOUT_MINUTES and the step's as STEP_TIMEOUT_MINUTES, each
measured from its start as the tool reads it off the runner's process tree (`readDeadline`).
A reset that has already passed names no time to wait for, so that refusal takes the tool's three
attempts, 10 s and then 20 s apart, instead. Where the tool's waits still differ from
gh-read.sh's, `gitHub`'s doc comment in the tool names each difference.

The 5 minutes were derived for a cap of 180 s on the waits: a refusal of the step's first read
leaves all of the step's work after the wait, so 300 - 120 = 180 s. The rule gives another number,
and the tool takes the rule's. The steps before this one take 30 s at most (above), so the step's
own limit, 300 s from its start, comes before the job's 360 s from the job's and binds: a wait may
end up to 270 s after the step's start, less only the time the reading has already run, and
leaves 30 s after it rather than the 120 s of the REST reader's longest step (the GraphQL
reader's steps took 24 to 28 s). A reading a long wait leaves too little time for is ended by
the step's own limit, which names the step, and a re-run after the reset reads the fortnight
again. The step's time grows with the merge queue's runs, which the listing reads a day at a time
and the GraphQL reads 100 to a call, so a busier fortnight moves it: a step that outran its budget
names itself.

## Why the quarantine check builds contracts

The step of this name in .github/workflows/dispatch-browser.yaml's shard job, for the reason
.github/ci-notes/dispatch-browser.md#why-every-job-that-runs-the-suite-builds-contracts-first
gives: the listing loads e2e/playwright.config.ts, which imports `@legion/contracts` out of the
build this step makes.
