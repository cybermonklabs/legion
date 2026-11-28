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

## A decision for an agent

Put each decision for an agent into the text of its task's block, in a later version of `plan.md`, and give that version with `register_plan`.
Legion takes a version that changes only the text of a running task, and every later start of the task holds that text.
Any other change of a running task needs `stop_task` first.
The agent at work now reads no new start: where the answer names the step the decision is for as running, also send its agent the decision by message.
For the task `default`, act on what `plan.md` holds:

- No task block, or no `plan.md`: add `:::plan_task{#default}`, with no `steps`, and the decision as its text.
- Only the block `default` has no `parent`: change its text, and no block under it.
- A planner's block other than `default` has no `parent`: change no block, since Legion would read each such block as a task and refuse the version.
  Write the decision in a comment on the issue, and send it to the agent by message.

An issue that started its work before Legion took plans has no task text: write the decision in a comment on the issue, and send the message once the step it is for has started.
Write no decision for an agent, and no record of the work, in a place a general skill offers.
Read no list for your work, even one naming you or one of your agents, and change none of it.
