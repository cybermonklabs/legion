# An issue's plan

Part of `skill://legion-architect`, sections 1 and 2.
Read it before you write or give an issue's plan.
Stopping a task, a later version of a plan Legion runs, and the `task-stopped`, `retro-held` and `plan-not-taken` notices:
`skill://legion-architect/references/plan-changes.md`.

## The task block

You choose which of Legion's steps each issue of your tree runs.
An issue whose experiment's group runs the project's default steps is the exception ("What Legion refuses", below).
The plan is the issue's `plan.md` document (`dispatch artifact` on the issue).
It holds one `plan_task` block for each task:

```markdown
:::plan_task{#a steps="implementer reviewer"}
What the task must deliver. What the steps after the first need to know, since no planner step
writes it for them. For each step, a fresh agent or the agent of an earlier step, and why.
:::
```

- Give the block `serves` naming the spec's `requirement` blocks, as a planner's task does, so the issue's coverage counts it.
  A change of `serves` alone changes no task.

- To have one agent work several steps, give each of them the same key after its role, as `steps="implementer:a tester:a reviewer"`.
  Legion runs the steps of one key as one agent, in one claim and one session, whatever role each step is on; a step with no key is an agent of its own.
  A key other than `owner` may stand on any role but `acceptance`, and `owner` on `acceptance` alone.
  Each agent is named for the role of its first step, so a later version that needs a fresh agent of a role whose name another agent of the plan holds is refused (`PLAN_REFUSED`).

## Several tasks

A task's `needs` names each task it needs, separated by spaces, each as a reference to that task's block: `dispatch://<KEY>/artifact/plan-md#b-<id>`.
A need that names another issue is refused (`PLAN_REFUSED`): a task needs only tasks of its own plan, and an issue waits on another issue by its `blocked_by` list (`skill://legion-architect/references/decomposition.md`, "Recording a wait on other work").
Legion starts each task with no `needs` when the issue's work starts, and each other task once every task it needs is done, from their joined work.
A task's work merges into the repository's default branch by itself when its block says `merges="true"` or when no task needs it.
Each such task gets Legion's retro and merge after its last step, and is done when its merge is on the default branch.
A task that needs such a task starts from the default branch.
Every other task is done when its last step passes, and its work merges with the task that needs it.
The issue is done when each of its tasks is done.
This release runs one agent of each role for an issue, so give the steps of one role in several tasks the same key.
That agent works one step at a time, and a step of another task that it works waits for it.

An example with five tasks:

| Task | Needs | Steps |
| --- | --- | --- |
| a | | `implementer:i reviewer:r` |
| b | | `implementer:i reviewer:r` |
| c | b | `implementer:i reviewer:r` |
| d | a, c | `implementer:i reviewer:r` |
| e | d | `implementer:i tester` |

a and b start at once, and one implementer and one reviewer work the steps of all five tasks, one step at a time.
e is the one task that no task needs: its tester tests the joined work of every task, and the whole issue merges as one change after e's retro.
If c's block said `merges="true"`, c would merge by itself, with its own retro, before d starts.

Task d as its block:

```markdown
:::plan_task{#d needs="dispatch://<KEY>/artifact/plan-md#b-a dispatch://<KEY>/artifact/plan-md#b-c" steps="implementer:i reviewer:r"}
What task d must deliver, on the joined work of tasks a and c.
:::
```

## The base of each task's pull request

Legion sets the base of each task's pull request, and a plan names no other.
The pull request of each task whose work merges by itself, and of each task with no `needs`, is against the repository's default branch.
So a plan of one task, the project's default steps included, has its pull request against the default branch.
Any other task with one need has its pull request against that need's branch, and a task with several against its join branch.
Legion joins their work there.
Needs whose work merged count together as one need, on the default branch.
The implementer's completion is refused (`HANDOFF_PULL_REQUEST_BASE`) while its pull request is against any other base, so never ask for another base in a task's text.
Work that must build on unmerged work of the same issue is a task that `needs` the task that does it.
Work that must build on another issue's unmerged branch goes in an issue whose `blocked_by` list names that issue, and Legion starts it once that issue is `done`.

## The roles and their order

Name the steps in `steps` in the order they run.
A `planner` step may stand at any place, and `implementer` stands before each `tester`, `reviewer` and `acceptance:owner` step, since each of them reads the pull request the implementer opens: a plan that names one of them with no `implementer` step before it is refused with a reason that ends `This release of Legion does not carry it out yet.`
The table lists the steps in the order of the default steps:

| Step | Type | Phase | What leaving it out means |
| --- | --- | --- | --- |
| `planner` | planning | `planning` | No implementation plan and no plan review: the steps work from your task's text and the spec. |
| `implementer` | implementation | `implementing` | Required. It also works the retro, and a failed step returns to it unless the failed step names an earlier one. |
| `tester` | testing | `testing` | Only the implementer records results against the checks, and no other step runs them again, except where a parent's pull request merged before its last round: you, or a subagent you dispatch, then run and record them (`skill://legion-architect/references/acceptance.md`, *The last round*). |
| `reviewer` | review | `reviewing` | No review: on a base whose rules require an approving review, the merger waits and tells you. |
| `acceptance:owner` | review | `accepting` | Your step. Without it the last step's pass starts the retro of a task that merges, or holds a parent before it while issues beneath it are in the workflow, and nothing waits for you to read the evidence. |

Without `acceptance:owner`, retro holds the checks (`skill://legion-retro`), and so does a reviewer step where the task has one.
A reviewer step approves only once a pass stands for every `before_merge` check at the head.
And retro's first step sends back a check with no pass standing there, except a `ci_check` whose run has not concluded or cannot be read.
So on a task with no reviewer step and no `acceptance:owner`, such a check holds the merge only where it is among the checks the base branch requires.

## What Legion refuses

`register_plan` answers `409 PLAN_REFUSED` with `reasons: [{task, reason}]`, one reason for each cause, its `task` empty for a cause of the whole plan.
Fix each reason and give a new version.
A need that names no task of the plan, and a task that needs itself through other tasks, are refused.
A reason that ends `This release of Legion does not carry it out yet.` names a plan a later release takes: write it again within the rules above.

An issue whose experiment's group runs the project's default steps takes no plan: give it none, and it runs the default steps.
Once the daemon records such an issue, `register_plan` on it answers `409 PLAN_NOT_TAKEN`, which is no fault of the plan.
`read_record` shows such an issue: one of its `assignments` has `plan` `default`.

The reasons a version gets for where its task stands are in `skill://legion-architect/references/plan-changes.md`.

## When to give it

- **A root:** before `register_gate`, because the gate's opening starts the root's first step.

- **A child:** before `release_children` releases it.
  The daemon has not recorded the child yet, so Legion keeps the plan until the release;
  the answer, as for any plan it takes, is the plan's `version` and each task with its `steps` and its `needs`.
  Its `added` names each task the version added over the plan in force, every task of a first version, and is `[]` for a version that adds none.
  A project's experiment draws the child when the daemon records it.
  If the child's group runs the project's default steps, its first step passes your plan over (`plan-not-taken`).
  A child the daemon has not recorded is refused once its Dispatch status is none of `triage`, `backlog`, `icebox` and `todo` (`ISSUE_ALREADY_RELEASED`).
  The child's first step can start in the same admission pass as its release, so a plan given after it comes too late.

A `plan.md` that a planner of the default steps wrote holds that planner's task blocks.
The version you give replaces them with your own blocks: delete theirs, and put what the next steps still need from the planner's plan into your tasks' text.

## Reading how plans went

`dispatch steps --issue <KEY>` lists each run of each step of an issue and how it ended.
Each finished issue of a project, with its plan's source and steps, its retro's ratings, its time and its count of failed steps, is `dispatch plans --project <KEY>`.
Read it before you choose the steps of an issue like one that finished.
