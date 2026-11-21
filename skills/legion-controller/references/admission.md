# Reading admission

Read this when you need to know where an issue stands in admission, what an `admission.held` word means, or which trees are live, and before you move an issue to `todo`. An issue's `phase` alone does not say it: read its key against the `admission` lists and the root its `tree` names. The `read_state` summary prints `admission.active`, `admission.waiting` and `admission.blocked` as its `active`, `waiting` and `blocked` lines; no read prints `admission.trees` or `admission.held`. The rules behind them: [Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label).

## The lists

- `admission.active`: every issue holding a slot, a root or a child, at work, `held` or waiting on a person. An issue past its review (`accepting`, `retro`, `merging`) holds none, and neither does a started issue that waits on another issue, except three roots that keep theirs: one in `awaiting_person`, one at `admitted` whose gate has never opened, and one carried from another tree.
- The live trees: a root is a record whose `tree` is its own key (`read_state` with `issues`). Its tree is live while the root stands in `admission.active` or `admission.waiting`, waits on a person, stands past `admitted` and not `done`, or waits under live work above it, and never while its record carries `lingerUntil`. Use this, not `admission.active`, for which trees run.
- `admission.waiting`: the line, in order. First the issues whose work came back, oldest first; then the issues whose first phase can start, by their root's Dispatch rank and then their own. `issues.<KEY>.waitingLine` says where one stands (`part`, `kind`, `holdsSlot`, `reason`): quote its `reason` before you call an order wrong.
- `admission.blocked`: issues waiting on an unfinished `blocked_by` issue, or a parent waiting for its open children. A started issue here gave its slot back and takes one again by itself; nothing of its work stops.
- `phase: awaiting_person` with `waitingOn`: a root waiting on a person's answer or review keeps its slot and is in no line. `waitingOn.answeredAt` means the daemon has put it in `admission.waiting` as well, among the issues whose work came back, oldest first, where it stands until the pass that resumes it.
- `phase: admitted`, in no list, with a `child_of` parent: a child that has not started. Leave it to its tree.
- `phase: admitted`, `status: todo`, in no list, no parent: a person took its label off. Nothing to do.
- `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro`: someone else moved a waiting root, or a root waiting on a person lost its label or claim, and the daemon took it out of the workflow. No wake tells you of it. Re-admit nothing.

Holding no slot is no fault by itself.

## `admission.held`: what nothing starts

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why nothing of it runs and `heldBy`, the issues that hold it. The `tick` wake carries it as `held`, and a held issue's record (`read_state` with `issue`) gives its word's sentence in `why`. The daemon starts none of them by itself: name them in the day's report and take nothing for them.

| Word | Meaning | What ends it |
|---|---|---|
| `tree_closed` | Its tree's root is `done`, or still lingering after it left the workflow | An architect's `release_children` of the live tree its Dispatch parent reaches, where that release is accepted |
| `tree_parked` | Its tree was parked and the linger ran out | A `todo` on the root; your own hold, the parked-root walk decides |
| `label_off` | A root whose `legion` label is off | A person's label |
| `ancestry` | Its Dispatch chain reaches another live root: that tree's unreleased work | That tree's architect releasing it or its parent, or the chain leaving that tree |
| `root_awaiting_person` | Its root waits on a person | The root's resume |
| `design_gate` | Its root's design gate is not open at every gated step | A person's approval of each step's document |
| `dispatch_blocker` | A `blocked_by` issue is not `done` | Each blocker `done`, or the link taken off |
| `children_first` | An open child under it | The last open child leaving the workflow |
| `accepted_ancestor` | An ancestor's reviewed head is accepted | That ancestor's close |
| `at_work` | Past its review, not `held`, on no slot | Its own sign-off, retro and merge |
| `unclassified` | None of the above | Nothing of yours: name it with its `phase` and `status` |

## Your moves to `todo`

Every move to `todo` you make is one of these:

- a root you labelled at a person's request, or filed for a clear defect;
- an issue a person asks you to start, a parked one included;
- a root in the workflow whose architect failed, once you found no cause or checked that its cause is gone; one that is held, once you checked that its cause is gone; one whose architect retired, once a person's decision gives its work a next step and you found no cause or checked that its cause is gone;
- a tree you parked with the mark `Parked by Legion while it waits on:`, once its cause is gone or its event happened (comment the mark and what you checked), or a person's later decision gives it a next step;
- stopped-session work a person's directive in the weekly plan moves to Legion.
