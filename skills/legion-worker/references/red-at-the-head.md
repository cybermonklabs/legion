# A red check at the head of the pull request

Part of `skill://legion-worker`. Read it when a check is red at the head of the issue's pull
request and you hold `reviewing` or `implementing`, or `merging` at a head your own forward merge
made. Every path it cites is in cybermonklabs/legion. The runs that show what GitHub answers here:
<https://cybermonklabs.github.io/legion/legion/checks-and-re-runs/>.

A red the change did not make costs the tree a whole lap: an implementing round, a new CI pass, a
new testing round, and often a new review round. The role that already reads that head clears such
a red where it stands. It writes the failure down before the `gh run rerun` call that re-runs the
failed jobs once: in a handoff record of its own phase (`dispatch handoff-write`), or, for the
merger, in a `dispatch comment` on the issue.

## Who may re-run

- The holders of the re-run are the reviewer in `reviewing`, the implementer while it holds
  `implementing`, and the merger in `merging` at a head its own forward merge made.
- The merger re-runs only a failed `pull_request` check at a head its own forward merge made: once
  at that head, only after it records the failure, and in the order, the waits and the spend this
  page gives. At any other head it re-runs nothing. A `merge_group` check goes only through the one
  requeue of a `failed_checks` removal, which its role prompt gives. When its re-run is spent or
  comes back red, it records the red and decides it as its role prompt's *A check that failed* says.
- The tester presses nothing: it reads no CI, and a red at the head does not end its round.
- The reviewer neither waits for a check the base does not require nor re-runs it for the approval,
  as `skill://legion-worker/references/merge-gate.md` says. It approves over such a red the change
  did not make and names the red in its `redAtTheHead` record and its review body, and nowhere else:
  with no re-run, nothing shows the test fails at random, so the red is no occurrence for the
  census. The implementer still re-runs such a red once.
- The architect in `accepting` and an implementer that runs retro re-run nothing.
- A role that is told of a red whose phase it does not hold re-runs nothing. While the tester or the
  reviewer holds the work, the implementer is told of each CI result and reads the failed job's log
  at once. Where the change made the red, it starts the fix at once, in a copy outside the issue
  workspace, and pushes the fix when the work comes back to it; while another role holds the work it
  re-runs nothing and pushes nothing but a forward merge the daemon asks of it.
- A red moves no phase: the daemon names each check of a settlement to the architect, the phase's
  worker and, before `merging`, the implementer, and judges none (`tellChecks`), so only the role
  ends a round such a red would hold, by this page.

## The record comes first, never after

Before it presses the re-run, the role records on the Dispatch issue all eight of these:

1. the repository
2. the pull request
3. the head commit
4. the check
5. the run id
6. the attempt
7. the failed job
8. the failing test or step, with the first line that states the failure

A record short of one of the eight is a record the next reader cannot act on, and the eight are
counted as such. The role reads that first line with `gh run view <run-id> --log-failed`. A read
that fails is recorded as failed, with the error, and the role makes the re-run all the same. Where
the log can be read and holds no line that states the failure, the record says that, naming the step
and what the log did hold, rather than an empty field or a line the log does not carry.

**The run that holds the check must have ended first.** A lane can fail and read `completed` while
other lanes of its workflow run still run. Until that run ends, `gh run view <run-id> --log-failed`
answers `run <id> is still in progress; logs will be available when it is complete` and exits 1, and
GitHub refuses `gh run rerun`. So before the log read the role reads the run's own status,
`gh run view <run-id> --json status --jq .status`, and reads the log and presses the re-run only on
`completed`. The run's own `completed` is the leave to press where the run is a `pull_request` run
of the head, whether or not the counted set lists the check yet; until then the role waits for
the run, as *The act and the wait* says. A re-run GitHub refuses all the same starts no attempt
and spends nothing: the role records GitHub's answer and reads the spend again on the next wake,
before it presses, since a refusal while the run reads `completed` can mean another press began one.

The implementer's or the reviewer's record is a handoff record of the role's own phase, written
with `dispatch handoff-write --issue <issue> --phase <implement|review> --body-file <file>` before
the `gh run rerun` call, never after it. Its body carries the field `redAtTheHead`, a list with one
entry per red, each entry naming all eight facts above. That record is a version of its own: the
role writes the phase's full record later, with `redAtTheHead` in it again, and `handoff_complete`
reports the newest version. A re-run pressed before Dispatch has recorded the first version loses
the failure, which is what this page exists to keep. The reviewer repeats the record in the body of
the one review it submits at the end of the round, since the reviewer's one review ends its round.
No file under `packages/daemon/internal/prompts/roles/` names that field, so the implementer and
the reviewer read it here. The merger writes no handoff record: its record is a `dispatch comment`
on the issue that carries the eight facts, posted before the `gh run rerun` call, as its
`failed_checks` requeue record is.

## Which red is the change's

Four readings decide it:

1. A check that judges the pull request itself rather than its code, such as the title check, is the
   change's when its own output names the pull request's own field (its title, body, labels, branch
   name or commits: their messages, authors and committers) as the cause, and it gets no re-run; a
   failure at a fetch, a limit or a runner in that check is read by the fourth reading, like any
   other failure in no test. The one exception is a check whose own output names a comment or a
   review as the cause. No role may edit a comment or a review, so the role tells the architect, and
   that red is re-run by nobody.
2. An aggregate check is resolved to the lane that failed it. That lane is judged in its place.
3. A failing test is the change's when its own package is one the pull request changes: the Go
   package for a Go test, the directory of the test file for a Bun or a Playwright suite. A run of
   that test in the role's own pod does not move it: a test that passes locally is still the
   change's when this reading says so, and it gets no re-run: the reviewer requests changes on it,
   and the implementer fixes the code. A re-run spent on a red the change made is the same lap this
   page exists to save, paid the other way.
4. A failure in no test is the change's only when the failing step reads a file the pull request
   changes. Such a file is the workflow file that job runs, the manifest or the lock file the step
   installs from, or the Dockerfile it builds.

These are therefore not the change's: a fetch that answered 5xx, a name that did not resolve, a
registry, a cache or a package index that refused the step, a runner that ended, a job GitHub
cancelled, and a job its own time limit killed.

A lint, type, build or format failure of the change's own code is the change's.

## One re-run belongs to the head

**The counted set** is what GitHub counts for the pull request, never every run at the commit:
every check of the head's rollup, the lanes under an aggregate and the checks the base does not
require included. The role reads it one of two ways. At the pull request's current head:

```bash
gh pr checks <n>
```

Never with `--required`, which prints the checks the base requires and no other, so a lane under
an aggregate such as `ci-ok` and a check the base does not require are never in it, and on a base
that requires nothing it prints that no required checks are reported and exits 1. At a named head,
which is what a role holds after a push, the rollup of that commit:

```bash
gh api graphql --paginate -F oid=<head> -f query='query($oid:GitObjectID!,$endCursor:String){repository(owner:"<owner>",name:"<repo>"){object(oid:$oid){... on Commit{statusCheckRollup{state contexts(first:100, after:$endCursor){totalCount pageInfo{hasNextPage endCursor} nodes{__typename ... on CheckRun{name databaseId status conclusion checkSuite{app{databaseId} workflowRun{event databaseId}}} ... on StatusContext{context state}}}}}}}}'
```

`--paginate` with the `$endCursor` variable and `endCursor` in the page info is what follows
`hasNextPage`, because the context list is capped at 100 a page.

**Do not judge a red from `repos/<o>/<r>/commits/<head>/check-runs`.** It answers every check
run at the commit, a by-hand `workflow_dispatch` run's too, which counts for no pull request.

**The spend is a different question, and a different read.** Whether the red is the change's is
judged on the check and its failed job, above. Whether the head's one re-run is already spent is
judged on the head's own `pull_request` workflow runs and their attempts, because GitHub's rollup
carries the latest run of each check name, so a superseded failure cannot appear there at all:

```bash
gh api "repos/<o>/<r>/actions/runs?head_sha=<head>&event=pull_request&per_page=100" \
  --jq '.workflow_runs[] | [.id,.name,.run_attempt,.conclusion] | @tsv'
```

Once the run that holds the failed job has completed, and before any re-run of it, read the spend
over every `pull_request` run of the head: the failing run on its first attempt says nothing, since
another run's second attempt spends the head's one re-run too. A run of that head whose
`run_attempt` is above 1 is a re-run already pressed. Whether it spent the head's one re-run is
read on each earlier attempt of that run, `<n>` from 1 to one below the run's `run_attempt`:

```bash
gh api --paginate "repos/<o>/<r>/actions/runs/<run-id>/attempts/<n>/jobs?per_page=100" \
  --jq '.total_count, (.jobs[] | [.name,.conclusion] | @tsv)'
```

Count the jobs it returned against `total_count`, because a run can hold more than 100 jobs; a count
that falls short is a read to make again, never a decision. The re-run after an earlier attempt
spends nothing only when every job of that attempt that did not succeed or skip is `cancelled`, or
is an aggregate that failed over such a lane. Any other conclusion spends it, one this page does not
name included. A lane GitHub cancelled at two attempts of the same run is read as `failure`, so a
cancelled lane gets one re-run that spends nothing, and the head's one re-run is still left after
it. That bound is what holds a hung job: GitHub cancels a job that runs past its `timeout-minutes`,
so a lane that hangs again reads `cancelled` again. `event=pull_request` keeps the by-hand
`workflow_dispatch` runs out, as the counted set does.

An aggregate is resolved to its lane, as step 2 of *Which red is the change's* says, before the
spend is judged. The reviewer's role prompt gives it one more re-run, of a required check absent
from the counted set or `completed` as `cancelled` there whose job in a `pull_request` run of the
head was cancelled or never ran. That re-run neither spends the head's re-run nor is spent by it,
and an aggregate that reads `failure` over a lane GitHub cancelled is that cancelled lane. A failed
job of a completed `pull_request` run of the head is never that case: it is a red, which this page
decides. A failed job of the head's own `pull_request` run is acted on once that run has completed,
whether or not the counted set lists its check yet, and no run that is not a `pull_request` run of
the head is acted on at all. A `gh run rerun <run-id> --failed` call pressed while a job of that
run, resolved to its lane, is `failure` spends the head's re-run, whatever else that call clears.

When the re-run is spent, the role re-runs nothing. The reviewer requests changes, names the
check, and says the head's one re-run is spent. The implementer records the red, tells the
architect, and completes its phase, and its record and its message to the architect name the
check and say the head's one re-run is spent; the reviewer judges the red in its round.

**An empty counted set.** A rollup of `null`, which `gh pr checks` prints as no checks reported,
read first in a turn, is waited on like any other absence, as *The act and the wait* says. A
rollup of `null` read on the wake after that wait means GitHub counts no context at that head: a
check with no counted run there has no re-run to spend, and there is nothing to re-run.

**An unread counted set is not an empty one.** After a push the counted set can lag, so a role that
reads it at once may find the failing check pending, queued, in progress or absent. A read made
seconds after a push never shows that the head has no red: a check the role was told of that no
`pull_request` run of the head holds is waited for once, as *The act and the wait* says.

## The act and the wait

**A red is acted on in the head's own `pull_request` run once that run has completed, however the
role learned of it.** A task that names a failing check, a notice, another role's word or the
counted set tells of a red; the head's `pull_request` runs (the spend read above) hold its failed
job. GitHub fills a head's rollup over seconds and minutes, and that lag decides nothing. A run
that is not a `pull_request` run of the head, a by-hand `workflow_dispatch` run among them, counts
for no pull request, and a re-run pressed there spends the head's one re-run on a run nobody
counted. While the run is queued or in progress, and once for a check no `pull_request` run of the
head holds yet, the role presses no re-run and ends no round on it: it subscribes to the pull
request's topic and ends the turn, as below. The wait ends one of three ways:

- The run that holds the failed job completes: the role acts on it.
- After that one wait no `pull_request` run of the head holds the check, and the counted set settles
  with every check it holds `completed` and that one absent, or the head moves. Then the role
  records the red, where it learned of it and what the counted set carries, re-runs nothing, goes
  on with its round, and tells the architect.
- No wake comes at all: the bound is the phase reminder or the daemon's backstop. While its
  process runs (under `runtime: tmux`, and under `runtime: kubernetes` with idle release off), a
  phase worker that ended its turn with a WAITING line gets the phase reminder 15 minutes later
  (`LEGION_STALL_WAITING_MS`). Where idle release is on, the pod goes first, and the bound is the
  daemon's backstop about an hour after the release. The role then reads the head's runs and the
  counted set once more and takes whichever of the first two the reading gives.

The role runs `gh run rerun <run id> --failed` in the head's own `pull_request` run. In that turn:

1. It subscribes to the pull request's own topic, `github.<owner>.<repo>.pr.<n>.>`, with the Envoy
   subscribe tool its host gives it (`skill://envoy`, *Wait for CI or a merge*).
2. It may go on with work that reads none of the head's checks or runs, subagents included.
3. It ends the turn, with a last line that starts WAITING and names the re-run it waits on.

The wait is the end of the turn, never time inside it: the role reads the counted set again only on
the wake the settlement brings. Any other wake, a subagent's result or a message, is no leave to
read: the role finishes that work and ends the turn WAITING again. Where no settlement comes, the
bound is the same as before a press: on the phase reminder or the daemon's backstop the role reads
the counted set once. A re-run still queued or in progress then is waited on again, with the turn
ended WAITING, and a completed one is read as the settlement would be. A read of the head's checks
or runs in the turn that pressed the re-run is the poll this forbids, and so are `gh run watch` and
`gh pr checks --watch`. Under `runtime: kubernetes` the daemon may release the issue's pod while the
role waits, and the wake brings it back with its session, its phase and its task kept.

The settlement wakes the role in that session, and one read of the head's checks then is no poll:

- Green: the round goes on, and the role records that it re-ran, with the run and the attempt.
  Before it completes its phase, it runs the three steps of *What becomes of the record* below.
- Red again: the reviewer requests changes and names the check. The implementer records the red,
  tells the architect, and completes its phase, naming the check; the reviewer judges the red in
  its round.
- If no run at the head holds the failing check, the role re-runs nothing and tells the architect.

## When the cause is another tree's, so no re-run clears it

A fetch that answered 5xx, a registry that refused, a runner that ended: on its first red each of
these takes its one re-run, as *Which red is the change's* says, and most clear there. This section
takes over in two cases: the re-run came back red at a step that ran **before any test of the
change** — a setup or install step, a toolchain, a runner, a registry — or the lane's own output
already shows the defect is in a file another tree owns. A failing test is never the second case,
whatever package holds it: one that is not the change's takes the one re-run on its first red, and
on its second red it is handled as *Red again* above says, never as an external cause. In both cases
a further run of the lane meets the same defect, so no re-run ends it. The role records the external
cause in the same record this page asks for — the lane, the error, the head, and the change it
waits on — tells the architect, and ends its round where it stands: a reviewer submits the
`REQUEST_CHANGES` naming the red, since that ends a reviewer's round. It waits for nothing here.
The fix is another tree's: it reaches this head only through a forward merge. A role that waits
holds its phase and its pod for that, and the architect, who arranges it, has not seen the record.

No role pushes a commit to make a new head for its own sake, and no role patches another tree's
file to clear its own lane. The work waits until the repository's default branch carries the
fix, and the forward merge of that branch is both the push and the next attempt
(`skill://legion-worker/references/conflicts-and-rewrites.md`, *Reintegrating the base*).

Whether the base carries it is read, never assumed from a key: the role names the repository
that holds the path or the commit, fetches first, since an unfetched clone answers no to every
ancestry question, and then reads it. For a file: `test -e <path>` before
`git log --oneline <base>..origin/main -- <path>`, because an absent path and an unchanged path
give the same empty log and no exit status tells them apart. For a commit: the exit status of
`git merge-base --is-ancestor <sha> <head>`, where 0 is carried, 1 is not, and anything else is
cannot compare, so fetch and read again. Two repositories of one fleet carry the same pull request
numbers, so a key is no answer and a file read is a date.

## What becomes of the record

A green re-run of a red in no test, such as a download that answered 5xx or a runner that ended,
owes the record before the press and none of these three steps: the census is one issue per test,
and a fault of a download or a runner belongs to the tree that owns that step. After a green re-run
whose log read failed, the role knows the job but not the test's name: it makes no search, posts no
comment, and sends step 3's message to the controller, naming the job and saying the log could not
be read. After a green re-run of a failing test, and before it completes its phase, the role runs
these three commands in this order. A red cleared and never written up is a flaky test nobody fixes.
A re-run that comes back red is handled as *Red again* above says and owes no occurrence.

1. `dispatch search --query '<the failing test's full name>'`, which finds the issue that tracks
   that test. An issue a record of an earlier red already cites is read again before it is cited
   again — whether it is open, whether its fix has merged, whether it is still the owner — since
   a stale key reads exactly like a tracked defect and becomes the resting place.
2. `dispatch comment --issue <that KEY> --body-file -`, the occurrence on that issue, unless it
   reads `done`: the test, the job, the head, the failing run and the green re-run.
3. The message to the controller: an `envoy_publish` to the controller topic the role's
   `Legion addressing` line names, with `expects_reply: false`. When the publish answers that no
   controller took it (`skill://legion-worker/references/findings.md`, *When no controller takes
   it*) and step 2 had nowhere to go, the role posts the same text as a `dispatch comment` on the
   report issue, its first line `Clear defect from <role token>`, which the controller reads at
   its next start. It opens no ask: a person has nothing to decide.

Where step 1 finds no issue, or finds one that reads `done`, step 2 has nowhere to go: the role
says so in step 3's message instead, naming the test, the job, the head and both results. It
files no issue itself in either case. The controller keeps the census, one issue per test
(`skill://legion-controller/references/finding-asks.md`).
