# Conflicts, retargets, fingerprints, and a wrong pushed commit

Read it when GitHub reports the pull request `CONFLICTING`, the controller asks you to resolve a conflict, the change needs something only the base has, the pull request is retargeted, you compare two heads after a forward merge, or a commit you pushed is wrong.

## Reintegrating the base

The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the controller asks because of a conflict, the architect's backward move names a parent's last round, the change needs something only the base has, or the pull request was retargeted. Never to refresh CI or pick up the default branch: each merge is a new head that reruns CI and can void a review. Never with `jj rebase`, which rewrites pushed commits (*A wrong pushed commit*, below).

Before you race another tree's pull request on shared files, `envoy_publish` to its implementer's role topic, ask when its next push lands, and merge once, after it.

1. Record the fingerprint (below) at the current tip.
2. Merge the bookmark, never `@`, with the base, in one new commit. `base` is the pull request's base branch, JSON-quoted so jj reads it as a string literal:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
     jj -R "$LEGION_WORKSPACE" new legion/<KEY> "${base}@origin" -m "<message>"
   ```

   Write `<message>` in the repository's own commit convention (its commit-lint configuration, else its recent commits on the base), such as `chore(<scope>): merge <base> into <branch>`, or `Merge <base> into <branch>` where none shows. `@` after a split is an empty commit with no description, which `jj git push` refuses.
3. Resolve any conflict in that one commit, then `jj -R "$LEGION_WORKSPACE" new` to move off it.
4. The fingerprint answers one question only: whether the branch's own diff changed. It cannot show an instance the merge brought in from the base, so before you push, search the merge head for the class of every finding of every earlier review round.
5. Push the merge as any code push, build the branch, and record the issue's checks again at the merge head, each check with no `ci_check` (a check carrying one is read again from its CI run), even with no text conflict.
6. Record the fingerprint at the new tip and post one PR comment with the Legion footer: `forward merge <old-tip-sha> → <new-tip-sha>; fingerprint <before> → <after>; unchanged|changed`, with each class search beside it. In a parent's last round, end it with `parent's last round: full test round`.

An equal fingerprint makes the merge a confirmation, not a new round; a changed one sends the head through the tester again. After a retarget, do the same merge onto the new base, and cite the new head's run in the PR body. Recover a jj mistake forward (`jj new`, `jj restore <paths>`), never with `jj undo`, `jj abandon` or `jj op restore`, which rewrite the shared operation log.

## The unchanged-diff check

```bash
base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && \
fingerprint=$(set -o pipefail; jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${base}@origin | <head-sha>)" --to <head-sha> \
    --git --context 0 '~(.legion | docs/solutions)' \
  | sed -e '/^@@/d' -e '/^index /d' | sha256sum) && echo "$fingerprint"
```

- `<head-sha>` is a full commit SHA; a head the merge replaced still resolves by its SHA.
- A nonzero exit or no printed fingerprint counts as changed, never as unchanged.
- The fileset must stay one expression: jj unions positional filesets, so `'~.legion' '~docs/solutions'` excludes nothing.
- Your two heads: the implementer, its tip before and after the merge; the tester, the commit its latest results name and the new head; the reviewer, the head its last submitted review's Legion footer names (never that review's `commit_id`, which GitHub moves) and the new head. The merger computes none.
- Beside the hash, compare the set of files the pull request changes before and after (`gh pr diff <n> --name-only`), explain each path that appears or goes, and remove a path that belongs to an already-merged pull request.

## A wrong pushed commit

Never rewrite a pushed commit: no `jj squash --into` it, no new description, no backward bookmark move, no force push. A rewrite moves every commit built on it and leaves reviews, CI runs and links naming a commit GitHub no longer holds. Answer a wrong commit with a follow-up commit on top, and a wrong message with a line in the PR body. A base frozen for others to stack on takes its fixes the same way; the body's `Chain` line records it.
