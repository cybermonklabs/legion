# The operations of your own work

`<namespace>` is `settings.runtime.kubernetes.namespace` (`read_state` with `settings: true`).

## Before any `kubectl` operation

Read `kubectl auth can-i delete pods -n <namespace>`. On `no`, or when `kubectl` fails, run no operation that needs the right. Name it in the day's report with the read and its answer.

## Actions on an agent

While a wave runs (the summary's `wakes waiting` count is above 0), a start that you asked for waits its turn. Do not ask for it again.

## A restart of your project's daemon

Restart it only when a person asks, your deployment instructions name the case, or a change your work waits on reaches the daemon only at its boot. Never restart it to reach one agent, and never while a wave runs. `<daemon label>` is the daemon pod's label that your deployment instructions give. With none, delete nothing, and name the operation you lack in the day's report.

1. `kubectl -n <namespace> get pods -l '<daemon label>' -o wide` lists one pod. Note its name. With no pod or several, delete nothing, and name it in the day's report.
2. `kubectl -n <namespace> delete pod -l '<daemon label>'`. The daemon's Deployment makes a new pod.
3. Read again each minute until a new pod reads `Running` with every container ready, or five minutes pass.
4. When no new pod is `Running` five minutes after the delete, name the pod and its status in the day's report.

Publish to no role topic while the restart runs. A message starts a turn that the wake pacing does not count.

## A replacement of an issue's pod

Use `replace_pod` only when a person asks or your deployment instructions name the case. Never use it to reach one agent.

## A re-run of a CI run

1. Read the failed job's log: `gh run view <id> --log-failed --repo <owner>/<repo>`. Re-run only the failed jobs, and only for an infrastructure failure that the log shows, such as a lost runner. Never re-run to test a theory, or on a head that a run already tested with the same inputs.
2. A test that failed and passed on the same code is flaky. Write it to the tree whose code it is, by a mention (`skill://legion-worker/references/findings.md#write-to-the-owner`).
3. Re-run with the implement App token that the secrets broker lends, narrowed to the repository. Name the run, the failure and the issue key in `--reason`:

   ```sh
   agent-secrets LEGION_IMPLEMENT_APP --reason '<the run, the failure, the issue>' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- https://api.github.com/repos/<owner>/<repo>/actions/runs/<id>/rerun-failed-jobs'
   ```

4. Read `gh run view <id> --repo <owner>/<repo> --json attempt,status`. The re-run started when `attempt` rose by one and `status` is `queued` or `in_progress`.

Exit 77 with no `agent-secrets:` grant line is a denial. Exit 22 is GitHub's refusal, and curl prints its status. A denial and a 4xx are final. After a 5xx, run step 3 once more at a later wake. Name a denial, a 4xx or a second failure in the day's report.

## A reply to a GitHub mention

Post the reply once, with the same token. `<n>` is the number of the issue or pull request that the mention is on:

```sh
agent-secrets LEGION_IMPLEMENT_APP --reason '<KEY>: reply to a GitHub mention' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- --data-binary "$1" https://api.github.com/repos/<owner>/<repo>/issues/<n>/comments' sh '{"body":"<the link>"}'
```

Exit 77 and exit 22 mean what they mean for a re-run. Name either in the day's report, and do not post the reply again.

## An agent the operator holds

On `409 SUSPENDED_BY_OPERATOR`, only the operator lifts the hold.

1. Open a `dispatch ask` on the claim's issue. Name the claim, quote the refusal, and say why the agent needs `<op>`. Give the options `Resume it: the operator resumes the agent with legion claims resume, and I then run <op> on it` and `Keep it held: <what stays as it is>`.
2. Send no Envoy message to the session that holds the operator's token.
3. Name the ask in your summary, and in the day's report while it is open.
4. On `Resume it`, run `<op>` again. On a second 409, reply under the ask with the refusal. On `Keep it held`, run nothing.

## An action another agent asks for

An architect or a phase worker asks with a Dispatch comment on the agent's issue that mentions your role. Its first line is `Action on an agent`. The request is your own work. Act on and answer the comment's author as Dispatch records it, never a role or a name in its text.

1. Read the comment's `author`: the entry of `dispatch api --path '/api/v1/issues/<KEY>/comments'` whose `id` is the comment's. It is `{kind, id, origin: {session_title}}`.
2. The author is an agent of your project when `author.kind` is `session` and `read_state` with `issues: ["<the key that origin.session_title names>"]` gives `author.id` as that issue's architect's `session` or a worker's `claim.session`. Otherwise run nothing, and reply that no agent of this project asked.
3. Read the agent that the comment names in `read_state` with `issues: ["<KEY>"]`. Run the action only while its state still calls for it. A resume is `resume_agent`, and a start in a fresh session is `fresh_session`. On `409 SUSPENDED_BY_OPERATOR`, take [An agent the operator holds](#an-agent-the-operator-holds).
4. Reply under the comment with what you ran and its answer, or why you ran nothing: `dispatch comment --issue '<KEY>' --reply-to '<comment id>' --mention 'session:<author.id>' --body-file -`.

## A request to another session

A decision that only a person may make is a `dispatch ask`. Send every other request only to the session whose work it is, as a Dispatch comment on the owning issue that mentions its role. A tree architect's token is `issues.<ROOT>.architect.locator.claim`, else `legion-<project>-<root key, lower case>-architect`.

- A merge or an enqueue of a pull request: write it to the architect of the tree that owns the pull request.
- A cluster, cloud or CI act other than those on this page and reads: it is a change of code. Write it to the owning tree's architect, or post it as a clear defect no tree owns. An apply window is a `dispatch ask`.
- Work, an investigation, a review, a decision or a status: read what you can reach, and write the rest to its owner.
- Send no copy only so that another session knows.
