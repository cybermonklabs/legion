# Commit messages, trailers, clones, and a planner's start

Read it before a push, after the trailer check in `skill://legion-worker/references/default-branch-check.md`; before you cure a commit that lacks a trailer line or clone a repository; and as a planner whose issue moved back from implementing.

- With the `$default` the trailer check set, read your branch's full commit messages: `jj -R "$LEGION_WORKSPACE" log -r "${default}@origin..@" -T 'description ++ "\n"'`. Earlier phases' commits in it are expected; `description.first_line()` shows no trailer.
- Write no trailer yourself. The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names; the git wrapper adds the first two to a `git commit`. Four kinds of commit get none of these lines added:
  - one that another process made outside the pane's shell with no overlay;
  - one that a git command made without `git commit` (a merge, a revert, a cherry-pick, a rebase);
  - a `git commit` where the git whose version the wrapper reads is older than 2.32 (the first git with `git commit --trailer`; cybermonklabs/legion `docs/deployment.md` says which git that is);
  - on the Claude harness, a jj commit of a subagent's bash that lacks the session's environment file. That subagent's `git commit` still carries the role line.
- Cure a commit that lacks a line by describing it again in the pane's shell; where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the trailer line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.
- A base branch can require the issue line in the commit that lands: a squash builds that message from the branch's commit messages, and it leaves a merge commit's message out, so the line belongs in an ordinary commit.
- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it, and push no commit made under it to a repository that takes no issue key.
- A planner whose issue moved back from implementing starts on the remote tip: `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`.
