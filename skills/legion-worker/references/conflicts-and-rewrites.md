# Conflicts, retargets, and a wrong pushed commit

Read it when GitHub reports the pull request `CONFLICTING`, the daemon's merge-forward message asks you to merge the base forward or to stop writing the workspace, the controller asks you to resolve a conflict, the change needs something only the base has, the pull request is retargeted, you read a merge of the base, or a commit you pushed is wrong.

## Reintegrating the base

The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the daemon's merge-forward message asks it (*The daemon's ask*, below), the controller asks because of a conflict, the architect's backward move names a parent's last round (its `Reason:` starts `Last round:`: the parent's full test round once its children merged, `skill://legion-architect/references/acceptance.md`, "The last round"), the change needs something only the base has, or the pull request was retargeted. Never to refresh CI or pick up the default branch: each merge is a new head that reruns CI and can void a review. Never by rebasing the branch, which rewrites pushed commits (*A wrong pushed commit*, below); the one rebase is of `@` in step 1, which is never pushed.

A tree is a root issue and the issues beneath it. When another tree's open pull request changes files yours changes, the two race: the second to merge meets a conflict. Before you race it, `envoy_publish` to its implementer's role topic, ask when its next push lands, and merge once, after it.

1. List the commits the bookmark lacks (`jj -R "$LEGION_WORKSPACE" log -r 'legion/<KEY>..@-'`): in `implementing` only your own, in any other phase none; else push nothing and tell the architect, since the merge leaves such a commit off the branch and the disk. In `implementing`, commit your own edits first and point the bookmark at them (`jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @-`), so the merge carries them; if jj refuses the move, stop and tell the architect. In any other phase, commit nothing but the merge (`skill://legion-worker/references/handoff-and-completion.md`). Then merge the bookmark, never `@`, with the base, in one new commit, and put `@` on top of it. `base` is the pull request's base branch, JSON-quoted so jj reads it as a string literal:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
     jj -R "$LEGION_WORKSPACE" new --no-edit legion/<KEY> "${base}@origin" -m "<message>"
   jj -R "$LEGION_WORKSPACE" rebase -r @ -o <merge>
   ```

   `jj new --no-edit` prints `<merge>`, its id. After the rebase, an `@` line the base also changed leaves its file on disk in conflict markers, its text still in `@`.

   Write `<message>` in the repository's own commit convention, read from the repository itself: its commit-message check (a commitlint configuration or the workflow that lints commits) where it has one, else its recent commits on the base. Where it uses Conventional Commits, that is a listed type and the merge in words, such as `chore(<scope>): merge <base> into <branch>`; where neither shows a convention, the merge in words alone, such as `Merge <base> into <branch>`. Never a fixed prefix of this skill's own: a check that lints every commit of the pull request refuses a type its list does not hold, and `merge` is in no Conventional Commits list.
2. Resolve only the merge's conflicts in `@`. Then list what `@` holds beyond the merge: `jj -R "$LEGION_WORKSPACE" diff -r @ --name-only`. Your resolutions are expected there; any other path is another role's write, or an edit of yours outside `implementing`. Move your resolutions into the merge:

   ```bash
   jj -R "$LEGION_WORKSPACE" squash --into @- <your resolution paths…>
   ```

   Skip it when there is nothing to resolve. `@-` is then the merge, with its message and your resolutions, which step 4 pushes. When `@` still holds a path, leave it there, tell the architect and the phase's role each such path and `@`'s change id, and run your role prompt's turn-start rule before your next commit. `@` has no description, so the daemon's adoption before a phase task keeps it as `@` for the next role that commits to set aside.

   When one path holds both your resolution and an edit the merge must not carry, push nothing, and tell the architect.
3. A merge brings instances from the base that the branch's own diff never shows, so before you push the merge search its head for the class of every finding of every earlier review round. A finding's class is the defect it named wherever the head holds it, not the one line it cited: search the whole head, not the lines the merge changed alone.
4. Push the merge as any code push, build the branch, and record the issue's checks again at the merge head, each check with no `ci_check` (a check carrying one is read again from its CI run), even with no text conflict.
5. Post one PR comment with the Legion footer: `forward merge <old-tip-sha> → <new-tip-sha>; <the condition it meets>`, with each class search beside it. In a parent's last round, end it with `parent's last round: full test round`.

After a retarget, do the same merge onto the new base, and cite the new head's run in the PR body. Recover a jj mistake forward (`jj new`, `jj restore <paths>`), never with `jj undo`, `jj abandon` or `jj op restore`. Each adds an operation rather than rewriting the operation log, but each can take back or drop another role's work in the workspace every role shares: `jj undo` reverts the newest operation, which can be another role's; `jj op restore` returns the repository to an earlier operation's state, undoing every operation since; and `jj abandon` drops a commit and the changes it holds. The one exception is `jj abandon` of the change your role prompt's turn-start rule set aside, once you took back every path it holds.

## Reading a merge of the base

Every role reads a merge of the base the same way:

- `git show --remerge-diff <merge>` shows its resolution, and `git diff --stat <merge>^1 <merge>` lists the files the base brought in.
- A check those files or the resolution can reach runs again, and a check the merge was made for has no pass to carry.

## The daemon's ask

A head that conflicts with its base keeps the issue in its phase. The daemon reads GitHub twice, at least a minute apart, and acts only on a conflict both reads report, and only when the next step needs a mergeable head: the issue stands in `accepting`, `retro` or `merging`; a check the base requires has no green result on the head, since GitHub runs no check on a conflicting head; the base requires a pull request to be current with it; or the daemon could not read the base's rules.

- The implementer gets a merge-forward message on its role topic. Merge the base forward in `$LEGION_WORKSPACE`, the one workspace every role of the issue shares, as *Reintegrating the base* says, and push the issue branch, with no handoff and no completion for the merge. In `accepting` the phase is the architect's. In `implementing` and `retro` the phase is the implementer's, and its assignment and its completion stand.
- In `testing`, `reviewing` and `merging`, the phase's worker is told to stop writing the workspace: stop at once, complete nothing at the old head (the merger merges and enqueues nothing until the new head lands and the reviewer has approved it), and keep your turn and its jobs. When the push lands you are told the new head: read the merge as above and go on at that head.

## A wrong pushed commit

Never rewrite a pushed commit: no `jj squash --into` it, no new description, no backward bookmark move, no force push. A rewrite moves every commit built on it and leaves reviews, CI runs and links naming a commit GitHub no longer holds. Answer a wrong commit with a follow-up commit on top, and a wrong message with a line in the PR body. A base frozen for others to stack on takes its fixes the same way; the body's `Chain` line records it.
