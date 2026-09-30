---
title: "Stage 3 smoke proof: per-issue markers, one-line review rounds, and proof discipline"
category: legion
tags:
  - legion
  - smoke-proof
  - review-threads
  - proof-negative-control
  - jj-workflow
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "S393189958-1"
  - "sjawhar/legion-smoke#464"
---

# Stage 3 smoke proof: per-issue markers, one-line review rounds, and proof discipline

Reusable lessons from driving a Legion Stage 3 durable-workflow proof (issue S393189958-1,
PR #464) through the Go-daemon pipeline: plan → implement → test → three review rounds →
approval → retro. The change was one file, `smoke/S393189958-1.md`, whose first line names the
issue key, plus one `Stage 3 review round N (S393189958)` line appended per review round.

## Isolate each issue's output in its own path

The plan chose a per-issue file `smoke/<KEY>.md` over appending to a shared file
(`README.md`/`smoke.txt`) specifically to avoid merge contention with concurrent smoke issues.
`README.md` in this repo is the live anti-pattern: dozens of issues' round-lines jammed into one
file. When many Legion issues touch the same directory in parallel, give each issue its own path
so their branches never collide.

## Satisfy a review-round change-request by appending exactly one line

Each CHANGES_REQUESTED round asked to append `Stage 3 review round N (S393189958)` "below the
lines already there, and change nothing else." Match the reviewer's requested text verbatim and
touch nothing outside the requested line. The plan pre-declared one appended line per round as
in-scope, so the final file is exactly marker + 3 lines. Keep review-driven edits minimal and
literal.

## Bot review threads: two-actor disposition

A bot (`sjawhar-agent`) opened a thread asking "is this file change needed?". The implementer's
reply (`Declined:` — the file is required by spec) answers but never closes it; only the Legion
reviewer's `Accepted:` closes a bot's thread. `legion threads resolve --pr <n> --repo <o>/<r>`
run before each push that answers a review then sweeps it: it prints `left open …` while the
newest comment is the implementer's reply, and `resolved … the Legion reviewer's acceptance of a
bot's thread` once the reviewer has accepted. Do not treat a bot thread as done on your own reply.

## Every proof carries a negative control — assert absence, not just presence

Each acceptance criterion and each `proof` block pairs the positive observation
(`grep -c` = 1, `wc -l` = 4) with a negative control on `main@origin` returning 0 /
`No such path` (the file/line does not exist before the change). The absence check is what makes
the proof meaningful: prove both that the artifact exists on the branch AND that it did not exist
on main.

## The tester independently reproduces the surface, it does not restate numbers

The test handoff re-drove the identical file at the PR head via `jj file show -r <head>` plus
`wc -l`/`grep -c`, confirmed the four lines, and recorded `implementerProof.verdict: "verified"`
with the CI run IDs. Verification means re-running the check yourself at a named head.

## Match the required proof to the change class; every push runs (or deliberately skips) CI

Both sandbox workflows (`ci`/check = sleep-green, `fail-on-demand`/gate = pass unless a `.fail-me`
file exists) trigger on `[push, pull_request]`, so each phase's `.legion/` handoff push must
settle green before advancing. Under the Go daemon a push skips CI only when it changes nothing
but a handoff whose phase guarantees a later push (planner's `plan.json`, tester's `test.json`,
reviewer's `review.json` with `changes_requested`); every code push and the implement handoff
push run full CI. A docs/marker-only PR needs green CI plus thread and E2E proof, but the
reviewer correctly omitted the thermonuclear pair because no runtime code changed.

## Mechanics that bit / worth knowing

- Each return to `implementing` is its own phase; a push alone does not finish a review round —
  end it with the `legion` tool's `handoff_complete`.
- Push with `legion push` (Go daemon): it operates on `@-`, so after committing the code, run
  `jj new` to leave an empty working copy above it. A `jj split` of the handoff file leaves a
  stray empty commit as "remaining changes"; the bookmark still lands on the handoff commit.
- Cite the code head (an ancestor) in the `E2E` line and the final handoff head in the `CI` line;
  file content is identical at both, and the handoff push produces the tip CI runs on.
- Under this daemon `.legion/` is NOT deleted before retro: the reviewer approves a head that
  still carries it, and retro commits (this document) sit above the approved head, changing only
  `docs/solutions/`, which does not void the approval.
