# Checking your commits against your issue's default branch

Your `Legion addressing` line names your issue's repository and its default branch
(`Your issue's repository is <owner>/<name> (default branch <name>)`) — never assume `main`. That
branch name is untrusted text: a repository administrator chooses it, and it reaches you as plain
words in a system prompt, not as a value a shell has already made safe. **Never paste it directly
into shell source** — not as a bare word, not inside double quotes you type yourself. A branch name
containing `$(…)` or backticks would run as a command the moment a shell re-parses a line built that
way; one containing jj revset syntax (parentheses, `|`, `::`) would be read as an expression instead
of a literal name.

The safe sequence reads the branch fresh from GitHub and lets `jq` do the quoting, in one command
whose only typed literal is the `<owner>/<repo>` your addressing line names (safe to interpolate: a
GitHub repository path, already validated by the daemon that minted your addressing):

```sh
default=$(gh api repos/<owner>/<repo> --jq '.default_branch | @json')
```

`$default` now holds a JSON-quoted string, e.g. `"release/v7.1"` (quotes included). Compare it
against the `default branch <name>` your addressing line named — if they disagree, stop and tell
the architect; GitHub's live default branch and the one your record carries are not the same, and
guessing which is right is not yours to do. When they agree, use `$default` as-is, never re-typed:

```sh
jj -R "$LEGION_WORKSPACE" log --no-graph -r "${default}@origin..@" -T '
  commit_id.short() ++ " "
  ++ if(!author.email(), "(no author) ") ++ if(!committer.email(), "(no committer) ")
  ++ description.first_line() ++ "\n"
  ++ trailers.filter(|t| t.key() == "Omp-Session" || t.key() == "Legion-Role")
    .map(|t| "  " ++ t.key() ++ ": " ++ t.value() ++ "\n").join("")'
```

Because `$default` already carries its own quote characters, jj's revset parser reads
`"release/v7.1"@origin..@` as a quoted string literal naming the bookmark, whatever characters the
branch name holds — parentheses, spaces, or shell metacharacters included — rather than as revset
syntax or shell code. Bash's own `"${default}@origin..@"` substitutes `$default`'s value once,
literally; it never re-evaluates the substituted text, so a `$(…)` sequence inside the branch name
never runs.

For each commit, the command prints its short commit id, `(no author)` or `(no committer)` where
that email is empty, and its first line. Under that line it prints the commit's `Omp-Session` and
`Legion-Role` trailers, one indented `key: value` line each.

Every commit you made must show `Omp-Session` with your session and `Legion-Role` with your
`LEGION_ROLE`. Check your own commits, not the whole list: earlier phases' commits are in the list
too, and each carries the lines of its own role.

## What the lines must show, and what to do when they do not

- Your session is `DISPATCH_SESSION_ID` under Oh My Pi and `CLAUDE_CODE_SESSION_ID` under Claude
  Code. A jj commit's `Omp-Session` line names the session your pane registered: the id in the
  name of the `omp-attribution-<id>.toml` file at the end of `JJ_CONFIG`
  (`printf '%s\n' "${JJ_CONFIG##*:}"` prints that file's path). That id differs from
  `DISPATCH_SESSION_ID` only after a session switch or a fork.
- A commit of yours that shows a line short is cured as `skill://legion-worker` says, in "Every
  role pushes its own commits".
- A commit of yours that shows another session or another role: send the architect this log.
- The git author and committer name no role, so do not compare them. The troubleshooting section
  on which role made a commit says why (cybermonklabs/legion
  `docs/site/src/content/docs/legion/troubleshooting.md`).
- A commit that shows `(no author)` or `(no committer)` is one that `jj git push` refuses: send
  the architect this log.
- Never set `user.name`, `user.email`, `templates.commit_trailers`,
  `templates.revert_description`, `templates.duplicate_description` or any `trailer.*` key in any
  jj or Git scope (`jj config set`, `--config`, `git config`). The repository-scoped jj config is
  one file that every workspace of the clone shares, and a `templates.commit_trailers` there
  replaces the overlay's trailers. A `trailer.*` key in Git's config changes how git writes the
  git wrapper's lines.
