# Conflicts and retargets

Merge the base into the issue branch only when GitHub reports the pull request conflicting, the daemon's merge-forward message asks for it, your own architect asks because of a conflict that another tree reported, the change needs something only the base has, or the pull request was retargeted. Each merge is a new head that runs CI again and can void a review.

What jj does here that an agent may not expect:

- After `jj new <bookmark> <base>`, `@` is the merge commit itself.
- `jj git push` refuses a commit with no description, as `@` is after a `jj split`.
- jj reads a bare branch name in a revset as an expression; the JSON-quoted name (`gh pr view <n> --json baseRefName -q '.baseRefName | @json'`) is one name.
- `jj undo` and `jj op restore` act on an operation log that every role of the issue and the repository's other workspaces share.

After the merge, build the branch, push it, and post one pull request comment that names the condition the merge meets.

## Reading a merge of the base

`git show --remerge-diff <merge>` shows its resolution, and `git diff --stat <merge>^1 <merge>` lists the files the base brought in. A check those files or the resolution can reach runs again; record each other `before_merge` check at the new head as carried from its earlier pass.

## The daemon's ask

A merge-forward message asks the implementer, or in `merging` the merger, to merge the base forward and push, with no handoff and no completion for the merge. A tester or reviewer that holds the phase is told to stop writing the workspace, and later the new head. `HANDOFF_HEAD_MERGED_FORWARD` means a merge landed above the head your completion names: read the merge, then complete again at the new head with a handoff that names it.
