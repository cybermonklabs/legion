# An issue's plan

Part of `skill://legion-architect`, sections 1 and 2.
Read it before you write or give an issue's plan.
Stopping a task, a later version of a plan Legion runs, and the `task-stopped`, `retro-held` and `plan-not-taken` notices:
`skill://legion-architect/references/plan-changes.md`.

## The task block

You choose which steps each issue of your tree runs.
An issue whose experiment's group runs the project's default steps is the exception ("What Legion refuses", below).
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

- To have one agent work several steps, give each of them the same key after its role, as `steps="implementer:a tester:a reviewer"`.
  Legion runs the steps of one key as one agent, in one claim and one session, whatever role each step is on; a step with no key is an agent of its own.
  The agent of the step whose role your `Production check` line names keeps its session past the close, for that check; where no step of the task has that role, the check starts a fresh agent of it.
  A key other than `owner` may stand on any role but `acceptance`, and `owner` on `acceptance` alone.
  Each agent is named for the role of its first step, so a later version that needs a fresh agent of a role whose name another agent of the plan holds is refused (`PLAN_REFUSED`).

## The roles and their order

Name the steps in `steps` in the order they run.
A step names one of Legion's roles in the table below, or a role this deployment defines: your `Legion addressing` line lists both ("The roles of this deployment"), each with the type of its step.
A `planner` step may stand at any place. The task's first `implementer` step stands before each `tester`, `reviewer` and `acceptance:owner` step, which read the pull request it opens, and a later one may follow them, as in `implementer:a reviewer implementer:a`: a plan with no `implementer` step before one of them is refused with a reason that ends `This release of Legion does not carry it out yet.`
A role this deployment defines runs where its step stands in your plan, at the phase `step`, and reports pass or fail.
It reads no pull request, so it may stand at any place too.
A role of the type implementation is the one exception: it stands before the task's first `implementer` step, since Legion's retro is done by the implementer in this release.
The table lists Legion's steps in the order of the default steps:

| Step | Type | Phase | What leaving it out means |
| --- | --- | --- | --- |
| `planner` | planning | `planning` | No implementation plan and no plan review: the steps work from your task's text and the spec. |
| `implementer` | implementation | `implementing` | Required: it also works the retro, and a failed step that names no earlier one returns to the first step of the type implementation before it, the implementer or a role of this deployment of that type where one stands before the implementer. |
| `tester` | testing | `testing` | Only the implementer records results against the checks, and no other step runs them again, except where a parent's pull request merged before its last round: you, or a subagent you dispatch, then run and record them (`skill://legion-architect/references/acceptance.md`, *The last round*). |
| `reviewer` | review | `reviewing` | No reviewer step, so no approval of the reviewer's: on a base whose rules require an approving review, the merger waits and tells you. |
| `acceptance:owner` | review | `accepting` | Your step. Without it the last step's pass starts the retro, or holds a parent before it while issues beneath it are in the workflow, and nothing waits for you to read the evidence. |

Without `acceptance:owner`, retro holds the checks (`skill://legion-retro`), and so does a reviewer step where the task has one.
A reviewer step approves only once a pass stands for every `before_merge` check at the head.
Retro's first step sends back a check with no pass at the head, with two exceptions.
Do not send back a `ci_check` whose run has not concluded or cannot be read, whose derived result is a fail, or whose name no run answers (`absent`).
Send a whole-suite `run` to the architect as a finding, not a move back, because the implementer runs no whole suite.
Send an `absent` name as a finding too, unless GitHub is holding the job back until its label is on.
On a task with no reviewer step and no `acceptance:owner`, such a check holds the merge only where it is among the checks the base branch requires.
A step of a role of this deployment runs and holds the checks only as its prompt file asks.

## What Legion refuses

`register_plan` answers `409 PLAN_REFUSED` with `reasons: [{task, reason}]`, one reason for each cause, its `task` empty for a cause of the whole plan.
Fix each reason and give a new version.
A reason that ends `This release of Legion does not carry it out yet.` names a plan a later release takes: write it again within the rules above.
Among them, a role of this deployment of the type implementation after the implementer's step: `task <id> names <role> at step <i>, a step of the type implementation after the step implementer at step <j>, and Legion's retro is done by the implementer in this release.`
A step that names a role neither Legion nor this deployment defines is refused `step <i> of task <id> names the role <role>, which neither the deployment nor the plan defines.`, and no release takes it.

An issue whose experiment's group runs the project's default steps takes no plan: give it none, and it runs the default steps.
Once the daemon records such an issue, `register_plan` on it answers `409 PLAN_NOT_TAKEN`, which is no fault of the plan.
`read_record` shows such an issue: one of its `assignments` has `plan` `default`.

The reasons a version gets for where its task stands are in `skill://legion-architect/references/plan-changes.md`.

## When to give it

- **A root:** before `register_gate`, because the root's first step can start as soon as the gate opens.

- **A child:** before `release_children` releases it.
  The daemon has not recorded the child yet, so Legion keeps the plan until the release;
  the answer, as for any plan it takes, is the plan's `version` and each task with its `steps`.
  A project's experiment draws the child when the daemon records it.
  If the child's group runs the project's default steps, its first step passes your plan over (`plan-not-taken`).
  A child the daemon has not recorded is refused once its Dispatch status is none of `triage`, `backlog`, `icebox` and `todo` (`ISSUE_ALREADY_RELEASED`).
  The child's first step can start in the same admission pass as its release, so a plan given after it comes too late.

A `plan.md` that a planner of the default steps wrote holds that planner's task blocks.
The version you give replaces them with your own one block: delete theirs, and put what the next steps still need from the planner's plan into your task's text.

## A root with no code of its own

When every change lands through children, the root's implementer opens its pull request on an empty commit.
Plan no code for the root, and create no child only to give it a commit.

## Reading how plans went

`dispatch steps --issue <KEY>` lists each run of each step of an issue and how it ended.
`dispatch plans --project <KEY>` lists each finished issue of a project with its plan and how that plan went.
Read it before you choose the steps of an issue like one that finished.
