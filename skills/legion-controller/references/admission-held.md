# `admission.held`: what nothing starts

Read this when an issue stands in `admission.held`, for what its word means and what ends it. Where an issue stands in the other lists: `skill://legion-controller/references/admission.md`.

Every issue the record holds in the workflow that is in neither `admission.active` nor `admission.waiting`, with one word for why nothing of it runs and `heldBy`, the issues that hold it. The daemon reads the words in the order of the table below and takes the first that is true, so one issue reads one word, and that word is the same in `read_state` and in the `tick` notice. The list names no issue `admission.active` or `admission.waiting` names, so the three lists together are every issue of the workflow and what each one is doing.

`admission.blocked` keeps its own rows beside them: it names Dispatch links and children-first waits alone, and an issue it names that stands in `admission.held` reads there under whichever word the daemon's order gives it, which can be an earlier one: a child under a shut gate that a link also holds reads `design_gate`. A blocked issue the line carries stands in `admission.waiting` and has no held row at all: a blocked root the line carries, and a started issue that gave its slot back to wait on another issue.

The daemon starts none of them by itself: name them in the day's report and take nothing for them, except where the row's last column names an act of yours.

| Word | Meaning | What ends it |
|---|---|---|
| `tree_closed` | Its tree's root is `done`, or still lingering after it left the workflow | An architect's `release_children` of the live tree its Dispatch parent reaches, where that release is accepted (below) |
| `tree_parked` | Its tree was parked and the linger ran out | A `todo` on the root, and nothing else: no release takes such a member. Where the park is your own hold, the parked-root walk decides it (`skill://legion-controller/references/parked-decisions.md`) |
| `label_off` | A root whose `legion` label is off | A person's label. Take no issue back yourself |
| `ancestry` | Its Dispatch chain reaches another live root: that tree's unreleased work | That tree's architect releasing it or its parent, or the chain leaving that tree |
| `root_awaiting_person` | Its root waits on a person | The root's resume |
| `design_gate` | Its root's design gate is not open at every gated step | A person's approval of each gated step's document at its current version |
| `dispatch_blocker` | A `blocked_by` issue is not `done` | Each blocker `done`, or the link taken off |
| `children_first` | An open child under it | The last open child leaving the workflow |
| `accepted_ancestor` | An ancestor's reviewed head is accepted | That ancestor's close |
| `at_work` | Past its review, not `held`, on no slot. An issue held from one of those phases reads `unclassified` instead, since its hold stopped its work | Its own sign-off, retro and merge; work its architect sends back to `implementing` takes a slot again |
| `unclassified` | None of the above | Nothing of yours: name it with its `phase` and `status` |

## When a `tree_closed` release is accepted

The release records the issue in that tree with its repository kept and its generation raised by one, at `admitted` when it is `todo` and in any other status as a parked child at `done`, which a `todo` runs again. The row alone does not say whether that release would be accepted, and three kinds of issue read this word that no release takes: a member of a tree still in its linger, and a member of a parked tree, whose park starts a linger of its own, both refused while that linger runs; and a member past `admitted`, refused `CHILD_IN_OTHER_TREE`. A member of a closed tree that holds an admission slot stands in `admission.active` and has no held row: a release of one past `admitted` is refused `CHILD_IN_OTHER_TREE`, and of one at `admitted`, `CHILD_IN_OTHER_TREE` while its root's linger runs and `CHILD_NOT_ADOPTABLE` once that linger has ended.
