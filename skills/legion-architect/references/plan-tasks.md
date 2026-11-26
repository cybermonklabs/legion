# A plan of several tasks

Part of `skill://legion-architect`, section 1.
Read it before you give an issue's plan more than one task, or give a task `needs`.
The task block, the steps and when to give a plan: `skill://legion-architect/references/plan.md`.

## Needs, starts and merges

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

## An example

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
