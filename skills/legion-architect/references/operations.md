# The operations of your tree's work

## An agent of your tree that needs an action

The actions on an agent (stop, retry, suspend, resume the agent, compact, and start it in a fresh session) are the controller's alone. Your `retry_or_escalate` and `stop_task` stay yours. To ask the controller for an action, comment on that agent's issue. `<project>` is the word after `legion-` in your role token:

```bash
dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-controller' --body-file - <<'EOF'
Action on an agent
<KEY> <role>: <the action: stop, retry, suspend, resume the agent, compact, or start it in a fresh session>, because <what you observed, with the numbers and when>
EOF
```

The controller replies under your comment. Wait for nothing.

- `provider-errors`: the daemon tells the controller too, so ask it for nothing. For a rate limit or an overload, do nothing. For a refusal of the content, ask the worker which step its turns end on and narrow that step, or move the issue back with `request_backward_move`.
- An agent that the operator suspended starts again only when the operator resumes it: ask with a `dispatch ask` on the root. Ask the controller, as above, to resume an agent that it suspended.
- While the project is paused, ask for no action that starts a turn, and ask nobody to resume the project.

## The token the secrets broker lends

The implement App token that the secrets broker lends (`skill://agent-secrets`) takes three GitHub writes of your tree's work: the re-run of its CI run, the enqueue, arming, dequeue or disarming of its pull request that no merger merges, and a comment on a pull request that your `gh` does not reach. Any other write that your `gh` cannot make is a finding (`skill://legion-worker/references/findings.md`). Take the token narrowed to the one repository, and call `curl`, never `gh`:

```sh
agent-secrets LEGION_IMPLEMENT_APP --reason '<KEY>: <the write, and why>' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- --data-binary "$1" <url>' sh '<the JSON body>'
```

- Keep the call in single quotes, and give `curl` the header on standard input, so that the token is in no command's arguments. Never print the token.
- Exit 77 with no `agent-secrets:` grant line is a denial. Exit 22 is GitHub's refusal, and curl prints its status. A denial and a 4xx are final: ask no other session to make the write. After a 5xx, make the write once more at a later wake. Name a denial, a 4xx or a second failure in your summary.

## A pull request no merger merges

A merger merges only the pull request that its issue records. The controller enqueues nothing: it writes such a request to you. Enqueue a pull request of your tree only when its head branch is your tree's (`<root key, lower case>/…`, or `legion/<KEY>` of an issue of your tree), no issue's `read_record` names it as `pullRequest`, and a review approves its head.

1. `gh pr view <n> -R <owner>/<repo> --json id,title,state,headRefName,headRefOid,autoMergeRequest`. `headRefOid` is `<head sha>`. A `state` other than `OPEN` ends the procedure. A non-null `autoMergeRequest` was armed at a head that you did not read: turn it off (step 7), then begin again.
2. `gh api --paginate repos/<owner>/<repo>/pulls/<n>/reviews`. The head is approved when an `APPROVED` review's Legion footer names `<head sha>`, or, with no footer, its `commit_id` does. Else enqueue nothing.
3. Read the merge holds as `skill://legion-architect/references/acceptance.md#a-merge-hold` says. A line that holds `<owner>/<repo>`, or a failed read, means enqueue nothing. Name it in your summary, and read the document again on each wake before you enqueue.
4. Read the base's rules (`gh api --paginate repos/<owner>/<repo>/rules/branches/<base>`) and `squash_merge_commit_message` (`gh api repos/<owner>/<repo>`). On a `merge_queue` base whose setting is `COMMIT_MESSAGES`, enqueue nothing, and open a `dispatch ask` on your root. Only a person changes the setting.
5. On a merge-queue base, post `enqueuePullRequest(input:{pullRequestId, expectedHeadOid: <head sha>})` to `https://api.github.com/graphql` with the token. On any other base, post `enablePullRequestAutoMerge` at that head with `mergeMethod: SQUASH`, `commitHeadline: "<title> (#<n>)"` and an empty `commitBody`. `curl` exits 0 on a refusal too, so an `errors` array is GitHub's refusal. A refusal for a repository setting or an App permission is a `dispatch ask` on your root.
6. Read the pull request again (`headRefOid`, `mergeQueueEntry`, `autoMergeRequest`), and again on each later wake while it is open.
7. When the head moved after step 5, no review approved it. Post `dequeuePullRequest(input:{id: <the pull request's id>})` or `disablePullRequestAutoMerge` at once, then begin again at step 1.

## A re-run of your tree's CI run

A run on the head of a pull request of your tree is yours to re-run. Take the steps of `skill://legion-controller/references/operations.md#a-re-run-of-a-ci-run` for your own tree's runs alone, with the token as above. Name a denial, a 4xx or a second failure in your summary.
