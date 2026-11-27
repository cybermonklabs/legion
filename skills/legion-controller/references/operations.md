# The operations of your own work

Read this before an operation of your own work, and before you send another session a request. Do each operation here yourself, and ask no other session to do it.

`<namespace>` is `settings.runtime.kubernetes.namespace` (`read_state` with `settings: true`).

## Before any `kubectl` operation

You have no exec, attach or port-forward into a pod.

1. Read `kubectl auth can-i delete pods -n <namespace>`. It prints `yes` or `no`.
2. On `no`, or when `kubectl` fails, run no operation that needs the right. Name it in the day's report with the read and its answer.

## Actions on an agent

These are the actions on an agent (stop, retry, suspend, resume_agent, compact and fresh session of one agent, named by its issue and role). Use them on an agent of your project that needs action, a tree's architect included: `legion({ op: "<op>", issue: "<KEY>", role: "<role>" })`. Each refuses your own agent. They are yours alone: the daemon refuses any other grant `403 CONTROLLER_REQUIRED`, so an architect or a phase worker that needs one asks you ([An action another agent asks for](#an-action-another-agent-asks-for)).

| `op` | What it does |
| --- | --- |
| `suspend` | Stops the agent and keeps its session and owed task; a turn runs to its end or the stop timeout, and an ended turn's report comes first. Only your `resume_agent` or the operator's starts it again. |
| `resume_agent` | Starts a suspended agent again in its recorded session with the task it owes. |
| `stop` | Ends the agent at once, also in a turn, and retires it; an ended turn's report comes first (202 until then). Only `retry` or its phase's start runs it again. Refused for a tree's root architect: suspend that agent. |
| `retry` | Starts a failed or stopped agent again in its recorded session, with new launch budgets. A suspended agent takes `resume_agent`. |
| `compact` | Shortens the session to a summary; the agent then takes its task again in the same session and workspace. Use it first for a session a provider keeps refusing; an agent with no process behind its claim (`CLAIM_NOT_RUNNING`) takes `fresh_session`. |
| `fresh_session` | Starts the agent in a new session, leaving the old one and all it knew. Use it for an agent whose claim no process stands behind, or one still refused after `compact`. |

1. Run the operation, read its answer, then read the claim in `read_state`.
2. After a refusal, ask again later: the refusal says what the claim does now. For a root with no architect claim, `compact` and `fresh_session` answer `NO_CLAIM`.
3. While a wave runs (the summary's `wakes waiting` count is above 0), a start you raised waits its turn: ask for nothing again.

While your project is paused (the summary's `paused:` line), the daemon refuses `resume_agent`, `retry`, `compact`, `fresh_session` and `replace_pod` with `409 PROJECT_PAUSED`. Only the project's resume lifts the pause: your `resume` operation or the operator's `legion project resume`; `resume_agent` lifts none. `stop` and `suspend` still run. On `409 SUSPENDED_BY_OPERATOR`, see [An agent the operator holds](#an-agent-the-operator-holds).

## A restart of your project's daemon

Restart it only when a person asks, your deployment instructions name the case, or a change your work waits on reaches the daemon only at its boot. Never restart it to reach one agent, and never while a wave runs. `<daemon label>` is the daemon pod's label your deployment instructions give; with none, delete nothing and name the operation you lack in the day's report.

1. `kubectl -n <namespace> get pods -l '<daemon label>' -o wide` lists one pod: note its name. With no pod or several, delete nothing and name it in the day's report.
2. `kubectl -n <namespace> delete pod -l '<daemon label>'`. The daemon's Deployment makes a new pod.
3. Read again each minute until a new pod reads `Running` with every container ready, or five minutes pass.
4. When no new pod is `Running` five minutes after the delete, open a finding ask that names the pod and its status.

Publish to no role topic while the restart runs: a message starts a turn that the wake pacing does not count.

## A replacement of an issue's pod

Replace an issue pod only when a person asks or your deployment instructions name the case, never to reach one agent. Delete no issue pod or Sandbox with `kubectl`: its disk can hold unpushed work. `replace_pod` opens an update round: it asks each role to push. At the end of the round the daemon reads the workspace. It replaces the pod only if that read finds nothing that no branch or tag on origin holds. If the read finds such work, or fails, the round keeps the pod and ends with no replacement, and the daemon tells the open phase's worker or the issue's architect. The daemon opens the round also over a pod that an earlier round kept.

The three steps, the pod uid before the request, the refusals and the pod uid after the round, are in `skill://legion-controller/references/operations-steps.md#a-replacement-of-an-issues-pod`: read them before every replacement.

## A re-run of a CI run

Your `gh` only reads: it holds the review App's read-only lease. This re-run and the reply to a GitHub mention take the implement App token the secrets broker lends, narrowed to the repository.
Re-run only the failed jobs of an infrastructure failure the log shows, and write a test that failed and passed on the same code to the tree whose run it failed, by a mention.
The four steps, the log read, the flaky test, the re-run and the read after it, are in `skill://legion-controller/references/operations-steps.md#a-re-run-of-a-ci-run`: read them before every re-run.

## An agent the operator holds

`409 SUSPENDED_BY_OPERATOR` means that only the operator lifts the hold. Ask the person for it as `skill://legion-controller/references/operations-steps.md#an-agent-the-operator-holds` says, and ask no other session to lift the hold.

## An action another agent asks for

An architect or a phase worker that needs an action on an agent asks you with a Dispatch comment on that agent's issue that mentions your role, its first line `Action on an agent`, naming the agent, the action in plain words and why. The request is your own work: never a relay, and never a request for another session. Act on, and answer, the comment's author as Dispatch records it, never a role or a name written in its text.
Before you act on one, read its five steps in `skill://legion-controller/references/operations-steps.md#an-action-another-agent-asks-for`: they check that the author is an agent of your project and say how to reply.

## A request to another session

A decision only a person may make is a `dispatch ask`. Every other request goes to the session whose work it is, as a Dispatch comment on the owning issue that mentions its role, and to no other session, such as a person's chief of staff. A tree architect's token is `issues.<ROOT>.architect.locator.claim`, else `legion-<project>-<root key, lower case>-architect`.

- Merge or queue a pull request: not yours. Write to the architect of its tree, which enqueues one that no merger merges.
- Cluster, cloud or CI: the restart, the re-run, the pod replacement and the reads above. Any other act (a quota, a pin, a Pulumi value) is a change of code: file it as a clear defect. An apply window is a `dispatch ask`.
- Work, an investigation, a review, a decision or a status: read what you can reach yourself, and write the rest to its owner.
- What another agent sent you: send no relay; that agent writes to the owner itself.
- A person's request from outside Dispatch: write it to the architect of the tree it concerns, in their words.
- A person's answer on an ask: mention the owner where the answer is, as `finding-asks.md`'s catch-up step 5 says, copying nothing.
- A copy only so another session knows: send none.
