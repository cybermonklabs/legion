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
counted as such.

The role reads that first line with `gh run view <run-id> --log-failed`. A read that fails is
recorded as failed, with the error, and the role makes the re-run all the same. Where the log
can be read and holds no line that states the failure, the record says that, naming the step and
what the log did hold, rather than leaving the field empty or writing a line the log does not
carry: a log of `677 bytes, none printed` hid an apt failure's own diagnosis for a whole fleet
until a round printed the body (dispatch://INFRA-1545).

The record is a handoff record of the role's own phase, written with
`dispatch handoff-write --issue <issue> --phase <implement|test|review> --body-file <file>` before the
`gh run rerun` call, never after it. Its body carries the field `redAtTheHead`, a list with one
entry per red, each entry naming all eight facts above. That record is a version of its own: the
role writes the phase's full record later, with `redAtTheHead` in it again, and `handoff_complete`
reports the newest version. A re-run pressed before Dispatch has recorded the first version loses
the failure, which is what this page exists to keep.

The reviewer writes that handoff record before the re-run as every other role does, and repeats
it in the body of the one review it submits at the end of the round, since a review cannot be
written before the re-run. No file under `packages/daemon/internal/prompts/roles/` names that
field. This page is where the implementer, the tester and the reviewer read it.

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
gh api graphql --paginate -F oid=<head> -f query='query($oid:GitObjectID!,$endCursor:String){repository(owner:"<owner>",name:"<repo>"){object(oid:$oid){... on Commit{statusCheckRollup{state contexts(first:100, after:$endCursor){totalCount pageInfo{hasNextPage endCursor} nodes{__typename ... on CheckRun{name databaseId status conclusion checkSuite{app{databaseId} workflowRun{event databaseId}}} ... on StatusContext{context state}}}}}}}}'
```

`--paginate` with the `$endCursor` variable and `endCursor` in the page info is what follows
`hasNextPage`, because the context list is capped at 100 a page; a real head of this repository
carries 65. The read costs 1 point of a limit of 10,000 an hour.

**Do not judge a red from `repos/<o>/<r>/commits/<head>/check-runs`.** That endpoint answers
every check run at the commit, a by-hand `workflow_dispatch` run's among them, which GitHub
counts for no pull request. Measured in this repository on 2026-11-11: at `9b728582e2762eb62b9c582d7d843df772b951ff`
it answers 113 check runs where the pull request's rollup holds 57, the 56 left out belonging to
two by-hand runs at that commit (dispatch://INFRA-3035).

**The spend is a different question, and a different read.** Whether the red is the change's is
judged on the counted set, above. Whether the head's one re-run is already spent is judged on
the head's own `pull_request` workflow runs and their attempts, because GitHub's rollup carries
the latest run of each check name, so a superseded failure cannot appear there at all:

```bash
gh api "repos/<o>/<r>/actions/runs?head_sha=<head>&event=pull_request&per_page=100" \
  --jq '.workflow_runs[] | [.id,.name,.run_attempt,.conclusion] | @tsv'
```

A run of that head whose `run_attempt` is above 1 is a re-run somebody already pressed, and the
head's one re-run is spent. `event=pull_request` keeps the by-hand `workflow_dispatch` runs out,
as the counted set does. The reviewer's existing re-run of a cancelled or absent required check
(`skill://legion-worker/references/merge-gate.md`) neither spends the head's re-run nor is spent
by it, since it presses no run of a check that was `failure`. A `gh run rerun <run-id> --failed`
call pressed while a check of the counted set is `failure` spends the head's re-run, whatever
else that call clears.

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

**A red the role has only been told of is not yet a red to act on.** A task that names a failing
check, a notice, or another role's word is not the counted set. Until the counted set reports
that check `completed`, the role presses no re-run and ends no round on it: it subscribes to the
pull request's topic and ends the turn, as below, and reads the counted set again when the wake
brings it back. GitHub fills a head's checks over seconds and minutes, so a counted set that is
silent about a check it has been told of is one that has not caught up, and a re-run pressed
there spends the head's one re-run on a run nobody counted. The wait ends three ways: the
counted set reports that check, and the role acts on it; the counted set settles with every
check it holds `completed` and that one absent, or the head moves, and the role then records
what it was told and what the counted set carries, re-runs nothing, goes on with its round and
tells the architect, as the last bullet of this section says — and that second way counts only
after the role has waited at least once, since a counted set read seconds after the task looks
the same whether it has settled without the check or has not reached it yet, so the first read
of a turn never ends the wait; or no wake comes at all, and the
bound is the role's own next wake, which for a phase worker that ends its turn with nothing else
coming is the daemon's backstop about an hour after its pod goes idle, and the role then reads
the counted set once more and takes whichever of those two the reading gives.

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
- Red again: the round fails, or the reviewer requests changes, and the check is named. A test
  that fails twice is this case, never the external cause below.
- Where no run at the head holds the failing check, the role re-runs nothing and tells the
  architect.

## When the cause is another tree's, so no re-run clears it

**A test that fails and fails again is the round's failure, never an external cause.** The role
records it and the round fails, as *Red again* above says. This section is the other case, and
the difference is where the lane broke: a step that failed **before any test of the change ran**
— a setup or install step, a toolchain, a runner, a registry, or a file another tree owns — or
one whose own output shows the defect belongs to another tree.

There a second run of the lane meets the same defect, so the re-run does not end it. The role
records the external cause in the same record this page already asks for — the lane, the error,
the head, and the change it waits on — tells the architect, and ends its round where it stands:
a tester completes its phase with a failing verdict, and a reviewer submits the
`REQUEST_CHANGES` naming the red, since that is how a reviewer's round ends. It waits for
nothing here. The daemon moves no phase on a red, and the fix is another tree's: it reaches this
head only through a forward merge, which is the architect's to arrange. A role that waits holds
its phase and its pod for that, and the architect, who arranges it, has not seen the record.

No role pushes a commit to make a new head for its own sake, and no role patches another tree's
file to clear its own lane. The work waits until the repository's default branch carries the
fix, and the forward merge of that branch is both the push and the next attempt
(`skill://legion-worker/references/conflicts-and-rewrites.md`, *Reintegrating the base*).

Whether the base carries it is read, never assumed from a key: the role names the repository
that holds the path or the commit, fetches first, since an unfetched clone answers no to every
ancestry question, and then reads it. For a file: `test -e <path>` before
`git log --oneline <base>..origin/main -- <path>`, because an absent path and an unchanged path
give the same empty log and no exit status tells them apart. For a commit: the exit status of
`git merge-base --is-ancestor <sha> <head>`, where 1 is not carried and anything else is cannot
compare, so fetch and read again. Two repositories of one fleet carry the same pull request
numbers, so a key is no answer and a file read is a date.

The case is live: on theorem-labs/agent-infra#3160 at head `8fc15cf3`, the `route-tests` lane
failed on the runner with `unable to write file spend/README.md`, exit 128, before a test of the
change had run, and the required aggregate was red only because of it. The implementer had
nothing of its own to change. A red again of the same test is not that case.

## What the daemon does with a red

A red moves no phase. When CI settles at the pull request's head, the daemon sends one message to
the tree's architect, the worker of the issue's current phase and the implementer, naming each
check of that settlement with what it came to — failed, cancelled or succeeded — and it judges
none of them (`tellChecks`, `packages/daemon/internal/workflow/review.go`). A role the same
settlement woke from a released pod reads it in its wake task instead. So nothing but the role
ends a round that a red the change did not cause would otherwise hold, and this page is the rule
it ends it by.

## What becomes of the record

After the re-run settles green, and before it completes its phase, the role runs these three
commands in this order. A red cleared and never written up is a flaky test nobody fixes, which is
the cost this page is paid to stop. A re-run that comes back red is the round's failure (*Red
again* above) and owes no occurrence.

1. `dispatch search --query '<the failing test's full name>'`, which finds the issue that tracks
   that test. An issue a record of an earlier red already cites is read again before it is cited
   again — whether it is open, whether its fix has merged, whether it is still the owner — since
   a stale key reads exactly like a tracked defect and becomes the resting place: one was carried
   as the remedy for four rounds after it had stopped being one (dispatch://INFRA-1192).
2. `dispatch comment --issue <that KEY> --body-file -`, the occurrence on that issue, whatever
   its status, unless it reads `done`: the test, the job, the head, and both results, the failing
   run and the green re-run.
3. The message to the controller, by the route
   `skill://legion-worker/references/findings.md` gives, which is an `envoy_publish` to the
   controller topic the role's `Legion addressing` line names.

Where step 1 finds no issue, or finds one that reads `done`, step 2 has nowhere to go: the role
says so in step 3's message instead, naming the test, the job, the head and both results. It
files no issue itself in either case. The controller keeps the census, one issue per test
(`skill://legion-controller/references/finding-asks.md`).

## The merge queue, for the merger

The merger's rule is in `skill://legion-worker/references/merge-gate.md` (*A `failed_checks`
ejection at an unchanged head is queued once more*): no role re-runs a merge-group run, and no
re-run of this page's kind belongs to one. Keeping it in one place keeps the two from drifting,
since the guard pins that page's wording and not this one's.
