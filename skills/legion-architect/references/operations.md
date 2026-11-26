# The operations of your tree's work

Read this on a `provider-errors` wake, when an agent of your tree needs an action or another role writes you a clear defect, and before you re-run a CI run of your tree, enqueue a pull request of your tree that no merger merges, or make a GitHub write your `gh` cannot make. The re-run, the enqueue and the write are yours: ask no other session, the controller included, to make them. An action on an agent is the controller's alone.

## An agent of your tree that needs an action

The actions on an agent (stop, retry, suspend, resume the agent, compact, and start it in a fresh session) are the project's controller's alone: your `legion` tool refuses each in an architect session, and the daemon refuses any grant but the controller's with `403 CONTROLLER_REQUIRED`. Your `retry_or_escalate` of a held phase stays yours (the `worker-died` row of `skill://legion-architect/references/wake-routing.md`), and so does `stop_task`: a tree operation that stops a task of an issue's plan, whose step the daemon suspends, and never a way to stop a named agent.

When an agent of your tree needs an action, ask the controller with a `dispatch comment` on that agent's issue, `<KEY>`, that mentions the controller's role. `<project>` is the word after `legion-` in your role token. Name the agent by its issue and role, the action in plain words, and why, with what you observed:

```bash
dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-controller' --body-file - <<'EOF'
Action on an agent
<KEY> <role>: <the action: stop, retry, suspend, resume the agent, compact, or start it in a fresh session>, because <what you observed, with the numbers and when>
EOF
```

- Take no action on the agent yourself, and send the controller no `envoy_publish` for it: Dispatch delivers the mention and retries it.
- The controller replies under your comment with what it ran and the daemon's answer, or why it ran nothing. Wait for nothing: the rest of your tree's work goes on.
- `provider-errors`: six turns of the agent in a row ended in a provider error, and the daemon keeps the task. The daemon tells the controller too, which compacts the agent or starts it in a fresh session: ask it for nothing. For a rate limit or an overload, do nothing. For a refusal of the content, ask the worker which step its turns end on and narrow that step, or move the issue back with `request_backward_move`.
- An agent that the operator or the controller suspended starts again only when one of them resumes the agent: the operator's resume lifts either suspension, and the controller's resume of the agent only its own. Ask the operator with a `dispatch ask` on the root, and the controller with a comment as above.
- While the project is paused, the daemon refuses every action that would start a turn of an agent: a retry, a resume of the agent, a compaction and a fresh session. Ask for none of them until the project's resume, which alone lifts the pause, and ask nobody to resume the project.

## A clear defect another role writes to you

A clear defect another role writes to you comes as a Dispatch comment that mentions your role, its first line `Clear defect from <role>` (`skill://legion-architect/SKILL.md`, "Escalation judgment").
Carry it in this tree: in an open pull request, or in a child.
Reply under its evidence comment with the issue that carries it.
It changes the root spec only where it changes the Summary, a requirement, a check or the scope (`skill://legion-architect/references/design-gate.md`, "After approval").

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
