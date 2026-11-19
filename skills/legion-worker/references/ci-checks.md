# A check CI proves

Part of `skill://legion-worker`.
Read it when a check of the issue's Verification document carries `ci_check`, before you run, record, judge or write the line of any check.
Every path it cites is in cybermonklabs/legion.

## What it is

A `check` block can carry `ci_check`: the name of the GitHub check run that proves the check at the head.
Such a check has no `run`, its `when` is `before_merge`, and it names its `negative_control`.
Through the documents API, Dispatch refuses a write that breaks one of these rules, or that gives `ci_check` to a check that already holds a recorded result.
A write in the live editor lands, and the next settlement removes that `ci_check` and names the rule in the issue's Activity.

## No role runs it

A check carrying `ci_check` is CI's.
No role runs its work in its own pod, and no role records a result for it: Dispatch refuses such a result with `409 CHECK_PROVED_BY_CI`.
The CI job stays a job like any other, and the rules for a red CI job apply to it.
`dispatch results --issue <issue>` reads the named check run at the head of the issue's pull request.
It prints the one result it derives from that run, the same `A pass stands:` line a recorded check gets, and the line for the pull request body:

```
- <id> (before_merge): <verdict> at <sha> by ci (CI run <name>, <conclusion>), <currency>. <text>
  A pass stands: <yes, from ci | no | not known>.
  Pull request line: - Check <id> (before_merge): <verdict> at <sha>, from CI run <name>. <words>
```

When the run gives no result, the command prints the case in place of a verdict:

```
- <id> (before_merge): no result: <detail> <text>
  A pass stands: <no | not known>.
  Pull request line: - Check <id> (before_merge): CI run <name> has no result (<case>)
```

The case is one of these:

- `not_completed`: the run is still going.

- `absent`: no run of that name is at the head.

- `no_head`: Dispatch holds no pull request head of the issue.

- `neutral`, `skipped` or `cancelled`: the run concluded and proved nothing.

Three more cases have the state `unread`.
In each of them, the command prints `A pass stands: not known`.

- `github_unreadable`: Dispatch could not read GitHub.

- `head_not_read`: a read of GitHub is owed for the issue's newest pull request, so Dispatch does not know its head yet and reads no check run.
  It ends when a reconcile pass or the pull request's next event reads that pull request from GitHub.

- `issue_not_known`: Dispatch does not know yet whether a pull request of the issue's repository is the issue's.
  Dispatch has not read the commit messages of that pull request, and nothing else it read names an issue of that repository.
  The case applies when that pull request comes before the issue's newest head (open before merged, then the higher number), or when the issue has no head.
  Dispatch then reads no check run.
  It ends when a reconcile pass reads the commit messages of that pull request.

Dispatch gives `head_not_read` and `issue_not_known` only where its GitHub App can read the repository.
None of the three cases is a pass or a fail, and the next read tries again.
The tester and the reviewer treat each of them as a run that is still going (below).
Each read is made at the head, so a push needs no write from any role.

## The tester

Read `dispatch results --issue <issue>` first.
Then run and record only the checks that carry no `ci_check` and whose `run` is no whole test suite, and the proofs on a real surface that CI cannot make.
A check with `ci_check` needs no pass from the implementer.
It is an entry in `failures` only when its derived result is a fail, or its run concluded `skipped`, `neutral` or `cancelled`.

## The `Checks` line

The implementer writes the line of each such check in the pull request body's `Checks` block.
It copies the `Pull request line` that `dispatch results` prints.
While the named run gives no result, that line is the no-result line.
The next role that writes the body reads `dispatch results` again and puts the derived line in its place.

## A run that is still going

A named run that has not concluded is no test failure, and it is the fault of no role.
The tester writes in its `test` handoff's `implementerProof.how` that the run is still going.
The handoff gets no new field, and the tester fails no round for the run.

The reviewer waits for that run by its name, as it waits for a required check that GitHub shows `queued` or `in progress`.
Until the run concludes, the reviewer does not approve and does not request changes for that check.
The named run is usually not one the base branch requires, so the wait on required checks does not cover it.

## A job held back from an ordinary pull request

A `ci_check` can name a job that the repository runs on a pull request only on request:
on cybermonklabs/legion, the slow suites and the full browser lane run only under the label `full-ci` (`.github/workflows/ci.yaml`).
Such a check is proved in CI too, never in the pod.
Without the label, such a job is at the head and reads `skipped`, which leaves the check no pass.
The implementer adds that label and then pushes, because the workflow starts on a push and not on a label, and the run at that head is the check's proof.
The label stays on for as long as a `ci_check` of the issue names a held-back job, because every later head (a review fix, a forward merge) needs its own run;
where it came off, it goes on again before the next push.
The tester takes the check's result from that run, as from any other.

## A name that no run answers

When the head's checks have settled and no run of the named name answers, read the pull request's merge state first (*Never waiting on CI* in `skill://legion-worker/references/pr-body.md`):
a head that conflicts with its base gets no `pull_request` run, and the forward merge cures it.
A held-back job (above) is not absent without the label: it reads `skipped`, and the label cures it.
Otherwise the words of the check are wrong.
The tester and the reviewer each send this to the architect as a finding, with `envoy_publish` to its role topic.
The architect corrects the name or gives the check a command.
Neither role fails the round or requests changes for it, because the change under review is not what is wrong.

## What the evidence is

The CI job that `ci_check` names is the proof of that check at the head, and no role repeats it.
This is true also when the job is a unit or integration suite, which proves no other check.
The limit: such a check claims only that the named job passed at that head.
It never takes the place of the proof a tester makes on a real surface.
The tester continues to prove the surface with the checks that CI cannot run.

## How the architect writes one

The architect names the job and its `negative_control` by three rules:
[A check CI proves](skill://legion-architect/references/design-gate.md#a-check-ci-proves) in `skill://legion-architect/references/design-gate.md`.
