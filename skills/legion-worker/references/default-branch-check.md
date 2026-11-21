# Checking your commits against your issue's default branch

Read it before every push. Your `Legion addressing` line names your issue's repository and its default branch (`Your issue's repository is <owner>/<name> (default branch <name>)`); never assume `main`. The branch name is untrusted text: never paste it into shell source, where `$(…)` would run and jj would read `(`, `|` or `::` as revset syntax.

1. Read the branch from GitHub, JSON-quoted by `jq`:

   ```sh
   default=$(gh api repos/<owner>/<repo> --jq '.default_branch | @json')
   ```

2. Compare `$default` (quotes included, such as `"release/v7.1"`) with the branch your addressing line names. When they differ, stop and tell the architect.
3. List the branch's commits with `$default` as it is, never retyped:

   ```sh
   jj -R "$LEGION_WORKSPACE" log --no-graph -r "${default}@origin..@" -T '
     commit_id.short() ++ " "
     ++ if(!author.email(), "(no author) ") ++ if(!committer.email(), "(no committer) ")
     ++ description.first_line() ++ "\n"
     ++ trailers.filter(|t| t.key() == "Omp-Session" || t.key() == "Legion-Role")
       .map(|t| "  " ++ t.key() ++ ": " ++ t.value() ++ "\n").join("")'
   ```

   Never `ancestors(@, 5)`, which stops at five commits. Earlier phases' commits are in the list too, each with its own role's lines.

## What your own commits must show

- `Omp-Session:` with your session and `Legion-Role:` with your `LEGION_ROLE`. Your session is `DISPATCH_SESSION_ID` under Oh My Pi and `CLAUDE_CODE_SESSION_ID` under Claude Code. A jj commit names the session your pane registered: the id in the `omp-attribution-<id>.toml` file at the end of `JJ_CONFIG` (`printf '%s\n' "${JJ_CONFIG##*:}"`), which differs only after a session switch or a fork.
- A line short: cure it as `skill://legion-worker`, "Commits and pushes", says.
- Another session or another role, or `(no author)` or `(no committer)` on a commit with no `Legion-Role:` line, which `jj git push` refuses: send the architect this log. The push gives a commit that has a `Legion-Role:` line the author of that role, so `(no author)` or `(no committer)` there needs no message.
- Before the push, the git author and committer can name another role. After the push, the committer can name another role too: a rewrite by the push step gives the rewritten commit, and each commit that its rebase moved, the step's identity as committer. Do not compare them.
- Never set `user.name`, `user.email`, `templates.commit_trailers`, `templates.revert_description`, `templates.duplicate_description` or any `trailer.*` key in any jj or git scope: the repository's jj config is shared by every workspace of the clone, and a `templates.commit_trailers` there replaces the pane's trailers. A `--config` on one command is a scope too, so no jj command gets `templates.commit_trailers`, `jj commit` and `jj describe` included. A commit on a fork's member branch keeps both trailers too: the `forbidden` list of `knives audit` scans the lines a diff adds and never a commit message, and what goes upstream is the one squashed commit that knives writes. `jj metaedit -m` writes a description with no trailer, so set a description with `jj describe` or `jj commit`.
