# The operations of your tree's work

Read this on a `provider-errors` or `update-stalled` wake, and before you act on a claim of your tree, re-run a CI run of your tree, enqueue a pull request of your tree that no merger merges, or make a GitHub write your `gh` cannot make. Each is yours: ask no other session, the controller included, to do it.

## A claim of your tree

`legion({ op, issue, role })`, with `op` one of `stop`, `retry`, `suspend`, `resume_claim`, `compact` and `fresh_session`, reaches the claims of the issues you own: a root's architect owns every issue of its tree, a sub-architect its own issue and those under it. Your own claim is refused (`OWN_CLAIM`), and so is a claim of an issue you do not own (`ISSUE_NOT_OWNED`) or of another tree (`ISSUE_OUTSIDE_TREE`). What each does: `skill://legion-controller/references/operations.md#a-claims-state`.

- Try `compact` before `fresh_session` for a session a provider keeps refusing.
- `provider-errors`: six turns of the claim in a row ended in a provider error, and the daemon keeps the task. The controller compacts or refreshes the claim; ask it for nothing. For a rate limit or an overload, do nothing. For a refusal of the content, ask the worker which step its turns end on and narrow that step, or move the issue back with `request_backward_move`.
- `update-stalled`: an update round past its bound waits on an architect's suspension of a claim you own. Read the claim in `read_record`, then `resume_claim` it once its cause is gone, or keep it held. Open no ask, and send the controller nothing.
- Your suspension is recorded as `architect`, and your `resume_claim` lifts it. Only the operator lifts the operator's suspension (`SUSPENDED_BY_OPERATOR`), and only the controller or the operator lifts the controller's (`SUSPENDED_BY_CONTROLLER`): ask the operator with a `dispatch ask` on the root, and the controller with `envoy_publish`.
- The daemon's own suspension of a park, a linger, a close or a carry takes yours over and refuses your `resume_claim` (`SUSPENDED_BY_DAEMON`). If you still want the claim held when the issue comes back, suspend it again.
- While the project is paused, each claim operation that would start a turn (`resume_claim`, `retry`, `compact` and `fresh_session`) is refused `409 PROJECT_PAUSED`; `stop` and `suspend` still run. Only the project's resume lifts the pause. Wait for it, and ask nobody to resume.

## The token the secrets broker lends

The implement App token the secrets broker lends (`skill://agent-secrets`) takes three GitHub writes of your tree's work: a re-run of your tree's CI run, the enqueue, arming, dequeue or disarming of a pull request no merger merges, and a comment on a pull request your `gh` does not reach. Any other write your `gh` cannot make is a finding (`skill://legion-worker/references/findings.md`). Take it narrowed to the one repository, and call `curl`, never `gh`:

```sh
agent-secrets LEGION_IMPLEMENT_APP --reason '<KEY>: <the write, and why>' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- --data-binary "$1" <url>' sh '<the JSON body>'
```

- Keep the call in single quotes and give `curl` the header on standard input, so the token is in no command's arguments. Never print the token.
- Name in `--reason` the write, why you make it, and your issue's key.
- Exit 77 with no `agent-secrets:` grant line is a denial, and exit 22 is GitHub's refusal, whose status curl prints. A denial and a 4xx are final: ask no other session to make the write. After a 5xx, make it once more at a later wake. Name a denial, a 4xx or a second failure in your summary.

## A pull request no merger merges

An issue's merger merges the pull request its issue records, and no other. Enqueue a pull request of your tree only when its head branch is your tree's (`<root key, lower case>/…`, or `legion/<KEY>` of an issue of your tree), no issue's `read_record` names it as `pullRequest`, and a review approves its head. The controller holds no enqueue: it writes its own request, or a person's in the person's words, to you by a mention.

1. `gh pr view <n> -R <owner>/<repo> --json id,title,state,headRefName,headRefOid,autoMergeRequest`. `headRefOid` is `<head sha>`. A `state` other than `OPEN` ends the procedure. A non-null `autoMergeRequest` was armed at a head you did not read: turn it off (step 7), then begin again.
2. `gh api --paginate repos/<owner>/<repo>/pulls/<n>/reviews`: the head is approved when an `APPROVED` review's Legion footer names `<head sha>`, or, with no footer, its `commit_id` does (`skill://legion-worker/references/merge-gate.md`). Else enqueue nothing.
3. Read the merge holds as `skill://legion-architect/references/acceptance.md#a-merge-hold` says. A line that holds `<owner>/<repo>`, or a failed read, means enqueue nothing: name it in your summary, and read the document again on each wake before you enqueue.
4. Read the base's rules (`gh api --paginate repos/<owner>/<repo>/rules/branches/<base>`) and `squash_merge_commit_message` (`gh api repos/<owner>/<repo>`). On a `merge_queue` base whose setting is `COMMIT_MESSAGES`, enqueue nothing and open a `dispatch ask` on your root: only a person changes the setting.
5. On a merge-queue base, post `enqueuePullRequest(input:{pullRequestId, expectedHeadOid: <head sha>})` to `https://api.github.com/graphql` with the token. On any other base, post `enablePullRequestAutoMerge` at that head with `mergeMethod: SQUASH`, `commitHeadline: "<title> (#<n>)"` and an empty `commitBody`. `curl` exits 0 on a refusal too: an `errors` array is GitHub's refusal. A refusal for a repository setting or an App permission is a `dispatch ask` on your root.
6. Read the pull request again (`headRefOid`, `mergeQueueEntry`, `autoMergeRequest`), and again on each later wake while it is open.
7. When the head moved after step 5, no review approved it: post `dequeuePullRequest` or `disablePullRequestAutoMerge` at once, then begin again at step 1.

## A re-run of your tree's CI run

A run on the head of a pull request of your tree is yours to re-run. Use the rules and reads of `skill://legion-controller/references/operations.md#a-re-run-of-a-ci-run`, with the token as above, for your own tree's runs alone. The re-run call's exit 22 is GitHub's refusal, whose status curl prints. A 4xx, such as a run that cannot be re-run, is final. After a 5xx, re-run once more at a later wake, and never otherwise. Name a denial, a 4xx or a second failure in your summary.
