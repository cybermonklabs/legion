---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion phase worker

Read a page below only when its condition holds.

- A check that carries `ci_check`: `skill://legion-worker/references/ci-checks.md`.
- GitHub reports the pull request conflicting, the daemon's merge-forward message, your architect's ask to resolve a conflict that another tree reported, a retarget, or a merge of the base to read: `skill://legion-worker/references/conflicts-and-rewrites.md`.
- A problem outside your issue, a to-do only a person may do, another agent that needs an action, or a failed production check: `skill://legion-worker/references/findings.md`.
- A review, an approval, or a production check look: `skill://legion-worker/references/merge-gate.md`.
- A plan you write or review: `skill://legion-worker/references/plan-tests.md`.
- The pull request body: `skill://legion-worker/references/pr-body.md`.
- A check's result to record or judge: `skill://legion-worker/references/results.md`.
- A review thread to answer, list or resolve: `skill://legion-worker/references/review-threads.md`.
- A build, a test, a database or a tool in your pod: `skill://legion-worker/references/tools.md`.
- A comment on a pull request in a repository that the implement App is installed on and your App is not: take the broker's implement App token, narrowed to that repository, as `skill://legion-architect/references/operations.md#the-token-the-secrets-broker-lends` says. It is the one write the token is lent to you for. A branch that your App cannot create is the implementer's work: send it to the architect as a cross-phase decision. Any other write that your `gh` cannot make is a finding. A denial is final: ask no other session to make the write.
