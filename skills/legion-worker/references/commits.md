# Commit messages, trailers, clones, and a planner's start

Read it before a push; before you cure a commit that lacks a trailer line or clone a repository; and as a planner whose issue moved back from implementing.

- Write no trailer yourself. The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names; the git wrapper adds the first two to a `git commit`. Four kinds of commit get none of these lines added:
  - one that another process made outside the pane's shell with no overlay;
  - one that a git command made without `git commit` (a merge, a revert, a cherry-pick, a rebase);
  - a `git commit` where the git whose version the wrapper reads is older than 2.32 (the first git with `git commit --trailer`; cybermonklabs/legion `docs/deployment.md` says which git that is);
  - on the Claude harness, a jj commit of a subagent's bash that lacks the session's environment file. That subagent's `git commit` still carries the role line.
- Cure a commit that lacks a line by describing it again in the pane's shell; where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the trailer line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.
- `jj git push` refuses a commit that has no author and that the push does not fill; `jj -R "$LEGION_WORKSPACE" metaedit -r <commit> --update-author` fills it.
- A base branch can require the issue line in the commit that lands: a squash builds that message from the branch's commit messages, and it leaves a merge commit's message out, so the line belongs in an ordinary commit.
- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it, and push no commit made under it to a repository that takes no issue key.
- A planner whose issue moved back from implementing starts on the remote tip, never with `jj new @-`, so it pushes no local commit of the implementer's: it reads the change id of `@` first with `jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'change_id.short()'`, then runs `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`, and tells the architect the change id it left.
