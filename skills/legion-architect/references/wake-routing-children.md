# Wake routing: a child's wakes

Read this when a `child-status`, `child-closed`, `child-release-stalled` or `child-not-recorded`
wake reaches you, or a `release_children` is refused with `CHILD_IN_OTHER_TREE`. "Section N" is
that section of `skill://legion-architect`.

| Wake | What to do |
| --- | --- |
| `child-status` | A child left, re-entered, joined the tree or started. The daemon starts what runs. |
| `child-closed` | Section 2. File any new child it calls for after the red-team check, and release it at once with `release_children`, blocked by what it builds on. When a worker waits on this child for a missing surface (see `phase-finished`), tell it to continue with `envoy_publish` to its role topic. The last child's close is not the end-game (section 3). When it was the last child an issue in `accepting` waits on, run that issue's last round. |
| `child-release-stalled` | It comes to the architect whose `release_children` it was, with two exceptions: the notice the daemon writes when it recovers a repository GitHub did not answer, and the one it writes when it stops waiting for such a repository. Whoever ran the release, these two go to the architect that owns each child's parent where your tree records that parent, and to the tree root's architect for the rest. Read the parent's record (`read_record`), release the parent first when the reason says so, then release the child again. Tell the controller if the parent never records. |
| `child-not-recorded` | Set the child's repository (`dispatch issue-update --repo owner/name`) or get the named App installed, then release it again; else tell the controller. |

## A root that joins your tree, and a refused release

- A root that waited in the admission queue, never ran and never left another tree joins your tree
  once a person makes it your child, if it is `todo`, and otherwise at your release of it (the
  `child-status` row above); once in, it is held for its blockers like any child you release. A
  root that left another tree, handed over by its finished tree or admitted as an orphan, stays its
  own tree and never joins yours, even one that was your root's child before your root was admitted
  again. A labelled `todo` issue under a child you have not released, unless it ran as a tree of
  its own or left another tree, is held under that unreleased child (not a blocked-by hold): it
  waits for no slot until your release of it or of that child brings it in, with any `todo` child
  of its own.
- A release refused `CHILD_IN_OTHER_TREE` names the tree the child runs in. One kind of other
  tree's child does join yours: a member of a tree whose root the daemon's record says is `done`
  and whose linger has run out, standing at `admitted` with no slot of its own, which your release
  records in your tree with its repository kept and its generation raised by one — at `admitted`
  when it is `todo`, and in any other status out of the workflow at `done`, which a `todo` runs
  again. `admission.held` reads `tree_closed` for more issues than a release takes: a member whose
  tree's linger still runs, and a member past `admitted`, read that word too and stay refused (the
  `tree_closed` row of the controller skill's `references/admission-held.md`). When the tree the
  refusal names is a root held under an unreleased child of yours (a root that queued and never
  ran, so no architect has started on it), release that root, or the unreleased child it sits
  under, instead: its children come with it. Otherwise re-parent the child back in Dispatch, or
  ask that tree's architect or the controller.
