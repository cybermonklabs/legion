# An issue's plan

Part of `skill://legion-architect`, sections 1 and 2.
Read it before you write or give an issue's plan.
Stopping a task, a later version of a plan Legion runs, and the `task-stopped`, `retro-held` and `plan-not-taken` notices:
`skill://legion-architect/references/plan-changes.md`.

## The task block

You choose which steps each issue of your tree runs, in any order and with no role required.
An issue whose experiment's group runs the project's default steps is the exception ("What Legion refuses", below).
The plan is the issue's `plan.md` document (`dispatch artifact` on the issue).
In this release it holds one `plan_task` block, the issue's one task:

```markdown
:::plan_task{#a steps="implementer reviewer"}
What the task must deliver. What the steps after the first need to know, since no planner step
writes it for them. For each step, a fresh agent or the agent of an earlier step, and why.
:::
```

- Give the block `serves` naming the spec's `requirement` blocks, so the issue's coverage counts it.
  A change of `serves` alone changes no task.

- Steps that share a key, `<role>:<key>`, are one agent in one session, and a step with no key has an agent of its own.
  Your role prompt gives the rules of keys and of a role at two steps.
  The key `owner` makes a step yours to do on your own model, so its role must name no `model` (`skill://legion-architect/references/own-steps.md`).
  The agent of the step whose role your `Production check` line names keeps its session past the close, for that check, and where two steps of the task have that role, the agent of the one that ran last; where no step of the task has that role, the check starts a fresh agent of it.
  Write in the task's text whether each step needs a fresh agent or an earlier one, and why.
  Retro rates the plan's steps, and the agent that worked each, against that choice.
  A later version's agent names: `skill://legion-architect/references/plan-changes.md`.

## The roles

Name the steps in `steps` in the order they run.
A step names one of Legion's roles in the table below, a role this deployment defines, or a role your plan defines (`skill://legion-architect/references/plan-roles.md`): your `Legion addressing` line lists the first two, each with the type of its step.
Any step may stand at any place, and a role at two steps, as step 3 of `implementer:a reviewer implementer:a` does.
A role this deployment defines runs where its step stands, at the phase `step`, and reports pass or fail.
The table lists Legion's steps in the default steps' order:

| Step | Type | Phase | What leaving it out means |
| --- | --- | --- | --- |
| `planner` | planning | `planning` | No plan or plan review: the steps work from your task's text and the spec. |
| `implementer` | implementation | `implementing` | None of this type: no new pull request, you do the retro, and a failed step naming none stops the task. Otherwise the last such step's agent does the retro, and a failed step that names no earlier one returns to the first step of the type implementation before it, the implementer or a role of this deployment of that type. |
| `tester` | testing | `testing` | Only the steps that record results against the checks record them, and no other step runs them again, except where a parent's pull request merged before its last round: you, or a subagent you dispatch, then run and record them (`skill://legion-architect/references/acceptance.md`, *The last round*). |
| `reviewer` | review | `reviewing` | No reviewer's approval: where the base requires one, the merger waits and tells you. With no pull request, a reviewer ends its step by its verdict. |
| `acceptance:owner` | review | `accepting` | Your step. Without it the last step's pass starts the retro, or holds a parent before it while issues beneath it are in the workflow. |
| `acceptance` | review | `accepting` | No fresh reading of the whole work. With it, a fresh agent reads the evidence and ends the step by its verdict. |

Without an acceptance step, retro holds the checks (`skill://legion-retro`), and so does a reviewer step where the task has one.
A reviewer step approves, or passes by its verdict, only once a pass stands for every `before_merge` check at the head.
Retro's first step sends back a check with no pass at the head, with two exceptions.
Do not send back a `ci_check` whose run has not concluded or cannot be read, whose derived result is a fail, or whose name no run answers (`absent`).
Send a whole-suite `run` to the architect as a finding, not a move back, because the implementer runs no whole suite.
Send an `absent` name as a finding too, unless GitHub is holding the job back until its label is on.
A `ci_check` that reads `no_head`, on an issue with no pull request, can never pass: it holds the retro until you rewrite the check.
On a task with no reviewer, `acceptance:owner` or `acceptance` step, such a check holds the merge only where it is among the checks the base branch requires.
A step of a role of this deployment runs and holds the checks only as its prompt file asks.

## What Legion refuses

`register_plan` answers `409 PLAN_REFUSED` with `reasons: [{task, reason}]`, one reason for each cause, its `task` empty for a cause of the whole plan.
The refusals of a role block your plan defines: `skill://legion-architect/references/plan-roles.md`.
A step that names a role neither the deployment nor the plan defines is refused (`step <n> of task <t> names the role <role>, which neither the deployment nor the plan defines.`), and no release takes it.
The steps of one key, `<role>:<key>`, are one agent.
So a key whose steps name roles of two models is refused (`… one agent runs on one model.`), and so is one whose steps name roles of two harnesses (`… one agent runs on one harness.`).
Fix each reason and give a new version.
A reason ending `This release of Legion does not carry it out yet.` names what a later release takes: more than one task or a task block's attribute `needs`, `owner`, `part_of` or `merges`.
Write the plan again as one task without them; a step's key `owner` is no such attribute.

An issue whose experiment's group runs the project's default steps takes no plan: give it none, and it runs the default steps.
Once the daemon records such an issue, `register_plan` on it answers `409 PLAN_NOT_TAKEN`, which is no fault of the plan.
`read_record` shows such an issue: one of its `assignments` has `plan` `default`.

Your role prompt gives the reasons a version gets for where its task stands.

## When to give it

- **A root:** before `register_gate`, because the root's first step can start as soon as the gate opens.

- **A child:** before `release_children` releases it, since its first step can start in the same admission pass.
  The daemon has not recorded the child yet, so Legion keeps the plan until the release;
  the answer is the plan's `version` and each task with its `steps`.
  A project's experiment draws the child when the daemon records it.
  If the child's group runs the project's default steps, its first step passes your plan over (`plan-not-taken`).
  A child the daemon has not recorded is refused once its Dispatch status is none of `triage`, `backlog`, `icebox` and `todo` (`ISSUE_ALREADY_RELEASED`).

## A root with no code of its own

When every change lands through children, the agent of the root's implementation step, where it has one, opens its pull request on an empty commit.
Plan no code for the root, and create no child only to give it a commit.

## Reading how plans went

`dispatch steps --issue <KEY>` lists each run of each step of an issue and how it ended.
`dispatch plans --project <KEY>` lists each finished issue of a project with its plan and how that plan went.
Read it before you plan an issue like one that finished.
