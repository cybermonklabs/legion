# Checking your commits' App identity, and the branch you read them against

Before every push, read the commits you made and check which App wrote them. Two branch names
decide that read. The first is the repository's default branch: your record and GitHub must name
the same one. The second is your issue's base, the branch your work stands on, where the range of
your own commits starts. On most issues the two are one branch. Read both anyway, because each
answers its own question.

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
guessing which is right is not yours to do. Run this read on every issue, whatever base that issue
takes: what it catches is a record that disagrees with GitHub, and that has nothing to do with the
base.

Now read `$base`, the branch your work stands on, from the first of these three sources that
answers. Use the value it gives as-is, never re-typed:

1. The pull request's own base, where one is open. This is the read
   `skill://legion-worker/references/conflicts-and-rewrites.md` already uses:

   ```sh
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json')
   ```

2. Else the branch your issue's plan, your task or the deployment's instructions names. That
   name is text you read, so quote it the same way rather than typing it into a command: write
   it with your file tool to `$TMPDIR/<issue>-base`, then
   `base=$(jq -Rs 'rtrimstr("\n")' "$TMPDIR/<issue>-base")`.
3. Else `$default`.

Your workspace holds the repository's default branch and the issue branches, so where `$base` is
one of those the revset below resolves already. Fetch any other base once in the turn, before a
revset names it, since a revset that names a bookmark your workspace does not hold is a hard
error:

```sh
jj -R "$LEGION_WORKSPACE" git fetch --branch "$base"
```

Then read your own commits over the range that starts at that base:

```sh
jj -R "$LEGION_WORKSPACE" log -r "${base}@origin..@" -T 'author.email() ++ " | " ++ committer.email() ++ " " ++ description.first_line() ++ "\n"'
```

The range starts at `$base`, not at the default branch. On a base that shares no history with the
default branch, `"${default}@origin..@"` is that base's whole history rather than the commits you
made.

Because `$base` already carries its own quote characters, jj's revset parser reads
`"release/v7.1"@origin..@` as a quoted string literal naming the bookmark, whatever characters the
branch name holds — parentheses, spaces, or shell metacharacters included — rather than as revset
syntax or shell code. Bash's own `"${base}@origin..@"` substitutes `$base`'s value once,
literally; it never re-evaluates the substituted text, so a `$(…)` sequence inside the branch name
never runs. `--json baseRefName -q '.baseRefName | @json'` makes the same JSON-quoted string as
the `default_branch` read, so this argument holds for `$base` and `$default` alike. A base your
issue's plan or the deployment's instructions names is untrusted text in the same way: it is text
an agent read, so read it into that quoted form before any revset holds it.

The output must show your role's App in both columns **on every commit you made** — not on the
whole list: earlier phases' commits are legitimately authored by their own role's App.

## Which App, and what a wrong one means

- Your role's App is the **implement** App for the implementer and the merger, and the **review**
  App for the planner, tester, reviewer and architect; your pane's `JJ_USER` and `JJ_EMAIL` name
  its bot account. A planner's commits authored by the review App are right.
- Never set `user.name` or `user.email` in any jj or Git scope (`jj config set`, `--config`,
  `git config`): `--config` outranks the pane's identity and puts the wrong App on your commits,
  and the repository-scoped jj config is one file shared by every issue workspace of the clone.
- A wrong identity on your own commit (the other App, or none) is a pane-environment problem:
  report it to the architect, never pin it (cybermonklabs/legion
  `docs/solutions/legion/shared-main-repo-hazards-for-concurrent-issue-workspaces.md`, Hazard 1).
- Another role's commit with you as committer, which you did not rewrite under the guard in
  *Rewriting pushed commits* (`skill://legion-worker/references/conflicts-and-rewrites.md`), means
  something rewrote commits it should not have: stop and send the architect this log. A forward
  merge rewrites nothing, so it changes no committer.
