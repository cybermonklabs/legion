# Reading admission

Read this when you need to know where an issue stands in admission, what an `admission.held` word means (`skill://legion-controller/references/admission-held.md`), or which trees are live, and before you move an issue to `todo`.
An issue's `phase` alone does not say it: read its key against the `admission` lists and the root its `tree` names.
The `read_state` summary prints `admission.active`, `admission.waiting`, `admission.blocked` and `admission.trees` as its `active`, `waiting`, `blocked` and `trees` lines, and each entry of `admission.held` as a `held` line.
The rules behind them: [Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label).

## The lists

- `admission.active`: every issue holding a slot, a root or a child, at work, `held` or waiting on a person.
  The regular cap counts only holders that are not P0 (`admission.p0Holders` gives how many are P0), so `admission.active` can hold more issues than `admission.cap`.
  An issue in `accepting`, `retro` or `merging` holds none (in the default steps, the phases after the review), and a step after `accepting` whose phase keeps a slot waits for one again.
  A started issue that waits on another issue holds none either, except three roots that keep theirs:
  one in `awaiting_person`, one at `admitted` whose gate has never opened, and one carried from another tree.

- `admission.p0Holders` and `admission.p0Bound`: how many holders in `admission.active` are P0, and the P0 bound in force (`bound`, null for none) with `legion.yaml`'s `configured` value and any `live` value.
  An issue is P0 when its `resolvedPriority` is 0, or, where `resolvedPriority` is absent, its own `priority` is 0.
  A P0 issue starts outside the cap while fewer than `bound` P0 issues hold a slot, or always when `bound` is null.

- The live trees: a root is a record whose `tree` is its own key (`read_state` with `issues`).
  Its tree is live while the root stands in `admission.active` or `admission.waiting`, waits on a person, stands past `admitted` and not `done`, or waits under live work above it, and never while its record carries `lingerUntil`.
  Use this, not `admission.active`, for which trees run.

- `admission.waiting`: the line, in order.
  It is ordered by the priority Dispatch resolves first: P0, P1, P2, P3, then the issues with no priority.
  Within one priority, first the issues whose work came back, oldest first; then the issues whose first phase can start, by their root's Dispatch rank and then their own.
  A priority changed in Dispatch moves the issue at the next admission pass; priority takes no slot back from work that runs.
  `issues.<KEY>.priority` is the issue's own priority (0 for P0 to 3 for P3, null for none), `resolvedPriority` is what Dispatch resolves (with `priorityFrom` naming the ancestor when inherited), and `waitingLine.priority` is the one it waits at (null for one that waits after P3).
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

## Free slots

A slot is one issue the daemon started, a root or a child.
Free slots are `admission.cap`, less the holders of `admission.active` that are not P0 (its length less `admission.p0Holders`), less each issue of `admission.waiting` that is not P0 and is in neither `admission.active` nor `admission.blocked`.

## Your moves to `todo`

Every move to `todo` you make is one of these:

- a root you labelled at a person's request, or filed for a clear defect;

- an issue a person asks you to start, one in `backlog` or `icebox` included;

- a root in the workflow whose architect failed, once you found no cause or checked that its cause is gone; one that is held, once you checked that its cause is gone; one whose architect retired, once a person's decision gives its work a next step and you found no cause or checked that its cause is gone;

- a tree you moved to `backlog` with your wait mark (`skill://legion-controller/references/backlog-asks.md` step 4), once its cause is gone or its event happened (comment the mark and what you checked), or a person's later decision gives it a next step;

- stopped-session work a person's directive in the weekly plan moves to Legion.
