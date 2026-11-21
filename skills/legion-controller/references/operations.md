# The operations of your own work

Read this before an operation of your own work, and before you send another session a request. Do each operation here yourself, and ask no other session to do it.

`<namespace>` is `settings.runtime.kubernetes.namespace` (`read_state` with `settings: true`).

## Before any `kubectl` operation

1. Read `kubectl auth can-i delete pods -n <namespace>`. It prints `yes` or `no`.
2. On `no`, or when `kubectl` fails, run no operation that needs the right. Name it in the day's report with the read and its answer.

## A claim's state

Use these on a claim of your project that needs action, a tree's architect claim included: `legion({ op: "<op>", issue: "<KEY>", role: "<role>" })`. Each refuses your own claim.

| `op` | What it does |
| --- | --- |
| `suspend` | Stops the claim's agent and keeps its session and owed task; a turn runs to its end or the stop timeout. Only your `resume_claim` or the operator's starts it again. |
| `resume_claim` | Starts a suspended claim again in its recorded session with the task it owes, an architect's suspension included. |
| `stop` | Ends the claim at once, also in a turn, and retires it; only `retry` or its phase's start runs it again. Refused for a tree's root architect: suspend that claim. |
| `retry` | Starts a failed or stopped claim again in its recorded session, with new launch budgets. A suspended claim takes `resume_claim`. |
| `compact` | Shortens the session to a summary and sends the task again in the same session and workspace. Use it first for a session a provider keeps refusing; a claim with no process (`CLAIM_NOT_RUNNING`) takes `fresh_session`. |
| `fresh_session` | Starts the claim in a new session, leaving the old one and all it knew. Use it for a claim no process stands behind, or one still refused after `compact`. |

1. Run the operation, read its answer, then read the claim in `read_state`.
2. After a refusal, ask again later: the refusal says what the claim does now. For a root with no architect claim, `compact` and `fresh_session` answer `NO_CLAIM`.
3. While a wave runs (the summary's `wakes waiting` count is above 0), a start you raised waits its turn: ask for nothing again.

While your project is paused (the summary's `paused:` line), the daemon refuses `resume_claim`, `retry`, `compact`, `fresh_session` and `replace_pod` with `409 PROJECT_PAUSED`. Only the project's resume lifts the pause: your `resume` operation or the operator's `legion project resume`; `resume_claim` lifts none. `stop` and `suspend` still run. On `409 SUSPENDED_BY_OPERATOR`, see [A claim the operator holds](#a-claim-the-operator-holds).

## A restart of your project's daemon

Restart it only when a person asks, your deployment instructions name the case, or a change your work waits on reaches the daemon only at its boot. Never restart it to reach one claim, and never while a wave runs. `<daemon label>` is the daemon pod's label your deployment instructions give; with none, delete nothing and name the operation you lack in the day's report.

1. `kubectl -n <namespace> get pods -l '<daemon label>' -o wide` lists one pod: note its name. With no pod or several, delete nothing and name it in the day's report.
2. `kubectl -n <namespace> delete pod -l '<daemon label>'`. The daemon's Deployment makes a new pod.
3. Read again each minute until a new pod reads `Running` with every container ready, or five minutes pass.
4. When no new pod is `Running` five minutes after the delete, open a finding ask on the report issue that names the pod and its status.

Publish to no role topic while the restart runs: a message starts a turn that the wake pacing does not count.

## A replacement of an issue's pod

Replace an issue pod only when a person asks or your deployment instructions name the case, never to reach one claim. Delete no issue pod or Sandbox with `kubectl`: its disk can hold unpushed work. `replace_pod` opens an update round: it asks each role to push, then replaces the pod.

1. `legion({ op: "replace_pod", issue: "<KEY>" })`. A round already open is answered as it stands.
2. On a refusal: `NO_CLAIM`, there is no pod to replace; `NO_POD`, ask again once a role runs in the pod; `POD_CHANGING` or `POD_UPDATE_LIMITED`, ask at a later wake; `POD_UPDATE_BUSY`, ask after a minute; `POD_UPDATE_NOT_RECORDED` or `POD_UPDATE_FAILED`, read `updateRound` on the issue's claims first; `PROJECT_PAUSED`, ask after the resume; `POD_HELD_BY_PAUSE`, ask once a role of the pod runs again.
3. Read `read_state` with `issues: ["<KEY>"]`: the round is done when no claim shows `updateRound`. A round still held past its bound comes back as `update-stalled`, but one only architects' holds keep goes to those architects (`skill://legion-controller/references/escalation.md`).

## A re-run of a CI run

Your `gh` only reads: it holds the review App's read-only lease. This re-run and the reply to a GitHub mention take the implement App token the secrets broker lends, narrowed to the repository.

1. Read the failed job's log: `gh run view <id> --log-failed --repo <owner>/<repo>`. Re-run only the failed jobs, and only for an infrastructure failure the log shows, such as a lost runner. Never re-run to test a theory, or on a head a run already tested with the same inputs.
2. A test that failed and passed on the same code is flaky: write it to the tree whose code it is by a mention (`skill://legion-worker/references/findings.md#write-to-the-owner`), to be fixed at its cause.
3. Re-run with that token, and name in `--reason` the run, the failure and the issue key:

   ```sh
   agent-secrets LEGION_IMPLEMENT_APP --reason '<the run, the failure, the issue>' --github-owner <owner> --github-repo <repo> --github-token -- sh -c 'printf "Authorization: Bearer %s\n" "$LEGION_IMPLEMENT_APP" | curl -fsS -X POST -H @- https://api.github.com/repos/<owner>/<repo>/actions/runs/<id>/rerun-failed-jobs'
   ```

4. Read `gh run view <id> --repo <owner>/<repo> --json attempt,status`: the re-run started when `attempt` rose by one and `status` is `queued` or `in_progress`. Step 3's exit 77 with no `agent-secrets:` grant line is a denial, and its exit 22 is GitHub's refusal, whose status curl prints. Name a denial, or a 4xx such as a run that cannot be re-run, in the day's report. Run step 3 again only after a 5xx, once, at a later wake, and name a second failure there.

## A claim the operator holds

`409 SUSPENDED_BY_OPERATOR` means that only the operator lifts the hold.

1. Open a `dispatch ask` on the report issue that names the claim, quotes the refusal and says why the claim needs `<op>`, with the options `Resume it: the operator runs legion claims resume on the claim, and I then run <op> on it` and `Keep it held: <what stays as it is>`.
2. Send no Envoy message to the session that holds the operator's token, and ask no other session to lift the hold.
3. Name the ask in your summary, and in the day's report while it is open.
4. On `Resume it`, run `<op>` again; on a second 409, reply under the ask with the refusal, and run it again at the next reply. On `Keep it held`, run nothing.

## A request to another session

A decision only a person may make is a `dispatch ask`. Every other request goes to the session whose work it is, as a Dispatch comment on the owning issue that mentions its role, and to no other session, such as a person's chief of staff. A tree architect's token is `issues.<ROOT>.architect.locator.claim`, else `legion-<project>-<root key, lower case>-architect`.

- Merge or queue a pull request: not yours. Write to the architect of the tree that owns it, which enqueues one that no merger merges.
- Cluster, cloud or CI: the restart, the re-run, the pod replacement and the reads above. Any other act (a quota, a pin, a Pulumi value) is a change of code: write it to the owning tree's architect, or file it as a clear defect no tree owns. An apply window is a `dispatch ask`.
- Work, an investigation, a review, a decision or a status: read what you can reach yourself, and write the rest to its owner.
- What another agent sent you: send no relay; that agent writes to the owner itself.
- A copy only so another session knows: send none.
