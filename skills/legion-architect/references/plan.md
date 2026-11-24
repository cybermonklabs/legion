# An issue's plan

Part of `skill://legion-architect`, sections 1 and 2.
Read it before you write or give an issue's plan.
Stopping a task, a later version of a plan Legion runs, and the `task-stopped`, `retro-held` and `plan-not-taken` notices:
`skill://legion-architect/references/plan-changes.md`.

## The task block

You choose the steps each issue of your tree runs, in any order and with no role required.
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
  Write in the task's text whether each step needs a fresh agent or an earlier one, and why.
  Retro rates the plan's steps, and the agent that worked each, against that choice.

## The roles

| Step | Type | Phase | What leaving it out means |
| --- | --- | --- | --- |
| `planner` | planning | `planning` | No plan or plan review: the steps work from your task's text and the spec. |
| `implementer` | implementation | `implementing` | None of this type: no new pull request, you do the retro, and a failed step naming none stops the task. Otherwise the last such step's agent does the retro, and a failed step returns to the first before it unless it names an earlier one. |
| `tester` | testing | `testing` | Only the steps that record results against the checks record them, and nobody runs them again. |
| `reviewer` | review | `reviewing` | No review: where the base requires one, the merger waits and tells you. With no pull request, it ends its step by its verdict. |
| `acceptance:owner` | review | `accepting` | Your step. Without it the last step's pass starts the retro. |
| `acceptance` | review | `accepting` | No fresh reading of the whole work. With it, a fresh agent reads the evidence and ends the step by its verdict. |

Without `acceptance:owner`, a reviewer step approves, or passes by its verdict, only once a pass stands for every `before_merge` check at the head.
Without it, retro's first step sends back a check with no pass there, except a `ci_check` whose run has not concluded or cannot be read.
Such a `ci_check` moves nothing back.
A `ci_check` that reads `absent` moves nothing back either: the retro sends it to you as a finding.
One that reads `no_head`, on an issue with no pull request, can never pass: it holds the retro until you rewrite the check.
So with no reviewer, `acceptance:owner` or `acceptance` step, a `ci_check` whose run has not concluded holds the merge only where the base requires it.

## A role of your plan

A plan can define a role of its own in a `plan_role` block:

```markdown
:::plan_role{#prober type="testing" model="<a listed model>"}
The role's prompt, which can hold headings, lists and code.
:::

:::plan_task{#a steps="implementer prober reviewer"}
What the task must deliver.
:::
```

- The block's id is the role's name, and its text is the prompt its agent starts with, on Oh My Pi.

- `type` is the type of the role's step, as in the table above, and may be left out.
  A role of the type implementation acts as the implement App, any other as the review App.

- `model` is the model the role's agent starts on, one your addressing lists.
  A role with no `model` starts on the default model, and where the deployment lists none, a role of your plan names none.

- A role's prompt has no bound of its own.
  An issue pod's launch carries the whole system prompt in one message of at most 1 MiB.
  Your role prompt says what Legion does with a larger one, and how you change the role then.

### A role under a running task

- A later version that changes the type, the model or the prompt of a role block that a step of the running task names is refused (`task <id> is running: stop it with stop_task before you change it.`).
  A change of a role block that no step names changes no task.

- A later version that takes away a role block that a step still names is refused whether the task runs or not (`… which neither the deployment nor the plan defines.`).
  Keep the block, or change the task's steps too, and the rule above for a running task applies.

- Once an operator adds a role of the deployment with your role's name, your issue keeps its plan's role.
  But Legion refuses every later version that keeps a role of that name.
  A new name changes each task naming the role: stop that task with `stop_task` first, then give a version whose role has a new name.

## What Legion refuses

`register_plan` answers `409 PLAN_REFUSED` with `reasons: [{task, reason}]`, one reason for each cause, its `task` empty for a cause of the whole plan.
A role block is refused for a name that is not of the form `[a-z][a-z0-9-]*` with at most 32 characters.
It is refused for a name Legion or the deployment defines already (`… which the deployment or Legion defines already; a role of a plan takes a name of its own.`), and for a `type` that is set and is not of that form (`… Dispatch's step record takes no other.`).
A role block whose `model` the deployment does not list is refused at the first step of each task that names it (`step <n> of task <t> names the role <role>, whose model <m> the deployment does not list; it lists <m1>, <m2>.`, or `it lists none.`).
Such a block that no step names is refused as a cause of the whole plan (`the plan defines the role <role> with the model <m>, which the deployment does not list; it lists <m1>, <m2>.`).
A step that names a role neither the deployment nor the plan defines is refused (`step <n> of task <t> names the role <role>, which neither the deployment nor the plan defines.`).
A key whose steps name roles of two models is refused (`… one agent runs on one model.`).
So is one whose steps name roles of two harnesses (`… one agent runs on one harness.`).
A reason ending `This release of Legion does not carry it out yet.` names what a later release takes: more than one task or a task block's attribute `needs`, `owner`, `part_of` or `merges`.
Write the plan again as one task without them.
A step's key `owner` is no such attribute.

Your role prompt gives the reasons a version gets for where its task stands.

## When to give it

- **A root:** before `register_gate`, because the gate's opening starts the root's first step.

- **A child:** before `release_children` releases it, since its first step can start in the same admission pass.
  Legion keeps the plan until the release, and answers with its `version` and each task's `steps`.
  A child the daemon has not recorded is refused once its Dispatch status is none of `triage`, `backlog`, `icebox` and `todo` (`ISSUE_ALREADY_RELEASED`).

A `plan.md` that a planner of the default steps wrote holds that planner's task blocks.
The version you give replaces them with your own one block.
Delete theirs, and put what the next steps need from the planner's plan into your task's text.

## Reading how plans went

`dispatch steps --issue <KEY>` lists each run of each step of an issue and how it ended.
`dispatch plans --project <KEY>` lists each finished issue of a project with its plan's source and steps, its retro's ratings, its time and its count of failed steps.
Read it before you plan an issue like one that finished.
