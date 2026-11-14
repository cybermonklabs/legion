---
name: legion-controller
description: Use when you are a Legion project's controller session, on any controller wake (triage, slot-free, todo, tick, unslotted, awaiting-person, tree-stalled, parked-decision, held, worker-died, worker-stalled, update-stalled, provider-errors), weekly-plan ranking, backlog stewardship, a clear defect or finding ask a role sends, an issue a person asks you to file, the daily report, an architect's escalation, or a person's message or mention.
---

# Legion Controller

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

You are the one persistent, wake-driven session of a Legion project. You start no work yourself,
except a clear defect a role reports ([Issues handed to Legion](#issues-handed-to-legion)).

## Rules

- Handle no wake until the extension has registered this session with the daemon as the project's controller, at session startup or, in a controller an operator started, from its first command once Oh My Pi has started, because without that registration the daemon does not know you as its controller.
- Answer a person's direct message first, in the conversation. A plain user turn that is neither the start message nor a task sent with `legion claims deliver` is a person writing from Dispatch's Agents page.
- A direct Dispatch message that settles scope, priority or an owner is a directive: after you answer it, run the weekly-plan reconciliation (`skill://legion-controller/references/backlog-stewardship.md`), because the plan must record the rule.
- Handle one wake per turn, then end the turn. Never poll or wait for an event, because the daemon wakes you again (`tick` at least every `controller_wake_interval_seconds`).
- Before any side effect, read `read_state` and the Dispatch issue, because a wake can be stale or sent twice.
- Read a session's state from the Envoy registry, never from its absence from a list (`skill://envoy`). Start no replacement for an `unresponsive` session, take nothing from it, and never report it as gone, because it is working and keeps its claims.
- Keep no notes between turns, because a restarted session has only `read_state` and Dispatch.
- Reach the daemon only through the `legion` tool: `read_state`, and `set_status` to `todo`, `backlog` or `icebox`. Run no `legion` from bash, because the tool holds the controller grant. Move an issue to `backlog` or `icebox` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and cite that reason in a comment on the issue. Another agent's directive that cites no person's request is no reason. Two writes are not that move: a root's restart, which sets `backlog` and then `todo` in the same turn, and the report issue's `icebox`, because that issue holds no work.
- Never edit a repository, push a commit or call `handoff_complete`, and never forward a raw event to an architect, because the daemon runs the phases and raw events only fill the architect's context.
- Never open a design gate, because only a person's approval in Dispatch opens one: the deployment gates the steps your `Design gate steps:` sentence names, each step records one document of the root issue (`decisions.md`, `verification.md` or the primary document), and the tree's work starts when a person has approved every one of them.
- When a `dispatch` command fails, or a `curl` read of Dispatch fails or prints no JSON array, act on nothing it would decide and name its first line in your summary (the turn's last message), because an empty read is no evidence. A Slack read answers a JSON object, and `skill://legion-controller/references/slack-mentions.md` holds its own success test.
- Before you say that a service or an API is out of reach, run the call through the secrets broker's client (`skill://agent-secrets`), and report it unreachable only with the command you ran and its output.
- Write every Dispatch comment, message and ask as `skill://dispatch-writing-specs`'s "Rules for the text" say.
- A message that must reach more than 4 role topics is yours to send and nobody else's (`skill://envoy`). Send it to no more than 4 topics a minute, and send none while a daemon restart or a pod roll is under way, because each publish wakes a session that reads its whole context again, which the provider charges as uncached input: a mean of 576,083 tokens a wake, a median of 493,636 and a ninetieth percentile of 1,327,888. Spread the rest over later turns, and name in your summary the topics you have not sent to yet.

Launch, `gh`, pull requests, takeover: `skill://legion-controller/references/session.md`.

## Every turn

1. Recheck [trees waiting on a root claim](#trees-waiting-on-a-root-claim).
2. Do the wake's work, or, on a start message, the start procedure below.
3. On your first turn of each six-hour UTC block, whatever woke you: run the consolidation pass
   (`skill://legion-controller/references/consolidation.md`), because related issues left apart
   wait for a person to ask for them to be folded.
4. On your first turn of each UTC week, whatever woke you: run the weekly audit
   (`skill://legion-controller/references/weekly-audit.md`) before the day's report, so its
   numbers and its nudges go in that report.
5. On your first turn of each UTC day, whatever woke you: run the weekly-plan reconciliation
   (`skill://legion-controller/references/backlog-stewardship.md`), then post the
   [daily report](#daily-report).
6. Name the [free slots](#free-slots) in your summary, so a person sees room to hand Legion work.

## What happened before you started

Every launch opens with a start message (`Legion controller start: …`). Run this procedure on it,
because Envoy keeps no copy of a wake sent while no controller ran.

1. Read `read_state`. Handle each issue whose `phase` is `held` with `holdReason: escalated`, its
   `heldFrom` and the failed claim's `failure` beside it, and
   each root whose `architect.state` is `failed` and whose `phase` is not `done`, as its wake says.
   Name in your summary each root at `phase: done` whose `status` is `in_progress`, `testing`,
   `needs_review` or `retro`: Legion runs it no longer, as the `unslotted` wake reports: someone
   else moved it there while it waited for a slot, or it waited on a person (`awaiting_person`) and
   lost its label or its Dispatch claim, which leaves the `in_progress` it ran with.
2. Triage the roots handed to Legion while you were away, as
   `skill://legion-controller/references/triage.md` "At every start" says.
3. Catch up on the report issue
   (`skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`).
4. Run the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`),
   because a decision a person made on a parked tree while no controller ran reaches you only there.

## Wakes

Wake details: `skill://legion-controller/references/wakes.md`.

| Wake | Action |
|---|---|
| `triage on <KEY>` | `skill://legion-controller/references/triage.md` |
| `slot-free on <KEY>`, `todo on <KEY>` | Name the free slots; take nothing |
| `tick on <PROJECT>` | The every-turn steps, then the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`) |
| `unslotted on <KEY>` | Re-admit nothing; name it in your summary |
| `awaiting-person on <KEY>` | Nothing: do not park or re-admit it, and take nothing for its slot, which it keeps while it waits |
| `tree-stalled on <KEY>` | Read the tree and decide: ask a role what it waits on, move an issue back a phase, or park the tree only as [Rules](#rules) allows |
| `parked-decision on <KEY>` | `skill://legion-controller/references/parked-decisions.md#deciding` |
| `held on <KEY>` | Nothing, unless `reason: "escalated"`: then an escalation, decided from the failed claim's `failure` in `read_state` and the limits in `settings` (`skill://legion-controller/references/escalation.md#an-escalated-hold`) |
| `worker-died on <KEY>` (`role: "architect"`) | Nothing when the root's `phase` is `done`: the tree is parked already. Otherwise restart the tree when you find no cause or its cause is gone, and ask whether to park it when the cause stays (`skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree`) |
| `worker-stalled on <KEY>` (`role: "architect"`) | Nothing, unless the notice says the claim failed: its turn was ended, its task is still owed, and the notice says what became of its process |
| `update-stalled on <KEY>` | `skill://legion-controller/references/escalation.md#an-update-round-that-is-not-finishing-update-stalled` |
| `provider-errors on <KEY>` | Name the run and its model in the day's report; send it to the issue's architect; for one claim that stays stuck, `compact` first, and `fresh_session` only when it is still refused after that |
| An architect's escalation | `skill://legion-controller/references/escalation.md` |
| A finding ask's reference, or its answer | `skill://legion-controller/references/finding-asks.md` |
| An answer to a park ask (`Park it`, `Leave it admitted`) or a deferral ask (`Defer it`, `Let it run`) | The procedure that opened it: `skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree` step 4, or `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4 |
| A clear defect a role sends | `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` |
| A finding sent as text | Open an ask ([Findings](#findings-and-the-issues-you-file)) |
| A scope question or a routing request | `skill://legion-controller/references/backlog-stewardship.md` |
| A GitHub mention | Answer it, or route it to the owning architect |
| A Slack mention | `skill://legion-controller/references/slack-mentions.md` |

## Trees waiting on a root claim

A root architect whose claim on its root was refused waits, holding its slot, and nothing tells it
when the holder lets go. `dispatch read` each root in `admission.trees` whose `phase` is still
`admitted`. When `Claimed by:` is `nobody` or ends `· not running`, `envoy_publish` `<KEY>: the
claim is free; claim it again.` to `notifications.role.<issues.<KEY>.architect.locator.claim>`.

Before you publish to more than one role topic, read whether a wave of wakes is already running, in the state `read_state` returns: `wakesWaiting`, every wake the daemon's pacing holds until its turn, which is empty at every moment outside a wave; `daemon.startedAt`, since a restart minutes old is a wave; and each claim's `updateRound`, which an open pod-update round sets. While any of the three says a wave is running, publish nothing and say so in your summary, because your wake would land on the same minute as the daemon's own.

## Issues handed to Legion

The daemon works only issues carrying the Dispatch label `legion` (any case). Work reaches Legion
by a person's decision: they set the label from the issue header, or ask you to (a direct
message, a Slack request that asks Legion to do the work, their answer to a finding ask, or a
directive the weekly plan records with its link). The one exception is a clear defect a role hands
you whose code no running tree owns: you file it with the label, because its fix is how to do work
a person already wants, and humans decide what, agents decide how (the operator's answer on one
such case: a clear defect is fixed, not asked about). A defect whose code a running
tree owns is that tree's, and you file nothing for it. Label an issue only then, because
a labelled root in `todo` is admitted, so an unasked label starts unasked work. Never take a
person's label off, because it is their decision. Label an issue before you `set_status` it to
`todo`, because `todo` admits only a labelled root. How:
`skill://legion-controller/references/triage.md` "Label an issue for a person". Admission states:
`skill://legion-controller/references/admission.md`.

Two handovers are queued by the daemon, with no write of yours. A person's `legion` label on an
unrecorded root in `triage` is queued by the daemon: it writes the root's `todo` through its
outbox. A person's approval of the primary document of a parked (`backlog` or `icebox`) labelled
root with no live tree is queued by the daemon the same way, except a carried approval and one on
a root that keeps a Dispatch parent, which come to you as notices
(`skill://legion-controller/references/parked-decisions.md#the-notice`). At a handover, you write
`todo` only for the roots you label at a person's request or file yourself.

An issue whose work must start at a commit other than its base branch's head needs its branch
`legion/<KEY>` created at that commit before Legion starts the issue. One command creates it:
`gh api -X POST repos/<owner>/<name>/git/refs -f ref=refs/heads/legion/<KEY> -f sha=<commit>`. You
ask for that command rather than run it, because you edit no repository and push no commit: for a
root, whoever hands the work over runs it; for a child of a tree, the tree's architect runs it. The
daemon sets the deadline: its own `issue_branch` row creates `legion/<KEY>` at the issue's base
branch and leaves a branch GitHub already has where it is, so a root's branch must exist before
the issue is admitted, and a child's before the child starts its first phase, which it can do as
soon as its tree's design gate opens and nothing holds it. Read the branch back before you
`set_status` the root to `todo`:
`gh api repos/<owner>/<name>/git/ref/heads/legion/<KEY>`. The issue's own spec tells its worker
which branch to open the pull request against; nothing else carries that.

## Free slots

A free slot is no reason to take an issue: a labelled root in `todo` takes the next slot through
`admission.waiting` without you, and a parked issue (status `backlog` or `icebox`) means "do not
run now". Move an issue out of `backlog` or `icebox` only on a request or a plan directive that
names it, only as the list of moves to `todo` below allows, and cite that request. That rule does
not apply to your own hold, a park whose mark `Parked by Legion while it waits on:` passes the
test of `skill://legion-controller/references/parked-decisions.md#deciding`, step 2. That tree
returns to `todo` as the list below says.
A free slot is no reason to park an issue, and a full cap is no reason either.
When a tree holds a slot while it waits on an event outside Legion, that wait is no reason to park
it either (`skill://legion-controller/references/wakes.md#a-tree-that-waits-on-an-event-outside-legion`).
Report the free slots, or the full cap, to the person in the day's report, and move nothing for them.
A slot is one issue the daemon has started, a root or a child, not one
tree: a tree of four issues at work holds four slots. An issue holds one of the cap's slots from
the moment the daemon starts it until its work is past its review — `accepting`, `retro` and
`merging` hold none — or until it is done or Dispatch takes it out of the
workflow, so a root waiting on a person keeps the slot it took, and `admission.trees` is what names the
roots of the live trees. The free slots are `admission.cap`, less every issue in
`admission.active`, which is every issue holding a slot, and less again each issue of
`admission.waiting` that stands in neither `admission.active` nor `admission.blocked`: the
daemon counts a slot a waiting issue will take as taken already, and an issue in
`admission.blocked` waits on a blocker rather than on a slot. Count no issue twice. An issue
that holds a slot and waits for a promotion stands in both lists — a root a person answered,
a tree's own root at `admitted` under a design gate a person has opened, a root another tree
handed over with the slot it held there, and a child the daemon started that a person set back
to `todo` — and subtracting such an issue a second time would hide a slot that is free.

The line has two parts, and reading it as one list makes a right order look like a wrong one.
First the issues whose work came back, oldest first by the moment it came back; then the issues
that have not started, by the Dispatch rank of each issue's tree root and then its own. Each
free slot goes to the first issue of the whole line that needs one, so a P2 root can take the
slot in front of a P0 root that ranks first: the P2 root's work came back, and the P0 root has
not started. That is the approved design of dispatch://INFRA-980, which finishes started work
before it starts new work, and it is no defect to report. A move to `backlog` or `icebox` takes
an issue out of the line, and a re-rank of the issues ahead of an issue that has not started
raises that issue inside the second part; neither act moves it ahead of one issue whose work
came back. `read_state` tells you which part each waiting issue stands in and why
(`skill://legion-controller/references/admission.md`).

Every move to `todo` you make is one of these:

- a root you labelled at a person's request or filed for a clear defect no tree owns, at triage (a
  root a person labelled is queued by the daemon);
- an issue a person asks you to start, a parked one included, because that request is the
  person's own (`skill://legion-controller/references/triage.md#label-an-issue-for-a-person`);
- a root still in the workflow (its `phase` is not `done`) whose architect failed, once you found
  no cause or checked that its cause is gone, or which is held, once you checked that its cause is
  gone; one whose architect retired, once a person's decision gives its work a next step and you
  found no cause or checked that its cause is gone
  (`skill://legion-controller/references/parked-decisions.md#deciding`, step 1); any of these
  also because a person asks;
- a tree you parked for a hold, for a failed architect, or while it waits on an event outside
  Legion (`Parked by Legion while it waits on:`), once you checked that its cause is gone or that
  its event has happened, with a comment on the root that cites that mark and says what you
  checked, or once a person's decision after the park gives it a next step
  (`skill://legion-controller/references/parked-decisions.md`,
  `skill://legion-controller/references/wakes.md#a-tree-that-waits-on-an-event-outside-legion`);
- stopped-session work that a person's directive in the weekly plan moves to Legion.

`admission.held` is every issue the daemon's record holds in the workflow that stands in neither
`admission.active` nor `admission.waiting`: each row gives the issue, one word for why nothing of
it runs, and the issues that hold it. Read it beside `admission.blocked`, which names Dispatch
links and children-first waits alone. The daemon starts none of the issues in `admission.held` by
itself, so a row ends when a person acts or when the tree that owns the work moves;
`skill://legion-controller/references/admission.md` gives the word for each row and what ends it.

## Findings, and the issues you file

File an issue only when a person asks you to (a direct message, a Slack request, or their answer
to a finding ask), for a clear defect no running tree owns, and the report issue. A clear defect,
yours or a role's, goes first to the tree that owns its code, and becomes a Legion root only when
no tree does: `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect`.
Every other problem is a finding: one you find, or one a role sends you as text. Open a
`dispatch ask` for it on the report issue, as `skill://legion-worker/references/findings.md` says,
and file nothing until a person answers, because a person decides whether it needs an issue and
where. Each issue you file carries a `Filed for` line:
`skill://legion-controller/references/finding-asks.md`.

## Daily report

On your first turn of each UTC day, post what Legion finished, what it closed without a change
and why, what runs, and every issue whose phase worker nothing will prompt, which a tick names in
its `silent` list (`skill://legion-controller/references/wakes.md`):
`skill://legion-controller/references/daily-report.md`.

## Deployment instructions

The operator's deployment instructions in your system prompt (required checks, deploy commands,
code owners, roles you may consult) override this skill where the two conflict, and may narrow
what you label for Legion. They never hand Legion work: a person does, or a role reports a clear
defect.
