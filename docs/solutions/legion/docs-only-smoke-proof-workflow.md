---
title: "Proving a docs-only Legion smoke change with no test suite"
category: legion
tags:
  - legion-workflow
  - docs-only
  - proof
  - negative-control
  - jj
date: 2026-09-28
status: active
module: legion-smoke
related_issues:
  - "S392537713-1"
  - "sjawhar/legion-smoke#442"
---

# Proving a docs-only Legion smoke change with no test suite

Reusable learnings from the S392537713-1 Stage 3 proof (PR #442): a single new
`smoke/<KEY>.md` file plus one appended line per review round, in a repo with no
build and no test suite. Integrates the implementer's own context with an
independent fresh-eyes pass.

## Prove the negative control against the only CI signal that can fail

`legion-smoke` CI is two workflows: `ci`/`check` is a fixed `sleep 20 && exit 0`
(always green — its pass proves nothing about content), and `fail-on-demand`/`gate`
exits 1 **iff** a top-level `.fail-me` file exists. The gate is the only
content-sensitive signal, so it is the substitute for a test suite. Prove it by
running the gate's own script in a temp dir with a `.fail-me` present (observe
`exit 1`) and confirming the pushed head carries none (`gh api
.../contents/.fail-me?ref=<sha>` → 404, or `jj file list` shows zero). Never push
a `.fail-me` to the real branch to "demonstrate" the failure — reproduce the guard
locally, then show your change does not trip it.

## Verify the criterion on the real pushed head, not just the local workspace

The acceptance surface is explicitly "the pushed/CI-built head" (production-like).
A local `cat` alone does not prove the branch GitHub/CI actually built carries the
content. Read the file independently at the exact head SHA via
`gh api repos/<owner>/<repo>/contents/<path>?ref=<headSha> | base64 -d`, and pin
that SHA in every proof line and the `E2E` lines.

## Each REQUEST_CHANGES round: append exactly one line, change nothing else, re-push

The spec's Scope permits one appended line per review round. The reviewer posted
body-only REQUEST_CHANGES (no inline threads) naming the literal string to add
(`Stage 3 review round N (S392537713)`). Append precisely that line, confirm the
round's diff touches exactly one line (`jj -R <ws> diff -r <sha> --git`), re-push,
and re-verify on the new head. Do not batch, reorder, or reword — the review text
dictates the exact string. Body-only reviews leave `reviewThreads` empty, so there
is nothing to resolve; `legion threads resolve` reports zero and the `Threads`
section records "body-only REQUEST_CHANGES, no inline threads." Do not hunt for
threads that do not exist.

## Distinguish the code head from the handoff-advanced branch head

Every phase writes a `.legion/<phase>.json` handoff, split into its own commit and
pushed, so the branch head after test/review sits **above** the last code commit
(e.g. code head `a3f91e8` vs. branch head `6e6b0dd3`/`c9ae03c9`). Cite CI status
against the commit that actually changed the deliverable, and expect the head to
move across phases — that is the handoff cadence, not a rebase anomaly. The
implementer's own per-round cadence: `jj describe` the code commit → `jj new` →
`jj bookmark set legion/<KEY> -r @- --allow-backwards` → `legion push` (carries
code, runs CI); then `handoff_write` → `jj split -m "implement: record handoff" .legion/implement.json`
→ `legion push` again.

## Docs-only: skip the quality passes and say so; honor the collision rule

`plan.json` sets `docsOnly: true`. There is no runtime code, so `ce-simplify-code`
and the thermonuclear pair do not run and the reviewer omits the `Thermo` line;
the tester records "no red test required" — the deliverable proof is the file read
plus the gate negative control, never an authored red/green test. Honor the
planner's collision rule: `smoke/` is a shared graveyard of ~25 prior proof files,
so if `smoke/<KEY>.md` already exists, do **not** overwrite — report to the
architect.

## Under the Go daemon, `.legion/` deletion is deferred; retro commits above the approved head

This daemon does not push the `.legion/` deletion before merge (deferred to a
later stage / operator follow-up). The reviewer approves a head that still carries
`.legion/`; retro commits its learnings under `docs/solutions/` **above** that
approved head, and a commit that changes only `docs/solutions/` does not void the
approval — the tree goes from retro straight to the merger. Prior Stage 3 runs
confirm this: main history shows each `smoke: add <KEY> marker` PR followed by a
`proof fixture: remove the handoffs and learnings` cleanup that strips both
`.legion/` and the learnings after the fact.
