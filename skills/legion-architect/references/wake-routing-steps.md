# The steps of a wake's routing

Read this when `skill://legion-architect/references/wake-routing.md` sends you here; its table names the wake each section here answers.

## A lost session

On `session-lost` or `session-forgotten`, check the fresh agent's account of the work against the record, then send it with `envoy_publish` each decision it can no longer read.
Your own lost session: read your record and your tree's issues first.

## A full issue volume

On `volume-near-full`, sort the bytes by where they are:

- Mostly cold bytes in `scratch/tmp` or a workspace: ask the pod's roles on their topics to delete by exact path what they made and no longer need.

- Bytes warm across the clone, workspaces and caches: open a finding ask with the sizes, since `issue_volume` is the operator's.

- Carry a trimmer that switched itself off as `skill://legion-worker/references/findings.md` says.

## A capped pull request

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by `<n>`.
Only requests for changes count: no tester's fail, no move back.
Read what every counted round found, choose one, act on it, and post the rounds you read and the choice as a `dispatch comment`:

- **Narrow**: the change is sound but each round finds more around it.
  Tell the implementer, and a reviewer step where there is one, which checks the next round answers, dropping no requirement.
  A correctness finding or a clear fix in a touched file stays in this pull request.

- **Split**: part of the work cannot settle here.
  On a child, it becomes a child of this issue's parent (a child's own architect sends it up with `envoy_publish`);
  that parent's architect files it with `--blocked-by <this issue>` and releases it at once.
  On the root, open a `dispatch ask`; the part stays a requirement until the person says otherwise.

- **Accept the rounds**: each round found less than the one before, and the last found no defect in the change itself.
  A later notice accepts again only when its round found less.

A finding that changes what the spec requires or a check proves is also a spec change, never instead of the choice.
