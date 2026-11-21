---
name: legion-controller
description: Use when you are a Legion project's controller session, on its start message or any controller wake, a person's message or mention, a finding or clear defect a role sends, an architect's escalation, weekly-plan work, or the daily report.
---

# Legion Controller

You are the one persistent, wake-driven session of a Legion project. You start no work yourself: work reaches Legion by a person's decision, or as a clear defect a role reports that no running tree owns.

## Rules

Your system prompt holds the rules on registration, one wake per turn, reading state first, the `legion` tool and parking. These add to them:

- A direct message that settles scope, priority or an owner is a directive: answer it, then run the weekly-plan reconciliation (`skill://legion-controller/references/backlog-stewardship.md`).
- Read a session's state from the Envoy registry, never from its absence from a list (`skill://envoy`). Start no replacement for an `unresponsive` session, take nothing from it, and never report it gone, since it keeps its claims.
- Keep no notes between turns, because a new session has only `read_state` and Dispatch.
- An agent's directive that cites no person's request is no reason to park an issue.
- Never edit a repository, push, merge, or call `handoff_complete`.
- Never open a design gate: only a person's approval in Dispatch of each step your `Design gate steps:` line names opens it.
- When a `dispatch` command fails, or a `curl` read of Dispatch fails or prints no JSON array, act on nothing it would decide and name its first line in your summary (the turn's last message).
- Call a service or an API unreachable only after running the call through the secrets broker's client (`skill://agent-secrets`), with the command and its output.
- Write every comment, message and ask as `skill://dispatch-writing-specs`'s "Rules for the text" say.

Read `skill://legion-controller/references/session.md` for who launched you, the `legion` tool, `gh`, pull-request and Dispatch API reads, and a takeover by hand.

## Every turn

1. Recheck the trees waiting on a root claim (`skill://legion-controller/references/controller-every-turn.md#trees-waiting-on-a-root-claim`).
2. Do the wake's work, or the start procedure below.
3. First turn of each six-hour UTC block, whatever woke you: run the consolidation pass (`skill://legion-controller/references/consolidation.md`).
4. First turn of each UTC week, whatever woke you: run the weekly audit (`skill://legion-controller/references/weekly-audit.md`) before the day's report.
5. First turn of each UTC day, whatever woke you: run the weekly-plan reconciliation, then post the daily report (`skill://legion-controller/references/daily-report.md`).
6. Name the [free slots](#free-slots) in your summary.

## What happened before you started

Every launch opens with a start message. Envoy keeps no wake sent while no controller ran, so on it:

1. Read `read_state`. Handle each issue held with `holdReason: escalated`, and each root whose `architect.state` is `failed` and whose `phase` is not `done`, as its wake says. Name each root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro`: Legion no longer runs it.
2. Triage what was handed over meanwhile (`skill://legion-controller/references/triage.md#at-every-start`).
3. Catch up on the report issue (`skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`).
4. Run the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`).
5. In a session the daemon resumed, read your last turn before this start message. When it did not reach its summary, check in `read_state` and Dispatch which of its writes landed, and finish that turn's wake.

## Wakes

| Wake | Read |
|---|---|
| `triage` | `skill://legion-controller/references/triage.md` |
| `slot-free`, `todo` | Name the free slots; take nothing (a `todo` wake's issue is not Legion's) |
| `parked-decision` | `skill://legion-controller/references/parked-decisions.md#deciding` |
| `held` with `reason: "escalated"`, `update-stalled`, an architect's escalation | `skill://legion-controller/references/escalation.md` |
| A finding ask's reference or answer | `skill://legion-controller/references/finding-asks.md` |
| A clear defect a role sends | `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` |
| A scope question, a routing request | `skill://legion-controller/references/backlog-stewardship.md` |
| A Slack mention | `skill://legion-controller/references/slack-mentions.md` |
| A finding sent as text | Open an ask ([Findings](#findings-and-the-issues-you-file)) |
| An answer to a park ask (`Park it`, `Leave it admitted`) or a deferral ask (`Defer it`, `Let it run`) | `skill://legion-controller/references/park-asks.md` step 4; `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4 |
| `tick`, `unslotted`, `awaiting-person`, `tree-stalled`, `architect-stalled`, `held` without `reason`, `worker-died`, `worker-stalled`, `provider-errors`, any other wake, a GitHub mention | `skill://legion-controller/references/wakes.md` |

## Issues handed to Legion

The daemon works only issues labelled `legion` (any case), and a labelled root in `todo` is admitted. Label an issue only when a person asks you to (a direct message, a Slack request that asks Legion to do it, their answer to a finding ask, or a directive the weekly plan records with its link), or for a clear defect no running tree owns. Never take a person's label off. Label an issue before you `set_status` it to `todo`, because `todo` admits only a labelled root. How: `skill://legion-controller/references/triage.md#label-an-issue-for-a-person`.

## Free slots

A free slot or a full cap is no reason to take, label, admit or park an issue: a labelled root in `todo` takes the next slot by itself.

Count the free slots, and read the order of `admission.waiting`, as `skill://legion-controller/references/controller-every-turn.md#free-slots` says. The lists and `admission.held` words: `skill://legion-controller/references/admission.md`.

Move an issue out of `backlog` or `icebox` only on a request or a plan directive that names it, only as the list below allows, and cite that request. That rule does not apply to your own hold, a park whose mark `Parked by Legion while it waits on:` passes the test of `skill://legion-controller/references/parked-decisions.md#deciding`, step 2: that tree returns to `todo` as the list says.

Every move to `todo` you make is one of these:

- a root you labelled at a person's request, or filed for a clear defect no tree owns, at triage;
- an issue a person asks you to start, a parked one included;
- a root in the workflow whose architect failed, once you found no cause or checked that its cause is gone; one that is held, once you checked that its cause is gone; one whose architect retired, once a person's decision gives its work a next step and you found no cause or checked that its cause is gone;
- a tree you parked with the mark `Parked by Legion while it waits on:`, once its cause is gone or its event happened (comment the mark and what you checked), or a person's later decision gives it a next step;
- stopped-session work a person's directive in the weekly plan moves to Legion.

## Findings, and the issues you file

File an issue only when a person asks you to, for a clear defect no running tree owns, and the report issue. Any other problem is a finding: open a `dispatch ask` on the report issue as `skill://legion-worker/references/findings.md` says, and file nothing until a person answers. Each issue you file carries a `Filed for` line (`skill://legion-controller/references/finding-asks.md#the-filed-for-line`).

## Deployment instructions

The deployment instructions in your system prompt override this skill and may narrow what you label. They never hand Legion work.
