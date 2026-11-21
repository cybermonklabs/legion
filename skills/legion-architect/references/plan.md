# An issue's plan

Part of `skill://legion-architect`, sections 1 and 2.
Read it before you write or give an issue's plan.
Stopping a task, a later version of a plan Legion runs, and the `task-stopped`, `retro-held` and `plan-not-taken` notices:
`skill://legion-architect/references/plan-changes.md`.

## The task block

You choose which of Legion's steps each issue of your tree runs.
The plan is the issue's `plan.md` document (`dispatch artifact` on the issue).
In this release it holds one `plan_task` block, the issue's one task:

```markdown
:::plan_task{#a steps="implementer reviewer"}
What the task must deliver. What the steps after the first need to know, since no planner step
writes it for them. For each step, a fresh agent or the agent of an earlier step, and why.
:::
```

- Give the block `serves` naming the spec's `requirement` blocks, as a planner's task does, so the issue's coverage counts it.
  A change of `serves` alone changes no task.

- This release gives each role one agent for each issue, the session that role keeps until the issue leaves the workflow.
  It cannot carry out your choice of a fresh or an earlier agent yet; write the choice all the same, because retro rates the plan's steps and the agent that worked each against it.

## The roles and their order

| Step | Type | Phase | What leaving it out means |
| --- | --- | --- | --- |
| `planner` | planning | `planning` | No implementation plan and no plan review: the steps work from your task's text and the spec. |
| `implementer` | implementation | `implementing` | Required. It also works the retro, and a failed step returns to it unless the failed step names an earlier one. |
| `tester` | testing | `testing` | Only the implementer records results against the checks, and nobody runs them again. |
| `reviewer` | review | `reviewing` | No review: on a base whose rules require an approving review, the merger waits and tells you. |
| `acceptance:owner` | review | `accepting` | Your step. Without it the last step's pass starts the retro, and nothing waits for you to read the evidence. |

Without `acceptance:owner`, two steps still hold the checks:
a reviewer step approves only once a pass stands for every `before_merge` check at the head, and retro's first step sends back a check with no pass standing there, except a `ci_check` whose run has not concluded or cannot be read, which moves nothing back (`skill://legion-retro`).
So on a task with no reviewer step and no `acceptance:owner`, such a check holds the merge only where it is among the checks the base branch requires.

## What Legion refuses

`register_plan` answers `409 PLAN_REFUSED` with `reasons: [{task, reason}]`, one reason for each cause, its `task` empty for a cause of the whole plan.
Fix each reason and give a new version.
A reason that ends `This release of Legion does not carry it out yet.` names a plan a later release takes: write it again within the table above.

The reasons a version gets for where its task stands are in `skill://legion-architect/references/plan-changes.md`.

## When to give it

- **A root:** before `register_gate`, because the gate's opening starts the root's first step.

- **A child:** before `release_children` releases it.
  The daemon has not recorded the child yet, so Legion keeps the plan until the release;
  the answer, as for any plan it takes, is the plan's `version` and each task with its `steps`.
  A child the daemon has not recorded is refused once its Dispatch status is none of `triage`, `backlog`, `icebox` and `todo` (`ISSUE_ALREADY_RELEASED`).
  The child's first step can start in the same admission pass as its release, so a plan given after it comes too late.

A `plan.md` that a planner of the default steps wrote holds that planner's task blocks.
The version you give replaces them with your own one block: delete theirs, and put what the next steps still need from the planner's plan into your task's text.

## Reading how plans went

`dispatch steps --issue <KEY>` lists each run of each step of an issue and how it ended.
Each finished issue of a project, with its plan's source and steps, its retro's ratings, its time and its count of failed steps, is `dispatch plans --project <KEY>`.
Read it before you choose the steps of an issue like one that finished.
