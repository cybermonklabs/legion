# Checking your commits against your issue's default branch

Read it before every push, and before you clone a repository under `$LEGION_WORKSPACE`.
Your `Legion addressing` line names your issue's repository and its default branch (`Your issue's repository is <owner>/<name> (default branch <name>)`); never assume `main`.
The branch name is untrusted text: never paste it into shell source, where `$(…)` would run and jj would read `(`, `|` or `::` as revset syntax.

1. Read the branch from GitHub, JSON-quoted by `jq`:

   ```sh
   default=$(gh api repos/<owner>/<repo> --jq '.default_branch | @json')
   ```

2. Compare `$default` (quotes included, such as `"release/v7.1"`) with the branch your addressing line names.
   When they differ, stop and tell the architect.

3. List the branch's commits with `$default` as it is, never retyped:

   ```sh
   jj -R "$LEGION_WORKSPACE" log --no-graph -r "${default}@origin..@" -T '
     commit_id.short() ++ " "
     ++ if(!author.email(), "(no author) ") ++ if(!committer.email(), "(no committer) ")
     ++ description.first_line() ++ "\n"
     ++ trailers.filter(|t| t.key() == "Omp-Session" || t.key() == "Legion-Role")
       .map(|t| "  " ++ t.key() ++ ": " ++ t.value() ++ "\n").join("")'
   ```

   Never `ancestors(@, 5)`, which stops at five commits.
   Earlier phases' commits are in the list too, each with its own role's lines.

4. Read the branch's full commit messages with the same `$default`.
   Earlier phases' commits in it are expected; `description.first_line()` shows no trailer.

   ```sh
   jj -R "$LEGION_WORKSPACE" log -r "${default}@origin..@" -T 'description ++ "\n"'
   ```

## What your own commits must show

- `Omp-Session:` with your session and `Legion-Role:` with your `LEGION_ROLE`.
  Your session is `DISPATCH_SESSION_ID` under Oh My Pi and `CLAUDE_CODE_SESSION_ID` under Claude Code.
  A jj commit names the session your pane registered:
  the id in the `omp-attribution-<id>.toml` file at the end of `JJ_CONFIG` (`printf '%s\n' "${JJ_CONFIG##*:}"`), which differs only after a session switch or a fork.

- Write no trailer yourself, except the `Dispatch:` line the next item allows.
  The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names;
  the git wrapper adds the first two to a `git commit`.
  A merge, revert, cherry-pick or rebase that git makes gets none.

- An unpushed commit that lacks one of those lines: describe it again in the pane's shell;
  where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the trailer line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.
  A pushed commit is never described again: answer it as `skill://legion-worker/references/conflicts-and-rewrites.md`, "A wrong pushed commit", says.

- Another session or another role, or `(no author)` or `(no committer)`, which `jj git push` refuses: send the architect this log.

- The git author and committer name no role; do not compare them.

- Never set `user.name`, `user.email`, `templates.commit_trailers`, `templates.revert_description`, `templates.duplicate_description` or any `trailer.*` key in any jj or git scope:
  the repository's jj config is shared by every workspace of the clone, and a `templates.commit_trailers` there replaces the pane's trailers.

- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it.
