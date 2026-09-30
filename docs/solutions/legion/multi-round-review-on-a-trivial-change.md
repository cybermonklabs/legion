---
title: "Running the Legion phase loop through multi-round review on a trivial one-file change"
category: legion
tags:
  - legion-workflow
  - phase-worker
  - review-rounds
  - jj
  - handoff-proof
date: 2026-09-30
status: active
module: legion-phase-workers
related_issues:
  - "S391616981-1"
  - "sjawhar/legion-smoke#459"
---

# Running the Legion phase loop through multi-round review on a trivial one-file change

Issue S391616981-1 added exactly one file, `smoke/S391616981-1.md`, then took three
`CHANGES_REQUESTED` review rounds that each appended one line. The whole plan → implement →
test → review → retro loop ran on a four-line file whose CI is a `sleep 20 && exit 0`. The
learnings below are about running the loop cleanly, not about the (trivial) code.

## Each return to implementing is its own phase; a push does not finish it

Under the Go daemon a review round comes back to the implementer as a fresh `Phase: implementing`
task. The round is not done when the corrective commit is pushed — it ends only when the `legion`
tool's `handoff_complete` is called. Do the append, push, refresh the implement handoff and PR
body, *then* `handoff_complete`. The tester's next round starts only after that call.

## `HANDOFF_NO_RUN` when a human message starts the turn before the daemon's task

Twice, the turn was started by a human Dispatch message targeted at the session, before the daemon
had delivered the matching phase task. Calling `handoff_complete` in that turn returned
`409 HANDOFF_NO_RUN: this completion belongs to no task`. This is not an error to retry inside the
same turn: end the turn, let the daemon deliver the `Continue … Phase: implementing` task, then
re-call `handoff_complete` **without redoing any work** — the commit and handoff are already
durable. Quote the daemon's answer verbatim when reporting, since it distinguishes "work lost"
from "task not yet delivered".

## jj rewrites the working copy in place — pin every proof to the current head

Because jj amends the working copy, every append produced a new head SHA. One four-line file
carried at least four distinct SHAs across phases (implement `eb1570d8`, test `9d21725c`, review
approve `f8360c29`, plus the code head of each earlier round). Consequences a worker must respect:

- A `proof` array records the head SHA it was observed at; the next phase re-reads at *its* current
  head rather than trusting the older SHA. The tester here correctly re-drove the surface at its own
  head.
- A CI run id is only meaningful paired with the head it ran on. Cite the run at the head your
  handoff names, not a run from an earlier phase's head.

## Append-only means the diff stays `+N/−0`

Three review rounds, three appended lines, and lines 1–N were never reflowed. If a "append one
line" round shows any `−N` on an untouched line, you edited instead of appended. `write` of the
full file is fine as long as the earlier lines are reproduced byte-for-byte; verify with
`jj diff` that only additions appear.

## Presence proof is worthless without the paired absent-on-base negative control

Every phase proved the marker present at the PR head **and** reproduced the same negative control —
`jj file show -r main@origin smoke/S391616981-1.md` → `Error: No such path`. Keeping the surface
and the negative control identical across phases is what makes the tester's re-run a genuine
independent check rather than a paraphrase.

## The implementer's reply never resolves a review thread — the reviewer does

The one bot thread (`sjawhar-agent`, "is this file change needed?") was answered by the implementer
with `Fixed in <commit>: … the change is needed per spec`. That reply closed nothing: GitHub only
lets the pull request author's App resolve its threads, and the implementer App opened the PR, so
the review App cannot resolve on the implementer's word. `legion threads resolve` left it open
(`newest reply by legion-implementer is not … acceptance`) until the reviewer posted `Accepted:` in
the next round; only then did the resolve command close it (`the Legion reviewer's acceptance of a
bot's thread`). Never bulk-resolve; the resolving acceptance must be the newest comment.

## Retro sits above the approved head and touches only `docs/solutions/`

Under the Go daemon, no role pushes the `.legion/` deletion until a later stage, so retro runs with
`.legion/` still present. Retro writes **no** `.legion` file: its only outputs are the
`docs/solutions/` document(s) and one Dispatch message on the issue, committed above the reviewer's
approved head. A commit that changes only `docs/solutions/` does not void the approval, and the
tree goes retro → merger, never back to tester or reviewer.
