# Reading admission

An issue's `phase` alone does not say where it stands in admission. Read its key in the `admission` lists, and read the root that its `tree` names.
The `read_state` summary prints `admission.active`, `admission.waiting` and `admission.blocked` as its `active`, `waiting` and `blocked` lines. No read prints `admission.trees` or `admission.held`.

## The lists

- The live trees: a root is a record whose `tree` is its own key (`read_state` with `issues`). Its tree is live while the root stands in `admission.active` or `admission.waiting`, waits on a person, stands past `admitted` and not `done`, or waits under live work above it, and never while its record carries `lingerUntil`. Use this, not `admission.active`, to know which trees run.
- `admission.active`: each issue that holds a slot. A root in `awaiting_person` keeps its slot and is in no line. `waitingOn.answeredAt` means the daemon has put it in `admission.waiting` as well, within its priority, among the issues whose work came back, oldest first, where it stands until the pass that resumes it.
- `admission.waiting`: the line, in order. Before you call an order wrong, quote `issues.<KEY>.waitingLine.reason`.
- `admission.blocked`: issues that wait on an unfinished `blocked_by` issue, and parents that wait for their open children. A started issue here gave its slot back. It takes a slot again by itself.

Holding no slot is no fault by itself.
A child at `phase: admitted` in no list waits for its tree. Leave it to its tree.
A root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro` left Legion. No wake tells you of it. Re-admit nothing.

## `admission.held`: what nothing starts

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why it stands in neither and `heldBy`, the issues that hold it. The `tick` wake carries it as `held`, and a held issue's record (`read_state` with `issue`) gives its word's sentence in `why`.
Take nothing for them. Name them in the day's report with what ends each.

| Word | Meaning | What ends it |
|---|---|---|
| `tree_closed` | Its root is `done`, or lingers after it left the workflow, or waits in the line for the promotion that opens its tree again | An accepted `release_children` of the live tree that its Dispatch parent reaches, the end of the root's linger, a `todo` on the root, or the root's promotion |
| `tree_parked` | Its tree was parked and the linger ran out | A `todo` on the root. For your own hold, the parked-root walk decides |
| `label_off` | A root whose `legion` label is off | A person's label |
| `ancestry` | Its Dispatch chain reaches another live root | That tree's architect releasing it or its parent, or the chain leaving that tree |
| `root_awaiting_person` | Its root waits on a person | The root's resume |
| `design_gate` | Its root's design gate is not open at each step that holds the tree | A person's approval of each step's document |
| `dispatch_blocker` | A `blocked_by` issue is not `done` | Each blocker `done`, or the link removed |
| `children_first` | An open child under it | The last open child leaving the workflow |
| `accepted_ancestor` | The daemon accepted the head that an ancestor's last step passed | That ancestor standing in neither `retro` nor `merging` |
| `children_before_retro` | A parent held before its retro while its children run | The last `heldBy` issue leaving the workflow |
| `task_stopped` | A task is stopped and no step of it runs | A later `register_plan` version that changes the task, a `todo` on a root, or a child's restart |
| `at_work` | In `accepting`, `retro` or `merging`, not `held`, on no slot. Its workers run as they did | Its own sign-off, retro and merge |
| `unclassified` | None of the above | Nothing of yours. Name it with its `phase` and `status` |

## Trees waiting on a root claim

A root architect whose claim on its root was refused waits. Nothing tells it when the holder lets go.
`dispatch read` each root (a record whose `tree` is its own key) that the summary's `active` line names and whose own line reads `<KEY> admitted <status>`.
When `Claimed by:` is `nobody` or ends `· not running`, post `<KEY>: the claim is free. Claim it again.` as a Dispatch comment on `<KEY>` that mentions `role:<token>`. Build the token as `skill://legion-controller/references/operations.md#a-request-to-another-session` says.

## Your moves to `todo`

Each `todo` that you write is one of these:

- a root that you labelled at a person's request, or filed for a clear defect
- an issue that a person asks you to start, a parked one included
- the restart of a root whose architect failed, or the release of a held issue, once you found no cause or checked that the cause is gone
- a root that you parked with the mark `Parked by Legion while it waits on:`, once its wait ended, or a parked root that a person's later decision gives a next step (`skill://legion-controller/references/parked-decisions.md`)
