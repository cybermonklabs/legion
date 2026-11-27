# The pull request body

The implementer writes the body when it opens the pull request, with the line `Dispatch: <KEY>`, and keeps it true after each push. Later roles edit their own lines of the live body (read it with `gh api repos/{owner}/{repo}/pulls/{number} --jq .body`, write it back with `--method PATCH -F body=@<file>`).

It holds, in this order: what someone can now do or what stops going wrong; why, ending with the Dispatch key; the change in a few bullets; how it is proven; and a `Checks` block with one line for each check of the Verification document, each the line `dispatch result` printed, or the `Pull request line` that `dispatch results` prints for a check that carries `ci_check`.

Where the task has no reviewer step, the body also holds `Threads`, one line for each bot thread: its node id, fixed in a commit or declined with the reason, and the head you read it at. It also holds `Not proven / risk`: one sentence for each risk that stands, and the name of each check whose run has not concluded.
