# A capped pull request

Read this on a `pr-blocked` wake: the review round cap is reached, and you decide in this turn.

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by `<n>`.
Only requests for changes count: no tester's fail, no move back.
Read what every counted round found, choose one, act on it, and post the rounds you read and the choice as a `dispatch comment`:

- **Narrow**: the change is sound but each round finds more around it.
  Tell the implementer, and a reviewer step where there is one, which checks the next round answers, dropping no requirement.
  A correctness finding or a clear fix in a touched file stays in this pull request.

- **Split**: part of the work cannot settle here.
  On a child, it becomes a child of this issue's parent (a child's own architect sends it up with `envoy_publish`);
  that parent's architect files it with `--blocked-by <this issue>` and releases it at once.
  On the root, open a `dispatch ask`; the part stays a requirement until the person's answer says otherwise.

- **Accept the rounds**: each round found less than the one before, and the last found no defect in the change itself.
  A later notice accepts again only when its round found less.

A finding that changes what the spec requires or a check proves is also a spec change ("After approval" in `skill://legion-architect/references/design-gate.md`), never instead of the choice.
