# Reading admission

An issue's `phase` alone does not say where it stands in admission. Read its key in the `admission` lists, and read the root that its `tree` names.
The rules behind the lists: [Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label).

## The lists

- `admission.trees`: the roots of the live trees. Use it, not `admission.active`, to know which trees run.
- `admission.active`: each issue that holds a slot. A root in `awaiting_person` keeps its slot and is in no line. `waitingOn.answeredAt` means the daemon has put it in `admission.waiting` as well, within its priority, among the issues whose work came back, oldest first, where it stands until the pass that resumes it.
- `admission.waiting`: the line, in order. Before you call an order wrong, quote `issues.<KEY>.waitingLine.reason`.
- `admission.blocked`: issues that wait on an unfinished `blocked_by` issue, and parents that wait for their open children. A started issue here gave its slot back. It takes a slot again by itself.

Holding no slot is no fault by itself.
A child at `phase: admitted` in no list waits for its tree. Leave it to its tree.
A root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro` left Legion. Re-admit nothing.

## `admission.held`: what nothing starts

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why it stands in neither and `heldBy`, the issues that hold it.
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
`dispatch read` each root in `admission.trees` that is still at `phase: admitted`.
When `Claimed by:` is `nobody` or ends `· not running`, `envoy_publish` `<KEY>: the claim is free. Claim it again.` to `notifications.role.<issues.<KEY>.architect.locator.claim>`.
