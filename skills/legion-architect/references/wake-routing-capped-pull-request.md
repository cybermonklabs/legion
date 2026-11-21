# A capped pull request

Read this when a `pr-blocked` notice reaches you: the review round cap is reached, and you decide
in this turn.

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by
`<n>`. `<n>` counts the reviews that requested changes, less the conflict rounds. A tester's fail,
a worker's `request_backward_move` and your own send-back from `accepting` count in neither number.
A backward move's reason is in its `phase-finished` notice
(`moved back to <phase> by the <role>: <reason>`), and Legion posts it on the issue. Read what every
counted round found, choose one, act on it, and post the rounds you read and the choice as a
`dispatch comment`:

- **Narrow**: the change is sound but each round finds more around it. Tell the implementer and the
  reviewer which checks the next round answers, dropping no requirement. A correctness finding is
  still fixed in this pull request, and so is a wording nit or any clear fix in a file it touches,
  so the `Fast-follow:` line carries only cleanup in files it does not touch.
- **Split**: part of the work cannot settle here. On a child, it becomes a child of this issue's
  parent (a child's own architect sends it up with `envoy_publish`); that parent's architect files
  it after the red-team check with `--blocked-by <this issue>` and releases it at once. A
  requirement moves only to that released child. On the root, open a `dispatch ask`; the part
  stays a requirement until the person's answer says otherwise.
- **Accept the rounds**: each round found fewer findings than the one before, and the last found no
  defect in the change itself. A later notice accepts again only when its round found fewer
  findings than the round you last accepted.

Deliver every criterion of your issue's spec in this tree: the root spec on a root, the child's
spec on a child. Work leaves the tree only when a root splits, as a `dispatch ask` for the rest that
the root's architect opens: work outside has no owner.

A finding that changes what the spec requires or a check proves is also a spec change, never
instead of the choice. On the root, it is a decision block ("After approval" in
`skill://legion-architect/references/design-gate.md`). On a child, it is a new version of the
child's spec, which the architect above the child reviews, and a decision only a person can make
goes to the architect above.
