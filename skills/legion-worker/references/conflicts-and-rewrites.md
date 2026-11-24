# Conflicts, retargets, and a wrong pushed commit

Read it when GitHub reports the pull request `CONFLICTING`, the daemon's merge-forward message asks you to merge the base forward or to stop writing the workspace, a tester's merge of a parent's head with its base conflicts, a message says the issue is in `awaiting_children`, the controller asks you to resolve a conflict, the change needs something only the base has, the pull request is retargeted, you read a merge of the base, or a commit you pushed is wrong.

## Reintegrating the base

The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the daemon's merge-forward message asks it (*The daemon's ask*, below), the controller asks because of a conflict, the change needs something only the base has, or the pull request was retargeted. No task asks for a forward merge of a parent's children: a parent's branch takes its base under these rules, as every pull request's does. Where the tester's merge of a parent's head with its base conflicts, the tester fails the round, and the implementer merges forward once GitHub reads the pull request `CONFLICTING`. Past the review the merger merges the base forward when it merges, for a conflict or for a check the base's newer code breaks (its role prompt, "A head that conflicts with its base"), so the implementer merges then only when the daemon's merge-forward message or the merger asks it. Never to refresh CI or pick up the default branch: each merge is a new head that reruns CI and can void a review. Never with `jj rebase`, which rewrites pushed commits (*A wrong pushed commit*, below).

Before you race another tree's pull request on shared files, `envoy_publish` to its implementer's role topic, ask when its next push lands, and merge once, after it.

1. Merge the bookmark, never `@`, with the base, in one new commit. `base` is the pull request's base branch, JSON-quoted so jj reads it as a string literal:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
     jj -R "$LEGION_WORKSPACE" new legion/<KEY> "${base}@origin" -m "<message>"
   ```

   Write `<message>` in the repository's own commit convention (its commit-lint configuration, else its recent commits on the base), such as `chore(<scope>): merge <base> into <branch>`, or `Merge <base> into <branch>` where none shows. `@` after a split is an empty commit with no description, which `jj git push` refuses.
2. Resolve any conflict in that one commit, then `jj -R "$LEGION_WORKSPACE" new` to move off it.
3. A merge brings instances from the base that the branch's own diff never shows, so before you push the merge search its head for the class of every finding of every earlier review round.
4. Push the merge as any code push, build the branch, and record the issue's checks again at the merge head, each check with no `ci_check` (a check carrying one is read again from its CI run), even with no text conflict. The merger records none itself: it asks the implementer, and the tester where the task has a tester step, to record theirs at the merge head (its role prompt, "A head that conflicts with its base").
5. Post one PR comment with the Legion footer: `forward merge <old-tip-sha> → <new-tip-sha>; <the condition it meets>`, with each class search beside it.

After a retarget, do the same merge onto the new base, and cite the new head's run in the PR body. Recover a jj mistake forward (`jj new`, `jj restore <paths>`), never with `jj undo`, `jj abandon` or `jj op restore`.

## Reading a merge of the base

Every role reads a merge of the base the same way:

- `git show --remerge-diff <merge>` shows its resolution, and `git diff --stat <merge>^1 <merge>` lists the files the base brought in.
- A check those files or the resolution can reach runs again, and a check the merge was made for has no pass to carry.

## The daemon's ask

A head that conflicts with its base keeps the issue in its phase. The daemon reads GitHub twice, at least a minute apart, and acts only on a conflict both reads report. It acts only when the next step needs a mergeable head, which is so in four cases. The issue stands in `merging`. A check the base requires has no green result on the head, since GitHub runs no check on a conflicting head. The base requires a pull request to be current with it. The daemon could not read the base's rules. Where every step of the task has run, in `retro` or in `accepting` with no step of its task after it, the conflict waits for the merger.

- Before `merging` the implementer gets a merge-forward message on its role topic, and in `merging` the merger does. Merge the base forward in `$LEGION_WORKSPACE`, the one workspace every role of the issue shares, as *Reintegrating the base* says, and push the issue branch, with no handoff and no completion for the merge. In `implementing` the phase is the implementer's, and its assignment and its completion stand.
- In `testing` and `reviewing`, the phase's worker is told to stop writing the workspace: stop at once, complete nothing at the old head, and keep your turn and its jobs. When the push lands you are told the new head: read the merge as above and go on at that head.

## A wait on the issue's children

On a message that the issue is in `awaiting_children`, the worker commits and pushes what it has to the issue branch, then pushes nothing more on that phase and does not complete it. While the issue waits there and a merge forward for a conflict has not landed, a worker other than the implementer writes and pushes nothing more, since the implementer may be merging in the workspace they share, and puts what it found on the Dispatch issue (`dispatch comment`); an implementer that has begun the merge finishes it, pushes it and does nothing more, and one that has not begun it begins none while the issue waits: after the return, it merges forward only as *Reintegrating the base* allows. The sentences after the first, and the phase-ended message's own words, decide over the first. Every other phase end still leaves the workspace read-only.

## A wrong pushed commit

Never rewrite a pushed commit: no `jj squash --into` it, no new description, no backward bookmark move, no force push. A rewrite moves every commit built on it and leaves reviews, CI runs and links naming a commit GitHub no longer holds. Answer a wrong commit with a follow-up commit on top, and a wrong message with a line in the PR body. A base frozen for others to stack on takes its fixes the same way; the body's `Chain` line records it.
