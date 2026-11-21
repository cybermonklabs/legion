# A refused release or rerun

Part of `skill://legion-architect/references/decomposition.md`. Read this when
`release_children` or `rerun_child` is refused. A refused release writes nothing:
release the rest again without the child it names.

- `CHILD_NOT_ADOPTABLE`: it runs or ran as a tree of its own, carries a run handed over to it, or
  was handed over as a root of its own; or it is a member of a closed tree that holds an admission
  slot, which the daemon leaves where it stands. Leave it there, or ask the controller.
- `CHILD_IN_OTHER_TREE`: re-parent it back in Dispatch, or ask that tree's architect or the
  controller. A member of a closed tree, or a root held under an unreleased child of yours: read
  "A root that joins your tree, and a refused release" in
  `skill://legion-architect/references/wake-routing-children.md`.
- `ISSUE_NOT_OWNED`: the issue is another architect's (the tree root, your own issue, a sibling of
  it or an issue under a sibling); send the move to the architect above you. A child's own
  architect cannot restart its own issue, so it sends a restart of its issue there too.
- `CHILD_UNDER_ACCEPTED_PARENT`: an ancestor is already accepted (in `retro` or `merging`). Ask the
  role that holds its phase (the implementer in `retro`, the merger in `merging`) to move it back
  with `request_backward_move`, or release the child once the ancestor has closed. Your own
  `request_backward_move` works only from `accepting`.
