# A role of your plan

Part of `skill://legion-architect/references/plan.md`.
Read it before you define a role in an issue's plan.

## The role block

A plan can define a role of its own in a `plan_role` block, which a step names by its name:

```markdown
:::plan_role{#prober type="testing" model="<a listed model>"}
The role's prompt, which can hold headings, lists and code.
:::

:::plan_task{#a steps="implementer prober reviewer"}
What the task must deliver.
:::
```

- The block's id is the role's name, and its text is the role's prompt.
  The role's agent runs on Oh My Pi and starts with that prompt.

- `type` is the type of the role's step, as in the table of `plan.md`; a role with no `type` is taken too.
  A role of the type implementation acts as the implement App, and a role of any other type, or of none, as the review App.
  No role but `implementer` of the type implementation stands after the first `implementer` step, since the implementer also works the retro.

- `model` is the model the role's agent starts on, one your addressing lists among the models of this deployment.
  A role with no `model` starts on the default model, and where the deployment lists no model, a role of your plan names none.

- A role's prompt has no bound of its own, but a launch into an issue pod carries the whole system prompt.
  It goes in one message of at most 1 MiB.
  Your role prompt says what Legion does with a larger one, and how you change the role then.

## A role under a running task

- A later version that changes the type, the model or the prompt of a role block that a step of the running task names is refused (`task <id> is running: stop it with stop_task before you change it.`).
  Stop the task with `stop_task` first, then give the version.
  A change of a role block that no step names changes no task.

- A later version that takes away a role block that a step still names is refused whether the task runs or not (`… which neither the deployment nor the plan defines.`), and stopping the task does not help.
  Keep the block, or change the task's steps too, and then the rule above for a running task applies.

- Once an operator adds a role of the deployment with your role's name, your issue keeps its plan's role, but Legion refuses every later version that keeps a role of that name (`… which the deployment or Legion defines already; …`).
  A version that gives the role a new name changes each task whose steps name it, so Legion refuses it while such a task runs:
  stop that task with `stop_task` first, then give a version whose role has a new name.

## What Legion refuses of a role block

A role block is refused for a name that is not of the form `[a-z][a-z0-9-]*` with at most 32 characters.
It is refused for a name Legion or the deployment defines already (`… which the deployment or Legion defines already; a role of a plan takes a name of its own.`), and for a `type` that is set and is not of that form (`… Dispatch's step record takes no other.`).
It is refused for a name Legion gives a second or later agent of another role, as `planner-2` (`… the name Legion gives a second or later agent of the role planner, so an agent of each role would hold one claim.`).
A role block whose `model` the deployment does not list is refused at the first step of each task that names it (`step <n> of task <t> names the role <role>, whose model <m> the deployment does not list; it lists <m1>, <m2>.`, or `it lists none.`).
Such a block that no step names is refused as a cause of the whole plan (`the plan defines the role <role> with the model <m>, which the deployment does not list; it lists <m1>, <m2>.`).
The steps of one key, `<role>:<key>`, are one agent.
So a key whose steps name roles of two models is refused (`… one agent runs on one model.`), and so is one whose steps name roles of two harnesses (`… one agent runs on one harness.`).
