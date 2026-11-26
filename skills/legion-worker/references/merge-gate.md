# The merge gate: review and the production check

## The reviewer

Approve a head only when every required check of the base has concluded green at it (read the check runs and every page of the commit statuses), every `before_merge` check of the issue has a pass standing at it (`dispatch results --issue <issue>`), and, where the diff touches runtime code, `task(agent="thermonuclear-deep-review")` and `task(agent="thermonuclear-code-quality")` ran at that head and their findings are answered. A required check still running is a wait, not a verdict. Name the head you approve by its commit.

## The production check after the close

A look reaches the role the check opened with. Reach the deploy as your deployment's instructions say. A staging pass is no production pass. Report the look's verdict with `handoff_complete`: `pass` only once production carries the merge and you saw each requirement that only the deploy can show; `pending` with `wake_at` on a look that is not the last; on the last look, evidence still missing is a `fail`. A `fail` follows `skill://legion-worker/references/findings.md`.
