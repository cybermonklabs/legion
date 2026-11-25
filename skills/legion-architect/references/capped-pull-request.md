# A capped pull request

Part of `skill://legion-architect/references/wake-routing.md`: read it on a `pr-blocked` wake.

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by `<n>`.
A request for changes and the fail of a review step are what it counts, except at a step of yours.
A tester's fail and a move back count none.
Read what every counted round found, choose one, act on it, and post the rounds you read and the choice as a `dispatch comment`:

- **Narrow**: the change is sound but each round finds more around it.
  Tell the implementation step's agent, and a reviewer step's where there is one, which checks the next round answers, dropping no requirement.
  A correctness finding or a clear fix in a touched file stays in this pull request.

- **Split**: part of the work cannot settle here.
  On a child, it becomes a child of this issue's parent (a child's own architect sends it up with `envoy_publish`).
  That parent's architect files it with `--blocked-by <this issue>` and releases it as section 2 of `skill://legion-architect` says.
  On the root, open a `dispatch ask`.
  The part stays a requirement until the person's answer says otherwise.

- **Accept the rounds**: each round found less than the one before, and the last found no defect in the change itself.
  A later notice accepts again only when its round found less.

A finding that changes what the spec requires or a check proves is also a spec change ("After approval" in `skill://legion-architect/references/design-gate.md`).
It never replaces the choice.
