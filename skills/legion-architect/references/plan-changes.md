# Stopped tasks, later plans and their notices

## Stopping and changing a task

- After `VERSION_NOT_IN_DISPATCH`, upload that version to Dispatch first.

## The notices

- **`task-stopped`:** Legion stopped the task and holds the issue.
  Either a step failed and no earlier step of the type implementation can take the work back, and the reason names the task and the step: read the failed step's report.
  Or another architect of your tree stopped it with `stop_task`, and the reason names that architect and the notice carries `stoppedBy`: no step failed.
  A stop you make yourself sends you no notice: the 200 answer of `stop_task` is the record.
  Then give a later version that changes the task, or end the work: for a child under your own issue, your own status write in Dispatch, by the rules for ending a child in your role prompt; `dispatch ask` on the root for a root.
  When the stopped task is your own issue's, as a child's architect, send the end of that work to the architect above you with `envoy_publish`: the daemon sets back your status write on your own issue.
  On an issue whose experiment's group runs the project's default steps, Legion takes no later version (`PLAN_NOT_TAKEN`).
  To run such a child under your own issue again, restart it (`backlog`, then `rerun_child`): it starts the default steps at their first step.

- **`retro-held`:** a parent's task passed its last step, which is not your `acceptance:owner` step, while issues beneath it are still in the workflow.
  Run each issue it names to done, or end it by your own status write in Dispatch, by the same rules.
  `retry_or_escalate` refuses that hold, a retry and an escalation alike: while issues beneath it are in the workflow, `409 HELD_FOR_CHILDREN` names each of them; with none left, the answer says that its tree lingers or that its root waits in the admission line.
  A `todo` on the root leaves it held.

- **`plan-not-taken`:** Legion did not take a plan at the issue's first step, in one of two cases.
  In the first, an earlier owner of the issue gave the plan.
  Its reason names the version, who gave it, and the plan the first step runs: yours or the default steps.
  To run the earlier plan, `stop_task` the task that runs now, then give the earlier plan again with `register_plan` as your own.
  In the second, the reason names the issue's group, whose issues run the project's default steps.
  Start nothing for it: that issue takes no plan (`skill://legion-architect/references/plan.md`, "What Legion refuses").
