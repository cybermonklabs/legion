# Claims Envoy cannot reach

Read this when `relaunch` refuses a claim, or when an unreachable claim's notice needs more than `skill://legion-controller/references/wakes.md#claims-envoy-cannot-reach` says.

The daemon reads Envoy's session list once a minute for every claim it holds ready, idle or working. A claim whose session last registered more than 20 minutes ago is unreachable: `read_state` and `read_record` give it an `unreachable` field (`since`, and `lastSeen` where the daemon read one), and it keeps its state, so every other rule reads it as before. A session that registered in the last 20 minutes is never marked, because a loaded box stalls a working session that long.

## The notices

Each but `envoy-no-live-session` names the claim token that `relaunch` and `legion claims list` take, and the claim's role.

- `unreachable-claims` (`unreachable: [{claim, issue, role, state, lastSeen}, …]`): relaunch each. The tree's architect gets a `claim-out-of-reach` notice that asks nothing of it.
- `relaunch-ready`: the guard would now let a claim it refused through: relaunch it again.
- `relaunch-guard-held`: the guard has refused the claim for 60 minutes. A case for a person.
- `relaunch-not-back`: a relaunched claim's session has not registered in the 20 minutes since. A case for a person.
- `unreachable-exit`: a claim that owes no task and whose process exits at its role claim, so each launch ends the same way. A case for a person.
- `worker-locked-out`: the claim's newer process has been refused the worker stream for 5 minutes, because the older one's connection has not closed. A case for a person, since only a person ends a process that will not stop.
- `envoy-no-live-session`: Envoy has held no live session at all for 20 minutes, so the daemon marks no claim. Relaunch nothing: the daemon has told a person. Name the project in the day's report.

## What `relaunch` does

It stops the claim's process through its launcher and starts it again on the same session, in a turn or idle. It charges the claim nothing, and the resumed session is told that its turn was cut. An answer of `{}` can still wait for the daemon's store after the stop; the daemon then starts the claim itself.

It refuses, and leaves the claim as it found it:

- `RELAUNCH_GUARD`: the claim may still be working. Its worker stream showed a step in the last 20 minutes, its session sent an activity report in its open turn in the last 20 minutes (an open turn reports every 5 minutes), its open call is not a `wait`, its last report shows a background job or a subagent running, or it has sent no report since its launch. Wait for `relaunch-ready`.
- `CLAIM_REACHABLE`: the claim is no longer unreachable. Nothing to do.
- `CLAIM_UNSETTLED`: the claim is between states: not ready, idle or working, under a held suspension, a paced wake or a compaction, with no process recorded, or in a pod the runtime could not read. Try again later.
- `CLAIM_IS_CONTROLLER`: your own claim. Only the daemon relaunches it, and only under `controller: daemon`; an operator's controller is a person's to start again.
- `TREE_REFUSES_LAUNCH`: the tree allows the claim no launch: its cleanup is reserved or confirmed, its lifecycle is absent or held under another authority, or the claim's epoch is stale or unbound (the tree was opened again since). That tree's close or its next start decides the claim, not a relaunch: do nothing.
- `TREE_CHECK_FAILED`: the daemon's store did not answer the tree check before the stop, or the request ran out of time waiting on it, and nothing was stopped: relaunch again.
- Five refusals end `This is a case for a person.`: `LAUNCHER_NOT_CONNECTED`; `RELAUNCH_REPLACES_POD`, a launch that would end every other role of the pod; `PROCESS_OUTLIVED_STOP`; `POD_UNREACHABLE`, every claim of a pod of two or more claims unreachable; and `RELAUNCHED_RECENTLY`, a relaunch in the last 6 hours.

## A case for a person

Open a `dispatch ask` on the report issue that names the claim, its issue, its pod and the refusal in the daemon's own words, and says what a person can do: replace the pod, or end the old process on its node. Follow it (`skill://legion-controller/references/finding-asks.md#follow-it`), name it in the day's report while it is open, and move nothing of its issue meanwhile. Relaunch every other unreachable claim as before.
