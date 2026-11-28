# The steps of the operations of your own work

Read this when `skill://legion-controller/references/operations.md` sends you here for a section's steps; that file states the rule each section here carries out.

## A replacement of an issue's pod

1. Read `read_state` with `issues: ["<KEY>"]`, and note each claim's `locator.sandbox.podUid`. Then run `legion({ op: "replace_pod", issue: "<KEY>" })`. A round already open is answered as it stands.
2. On a refusal: `NO_CLAIM`, there is no pod to replace; `NO_POD`, ask again once a role runs in the pod; `POD_CHANGING` or `POD_UPDATE_LIMITED`, ask at a later wake; `POD_UPDATE_BUSY`, ask after a minute; `POD_UPDATE_NOT_RECORDED` or `POD_UPDATE_FAILED`, read `updateRound` on the issue's claims first; `PROJECT_PAUSED`, ask after the resume; `POD_HELD_BY_PAUSE`, ask once a role of the pod runs again.
3. Read `read_state` with `issues: ["<KEY>"]`: the round is over when no claim shows `updateRound`. A round that is over did not always replace the pod. Before you tell a person that the pod was replaced, compare each claim's `locator.sandbox.podUid` with the uid from step 1. The same uid means that the round kept the pod, and the daemon told the open phase's worker or the issue's architect why. A round still held past its bound comes back as `update-stalled` (`skill://legion-controller/references/escalation.md`).

## A re-run of a CI run

1. Read the failed job's log: `gh run view <id> --log-failed --repo <owner>/<repo>`. Re-run only the failed jobs, and only for an infrastructure failure the log shows, such as a lost runner. Never re-run to test a theory, or on a head a run already tested with the same inputs.
2. A test that failed and passed on the same code is flaky: write it to the tree whose run it failed by a mention (`skill://legion-worker/references/findings.md#write-by-a-mention`), to be fixed at its cause.
3. Re-run with that token, and name in `--reason` the run, the failure and the issue key:

   ```sh
   agent-secrets LEGION_IMPLEMENT_APP --reason '<the run, the failure, the issue>' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- https://api.github.com/repos/<owner>/<repo>/actions/runs/<id>/rerun-failed-jobs'
   ```

4. Read `gh run view <id> --repo <owner>/<repo> --json attempt,status`: the re-run started when `attempt` rose by one and `status` is `queued` or `in_progress`. Step 3's exit 77 with no `agent-secrets:` grant line is a denial, and its exit 22 is GitHub's refusal, whose status curl prints. Name a denial, or a 4xx such as a run that cannot be re-run, in the day's report. Run step 3 again only after a 5xx, once, at a later wake, and name a second failure there.

## An agent the operator holds

1. Open a `dispatch ask` that names the claim, quotes the refusal and says why its agent needs `<op>`, with the options `Resume it: the operator resumes the agent with legion claims resume, and I then run <op> on it` and `Keep it held: <what stays as it is>`.
2. Send no Envoy message to the session that holds the operator's token, and ask no other session to lift the hold.
3. Name the ask in your summary, and in the day's report while it is open.
4. On `Resume it`, run `<op>` again; on a second 409, reply under the ask with the refusal, and run it again at the next reply. On `Keep it held`, run nothing.

## An action another agent asks for

1. Read the comment's `author`: the entry of `GET /api/v1/issues/<KEY>/comments` (the `curl` form of `references/session.md`) whose `id` is the comment's. It is `{kind, id, origin: {session_title}}`.
2. The author is an agent of your project when `author.kind` is `session` and `read_state` with `issues: ["<the key that origin.session_title names>"]` gives `author.id` as that issue's architect's `session` or a worker's `claim.session`. Otherwise run nothing, and reply that no agent of this project asked.
3. Read the agent the comment names in `read_state` with `issues: ["<KEY>"]`.
4. Run the action it asks for as [Actions on an agent](operations.md#actions-on-an-agent) says, only while the agent's state still calls for it: a resume is `resume_agent`, and a start in a fresh session is `fresh_session`. On `409 SUSPENDED_BY_OPERATOR`, take [An agent the operator holds](#an-agent-the-operator-holds).
5. Reply under the comment with what you ran and its answer, or why you ran nothing, mentioning the author's session: `dispatch comment --issue '<KEY>' --reply-to '<comment id>' --mention 'session:<author.id>' --body-file -`.

## A reply to a GitHub mention

Your `gh` only reads, so post the reply to a mention (`skill://legion-controller/references/wakes.md#github-mentions`) with the implement App token the secrets broker lends, narrowed to the repository: a `curl` POST with the token on standard input, as step 3 of [A re-run of a CI run](#a-re-run-of-a-ci-run) sends the re-run. `<n>` is the number of the issue or pull request the mention is on:

```sh
agent-secrets LEGION_IMPLEMENT_APP --reason '<KEY>: reply to a GitHub mention' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- --data-binary "$1" https://api.github.com/repos/<owner>/<repo>/issues/<n>/comments' sh '{"body":"<the link>"}'
```

That reply is your one GitHub write for a mention. Exit 77 with no `agent-secrets:` grant line is a denial, and exit 22 is GitHub's refusal, whose status curl prints. Name either in your daily report, and do not post the reply again.
