---
title: "Stage 3 durable-workflow smoke proof: marker-file conventions"
category: legion
tags:
  - legion-smoke
  - stage3-proof
  - marker-file
  - review-rounds
date: 2026-10-07
status: active
module: smoke
related_issues:
  - "S394543611-1"
  - "sjawhar/legion-smoke#518"
---

# Stage 3 durable-workflow smoke proof: marker-file conventions

A Stage 3 proof issue adds exactly one file, `smoke/<KEY>.md`, and exercises the full Legion
workflow (plan → implement → test → review rounds → retro → merge) on it. The file's content is
the proof artifact, so its bytes and its growth across rounds are the only things that matter.

## Rules

- **Pin the marker line byte-for-byte; copy, never retype.** Line 1 is `<KEY> — <Title>` with an
  em-dash **U+2014** (bytes `E2 80 94`), not an ASCII hyphen `-` or ` - `. Write it with a tool
  that preserves the exact bytes and verify the codepoint before committing — an ASCII dash would
  silently diverge from every sibling and from any exact-match check.
- **Append exactly one review-round line per review round; never pre-write them.** Each review
  round that requests the correction adds one line `<prefix> review round N (<KEY>)` below the
  prior lines and changes nothing else. The point of the proof is a workflow that survives several
  handoff/review cycles; pre-adding the lines defeats it. Verify the line count grew by exactly one
  each round.
- **Keep the review-line prefix consistent with the issue's own series.** Two prefixes coexist in
  the repo — `Legion proof review round N` and `Stage 3 review round N`. Only line 1's
  `Primary durable workflow profile` marker is shared across that family; the review prefix varies
  per series and MUST be uniform across the one file's rounds. Follow the prefix the round's
  instruction names.
- **CI proves nothing about content — a real file read is the proof.** `ci.yml` runs only
  `sleep 20 && exit 0` and never checks out the repo; `fail-on-demand.yml` checks out but only
  tests for a committed `.fail-me`. A green pipeline is a gate, not content verification: the
  acceptance proof is reading `smoke/<KEY>.md` (marker bytes + N round lines) with a negative
  control (absent on `main`). Never commit `.fail-me` unless deliberately exercising the red path.

## Implementer workflow notes

- **The implement handoff's `proof.headSha` names the code commit, not the handoff commit.** Each
  round splits the `.legion/<issue>/implement.json` handoff into its own commit above the code
  commit; the file content is unchanged by that handoff commit, so the proof legitimately cites the
  code head (e.g. `edd2b905`) while the branch tip is the handoff commit.
- **`handoff_complete` returns `HANDOFF_NO_RUN` when the turn started before the daemon's task
  arrived.** When a `dispatch_message` (not the daemon task) starts the turn, the task is delivered
  at turn end. Do not redo work: end the turn, and on the next `Continue …` prompt call
  `handoff_complete` again against the already-committed work.
- **An implementer reply never closes a bot review thread.** The implementer answers a bot thread
  `Fixed in <commit>: …`; it stays open until the Legion reviewer posts `Accepted:`, and
  `legion threads resolve` closes it on the *next* push after that acceptance.

## Evidence

- PR sjawhar/legion-smoke#518, issue S394543611-1. File `smoke/S394543611-1.md` grew one line per
  review round across rounds 1–3 (`Legion proof review round 1/2/3 (S394543611)`).
- Em-dash confirmed as bytes `E2 80 94` by the tester's `jj file show … | cat -A` (verdict
  `verified`); implementer proof read the file at `edd2b905` with negative control `No such path`
  on `main`.
- Bot thread `4212394868` ("is this file change needed?") stayed open under the implementer's
  `Fixed in a658a471` reply and resolved only after the reviewer's `Accepted:` on the following
  round's push.
- `ci.yml` (`sleep 20 && exit 0`) and `fail-on-demand.yml` (`.fail-me` gate) both green at every
  head; neither inspects the marker file.
