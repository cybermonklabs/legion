# Reading admission

Read this when you need to know where an issue stands in admission, what an `admission.held` word means, or which trees are live, before you move an issue to `todo`, and before you change admission by signals.
An issue's `phase` alone does not say it: read its key against the `admission` lists and the root its `tree` names.
The `read_state` summary prints `admission.active`, `admission.waiting` and `admission.blocked` as its `active`, `waiting` and `blocked` lines;
no read prints `admission.trees` or `admission.held`.
The rules behind them: [Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label).

## The lists

- `admission.active`: every issue holding a slot, a root or a child, at work, `held` or waiting on a person.
  An issue in `accepting`, `retro` or `merging` holds none (in the default steps, the phases after the review), and a step after `accepting` whose phase keeps a slot waits for one again.
  A started issue that waits on another issue holds none either, except three roots that keep theirs:
  one in `awaiting_person`, one at `admitted` whose gate has never opened, and one carried from another tree.

- The live trees: a root is a record whose `tree` is its own key (`read_state` with `issues`).
  Its tree is live while the root stands in `admission.active` or `admission.waiting`, waits on a person, stands past `admitted` and not `done`, or waits under live work above it, and never while its record carries `lingerUntil`.
  Use this, not `admission.active`, for which trees run.

- `admission.waiting`: the line, in order.
  It is ordered by Dispatch priority first: P0, P1, P2, P3, then the issues with no priority, each issue at its own priority, else at its tree root's.
  Within one priority, first the issues whose work came back, oldest first; then the issues whose first phase can start, by their root's Dispatch rank and then their own.
  A priority changed in Dispatch moves the issue at the next admission pass; priority takes no slot back from work that runs.
  `issues.<KEY>.priority` is the issue's own priority (0 for P0 to 3 for P3, null for none), and `waitingLine.priority` the one it waits at (null for one that waits after P3).
  `issues.<KEY>.waitingLine` says where one stands (`priority`, `part`, `kind`, `holdsSlot`, `reason`): quote its `reason` before you call an order wrong.

- `admission.blocked`: issues waiting on an unfinished `blocked_by` issue, or a parent waiting for its open children.
  A started issue here gave its slot back and takes one again by itself; nothing of its work stops.

- `phase: awaiting_person` with `waitingOn`: a root waiting on a person's answer or review keeps its slot and is in no line.
  `waitingOn.answeredAt` means the daemon has put it in `admission.waiting` as well, within its priority, among the issues whose work came back, oldest first, where it stands until the pass that resumes it.

- `phase: admitted`, in no list, with a `child_of` parent: a child that has not started.
  Leave it to its tree.

- `phase: admitted`, `status: todo`, in no list, no parent: a person took its label off.
  Nothing to do.

- `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro`:
  someone else moved a waiting root, or a root waiting on a person lost its label or claim, and the daemon took it out of the workflow.
  No wake tells you of it.
  Re-admit nothing.

Holding no slot is no fault by itself.

## `admission.held`: what nothing starts

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why nothing of it runs and `heldBy`, the issues that hold it.
The `tick` wake carries it as `held`, and a held issue's record (`read_state` with `issue`) gives its word's sentence in `why`.
Each waits for what ends it, in the table's last column.
Name them in the day's report and take nothing for them.

| Word | Meaning | What ends it |
|---|---|---|
| `tree_closed` | Its tree's root is `done`, or still lingering after it left the workflow, or waiting in the line, its design gate open, for the promotion that opens its tree again | An architect's `release_children` of the live tree its Dispatch parent reaches, where that release is accepted, the end of the root's linger, a `todo` on the root, or the root's promotion from the waiting line |
| `tree_parked` | Its tree was parked and the linger ran out | A `todo` on the root; your own hold, the parked-root walk decides |
| `label_off` | A root whose `legion` label is off | A person's label |
| `ancestry` | Its Dispatch chain reaches another live root: that tree's unreleased work | That tree's architect releasing it or its parent, or the chain leaving that tree |
| `root_awaiting_person` | Its root waits on a person | The root's resume |
| `design_gate` | Its root's design gate is not open at every step that holds the tree: each gated step, and `criteria` where the root registered it | A person's approval of each step's document |
| `dispatch_blocker` | A `blocked_by` issue is not `done` | Each blocker `done`, or the link taken off |
| `children_first` | An open child under it | The last open child leaving the workflow |
| `accepted_ancestor` | The daemon has accepted the head an ancestor's last step passed | That ancestor standing in neither `retro` nor `merging` (a `held` ancestor stands in the phase it was held from) |
| `children_before_retro` | A parent whose task's last step is not `acceptance:owner` is held before retro while its children run | The last `heldBy` issue leaves the workflow, then Legion starts retro |
| `task_stopped` | A task is stopped and no step of it runs | A later `register_plan` version that changes the task, a root's `todo`, or a child's restart (`backlog`, then `rerun_child`) starts its first step, and an issue whose experiment's group runs the project's default steps takes no later version |
| `at_work` | In `accepting`, `retro` or `merging`, not `held`, on no slot | Its own sign-off, retro and merge |
| `unclassified` | None of the above | Nothing of yours: name it with its `phase` and `status` |

## Free slots

A slot is one issue the daemon started, a root or a child. Free slots are `admission.cap`, less every issue in `admission.active`, less each issue of `admission.waiting` in neither `admission.active` nor `admission.blocked`. The line is ordered by Dispatch priority first; within one priority a returned issue goes before an unstarted one whatever their ranks, by design (the site's concepts page, "Admission and the `legion` label").

## Your moves to `todo`

Every move to `todo` you make is one of these:

- a root you labelled at a person's request, or filed for a clear defect;

- an issue a person asks you to start, a parked one included;

- a root in the workflow whose architect failed, once you found no cause or checked that its cause is gone; one that is held, once you checked that its cause is gone; one whose architect retired, once a person's decision gives its work a next step and you found no cause or checked that its cause is gone;

- a tree you parked with the mark `Parked by Legion while it waits on:`, once its cause is gone or its event happened (comment the mark and what you checked), or a person's later decision gives it a next step;

- stopped-session work a person's directive in the weekly plan moves to Legion.

## Admission by signals

Change admission by signals only on a person's cited word, in `reason`: `signals_on`, `signals_off`, `set_signal_limit`, `clear_signal_limit`.
You regulate nothing yourself.
What each signal is, its limit and how to read it: [Admission by signals](https://cybermonklabs.github.io/legion/legion/admission-signals/).
