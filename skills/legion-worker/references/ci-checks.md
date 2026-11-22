# A check CI proves

Read it when a check of the issue's Verification document carries `ci_check`, before you run, record, judge or write the line of any check.

## What it is

`ci_check` names the GitHub check run that proves the check at the head. Such a check has no `run`, its `when` is `before_merge`, and it names its `negative_control`. The architect writes it as `skill://legion-architect/references/design-gate.md` says.

## No role runs it

A check carrying `ci_check` is CI's. No role runs its work in the pod or records a result for it: Dispatch refuses one with `409 CHECK_PROVED_BY_CI`. `dispatch results --issue <issue>` reads the named run at the head of the issue's pull request and prints the one result it derives, its `A pass stands:` line, and the line for the PR body:

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

- The implementer copies each such check's `Pull request line` into the `Checks` block. The next role that writes the body reads `dispatch results` again and replaces a no-result line.
- The tester runs and records only the checks with no `ci_check`. It neither reads nor records a `ci_check` check's derived result, which is CI's: the reviewer reads it before it approves.
- The reviewer waits for a run still going by its name, and neither approves nor requests changes for that check until it concludes.
- A job the repository runs on a pull request only under the label `full-ci` (on cybermonklabs/legion, the slow suites and the full browser lane) reads `skipped` without it. The implementer adds the label, then pushes, because the workflow starts on a push and not on a label. The label stays on while a `ci_check` of the issue names such a job.

## A name that no run answers

When the head's checks have settled and no run of the name answers:

1. Read the merge state first (`gh pr view <n> --json mergeStateStatus`): a `DIRTY` head gets no `pull_request` run, and the forward merge cures it.
2. A held-back job reads `skipped`, not absent: the label cures it.
3. Otherwise the check's words are wrong. The reviewer sends it to the architect as a finding with `envoy_publish`, and requests no changes for it; the tester reads no CI run, so the case never reaches it.

## What the evidence is

The named job is the check's proof at the head, and no role repeats it, even when it is a unit or integration suite. It proves only that the job passed there: the tester still proves the real surface with the checks CI cannot run.
