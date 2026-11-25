# Stopped tasks, later plans and their notices

Part of `skill://legion-architect`, section 1.
Read it before you stop a task or give a later version of a plan Legion runs, and on a `task-stopped`, `retro-held` or `plan-not-taken` notice.
A plan's text, its steps and when to give it first: `skill://legion-architect/references/plan.md`.

## Stopping and changing a task

Your role prompt says what `stop_task` does, what holds a stopped task and what starts it again, and what each refusal of `stop_task` or of a plan's version means.

- After `VERSION_NOT_IN_DISPATCH`, upload that version to Dispatch first.

## The notices

- **`task-stopped`:** Legion stopped the task (`task`) and holds the issue from the phase that `phase` names.
  `cause: no_return` is a step that failed where no earlier step of the type implementation can take the work back: `step` names its place in the task, and `role` its role, so read the failed step's report.
  `cause: owner` is another architect of your tree that stopped it with `stop_task`: the notice carries `stoppedBy`, the claim of that architect, and no step failed.
  A stop you make yourself sends you no notice: the 200 answer of `stop_task` is the record.
  Then give a later version that changes the task, or end the work: for a child under your own issue, your own status write in Dispatch, by the rules for ending a child in your role prompt; `dispatch ask` on the root for a root.
  When the stopped task is your own issue's, as a child's architect, send the end of that work to the architect above you with `envoy_publish`: the daemon sets back your status write on your own issue.
  On an issue whose experiment's group runs the project's default steps, Legion takes no later version (`PLAN_NOT_TAKEN`).
  To run such a child under your own issue again, restart it (`backlog`, then `rerun_child`): it starts the default steps at their first step.

- **`retro-held`:** a parent's task passed its last step, which is not your `acceptance:owner` step, while issues beneath it are still in the workflow.
  `waiting_for` names each of them.
  Legion holds the parent before its retro (`phase: retro`), so that it merges after them, and starts the retro when the last one leaves.
  Run each issue it names to done, or end it by your own status write in Dispatch, by the same rules.
  The notice gives those rules word for word in `end_child_rule`, and the rule for a status write the daemon has not taken yet in `status_not_taken_yet`.
  `retry_or_escalate` refuses that hold, a retry and an escalation alike: while issues beneath it are in the workflow, `409 HELD_FOR_CHILDREN` names each of them; with none left, the answer says that its tree lingers or that its root waits in the admission line.
  A `todo` on the root leaves it held.

- **`plan-not-taken`:** Legion did not take a plan at the issue's first step, in one of two cases, which `why` names.
  Each row of `not_taken` names one version that it did not take, once: its `version`, its document's `artifact` and the architect claim that gave it (`given_by`).
  `runs` names the plan that the first step runs: `owner`, yours, with `runs_version`, `runs_artifact` and `runs_given_by`, or `default`, the project's default steps.
  In the first case, `why: earlier_owner`, an earlier owner of the issue gave the plan.
  To run the earlier plan, `stop_task` the task that runs now, then give the earlier plan again with `register_plan` as your own.
  In the second, `why: group`, the issue's group runs the project's default steps: `experiment` and `variant` name that group, and `runs` is `default`.
  Start nothing for it: that issue takes no plan (`skill://legion-architect/references/plan.md`, "What Legion refuses").
