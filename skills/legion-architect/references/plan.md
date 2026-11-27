# An issue's plan

You choose which of Legion's steps each issue of your tree runs.
The plan is the issue's `plan.md` document, with one `plan_task` block, the issue's one task:

```markdown
:::plan_task{#a steps="implementer reviewer" serves="dispatch://<KEY>/spec#b-r1"}
What the task must deliver. What each step after the first needs to know, since no planner step
writes it for them. For each step, a fresh agent or the agent of an earlier step, and why.
:::
```

- Give the block an id (`#a` above) of lowercase letters, digits and hyphens that starts with a letter or a digit and is at most 64 bytes.
  Legion refuses any other id, since Dispatch records no step of such a task.
- `serves` names the spec's `requirement` blocks that the task serves, so that the issue's coverage counts it.
- A task that names no steps runs the project's default steps: `planner implementer tester reviewer acceptance:owner`, where the project names no other.

## What leaving a step out means

- No `planner`: no implementation plan and no plan review. The steps work from your task's text and the spec.
- No `tester`: only the implementer records results against the checks.
- No `reviewer`: no review. On a base whose rules require an approving review, the merger waits and tells you.
- No `acceptance:owner`: nothing waits for you to read the evidence. The last step's pass starts the retro, or holds a parent before its retro while issues beneath it are in the workflow.
  Give a parent this step when you want to read its finished work.
- No `reviewer` and no `acceptance:owner`: a `before_merge` check with no pass holds the merge only where it is among the checks that the base branch requires.

## A stopped task

On `task-stopped`, read the reason.
Where a step failed, read that step's report first.
Then give a later version of the plan that changes the task, or end the work:

- a child under your own issue, with your own status write;
- your own issue as a child's architect, through the architect above you;
- a root, with a `dispatch ask` on the root.
