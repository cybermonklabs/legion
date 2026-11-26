# Conflicts and retargets

Read it when GitHub reports the pull request `CONFLICTING`, the daemon's merge-forward message asks you to merge the base forward or to stop writing the workspace, the controller asks you to resolve a conflict, the change needs something only the base has, the pull request is retargeted, or you read a merge of the base.

## Reintegrating the base

The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the daemon's merge-forward message asks it (*The daemon's ask*, below), the controller asks because of a conflict, the change needs something only the base has, or the pull request was retargeted. Past the review the merger merges the base forward when it merges, for a conflict or for a check the base's newer code breaks (its role prompt, "A head that conflicts with its base"), so the implementer merges then only when the daemon's merge-forward message or the merger asks it. Never to refresh CI or pick up the default branch: each merge is a new head that reruns CI and can void a review.

Before you race another tree's pull request on shared files, `envoy_publish` to its implementer's role topic, ask when its next push lands, and merge once, after it.

1. Push the merge as any code push and build the branch: a clean merge can change a function's signature under the branch's own callers. Run again each `before_merge` check the merge can reach (*Reading a merge of the base*, below), and record each other `before_merge` check again at the merge head as carried from its earlier pass, without running it (`skill://legion-worker/references/results.md#what-a-result-is`). A `ci_check` check is read again from its CI run at the merge head. The merger records none itself: it asks the implementer, and the tester where the task has a tester step, to record theirs at the merge head (its role prompt, "A head that conflicts with its base").
2. Post one PR comment with the Legion footer: `forward merge <old-tip-sha> → <new-tip-sha>; <the condition it meets>`.

After a retarget, do the same onto the new base, and cite the new head's run in the PR body.

What jj does here that an agent may not expect:

- After `jj new <bookmark> <base>`, `@` is the merge commit itself: an edit after it changes the merge, and `@-` names both of its parents.
- `jj git push` refuses a commit with no description, and `@` after a `jj split` is one.
- jj reads a bare branch name in a revset as an expression, so a name git accepts, such as `release|stable`, can name other revisions. The JSON-quoted name that `gh pr view <n> --json baseRefName -q '.baseRefName | @json'` prints is read as that one name.
- `jj undo` and `jj op restore` act on the repository's operation log, which holds the operations of every role of the issue and of the repository's other workspaces, so the last operation there can be another's.

## Reading a merge of the base

Every role reads a merge of the base the same way:

- `git show --remerge-diff <merge>` shows its resolution, and `git diff --stat <merge>^1 <merge>` lists the files the base brought in.
- A check those files or the resolution can reach runs again, and a check the merge was made for has no pass to carry.

For a reviewer, this lists your session's reviews, oldest first; no line, or `no head` on the last one, means you read the whole diff:

```bash
gh api --paginate repos/<o>/<r>/pulls/<n>/reviews --jq '.[] | {id, state, submitted_at, footer: ([.body // "" | scan("<!-- legion: (\\{.*?\\}) -->")] | last | .[0]? // "" | try fromjson catch {})} | select(.footer.phase == "review" and .footer.session == "<your session id>") | [.id, .state, .submitted_at, (.footer.head // "no head")] | @tsv'
```

## The daemon's ask

A head that conflicts with its base keeps the issue in its phase. The daemon reads GitHub twice, at least a minute apart, and acts only on a conflict both reads report. It acts only when the next step needs a mergeable head, which is so in four cases. The issue stands in `merging`. A check the base requires has no green result on the head, since GitHub runs no check on a conflicting head. The base requires a pull request to be current with it. The daemon could not read the base's rules. Where every step of the task has run, in `retro` or in `accepting` with no step of its task after it, the conflict waits for the merger.

- Before `merging` the implementer gets a merge-forward message on its role topic, and in `merging` the merger does. Merge the base forward in `$LEGION_WORKSPACE`, the one workspace every role of the issue shares, as *Reintegrating the base* says, and push the issue branch, with no handoff and no completion for the merge. In `implementing` the phase is the implementer's, and its assignment and its completion stand.
- In `testing` and `reviewing`, the phase's worker is told to stop writing the workspace: stop at once, complete nothing at the old head, and keep your turn and its jobs. When the push lands you are told the new head: read the merge as above and go on at that head.
- `HANDOFF_HEAD_MERGED_FORWARD`: a merge forward the daemon asked for has landed above the head your completion names, or your completion names no head; it moved nothing, and the daemon keeps the refusal, so the same call again is answered `HANDOFF_ALREADY_RECORDED`.
  Read the merge (*Reading a merge of the base*, above), then complete again at the new head with a handoff that names it.
