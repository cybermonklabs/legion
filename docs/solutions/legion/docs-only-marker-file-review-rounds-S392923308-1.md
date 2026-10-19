---
title: "Driving a docs-only marker-file change through the full Legion workflow"
category: legion
tags:
  - docs-only-change
  - byte-exact-markdown
  - handoff-only-push
  - review-threads
  - ci-verdict-head
date: 2026-10-19
status: active
module: legion-workflow
related_issues:
  - "S392923308-1"
  - "sjawhar/legion-smoke#664"
---

# Driving a docs-only marker-file change through the full Legion workflow

A marker/docs-only change still runs the whole plan→implement→test→review→retro→merge gate. The
file is trivial; the workflow mechanics are where the mistakes live. Follow these rules.

- Append the exact string the reviewer's live review thread (and its paired instruction) dictates,
  not the string the plan forecast. A per-round reviewer instruction supersedes the plan's
  prediction; trusting the plan text verbatim ships the wrong line.
- Preserve a byte-exact first line across every append and re-prove it. For an em-dash (U+2014)
  separator, write it with `printf '\xe2\x80\x94'`, verify with `cat -A`, and keep an ASCII-hyphen
  `grep -qxF` negative control plus a byte count: a downstream check greps for that exact line, so
  a hyphen autocorrect or a stray trailing newline breaks the marker while the diff still looks
  plausible.
- Append only; never rewrite lines already present. Each round adds one line below the existing
  ones and leaves the first line and all other files untouched.
- Read and record the CI verdict against the code-bearing head SHA, never the PR head. A
  handoff-only push carries a `skip-checks: true` trailer (last line, after two blank lines) so
  GitHub starts no workflow; "green" is valid only because the inter-head compare touches nothing
  but `.legion/` and the daemon carries the code head's verdict forward.
- Scope the single-file guard to the code commit, not the whole PR, and state it that way in the
  proof. `jj diff -r <codehead> --name-only` lists only the marker file, while the PR as a whole
  legitimately also carries the `.legion/<issue>/` handoff files; a naive whole-PR single-file
  assertion false-fails.
- Answer and resolve each review thread individually with its own disposition reply citing the fix
  SHA, never in bulk. A bot-opened thread ("is this file change needed?") requires the reviewer to
  adjudicate/accept before the implementer (the PR author) resolves it; a bulk resolve would close
  an unadjudicated thread.
- Refresh the PR body's Change/Proven-by/Size/CI/Threads/E2E lines to the latest head every round.
  The reviewer re-checks thread counts, per-thread fix SHAs, and both E2E blocks against the live
  body before approving; a stale body fails the gate even with correct code.

## Evidence

PR sjawhar/legion-smoke#664 (issue S392923308-1): one file `smoke/S392923308-1.md`, first line
`S392923308-1 — Primary durable workflow profile` (em-dash U+2014), then three appended
`Legion proof review round N (S392923308)` lines across three review rounds. The plan's `plan.json`
AC4 had forecast the appended text as `Stage 3 review round N (...)`; every round instead appended
`Legion proof review round N (...)` per the reviewer's thread, which was correct. CI (`ci` job
`check`, `fail-on-demand` job `gate`) ran only on code heads (e.g. 5609f4bb); the PR/approval heads
(91fd7281, b32d4af5) were handoff-only commits with `skip-checks: true`. Four threads (three round
threads + one bot thread) were each resolved one at a time after a disposition reply.

## Known gap

The tester's `E2E (tester)` re-ran the implementer's identical commands on the same GitHub surface
at the same code head rather than taking an orthogonal path, so the two proofs largely duplicate
rather than cross-check. For a 4-line marker file the verification surface is inherently narrow
(grep the exact line, confirm the single-file diff, read check-runs), so an orthogonal path adds
little here; on a change with real behavior, the tester should drive the surface independently
(different inputs, a second/concurrent run, a boundary the implementer did not exercise) instead of
re-running the implementer's commands.
