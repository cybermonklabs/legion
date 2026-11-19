# Conflicts, retargets, and a wrong pushed commit

Part of `skill://legion-worker`. Read it when GitHub reports the pull request `CONFLICTING`, the
daemon asks you to merge the base forward (a merge-forward message on your role topic), the
controller asks you to resolve a conflict, the change needs something only the base has, the pull
request is retargeted to a new base, or a commit you already pushed is wrong.
Every path it cites is in cybermonklabs/legion.

## Reintegrating the base

- **Merge the base forward only on a real conflict, a need only the base meets, a retarget, or a
  parent's last round, and never with `jj rebase`.**
  The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the daemon's
  merge-forward message asks it (*The daemon's ask*, below), the controller asks
  because of a conflict, the architect's backward move names a parent's last round,
  the change needs something that exists only on the base (a fix for its
  only red check, or a sentence it must change), or after the pull request is retargeted to a new
  base. Otherwise, never reintegrate the base to pick up the default branch or refresh CI. Each
  merge is a new head, reruns CI and can void a review. A single failed CI job is re-run on its
  own (*Never waiting on CI* in `skill://legion-worker/references/pr-body.md`). After any forward merge, build the branch and
  record the issue's checks again at the merge head, each check with no `ci_check` (a check carrying one is read again from its CI run), even with no text conflict: a clean merge can change a
  function's signature under the branch's own callers. Before you race another tree's pull request on
  shared files, `envoy_publish` to its implementer's role topic, ask when its next push lands, and
  merge once, after it.
  For a merge of the base, `git show --remerge-diff <merge>` shows its resolution, and `git diff --stat <merge>^1 <merge>` lists the files the base brought in.
  A check those files or the resolution can reach runs again, and a check the merge was made for has no pass to carry.
  After the push, post one PR comment (Legion footer):
  `forward merge <old-tip-sha> → <new-tip-sha>; <the condition it meets>`.
  A merge brings instances from the base that the branch's own diff never shows, so before you
  push the merge search its head for the class of every finding of every earlier review round,
  and record each search beside that comment.
  In a parent's last round, end that comment with `parent's last round: full test round`, the line
  the tester reads for a full round (`skill://legion-architect/references/acceptance.md`,
  "The last round").
  On the tmux runtime, where issue workspaces are still jj workspaces of one shared clone,
  `jj undo`, `jj abandon` and `jj op restore` rewrite the shared operation log, so a mistake there
  is recovered forward (`jj -R "$LEGION_WORKSPACE" new`, or `jj -R "$LEGION_WORKSPACE" restore
  <paths>` of files). In that shared repository jj always rebases every descendant of any commit it
  rewrites — a revset naming the root of your own chain and rewriting it in place also rewrites
  whatever another tree has stacked on that root, whichever selector chose it (`-s`, `-b`, and
  `-r` all rewrite descendants; `-r` only re-parents them to fill the hole, which is worse).
  Resolve the conflict with a forward merge instead of a rewrite — merge the branch's own
  bookmark with the destination in one new commit, so nothing existing is rewritten and nothing
  built on your prior commits, in this tree or another, ever moves. `base` is the pull request's
  base branch (the repository's default branch unless the pull request is stacked), read as a jj
  string literal: jj parses a bare bookmark name as an expression, and a name pasted between
  quotes can close them itself (`release"@origin|"stable` would select `release` and `stable`), so
  the revset is built from the serialized name alone:

  ```bash
  base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
    jj -R "$LEGION_WORKSPACE" new legion/<KEY> "${base}@origin" -m "<message>"
  ```

  `<message>` is written in the repository's own commit convention, read from the repository
  itself: its commit-message check (a commitlint configuration or the workflow that lints
  commits) where it has one, else its recent commits on the base. Where it uses Conventional
  Commits, that is a listed type and the merge in words, for example
  `chore(<scope>): merge <base> into <branch>`. Where neither shows a convention, it is the
  merge in words alone, for example `Merge <base> into <branch>`. Never a fixed prefix of the
  skill's own: a check that lints every commit of the pull request refuses a type its list does
  not hold, and `merge` is in no Conventional Commits list.

  Merge from the bookmark, never from `@`: a `jj split` leaves `@` an empty, undescribed
  commit above the described one the bookmark already names, and `jj git push` refuses to push
  any commit without a description — merging from `@` drags that undescribed commit into the
  ancestry and the push fails (`Won't push commit … since it has no description`); the bookmark
  is always on a described, already-pushed commit. If the merge conflicts, resolve it in that
  one commit — edit the markers directly; there is nothing to squash, since the merge is the
  only new commit. Then `jj -R "$LEGION_WORKSPACE" new` to move off it, and push it (*Every role
  pushes its own commits* in `skill://legion-worker`; a merge is a code push like any other): the
  merge descends from both the bookmark's old position and the destination, so it is a genuine
  fast-forward.
- **After a retarget.** Retargeting a pull request to a new base does not re-run Tests. Read
  `base` again and merge the bookmark onto it in one command
  (`base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && jj -R "$LEGION_WORKSPACE" new legion/<KEY> "${base}@origin" -m "<message>"`, its `<message>` in the repository's own commit convention, as above) and push it the
  ordinary way, a genuine fast-forward, so the new head runs Tests
  against the new merge result, and cite that run in the PR body.
- **The daemon's ask.** A head that conflicts with its base keeps the issue in its phase. The
  daemon reads GitHub twice, a minute apart, and acts on a conflict only
  when the next step needs a mergeable head: the issue stands in `accepting`, `retro` or
  `merging`, a check the base requires has no green result on the head (GitHub runs no check on
  a conflicting head), the base requires a pull request to be current with it, or the daemon
  could not read the base's rules. Then the issue keeps its phase, and the implementer gets a
  merge-forward message on its role topic: merge the base forward in `$LEGION_WORKSPACE`, the one
  workspace every role of the issue shares, after the working-copy check of
  `skill://legion-worker/references/foreign-path.md`, as this section says, and push the issue
  branch. Write no handoff and make no completion for the merge itself. In `testing`,
  `reviewing` and `merging` the phase is another role's: the daemon told its worker to stop
  writing the workspace and tells it the new head when your push lands. In `accepting` the phase
  is the architect's. In `implementing` and in `retro` the phase is your own, and your
  assignment and its completion stand.

## A wrong pushed commit

A commit you pushed is never rewritten: no `jj squash --into` it, no new description of it, no
backwards bookmark move and no force push. A rewrite moves every commit built on it, another
tree's branch included, and leaves a review, a CI run or a link that names a commit GitHub no
longer holds. Answer a wrong pushed commit with a follow-up commit on top, and a wrong commit
message with a line in the pull request body. A base frozen for others to stack on takes its fixes
the same way, and the PR body's `Chain` line records what is frozen.
