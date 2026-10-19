---
title: "Legion smoke marker-file workflow: append-only review rounds and head discipline"
category: legion
tags:
  - legion
  - smoke
  - review-rounds
  - proof-discipline
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S393939591-1"
  - "sjawhar/legion-smoke#653"
---

# Legion smoke marker-file workflow: append-only review rounds and head discipline

A Stage 3 Legion run on `sjawhar/legion-smoke` produces one marker file `smoke/<ISSUE>.md`
named after the issue. Follow these rules for that marker and its review rounds.

## Rules

- Marker first line is byte-exact: `<ISSUE> — Primary durable workflow profile`, separator an
  em dash **U+2014** (bytes `e2 80 94`) with single spaces — never an ASCII hyphen. Write it once
  with `printf` and never retype it. Verify `grep -q ' — '` matches and `grep -q ' - '` (ASCII
  hyphen) does not.
- Each review round **appends exactly one line** `Legion proof review round N (<ISSUE>)` below the
  existing lines; never edit an earlier line. Verify append-only: line 1 and prior round lines are
  byte-unchanged, and `grep -c ''` increments by exactly 1 per round. Two lines in one round is out
  of scope.
- The permitted round count is an architect scope ruling, not open-ended. A reviewer asking for a
  line beyond what the spec permits is an escalation to the architect, not an append.
- The implementation commit touches only `smoke/<ISSUE>.md`. Never create `.fail-me`: it both adds
  a second diff path and fails the `fail-on-demand`/`gate` job.

## Head discipline (handoff vs code head)

- The branch carries code commits (marker edits) plus `.legion/<ISSUE>/` handoff commits pushed
  **above** the code head. A handoff-only push ends its head message with `skip-checks: true`
  (last line, after two blank lines) so GitHub starts no workflow; the daemon carries the code
  head's verdict forward. Do **not** expect CI on the PR/handoff head — read and cite checks at the
  **code head SHA**.
- CI is two workflows, each triggered on `push` and `pull_request`: `ci`/`check` (sleep 20) and
  `fail-on-demand`/`gate` (fails iff `.fail-me` exists). All four runs must be `success`.
- Test and review phases advance the branch after each round. Base the next round's commit on the
  **current branch tip** (`jj new legion/<ISSUE>`), not on your own earlier commit, or the append
  lands on a stale parent and drops prior rounds.

## Proof surface

Prove every acceptance criterion on the GitHub surface at the code head SHA, never by rerunning:
`git show <head>:smoke/<ISSUE>.md` for bytes, `jj diff -r <head> --name-only` for AC "only the
marker changed", and `gh pr checks <PR>` / `gh api .../commits/<head>/check-runs` for green. The
standing negative control: `.fail-me` absent ⇒ `gate` green; creating it would fail both the diff
scope and the gate.

## Review threads

A docs-only marker PR skips the thermonuclear/code passes, but a bot review thread (e.g.
"is this file change needed?") must still be answered **and** resolved by the implementer (the PR
author), not left dangling; the reviewer adjudicates and names the bot threads it accepts.

## Evidence

Issue S393939591-1, PR sjawhar/legion-smoke#653. Final code head `ef37901b`: marker had 4 lines
(title + rounds 1–3), `grep -c '' == 4`, commit touched only `smoke/S393939591-1.md`. CI green on
`ef37901b`: `ci`/`check` runs 39241067555 (pull_request) + 39241059837 (push), `fail-on-demand`/
`gate` runs 39241067728 (pull_request) + 39241059838 (push). Bot thread `PRRT_kwDOUCdo886t4eWo`
answered and resolved by the implementer, accepted by the reviewer. Handoff heads (`49fa5e3`,
approved `086b1062`) added only `.legion/S393939591-1/*.json` above the code head.
