# A check CI proves

Read it when a check of the issue's Verification document carries `ci_check`, or a pull request needs a lane label (`full-ci`, `lane:<output>`), before you run, record, judge or write the line of any check.

## What it is

`ci_check` names the GitHub check run that proves the check at the head.
Such a check has no `run`, its `when` is `before_merge`, and it names its `negative_control`.
The architect writes it as `skill://legion-architect/references/design-gate.md` says.

## No role runs it

A check carrying `ci_check` is CI's.
No role runs its work in the pod or records a result for it: Dispatch refuses one with `409 CHECK_PROVED_BY_CI`.
`dispatch results --issue <issue>` reads the named run at the head of the issue's pull request and prints the one result it derives, its `A pass stands:` line, and the line for the PR body:

```
- <id> (before_merge): <verdict> at <sha> by ci (CI run <name>, <conclusion>), <currency>. <text>
  A pass stands: <yes, from ci | no | not known>.
  Pull request line: - Check <id> (before_merge): <verdict> at <sha>, from CI run <name>. <words>
```

When the run gives no result, the line reads `CI run <name> has no result (<case>)`, the case one of:

- `not_completed`: the run is still going.

- `absent`: no run of that name is at the head.

- `no_head`: Dispatch holds no pull request head of the issue.

- `neutral`, `skipped` or `cancelled`: the run concluded and proved nothing.

- `github_unreadable`: Dispatch could not read GitHub; treat it as a run still going.

## What each role does

- The implementer copies each such check's `Pull request line` into the `Checks` block.
  The next role that writes the body reads `dispatch results` again and replaces a no-result line.

- The tester runs and records only the checks with no `ci_check`.
  A `ci_check` check goes in `failures` only when its derived result is a fail, or its run concluded `skipped`, `neutral` or `cancelled`.
  A run still going fails no round: say so in `implementerProof.how`.

- The reviewer waits for a run still going by its name, and neither approves nor requests changes for that check until it concludes.

- A job held back on a pull request runs there only under the label `full-ci` or its own `lane:<output>`.
  On cybermonklabs/legion that is any of the fifteen outputs `.github/ci-notes/ci.md`'s lane-label entry lists, `daemon_go_tests`, `envoy_go`, `pi_legion`, `test` and the full browser lane among them; when a run holds one back, the ci-ok summary lists it on a `not run at this head` line.
  The implementer adds the label, then pushes, or closes and reopens the pull request when there is nothing to push: a label on its own starts no run.
  Before asking for review, the implementer reads the ci-ok summary's `not run at this head` lines: a lane the pull request needs that is listed there means the label did not take, so push or reopen again.
  The label comes off once the run it was added for passes, unless a `ci_check` of the issue names such a job, which keeps it on.
  The reviewer reads the same lines before approving, whether or not a check carries `ci_check`: a line listing a job that a `ci_check` names, or the one job that runs a test the change adds or edits, takes a request for changes naming the label and the push or reopen that runs it, not an approval on that `ci-ok`.

## A name that no run answers

When the head's checks have settled and no run of the name answers:

1. Read the merge state first (`gh pr view <n> --json mergeStateStatus`): a `DIRTY` head gets no `pull_request` run, and the forward merge cures it.

2. A held-back job reads `skipped`, not absent: the label cures it.

3. Otherwise the check's words are wrong.
   Each tester and reviewer step sends it to the architect as a finding with `envoy_publish`; with neither step, the implementer sends it during retro.
   No role fails the round or requests changes for it.

## What the evidence is

The named job is the check's proof at the head, and no role repeats it, even when it is a unit or integration suite.
It proves only that the job passed there: the tester, or the implementer with no tester step, still proves the real surface with the checks CI cannot run.
