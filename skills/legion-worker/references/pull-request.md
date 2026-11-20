# The pull request and the merge gate

Read it before you open or comment on a pull request, write a GitHub issue, or change `gh`'s config, its aliases or its token variables (`skill://legion-worker`, "Pull request and merge gate").

- The implementer opens the pull request with the line `Dispatch: <KEY>` in its body. Later phases push to that branch and edit that pull request.
- The implementer records a result for every `before_merge` check with no `ci_check` before its phase completes, and writes the `Checks` lines when the pull request opens. Whatever blocks a check is fixed, not skipped (`skill://legion-worker/references/pr-body.md#when-no-surface-reaches-the-changed-path`).
- The merge gate, in order: the tester's evidence green → the reviewer's approval of the head → the architect's acceptance → retro, which commits nothing → the merger's merge of the head it read → the daemon's reading of that merge, which closes the issue → the production check after the close, the tester's unless the project's `production_check` is `off`, whose every `pending` look but the last names its next look with `wake_at`. Only the merger runs `gh pr merge`; nobody turns auto-merge on.
- End every pull-request comment you post with `<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->`.
- A Dispatch comment or message never carries the `<!-- legion: … -->` footer. Dispatch records the session that wrote it, and only a pull-request comment or review needs the footer.
- Never write a GitHub issue, by `gh issue` or a non-GET `gh api` on `/issues` (post or edit a PR conversation comment with `gh pr comment`): Legion's issues live on Dispatch.
- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset: a set variable outranks the gh directory the daemon refreshes.
