# Stopped tasks, later plans and their notices

Part of `skill://legion-architect`, section 1.
Read it before you stop a task or give a later version of a plan Legion runs, and on a `task-stopped`, `retro-held` or `plan-not-taken` notice.
A plan's text, its steps and when to give it first: `skill://legion-architect/references/plan.md`.

## Stopping and changing a task

Your role prompt says what `stop_task` does, what holds a stopped task and what starts it again, and what each refusal of `stop_task` or of a plan's version means.

- After `VERSION_NOT_IN_DISPATCH`, upload that version to Dispatch first.

- A key keeps its name in every version: a later version that names the key again runs its steps in the same agent and session, and while a version leaves the key out, no other agent takes its name, so the agent that would have taken it takes a numbered name and a slot of its own, whose start can wait for the pod's update round.
  A name that only agents with no key held is free again once no agent of the version holds it: a later agent of their role can take it, in a session of its own, and an agent of another role never does.

- A `plan.md` that a planner of the default steps wrote holds that planner's task blocks.
  The version you give replaces them with your own one block: delete theirs, and put what the next steps still need from the planner's plan into your task's text.

### A role under a running task

- A later version that changes the type, the model or the prompt of a role block that a step of the running task names is refused (`task <id> is running: stop it with stop_task before you change it.`).
  Stop the task with `stop_task` first, then give the version.
  A change of a role block that no step names changes no task.

- A later version that takes away a role block that a step still names is refused whether the task runs or not (`… which neither the deployment nor the plan defines.`), and stopping the task does not help.
  Keep the block, or change the task's steps too, and then the rule above for a running task applies.

- Once an operator adds a role of the deployment with your role's name, your issue keeps its plan's role, but Legion refuses every later version that keeps a role of that name (`… which the deployment or Legion defines already; …`).
  A version that gives the role a new name changes each task whose steps name it, so Legion refuses it while such a task runs:
  stop that task with `stop_task` first, then give a version whose role has a new name.

## The notices

- **`task-stopped`:** Legion stopped the task and holds the issue.
  Either a step failed and no earlier step of the type implementation can take the work back, and the reason names the task and the step: read the failed step's report.
  Or another architect of your tree stopped it with `stop_task`, and the reason names that architect and the notice carries `stoppedBy`: no step failed.
  Or the deployment no longer defines the role of a step the task had still to run, and the reason reads "step <i> names the role <role>, which the deployment no longer defines": give a later version that changes the task, or the operator defines the role again and a person's todo restarts the task.
  Or the deployment no longer defines the role an agent of the task's remaining work is named for, the role whose slot it runs in, and the reason reads "step <i> is the work of the agent <agent>, which runs in the slot of the role <agent>, which the deployment no longer defines" (for the retro, "the retro is the work of the agent <agent>, …"): the same two ways, but a later version ends it only where it gives that work an agent of another name, a step with another key or with none, since a key keeps the name an earlier version gave its agent.
  A stop you make yourself sends you no notice: the 200 answer of `stop_task` is the record.
  Then give a later version that changes the task, or end the work: for a child under your own issue, your own status write in Dispatch, by the rules for ending a child in your role prompt; `dispatch ask` on the root for a root.
  When the stopped task is your own issue's, as a child's architect, send the end of that work to the architect above you with `envoy_publish`: the daemon sets back your status write on your own issue.
  On an issue whose experiment's group runs the project's default steps, Legion takes no later version (`PLAN_NOT_TAKEN`).
  To run such a child under your own issue again, restart it (`backlog`, then `rerun_child`): it starts the default steps at their first step.

- **`retro-held`:** a parent's task passed its last step, which is not your `acceptance:owner` step, while issues beneath it are still in the workflow.
  Legion holds it before its retro, so that it merges after them, and starts the retro when the last one leaves.
  Run each issue it names to done, or end it by your own status write in Dispatch, by the same rules.
  `retry_or_escalate` refuses that hold, a retry and an escalation alike: while issues beneath it are in the workflow, `409 HELD_FOR_CHILDREN` names each of them; with none left, the answer says that its tree lingers or that its root waits in the admission line.
  A `todo` on the root leaves it held.

- **`plan-not-taken`:** Legion did not take a plan at the issue's first step, in one of two cases.
  In the first, an earlier owner of the issue gave the plan.
  Its reason names the version, who gave it, and the plan the first step runs: yours or the default steps.
  To run the earlier plan, `stop_task` the task that runs now, then give the earlier plan again with `register_plan` as your own.
  In the second, the reason names the issue's group, whose issues run the project's default steps.
  Start nothing for it: that issue takes no plan (`skill://legion-architect/references/plan.md`, "What Legion refuses").
