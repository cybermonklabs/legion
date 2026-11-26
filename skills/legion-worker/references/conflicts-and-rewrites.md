# Conflicts, retargets, and a wrong pushed commit

Read this section for a conflict GitHub reports, a merge-forward message, a change that needs something only the base has, a retarget, a merge of the base, or a wrong pushed commit.

## Reintegrating the base

The agent of the task's implementation step (the implementer, in the default steps) merges the base into the issue branch on only three triggers:

- GitHub reports the pull request `CONFLICTING`.
  The daemon's merge-forward message counts as this trigger because two reads of GitHub confirmed the conflict (*The daemon's ask*, below).
  A controller request counts only if it identifies a conflict GitHub reports.

- The change needs something only the base has, such as a fix for its only red check.

- The pull request was retargeted.

Outside `implementing`, wait for the daemon's merge-forward message because the current phase owns the workspace.
In `retro`, in `merging`, and in `accepting` with no task step after acceptance, the merger owns the merge.
The daemon asks the merger in `merging`.
In those phases, merge only if the daemon already sent you the merge-forward message or the merger asks you to.
The merger merges forward for a conflict or for a check the base's newer code breaks (its role prompt, "A head that conflicts with its base").
If any other message asks for a merge without identifying a conflict GitHub reports, reply with this rule and do not merge.

Never merge just to refresh CI or to pick up the default branch.
Each merge creates a new head, reruns CI and can void a review.
Never use `jj rebase`, which rewrites pushed commits (*A wrong pushed commit*, below).

Before you race another tree's pull request on shared files, ask the agent of its implementation step when its next push lands.
`envoy_publish` to that agent's role topic, which `dispatch steps --issue <KEY>` names, since an issue can have two implementer agents.
Where its task has no step of the type implementation, ask its architect.
Merge once, after that push.

1. Merge the bookmark, never `@`, with the base, in one new commit. `base` is the pull request's base branch, JSON-quoted so jj reads it as a string literal:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && \
     jj -R "$LEGION_WORKSPACE" new legion/<KEY> "${base}@origin" -m "<message>"
   ```

   Write `<message>` in the repository's own commit convention (its commit-lint configuration, else its recent commits on the base), such as `chore(<scope>): merge <base> into <branch>`, or `Merge <base> into <branch>` where none shows. `@` after a split is an empty commit with no description, which `jj git push` refuses.
2. Resolve any conflict in that one commit, then `jj -R "$LEGION_WORKSPACE" new` to move off it.
3. Push the merge as any code push and build the branch: a clean merge can change a function's signature under the branch's own callers. Run again each check the merge can reach (*Reading a merge of the base*, below), and record each other check again at the merge head as carried from its earlier pass, without running it (`skill://legion-worker/references/results.md#what-a-result-is`). A `ci_check` check is read again from its CI run at the merge head. The merger records none itself: it asks the implementer, and the tester where the task has a tester step, to record theirs at the merge head (its role prompt, "A head that conflicts with its base").
4. Post one PR comment with the Legion footer: `forward merge <old-tip-sha> → <new-tip-sha>; <the condition it meets>`.

After a retarget, do the same merge onto the new base, and cite the new head's run in the PR body. Recover a jj mistake forward (`jj new`, `jj restore <paths>`), never with `jj undo`, `jj abandon` or `jj op restore`.

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

- Before `merging` the agent of the task's implementation step, its last step of the type implementation at or before the issue's place, else its last such step (the implementer, in the default steps), gets a merge-forward message on its role topic, and in `merging` the merger does. Merge the base forward in `$LEGION_WORKSPACE`, the one workspace every role of the issue shares, as *Reintegrating the base* says, and push the issue branch, with no handoff and no completion for the merge. At that agent's own step (`implementing`, in the default steps) it is told nothing more, and its assignment and its completion stand.
- Elsewhere, in `testing`, `reviewing`, `step`, `accepting` or `retro`, the agent of the step or retro the issue stands at (at `step`, the role of the deployment or of the plan whose step runs) is told to stop writing the workspace, unless it is that same agent: stop at once, complete nothing at the old head, and keep your turn and its jobs. When the push lands you are told the new head: read the merge as above and go on at that head.
- `HANDOFF_HEAD_MERGED_FORWARD`: a merge forward the daemon asked for has landed above the head your completion names, or your completion names no head; it moved nothing, and the daemon keeps the refusal, so the same call again is answered `HANDOFF_ALREADY_RECORDED`.
  Read the merge (*Reading a merge of the base*, above), then complete again at the new head, with a handoff that names it where your phase writes one.

## A wrong pushed commit

Never rewrite a pushed commit: no `jj squash --into` it, no new description, no backward bookmark move, no force push. A rewrite moves every commit built on it and leaves reviews, CI runs and links naming a commit GitHub no longer holds. Answer a wrong commit with a follow-up commit on top, and a wrong message with a line in the PR body. A base frozen for others to stack on takes its fixes the same way; the body's `Chain` line records it.
