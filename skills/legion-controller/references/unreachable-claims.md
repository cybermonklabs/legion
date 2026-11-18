# Claims Envoy cannot reach

The daemon reads Envoy's session list once a minute, unresponsive sessions included, for every claim it holds as ready, idle or working.
A claim whose session's last registration is more than 20 minutes old is unreachable: `read_state` and `read_record` give the claim an `unreachable` field (`since`, and `lastSeen`, that last registration, where the daemon read one), and the claim keeps the state it had, so the turn hold, admission and every other rule read it as before.
A claim whose session registered in the last 20 minutes is never marked, because a loaded box stalls a session for up to 1,148 s and that session is working: nothing replaces a session unresponsive for 20 minutes or less.

Seven notices come to you about these claims and no others.
Each of them but `envoy-no-live-session`, which is about no one claim, names the claim token that the `legion` tool's `relaunch` and `legion claims list` take, with that claim's role.

- `unreachable-claims` (`{kind: "unreachable-claims", unreachable: [{claim, issue, role, state, lastSeen}, …]}`): every claim that has become unreachable since the last such notice.
  Relaunch each one.
  The tree's architect is told of each claim of its tree other than its own in a `claim-unreachable` notice, which asks nothing of it, because the relaunch is yours.

- `relaunch-ready` (`{kind: "relaunch-ready", claim, role, reason}`): a claim whose relaunch the guard refused and would now let through.
  Relaunch it again.

- `relaunch-guard-held` (`{kind: "relaunch-guard-held", claim, role, reason}`): a claim the guard has refused for 60 minutes.
  It is a case for a person.

- `relaunch-not-back` (`{kind: "relaunch-not-back", claim, role, reason}`): a claim a relaunch reached whose session has not registered with Envoy in the 20 minutes since, so it is still unreachable.
  It is a case for a person.

- `unreachable-exit` (`{kind: "unreachable-exit", claim, role, reason}`): an unreachable claim that owes no task and whose process exits at its Envoy role claim, so each launch of it ends the same way and a relaunch of yours would add nothing.
  It is a case for a person.

- `worker-locked-out` (`{kind: "worker-locked-out", claim, role, reason}`): the claim's newer process has been refused the worker stream for 5 minutes, because the older generation's connection has not closed.
  Only a person can end a process that will not stop, so it is a case for a person.

- `envoy-no-live-session` (`{kind: "envoy-no-live-session", reason}`): Envoy's whole list, Legion's sessions and every other, has held no live session for 20 minutes, so the daemon marks no claim at all.
  Nothing here is yours to relaunch: the daemon has written the line that tells a person, and you name the project in the day's report.

`relaunch` stops the claim's process through its launcher and starts it again on the same session, which keeps its context, its workspace and the task it owes.
It is no fresh session, it charges the claim nothing, and the resumed session is told that its turn was cut, so it reads what that turn did before it acts again.
It reaches a claim in a turn as well as an idle one.

The daemon refuses it `RELAUNCH_GUARD`, and leaves the claim exactly as it found it, unless its worker stream has shown no step in the last 20 minutes, it is in no turn or its session has sent no activity report in that turn in the last 20 minutes, its open call, if it has one, is a `wait`, and its last activity report since its launch shows no background job and no subagent running.
A claim that has sent no activity report since its launch is refused `RELAUNCH_GUARD` too.
A session in a turn sends its activity report every 5 minutes whatever it says, so a turn whose process still runs is refused, a `wait` on a named `bash` service among them, and a process that has frozen is let through 20 minutes after its last report.
That guard is what keeps a relaunch off a turn that is still doing work it cannot tell you about.
It also refuses a claim the daemon no longer marks unreachable (`CLAIM_REACHABLE`), and your own claim (`CLAIM_IS_CONTROLLER`), which only the daemon relaunches, and only where it runs the controller (`controller: daemon`).
It refuses `CLAIM_UNSETTLED` a claim that is between states:

- a claim that is not ready, idle or working;
- a claim that a suspension is held over;
- a claim whose next wake waits its turn in the daemon's wake pacing;
- a claim whose session is being compacted;
- a claim that records no process;
- a claim whose pod the runtime could not read, or that the runtime refuses for a reason with no code of its own.

Five refusals are a case for a person rather than something to try again: the role's launcher is not connected (`LAUNCHER_NOT_CONNECTED`); the launch would replace the pod, which would end every other role of that pod (`RELAUNCH_REPLACES_POD`); the claim's process outlived the stop, which the daemon reports after the stop and before it starts anything else (`PROCESS_OUTLIVED_STOP`); every claim of a pod that holds two or more claims is unreachable, which no relaunch into that pod recovers (`POD_UNREACHABLE`); and the claim was relaunched in the last 6 hours, whatever it did in between, because a cause that stays in the pod comes back and a person can have the pod replaced (`RELAUNCHED_RECENTLY`).

For each case for a person, open a `dispatch ask` on the report issue that names the claim, its issue, its pod and the refusal in the daemon's own words, and says what a person can do: replace the pod, or end the old process on its node.
Follow the ask so that its answer reaches you (`skill://legion-controller/references/finding-asks.md#follow-it`), name the claim and the ask in the day's report while it is open, and move nothing of the issue meanwhile, because the claim keeps its state and its work.
Such a case stops nothing else: relaunch every other unreachable claim, the other claims of that same pod included, and relaunch again a claim the guard refused once the `relaunch-ready` notice says the guard would let it through.

You cannot relaunch yourself.
Where the daemon runs the controller (`controller: daemon`), it reads your claim by the same rule and relaunches it on this same session, at most once in 6 hours, and after that a person is told.
A controller an operator started is read by no such rule, and only a person starts it again (`skill://legion-controller/references/session.md`).
