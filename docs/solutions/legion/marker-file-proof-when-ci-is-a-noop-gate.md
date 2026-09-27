---
title: "Proving marker/docs-file Legion issues when CI is a no-op gate"
category: legion
tags:
  - proof
  - smoke-marker
  - docs-only
  - review-rounds
  - negative-control
date: 2026-09-27
status: active
module: smoke
related_issues:
  - "S392028645-1"
  - "sjawhar/legion-smoke#414"
---

# Proving marker/docs-file Legion issues when CI is a no-op gate

## Context

Stage 3 durable-workflow proof issues each produce one marker file `smoke/S<key>-1.md`.
`.github/workflows/ci.yml` is a decorative gate — job `check` runs `sleep 20 && exit 0` — so a
green CI run cannot distinguish a correct file from a wrong one. The proof surface is the file
content itself, read on the branch GitHub serves.

## What proves the change (reusable triad)

Green CI is never correctness evidence for a marker/docs issue. The assertion that actually
proves it, used by both implementer and tester on #414:

1. **Exact whole-line match, not substring.** `head -1 smoke/S<key>-1.md | grep -qx 'S<key>-1'`
   for the first line (the issue key); `tail -1 … | grep -qx 'Stage 3 review round N (S<key>)'`
   for the latest appended line. `grep` without `-x` passes a near-miss line (wrong key, wrong
   round number, a trailing space).
2. **Line count to prove append-not-overwrite.** `wc -l` must equal the expected total and every
   prior round line must still be present. This catches the two realistic failure modes of this
   workflow: overwriting the file instead of appending, and dropping an earlier line.
3. **A negative control.** The same exact-match check run against another issue's file (or an
   absent file) must fail (`grep` exits non-zero). Without it the assertion could be trivially
   true. Example: `tail -1 smoke/S<other>-1.md | grep -qx 'Stage 3 review round N (S<key>)'` → no
   match.

## Review-round protocol is deterministic and self-documenting

Correctness is driven by the review loop, not CI:

- The reviewer posts `CHANGES_REQUESTED` for each round with **no inline threads**; the review
  body names the exact correction ("append this one line, change nothing else").
- `legion threads resolve --pr <n> --repo <owner>/<repo>` therefore reports `no unresolved
  threads` every round — do not wait for inline threads; act on the review body.
- The implementer appends exactly the named line, pushes, and replies with the fixing commit SHA
  and the CI run at that head; the final review `APPROVE`s a named head.

A future worker can rely on this shape: one appended line per round, record the fixing SHA per
round, re-cite CI at the new head.

## Real gap: two incompatible line-1 conventions in `smoke/`

The acceptance requires the first line to *be* the issue key, and bare-key files
(`S392028645-1`) satisfy `grep -qx '<key>'`. But some siblings use a key+descriptor first line
(e.g. `S394570180-1 — Primary durable workflow profile`), which would **fail** an exact-match
`head -1 | grep -qx '<key>'` validator. On #414 the descriptor ("Primary durable workflow
profile marker") lives only in the PR title/body, not the file. Any future automated validator
of these markers must decide between exact-match and prefix-match up front; an exact-match check
will report the descriptor-form files as non-conforming.

## Implementer notes

- Each review round is its own `implementing` phase: a push does not finish it — the round ends
  with the `legion` tool's `handoff_complete`, and the tester re-runs only after that.
- The `.legion/` handoff commits from every phase legitimately ride the issue branch alongside
  the one-line marker diff; that is expected, not scope creep. The unchanged-diff fingerprint and
  review scope exclude `.legion/` and `docs/solutions/` precisely so these process artifacts do
  not re-trigger review.
