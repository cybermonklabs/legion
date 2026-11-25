# Reading admission

Read this when you need to know where an issue stands in admission, or what an `admission.held` word means.
An issue's `phase` alone does not say it: read its key against the `admission` lists and the root its `tree` names.
The rules behind them: [Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label).

## The lists

- `admission.active`: every issue holding a slot, a root or a child, at work, `held` or waiting on a person.
  An issue in `accepting`, `retro` or `merging` holds none (in the default steps, the phases after the review), and a step after `accepting` whose phase keeps a slot waits for one again.
  A started issue that waits on another issue holds none either, except three roots that keep theirs:
  one in `awaiting_person`, one at `admitted` whose gate has never opened, and one carried from another tree.

- `admission.trees`: the roots of the live trees, whether or not any issue of them holds a slot.
  Use it, not `admission.active`, for which trees run.

- `admission.waiting`: the line, in order.
  First the issues whose work came back, oldest first; then the issues whose first phase can start, by their root's Dispatch rank and then their own.
  `issues.<KEY>.waitingLine` says where one stands (`part`, `kind`, `holdsSlot`, `reason`): quote its `reason` before you call an order wrong.

- `admission.blocked`: issues waiting on an unfinished `blocked_by` issue, or a parent waiting for its open children.
  A started issue here gave its slot back and takes one again by itself; nothing of its work stops.

- `phase: awaiting_person` with `waitingOn`: a root waiting on a person's answer or review keeps its slot and is in no line.
  `waitingOn.answeredAt` means the daemon has put it in `admission.waiting` as well, among the issues whose work came back, oldest first, where it stands until the pass that resumes it.

- `phase: admitted`, in no list, with a `child_of` parent: a child that has not started.
  Leave it to its tree.

- `phase: admitted`, `status: todo`, in no list, no parent: a person took its label off.
  Nothing to do.

- `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro`:
  someone else moved a waiting root, or a root waiting on a person lost its label or claim, and the daemon sent `unslotted`.
  Re-admit nothing.

Holding no slot is no fault by itself.

## `admission.held`: what nothing starts

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why nothing of it runs and `heldBy`, what holds it: the issues that hold it, or, under `fix_running` and `ci_at_head`, CI checks and a head, as each row says.
Each waits for what ends it, in the table's last column.
Name them in the day's report and take nothing for them.

| Word | Meaning | What ends it |
|---|---|---|
| `tree_closed` | Its tree's root is `done`, or still lingering after it left the workflow | An architect's `release_children` of the live tree its Dispatch parent reaches, where that release is accepted, the end of the root's linger, or a `todo` on the root |
| `tree_parked` | Its tree was parked and the linger ran out | A `todo` on the root; your own hold, the parked-root walk decides |
| `label_off` | A root whose `legion` label is off | A person's label |
| `ancestry` | Its Dispatch chain reaches another live root: that tree's unreleased work | That tree's architect releasing it or its parent, or the chain leaving that tree |
| `root_awaiting_person` | Its root waits on a person | The root's resume |
| `design_gate` | Its root's design gate is not open at every gated step | A person's approval of each step's document |
| `dispatch_blocker` | A `blocked_by` issue is not `done` | Each blocker `done`, or the link taken off |
| `children_first` | An open child under it | The last open child leaving the workflow |
| `accepted_ancestor` | The daemon has accepted the head an ancestor's last step passed | That ancestor standing in neither `retro` nor `merging` (a `held` ancestor stands in the phase it was held from) |
| `children_before_retro` | A parent whose task's last step is not `acceptance:owner` is held before retro while its children run | The last `heldBy` issue leaves the workflow, then Legion starts retro, or holds it further for a fix or its CI |
| `task_stopped` | A task is stopped and no step of it runs | A later `register_plan` version that changes the task, a root's `todo`, or a child's restart (`backlog`, then `rerun_child`) starts its first step, and an issue whose experiment's group runs the project's default steps takes no later version |
| `fix_running` | Its task's last step passed, and a fix of the task runs; `heldBy` is the fix's check and head | The fix ends: the steps after it run again, the task goes back, or Legion starts retro once its CI has passed at the head |
| `ci_at_head` | Its task's last step passed, and a check a CI job runs has not passed at its head; `heldBy` is each such check | Each such check passes at the head, then Legion starts retro; a failure starts a fix where the task takes one, else tells its owner once |
| `at_work` | In `accepting`, `retro` or `merging`, not `held`, on no slot | Its own sign-off, retro and merge |
| `unclassified` | None of the above | Nothing of yours: name it with its `phase` and `status` |
