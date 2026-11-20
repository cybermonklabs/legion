# A check CI proves

Read it when a check of the issue's Verification document carries `ci_check`, before you run, record, judge or write the line of any check.

## What it is

`ci_check` names the GitHub check run that proves the check at the head. Such a check has no `run`, its `when` is `before_merge`, and it names its `negative_control`. The architect writes it as `skill://legion-architect/references/design-gate.md` says.

## No role runs it

A check carrying `ci_check` is CI's. No role runs its work in the pod or records a result for it: Dispatch refuses one with `409 CHECK_PROVED_BY_CI`. The CI job stays a job like any other, and the rules for a red CI job apply to it. `dispatch results --issue <issue>` reads the named run at the head of the issue's pull request and prints the one result it derives, its `A pass stands:` line, and the line for the PR body:

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

The case is one of:

- `not_completed`: the run is still going.
- `absent`: no run of that name is at the head.
- `no_head`: Dispatch holds no pull request head of the issue.
- `neutral`, `skipped` or `cancelled`: the run concluded and proved nothing.
- `github_unreadable`: Dispatch could not read GitHub; treat it as a run still going.

Each read is made at the head, so a push needs no write from any role.

## What each role does

- The implementer copies each such check's `Pull request line` into the `Checks` block. The next role that writes the body reads `dispatch results` again and replaces a no-result line.
- The tester reads `dispatch results --issue <issue>` first. Then it runs and records only the checks that carry no `ci_check` and whose `run` is no whole test suite, and the proofs on a real surface that CI cannot make. A `ci_check` check goes in `failures` only when its derived result is a fail, or its run concluded `skipped`, `neutral` or `cancelled`. A run still going fails no round: say so in `implementerProof.how`; the handoff gets no new field for it.
- The reviewer waits for a run still going by its name, and neither approves nor requests changes for that check until it concludes.
- A job the repository runs on a pull request only under the label `full-ci` (on cybermonklabs/legion, the slow suites and the full browser lane) reads `skipped` without it. The implementer adds the label, then pushes, because the workflow starts on a push and not on a label. The label stays on while a `ci_check` of the issue names such a job, because every later head (a review fix, a forward merge) needs its own run; where it came off, it goes on again before the next push.

## A name that no run answers

When the head's checks have settled and no run of the name answers:

1. Read the merge state first (`gh pr view <n> --json mergeStateStatus`): a `DIRTY` head gets no `pull_request` run, and the forward merge cures it.
2. A held-back job reads `skipped`, not absent: the label cures it.
3. Otherwise the check's words are wrong. The tester and the reviewer each send it to the architect as a finding with `envoy_publish`; neither fails the round nor requests changes for it. The architect corrects the name or gives the check a command.

## What the evidence is

The named job is the check's proof at the head, and no role repeats it, even when it is a unit or integration suite. It proves only that the job passed there: the tester still proves the real surface with the checks CI cannot run.
