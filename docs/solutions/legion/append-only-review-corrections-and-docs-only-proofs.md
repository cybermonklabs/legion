---
title: "Append-only review corrections and docs-only proof surfaces in Legion phase work"
category: legion
tags:
  - legion-workflow
  - review-threads
  - e2e-proof
  - handoffs
  - docs-only
date: 2026-09-28
status: active
module: legion-phase-worker
related_issues:
  - "S391461798-1"
  - "sjawhar/legion-smoke#437"
---

# Append-only review corrections and docs-only proof surfaces in Legion phase work

Retrospective for PR #437 (issue S391461798-1): a docs-only change that grew
`smoke/S391461798-1.md` from one line to four across three append-only review rounds. The
value is not the file — it is the repeatable correction / disposition / proof discipline the
trace exercises end to end.

## Correction cadence

- **Append in place; never open a sibling file.** Each `CHANGES_REQUESTED: add a line` was
  satisfied by appending exactly one line to the PR's own file (`1 -> 2 -> 3 -> 4` lines),
  executing the planner rule "append exactly one more line to this same file; open no new
  file; never overwrite." A correction edits the file the PR already changes.
- **One review round = one commit + push + green CI, then advance the thread.** The three
  corrective heads (`1b96b7b18771`, `78fe4d3cbf67`, `81752e47830b`) each carried their own
  `check` + `gate` runs green before the round's handoff. Do not batch multiple rounds into
  one push; a push alone does not finish a round — the `handoff_complete` call does.

## Review-thread disposition

- **Strict three-beat handshake, never bulk-resolved.** Per thread: (a) implementer replies
  `Fixed in <sha>: appended the line …` naming the exact commit; (b) reviewer replies
  `Accepted: fixed in <sha> — … and nothing else changed`; (c) `legion threads resolve`
  resolves it. Only the pull request's author App can resolve its own threads, so the
  implementer runs `legion threads resolve` — it acts on a thread only when the newest comment
  is the opener's `Accepted:`.
- **An Accepted-but-unresolved thread does not block approval.** `review.json` shows the
  round-3 thread `isResolved=false` at `verdict=approved`; resolution of the last accepted
  thread is a head-unchanging follow-up, not a gate. Do not spin waiting to self-resolve your
  final thread.
- **Once the opener replies `Accepted:`, write nothing further on that thread** — a later
  comment becomes the newest reply and re-opens the disposition.

## Heads move every phase — re-review only the product diff

Implementer proof ran at `81752e47830b`, tester at `ba6acdee`, reviewer reviewed
`67a97045baef`, final `APPROVE` at `4e4b5f902822`. Each phase commits its own
`.legion/<phase>.json`, so the head advances without the product changing. Treat `.legion/*`
and `docs/solutions/*` deltas as non-substantive (the unchanged-diff fingerprint excludes
exactly those paths); compare against your *own* last verified head, not another role's number.

## Docs-only proof surface

- **The production-like surface for a repo with no runtime is the GitHub contents API at the
  exact head**, not the local working copy:
  `legion gh -- api repos/<owner>/<repo>/contents/<path>?ref=<sha> --jq '.content' | base64 -d`.
  Prove there; a local `cat` is not the proof.
- **The negative control must exercise the same surface.** A wrong-key contents read returning
  HTTP 404 is a stronger control than a local `grep … exits 1`, because it proves absence on
  the same remote surface as the positive proof.
- **Tester independence = re-drive the implementer's exact proof**, observing the identical
  output at the tester's own head, rather than citing the implementer's number.
- **Leave the production AC honestly pending pre-merge.** AC3 (file on `main` at the merge
  commit) stays `Production: pending — merge not yet landed` with the planned command recorded
  and a merge-is-the-cause negative control (reading the path at a pre-merge `main` SHA returns
  404). Never fabricate a production line before the merge lands.

## Right-sized testing

A permanent regression test is not owed for a one-line docs marker; verification is a content
check at the PR head and again at the merge commit. Match the proof to the change class.

## Handoffs are the audit trail

`.legion/<phase>.json` files are the durable, machine-readable state that let this retro be
reconstructed without re-running anything: populate them fully — `implement.json` with
`reviewRounds` (`correctionHead`, `commentId`, disposition, reply), `ci`, and `proof`
(command / criterion / observed / surface / negativeControl); `review.json` with
`acceptedThreads` (disposition, `isResolved`). Under the Go daemon before Stage 7 no role
pushes the `.legion/` deletion, so these records ride the PR through retro; the operator
removes them from the default branch in a follow-up after merge.
