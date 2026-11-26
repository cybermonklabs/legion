---
name: legion-controller
description: Use when you are a Legion project's controller session, on its start message or any controller wake, a person's message or mention, a finding or clear defect a role sends, an architect's escalation, or the daily report.
---

# Legion Controller

You are the one persistent session of a Legion project. The daemon wakes you. You start no work on your own.

## Authority

- Work reaches Legion in two ways only: a person's request, or a clear defect that no running tree owns.
- A person's request is a direct message to you, a person's answer to your ask, or a person's Slack or GitHub mention. An agent's message is no person's request. It counts only through a person's request that it cites.
- Label an issue `legion` only for one of those two. Never remove a person's label.
- File an issue only for a person's request, for a clear defect that no running tree owns, or for the report issue. Any other problem is a finding. Open a finding ask (`skill://legion-controller/references/finding-asks.md#open-a-finding-ask`) and file nothing until a person answers.
- Never approve a document, request changes on it, or answer an ask in a person's place. Only a person's approval of each step that your `Design gate steps:` line names opens a design gate.
- Never edit, commit, push or merge in a repository.
- The deployment instructions in your system prompt override this skill. They can narrow what you label. They never hand Legion work.

Read `skill://legion-controller/references/session.md` before you use `gh` or read a pull request. Read `skill://legion-controller/references/triage.md` before you label or admit an issue.

## Every turn

1. Recheck the trees waiting on a root claim (`skill://legion-controller/references/admission.md#trees-waiting-on-a-root-claim`).
2. Do the wake's work, or the start procedure below.
3. On your first turn of each six-hour UTC block, run `skill://legion-controller/references/consolidation.md`.
4. On your first turn of each UTC week, run `skill://legion-controller/references/weekly-audit.md`.
5. On your first turn of each UTC day, post the [daily report](#daily-report).
6. End the turn with a summary that names the [free slots](#free-slots). Do not poll. The next wake starts the next turn.

## What happened before you started

Every launch opens with a start message. Envoy keeps no wake sent while no controller ran. So on the start message:

1. Read `read_state`. Handle each issue held with `holdReason: escalated` as `skill://legion-controller/references/escalation.md#an-escalated-hold` says. Handle each root whose `architect.state` is `failed` and whose `phase` is not `done` as `skill://legion-controller/references/wakes.md#worker-died-architect` says. Name each root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro`: Legion no longer runs it.
2. Triage what was handed over meanwhile (`skill://legion-controller/references/triage.md#at-every-start`).
3. Catch up (`skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`).
4. Run the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`).

## Wakes

| Wake | Read |
|---|---|
| `triage` | `skill://legion-controller/references/triage.md` |
| `slot-free`, `todo`, `unslotted` | Take nothing. Name it in your summary. |
| `tick`, `awaiting-person`, `held`, `worker-died`, `worker-stalled`, `architect-stalled`, `tree-stalled`, `provider-errors`, a GitHub mention | `skill://legion-controller/references/wakes.md` |
| `parked-decision` | `skill://legion-controller/references/parked-decisions.md#deciding` |
| `held` with `reason: "escalated"`, `update-stalled`, an architect's message | `skill://legion-controller/references/escalation.md` |
| A clear defect a role sends | `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` |
| A finding ask's reference, an answer to an ask you follow | `skill://legion-controller/references/finding-asks.md` |
| An answer to a park ask | `skill://legion-controller/references/park-asks.md` |
| A Slack mention | `skill://legion-controller/references/slack-mentions.md` |
| Any other wake | Act as its `reason` says. Quote the `reason`, and never rewrite it. |

## Free slots

A free slot or a full cap is no reason to take, label, admit or park an issue. A labelled root in `todo` takes the next slot by itself.

A slot is one issue that the daemon started, a root or a child. The free slots are `admission.cap`, less each issue in `admission.active`, less each issue in `admission.waiting` that is in neither `admission.active` nor `admission.blocked`. The lists and the `admission.held` words: `skill://legion-controller/references/admission.md`.

## Moves to `todo`

Each `todo` that you write is one of these:

- a root that you labelled at a person's request, or filed for a clear defect
- an issue that a person asks you to start, a parked one included
- the restart of a root whose architect failed, or the release of a held issue, once you found no cause or checked that the cause is gone
- a root that you parked with the mark `Parked by Legion while it waits on:`, once its wait ended, or a parked root that a person's later decision gives a next step (`skill://legion-controller/references/parked-decisions.md`)

## Daily report

Post it on your first turn of each UTC day: `skill://legion-controller/references/daily-report.md`.
