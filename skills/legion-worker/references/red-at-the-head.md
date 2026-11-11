# A red check at the head of the pull request

Part of `skill://legion-worker`. Read it when a check is red at the head of the issue's pull
request and a phase of that issue is open. Every path it cites is in cybermonklabs/legion.

A red the change did not make costs the tree a whole lap today: an implementing round, a new CI
pass, a new testing round, and often a new review round. The role that already reads that head
clears such a red where it stands. It writes the failure down first. Then it re-runs the failed
jobs once.

## Who may re-run

- The holder of the issue's phase presses the re-run: the tester in `testing`, the reviewer in
  `reviewing`, the implementer in `implementing`.
- The merger, the architect in `accepting` and an implementer that runs retro re-run nothing.
- A role that is told of a red whose phase it does not hold re-runs nothing.

## The record comes first, never after

Before it presses the re-run, the role records on the Dispatch issue:

- the repository and the pull request
- the head commit
- the check and the run id
- the attempt and the failed job
- the failing test or step, with the first line that states the failure

The role reads that first line with `gh run view <run-id> --log-failed`. A read that fails is
recorded as failed, with the error, and the role makes the re-run all the same.

The record is a handoff record of the role's own phase, written with
`dispatch handoff-write --issue <issue> --phase <test|review> --body-file <file>` before the
`gh run rerun` call, never after it. Its body carries the field `redAtTheHead`, a list with one
entry per red, each entry naming all eight facts above. That record is a version of its own: the
role writes the phase's full record later, with `redAtTheHead` in it again, and `handoff_complete`
reports the newest version. A re-run pressed before Dispatch has recorded the first version loses
the failure, which is what this page exists to keep.

The reviewer's round carries the same record in the body of the one review it submits at the end
of the round, because a review cannot be written before the re-run. No file under
`packages/daemon/internal/prompts/roles/` names that field. This page is where the tester and the
reviewer read it.

## Which red is the change's

Four readings decide it:

1. A check that judges the pull request itself rather than its code, such as the title check, is
   the change's, and it gets no re-run. The one exception is a check whose own output names a
   comment or a review as the cause. No role may edit a comment or a review, so the role tells
   the architect, and that red is re-run by nobody.
2. An aggregate check is resolved to the lane that failed it. That lane is judged in its place.
3. A failing test is the change's when its own package is one the pull request changes: the Go
   package for a Go test, the directory of the test file for a Bun or a Playwright suite.
4. A failure in no test is the change's only when the failing step reads a file the pull request
   changes. Such a file is the workflow file that job runs, the manifest or the lock file the step
   installs from, or the Dockerfile it builds.

These are therefore not the change's:

- a fetch that answered 5xx
- a name that did not resolve
- a registry, a cache or a package index that refused the step
- a runner that ended
- a job GitHub cancelled
- a job its own time limit killed

A lint, type, build or format failure of the change's own code is the change's.

## One re-run belongs to the head, and it is counted on the counted set

**The counted set** is what GitHub counts for the pull request, never every run at the commit.
The role reads it one of two ways. At the pull request's current head:

```bash
gh pr checks <n> --required
```

At a named head, which is what a role holds after a push, the rollup of that commit:

```bash
gh api graphql -F oid=<head> -f query='query($oid:GitObjectID!){repository(owner:"<owner>",name:"<repo>"){object(oid:$oid){... on Commit{statusCheckRollup{state contexts(first:100){totalCount pageInfo{hasNextPage} nodes{__typename ... on CheckRun{name databaseId status conclusion checkSuite{app{databaseId} workflowRun{event databaseId}}} ... on StatusContext{context state}}}}}}}}'
```

paged on `pageInfo.hasNextPage`, because the context list is capped at 100 a page and a real head
of this repository carries 65. The read costs 1 point of a limit of 10,000 an hour.

**Do not judge a red from `repos/<o>/<r>/commits/<head>/check-runs`.** That endpoint answers
every check run at the commit, a by-hand `workflow_dispatch` run's among them, which GitHub
counts for no pull request. Measured in this repository on 2026-11-11: at `9b728582e2762eb62b9c582d7d843df772b951ff`
it answers 113 check runs where the pull request's rollup holds 57, the 56 left out belonging to
two by-hand runs at that commit (dispatch://INFRA-3035).

**The spend.** The head's one re-run is spent when a check of the counted set carries a run whose
conclusion was `failure` **and a later run of that same check**: the earlier failure was
superseded by a run somebody already pressed. The red the role is judging is not itself that
case, since it has one run at the head and no later one. A failure outside the counted set spends
nothing, because nobody counted it. The reviewer's existing re-run of a cancelled or absent
required check (`skill://legion-worker/references/merge-gate.md`) neither spends the head's
re-run nor is spent by it, because it leaves no such superseded failure. A
`gh run rerun <run-id> --failed` call pressed while a check of the counted set is `failure`
spends the head's re-run, whatever else that call clears.

When the re-run is spent, the role re-runs nothing. It fails the round, or it requests changes,
and it names the check.

**An empty counted set.** A rollup of `null`, which `gh pr checks` prints as no checks reported,
means GitHub counts no context at that head. A check with no counted run there has no re-run to
spend: there is nothing to re-run and nothing is spent. Measured at
`ebb186ba575924b3a341769ad2168387d056724f`, where the commit endpoint answers 55 runs of a
by-hand `workflow_dispatch` and the rollup is `null`.

**An unread counted set is not an empty one.** After a push the counted set can lag, so a role
that reads it at once may find the failing check pending, queued, in progress or absent. The
spend test is decided only where the counted set reports that check `completed`. In every other
state nothing is decided yet: the role waits for the pull request's checks event, as it waits for
a re-run below, and reads the counted set again when the wake brings it back.

## The act and the wait

The role runs `gh run rerun <run id> --failed` in the head's own `pull_request` run. Then, in the
same turn, it does these two things and nothing else:

1. Subscribes to the pull request's own topic, `github.<owner>.<repo>.pr.<n>.>`, with the Envoy
   subscribe tool its host gives it (`skill://envoy`, *Wait for CI or a merge*).
2. Ends the turn, with a last line that starts WAITING and names the re-run it waits on.

The wait is the end of the turn, never time inside it. Re-reading the head to see whether the
re-run has settled is the poll this forbids, with a `sleep` in it or without one, and so are
`gh run watch` and `gh pr checks --watch`. Under `runtime: kubernetes` the daemon may release the
issue's pod while the role waits, and the wake brings the role back with its session, its phase
and its task kept.

The settlement wakes the role in that same session. One read of the head's checks then is the
result of the wait and no poll at all:

- Green: the round goes on, and the role records that it re-ran, with the run and the attempt.
- Red again: the round fails, or the reviewer requests changes, and the check is named.
- Where no run at the head holds the failing check, the role re-runs nothing and tells the
  architect.

## When the re-run does not clear it, and the cause is another tree's

A red the change did not make is sometimes not the re-run's to clear: the failing step shows a
defect another tree owns, and a second run of the same lane meets the same defect. The role then
records the external cause in the same record this page already asks for — the lane, the error,
the head, and the change it waits on — tells the architect, and completes nothing.

No role pushes a commit to make a new head for its own sake, and no role patches another tree's
file to clear its own lane. The work waits until the repository's default branch carries the
fix, and the forward merge of that branch is both the push and the next attempt
(`skill://legion-worker/references/conflicts-and-rewrites.md`, *Reintegrating the base*).

The case is live: on theorem-labs/agent-infra#3160 at head `8fc15cf3`, the `route-tests` lane
failed on the runner with `unable to write file spend/README.md`, exit 128, before a test of the
change had run, and the required aggregate was red only because of it. The implementer had
nothing of its own to change.

## The daemon's half, while cybermonklabs/legion#2539 is open

A red by a check the base branch requires still ends the round, whatever the role has found. A
role whose phase ends while it waits for its re-run has already written the record. That record
goes to the implementer with the work. The role starts nothing new.

## What becomes of the record

After the re-run settles and before it completes its phase, the role runs these three commands in
this order. A red cleared and never written up is a flaky test nobody fixes, which is the cost
this page is paid to stop.

1. `dispatch search --query '<the failing test's full name>'`, which finds the issue that tracks
   that test.
2. `dispatch comment --issue <that KEY> --body-file -`, the occurrence on that issue, whatever
   its status: the test, the job, the head, and both results, the failing run and the green
   re-run.
3. The message to the controller, by the route
   `skill://legion-worker/references/findings.md` gives, which is an `envoy_publish` to the
   controller topic the role's `Legion addressing` line names.

Where step 1 finds no issue, or finds one that reads `done`, step 2 has nowhere to go: the role
says so in step 3's message instead, naming the test, the job, the head and both results. It
files no issue itself in either case. The controller keeps the census, one issue per test
(`skill://legion-controller/references/finding-asks.md`).

## The merge queue, for the merger

A `failed_checks` ejection at an unchanged head is queued once more. The work goes back only on a
second such ejection of that head. A merge-group run is never re-run. GitHub does re-run one, but
the re-run puts nothing back in the queue, and GitHub deletes the entry's branch:
`GET /git/ref/heads/gh-readonly-queue/main/pr-<n>-<sha>` answers 404, measured on
cybermonklabs/legion#2811. This rule acts once dispatch://INFRA-714 gives the merger the merge.
